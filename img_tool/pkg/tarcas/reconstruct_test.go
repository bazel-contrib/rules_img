package tarcas

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compress"
)

type memBlobStore struct {
	blobs map[string][]byte
}

func newMemBlobStore() *memBlobStore {
	return &memBlobStore{blobs: make(map[string][]byte)}
}

func (m *memBlobStore) Store(data []byte) []byte {
	digest := sha256.Sum256(data)
	m.blobs[string(digest[:])] = append([]byte(nil), data...)
	return digest[:]
}

func (m *memBlobStore) ReaderForBlob(_ context.Context, digest []byte, _ int64) (io.ReadCloser, error) {
	data, ok := m.blobs[string(digest)]
	if !ok {
		return nil, fmt.Errorf("blob not found: %x", digest)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

type tarSettings struct {
	compression      string
	estargz          bool
	compressionLevel int
	compressorJobs   int
}

func (s tarSettings) originalCompression() uint8 {
	switch s.compression {
	case "gzip":
		return compactstream.OriginalCompressionGzip
	case "zstd":
		return compactstream.OriginalCompressionZstd
	default:
		return compactstream.OriginalCompressionNone
	}
}

func (s tarSettings) compressOpts() []compress.Option {
	var opts []compress.Option
	if s.compressionLevel >= 0 {
		opts = append(opts, compress.CompressionLevel(s.compressionLevel))
	}
	if s.compressorJobs > 0 {
		opts = append(opts, compress.CompressorJobs(s.compressorJobs))
	}
	return opts
}

func buildTarDirect(t *testing.T, entries []testEntry, settings tarSettings) []byte {
	t.Helper()
	var buf bytes.Buffer
	appender, err := compress.TarAppenderFactory("sha256", settings.compression, settings.estargz, &buf, settings.compressOpts()...)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		headerBytes, err := compactstream.CaptureTarHeaderBytes(e.hdr)
		if err != nil {
			t.Fatal(err)
		}
		if e.hdr.Typeflag == tar.TypeReg && e.hdr.Size > 0 {
			entryReader := io.MultiReader(bytes.NewReader(headerBytes), bytes.NewReader(e.content))
			padded := &paddedReader{Reader: entryReader, padSize: 512}
			if err := appender.AppendTar(padded); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := appender.AppendTar(bytes.NewReader(headerBytes)); err != nil {
				t.Fatal(err)
			}
		}
	}

	if _, err := appender.Finalize(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func buildIndexAndReconstruct(t *testing.T, entries []testEntry, settings tarSettings, streamCompression uint8, inlineThreshold int64) []byte {
	t.Helper()
	store := newMemBlobStore()

	var indexBuf bytes.Buffer
	iw := compactstream.NewWriter(&indexBuf, compactstream.HashAlgoSHA256, 32, streamCompression,
		compactstream.OriginalCompressionInfo{
			Compression:      settings.originalCompression(),
			Seekable:         settings.estargz,
			CompressionLevel: int8(settings.compressionLevel),
			CompressorJobs:   uint8(settings.compressorJobs),
		}, inlineThreshold)

	obs := newCompactStreamObserver[SHA256Helper](iw)

	for _, e := range entries {
		var digest []byte
		if e.hdr.Typeflag == tar.TypeReg && e.hdr.Size > 0 {
			digest = store.Store(e.content)
		}

		contentWriter, err := obs.BeginEntry(e.hdr, digest)
		if err != nil {
			t.Fatal(err)
		}
		if contentWriter != nil && e.content != nil {
			if _, err := contentWriter.Write(e.content); err != nil {
				t.Fatal(err)
			}
		}
		if err := obs.EndEntry(); err != nil {
			t.Fatal(err)
		}
	}

	if err := obs.Close(); err != nil {
		t.Fatal(err)
	}
	if err := iw.Close(); err != nil {
		t.Fatal(err)
	}

	var reconstructed bytes.Buffer
	err := compactstream.Reconstruct(context.Background(), &indexBuf, store, &reconstructed)
	if err != nil {
		t.Fatal(err)
	}
	return reconstructed.Bytes()
}

type testEntry struct {
	hdr     *tar.Header
	content []byte
}

// TestReconstructValidatesCompressedStreamInfo checks that when the header
// records the compressed-stream digest and size, compactstream.Reconstruct succeeds
// only when the reconstructed compressed stream matches, and fails with a clear
// error otherwise.
func TestReconstructValidatesCompressedStreamInfo(t *testing.T) {
	store := newMemBlobStore()
	content := bytes.Repeat([]byte("payload-"), 200)
	entries := []testEntry{
		{hdr: &tar.Header{Typeflag: tar.TypeReg, Name: "file.bin", Size: int64(len(content)), Mode: 0o644}, content: content},
	}
	settings := tarSettings{compression: "gzip", compressionLevel: 6, compressorJobs: 1}

	buildIndex := func(compressedDigest []byte, compressedSize uint64) *bytes.Buffer {
		var indexBuf bytes.Buffer
		iw := compactstream.NewWriter(&indexBuf, compactstream.HashAlgoSHA256, 32, compactstream.StreamCompressionZstd,
			compactstream.OriginalCompressionInfo{
				Compression:      settings.originalCompression(),
				CompressionLevel: int8(settings.compressionLevel),
				CompressorJobs:   uint8(settings.compressorJobs),
			}, 0)
		obs := newCompactStreamObserver[SHA256Helper](iw)
		for _, e := range entries {
			var digest []byte
			if e.hdr.Typeflag == tar.TypeReg && e.hdr.Size > 0 {
				digest = store.Store(e.content)
			}
			w, err := obs.BeginEntry(e.hdr, digest)
			if err != nil {
				t.Fatal(err)
			}
			if w != nil && e.content != nil {
				if _, err := w.Write(e.content); err != nil {
					t.Fatal(err)
				}
			}
			if err := obs.EndEntry(); err != nil {
				t.Fatal(err)
			}
		}
		if err := obs.Close(); err != nil {
			t.Fatal(err)
		}
		if compressedDigest != nil {
			if err := iw.SetCompressedStreamInfo(compressedDigest, compressedSize); err != nil {
				t.Fatal(err)
			}
		}
		if err := iw.Close(); err != nil {
			t.Fatal(err)
		}
		return &indexBuf
	}

	// Reconstruct once without recorded info to learn the true compressed
	// digest and size of the reconstructed stream.
	var truth bytes.Buffer
	if err := compactstream.Reconstruct(context.Background(), buildIndex(nil, 0), store, &truth); err != nil {
		t.Fatal(err)
	}
	trueDigest := sha256.Sum256(truth.Bytes())
	trueSize := uint64(truth.Len())

	// Correct info → success, identical output.
	var out bytes.Buffer
	if err := compactstream.Reconstruct(context.Background(), buildIndex(trueDigest[:], trueSize), store, &out); err != nil {
		t.Fatalf("expected success with correct compressed-stream info: %v", err)
	}
	if !bytes.Equal(out.Bytes(), truth.Bytes()) {
		t.Fatal("reconstructed output changed when compressed-stream info was present")
	}

	// Wrong size → failure.
	if err := compactstream.Reconstruct(context.Background(), buildIndex(trueDigest[:], trueSize+1), store, io.Discard); err == nil {
		t.Fatal("expected failure with wrong compressed-stream size")
	}

	// Wrong digest → failure with a descriptive message.
	wrongDigest := sha256.Sum256([]byte("not the stream"))
	err := compactstream.Reconstruct(context.Background(), buildIndex(wrongDigest[:], trueSize), store, io.Discard)
	if err == nil {
		t.Fatal("expected failure with wrong compressed-stream digest")
	}
	if !strings.Contains(err.Error(), "expected a compressed stream with digest") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestReconstructEmptyArchive(t *testing.T) {
	settings := tarSettings{compression: "gzip", compressionLevel: 6, compressorJobs: 1}
	entries := []testEntry{}
	direct := buildTarDirect(t, entries, settings)
	reconstructed := buildIndexAndReconstruct(t, entries, settings, compactstream.StreamCompressionZstd, 0)

	if !bytes.Equal(direct, reconstructed) {
		t.Fatalf("empty archive mismatch: direct=%d bytes, reconstructed=%d bytes", len(direct), len(reconstructed))
	}
}

func TestReconstructSingleFile(t *testing.T) {
	settings := tarSettings{compression: "gzip", compressionLevel: 6, compressorJobs: 1}
	content := []byte("hello world, this is a test file")
	entries := []testEntry{
		{
			hdr: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "hello.txt",
				Size:     int64(len(content)),
				Mode:     0o644,
			},
			content: content,
		},
	}

	direct := buildTarDirect(t, entries, settings)
	reconstructed := buildIndexAndReconstruct(t, entries, settings, compactstream.StreamCompressionZstd, 0)

	if !bytes.Equal(direct, reconstructed) {
		t.Fatalf("single file mismatch: direct=%d bytes, reconstructed=%d bytes", len(direct), len(reconstructed))
	}
}

func TestReconstructMixedEntries(t *testing.T) {
	settings := tarSettings{compression: "gzip", compressionLevel: 6, compressorJobs: 1}
	fileContent := []byte("file content here")
	entries := []testEntry{
		{
			hdr: &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     "mydir/",
				Mode:     0o755,
			},
		},
		{
			hdr: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "mydir/file.txt",
				Size:     int64(len(fileContent)),
				Mode:     0o644,
			},
			content: fileContent,
		},
		{
			hdr: &tar.Header{
				Typeflag: tar.TypeSymlink,
				Name:     "mydir/link",
				Linkname: "file.txt",
			},
		},
		{
			hdr: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "empty",
				Size:     0,
				Mode:     0o644,
			},
		},
	}

	direct := buildTarDirect(t, entries, settings)
	reconstructed := buildIndexAndReconstruct(t, entries, settings, compactstream.StreamCompressionZstd, 0)

	if !bytes.Equal(direct, reconstructed) {
		t.Fatalf("mixed entries mismatch: direct=%d bytes, reconstructed=%d bytes", len(direct), len(reconstructed))
	}
}

