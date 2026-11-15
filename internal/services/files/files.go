// Package files is the upload service: quota reservation, streaming into
// storage, listing, deletion, serving metadata, moderation and the expiry
// worker.
package files

import (
	"bufio"
	"context"
	"errors"
	"image"
	_ "image/gif"  // DecodeConfig
	_ "image/jpeg" // DecodeConfig
	_ "image/png"  // DecodeConfig
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/boson-chat/files/internal/apperr"
	"github.com/boson-chat/files/internal/db"
	filesrepo "github.com/boson-chat/files/internal/db/repositories/files"
	"github.com/boson-chat/files/internal/db/repositories/overrides"
	"github.com/boson-chat/files/internal/ids"
	"github.com/boson-chat/files/internal/logger"
	"github.com/boson-chat/files/internal/principal"
	"github.com/boson-chat/files/internal/services/settings"
	"github.com/boson-chat/files/internal/services/storage"
	"github.com/boson-chat/files/internal/tracing"
	"github.com/boson-chat/files/metrics"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	_ "golang.org/x/image/webp" // DecodeConfig
)

// Errors.
var (
	ErrDisabled     = apperr.New(http.StatusServiceUnavailable, "uploads_disabled", "file uploads are turned off on this network")
	ErrGuestOff     = apperr.New(http.StatusForbidden, "guest_disabled", "guest uploads are turned off on this network")
	ErrEmpty        = apperr.New(http.StatusBadRequest, "empty_file", "the file is empty")
	ErrIncomplete   = apperr.New(http.StatusBadRequest, "incomplete_body", "the upload ended before Content-Length bytes arrived")
	ErrTooLong      = apperr.New(http.StatusBadRequest, "body_too_long", "the body is longer than Content-Length")
	errTooLarge     = apperr.New(http.StatusRequestEntityTooLarge, "file_too_large", "the file is larger than the maximum file size")
	errQuota        = apperr.New(http.StatusRequestEntityTooLarge, "quota_exceeded", "this upload would go over your upload quota")
	ErrStorageFull  = apperr.New(http.StatusInsufficientStorage, "storage_full", "server storage is full; try again later")
	ErrNotFound     = apperr.New(http.StatusNotFound, "not_found", "no such file")
	ErrGone         = apperr.New(http.StatusGone, "gone", "this file expired or was deleted")
	errBadOverride  = apperr.Invalid("quota_bytes and max_file_bytes must be positive or null")
	errUnknownOwner = apperr.Invalid("user must be an account id (usr_…) or a guest key (gst_…)")
)

// Deps are the service's collaborators.
type Deps struct {
	Repo      filesrepo.FileRepositoryImpl
	Overrides overrides.OverrideRepositoryImpl
	Tx        db.TxManagerImpl
	Storage   storage.Storage
	Settings  settings.SettingsServiceImpl
	Metrics   *metrics.Metrics
	// PublicURL is the files host base URL (FILES_PUBLIC_URL).
	PublicURL string
	// GuestsConfigured: a guest secret is set.
	GuestsConfigured bool
	// IPQuotaFactor multiplies the guest quota for the per-IP total.
	IPQuotaFactor  int
	PendingTimeout time.Duration
	Now            func() time.Time
}

// UploadInput is one upload.
type UploadInput struct {
	Name        string
	ContentType string
	Size        int64
	Body        io.Reader
}

// FileView is a file as clients see it.
type FileView struct {
	ID          string    `json:"id"`
	URL         string    `json:"url"`
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Width       *int      `json:"width,omitempty"`
	Height      *int      `json:"height,omitempty"`
}

