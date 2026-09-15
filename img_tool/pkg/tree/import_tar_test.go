package tree

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compress"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tarcas"
)

// writeTestTar writes an uncompressed tar of the given regular entries and
// returns its path.
func writeTestTar(t *testing.T, entries map[string][]byte, order []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input.tar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for _, name := range order {
		content := entries[name]
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Size:     int64(len(content)),
			Mode:     0o755,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// newTestRecorder returns a Recorder writing an uncompressed tar into out, so
// tests can walk the result without a decompressor in the way.
func newTestRecorder(t *testing.T, out io.Writer) (Recorder, api.TarCAS) {
	t.Helper()
	appender, err := compress.TarAppenderFactory("sha256", "none", false, out)
	if err != nil {
		t.Fatal(err)
	}
	cas := tarcas.NewSHA256CAS(appender)
	return NewRecorder(cas), cas
}

func importTar(t *testing.T, tarFile string) []*tar.Header {
	t.Helper()
	var out bytes.Buffer
	recorder, cas := newTestRecorder(t, &out)
	if err := recorder.ImportTar(tarFile); err != nil {
		t.Fatal(err)
	}
	if err := cas.Close(); err != nil {
		t.Fatal(err)
	}

	var headers []*tar.Header
	tr := tar.NewReader(bytes.NewReader(out.Bytes()))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			t.Fatal(err)
		}
		headers = append(headers, hdr)
	}
	return headers
}

// TestImportTarDeduplicates checks the digest pass feeds the write pass the
// digests it needs to recognize repeated content: the second copy has to become
// a hardlink to the first rather than a second body.
func TestImportTarDeduplicates(t *testing.T) {
	shared := bytes.Repeat([]byte("shared"), 1024)
	entries := map[string][]byte{
		"first":  shared,
		"second": shared,
		"other":  []byte("other"),
	}
	path := writeTestTar(t, entries, []string{"first", "second", "other"})

	var bodies, links int
	for _, hdr := range importTar(t, path) {
		switch hdr.Typeflag {
		case tar.TypeReg:
			bodies++
			if want := int64(len(entries[hdr.Name])); hdr.Size != want {
				t.Errorf("%s: size = %d, want %d", hdr.Name, hdr.Size, want)
			}
		case tar.TypeLink:
			links++
			if hdr.Name != "second" || hdr.Linkname != "first" {
				t.Errorf("unexpected hardlink %s -> %s", hdr.Name, hdr.Linkname)
			}
		default:
			t.Errorf("unexpected entry type %q for %s", hdr.Typeflag, hdr.Name)
		}
	}
	if bodies != 2 || links != 1 {
		t.Errorf("got %d bodies and %d hardlinks, want 2 and 1", bodies, links)
	}
}

// TestImportTarDoesNotBufferEntries is the regression test for the import path
// holding whole entries in memory. Digesting in a separate pass means the write
// pass streams, so the total allocated over an import must not scale with the
// largest entry.
func TestImportTarDoesNotBufferEntries(t *testing.T) {
	const entrySize = 8 << 20
	path := writeTestTar(t, map[string][]byte{"big": make([]byte, entrySize)}, []string{"big"})

	// The output goes nowhere: the point is what the import allocates, not what
	// it produces, and collecting the tar would dwarf the reading.
	recorder, cas := newTestRecorder(t, io.Discard)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if err := recorder.ImportTar(path); err != nil {
		t.Fatal(err)
	}
	if err := cas.Close(); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)

	// Generous: buffering allocated well over the entry size, because
	// bytes.Buffer reallocates as it grows. Streaming stays in the tens of KiB.
	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated > entrySize/2 {
		t.Errorf("import allocated %d bytes for a %d byte entry; content is being buffered", allocated, entrySize)
	}
}

// TestImportTarDetectsArchiveChangedBetweenPasses covers the guard that stops
// the write pass from pairing an entry with a digest taken from different
// content, which would otherwise store a body under the wrong hash.
func TestImportTarDetectsArchiveChangedBetweenPasses(t *testing.T) {
	path := writeTestTar(t, map[string][]byte{"a": []byte("aaaa"), "b": []byte("bb")}, []string{"a", "b"})
	recorder, _ := newTestRecorder(t, io.Discard)

	digests, err := recorder.digestTarEntries(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(digests) != 2 {
		t.Fatalf("digested %d entries, want 2", len(digests))
	}

	t.Run("size mismatch", func(t *testing.T) {
		stale := []tarEntryDigest{{size: digests[0].size + 1, digest: digests[0].digest}, digests[1]}
		if err := recorder.importTarEntries(path, stale); err == nil {
			t.Error("write pass accepted a digest recorded for a different size")
		}
	})

	t.Run("entry gained", func(t *testing.T) {
		if err := recorder.importTarEntries(path, digests[:1]); err == nil {
			t.Error("write pass accepted an archive with more entries than were digested")
		}
	})

	t.Run("entry lost", func(t *testing.T) {
		extra := append(append([]tarEntryDigest{}, digests...), digests[0])
		if err := recorder.importTarEntries(path, extra); err == nil {
			t.Error("write pass accepted an archive with fewer entries than were digested")
		}
	})
}
