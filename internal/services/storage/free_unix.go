//go:build unix

package storage

import "golang.org/x/sys/unix"

// FreeSpace reports the free and total bytes of the volume holding the
// directory (statfs; free is what an unprivileged writer may use).
func (l *LocalStorage) FreeSpace() (free, total int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(l.dir, &st); err != nil {
		return 0, 0, err
	}
	bs := int64(st.Bsize) //nolint:unconvert // uint32 on darwin
	return int64(st.Bavail) * bs, int64(st.Blocks) * bs, nil
}
