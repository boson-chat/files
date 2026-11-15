// Package ids makes identifiers: unguessable base62 file ids and prefixed
// ULIDs for requests and guest tokens.
package ids

import (
	"crypto/rand"
	"strings"

	"github.com/oklog/ulid/v2"
)

const (
	Request = "req"
	Guest   = "gtk"
)

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// FileIDLen is the length of a file id: 22 base62 chars ≈ 131 random bits.
const FileIDLen = 22

// FileID returns FileIDLen chars of crypto-random base62 (rejection
// sampling, so every char is uniform).
func FileID() string {
	out := make([]byte, 0, FileIDLen)
	buf := make([]byte, 32)
	for len(out) < FileIDLen {
		_, _ = rand.Read(buf)
		for _, b := range buf {
			if b < 248 { // 248 = 62*4: keeps the distribution uniform
				out = append(out, base62[b%62])
				if len(out) == FileIDLen {
					break
				}
			}
		}
	}
	return string(out)
}

// ValidFileID reports whether s looks like a file id.
func ValidFileID(s string) bool {
	if len(s) < FileIDLen || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(base62, rune(s[i])) {
			return false
		}
	}
	return true
}

// New returns prefix_ULID.
func New(prefix string) string { return prefix + "_" + ulid.Make().String() }
