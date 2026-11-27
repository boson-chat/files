package middleware_test

import (
	"context"
	nethttp "net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/boson-chat/files/http/middleware"
	"github.com/boson-chat/files/internal/origins"
)

func cors(t *testing.T, h nethttp.Handler, method, path, origin string, preflight bool) *httptest.ResponseRecorder {
	t.Helper()
	m := method
	if preflight {
		m = nethttp.MethodOptions
	}
	req := httptest.NewRequest(m, path, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if preflight {
		req.Header.Set("Access-Control-Request-Method", method)
		req.Header.Set("Access-Control-Request-Headers", "authorization, x-filename")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

var teapot = nethttp.HandlerFunc(func(w nethttp.ResponseWriter, _ *nethttp.Request) { w.WriteHeader(nethttp.StatusTeapot) })

func TestCORSAnyByDefault(t *testing.T) {
	h := middleware.CORS(teapot)
	rec := cors(t, h, nethttp.MethodPost, "/v1/files", "https://anything.test", true)
	if rec.Code != nethttp.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "*" ||
		rec.Header().Get("Access-Control-Allow-Headers") != "Authorization, Content-Type, X-Filename, Range" {
		t.Fatalf("preflight: %d %v", rec.Code, rec.Header())
	}
	if rec := cors(t, h, nethttp.MethodGet, "/v1/me/usage", "", false); rec.Code != nethttp.StatusTeapot || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("no origin: %v", rec.Header())
	}
}

func TestCORSDirectory(t *testing.T) {
	auth := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		_, _ = w.Write([]byte(`{"origins":["https://chat.other.test"]}`))
	}))
	defer auth.Close()
	f := origins.NewFetcher(auth.URL, time.Hour, nil)
	p, err := origins.Parse("directory,https://chat.own.test", f)
	if err != nil {
		t.Fatal(err)
	}
	h := middleware.CORSPolicy(p)(teapot)
	allow := func(method, path, origin string) string {
		return cors(t, h, method, path, origin, true).Header().Get("Access-Control-Allow-Origin")
	}
	// Before the first fetch: the own origin only.
	if allow("POST", "/v1/files", "https://chat.own.test") != "https://chat.own.test" || allow("POST", "/v1/files", "https://chat.other.test") != "" {
		t.Fatal("before the fetch")
	}
	if _, err := f.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/v1/files"}, {"GET", "/v1/me/usage"}, {"GET", "/v1/me/files"}, {"POST", "/v1/guest-token"}, {"DELETE", "/v1/files/f_1"},
	} {
		if got := allow(c.method, c.path, "https://Chat.Other.test"); got != "https://Chat.Other.test" {
			t.Errorf("%s %s listed: %q", c.method, c.path, got)
		}
		if got := allow(c.method, c.path, "https://evil.test"); got != "" {
			t.Errorf("%s %s unlisted: %q", c.method, c.path, got)
		}
	}
	// Downloads, metadata and the public config stay open to any page.
	for _, c := range []struct{ method, path string }{
		{"GET", "/f/f_1"}, {"GET", "/f/f_1/cat.png"}, {"HEAD", "/v1/files/f_1"}, {"GET", "/v1/files/f_1"}, {"GET", "/v1/config"},
	} {
		if got := allow(c.method, c.path, "https://evil.test"); got != "*" {
			t.Errorf("%s %s: %q", c.method, c.path, got)
		}
	}
	// A real (non-preflight) request from an unlisted page still reaches the
	// handler but gets no CORS headers (the browser hides the answer).
	rec := cors(t, h, nethttp.MethodGet, "/v1/me/usage", "https://evil.test", false)
	if rec.Code != nethttp.StatusTeapot || rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Vary") != "Origin" {
		t.Fatalf("unlisted get: %d %v", rec.Code, rec.Header())
	}
}
