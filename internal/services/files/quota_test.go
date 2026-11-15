package files

import (
	"testing"
	"time"

	filesrepo "github.com/boson-chat/files/internal/db/repositories/files"
)

const gb = int64(1e9)

func TestFreesAt(t *testing.T) {
	day := 24 * time.Hour
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	window := 3 * day
	entries := []filesrepo.Entry{{Size: 1 * gb, CreatedAt: t0}, {Size: 4 * gb, CreatedAt: t0.Add(2 * day)}}
	if Used(entries) != 5*gb {
		t.Fatal(Used(entries))
	}
	// Full: 1 GB fits once the day-1 upload leaves the window.
	at := FreesAt(entries, 5*gb, 1*gb, window)
	if at == nil || !at.Equal(t0.Add(3*day)) {
		t.Fatal("1GB frees at", at)
	}
	// 2 GB needs both to go.
	at = FreesAt(entries, 5*gb, 2*gb, window)
	if at == nil || !at.Equal(t0.Add(5*day)) {
		t.Fatal("2GB frees at", at)
	}
	// Fits now / never fits.
	if FreesAt(entries[:1], 5*gb, 1*gb, window) != nil {
		t.Fatal("fits now")
	}
	if FreesAt(entries, 5*gb, 6*gb, window) != nil {
		t.Fatal("never fits")
	}
	// Boundary: created exactly window ago no longer counts.
	if !WindowStart(t0.Add(window), window).Equal(t0) {
		t.Fatal("window start")
	}
}

func TestSchedule(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	w := time.Hour
	en := func(n int64, at time.Time) filesrepo.Entry { return filesrepo.Entry{Size: n, CreatedAt: at} }
	e := []filesrepo.Entry{en(1, t0), en(2, t0), en(3, t0.Add(time.Minute)), en(4, t0.Add(2*time.Minute)), en(5, t0.Add(3*time.Minute))}
	s := Schedule(e, w, 3)
	if len(s) != 3 || s[0].Bytes != 3 || !s[0].At.Equal(t0.Add(w)) || s[1].Bytes != 3 || s[2].Bytes != 9 || !s[2].At.Equal(t0.Add(w+3*time.Minute)) {
		t.Fatalf("%+v", s)
	}
	if got := Schedule(nil, w, 3); got == nil || len(got) != 0 {
		t.Fatal("empty schedule must be []")
	}
}
