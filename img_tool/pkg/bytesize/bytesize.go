// Package bytesize formats and parses byte counts using binary (power-of-two)
// units, so every `img` subcommand reports the same number the same way.
package bytesize

import (
	"fmt"
	"strconv"
	"strings"
)

// Format renders a byte count with a binary unit, e.g. "10.5 MiB".
func Format(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/unit)
}

// Parse parses a byte count, optionally with a binary unit suffix
// ("512MiB", "2 GB", "1024"). Suffixes are always powers of two, so "GB" and
// "GiB" both mean 1<<30.
func Parse(raw string) (int64, error) {
	digits := strings.TrimRight(raw, "kKmMgGtTiIbB \t")
	unit := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(raw, digits)))
	value, err := strconv.ParseFloat(strings.TrimSpace(digits), 64)
	if err != nil {
		return 0, fmt.Errorf("not a byte count")
	}
	multiplier := int64(1)
	switch unit {
	case "", "b":
	case "k", "kb", "kib":
		multiplier = 1 << 10
	case "m", "mb", "mib":
		multiplier = 1 << 20
	case "g", "gb", "gib":
		multiplier = 1 << 30
	case "t", "tb", "tib":
		multiplier = 1 << 40
	default:
		return 0, fmt.Errorf("unknown unit %q", unit)
	}
	if value < 0 {
		return 0, fmt.Errorf("negative size")
	}
	return int64(value * float64(multiplier)), nil
}
