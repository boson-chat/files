package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

var sizeUnits = map[string]float64{
	"": 1, "b": 1,
	"k": 1e3, "kb": 1e3, "m": 1e6, "mb": 1e6, "g": 1e9, "gb": 1e9, "t": 1e12, "tb": 1e12,
	"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40,
}

// ParseSize parses "5GB", "500 MB", "1.5GiB" or a plain byte count.
// KB/MB/GB/TB are powers of 1000; KiB/MiB/GiB/TiB powers of 1024.
func ParseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	i := 0
	for i < len(t) && (t[i] >= '0' && t[i] <= '9' || t[i] == '.') {
		i++
	}
	num, unit := t[:i], strings.ToLower(strings.TrimSpace(t[i:]))
	mult, ok := sizeUnits[unit]
	if num == "" || !ok {
		return 0, fmt.Errorf("invalid size %q (use e.g. 100MB, 5GB, 1GiB or a byte count)", s)
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	v := f * mult
	if v > math.MaxInt64/2 {
		return 0, fmt.Errorf("size %q is too large", s)
	}
	return int64(math.Round(v)), nil
}

// FormatSize renders n with the largest decimal unit that divides it
// exactly (5000000000 → "5GB"), else as a byte count.
func FormatSize(n int64) string {
	for _, u := range []struct {
		s string
		v int64
	}{{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}} {
		if n >= u.v && n%u.v == 0 {
			return strconv.FormatInt(n/u.v, 10) + u.s
		}
	}
	for _, u := range []struct {
		s string
		v int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if n >= u.v && n%u.v == 0 {
			return strconv.FormatInt(n/u.v, 10) + u.s
		}
	}
	return strconv.FormatInt(n, 10)
}
