package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	nethttp "net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boson-chat/files/config"
	"github.com/boson-chat/files/internal/apperr"
	"github.com/boson-chat/files/internal/principal"
	"github.com/boson-chat/files/internal/services/files"
	"github.com/boson-chat/files/internal/services/storage"
)

const (
	day = 24 * time.Hour
	gb  = int64(1e9)
)

// The contract's example: 5 GB per rolling 3 days. Day 1 uploads 1 GB, day
// 3 reaches 5 GB, day 4 has 1 GB free again. Real sizes, streamed through
// the service into a storage that discards the bytes.
func TestQuotaExample5GBPer3Days(t *testing.T) {
	e := newEnv(t, storage.DiscardStorage{}, func(c *config.Config) { c.FilesConfig.AccountMax = "5GB" })
	ctx := context.Background()
	alice := &principal.Principal{Kind: principal.KindAccount, Owner: "usr_alice"}
	up := func(size int64) (*files.FileView, error) {
		return e.app.Files.Upload(ctx, alice, files.UploadInput{Name: "big.bin", ContentType: "application/octet-stream", Size: size, Body: &zeros{left: size}})
	}
	day1 := e.clock.Now()

	if _, err := up(1 * gb); err != nil {
		t.Fatal("day 1:", err)
	}
	e.clock.Advance(2 * day) // day 3
	if _, err := up(4 * gb); err != nil {
		t.Fatal("day 3, 4 GB:", err)
	}
	u, err := e.app.Files.Usage(ctx, alice)
	if err != nil || u.Used != 5*gb || u.Quota != 5*gb || u.WindowSeconds != int64(3*day/time.Second) {
		t.Fatalf("day 3 usage %+v %v", u, err)
	}
	if len(u.Frees) != 2 || u.Frees[0].Bytes != gb || !u.Frees[0].At.Equal(day1.Add(3*day)) {
		t.Fatalf("frees %+v", u.Frees)
	}
	// Full on day 3: even one more byte is refused, with when 1 GB frees.
	_, err = up(1)
	ae, ok := apperr.As(err)
	if !ok || ae.Code != "quota_exceeded" || ae.Status != nethttp.StatusRequestEntityTooLarge {
		t.Fatal("day 3 over quota:", err)
	}
	if ae.Details["used"] != 5*gb || ae.Details["quota"] != 5*gb || ae.Details["window_seconds"] != int64(259200) {
		t.Fatalf("details %+v", ae.Details)
	}
	if at, _ := ae.Details["frees_at"].(*time.Time); at == nil || !at.Equal(day1.Add(3*day)) {
		t.Fatalf("frees_at %v", ae.Details["frees_at"])
	}

	e.clock.Advance(1 * day) // day 4: the day-1 upload left the window
	u, _ = e.app.Files.Usage(ctx, alice)
	if u.Used != 4*gb {
		t.Fatal("day 4 used", u.Used)
	}
	if _, err := up(1 * gb); err != nil {
		t.Fatal("day 4, 1 GB:", err)
	}
	if _, err := up(1); err == nil {
		t.Fatal("day 4: only 1 GB was free")
	}
}

func TestDeletingDoesNotRefund(t *testing.T) {
	store := storage.NewMemStorage()
	e := newEnv(t, store, func(c *config.Config) {
		c.FilesConfig.AccountQuota, c.FilesConfig.Window, c.FilesConfig.Retention = "1000", 2*time.Hour, 72*time.Hour
	})
	auth := e.bearer("usr_bob")
	r := e.upload(auth, "a.txt", "text/plain", []byte(strings.Repeat("a", 600)))
	if r.status != 201 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	f := r.json(t)
	id, path := f["id"].(string), filePath(t, f["url"].(string))
	if !store.Has(id[:2] + "/" + id) {
		t.Fatal("body not stored")
	}

	if r := e.public("DELETE", "/v1/files/"+id, nil, "Authorization", auth); r.status != 204 {
		t.Fatalf("delete %d %s", r.status, r.body)
	}
	// Removed immediately …
	if store.Has(id[:2] + "/" + id) {
		t.Fatal("body still in storage")
	}
	if r := e.public("GET", path, nil); r.status != 410 || !strings.Contains(string(r.body), "expired") {
		t.Fatalf("deleted file: %d", r.status)
	}
	if r := e.public("GET", "/v1/me/files", nil, "Authorization", auth); len(r.json(t)["files"].([]any)) != 0 {
		t.Fatal("deleted file still listed")
	}
	// … but still counted.
	u := e.public("GET", "/v1/me/usage", nil, "Authorization", auth).json(t)
	if num(u["used"]) != 600 || num(u["quota"]) != 1000 {
		t.Fatalf("usage %v", u)
	}
	r = e.upload(auth, "b.txt", "text/plain", []byte(strings.Repeat("b", 600)))
	if r.status != 413 || r.json(t)["error"] != "quota_exceeded" {
		t.Fatalf("refund happened: %d %s", r.status, r.body)
	}
	body := r.json(t)
	if num(body["used"]) != 600 || num(body["quota"]) != 1000 || num(body["window_seconds"]) != 7200 || body["frees_at"] == nil {
		t.Fatalf("quota body %v", body)
	}
	// Someone else can't delete it either way (404, not 403).
	if r := e.public("DELETE", "/v1/files/"+id, nil, "Authorization", e.bearer("usr_mallory")); r.status != 404 {
		t.Fatal(r.status)
	}

	// Once it left the window the space is back.
	e.clock.Advance(2 * time.Hour)
	if r := e.upload(auth, "b.txt", "text/plain", []byte(strings.Repeat("b", 600))); r.status != 201 {
		t.Fatalf("after window: %d %s", r.status, r.body)
	}
}

