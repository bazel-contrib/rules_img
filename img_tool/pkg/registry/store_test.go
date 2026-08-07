package registry

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func TestMemStoreTagsPointAtManifests(t *testing.T) {
	store := NewMemStore()
	config := descriptorFor(types.OCIConfigJSON, "config")
	blob := imageManifest(t, "one", config)
	digest := digestOf(t, blob)

	if store.HasRepo("app") {
		t.Fatal("empty store reports a repository")
	}

	store.PutManifest("app", digest, Manifest{ContentType: string(types.OCIManifestSchema1), Kind: KindManifest, Blob: blob})
	store.PutTag("app", "v1", digest)

	if !store.HasRepo("app") {
		t.Fatal("store with a manifest reports no repository")
	}
	resolved, ok := store.ResolveTag("app", "v1")
	if !ok || resolved != digest {
		t.Fatalf("ResolveTag got %s, %t; want %s, true", resolved, ok, digest)
	}
	stored, ok := store.GetManifest("app", digest)
	if !ok || string(stored.Blob) != string(blob) {
		t.Fatalf("GetManifest got %q, %t; want the pushed manifest", stored.Blob, ok)
	}

	// Deleting the tag leaves the manifest, which is what an untag means.
	store.DeleteTag("app", "v1")
	if _, ok := store.ResolveTag("app", "v1"); ok {
		t.Fatal("deleted tag still resolves")
	}
	if _, ok := store.GetManifest("app", digest); !ok {
		t.Fatal("deleting a tag deleted the manifest it pointed at")
	}

	store.DeleteManifest("app", digest)
	if store.HasRepo("app") {
		t.Fatal("emptied store still reports a repository")
	}
}

func TestMemStoreRangesAreScopedToARepo(t *testing.T) {
	store := NewMemStore()
	first := imageManifest(t, "first", descriptorFor(types.OCIConfigJSON, "config"))
	second := imageManifest(t, "second", descriptorFor(types.OCIConfigJSON, "config"))
	store.PutManifest("app", digestOf(t, first), Manifest{Blob: first, Kind: KindManifest})
	store.PutTag("app", "v1", digestOf(t, first))
	store.PutManifest("other", digestOf(t, second), Manifest{Blob: second, Kind: KindManifest})

	var repos []string
	store.RangeRepos(func(repo string) bool {
		repos = append(repos, repo)
		return true
	})
	if len(repos) != 2 {
		t.Fatalf("RangeRepos got %v, want both repositories", repos)
	}

	var manifests int
	store.RangeManifests("app", func(v1.Hash, Manifest) bool {
		manifests++
		return true
	})
	if manifests != 1 {
		t.Fatalf("RangeManifests over app got %d manifests, want 1", manifests)
	}

	var tags []string
	store.RangeTags("app", func(tag string, _ v1.Hash) bool {
		tags = append(tags, tag)
		return true
	})
	if len(tags) != 1 || tags[0] != "v1" {
		t.Fatalf("RangeTags over app got %v, want [v1]", tags)
	}
	store.RangeTags("other", func(tag string, _ v1.Hash) bool {
		t.Fatalf("RangeTags over other got tag %q, want none", tag)
		return false
	})
}

func TestMemStoreRangeStopsWhenAskedTo(t *testing.T) {
	store := NewMemStore()
	for _, name := range []string{"a", "b", "c"} {
		blob := imageManifest(t, name, descriptorFor(types.OCIConfigJSON, "config"))
		store.PutManifest("app", digestOf(t, blob), Manifest{Blob: blob, Kind: KindManifest})
		store.PutTag("app", name, digestOf(t, blob))
	}

	var seen int
	store.RangeManifests("app", func(v1.Hash, Manifest) bool {
		seen++
		return false
	})
	if seen != 1 {
		t.Fatalf("RangeManifests visited %d manifests after being told to stop, want 1", seen)
	}
	seen = 0
	store.RangeTags("app", func(string, v1.Hash) bool {
		seen++
		return false
	})
	if seen != 1 {
		t.Fatalf("RangeTags visited %d tags after being told to stop, want 1", seen)
	}
}

func TestKindOfPrefersTheBodyOverTheContentType(t *testing.T) {
	index := imageIndex(t, "index")
	image := imageManifest(t, "image", descriptorFor(types.OCIConfigJSON, "config"))
	dockerList := mustMarshal(t, v1.IndexManifest{
		SchemaVersion: 2,
		MediaType:     types.DockerManifestList,
		Manifests:     []v1.Descriptor{},
	})
	shapelessIndex := []byte(`{"schemaVersion":2,"manifests":[]}`)
	shapelessImage := []byte(`{"schemaVersion":2,"config":{"digest":"sha256:0"},"layers":[]}`)
	shapeless := []byte(`{"schemaVersion":2}`)

	for _, tc := range []struct {
		name        string
		contentType string
		blob        []byte
		want        Kind
	}{
		{"the body says index", "", index, KindIndex},
		{"the body says image", "", image, KindManifest},
		{"a docker manifest list is an index", "", dockerList, KindIndex},
		{"the body outranks a content type that says image", string(types.OCIManifestSchema1), index, KindIndex},
		{"the body outranks a content type that says index", string(types.OCIImageIndex), image, KindManifest},
		{"an agreeing content type changes nothing", string(types.OCIImageIndex), index, KindIndex},
		{"shape decides when the body states no media type: index", "", shapelessIndex, KindIndex},
		{"shape decides when the body states no media type: image", "", shapelessImage, KindManifest},
		{"the content type is the last resort", string(types.OCIImageIndex), shapeless, KindIndex},
		{"an unrecognizable manifest is neither", "", shapeless, KindOther},
		{"invalid json falls back to the content type", string(types.OCIImageIndex), []byte("not json"), KindIndex},
		{"invalid json with no content type is neither", "", []byte("not json"), KindOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := kindOf(tc.contentType, tc.blob); got != tc.want {
				t.Fatalf("kindOf(%q, ...) got %v, want %v", tc.contentType, got, tc.want)
			}
		})
	}
}

