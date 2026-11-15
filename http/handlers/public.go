package handlers

import (
	"errors"
	nethttp "net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/boson-chat/files/internal/apperr"
	"github.com/boson-chat/files/internal/logger"
	"github.com/boson-chat/files/internal/principal"
	"github.com/boson-chat/files/internal/reqmeta"
	"github.com/boson-chat/files/internal/services/files"
	"github.com/boson-chat/files/internal/services/guest"
	"github.com/boson-chat/files/internal/services/settings"
	"github.com/boson-chat/files/internal/services/storage"
	"github.com/boson-chat/files/metrics"
)

var (
	errUnauthenticated = apperr.New(nethttp.StatusUnauthorized, "unauthenticated", "send an account access token (Authorization: Bearer) or a guest token (Authorization: Guest)")
	errLengthRequired  = apperr.New(nethttp.StatusLengthRequired, "length_required", "Content-Length is required")
)

// Public serves the files host API (files.<domain>).
type Public struct {
	Files    files.FileServiceImpl
	Guests   guest.GuestServiceImpl
	Settings settings.SettingsServiceImpl
	// AccountsEnabled: AUTH_ISSUER is set.
	AccountsEnabled bool
	// StorageDriver is the driver in use (direct bucket serving needs s3).
	StorageDriver string
	Metrics       *metrics.Metrics
	Now           func() time.Time
}

// Routes mounts the public routes.
func (h *Public) Routes(mux *nethttp.ServeMux) {
	mux.HandleFunc("GET /v1/config", h.config)
	mux.HandleFunc("POST /v1/guest-token", h.guestToken)
	mux.HandleFunc("GET /v1/me/usage", h.authed(h.usage))
	mux.HandleFunc("GET /v1/me/files", h.authed(h.listMine))
	mux.HandleFunc("POST /v1/files", h.authed(h.upload))
	mux.HandleFunc("DELETE /v1/files/{id}", h.authed(h.deleteMine))
	mux.HandleFunc("GET /v1/files/{id}", h.metadata)
	mux.HandleFunc("GET /f/{id}", h.serve)
	mux.HandleFunc("GET /f/{id}/{name...}", h.serve)
}

func (h *Public) authed(fn func(nethttp.ResponseWriter, *nethttp.Request, *principal.Principal)) nethttp.HandlerFunc {
	return func(w nethttp.ResponseWriter, r *nethttp.Request) {
		p := principal.From(r.Context())
		if p == nil {
			WriteError(w, r, errUnauthenticated)
			return
		}
		fn(w, r, p)
	}
}

type limitsJSON struct {
	QuotaBytes    int64 `json:"quota_bytes"`
	WindowSeconds int64 `json:"window_seconds"`
	MaxFileBytes  int64 `json:"max_file_bytes"`
}

type configJSON struct {
	Enabled         bool `json:"enabled"`
	GuestEnabled    bool `json:"guest_enabled"`
	AccountsEnabled bool `json:"accounts_enabled"`
	Limits          struct {
		Account limitsJSON `json:"account"`
		Guest   limitsJSON `json:"guest"`
	} `json:"limits"`
	RetentionSeconds int64 `json:"retention_seconds"`
}

func (h *Public) guestEnabled(s settings.Settings) bool {
	return s.Enabled && s.GuestEnabled && h.Guests.Configured()
}

func (h *Public) config(w nethttp.ResponseWriter, _ *nethttp.Request) {
	s := h.Settings.Get()
	ws := int64(s.Window / time.Second)
	var out configJSON
	out.Enabled = s.Enabled
	out.GuestEnabled = h.guestEnabled(s)
	out.AccountsEnabled = s.Enabled && h.AccountsEnabled
	out.Limits.Account = limitsJSON{QuotaBytes: s.Account.QuotaBytes, WindowSeconds: ws, MaxFileBytes: s.Account.MaxFileBytes}
	out.Limits.Guest = limitsJSON{QuotaBytes: s.Guest.QuotaBytes, WindowSeconds: ws, MaxFileBytes: s.Guest.MaxFileBytes}
	out.RetentionSeconds = int64(s.Retention / time.Second)
	w.Header().Set("Cache-Control", "public, max-age=60")
	WriteJSON(w, nethttp.StatusOK, out)
}

func (h *Public) guestToken(w nethttp.ResponseWriter, r *nethttp.Request) {
	s := h.Settings.Get()
	if !h.guestEnabled(s) {
		h.Metrics.GuestToken(r.Context(), "disabled")
		WriteError(w, r, guest.ErrDisabled)
		return
	}
	tok, err := h.Guests.Issue(r.Context(), reqmeta.From(r.Context()).IP)
	if err != nil {
		if errors.Is(err, guest.ErrRateLimited) {
			h.Metrics.GuestToken(r.Context(), "rate_limited")
		}
		WriteError(w, r, err)
		return
	}
	h.Metrics.GuestToken(r.Context(), "issued")
	WriteJSON(w, nethttp.StatusCreated, map[string]any{
		"token": tok.Token, "expires_at": tok.ExpiresAt,
		"limits": limitsJSON{QuotaBytes: s.Guest.QuotaBytes, WindowSeconds: int64(s.Window / time.Second), MaxFileBytes: s.Guest.MaxFileBytes},
	})
}

