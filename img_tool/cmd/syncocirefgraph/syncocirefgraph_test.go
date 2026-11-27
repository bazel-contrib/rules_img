package syncocirefgraph

import (
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

const (
	index  = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	child  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	single = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	sig    = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

func testGraph() (map[string]ImageInfo, map[string]RefGraphEntry) {
	images := map[string]ImageInfo{
		index:  {Sources: map[string][]string{"example/index": {"a.example"}}},
		single: {Sources: map[string][]string{"example/child": {"b.example"}}},
	}
	graph := map[string]RefGraphEntry{
		index:  {Kind: "index", Manifests: []string{child}},
		child:  {Kind: "manifest"},
		single: {Kind: "manifest"},
	}
	return images, graph
}

func TestReferrerJobsMergeSourcesOfImagesContainingTheAnchor(t *testing.T) {
	images, graph := testGraph()
	// The child is pulled through the index; the request anchored there is
	// fetched from the index's sources. An anchor outside every image is skipped.
	jobs := referrerJobs([]ReferrerRequest{
		{Digest: sig, Anchor: child},
		{Digest: "sha256:4444444444444444444444444444444444444444444444444444444444444444", Anchor: "sha256:5555555555555555555555555555555555555555555555555555555555555555"},
	}, images, graph)
	if len(jobs) != 1 || jobs[0].Digest != sig || !jobs[0].RequireSubject {
		t.Fatalf("jobs = %+v, want one job for the signature", jobs)
	}
	if got := jobs[0].Img.Sources["example/index"]; !slices.Equal(got, []string{"a.example"}) {
		t.Errorf("sources = %v, want the index's", jobs[0].Img.Sources)
	}
	if _, ok := jobs[0].Img.Sources["example/child"]; ok {
		t.Errorf("sources = %v, must not include an unrelated image", jobs[0].Img.Sources)
	}
}

func TestDiscoverReferrersReusesFacts(t *testing.T) {
	images, graph := testGraph()
	facts := Facts{
		discoveryFactPrefix + index: map[string]any{"application/vnd.cncf.notary.signature": []any{sig}},
		discoveryFactPrefix + child: map[string]any{"application/vnd.cncf.notary.signature": []any{}},
	}
	// Every requested type is recorded for every manifest of the tree, so no
	// registry is asked (the sources don't exist).
	discovered, err := discoverReferrers([]DiscoveryRequest{
		{Anchor: index, ArtifactTypes: []string{"application/vnd.cncf.notary.signature"}},
	}, images, graph, facts)
	if err != nil {
		t.Fatalf("discoverReferrers: %v", err)
	}
	if got := discovered[index]["application/vnd.cncf.notary.signature"]; !slices.Equal(got, []string{sig}) {
		t.Errorf("index referrers = %v, want %v", got, []string{sig})
	}
	if got, ok := discovered[child]["application/vnd.cncf.notary.signature"]; !ok || len(got) != 0 {
		t.Errorf("child referrers = %v (recorded: %v), want an empty list", got, ok)
	}
}

// A BuildKit attestation manifest without a subject field (oci-artifact=false) is about the
// manifest the index lists it for, which is the one it was discovered for.
func TestAttributeBuildKitAttestations(t *testing.T) {
	_, graph := testGraph()
	const attestation = "sha256:6666666666666666666666666666666666666666666666666666666666666666"
	const withSubject = "sha256:7777777777777777777777777777777777777777777777777777777777777777"
	graph[attestation] = RefGraphEntry{Kind: "manifest"}
	graph[withSubject] = RefGraphEntry{Kind: "manifest", Subject: child}
	discovered := map[string]map[string][]string{
		index: {"buildkit:attestation": {}},
		child: {"buildkit:attestation": {attestation, withSubject}},
	}
	attributeCosignArtifacts([]DiscoveryRequest{{Anchor: index, ArtifactTypes: []string{"buildkit:attestation"}}}, discovered, graph)
	if got := graph[attestation].Subject; got != child {
		t.Errorf("subject of the attestation without a subject field = %q, want %q", got, child)
	}
	if got := graph[withSubject].Subject; got != child {
		t.Errorf("subject of the OCI artifact attestation = %q, want its own %q", got, child)
	}
}

// The BuildKit attestations of the children of an index come from the facts like any other
// discovery, so no registry is asked for the index.
func TestDiscoverBuildKitAttestationsReusesFacts(t *testing.T) {
	images, graph := testGraph()
	const attestation = "sha256:6666666666666666666666666666666666666666666666666666666666666666"
	facts := Facts{
		discoveryFactPrefix + index: map[string]any{"buildkit:attestation": []any{}},
		discoveryFactPrefix + child: map[string]any{"buildkit:attestation": []any{attestation}},
	}
	discovered, err := discoverReferrers([]DiscoveryRequest{
		{Anchor: index, ArtifactTypes: []string{"buildkit:attestation"}},
	}, images, graph, facts)
	if err != nil {
		t.Fatalf("discoverReferrers: %v", err)
	}
	if got := discovered[child]["buildkit:attestation"]; !slices.Equal(got, []string{attestation}) {
		t.Errorf("child attestations = %v, want %v", got, []string{attestation})
	}
}

// Sources are tried in the same order every time: repositories sorted, each
// one's registries as given.
func TestOrderedSources(t *testing.T) {
	got := orderedSources(map[string][]string{
		"example/b": {"z.example", "a.example"},
		"example/a": {"m.example"},
	})
	want := []source{
		{repo: "example/a", registry: "m.example"},
		{repo: "example/b", registry: "z.example"},
		{repo: "example/b", registry: "a.example"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("orderedSources() = %v, want %v", got, want)
	}
}

// The index whose entries name the BuildKit attestations of its children is
// fetched once, not once per child, from the first source that has it.
func TestIndexCacheFetchesOnce(t *testing.T) {
	var fetches atomic.Int32
	cache := newIndexCache()
	cache.fetch = func(src source, digest string) ([]byte, error) {
		fetches.Add(1)
		if src.registry == "mirror.example" {
			return nil, errors.New("not found")
		}
		return []byte(`{"manifests":[]}`), nil
	}
	sources := []source{{repo: "example/index", registry: "mirror.example"}, {repo: "example/index", registry: "a.example"}}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if raw, err := cache.get(index, sources); err != nil || string(raw) != `{"manifests":[]}` {
				t.Errorf("get() = %q, %v", raw, err)
			}
		}()
	}
	wg.Wait()
	if got := fetches.Load(); got != 2 {
		t.Errorf("fetched %d times, want 2 (the mirror, then the registry that has it)", got)
	}
}
