package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// exercise runs the same checks against any driver.
func exercise(t *testing.T, s Storage) {
	t.Helper()
	ctx := context.Background()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	key := "t/" + hex.EncodeToString(b)
	body := bytes.Repeat([]byte("0123456789"), 2000) // 20 kB
	if err := s.Put(ctx, key, bytes.NewReader(body), int64(len(body)), Meta{ContentType: "text/plain"}); err != nil {
		t.Fatal("put:", err)
	}
	t.Cleanup(func() { _ = s.Delete(context.Background(), key) })

	r, err := s.Open(ctx, key, int64(len(body)))
	if err != nil {
		t.Fatal("open:", err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("read back: %v (%d bytes)", err, len(got))
	}
	// Seek + partial read (what Range requests do).
	r, err = s.Open(ctx, key, int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if end, err := r.Seek(0, io.SeekEnd); err != nil || end != int64(len(body)) {
		t.Fatal("seek end", end, err)
	}
	if _, err := r.Seek(12345, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	part := make([]byte, 10)
	if _, err := io.ReadFull(r, part); err != nil || !bytes.Equal(part, body[12345:12355]) {
		t.Fatalf("ranged read %q %v", part, err)
	}
	if _, err := r.Seek(5, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(r, part); err != nil || !bytes.Equal(part, body[5:15]) {
		t.Fatalf("re-seek read %q %v", part, err)
	}
	_ = r.Close()

	// Short and long bodies fail and leave nothing behind.
	short := key + "-short"
	if err := s.Put(ctx, short, strings.NewReader("abc"), 10, Meta{}); !errors.Is(err, ErrShortBody) {
		t.Fatal("short body:", err)
	}
	long := key + "-long"
	if err := s.Put(ctx, long, strings.NewReader("abcdefghijkl"), 10, Meta{}); !errors.Is(err, ErrLongBody) {
		t.Fatal("long body:", err)
	}
	for _, k := range []string{short, long} {
		if r, err := s.Open(ctx, k, 10); err == nil {
			_, rerr := io.ReadAll(r)
			_ = r.Close()
			if rerr == nil {
				t.Fatalf("%s was stored", k)
			}
		}
	}

	if err := s.Delete(ctx, key); err != nil {
		t.Fatal("delete:", err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatal("delete twice:", err)
	}
	if r, err := s.Open(ctx, key, int64(len(body))); err == nil {
		_, rerr := io.ReadAll(r)
		_ = r.Close()
		if !errors.Is(rerr, ErrNotFound) {
			t.Fatal("deleted object still readable", rerr)
		}
	} else if !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.Probe(ctx); err != nil {
		t.Fatal("probe:", err)
	}
}

func TestLocalStorage(t *testing.T) {
	s, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, s)
	if err := s.Put(context.Background(), "../escape", strings.NewReader("x"), 1, Meta{}); err == nil {
		t.Fatal("path escape accepted")
	}
}

func TestMemStorage(t *testing.T) { exercise(t, NewMemStorage()) }

// TestS3Storage runs against MinIO (CI service container, or
// `make minio-up`). Skipped unless TEST_S3_ENDPOINT is set.
func TestS3Storage(t *testing.T) {
	ep := os.Getenv("TEST_S3_ENDPOINT")
	if ep == "" {
		t.Skip("TEST_S3_ENDPOINT not set")
	}
	bucket := os.Getenv("TEST_S3_BUCKET")
	if bucket == "" {
		bucket = "files-test"
	}
	s := NewS3Storage(S3Options{
		Endpoint: ep, Bucket: bucket, Region: "us-east-1", PathStyle: true,
		AccessKey: os.Getenv("TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("TEST_S3_SECRET_KEY"),
		PartSize: 5 << 20,
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.Probe(ctx); err != nil {
		t.Fatal(err)
	}
	exercise(t, s)

	// Multipart: a body over the part size streams in parts.
	big := int64(11 << 20)
	key := "t/multipart"
	if err := s.Put(ctx, key, io.LimitReader(rand.Reader, big), big, Meta{ContentType: "application/octet-stream"}); err != nil {
		t.Fatal("multipart put:", err)
	}
	defer s.Delete(ctx, key)
	r, err := s.Open(ctx, key, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	n, err := io.Copy(io.Discard, r)
	if err != nil || n != big {
		t.Fatal("multipart read", n, err)
	}
}
