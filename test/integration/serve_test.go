package integration

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/boson-chat/files/config"
	"github.com/boson-chat/files/http/handlers"
	"github.com/boson-chat/files/internal/services/storage"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(1, 1, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestServeHeaders(t *testing.T) {
	e := newEnv(t, nil)
	auth := e.bearer("usr_frank")
	cases := []struct {
		name, ct    string
		body        []byte
		wantType    string
		disposition string
	}{
		{"logo.svg", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), "application/octet-stream", "attachment"},
		{"page.html", "text/html", []byte("<!doctype html><html><script>alert(1)</script></html>"), "application/octet-stream", "attachment"},
		{"fake.png", "image/png", []byte("<html><script>alert(1)</script></html>"), "application/octet-stream", "attachment"},
		{"notes.txt", "text/plain", []byte("hello world, this is text"), "text/plain; charset=utf-8", "inline"},
		{"pic.png", "image/png", pngBytes(t, 37, 21), "image/png", "inline"},
		{"doc.pdf", "application/pdf", []byte("%PDF-1.4\n%%EOF\n"), "application/pdf", "inline"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := e.upload(auth, c.name, c.ct, c.body)
			if r.status != 201 {
				t.Fatalf("%d %s", r.status, r.body)
			}
			f := r.json(t)
			if f["name"] != c.name || num(f["size"]) != int64(len(c.body)) {
				t.Fatal(f)
			}
			if c.name == "pic.png" && (num(f["width"]) != 37 || num(f["height"]) != 21) {
				t.Fatalf("dimensions %v x %v", f["width"], f["height"])
			}
			if c.name != "pic.png" && f["width"] != nil {
				t.Fatal("dimensions on a non-image", f)
			}
			if !strings.HasPrefix(f["url"].(string), "https://files.test/f/"+f["id"].(string)+"/") || len(f["id"].(string)) < 22 {
				t.Fatal(f["url"])
			}
			g := e.public("GET", filePath(t, f["url"].(string)), nil)
			if g.status != 200 || !bytes.Equal(g.body, c.body) {
				t.Fatalf("get %d", g.status)
			}
			h := g.header
			if h.Get("Content-Type") != c.wantType {
				t.Errorf("Content-Type %q, want %q", h.Get("Content-Type"), c.wantType)
			}
			if !strings.HasPrefix(h.Get("Content-Disposition"), c.disposition+";") || !strings.Contains(h.Get("Content-Disposition"), c.name) {
				t.Errorf("Content-Disposition %q", h.Get("Content-Disposition"))
			}
			if h.Get("X-Content-Type-Options") != "nosniff" {
				t.Error("nosniff missing")
			}
			if h.Get("Content-Security-Policy") != handlers.FileCSP {
				t.Errorf("CSP %q", h.Get("Content-Security-Policy"))
			}
			if h.Get("Cache-Control") != "public, max-age=259200" {
				t.Errorf("Cache-Control %q", h.Get("Cache-Control"))
			}
			if h.Get("Accept-Ranges") != "bytes" {
				t.Error("Accept-Ranges missing")
			}
		})
	}
}

func TestRangeHeadAndMetadata(t *testing.T) {
	e := newEnv(t, nil)
	body := []byte("0123456789abcdefghij")
	r := e.upload(e.bearer("usr_gina"), "r.txt", "text/plain", body)
	f := r.json(t)
	path := filePath(t, f["url"].(string))

	g := e.public("GET", path, nil, "Range", "bytes=5-9")
	if g.status != 206 || string(g.body) != "56789" || g.header.Get("Content-Range") != "bytes 5-9/20" {
		t.Fatalf("range %d %q %q", g.status, g.body, g.header.Get("Content-Range"))
	}
	g = e.public("GET", path, nil, "Range", "bytes=-3")
	if g.status != 206 || string(g.body) != "hij" {
		t.Fatalf("suffix range %d %q", g.status, g.body)
	}
	if g := e.public("GET", path, nil, "Range", "bytes=50-60"); g.status != 416 {
		t.Fatal("unsatisfiable", g.status)
	}
	if g := e.public("HEAD", path, nil); g.status != 200 || g.header.Get("Content-Length") != "20" || len(g.body) != 0 {
		t.Fatal("HEAD", g.status, g.header)
	}
	// Cache-Control counts down; ETag revalidation works.
	e.clock.Advance(time.Hour)
	g = e.public("GET", path, nil)
	if g.header.Get("Cache-Control") != "public, max-age=255600" {
		t.Fatal(g.header.Get("Cache-Control"))
	}
	if g := e.public("GET", path, nil, "If-None-Match", g.header.Get("ETag")); g.status != 304 {
		t.Fatal("etag", g.status)
	}
	// The name in the URL is cosmetic; the id alone works too.
	if g := e.public("GET", "/f/"+f["id"].(string), nil); g.status != 200 {
		t.Fatal(g.status)
	}
	m := e.public("GET", "/v1/files/"+f["id"].(string), nil).json(t)
	if m["name"] != "r.txt" || num(m["size"]) != 20 || m["content_type"] != "text/plain" {
		t.Fatal(m)
	}
	// Unknown ids: 404 page.
	if g := e.public("GET", "/f/AAAAAAAAAAAAAAAAAAAAAA/x", nil); g.status != 404 || !strings.Contains(g.header.Get("Content-Type"), "text/html") {
		t.Fatal(g.status)
	}
	if g := e.public("GET", "/f/short/x", nil); g.status != 404 {
		t.Fatal(g.status)
	}
}

