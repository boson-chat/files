// Package middleware holds the HTTP middleware.
package middleware

import (
	"fmt"
	nethttp "net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/boson-chat/files/internal/ids"
	"github.com/boson-chat/files/internal/logger"
	"github.com/boson-chat/files/internal/reqmeta"
)

type statusRecorder struct {
	nethttp.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Unwrap() nethttp.ResponseWriter { return s.ResponseWriter }

// Logging attaches a request-scoped logger and logs each request (never the query string).
func Logging(next nethttp.Handler) nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		start := time.Now()
		reqID := ids.New(ids.Request)
		l := logger.FromCtx(r.Context()).With().Str("request_id", reqID).Logger()
		ctx := logger.WithLogger(r.Context(), l)
		rec := &statusRecorder{ResponseWriter: w, status: nethttp.StatusOK}
		w.Header().Set("X-Request-Id", reqID)
		next.ServeHTTP(rec, r.WithContext(ctx))
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics" {
			return
		}
		ev := l.Info()
		if rec.status >= 500 {
			ev = l.Error()
		}
		ev.Str("method", r.Method).Str("path", logPath(r.URL.Path)).Int("status", rec.status).
			Str("ip", reqmeta.From(ctx).IP).Dur("duration", time.Since(start)).Msg("request")
	})
}

// logPath drops the file name from /f/{id}/{name} (names can be personal).
func logPath(p string) string {
	if rest, ok := strings.CutPrefix(p, "/f/"); ok {
		id, _, _ := strings.Cut(rest, "/")
		return "/f/" + id
	}
	return p
}

// Recover turns panics into 500s.
func Recover(next nethttp.Handler) nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == nethttp.ErrAbortHandler {
					panic(v)
				}
				logger.FromCtx(r.Context()).Error().Str("panic", fmt.Sprint(v)).Bytes("stack", debug.Stack()).Msg("panic")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(nethttp.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"internal_error","message":"internal error","code":"internal_error"}`))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// SecurityHeaders sets conservative defaults. The file handler overrides
// Cache-Control, the CSP and X-Frame-Options.
func SecurityHeaders(next nethttp.Handler) nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// CORS: the files API is called cross-origin by the boson clients with
// bearer/guest tokens (never cookies), so any origin may call it.
func CORS(next nethttp.Handler) nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		if r.Header.Get("Origin") == "" {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Expose-Headers", "Retry-After, X-Request-Id, Content-Length, Content-Range, Content-Disposition, Accept-Ranges, X-File-Expires-At, X-File-Content-Type")
		if r.Method == nethttp.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, DELETE")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Filename, Range")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(nethttp.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Chain applies middleware so the first listed runs first.
func Chain(h nethttp.Handler, mw ...func(nethttp.Handler) nethttp.Handler) nethttp.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}
