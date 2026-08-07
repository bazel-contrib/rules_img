package main

import (
	"errors"
	"strings"
	"time"
)

type blobStores []string

func (s *blobStores) String() string {
	if s == nil || len(*s) == 0 {
		return ""
	}
	return strings.Join(*s, ", ")
}

func (s *blobStores) Set(value string) error {
	switch strings.ToLower(value) {
	case "s3", "reapi", "upstream":
		// Valid values, do nothing.
	default:
		return errors.New("invalid blob store type: " + value)
	}
	*s = append(*s, value)
	return nil
}

// optionalDuration is a duration flag that remembers whether it was given, so
// that its default can be another flag's value rather than a constant. An unset
// one prints as empty, which keeps the flag package from advertising a default
// it does not have.
type optionalDuration struct {
	value time.Duration
	set   bool
}

func (d *optionalDuration) String() string {
	if d == nil || !d.set {
		return ""
	}
	return d.value.String()
}

func (d *optionalDuration) Set(value string) error {
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return err
	}
	d.value = parsed
	d.set = true
	return nil
}

// orElse returns the duration that was given, or fallback if none was.
func (d *optionalDuration) orElse(fallback time.Duration) time.Duration {
	if d == nil || !d.set {
		return fallback
	}
	return d.value
}
