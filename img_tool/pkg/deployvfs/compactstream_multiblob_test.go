package deployvfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
)

// recordingCASReader is a stubCASReader that records the digest lists its
// ReaderForBlobs was handed, so a test can tell how a request was grouped.
type recordingCASReader struct {
	stubCASReader
	lists [][]string // hex hashes per ReaderForBlobs call
}

func (s *recordingCASReader) ReaderForBlobs(ctx context.Context, digests []cas.Digest) (io.ReadCloser, error) {
	hashes := make([]string, len(digests))
	for i, d := range digests {
		hashes[i] = hex.EncodeToString(d.Hash)
	}
	s.lists = append(s.lists, hashes)
	return s.stubCASReader.ReaderForBlobs(ctx, digests)
}

// blobSet is a set of test blobs addressed by content.
type blobSet struct {
	contents [][]byte
	requests []compactstream.BlobRequest
	byHash   map[string][]byte
}

func makeBlobs(n int) blobSet {
	set := blobSet{byHash: map[string][]byte{}}
	for i := range n {
		content := []byte(fmt.Sprintf("blob-%02d-content", i))
		digest := sha256.Sum256(content)
		set.contents = append(set.contents, content)
		set.requests = append(set.requests, compactstream.BlobRequest{Digest: digest[:], Size: int64(len(content))})
		set.byHash[hex.EncodeToString(digest[:])] = content
	}
	return set
}

func (b blobSet) joined(indices ...int) []byte {
	var out []byte
	for _, i := range indices {
		out = append(out, b.contents[i]...)
	}
	return out
}

func (b blobSet) all() []byte {
	return b.joined(rangeN(len(b.contents))...)
}

func rangeN(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func readBlobStream(t *testing.T, s *casDirStore, requests []compactstream.BlobRequest) []byte {
	t.Helper()
	rc, err := s.ReaderForBlobs(context.Background(), requests)
	if err != nil {
		t.Fatal(err)
	}
	return readAllClose(t, rc)
}

// With nothing local, the whole reference table goes to the remote cache in one
// piece -- which is what lets it batch.
func TestCasDirStoreReadsAllRemoteBlobsAsOneRun(t *testing.T) {
	blobs := makeBlobs(6)
	remote := &recordingCASReader{stubCASReader: stubCASReader{blobs: blobs.byHash}}
	s := &casDirStore{casReader: remote}

	if got := readBlobStream(t, s, blobs.requests); !bytes.Equal(got, blobs.all()) {
		t.Fatalf("stream = %q, want %q", got, blobs.all())
	}
	if len(remote.lists) != 1 {
		t.Fatalf("remote cache saw %d lists, want 1", len(remote.lists))
	}
	if len(remote.lists[0]) != len(blobs.requests) {
		t.Fatalf("remote cache was asked for %d blobs, want %d", len(remote.lists[0]), len(blobs.requests))
	}
}

// Blobs shipped in the input directory are read from disk, and the remote cache
// is never called.
func TestCasDirStoreReadsAllLocalBlobsWithoutTheRemoteCache(t *testing.T) {
	blobs := makeBlobs(4)
	shaDir := filepath.Join(t.TempDir(), "sha256")
	for hash, content := range blobs.byHash {
		writeBlobFile(t, filepath.Join(shaDir, hash), content)
	}
	remote := &recordingCASReader{stubCASReader: stubCASReader{blobs: map[string][]byte{}}}
	s := &casDirStore{shaDir: shaDir, casReader: remote}

	if got := readBlobStream(t, s, blobs.requests); !bytes.Equal(got, blobs.all()) {
		t.Fatalf("stream = %q, want %q", got, blobs.all())
	}
	if len(remote.lists) != 0 {
		t.Fatalf("remote cache was called %d times for blobs the input directory has", len(remote.lists))
	}
}

// A mixed store groups the remote blobs into runs instead of fetching them one
// by one, and still serves the local ones from disk.
func TestCasDirStoreGroupsRemoteRunsAroundLocalBlobs(t *testing.T) {
	blobs := makeBlobs(7)
	// Ship blobs 2 and 5 locally; 0-1, 3-4 and 6 must come from the remote cache.
	shaDir := filepath.Join(t.TempDir(), "sha256")
	local := []int{2, 5}
	remoteBlobs := map[string][]byte{}
	for i, request := range blobs.requests {
		hash := hex.EncodeToString(request.Digest)
		if slices.Contains(local, i) {
			writeBlobFile(t, filepath.Join(shaDir, hash), blobs.contents[i])
			continue
		}
		remoteBlobs[hash] = blobs.contents[i]
	}
	remote := &recordingCASReader{stubCASReader: stubCASReader{blobs: remoteBlobs}}
	s := &casDirStore{shaDir: shaDir, casReader: remote}

	if got := readBlobStream(t, s, blobs.requests); !bytes.Equal(got, blobs.all()) {
		t.Fatalf("stream = %q, want %q", got, blobs.all())
	}
	var gotRuns [][]string
	for _, list := range remote.lists {
		gotRuns = append(gotRuns, list)
	}
	wantRuns := [][]string{
		{hexOf(blobs, 0), hexOf(blobs, 1)},
		{hexOf(blobs, 3), hexOf(blobs, 4)},
		{hexOf(blobs, 6)},
	}
	if fmt.Sprint(gotRuns) != fmt.Sprint(wantRuns) {
		t.Fatalf("remote runs = %v, want %v", gotRuns, wantRuns)
	}
}

// A blob the disk cache has is local too, as long as its size matches.
func TestCasDirStoreTreatsDiskCacheHitsAsLocal(t *testing.T) {
	blobs := makeBlobs(2)
	diskCache := t.TempDir()
	writeBlobFile(t, diskCacheBlobPath(diskCache, "sha256:"+hexOf(blobs, 0)), blobs.contents[0])
	remote := &recordingCASReader{stubCASReader: stubCASReader{blobs: map[string][]byte{
		hexOf(blobs, 1): blobs.contents[1],
	}}}
	s := &casDirStore{diskCachePath: diskCache, casReader: remote}

	if got := readBlobStream(t, s, blobs.requests); !bytes.Equal(got, blobs.all()) {
		t.Fatalf("stream = %q, want %q", got, blobs.all())
	}
	if len(remote.lists) != 1 || len(remote.lists[0]) != 1 {
		t.Fatalf("remote cache saw %v, want only the uncached blob", remote.lists)
	}
}

// Without a remote cache the store still serves the list; a blob it does not
// have fails with the usual not-found error rather than a nil dereference.
func TestCasDirStoreReadsBlobsWithoutARemoteCache(t *testing.T) {
	blobs := makeBlobs(2)
	shaDir := filepath.Join(t.TempDir(), "sha256")
	writeBlobFile(t, filepath.Join(shaDir, hexOf(blobs, 0)), blobs.contents[0])
	s := &casDirStore{shaDir: shaDir}

	rc, err := s.ReaderForBlobs(context.Background(), blobs.requests)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err == nil || !strings.Contains(err.Error(), "not found in input file CAS directory") {
		t.Fatalf("error = %v, want the not-found error for the second blob", err)
	}
}

// The store is what Reconstruct looks for; a compact stream must go through the
// batched path.
func TestCasDirStoreIsAMultiBlobStore(t *testing.T) {
	var store compactstream.BlobStore = &casDirStore{}
	if _, ok := store.(compactstream.MultiBlobStore); !ok {
		t.Fatal("casDirStore does not implement compactstream.MultiBlobStore")
	}
}

func hexOf(blobs blobSet, i int) string {
	return hex.EncodeToString(blobs.requests[i].Digest)
}
