package syncocirefgraph

import (
	"slices"
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
