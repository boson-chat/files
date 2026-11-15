//go:build !unix

package storage

import "errors"

// FreeSpace is not available on this platform.
func (l *LocalStorage) FreeSpace() (free, total int64, err error) {
	return 0, 0, errors.New("storage: free space unknown on this platform")
}
