package files

import (
	"context"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	filesrepo "github.com/boson-chat/files/internal/db/repositories/files"
	"github.com/boson-chat/files/internal/services/settings"
	"github.com/boson-chat/files/internal/services/storage"
)

// ErrorEntry is a recent background/storage error.
type ErrorEntry struct {
	At    time.Time `json:"at"`
	Op    string    `json:"op"`
	Error string    `json:"error"`
}

// StorageStatus is the storage part of GET /internal/files/status.
type StorageStatus struct {
	Driver    string `json:"driver"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
	// FreeBytes / TotalBytes: the local volume (statfs), or for s3 the
	// capacity budget minus stored bytes (null when unknown: s3 without
	// capacity).
	FreeBytes  *int64 `json:"free_bytes"`
	TotalBytes *int64 `json:"total_bytes"`
	// UsedBytes are the bodies this service has in storage.
	UsedBytes     int64 `json:"used_bytes"`
	MinFreeBytes  int64 `json:"min_free_bytes"`
	CapacityBytes int64 `json:"capacity_bytes"`
	// Full: new uploads are refused with 507 storage_full.
	Full bool `json:"full"`
}

// Status is GET /internal/files/status.
type Status struct {
	OK      bool             `json:"ok"`
	Storage StorageStatus    `json:"storage"`
	Totals  filesrepo.Totals `json:"totals"`
	Config  settings.Echo    `json:"config"`
	Errors  []ErrorEntry     `json:"errors"`
}

func (s *FileService) recordError(op string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs = append(s.errs, ErrorEntry{At: s.now(), Op: op, Error: err.Error()})
	if len(s.errs) > 20 {
		s.errs = s.errs[len(s.errs)-20:]
	}
}

func (s *FileService) Status(ctx context.Context) Status {
	ctx, span := s.span(ctx, "Status")
	defer span.End()
	cfg := s.d.Settings.Get()
	st := StorageStatus{Driver: s.d.Storage.Driver(), MinFreeBytes: cfg.Storage.MinFree, CapacityBytes: cfg.Storage.Capacity}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	start := time.Now()
	err := s.d.Storage.Probe(pctx)
	cancel()
	st.LatencyMS = time.Since(start).Milliseconds()
	st.OK = err == nil
	if err != nil {
		st.Error = err.Error()
	}
	var errs []error
	now := s.now()
	totals, terr := s.d.Repo.Totals(ctx, now, WindowStart(now, cfg.Window))
	if terr != nil {
		errs = append(errs, terr)
	}
	st.UsedBytes = totals.StoredBytes
	if fs, ok := s.d.Storage.(storage.FreeSpacer); ok {
		if free, total, ferr := fs.FreeSpace(); ferr == nil {
			st.FreeBytes, st.TotalBytes = &free, &total
			st.Full = free < cfg.Storage.MinFree
		} else {
			errs = append(errs, ferr)
		}
	} else if cfg.Storage.Capacity > 0 {
		free := max(cfg.Storage.Capacity-totals.StoredBytes, 0)
		total := cfg.Storage.Capacity
		st.FreeBytes, st.TotalBytes = &free, &total
		st.Full = free < cfg.Storage.MinFree
	}
	s.mu.Lock()
	recent := append([]ErrorEntry{}, s.errs...)
	s.mu.Unlock()
	for _, e := range errs {
		recent = append(recent, ErrorEntry{At: now, Op: "status", Error: e.Error()})
	}
	return Status{
		OK: st.OK && terr == nil, Storage: st, Totals: totals, Config: s.d.Settings.Echo(), Errors: recent,
	}
}

func encodeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(t.UnixMicro(), 10) + ":" + id))
}

func decodeCursor(c string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", err
	}
	ts, id, ok := strings.Cut(string(raw), ":")
	if !ok || id == "" {
		return time.Time{}, "", errors.New("bad cursor")
	}
	us, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return time.Time{}, "", err
	}
	return time.UnixMicro(us).UTC(), id, nil
}