// AdminFileView adds moderation fields.
type AdminFileView struct {
	FileView
	Owner     string     `json:"user"`
	OwnerKind string     `json:"user_kind"`
	Status    string     `json:"status"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	DeletedBy *string    `json:"deleted_by,omitempty"`
}

// Usage is GET /v1/me/usage.
type Usage struct {
	Used          int64  `json:"used"`
	Quota         int64  `json:"quota"`
	WindowSeconds int64  `json:"window_seconds"`
	MaxFile       int64  `json:"max_file"`
	Frees         []Free `json:"frees"`
}

// AdminUsage is GET /internal/files/usage.
type AdminUsage struct {
	Usage
	User      string          `json:"user"`
	UserKind  string          `json:"user_kind"`
	LiveFiles int             `json:"live_files"`
	LiveBytes int64           `json:"live_bytes"`
	OverrideV *OverrideView   `json:"override"`
	Defaults  settings.Limits `json:"defaults"`
}

// OverrideView is a per-user override (nil fields = class default).
type OverrideView struct {
	QuotaBytes   *int64    `json:"quota_bytes"`
	MaxFileBytes *int64    `json:"max_file_bytes"`
	UpdatedAt    time.Time `json:"updated_at"`
	UpdatedBy    *string   `json:"updated_by,omitempty"`
}

// OverrideInput is PUT /internal/quota/{user}.
type OverrideInput struct {
	QuotaBytes   *int64 `json:"quota_bytes"`
	MaxFileBytes *int64 `json:"max_file_bytes"`
}

// SearchInput is GET /internal/files.
type SearchInput struct {
	User   string
	Q      string
	Status string
	Cursor string
	Limit  int
}

// SearchResult is a page of the moderation list.
type SearchResult struct {
	Files      []AdminFileView `json:"files"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

type FileServiceImpl interface {
	Upload(ctx context.Context, p *principal.Principal, in UploadInput) (*FileView, error)
	ListMine(ctx context.Context, p *principal.Principal) ([]FileView, error)
	DeleteMine(ctx context.Context, p *principal.Principal, id string) error
	Usage(ctx context.Context, p *principal.Principal) (*Usage, error)
	// Lookup returns a live file for serving, or ErrNotFound / ErrGone.
	Lookup(ctx context.Context, id string) (*filesrepo.File, error)
	Open(ctx context.Context, f *filesrepo.File) (io.ReadSeekCloser, error)
	View(f *filesrepo.File) FileView

	Search(ctx context.Context, in SearchInput) (*SearchResult, error)
	AdminDelete(ctx context.Context, id, by string) error
	AdminUsage(ctx context.Context, user string) (*AdminUsage, error)
	SetOverride(ctx context.Context, user string, in *OverrideInput, by string) (*AdminUsage, error)
	Status(ctx context.Context) Status

	// RunExpiry is one pass of the expiry worker.
	RunExpiry(ctx context.Context) error
}

type FileService struct {
	d    Deps
	mu   sync.Mutex
	errs []ErrorEntry
}

func NewFileService(d Deps) *FileService {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Metrics == nil {
		d.Metrics = metrics.Noop()
	}
	if d.IPQuotaFactor <= 0 {
		d.IPQuotaFactor = 3
	}
	if d.PendingTimeout <= 0 {
		d.PendingTimeout = time.Hour
	}
	d.PublicURL = strings.TrimRight(d.PublicURL, "/")
	return &FileService{d: d}
}

func (s *FileService) span(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	attrs = append(attrs, attribute.String("service", "files"))
	return tracing.Tracer().Start(ctx, "FileService."+name, trace.WithAttributes(attrs...))
}

func (s *FileService) now() time.Time { return s.d.Now().UTC().Truncate(time.Microsecond) }

// limitsFor resolves the effective limits of a quota key.
func (s *FileService) limitsFor(ctx context.Context, owner string, guest bool, cfg settings.Settings) (settings.Limits, *overrides.Override, error) {
	l := cfg.Account
	if guest {
		l = cfg.Guest
	}
	o, err := s.d.Overrides.Get(ctx, owner)
	if err != nil {
		return l, nil, err
	}
	if o != nil {
		if o.QuotaBytes != nil {
			l.QuotaBytes = *o.QuotaBytes
		}
		if o.MaxFileBytes != nil {
			l.MaxFileBytes = *o.MaxFileBytes
		}
	}
	return l, o, nil
}

// URLFor is the public URL of a file.
func (s *FileService) URLFor(id, name string) string {
	return s.d.PublicURL + "/f/" + id + "/" + url.PathEscape(name)
}

func (s *FileService) View(f *filesrepo.File) FileView {
	return FileView{
		ID: f.ID, URL: s.URLFor(f.ID, f.Name), Name: f.Name, Size: f.Size, ContentType: f.ContentType,
		CreatedAt: f.CreatedAt.UTC(), ExpiresAt: f.ExpiresAt.UTC(), Width: f.Width, Height: f.Height,
	}
}

func quotaError(used, quota int64, window time.Duration, freesAt *time.Time) error {
	return errQuota.With(map[string]any{
		"used": used, "quota": quota, "window_seconds": int64(window / time.Second), "frees_at": freesAt,
	})
}

// checkStorage refuses uploads that would leave less than min_free.
func (s *FileService) checkStorage(ctx context.Context, cfg settings.Settings, size int64) error {
	st := cfg.Storage
	if fs, ok := s.d.Storage.(storage.FreeSpacer); ok {
		free, _, err := fs.FreeSpace()
		if err != nil {
			logger.FromCtx(ctx).Warn().Err(err).Msg("files: free space unknown")
			return nil
		}
		if free-size < st.MinFree {
			return ErrStorageFull
		}
		return nil
	}
	if st.Capacity <= 0 {
		return nil
	}
	stored, err := s.d.Repo.StoredBytes(ctx)
	if err != nil {
		return err
	}
	if st.Capacity-stored-size < st.MinFree {
		return ErrStorageFull
	}
	return nil
}

// reserve inserts the pending row if the quota allows, under advisory
// locks on the quota keys, so concurrent uploads can't overshoot.
func (s *FileService) reserve(ctx context.Context, p *principal.Principal, f *filesrepo.File, l settings.Limits, cfg settings.Settings) error {
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		keys := []string{p.Owner}
		if p.Kind == principal.KindGuest && p.IPKey != "" {
			keys = append(keys, p.IPKey)
		}
		if err := s.d.Repo.Lock(ctx, keys...); err != nil {
			return err
		}
		since := WindowStart(f.CreatedAt, cfg.Window)
		entries, err := s.d.Repo.WindowEntries(ctx, filesrepo.Key{Value: p.Owner}, since)
		if err != nil {
			return err
		}
		used := Used(entries)
		if used+f.Size > l.QuotaBytes {
			return quotaError(used, l.QuotaBytes, cfg.Window, FreesAt(entries, l.QuotaBytes, f.Size, cfg.Window))
		}
		if p.Kind == principal.KindGuest && p.IPKey != "" {
			ipQuota := cfg.Guest.QuotaBytes * int64(s.d.IPQuotaFactor)
			ipEntries, err := s.d.Repo.WindowEntries(ctx, filesrepo.Key{IP: true, Value: p.IPKey}, since)
			if err != nil {
				return err
			}
			if ipUsed := Used(ipEntries); ipUsed+f.Size > ipQuota {
				return quotaError(ipUsed, ipQuota, cfg.Window, FreesAt(ipEntries, ipQuota, f.Size, cfg.Window))
			}
		}
		return s.d.Repo.Create(ctx, f)
	})
}

