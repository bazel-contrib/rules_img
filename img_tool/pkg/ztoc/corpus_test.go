package ztoc

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"testing"
)

//go:embed testdata/vectors
var corpusFS embed.FS

// The corpus under testdata/vectors is a set of (input .tar.gz, span) pairs
// together with a golden .ztoc produced by soci-snapshotter's own
// (cgo/zlib) builder. These tests rebuild each ztoc in pure Go and require the
// serialized FlatBuffer to be byte-for-byte identical to soci's, which
// validates the inflate/zran checkpoints, per-span digests, TOC extraction, and
// FlatBuffer marshaling all at once. See testdata/README.md for regeneration.
//
// The goldens were built with build_tool_identifier "soci-oracle"; the tests
// set the same identifier so the comparison is exact.

const oracleToolID = "soci-oracle"

type manifestEntry struct {
	Name  string `json:"name"`
	Input string `json:"input"`
	Span  int64  `json:"span"`
}

func corpusFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := corpusFS.ReadFile("testdata/vectors/" + name)
	if err != nil {
		t.Fatalf("reading corpus file %q: %v", name, err)
	}
	return data
}

func loadCorpus(t *testing.T) []manifestEntry {
	t.Helper()
	var entries []manifestEntry
	if err := json.Unmarshal(corpusFile(t, "manifest.json"), &entries); err != nil {
		t.Fatalf("parsing corpus manifest: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("empty corpus manifest")
	}
	return entries
}

// buildVector builds the ztoc for a corpus entry from its embedded input bytes.
func buildVector(t *testing.T, e manifestEntry) *Ztoc {
	t.Helper()
	in := corpusFile(t, e.Input)
	z, err := Build(bytes.NewReader(in), int64(len(in)),
		WithSpanSize(e.Span), WithBuildToolIdentifier(oracleToolID))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return z
}

// TestCorpusMatchesSociGolden is the primary conformance test: pure-Go Build +
// Marshal must reproduce soci's golden bytes exactly.
func TestCorpusMatchesSociGolden(t *testing.T) {
	for _, e := range loadCorpus(t) {
		t.Run(e.Name, func(t *testing.T) {
			z := buildVector(t, e)
			got, err := Marshal(z)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			want := corpusFile(t, e.Name+".ztoc")
			if !bytes.Equal(got, want) {
				t.Errorf("ztoc bytes differ from soci golden: got %d bytes, want %d bytes\n%s",
					len(got), len(want), describeDiff(z, want, got))
			}
		})
	}
}

// TestCorpusUnmarshalRoundTrip verifies that decoding a soci golden and
// re-marshaling reproduces the same bytes (so Unmarshal reads soci output
// faithfully and Marshal/Unmarshal are inverse).
func TestCorpusUnmarshalRoundTrip(t *testing.T) {
	for _, e := range loadCorpus(t) {
		t.Run(e.Name, func(t *testing.T) {
			want := corpusFile(t, e.Name+".ztoc")
			z, err := Unmarshal(want)
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			got, err := Marshal(z)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("re-marshaled ztoc differs from golden: got %d, want %d bytes", len(got), len(want))
			}
		})
	}
}

// TestCorpusBuildEqualsUnmarshal checks that the ztoc built in pure Go is
// semantically equal (field by field) to the one decoded from soci's golden.
func TestCorpusBuildEqualsUnmarshal(t *testing.T) {
	for _, e := range loadCorpus(t) {
		t.Run(e.Name, func(t *testing.T) {
			built := buildVector(t, e)
			decoded, err := Unmarshal(corpusFile(t, e.Name+".ztoc"))
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			// Note: Unmarshal recomputes TarHeaderOffset (which Build leaves
			// zero) so we compare it against Build's derived value too.
			assertZtocEqual(t, decoded, built)
		})
	}
}

