package main

import (
	"flag"
	"io"
	"testing"
	"time"
)

func TestOptionalDurationFallsBackWhenUnset(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want time.Duration
	}{
		{"unset follows the fallback", nil, 6 * time.Hour},
		{"an explicit value wins", []string{"-tag-ttl", "168h"}, 168 * time.Hour},
		// The whole point of the type: an explicit zero has to be
		// distinguishable from no flag at all, since zero means "keep tags
		// forever" while no flag means "do what --ttl does".
		{"an explicit zero wins too", []string{"-tag-ttl", "0"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tagTTL optionalDuration
			flagSet := flag.NewFlagSet("registry", flag.ContinueOnError)
			flagSet.SetOutput(io.Discard)
			flagSet.Var(&tagTTL, "tag-ttl", "")
			if err := flagSet.Parse(tc.args); err != nil {
				t.Fatalf("parsing %v: %v", tc.args, err)
			}
			if got := tagTTL.orElse(6 * time.Hour); got != tc.want {
				t.Fatalf("orElse got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestOptionalDurationAdvertisesNoDefaultUntilItHasOne(t *testing.T) {
	var unset optionalDuration
	if got := unset.String(); got != "" {
		t.Fatalf("an unset optionalDuration prints %q, want empty so the flag package advertises no default", got)
	}

	var set optionalDuration
	if err := set.Set("90m"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := set.String(); got != "1h30m0s" {
		t.Fatalf("String got %q, want 1h30m0s", got)
	}
	if got := set.orElse(time.Hour); got != 90*time.Minute {
		t.Fatalf("orElse got %s, want 1h30m0s", got)
	}
}

func TestOptionalDurationRejectsNonsense(t *testing.T) {
	var duration optionalDuration
	if err := duration.Set("a fortnight"); err == nil {
		t.Fatal("Set accepted a value that is not a duration")
	}
	if got := duration.orElse(time.Hour); got != time.Hour {
		t.Fatalf("a rejected value left the flag set: orElse got %s, want 1h0m0s", got)
	}
}

func TestBlobStoresAcceptsOnlyKnownStores(t *testing.T) {
	var stores blobStores
	for _, store := range []string{"reapi", "s3", "upstream"} {
		if err := stores.Set(store); err != nil {
			t.Fatalf("Set(%q): %v", store, err)
		}
	}
	if err := stores.Set("nfs"); err == nil {
		t.Fatal("Set accepted an unknown blob store")
	}
	if got := stores.String(); got != "reapi, s3, upstream" {
		t.Fatalf("String got %q, want the three stores that were accepted", got)
	}
}