func (s *FileService) Upload(ctx context.Context, p *principal.Principal, in UploadInput) (_ *FileView, err error) {
	ctx, span := s.span(ctx, "Upload", attribute.String("principal.kind", string(p.Kind)), attribute.Int64("file.size", in.Size))
	defer span.End()
	kind := string(p.Kind)
	outcome := "failed"
	defer func() {
		if e, ok := apperr.As(err); ok && e.Status == http.StatusRequestEntityTooLarge || ok && e.Status == http.StatusInsufficientStorage {
			outcome = e.Code
		}
		s.d.Metrics.Upload(ctx, kind, outcome, in.Size)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
		}
	}()

	cfg := s.d.Settings.Get()
	if !cfg.Enabled {
		return nil, ErrDisabled
	}
	guest := p.Kind == principal.KindGuest
	if guest && (!cfg.GuestEnabled || !s.d.GuestsConfigured) {
		return nil, ErrGuestOff
	}
	if in.Size <= 0 {
		return nil, ErrEmpty
	}
	l, _, err := s.limitsFor(ctx, p.Owner, guest, cfg)
	if err != nil {
		return nil, err
	}
	if in.Size > l.MaxFileBytes {
		return nil, errTooLarge.With(map[string]any{"max": l.MaxFileBytes})
	}
	if err := s.checkStorage(ctx, cfg, in.Size); err != nil {
		return nil, err
	}

	// Sniff the head (bounded peek; the body is still streamed).
	br := bufio.NewReaderSize(in.Body, 4096)
	head, _ := br.Peek(512)
	ct := DetectType(in.ContentType, head)

	now := s.now()
	id := ids.FileID()
	f := &filesrepo.File{
		ID: id, Owner: p.Owner, OwnerKind: kind, Name: SanitizeName(in.Name), ContentType: ct, Size: in.Size,
		StorageKey: id[:2] + "/" + id, Status: filesrepo.StatusPending, CreatedAt: now, ExpiresAt: now.Add(cfg.Retention),
	}
	if guest {
		ip := p.IPKey
		f.IPKey = &ip
	}
	if err := s.reserve(ctx, p, f, l, cfg); err != nil {
		return nil, err
	}
	release := func(cause error) {
		rctx := context.WithoutCancel(ctx)
		if derr := s.d.Storage.Delete(rctx, f.StorageKey); derr != nil {
			logger.FromCtx(ctx).Warn().Err(derr).Str("file_id", id).Msg("files: removing a failed upload's body")
		}
		if rerr := s.d.Repo.Remove(rctx, id); rerr != nil {
			logger.FromCtx(ctx).Error().Err(rerr).Str("file_id", id).Msg("files: releasing a reservation")
		}
		logger.FromCtx(ctx).Info().Err(cause).Str("file_id", id).Msg("upload failed; reservation released")
	}

	// Stream into storage; images are measured on the way through.
	var body io.Reader = br
	var dims chan *image.Config
	var pw *io.PipeWriter
	if imageTypes[baseType(ct)] {
		var pr *io.PipeReader
		pr, pw = io.Pipe()
		dims = make(chan *image.Config, 1)
		go func() {
			c, _, derr := image.DecodeConfig(pr)
			_, _ = io.Copy(io.Discard, pr)
			if derr != nil {
				dims <- nil
				return
			}
			dims <- &c
		}()
		body = io.TeeReader(br, pw)
	}
	perr := s.d.Storage.Put(ctx, f.StorageKey, body, in.Size, storage.Meta{
		ContentType:        PolicyFor(ct).ContentType,
		ContentDisposition: ContentDisposition(PolicyFor(ct).Inline, f.Name),
	})
	var cfgDims *image.Config
	if pw != nil {
		_ = pw.CloseWithError(perr)
		cfgDims = <-dims
	}
	if perr != nil {
		release(perr)
		switch {
		case errors.Is(perr, storage.ErrShortBody), errors.Is(perr, io.ErrUnexpectedEOF):
			return nil, ErrIncomplete
		case errors.Is(perr, storage.ErrLongBody):
			return nil, ErrTooLong
		}
		var mbe *http.MaxBytesError
		if errors.As(perr, &mbe) {
			return nil, ErrTooLong
		}
		return nil, perr
	}
	if cfgDims != nil && cfgDims.Width > 0 && cfgDims.Height > 0 {
		w, h := cfgDims.Width, cfgDims.Height
		f.Width, f.Height = &w, &h
	}
	if err := s.d.Repo.MarkReady(context.WithoutCancel(ctx), id, f.Width, f.Height); err != nil {
		release(err)
		return nil, err
	}
	f.Status = filesrepo.StatusReady
	outcome = "ok"
	v := s.View(f)
	logger.FromCtx(ctx).Info().Str("file_id", id).Str("owner", p.Owner).Int64("size", f.Size).Str("type", ct).Msg("file uploaded")
	return &v, nil
}

