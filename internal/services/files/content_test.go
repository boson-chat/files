package files

import (
	"strings"
	"testing"
)

func TestPolicyFor(t *testing.T) {
	cases := map[string]ServePolicy{
		"image/png":                {"image/png", true},
		"image/jpeg":               {"image/jpeg", true},
		"video/mp4":                {"video/mp4", true},
		"audio/mpeg":               {"audio/mpeg", true},
		"application/pdf":          {"application/pdf", true},
		"text/plain":               {"text/plain; charset=utf-8", true},
		"text/plain; charset=utf8": {"text/plain; charset=utf-8", true},
		"image/svg+xml":            {"application/octet-stream", false},
		"text/html":                {"application/octet-stream", false},
		"text/html; charset=utf-8": {"application/octet-stream", false},
		"application/javascript":   {"application/octet-stream", false},
		"application/zip":          {"application/octet-stream", false},
		"":                         {"application/octet-stream", false},
	}
	for in, want := range cases {
		if got := PolicyFor(in); got != want {
			t.Errorf("PolicyFor(%q) = %+v, want %+v", in, got, want)
		}
	}
}

func TestDetectType(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n0000")
	html := []byte("<!DOCTYPE html><html><script>alert(1)</script>")
	cases := []struct {
		declared string
		head     []byte
		want     string
	}{
		{"image/png", png, "image/png"},
		{"", png, "image/png"},
		{"application/octet-stream", png, "image/png"},
		{"image/png", html, "application/octet-stream"}, // HTML posing as an image
		{"text/plain", html, "text/html"},               // HTML posing as text
		{"text/plain; charset=utf-8", []byte("hello"), "text/plain"},
		{"image/svg+xml", []byte("<svg xmlns='http://www.w3.org/2000/svg'/>"), "image/svg+xml"},
		{"text/html", html, "text/html"},
		{"not a type", []byte("hello"), "text/plain"},
	}
	for _, c := range cases {
		if got := DetectType(c.declared, c.head); got != c.want {
			t.Errorf("DetectType(%q) = %q, want %q", c.declared, got, c.want)
		}
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"photo.png":         "photo.png",
		"../../etc/passwd":  "passwd",
		`C:\Users\me\a.txt`: "a.txt",
		"  spaced .txt  ":   "spaced .txt",
		"evil\u202egpj.exe": "evilgpj.exe",
		"new\nline.txt":     "newline.txt",
		"":                  "file",
		"...":               "file",
		`quote"d.txt`:       "quote_d.txt",
		"héllo wörld.pdf":   "héllo wörld.pdf",
	}
	for in, want := range cases {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("é", 300) + ".jpeg"
	got := SanitizeName(long)
	if len(got) > MaxNameBytes || !strings.HasSuffix(got, ".jpeg") {
		t.Fatalf("long name: %d %q", len(got), got[len(got)-10:])
	}
}

func TestContentDisposition(t *testing.T) {
	if got := ContentDisposition(true, "a b.png"); got != `inline; filename="a b.png"` {
		t.Fatal(got)
	}
	if got := ContentDisposition(false, "x.svg"); got != "attachment; filename=x.svg" {
		t.Fatal(got)
	}
	if got := ContentDisposition(false, "ü.txt"); !strings.HasPrefix(got, "attachment; filename*=utf-8''%C3%BC.txt") {
		t.Fatal(got)
	}
}
