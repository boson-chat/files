package ids

import "testing"

func TestFileID(t *testing.T) {
	seen := map[string]bool{}
	for range 2000 {
		id := FileID()
		if len(id) != FileIDLen || !ValidFileID(id) {
			t.Fatalf("bad id %q", id)
		}
		if seen[id] {
			t.Fatal("duplicate id")
		}
		seen[id] = true
	}
	for _, bad := range []string{"", "short", "aaaaaaaaaaaaaaaaaaaaa/", "aaaaaaaaaaaaaaaaaaaaa-b"} {
		if ValidFileID(bad) {
			t.Fatalf("%q accepted", bad)
		}
	}
}
