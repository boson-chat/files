package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	nethttp "net/http"
	"strconv"

	"github.com/boson-chat/files/http/middleware"
	"github.com/boson-chat/files/internal/apperr"
	"github.com/boson-chat/files/internal/configfile"
	"github.com/boson-chat/files/internal/services/files"
)

// Internal serves the admin API on the internal listener (X-Internal-Token).
type Internal struct {
	Files files.FileServiceImpl
	Token string
	// ConfigStatus is the stack config file watcher status.
	ConfigStatus func() configfile.Status
}

// Routes mounts the internal routes.
func (h *Internal) Routes(mux *nethttp.ServeMux) {
	guard := func(fn nethttp.HandlerFunc) nethttp.Handler {
		return middleware.RequireInternalToken(h.Token, WriteError, fn)
	}
	mux.Handle("GET /internal/files/status", guard(h.status))
	mux.Handle("GET /internal/files", guard(h.search))
	mux.Handle("DELETE /internal/files/{id}", guard(h.delete))
	mux.Handle("GET /internal/files/usage", guard(h.usage))
	mux.Handle("PUT /internal/quota/{user}", guard(h.setQuota))
	mux.Handle("DELETE /internal/quota/{user}", guard(h.clearQuota))
	// Like the other services: no token needed for the config file status.
	mux.Handle("GET /internal/config/status", configfile.StatusHandler(h.ConfigStatus))
}

type statusJSON struct {
	files.Status
	ConfigFile configfile.Status `json:"config_file"`
	// RestartRequired lists files.* keys changed in the config file that
	// only apply after a restart (the storage driver settings).
	RestartRequired []string `json:"restart_required"`
}

func (h *Internal) status(w nethttp.ResponseWriter, r *nethttp.Request) {
	st := statusJSON{Status: h.Files.Status(r.Context()), RestartRequired: []string{}}
	if h.ConfigStatus != nil {
		st.ConfigFile = h.ConfigStatus()
		st.RestartRequired = st.ConfigFile.RestartRequired
		if !st.ConfigFile.OK {
			st.OK = false
		}
	} else {
		st.ConfigFile = configfile.Status{OK: true, RestartRequired: []string{}}
	}
	w.Header().Set("Cache-Control", "no-store")
	WriteJSON(w, nethttp.StatusOK, st)
}

func (h *Internal) search(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	res, err := h.Files.Search(r.Context(), files.SearchInput{
		User: q.Get("user"), Q: q.Get("q"), Status: q.Get("status"), Cursor: q.Get("cursor"), Limit: limit,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, nethttp.StatusOK, res)
}

func (h *Internal) delete(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := h.Files.AdminDelete(r.Context(), r.PathValue("id"), middleware.ActingUser(r)); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *Internal) usage(w nethttp.ResponseWriter, r *nethttp.Request) {
	u, err := h.Files.AdminUsage(r.Context(), r.URL.Query().Get("user"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, nethttp.StatusOK, u)
}

// setQuota: body {quota_bytes, max_file_bytes} (either may be null = class
// default) or null / empty to remove the override.
func (h *Internal) setQuota(w nethttp.ResponseWriter, r *nethttp.Request) {
	raw, err := io.ReadAll(nethttp.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		WriteError(w, r, apperr.Invalid("body too large"))
		return
	}
	var in *files.OverrideInput
	if t := bytes.TrimSpace(raw); len(t) > 0 && !bytes.Equal(t, []byte("null")) {
		in = &files.OverrideInput{}
		dec := json.NewDecoder(bytes.NewReader(t))
		dec.DisallowUnknownFields()
		if err := dec.Decode(in); err != nil {
			WriteError(w, r, apperr.Invalid("body must be {quota_bytes, max_file_bytes} or null"))
			return
		}
	}
	u, err := h.Files.SetOverride(r.Context(), r.PathValue("user"), in, middleware.ActingUser(r))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, nethttp.StatusOK, u)
}

func (h *Internal) clearQuota(w nethttp.ResponseWriter, r *nethttp.Request) {
	u, err := h.Files.SetOverride(r.Context(), r.PathValue("user"), nil, middleware.ActingUser(r))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, nethttp.StatusOK, u)
}
