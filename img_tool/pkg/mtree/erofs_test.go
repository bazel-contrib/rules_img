package mtree

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compress"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/digestfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/erofscas"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tarcas"
)

// layerEntry is one entry to record into a layer, described the way both sinks
// take it: a tar header plus either inline content or a path on disk.
type layerEntry struct {
	hdr     *tar.Header
	content string
	path    string
	dedup   bool
}

// recordInto writes the entries into a sink through the same calls the recorder
// makes.
func recordInto(t *testing.T, sink interface {
	WriteHeader(*tar.Header) error
	WriteRegular(*tar.Header, io.Reader) error
	WriteRegularFromPath(*tar.Header, string) error
	WriteRegularFromPathDeduplicated(*tar.Header, string) error
	Close() error
}, entries []layerEntry,
) {
	t.Helper()
	for _, e := range entries {
		var err error
		switch {
		case e.hdr.Typeflag != tar.TypeReg:
			err = sink.WriteHeader(e.hdr)
		case e.path != "" && e.dedup:
			err = sink.WriteRegularFromPathDeduplicated(e.hdr, e.path)
		case e.path != "":
			err = sink.WriteRegularFromPath(e.hdr, e.path)
		default:
			err = sink.WriteRegular(e.hdr, strings.NewReader(e.content))
		}
		if err != nil {
			t.Fatalf("recording %s: %v", e.hdr.Name, err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("closing sink: %v", err)
	}
}

// equivalenceEntries is a layer that exercises the entry types both sinks
// support. Parent directories are described explicitly so the two formats can be
// compared without the synthesized-directory difference getting in the way.
func equivalenceEntries(t *testing.T) []layerEntry {
	t.Helper()
	dir := t.TempDir()
	big := strings.Repeat("shared payload\n", 500)
	bigPath := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(bigPath, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}

	return []layerEntry{
		{hdr: &tar.Header{Typeflag: tar.TypeDir, Name: "etc/", Mode: 0o755, Uid: 0, Gid: 0}},
		{hdr: &tar.Header{Typeflag: tar.TypeDir, Name: "usr/", Mode: 0o755}},
		{hdr: &tar.Header{Typeflag: tar.TypeDir, Name: "usr/bin/", Mode: 0o755}},
		{hdr: &tar.Header{Typeflag: tar.TypeDir, Name: "var/", Mode: 0o755}},
		{hdr: &tar.Header{Typeflag: tar.TypeDir, Name: "var/tmp/", Mode: 0o1777}},
		{hdr: &tar.Header{Typeflag: tar.TypeDir, Name: "dev/", Mode: 0o755}},

		{hdr: &tar.Header{Typeflag: tar.TypeReg, Name: "usr/bin/tool", Size: int64(len(big)), Mode: 0o755}, path: bigPath, dedup: true},
		{hdr: &tar.Header{Typeflag: tar.TypeReg, Name: "usr/bin/tool2", Size: int64(len(big)), Mode: 0o755}, path: bigPath, dedup: true},
		{hdr: &tar.Header{Typeflag: tar.TypeReg, Name: "etc/small.conf", Size: 6, Mode: 0o644}, content: "small\n"},
		{hdr: &tar.Header{Typeflag: tar.TypeReg, Name: "etc/empty", Size: 0, Mode: 0o600}},
		{hdr: &tar.Header{
			Typeflag: tar.TypeReg, Name: "etc/shadow", Size: 5, Mode: 0o640, Uid: 0, Gid: 42,
			PAXRecords: map[string]string{"SCHILY.xattr.user.marker": "yes"},
		}, content: "s3cr\n"},
		{hdr: &tar.Header{Typeflag: tar.TypeSymlink, Name: "usr/bin/link", Linkname: "tool", Mode: 0o777}},
		{hdr: &tar.Header{Typeflag: tar.TypeChar, Name: "dev/null", Mode: 0o666, Devmajor: 1, Devminor: 3}},
		{hdr: &tar.Header{Typeflag: tar.TypeBlock, Name: "dev/sda", Mode: 0o660, Devmajor: 8, Devminor: 0}},
		{hdr: &tar.Header{Typeflag: tar.TypeFifo, Name: "var/tmp/pipe", Mode: 0o644}},
	}
}

// buildTarLayer records the entries into a tar layer and returns the blob.
func buildTarLayer(t *testing.T, entries []layerEntry) []byte {
	t.Helper()
	var blob bytes.Buffer
	appender, err := compress.TarAppenderFactory("sha256", "none", false, &blob)
	if err != nil {
		t.Fatal(err)
	}
	sink := tarcas.NewSHA256CASWithDigestFS(appender, digestfs.New(tarcas.SHA256Helper{}))
	recordInto(t, sink, entries)
	if _, err := appender.Finalize(); err != nil {
		t.Fatal(err)
	}
	return blob.Bytes()
}

// buildErofsLayer records the entries into an EROFS layer and returns the image
// and its compact stream.
func buildErofsLayer(t *testing.T, entries []layerEntry) (image []byte, cstream []byte) {
	t.Helper()
	var blob bytes.Buffer
	appender, err := compress.AppenderFactory("sha256", "none", &blob)
	if err != nil {
		t.Fatal(err)
	}
	var cs bytes.Buffer
	csWriter := compactstream.NewWriter(&cs, compactstream.HashAlgoSHA256, sha256.Size,
		compactstream.StreamCompressionZstd,
		compactstream.OriginalCompressionInfo{CompressionLevel: -1, CompressorJobs: 1}, 0)

	sink := erofscas.NewSHA256CASWithDigestFS(appender, digestfs.New(erofscas.SHA256Helper{}),
		erofscas.WithCompactStreamWriter{Writer: csWriter},
		erofscas.WithSyntheticDirectories(erofscas.SyntheticDirectoriesDefault))
	recordInto(t, sink, entries)

	state, err := appender.Finalize()
	if err != nil {
		t.Fatal(err)
	}
	if err := csWriter.SetCompressedStreamInfo(state.OuterHash, uint64(state.CompressedSize)); err != nil {
		t.Fatal(err)
	}
	if err := csWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return blob.Bytes(), cs.Bytes()
}

func renderTar(t *testing.T, blob []byte, opts Options) string {
	t.Helper()
	var out bytes.Buffer
	if err := WriteMulti(&out, opts, []Input{{Kind: TarInput, Reader: bytes.NewReader(blob), Digester: HashContent}}); err != nil {
		t.Fatalf("rendering tar mtree: %v", err)
	}
	return out.String()
}

func renderErofs(t *testing.T, image []byte, opts Options) string {
	t.Helper()
	var out bytes.Buffer
	if err := WriteMulti(&out, opts, []Input{{Kind: ErofsInput, ReaderAt: bytes.NewReader(image), Digester: HashContent}}); err != nil {
		t.Fatalf("rendering EROFS mtree: %v", err)
	}
	return out.String()
}

// dropDirNlink removes the nlink keyword from directory entries. A directory's
// link count is a filesystem property (2 plus one per subdirectory) that a tar
// layer has no way to express, so it is the one keyword the two formats cannot
// agree on.
func dropDirNlink(spec string) string {
	lines := strings.Split(spec, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "type=dir") {
			continue
		}
		var kept []string
		for _, field := range strings.Fields(line) {
			if strings.HasPrefix(field, "nlink=") {
				continue
			}
			kept = append(kept, field)
		}
		lines[i] = strings.Join(kept, " ")
	}
	return strings.Join(lines, "\n")
}

// TestMtreeEquivalence is the cross-format oracle: for the same recorded
// entries, the mtree of a tar layer and the mtree of an EROFS layer must agree.
//
// The allowed differences are encoded here rather than left implicit, so drift
// in either sink shows up as a test failure:
//
//   - directory link counts, which only a filesystem image has (dropDirNlink);
//   - synthesized intermediate directories, avoided here by describing every
//     directory explicitly;
//   - owner names, which an EROFS inode cannot store (the entries here set none);
//   - whiteout representation, covered by TestMtreeWhiteoutEquivalence.
func TestMtreeEquivalence(t *testing.T) {
	entries := equivalenceEntries(t)
	opts := DefaultOptions()
	opts.Layout = LayoutOCIChangeset

	tarSpec := renderTar(t, buildTarLayer(t, entries), opts)
	image, _ := buildErofsLayer(t, entries)
	erofsSpec := renderErofs(t, image, opts)

	if got, want := dropDirNlink(erofsSpec), dropDirNlink(tarSpec); got != want {
		t.Errorf("EROFS and tar layers render different mtrees.\n--- tar ---\n%s\n--- erofs ---\n%s", want, got)
	}
}

// TestMtreeFromCompactStream checks that an EROFS layer's mtree can be produced
// from its compact stream alone, without any of the file payloads, and matches
// the one produced from the materialized image.
func TestMtreeFromCompactStream(t *testing.T) {
	entries := equivalenceEntries(t)
	opts := DefaultOptions()
	opts.Layout = LayoutOCIChangeset

	image, cstream := buildErofsLayer(t, entries)
	fromImage := renderErofs(t, image, opts)

	sparse, err := compactstream.NewSparseReaderAt(bytes.NewReader(cstream))
	if err != nil {
		t.Fatal(err)
	}
	if sparse.Header().ContentFormat != compactstream.ContentFormatErofs {
		t.Errorf("compact stream content format %d, want %d (erofs)", sparse.Header().ContentFormat, compactstream.ContentFormatErofs)
	}
	if sparse.Size() != int64(len(image)) {
		t.Errorf("sparse view is %d bytes, image is %d", sparse.Size(), len(image))
	}

	var out bytes.Buffer
	if err := WriteMulti(&out, opts, []Input{{
		Kind:          ErofsInput,
		ReaderAt:      sparse,
		Digester:      HashContent,
		RangeDigester: sparse.RefDigestAt,
	}}); err != nil {
		t.Fatalf("rendering mtree from the compact stream: %v", err)
	}

	if out.String() != fromImage {
		t.Errorf("mtree from the compact stream differs from the one from the image.\n--- image ---\n%s\n--- cstream ---\n%s", fromImage, out.String())
	}
}

// TestMtreeWhiteoutEquivalence checks that the changeset layout applies an
// EROFS layer's overlayfs whiteouts the same way it applies a tar layer's
// ".wh." markers, so a merged image mtree is correct for either format.
func TestMtreeWhiteoutEquivalence(t *testing.T) {
	base := []layerEntry{
		{hdr: &tar.Header{Typeflag: tar.TypeDir, Name: "etc/", Mode: 0o755}},
		{hdr: &tar.Header{Typeflag: tar.TypeReg, Name: "etc/keep", Size: 5, Mode: 0o644}, content: "keep\n"},
		{hdr: &tar.Header{Typeflag: tar.TypeReg, Name: "etc/gone", Size: 5, Mode: 0o644}, content: "gone\n"},
		{hdr: &tar.Header{Typeflag: tar.TypeDir, Name: "opt/", Mode: 0o755}},
		{hdr: &tar.Header{Typeflag: tar.TypeReg, Name: "opt/hidden", Size: 7, Mode: 0o644}, content: "hidden\n"},
	}
	// The upper layer deletes /etc/gone and makes /opt opaque.
	upperTar := []layerEntry{
		{hdr: &tar.Header{Typeflag: tar.TypeReg, Name: "etc/.wh.gone", Size: 0, Mode: 0o644}},
		{hdr: &tar.Header{Typeflag: tar.TypeDir, Name: "opt/", Mode: 0o755}},
		{hdr: &tar.Header{Typeflag: tar.TypeReg, Name: "opt/.wh..wh..opq", Size: 0, Mode: 0o644}},
		{hdr: &tar.Header{Typeflag: tar.TypeReg, Name: "opt/new", Size: 4, Mode: 0o644}, content: "new\n"},
	}

	opts := DefaultOptions()
	opts.Layout = LayoutOCIChangeset

	baseTar := buildTarLayer(t, base)
	var tarOut bytes.Buffer
	if err := WriteMulti(&tarOut, opts, []Input{
		{Kind: TarInput, Reader: bytes.NewReader(baseTar), Digester: HashContent},
		{Kind: TarInput, Reader: bytes.NewReader(buildTarLayer(t, upperTar)), Digester: HashContent},
	}); err != nil {
		t.Fatal(err)
	}

	upperImage, _ := buildErofsLayer(t, upperTar)
	var erofsOut bytes.Buffer
	if err := WriteMulti(&erofsOut, opts, []Input{
		{Kind: TarInput, Reader: bytes.NewReader(baseTar), Digester: HashContent},
		{Kind: ErofsInput, ReaderAt: bytes.NewReader(upperImage), Digester: HashContent},
	}); err != nil {
		t.Fatal(err)
	}

	// Both must have removed the whited-out entry and hidden the opaque
	// directory's lower-layer contents, keeping the new entry.
	for name, spec := range map[string]string{"tar": tarOut.String(), "erofs": erofsOut.String()} {
		if strings.Contains(spec, "etc/gone") {
			t.Errorf("%s: /etc/gone survived the whiteout:\n%s", name, spec)
		}
		if strings.Contains(spec, "opt/hidden") {
			t.Errorf("%s: /opt/hidden survived the opaque whiteout:\n%s", name, spec)
		}
		if !strings.Contains(spec, "etc/keep") {
			t.Errorf("%s: /etc/keep was removed but should have survived:\n%s", name, spec)
		}
		if !strings.Contains(spec, "opt/new") {
			t.Errorf("%s: /opt/new is missing:\n%s", name, spec)
		}
		if strings.Contains(spec, ".wh.") {
			t.Errorf("%s: a whiteout marker leaked into the merged mtree:\n%s", name, spec)
		}
	}
}
