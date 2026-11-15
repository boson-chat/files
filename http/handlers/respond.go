// Package handlers holds the HTTP handlers (thin: decode, call a service,
// encode).
package handlers

import (
	"encoding/json"
	"errors"
	"mime"
	nethttp "net/http"

	"github.com/boson-chat/files/internal/apperr"
	"github.com/boson-chat/files/internal/logger"
)

// MaxBody caps JSON request bodies.
const MaxBody = 64 << 10

// WriteJSON writes v with status (no body for 204).
func WriteJSON(w nethttp.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if status != nethttp.StatusNoContent {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// WriteError renders apperr errors as {"error", "message", "code", ...details}
// and anything else as a logged 500.
func WriteError(w nethttp.ResponseWriter, r *nethttp.Request, err error) {
	if e, ok := apperr.As(err); ok {
		body := map[string]any{}
		for k, v := range e.Details {
			body[k] = v
		}
		body["error"], body["message"], body["code"] = e.Code, e.Message, e.Code
		if e.Status == nethttp.StatusTooManyRequests {
			w.Header().Set("Retry-After", "60")
		}
		WriteJSON(w, e.Status, body)
		return
	}
	logger.FromCtx(r.Context()).Error().Err(err).Str("path", r.URL.Path).Msg("internal error")
	WriteJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "internal_error", "message": "internal error", "code": "internal_error"})
}

var errUnsupportedMedia = apperr.New(nethttp.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")

// Decode reads a JSON body (Content-Type application/json required).
func Decode(w nethttp.ResponseWriter, r *nethttp.Request, dst any) error {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/json" {
		return errUnsupportedMedia
	}
	r.Body = nethttp.MaxBytesReader(w, r.Body, MaxBody)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var mbe *nethttp.MaxBytesError
		if errors.As(err, &mbe) {
			return apperr.New(nethttp.StatusRequestEntityTooLarge, "body_too_large", "request body too large")
		}
		return apperr.Invalid("malformed JSON body")
	}
	return nil
}
