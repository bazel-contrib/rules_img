package syncocirefgraph

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCachedDescriptorsDoNotDownload(t *testing.T) {
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	entry := RefGraphEntry{
		Kind:      "index",
		Manifests: []string{"sha256:child"},
		Descriptors: []map[string]interface{}{{
			"digest":      "sha256:child",
			"platform":    map[string]interface{}{"os": "linux", "architecture": "arm64", "variant": "v9"},
			"annotations": map[string]interface{}{"test": "preserved"},
		}},
	}
	// No sources: this fails if the cache hit attempts a download.
	result := downloadAndParseManifest(digest, ImageInfo{}, Facts{"oci_ref_graph_v2@" + digest: entry})
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	if !reflect.DeepEqual(result.RefGraphEntry, entry) {
		t.Fatalf("got %#v, want %#v", result.RefGraphEntry, entry)
	}
	if result.ManifestData != nil {
		t.Fatal("facts should not contain blob contents")
	}
}

func TestFactsCompleteness(t *testing.T) {
	cases := []struct {
		name  string
		entry RefGraphEntry
		valid bool
	}{
		{"manifest", RefGraphEntry{Kind: "manifest", Config: "sha256:config", Layers: []string{}}, true},
		{"empty index", RefGraphEntry{Kind: "index", Manifests: []string{}, Descriptors: []map[string]interface{}{}}, true},
		{"old index", RefGraphEntry{Kind: "index", Manifests: []string{"sha256:child"}}, false},
		{"missing config", RefGraphEntry{Kind: "manifest", Layers: []string{}}, false},
		{"missing layers", RefGraphEntry{Kind: "manifest", Config: "sha256:config"}, false},
		{"unknown kind", RefGraphEntry{Kind: "artifact"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Verify empty arrays survive JSON roundtrips through the lockfile.
			data, err := json.Marshal(tc.entry)
			if err != nil {
				t.Fatal(err)
			}
			var restored RefGraphEntry
			if err := json.Unmarshal(data, &restored); err != nil {
				t.Fatal(err)
			}
			if got := validRefGraphEntry(restored); got != tc.valid {
				t.Fatalf("valid = %v, want %v: %s", got, tc.valid, data)
			}
		})
	}
}

func TestMergeChildSources(t *testing.T) {
	a := ImageInfo{Sources: map[string][]string{"one": {"a", "b"}}}
	b := ImageInfo{Sources: map[string][]string{"one": {"b", "c"}, "two": {"d"}}}
	got := mergeSources(a, b)
	want := map[string][]string{"one": {"a", "b", "c"}, "two": {"d"}}
	if !reflect.DeepEqual(got.Sources, want) {
		t.Fatalf("got %v, want %v", got.Sources, want)
	}
	if !reflect.DeepEqual(a.Sources["one"], []string{"a", "b"}) {
		t.Fatal("modified parent sources")
	}
}