func TestReconstructUncompressedOriginal(t *testing.T) {
	settings := tarSettings{compression: "none", compressionLevel: -1, compressorJobs: 1}
	content := []byte("uncompressed tar content")
	entries := []testEntry{
		{
			hdr: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "file.txt",
				Size:     int64(len(content)),
				Mode:     0o644,
			},
			content: content,
		},
	}

	direct := buildTarDirect(t, entries, settings)
	reconstructed := buildIndexAndReconstruct(t, entries, settings, compactstream.StreamCompressionNone, 0)

	if !bytes.Equal(direct, reconstructed) {
		t.Fatalf("uncompressed mismatch: direct=%d bytes, reconstructed=%d bytes", len(direct), len(reconstructed))
	}
}

func TestReconstructWithInlineThreshold(t *testing.T) {
	settings := tarSettings{compression: "gzip", compressionLevel: 6, compressorJobs: 1}
	smallContent := []byte("small")
	largeContent := make([]byte, 5000)
	for i := range largeContent {
		largeContent[i] = byte(i % 256)
	}

	entries := []testEntry{
		{
			hdr: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "small.txt",
				Size:     int64(len(smallContent)),
				Mode:     0o644,
			},
			content: smallContent,
		},
		{
			hdr: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "large.bin",
				Size:     int64(len(largeContent)),
				Mode:     0o644,
			},
			content: largeContent,
		},
	}

	direct := buildTarDirect(t, entries, settings)
	reconstructed := buildIndexAndReconstruct(t, entries, settings, compactstream.StreamCompressionZstd, 4096)

	if !bytes.Equal(direct, reconstructed) {
		t.Fatalf("inline threshold mismatch: direct=%d bytes, reconstructed=%d bytes", len(direct), len(reconstructed))
	}
}

