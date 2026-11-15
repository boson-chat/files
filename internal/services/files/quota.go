package files

import (
	"time"

	filesrepo "github.com/boson-chat/files/internal/db/repositories/files"
)

// Quota window math. used(key) is the sum of the sizes of key's uploads
// created after now - window, whatever their status (deleting doesn't
// refund). An upload of size fits when used + size <= quota.

// WindowStart is the exclusive lower bound of the window at now: uploads
// created strictly after it count.
func WindowStart(now time.Time, window time.Duration) time.Time { return now.Add(-window) }

// Used sums the entries.
func Used(entries []filesrepo.Entry) int64 {
	var n int64
	for _, e := range entries {
		n += e.Size
	}
	return n
}

// FreesAt is when enough of the oldest entries have left the window for an
// upload of size to fit; nil when it fits now or can never fit (size >
// quota). entries are oldest first. An entry created at t counts until
// t + window (exclusive), so it is gone at exactly t + window.
func FreesAt(entries []filesrepo.Entry, quota, size int64, window time.Duration) *time.Time {
	used := Used(entries)
	if used+size <= quota || size > quota {
		return nil
	}
	for _, e := range entries {
		used -= e.Size
		if used+size <= quota {
			t := e.CreatedAt.Add(window).UTC()
			return &t
		}
	}
	return nil
}

// Free is one step of the schedule of space coming back.
type Free struct {
	Bytes int64     `json:"bytes"`
	At    time.Time `json:"at"`
}

// Schedule lists when each counted upload leaves the window (merged per
// instant, oldest first, at most limit steps; the last step absorbs the
// rest).
func Schedule(entries []filesrepo.Entry, window time.Duration, limit int) []Free {
	out := []Free{}
	for _, e := range entries {
		at := e.CreatedAt.Add(window).UTC()
		if n := len(out); n > 0 && (out[n-1].At.Equal(at) || n >= limit) {
			out[n-1].Bytes += e.Size
			if n >= limit {
				out[n-1].At = at
			}
			continue
		}
		out = append(out, Free{Bytes: e.Size, At: at})
	}
	return out
}