func (h *Public) usage(w nethttp.ResponseWriter, r *nethttp.Request, p *principal.Principal) {
	u, err := h.Files.Usage(r.Context(), p)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, nethttp.StatusOK, u)
}

func (h *Public) listMine(w nethttp.ResponseWriter, r *nethttp.Request, p *principal.Principal) {
	list, err := h.Files.ListMine(r.Context(), p)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, nethttp.StatusOK, map[string]any{"files": list})
}

func (h *Public) upload(w nethttp.ResponseWriter, r *nethttp.Request, p *principal.Principal) {
	if r.ContentLength < 0 {
		WriteError(w, r, errLengthRequired)
		return
	}
	name := r.Header.Get("X-Filename")
	if dec, err := url.PathUnescape(name); err == nil {
		name = dec
	}
	// net/http already stops the body at Content-Length; this makes a
	// longer body an error rather than silently truncated.
	body := nethttp.MaxBytesReader(w, r.Body, r.ContentLength)
	v, err := h.Files.Upload(r.Context(), p, files.UploadInput{
		Name: name, ContentType: r.Header.Get("Content-Type"), Size: r.ContentLength, Body: body,
	})
	if err != nil {
		if e, ok := apperr.As(err); ok && e.Status == nethttp.StatusRequestEntityTooLarge {
			// Don't make the client send a body we already refused.
			w.Header().Set("Connection", "close")
		}
		WriteError(w, r, err)
		return
	}
	w.Header().Set("Location", v.URL)
	WriteJSON(w, nethttp.StatusCreated, v)
}

func (h *Public) deleteMine(w nethttp.ResponseWriter, r *nethttp.Request, p *principal.Principal) {
	if err := h.Files.DeleteMine(r.Context(), p, r.PathValue("id")); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

// metadata is GET /v1/files/{id}: the public description of a live file
// (for file cards). Anyone holding the unguessable id may read it, like
// the file itself.
func (h *Public) metadata(w nethttp.ResponseWriter, r *nethttp.Request) {
	f, err := h.Files.Lookup(r.Context(), r.PathValue("id"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, nethttp.StatusOK, h.Files.View(f))
}

// FileCSP is the Content-Security-Policy of every served file.
const FileCSP = "sandbox; default-src 'none'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'"

func (h *Public) serve(w nethttp.ResponseWriter, r *nethttp.Request) {
	ctx := r.Context()
	f, err := h.Files.Lookup(ctx, r.PathValue("id"))
	switch {
	case errors.Is(err, files.ErrGone):
		h.Metrics.Download(ctx, "gone")
		page(w, nethttp.StatusGone, "This file has expired", "Files shared here are kept for a limited time. This one has expired or was deleted by the person who shared it.")
		return
	case errors.Is(err, files.ErrNotFound):
		h.Metrics.Download(ctx, "not_found")
		page(w, nethttp.StatusNotFound, "File not found", "There is no file at this address. Check the link, or ask for it to be shared again.")
		return
	case err != nil:
		WriteError(w, r, err)
		return
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	left := int64(f.ExpiresAt.Sub(now()) / time.Second)
	if left < 0 {
		left = 0
	}
	pol := files.PolicyFor(f.ContentType)
	hd := w.Header()
	hd.Del("X-Frame-Options")
	hd.Set("Content-Type", pol.ContentType)
	hd.Set("Content-Disposition", files.ContentDisposition(pol.Inline, f.Name))
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", FileCSP)
	hd.Set("Cache-Control", "public, max-age="+strconv.FormatInt(left, 10))
	hd.Set("Cross-Origin-Resource-Policy", "cross-origin")
	hd.Set("Access-Control-Allow-Origin", "*")
	hd.Set("Referrer-Policy", "no-referrer")
	hd.Set("ETag", `"`+f.ID+`"`)
	hd.Set("X-File-Expires-At", f.ExpiresAt.UTC().Format(time.RFC3339))
	hd.Set("X-File-Content-Type", f.ContentType)

	// Optional direct serving from the bucket/CDN, only for inline-safe
	// types (anything else keeps going through here so the headers apply).
	if s := h.Settings.Get(); pol.Inline && h.StorageDriver == settings.DriverS3 && s.Storage.S3.PublicBaseURL != "" {
		h.Metrics.Download(ctx, "redirect")
		nethttp.Redirect(w, r, s.Storage.S3.PublicBaseURL+"/"+f.StorageKey, nethttp.StatusFound)
		return
	}
	rs, err := h.Files.Open(ctx, f)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			logger.FromCtx(ctx).Error().Str("file_id", f.ID).Msg("file body missing from storage")
			h.Metrics.Download(ctx, "not_found")
			page(w, nethttp.StatusNotFound, "File not found", "This file is no longer available.")
			return
		}
		WriteError(w, r, err)
		return
	}
	defer rs.Close()
	h.Metrics.Download(ctx, "ok")
	nethttp.ServeContent(w, r, "", f.CreatedAt, rs)
}