func randomTarSettings(rng *rand.Rand) tarSettings {
	algorithms := []string{"none", "gzip", "zstd"}
	algo := algorithms[rng.Intn(len(algorithms))]

	var level int
	var jobs int
	estargz := false

	switch algo {
	case "gzip":
		level = 1 + rng.Intn(9) // 1-9
		jobs = 1 + rng.Intn(4)  // 1-4
		estargz = rng.Intn(2) == 0
		if estargz {
			// estargz gzip is not parallelized; keep jobs deterministic so the
			// recorded/replayed value cannot perturb the comparison.
			jobs = 1
		}
	case "zstd":
		level = 1 + rng.Intn(4) // 1-4 (keep fast for tests)
		jobs = 1
		estargz = rng.Intn(2) == 0
	default:
		level = -1
		jobs = 1
	}

	return tarSettings{
		compression:      algo,
		estargz:          estargz,
		compressionLevel: level,
		compressorJobs:   jobs,
	}
}

func randomInlineThreshold(rng *rand.Rand) int64 {
	choices := []int64{0, 0, 32, 128, 256, 1024, 4096, 8192, 16384}
	return choices[rng.Intn(len(choices))]
}

func randomStreamCompression(rng *rand.Rand) uint8 {
	if rng.Intn(2) == 0 {
		return compactstream.StreamCompressionNone
	}
	return compactstream.StreamCompressionZstd
}

