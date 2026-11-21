package bom

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// When an SBOM records its creation time.
const (
	// TimestampNone records no creation time, so the SBOM is reproducible.
	// CycloneDX leaves its (optional) timestamp out; SPDX, which requires
	// one, gets the Unix epoch.
	TimestampNone = "none"
	// TimestampStamp records a value of Bazel's workspace status, when the
	// image is stamped; unstamped builds record no time.
	TimestampStamp = "stamp"
	// TimestampNow records the time the SBOM was generated.
	TimestampNow = "now"
)

// Created returns the creation time to record in an SBOM, or the zero time
// for none. For stamp, it is the value of key in the first of the workspace
// status files that has it (Unix seconds, like BUILD_TIMESTAMP, or RFC 3339);
// without status files (the image is not stamped), there is none.
func Created(mode string, statusFiles []string, key string, now func() time.Time) (time.Time, error) {
	switch mode {
	case "", TimestampNone:
		return time.Time{}, nil
	case TimestampNow:
		return now().UTC().Truncate(time.Second), nil
	case TimestampStamp:
		if len(statusFiles) == 0 {
			return time.Time{}, nil
		}
		for _, path := range statusFiles {
			value, ok, err := statusValue(path, key)
			if err != nil {
				return time.Time{}, err
			}
			if ok {
				return parseTimestamp(key, value)
			}
		}
		return time.Time{}, fmt.Errorf("the workspace status has no %s to stamp the SBOM with", key)
	}
	return time.Time{}, fmt.Errorf("unknown timestamp mode %q", mode)
}

// statusValue looks a key up in a workspace status file (lines of "KEY VALUE").
func statusValue(path, key string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, _ := strings.Cut(line, " ")
		if k == key {
			return strings.TrimSpace(v), true, nil
		}
	}
	return "", false, nil
}

func parseTimestamp(key, value string) (time.Time, error) {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Unix(seconds, 0).UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("%s is %q: want Unix seconds or an RFC 3339 time", key, value)
}
