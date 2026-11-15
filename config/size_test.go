package config

import "testing"

func TestParseSize(t *testing.T) {
	ok := map[string]int64{
		"5GB": 5e9, "500MB": 5e8, "25 MB": 25e6, "100mb": 1e8, "1GiB": 1 << 30, "1.5KiB": 1536,
		"1048576": 1048576, "7B": 7, "2k": 2000,
	}
	for in, want := range ok {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "GB", "5 XB", "-1MB", "1e3", "lots"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q): want error", in)
		}
	}
}

func TestFormatSize(t *testing.T) {
	for n, want := range map[int64]string{5e9: "5GB", 25e6: "25MB", 1 << 30: "1GiB", 1234: "1234", 0: "0"} {
		if got := FormatSize(n); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", n, got, want)
		}
		if n > 0 {
			if back, _ := ParseSize(FormatSize(n)); back != n {
				t.Errorf("round trip %d -> %d", n, back)
			}
		}
	}
}