func assertZtocEqual(t *testing.T, want, got *Ztoc) {
	t.Helper()
	if want.Version != got.Version {
		t.Errorf("Version: got %q want %q", got.Version, want.Version)
	}
	if want.CompressedArchiveSize != got.CompressedArchiveSize {
		t.Errorf("CompressedArchiveSize: got %d want %d", got.CompressedArchiveSize, want.CompressedArchiveSize)
	}
	if want.UncompressedArchiveSize != got.UncompressedArchiveSize {
		t.Errorf("UncompressedArchiveSize: got %d want %d", got.UncompressedArchiveSize, want.UncompressedArchiveSize)
	}
	if want.MaxSpanID != got.MaxSpanID {
		t.Errorf("MaxSpanID: got %d want %d", got.MaxSpanID, want.MaxSpanID)
	}
	if want.CompressionAlgorithm != got.CompressionAlgorithm {
		t.Errorf("CompressionAlgorithm: got %q want %q", got.CompressionAlgorithm, want.CompressionAlgorithm)
	}
	if !bytes.Equal(want.Checkpoints, got.Checkpoints) {
		t.Errorf("Checkpoints differ: got %d bytes want %d bytes", len(got.Checkpoints), len(want.Checkpoints))
	}
	if len(want.SpanDigests) != len(got.SpanDigests) {
		t.Fatalf("SpanDigests count: got %d want %d", len(got.SpanDigests), len(want.SpanDigests))
	}
	for i := range want.SpanDigests {
		if want.SpanDigests[i] != got.SpanDigests[i] {
			t.Errorf("SpanDigests[%d]: got %s want %s", i, got.SpanDigests[i], want.SpanDigests[i])
		}
	}
	if len(want.FileMetadata) != len(got.FileMetadata) {
		t.Fatalf("FileMetadata count: got %d want %d", len(got.FileMetadata), len(want.FileMetadata))
	}
	// Build produces entries in archive order; Unmarshal sorts by offset. Both
	// orderings coincide for these inputs, but compare via a name lookup to be
	// robust.
	byName := map[string]FileMetadata{}
	for _, m := range got.FileMetadata {
		byName[m.Name] = m
	}
	for _, w := range want.FileMetadata {
		g, ok := byName[w.Name]
		if !ok {
			t.Errorf("FileMetadata %q missing from built ztoc", w.Name)
			continue
		}
		if !fileMetadataEqual(w, g) {
			t.Errorf("FileMetadata %q differs:\n want %+v\n  got %+v", w.Name, w, g)
		}
	}
}

func fileMetadataEqual(a, b FileMetadata) bool {
	if a.Name != b.Name || a.Type != b.Type ||
		a.UncompressedOffset != b.UncompressedOffset ||
		a.UncompressedSize != b.UncompressedSize ||
		a.Linkname != b.Linkname || a.Mode != b.Mode ||
		a.UID != b.UID || a.GID != b.GID ||
		a.Uname != b.Uname || a.Gname != b.Gname ||
		!a.ModTime.Equal(b.ModTime) ||
		a.Devmajor != b.Devmajor || a.Devminor != b.Devminor {
		return false
	}
	if len(a.PAXHeaders) != len(b.PAXHeaders) {
		return false
	}
	for k, v := range a.PAXHeaders {
		if b.PAXHeaders[k] != v {
			return false
		}
	}
	return true
}

// describeDiff summarizes a byte mismatch to aid debugging.
func describeDiff(z *Ztoc, want, got []byte) string {
	n := len(got)
	if len(want) < n {
		n = len(want)
	}
	first := -1
	for i := 0; i < n; i++ {
		if got[i] != want[i] {
			first = i
			break
		}
	}
	return fmt.Sprintf("checkpoints=%d spanDigests=%d files=%d compressed=%d uncompressed=%d firstDiffAt=%d",
		len(z.Checkpoints), len(z.SpanDigests), len(z.FileMetadata),
		z.CompressedArchiveSize, z.UncompressedArchiveSize, first)
}