func (s *FileService) ListMine(ctx context.Context, p *principal.Principal) ([]FileView, error) {
	ctx, span := s.span(ctx, "ListMine")
	defer span.End()
	list, err := s.d.Repo.ListLive(ctx, p.Owner, s.now(), 1000)
	if err != nil {
		return nil, err
	}
	out := make([]FileView, 0, len(list))
	for i := range list {
		out = append(out, s.View(&list[i]))
	}
	return out, nil
}

// purge removes a body from storage and records it; failures are left for
// the expiry worker to retry.
func (s *FileService) purge(ctx context.Context, f *filesrepo.File) bool {
	if err := s.d.Storage.Delete(ctx, f.StorageKey); err != nil {
		s.recordError("purge", err)
		logger.FromCtx(ctx).Warn().Err(err).Str("file_id", f.ID).Msg("files: deleting from storage failed; will retry")
		return false
	}
	if err := s.d.Repo.MarkPurged(ctx, f.ID, s.now()); err != nil {
		s.recordError("purge", err)
		return false
	}
	return true
}

func (s *FileService) DeleteMine(ctx context.Context, p *principal.Principal, id string) error {
	ctx, span := s.span(ctx, "DeleteMine", attribute.String("file.id", id))
	defer span.End()
	return s.delete(ctx, id, p.Owner, p.Owner)
}