func randomEntries(rng *rand.Rand) []testEntry {
	numEntries := rng.Intn(129) // 0 to 128
	var entries []testEntry
	dirs := []string{""}
	var lastReg string // most recent regular file, used as a hardlink target

	modes := []int64{0o644, 0o755, 0o600, 0o444, 0o777}

	for j := range numEntries {
		roll := rng.Float64()
		switch {
		case roll < 0.45:
			// Regular file
			size := rng.Intn(8193) // 0 to 8192 bytes
			content := make([]byte, size)
			rng.Read(content)
			parent := dirs[rng.Intn(len(dirs))]
			name := fmt.Sprintf("%sfile_%d.bin", parent, j)
			entries = append(entries, testEntry{
				hdr: &tar.Header{
					Typeflag: tar.TypeReg,
					Name:     name,
					Size:     int64(size),
					Mode:     modes[rng.Intn(len(modes))],
				},
				content: content,
			})
			lastReg = name
		case roll < 0.65:
			// Directory
			parent := dirs[rng.Intn(len(dirs))]
			name := fmt.Sprintf("%sdir_%d/", parent, j)
			entries = append(entries, testEntry{
				hdr: &tar.Header{
					Typeflag: tar.TypeDir,
					Name:     name,
					Mode:     modes[rng.Intn(len(modes))],
				},
			})
			dirs = append(dirs, name)
		case roll < 0.80:
			// Symlink
			parent := dirs[rng.Intn(len(dirs))]
			name := fmt.Sprintf("%slink_%d", parent, j)
			target := fmt.Sprintf("target_%d", rng.Intn(100))
			entries = append(entries, testEntry{
				hdr: &tar.Header{
					Typeflag: tar.TypeSymlink,
					Name:     name,
					Linkname: target,
				},
			})
		case roll < 0.90 && lastReg != "":
			// Hardlink to an earlier regular file
			parent := dirs[rng.Intn(len(dirs))]
			name := fmt.Sprintf("%shard_%d", parent, j)
			entries = append(entries, testEntry{
				hdr: &tar.Header{
					Typeflag: tar.TypeLink,
					Name:     name,
					Linkname: lastReg,
					Size:     0,
				},
			})
		default:
			// Empty regular file
			parent := dirs[rng.Intn(len(dirs))]
			name := fmt.Sprintf("%sempty_%d", parent, j)
			entries = append(entries, testEntry{
				hdr: &tar.Header{
					Typeflag: tar.TypeReg,
					Name:     name,
					Size:     0,
					Mode:     modes[rng.Intn(len(modes))],
				},
			})
		}
	}
	return entries
}

func TestReconstructRandomized(t *testing.T) {
	for i := range 10 {
		t.Run(fmt.Sprintf("seed_%d", i), func(t *testing.T) {
			rng := rand.New(rand.NewSource(int64(i)))

			settings := randomTarSettings(rng)
			streamComp := randomStreamCompression(rng)
			threshold := randomInlineThreshold(rng)
			entries := randomEntries(rng)

			t.Logf("tar=%s level=%d jobs=%d estargz=%v | stream_comp=%d inline=%d | entries=%d",
				settings.compression, settings.compressionLevel, settings.compressorJobs,
				settings.estargz, streamComp, threshold, len(entries))

			direct := buildTarDirect(t, entries, settings)
			reconstructed := buildIndexAndReconstruct(t, entries, settings, streamComp, threshold)

			if !bytes.Equal(direct, reconstructed) {
				t.Fatalf("mismatch: direct=%d bytes, reconstructed=%d bytes", len(direct), len(reconstructed))
			}
		})
	}
}
