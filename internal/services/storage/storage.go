// Package storage keeps file bodies: on a local volume (the default) or in
// an S3-compatible bucket (AWS, Cloudflare R2, MinIO). Bodies are streamed
// in and out; nothing holds a whole file in memory.
package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
)

// Meta is stored alongside an object (used when a bucket/CDN serves it
// directly).
type Meta struct {
	ContentType        string
	ContentDisposition string
	CacheControl       string
}

// ErrShortBody is returned by Put when the reader ends before size bytes.
var ErrShortBody = errors.New("storage: body shorter than its declared size")

// ErrLongBody is returned by Put when the reader has more than size bytes.
var ErrLongBody = errors.New("storage: body longer than its declared size")

// Storage is a blob store keyed by opaque keys ("ab/<id>").
type Storage interface {
	// Driver is "local", "s3" or "mem".
	Driver() string
	// Put streams exactly size bytes from r into key. It fails (and leaves
	// nothing behind) when r yields fewer or more bytes, or errors.
	Put(ctx context.Context, key string, r io.Reader, size int64, meta Meta) error
	// Open returns a seekable reader over key (size is the stored size).
	// Range requests seek; remote drivers fetch lazily from the offset.
	Open(ctx context.Context, key string, size int64) (io.ReadSeekCloser, error)
	// Delete removes key; a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// Probe checks the store is reachable and writable/readable.
	Probe(ctx context.Context) error
}

// exact copies exactly size bytes from r to w, failing on a short or long
// body.
func exact(w io.Writer, r io.Reader, size int64) error {
	n, err := io.Copy(w, io.LimitReader(r, size))
	if err != nil {
		return err
	}
	if n < size {
		return ErrShortBody
	}
	var one [1]byte
	if m, _ := io.ReadFull(r, one[:]); m > 0 {
		return ErrLongBody
	}
	return nil
}

// ---- in-memory (tests) ----

// MemStorage keeps objects in memory (tests only).
type MemStorage struct {
	mu      sync.Mutex
	objects map[string][]byte
	// PutHook runs before Put stores (tests: block, fail).
	PutHook func(key string) error
}

func NewMemStorage() *MemStorage { return &MemStorage{objects: map[string][]byte{}} }

func (m *MemStorage) Driver() string { return "mem" }

func (m *MemStorage) Put(_ context.Context, key string, r io.Reader, size int64, _ Meta) error {
	var buf writerBuf
	if err := exact(&buf, r, size); err != nil {
		return err
	}
	if m.PutHook != nil {
		if err := m.PutHook(key); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = buf.b
	return nil
}

func (m *MemStorage) Open(_ context.Context, key string, _ int64) (io.ReadSeekCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[key]
	if !ok {
		return nil, ErrNotFound
	}
	return nopCloser{bytes.NewReader(b)}, nil
}

func (m *MemStorage) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *MemStorage) Probe(context.Context) error { return nil }

// Has reports whether key is stored.
func (m *MemStorage) Has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[key]
	return ok
}

// Len is the number of stored objects.
func (m *MemStorage) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.objects)
}

// ErrNotFound: the object doesn't exist.
var ErrNotFound = errors.New("storage: object not found")

type writerBuf struct{ b []byte }

func (w *writerBuf) Write(p []byte) (int, error) { w.b = append(w.b, p...); return len(p), nil }

type nopCloser struct{ io.ReadSeeker }

func (nopCloser) Close() error { return nil }

// DiscardStorage accepts any body and stores nothing (quota tests with
// large sizes). Open always fails.
type DiscardStorage struct{}

func (DiscardStorage) Driver() string { return "discard" }
func (DiscardStorage) Put(_ context.Context, _ string, r io.Reader, size int64, _ Meta) error {
	return exact(io.Discard, r, size)
}
func (DiscardStorage) Open(context.Context, string, int64) (io.ReadSeekCloser, error) {
	return nil, ErrNotFound
}
func (DiscardStorage) Delete(context.Context, string) error { return nil }
func (DiscardStorage) Probe(context.Context) error          { return nil }

// FreeSpacer is implemented by drivers that can report the free space of
// their volume (local).
type FreeSpacer interface {
	FreeSpace() (free, total int64, err error)
}
