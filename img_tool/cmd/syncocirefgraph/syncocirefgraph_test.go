package syncocirefgraph

import (
	"encoding/json"
	"reflect"
	"testing"
)

const indexDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestCachedEntryIsUsedWithoutDownloading(t *testing.T) {
	entry := RefGraphEntry{
		Kind:      "index",
		Manifests: []string{"sha256:child"},
		Descriptors: []map[string]interface{}{{
			"digest":      "sha256:child",
			"platform":    map[string]interface{}{"os": "linux", "architecture": "arm64", "variant": "v9"},
			"annotations": map[string]interface{}{"test": "preserved"},
		}},
	}
	// No sources at all: this fails if the cache hit attempts a download.
	result := downloadAndParseManifest(indexDigest, ImageInfo{}, Facts{graphFactPrefix + indexDigest: entry})
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	if !reflect.DeepEqual(result.RefGraphEntry, entry) {
		t.Fatalf("got %#v, want %#v", result.RefGraphEntry, entry)
	}
	if result.ManifestData != nil {
		t.Fatal("facts must not carry blob contents")
	}
}

func TestFactsWithoutDescriptorsAreRediscovered(t *testing.T) {
	cases := []struct {
		name  string
		entry RefGraphEntry
		valid bool
	}{
		{"manifest", RefGraphEntry{Kind: "manifest", Config: "sha256:config", Layers: []string{"sha256:layer"}}, true},
		{"manifest without layers", RefGraphEntry{Kind: "manifest", Config: "sha256:config"}, true},
		{"empty index", RefGraphEntry{Kind: "index"}, true},
		{
			"index",
			RefGraphEntry{
				Kind:        "index",
				Manifests:   []string{"sha256:child"},
				Descriptors: []map[string]interface{}{{"digest": "sha256:child"}},
			},
			true,
		},
		{"index from before descriptors", RefGraphEntry{Kind: "index", Manifests: []string{"sha256:child"}}, false},
		{"unknown kind", RefGraphEntry{Kind: "artifact"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Facts survive a JSON roundtrip through the lockfile before they are read back.
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

func TestParseIndexKeepsDescriptorsInLockstep(t *testing.T) {
	index := `{
		"mediaType": "application/vnd.oci.image.index.v1+json",
		"manifests": [
			{"digest": "sha256:amd64", "platform": {"os": "linux", "architecture": "amd64"}},
			{"digest": "sha256:attestation", "annotations": {"vnd.docker.reference.type": "attestation-manifest"}}
		]
	}`
	entry, err := parseRefGraphEntry(indexDigest, []byte(index))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"sha256:amd64", "sha256:attestation"}; !reflect.DeepEqual(entry.Manifests, want) {
		t.Fatalf("manifests = %v, want %v", entry.Manifests, want)
	}
	if len(entry.Descriptors) != len(entry.Manifests) {
		t.Fatalf("got %d descriptors for %d manifests", len(entry.Descriptors), len(entry.Manifests))
	}
	// A descriptor is stored whole: the extension has no other source for the annotations and
	// the platform of a child.
	if entry.Descriptors[1]["annotations"] == nil {
		t.Fatalf("descriptor lost its annotations: %#v", entry.Descriptors[1])
	}
}

func TestParseIndexRejectsChildWithoutDigest(t *testing.T) {
	index := `{"mediaType": "application/vnd.oci.image.index.v1+json", "manifests": [{"size": 1}]}`
	if _, err := parseRefGraphEntry(indexDigest, []byte(index)); err == nil {
		t.Fatal("expected an error for a child without a digest")
	}
}

func TestMergeSourceMaps(t *testing.T) {
	a := map[string][]string{"one": {"a", "b"}}
	b := map[string][]string{"one": {"b", "c"}, "two": {"d"}}
	got := mergeSourceMaps(a, b)
	want := map[string][]string{"one": {"a", "b", "c"}, "two": {"d"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if !reflect.DeepEqual(a["one"], []string{"a", "b"}) {
		t.Fatal("mergeSourceMaps modified its input")
	}
	if got := mergeSourceMaps(nil, b); !reflect.DeepEqual(got, b) {
		t.Fatalf("merging into nothing = %v, want %v", got, b)
	}
}