func TestParseReferencesFollowsEachKind(t *testing.T) {
	config := descriptorFor(types.OCIConfigJSON, "config")
	layer := descriptorFor(types.OCILayer, "layer")
	subject := descriptorFor(types.OCIManifestSchema1, "subject")

	image := imageManifest(t, "image", config, layer)
	refs := parseReferences(Manifest{Kind: KindManifest, Blob: image})
	if len(refs.manifests) != 0 {
		t.Fatalf("an image manifest referenced %d manifests, want 0", len(refs.manifests))
	}
	if len(refs.blobs) != 2 || refs.blobs[0].digest != config.Digest || refs.blobs[1].digest != layer.Digest {
		t.Fatalf("an image manifest referenced %v, want its config and layer", refs.blobs)
	}
	if refs.blobs[1].size != layer.Size {
		t.Fatalf("layer descriptor size got %d, want %d", refs.blobs[1].size, layer.Size)
	}
	if refs.subject != nil {
		t.Fatal("an image manifest with no subject reported one")
	}

	child := manifestDescriptor(image, types.OCIManifestSchema1)
	index := imageIndex(t, "index", child)
	refs = parseReferences(Manifest{Kind: KindIndex, Blob: index})
	if len(refs.manifests) != 1 || refs.manifests[0] != child.Digest {
		t.Fatalf("an index referenced %v, want its child manifest", refs.manifests)
	}

	signature := referrerManifest(t, "signature", subject.Digest, config)
	refs = parseReferences(Manifest{Kind: KindManifest, Blob: signature})
	if refs.subject == nil || *refs.subject != subject.Digest {
		t.Fatalf("a referrer reported subject %v, want %s", refs.subject, subject.Digest)
	}

	// A manifest we cannot parse drags nothing down with it.
	refs = parseReferences(Manifest{Kind: KindManifest, Blob: []byte("not json")})
	if len(refs.manifests) != 0 || len(refs.blobs) != 0 || refs.subject != nil {
		t.Fatalf("an unparsable manifest reported references: %+v", refs)
	}
}

// descriptorFor returns the descriptor of a blob with the given contents.
func descriptorFor(mediaType types.MediaType, content string) v1.Descriptor {
	digest, size, err := v1.SHA256(strings.NewReader(content))
	if err != nil {
		panic(err)
	}
	return v1.Descriptor{MediaType: mediaType, Digest: digest, Size: size}
}

// manifestDescriptor returns the descriptor of an already serialized manifest.
func manifestDescriptor(blob []byte, mediaType types.MediaType) v1.Descriptor {
	digest, size, err := v1.SHA256(strings.NewReader(string(blob)))
	if err != nil {
		panic(err)
	}
	return v1.Descriptor{MediaType: mediaType, Digest: digest, Size: size}
}

func digestOf(t *testing.T, blob []byte) v1.Hash {
	t.Helper()
	digest, _, err := v1.SHA256(strings.NewReader(string(blob)))
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	return digest
}

// imageManifest serializes an image manifest. The name only distinguishes one
// test manifest from another, since identical bytes are the same manifest.
func imageManifest(t *testing.T, name string, config v1.Descriptor, layers ...v1.Descriptor) []byte {
	t.Helper()
	if layers == nil {
		layers = []v1.Descriptor{}
	}
	return mustMarshal(t, v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		Config:        config,
		Layers:        layers,
		Annotations:   map[string]string{"test.name": name},
	})
}

// imageIndex serializes an image index over the given children.
func imageIndex(t *testing.T, name string, children ...v1.Descriptor) []byte {
	t.Helper()
	if children == nil {
		children = []v1.Descriptor{}
	}
	return mustMarshal(t, v1.IndexManifest{
		SchemaVersion: 2,
		MediaType:     types.OCIImageIndex,
		Manifests:     children,
		Annotations:   map[string]string{"test.name": name},
	})
}

// referrerManifest serializes a manifest that declares a subject, the way a
// signature or an attestation attaches itself to an image.
func referrerManifest(t *testing.T, name string, subject v1.Hash, config v1.Descriptor) []byte {
	t.Helper()
	return mustMarshal(t, v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		Config:        config,
		Layers:        []v1.Descriptor{},
		Subject:       &v1.Descriptor{MediaType: types.OCIManifestSchema1, Digest: subject, Size: 1},
		Annotations:   map[string]string{"test.name": name},
	})
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	blob, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}
	return blob
}
