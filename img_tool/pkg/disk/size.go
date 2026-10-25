package disk

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ParseSize parses a size in bytes. It accepts plain numbers and numbers with a
// binary suffix (KiB, MiB, GiB, TiB; K, M, G and T mean the same) or a decimal
// suffix (KB, MB, GB, TB), with an optional fraction ("1.5GiB"). The suffixes
// are case-insensitive, so "1.5gib" is the same size as "1.5GiB".
func ParseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   float64
	}{
		{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3},
		{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1},
	}
	mult := 1.0
	for _, u := range units {
		// The suffixes are ASCII, so folding the tail cannot change its length.
		if len(t) >= len(u.suffix) && strings.EqualFold(t[len(t)-len(u.suffix):], u.suffix) {
			t, mult = strings.TrimSpace(t[:len(t)-len(u.suffix)]), u.mult
			break
		}
	}
	if n, err := strconv.ParseInt(t, 10, 64); err == nil {
		if n < 0 || (mult > 1 && float64(n) > float64(1<<62)/mult) {
			return 0, fmt.Errorf("disk: size %q is out of range", s)
		}
		return n * int64(mult), nil
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil || math.IsNaN(f) || f < 0 || f*mult > float64(1<<62) {
		return 0, fmt.Errorf("disk: %q is not a size like 4096, 64MiB or 1.5GiB", s)
	}
	return int64(f * mult), nil
}