func (s *FileService) delete(ctx context.Context, id, owner, by string) error {
	if !ids.ValidFileID(id) {
		return ErrNotFound
	}
	ok, err := s.d.Repo.MarkDeleted(ctx, id, owner, by, s.now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	f, err := s.d.Repo.Get(ctx, id)
	if err != nil {
		return err
	}
	s.purge(context.WithoutCancel(ctx), f)
	return nil
}

func (s *FileService) usage(ctx context.Context, owner string, guest bool) (*Usage, *overrides.Override, error) {
	cfg := s.d.Settings.Get()
	l, o, err := s.limitsFor(ctx, owner, guest, cfg)
	if err != nil {
		return nil, nil, err
	}
	entries, err := s.d.Repo.WindowEntries(ctx, filesrepo.Key{Value: owner}, WindowStart(s.now(), cfg.Window))
	if err != nil {
		return nil, nil, err
	}
	return &Usage{
		Used: Used(entries), Quota: l.QuotaBytes, WindowSeconds: int64(cfg.Window / time.Second),
		MaxFile: l.MaxFileBytes, Frees: Schedule(entries, cfg.Window, 100),
	}, o, nil
}

func (s *FileService) Usage(ctx context.Context, p *principal.Principal) (*Usage, error) {
	ctx, span := s.span(ctx, "Usage")
	defer span.End()
	u, _, err := s.usage(ctx, p.Owner, p.Kind == principal.KindGuest)
	return u, err
}

func (s *FileService) Lookup(ctx context.Context, id string) (*filesrepo.File, error) {
	ctx, span := s.span(ctx, "Lookup", attribute.String("file.id", id))
	defer span.End()
	if !ids.ValidFileID(id) {
		return nil, ErrNotFound
	}
	f, err := s.d.Repo.Get(ctx, id)
	if errors.Is(err, filesrepo.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	switch {
	case f.Status == filesrepo.StatusPending:
		return nil, ErrNotFound
	case f.Status != filesrepo.StatusReady || !f.ExpiresAt.After(s.now()):
		return f, ErrGone
	}
	return f, nil
}

func (s *FileService) Open(ctx context.Context, f *filesrepo.File) (io.ReadSeekCloser, error) {
	return s.d.Storage.Open(ctx, f.StorageKey, f.Size)
}

// ---- moderation ----

func ownerKind(user string) (guest bool, ok bool) {
	switch {
	case strings.HasPrefix(user, "gst_"):
		return true, true
	case user != "" && len(user) <= 128:
		return false, true
	}
	return false, false
}

func (s *FileService) adminView(f *filesrepo.File) AdminFileView {
	return AdminFileView{
		FileView: s.View(f), Owner: f.Owner, OwnerKind: f.OwnerKind, Status: s.effectiveStatus(f),
		DeletedAt: f.DeletedAt, DeletedBy: f.DeletedBy,
	}
}

func (s *FileService) effectiveStatus(f *filesrepo.File) string {
	if f.Status == filesrepo.StatusReady && !f.ExpiresAt.After(s.now()) {
		return filesrepo.StatusExpired
	}
	return f.Status
}

func (s *FileService) Search(ctx context.Context, in SearchInput) (*SearchResult, error) {
	ctx, span := s.span(ctx, "Search")
	defer span.End()
	limit := in.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := filesrepo.Query{Owner: in.User, Q: in.Q, Status: in.Status, Limit: limit + 1, Now: s.now()}
	switch in.Status {
	case "", "all", filesrepo.StatusReady, filesrepo.StatusDeleted, filesrepo.StatusExpired, filesrepo.StatusPending:
	case "live":
		q.Status = ""
	default:
		return nil, apperr.Invalid("status must be live, all, ready, pending, deleted or expired")
	}
	if in.Cursor != "" {
		t, id, err := decodeCursor(in.Cursor)
		if err != nil {
			return nil, apperr.Invalid("bad cursor")
		}
		q.CursorTime, q.CursorID = &t, id
	}
	list, err := s.d.Repo.Search(ctx, q)
	if err != nil {
		return nil, err
	}
	res := &SearchResult{Files: []AdminFileView{}}
	for i := range list {
		if i == limit {
			last := list[limit-1]
			res.NextCursor = encodeCursor(last.CreatedAt, last.ID)
			break
		}
		res.Files = append(res.Files, s.adminView(&list[i]))
	}
	return res, nil
}

func (s *FileService) AdminDelete(ctx context.Context, id, by string) error {
	ctx, span := s.span(ctx, "AdminDelete", attribute.String("file.id", id))
	defer span.End()
	if by == "" {
		by = "admin"
	} else {
		by = "admin:" + by
	}
	err := s.delete(ctx, id, "", by)
	if err == nil {
		logger.FromCtx(ctx).Info().Str("file_id", id).Str("by", by).Msg("file deleted by an admin")
	}
	return err
}

func (s *FileService) AdminUsage(ctx context.Context, user string) (*AdminUsage, error) {
	ctx, span := s.span(ctx, "AdminUsage")
	defer span.End()
	guest, ok := ownerKind(user)
	if !ok {
		return nil, errUnknownOwner
	}
	u, o, err := s.usage(ctx, user, guest)
	if err != nil {
		return nil, err
	}
	cfg := s.d.Settings.Get()
	out := &AdminUsage{Usage: *u, User: user, UserKind: filesrepo.KindAccount, Defaults: cfg.Account}
	if guest {
		out.UserKind, out.Defaults = filesrepo.KindGuest, cfg.Guest
	}
	if o != nil {
		out.OverrideV = &OverrideView{QuotaBytes: o.QuotaBytes, MaxFileBytes: o.MaxFileBytes, UpdatedAt: o.UpdatedAt.UTC(), UpdatedBy: o.UpdatedBy}
	}
	live, err := s.d.Repo.ListLive(ctx, user, s.now(), 10000)
	if err != nil {
		return nil, err
	}
	out.LiveFiles = len(live)
	for _, f := range live {
		out.LiveBytes += f.Size
	}
	return out, nil
}

func (s *FileService) SetOverride(ctx context.Context, user string, in *OverrideInput, by string) (*AdminUsage, error) {
	ctx, span := s.span(ctx, "SetOverride")
	defer span.End()
	if _, ok := ownerKind(user); !ok {
		return nil, errUnknownOwner
	}
	if in == nil || (in.QuotaBytes == nil && in.MaxFileBytes == nil) {
		if err := s.d.Overrides.Delete(ctx, user); err != nil {
			return nil, err
		}
	} else {
		if (in.QuotaBytes != nil && *in.QuotaBytes <= 0) || (in.MaxFileBytes != nil && *in.MaxFileBytes <= 0) {
			return nil, errBadOverride
		}
		o := &overrides.Override{Owner: user, QuotaBytes: in.QuotaBytes, MaxFileBytes: in.MaxFileBytes, UpdatedAt: s.now()}
		if by != "" {
			o.UpdatedBy = &by
		}
		if err := s.d.Overrides.Put(ctx, o); err != nil {
			return nil, err
		}
	}
	logger.FromCtx(ctx).Info().Str("user", user).Str("by", by).Bool("cleared", in == nil).Msg("quota override set")
	return s.AdminUsage(ctx, user)
}

// ---- expiry worker ----

func (s *FileService) RunExpiry(ctx context.Context) error {
	ctx, span := s.span(ctx, "RunExpiry")
	defer span.End()
	now := s.now()
	cfg := s.d.Settings.Get()
	var errs []error
	// 1. Live files past their retention become expired.
	for {
		n, err := s.d.Repo.ExpireDue(ctx, now, 500)
		if err != nil {
			errs = append(errs, err)
			break
		}
		if n < 500 {
			break
		}
	}
	// 2. Bodies of deleted/expired files leave storage (retried until done).
	purged := 0
	for round := 0; round < 10; round++ {
		list, err := s.d.Repo.Unpurged(ctx, 200)
		if err != nil {
			errs = append(errs, err)
			break
		}
		ok := 0
		for i := range list {
			if s.purge(ctx, &list[i]) {
				ok++
			}
		}
		purged += ok
		if len(list) < 200 || ok == 0 {
			break
		}
	}
	if purged > 0 {
		s.d.Metrics.Expired(ctx, purged)
	}
	// 3. Reservations of uploads that never finished (crash) are released.
	stale, err := s.d.Repo.StalePending(ctx, now.Add(-s.d.PendingTimeout), 200)
	if err != nil {
		errs = append(errs, err)
	}
	for _, f := range stale {
		if err := s.d.Storage.Delete(ctx, f.StorageKey); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := s.d.Repo.Remove(ctx, f.ID); err != nil {
			errs = append(errs, err)
		}
	}
	// 4. Ledger rows whose body is gone and that left the window are pruned.
	if _, err := s.d.Repo.Prune(ctx, WindowStart(now, cfg.Window)); err != nil {
		errs = append(errs, err)
	}
	err = errors.Join(errs...)
	if err != nil {
		s.recordError("expiry", err)
	}
	return err
}