func TestExpiryWorker(t *testing.T) {
	store := storage.NewMemStorage()
	e := newEnv(t, store, func(c *config.Config) {
		c.FilesConfig.Retention, c.FilesConfig.Window, c.FilesConfig.AccountQuota = time.Hour, 3*time.Hour, "1000"
	})
	auth := e.bearer("usr_hank")
	f := e.upload(auth, "e.txt", "text/plain", []byte(strings.Repeat("e", 400))).json(t)
	id, path := f["id"].(string), filePath(t, f["url"].(string))
	exp, _ := time.Parse(time.RFC3339, f["expires_at"].(string))
	if !exp.Equal(e.clock.Now().Add(time.Hour)) {
		t.Fatal("expires_at", exp)
	}

	e.clock.Advance(time.Hour)
	// Past expires_at it's gone for users even before the worker runs.
	if g := e.public("GET", path, nil); g.status != 410 {
		t.Fatal("expired before worker:", g.status)
	}
	if err := e.app.Files.RunExpiry(t.Context()); err != nil {
		t.Fatal(err)
	}
	if store.Has(id[:2] + "/" + id) {
		t.Fatal("worker didn't delete the body")
	}
	if g := e.public("GET", path, nil); g.status != 410 || !strings.Contains(string(g.body), "expired") {
		t.Fatal("after worker:", g.status)
	}
	// Still counted until it leaves the window.
	if u := e.public("GET", "/v1/me/usage", nil, "Authorization", auth).json(t); num(u["used"]) != 400 {
		t.Fatal(u)
	}
	e.clock.Advance(2 * time.Hour)
	if err := e.app.Files.RunExpiry(t.Context()); err != nil {
		t.Fatal(err)
	}
	var rows int
	_ = e.sqlDB.QueryRow("SELECT count(*) FROM files").Scan(&rows)
	if rows != 0 {
		t.Fatal("ledger row not pruned", rows)
	}
	if g := e.public("GET", path, nil); g.status != 404 {
		t.Fatal("pruned:", g.status)
	}
	if u := e.public("GET", "/v1/me/usage", nil, "Authorization", auth).json(t); num(u["used"]) != 0 {
		t.Fatal(u)
	}
}

func TestStalePendingReleased(t *testing.T) {
	e := newEnv(t, nil)
	now := e.clock.Now()
	if _, err := e.sqlDB.Exec(`INSERT INTO files (id, owner, owner_kind, name, content_type, size, storage_key, status, created_at, expires_at)
		VALUES ('PendingPendingPending1', 'usr_ivy', 'account', 'x', 'text/plain', 500, 'Pe/PendingPendingPending1', 'pending', $1, $2)`,
		now, now.Add(72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if u := e.public("GET", "/v1/me/usage", nil, "Authorization", e.bearer("usr_ivy")).json(t); num(u["used"]) != 500 {
		t.Fatal("reservation counts", u)
	}
	e.clock.Advance(2 * time.Hour)
	if err := e.app.Files.RunExpiry(t.Context()); err != nil {
		t.Fatal(err)
	}
	if u := e.public("GET", "/v1/me/usage", nil, "Authorization", e.bearer("usr_ivy")).json(t); num(u["used"]) != 0 {
		t.Fatal("stale reservation kept", u)
	}
}

// The whole path against MinIO (TEST_S3_ENDPOINT): upload, ranged read,
// delete purges the object.
func TestS3EndToEnd(t *testing.T) {
	ep := os.Getenv("TEST_S3_ENDPOINT")
	if ep == "" {
		t.Skip("TEST_S3_ENDPOINT not set")
	}
	bucket := os.Getenv("TEST_S3_BUCKET")
	if bucket == "" {
		bucket = "files-test"
	}
	s3 := storage.NewS3Storage(storage.S3Options{
		Endpoint: ep, Bucket: bucket, Region: "us-east-1", PathStyle: true,
		AccessKey: os.Getenv("TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("TEST_S3_SECRET_KEY"),
	})
	e := newEnv(t, s3)
	auth := e.bearer("usr_s3")
	img := pngBytes(t, 64, 48)
	f := e.upload(auth, "s3.png", "image/png", img).json(t)
	if num(f["width"]) != 64 || num(f["height"]) != 48 {
		t.Fatal(f)
	}
	path := filePath(t, f["url"].(string))
	g := e.public("GET", path, nil, "Range", "bytes=1-3")
	if g.status != 206 || string(g.body) != "PNG" || g.header.Get("Content-Disposition") != `inline; filename=s3.png` {
		t.Fatalf("%d %q %v", g.status, g.body, g.header)
	}
	if g := e.public("GET", path, nil); g.status != 200 || !bytes.Equal(g.body, img) {
		t.Fatal(g.status)
	}
	id := f["id"].(string)
	if r := e.public("DELETE", "/v1/files/"+id, nil, "Authorization", auth); r.status != 204 {
		t.Fatal(r.status)
	}
	rd, err := s3.Open(t.Context(), id[:2]+"/"+id, int64(len(img)))
	if err == nil {
		_, err = io.ReadAll(rd)
		rd.Close()
	}
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("object not purged:", err)
	}
}