// Concurrent uploads reserve under a lock: they can never add up to more
// than the quota, however they interleave.
func TestConcurrentUploadsCannotExceedQuota(t *testing.T) {
	store := storage.NewMemStorage()
	store.PutHook = func(string) error { time.Sleep(20 * time.Millisecond); return nil } // keep uploads overlapping
	e := newEnv(t, store, func(c *config.Config) { c.FilesConfig.AccountQuota = "10KB" })
	auth := e.bearer("usr_carol")
	body := []byte(strings.Repeat("x", 1000))

	var ok, over atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := e.upload(auth, fmt.Sprintf("f%d.txt", i), "text/plain", body)
			switch {
			case r.status == 201:
				ok.Add(1)
			case r.status == 413 && strings.Contains(string(r.body), "quota_exceeded"):
				over.Add(1)
			default:
				t.Errorf("unexpected %d %s", r.status, r.body)
			}
		}()
	}
	close(start)
	wg.Wait()
	if ok.Load() != 10 || over.Load() != 20 {
		t.Fatalf("accepted %d, refused %d; want 10 / 20", ok.Load(), over.Load())
	}
	u := e.public("GET", "/v1/me/usage", nil, "Authorization", auth).json(t)
	if num(u["used"]) != 10000 || store.Len() != 10 {
		t.Fatalf("used %v, stored %d", u["used"], store.Len())
	}
}

type failingReader struct {
	n   int
	err error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, f.err
	}
	n := min(len(p), f.n)
	f.n -= n
	return n, nil
}

// A failed upload gives its reservation back.
func TestFailedUploadReleasesReservation(t *testing.T) {
	store := storage.NewMemStorage()
	e := newEnv(t, store, func(c *config.Config) { c.FilesConfig.AccountQuota = "1000" })
	ctx := context.Background()
	p := &principal.Principal{Kind: principal.KindAccount, Owner: "usr_dave"}
	for _, rd := range []io.Reader{
		&failingReader{n: 300, err: errors.New("connection reset")},
		&failingReader{n: 300, err: io.EOF}, // short body
	} {
		_, err := e.app.Files.Upload(ctx, p, files.UploadInput{Name: "x", ContentType: "text/plain", Size: 900, Body: rd})
		if err == nil {
			t.Fatal("upload succeeded")
		}
	}
	u, _ := e.app.Files.Usage(ctx, p)
	if u.Used != 0 || store.Len() != 0 {
		t.Fatalf("reservation kept: used %d, stored %d", u.Used, store.Len())
	}
	var rows int
	_ = e.sqlDB.QueryRow("SELECT count(*) FROM files").Scan(&rows)
	if rows != 0 {
		t.Fatal("rows left", rows)
	}
	if _, err := e.app.Files.Upload(ctx, p, files.UploadInput{Name: "x", ContentType: "text/plain", Size: 900, Body: strings.NewReader(strings.Repeat("y", 900))}); err != nil {
		t.Fatal("quota not available after failures:", err)
	}
	// Over HTTP: a body shorter than Content-Length.
	r := e.public("GET", "/v1/me/usage", nil, "Authorization", e.bearer("usr_dave"))
	if num(r.json(t)["used"]) != 900 {
		t.Fatal(string(r.body))
	}
}

func TestFileTooLargeAndRequestChecks(t *testing.T) {
	e := newEnv(t, nil, func(c *config.Config) { c.FilesConfig.AccountMax = "100" })
	auth := e.bearer("usr_erin")
	r := e.upload(auth, "big", "text/plain", []byte(strings.Repeat("z", 101)))
	if r.status != 413 || r.json(t)["error"] != "file_too_large" || num(r.json(t)["max"]) != 100 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	if r := e.upload(auth, "empty", "text/plain", []byte{}); r.status != 400 || r.json(t)["error"] != "empty_file" {
		t.Fatalf("empty: %d %s", r.status, r.body)
	}
	// No credentials / bad token.
	if r := e.public("POST", "/v1/files", []byte("hi"), "X-Filename", "a"); r.status != 401 {
		t.Fatal(r.status)
	}
	if r := e.upload("Bearer nope", "a", "text/plain", []byte("hi")); r.status != 401 {
		t.Fatal(r.status)
	}
	if r := e.upload("Guest nope", "a", "text/plain", []byte("hi")); r.status != 401 || r.json(t)["error"] != "invalid_guest_token" {
		t.Fatal(r.status, string(r.body))
	}
	// Chunked (no Content-Length) is refused.
	req, _ := nethttp.NewRequest("POST", e.srv.URL+"/v1/files", io.NopCloser(strings.NewReader("chunky")))
	req.ContentLength = -1
	req.Header.Set("Authorization", auth)
	res, err := nethttp.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != nethttp.StatusLengthRequired {
		t.Fatal("chunked:", res.StatusCode)
	}
}
