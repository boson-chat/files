package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// LocalStorage keeps objects under a directory (a volume in the stack).
type LocalStorage struct{ dir string }

// NewLocalStorage creates dir if needed.
func NewLocalStorage(dir string) (*LocalStorage, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	return &LocalStorage{dir: dir}, nil
}

func (l *LocalStorage) Driver() string { return "local" }

// safeKey rejects keys that would escape the directory.
func safeKey(key string) (string, error) {
	clean := path.Clean("/" + key)[1:]
	if clean == "" || clean != key || strings.Contains(key, "..") || strings.ContainsRune(key, '\\') {
		return "", errors.New("storage: invalid key")
	}
	return clean, nil
}

func (l *LocalStorage) path(key string) (string, error) {
	k, err := safeKey(key)
	if err != nil {
		return "", err
	}
	return filepath.Join(l.dir, filepath.FromSlash(k)), nil
}

func (l *LocalStorage) Put(_ context.Context, key string, r io.Reader, size int64, _ Meta) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".upload-*")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := exact(tmp, r, size); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return err
	}
	ok = true
	return nil
}

func (l *LocalStorage) Open(_ context.Context, key string, _ int64) (io.ReadSeekCloser, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

func (l *LocalStorage) Delete(_ context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Probe writes, reads back and removes a small file.
func (l *LocalStorage) Probe(_ context.Context) error {
	f, err := os.CreateTemp(l.dir, ".probe-*")
	if err != nil {
		return fmt.Errorf("storage: %s not writable: %w", l.dir, err)
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.Write([]byte("ok")); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	b, err := os.ReadFile(name)
	if err != nil || !bytes.Equal(b, []byte("ok")) {
		return fmt.Errorf("storage: %s not readable: %v", l.dir, err)
	}
	return nil
}
