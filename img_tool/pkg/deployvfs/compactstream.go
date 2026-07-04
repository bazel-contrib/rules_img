package deployvfs

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
)

// layerFromOCILayoutCompactStream reconstructs a layer from a compact stream stored
// inside an explicit OCI layout directory (--oci-layout). The stream is looked up at
// <layout>/blobs/<algo>/<hex>.cstream. CAS references embedded in the stream are
// resolved against the same layout's blobs directory (blobs/sha256/<hex>) when the
// referenced blob is present there, falling back to the disk cache / remote cache
// (see casDirStore). This lets an OCI layout be a self-contained deploy source.
func (b *Builder) layerFromOCILayoutCompactStream(desc api.Descriptor) (blobEntry, error) {
	if len(b.ociLayouts) == 0 {
		return blobEntry{}, &BlobSourceError{Source: "OCI layout compact stream", Digest: desc.Digest, Kind: BlobSourceUnconfigured, Message: "no OCI layouts configured"}
	}
	var checkedPaths []string
	for _, layoutPath := range b.ociLayouts {
		compactStreamPath := sparseLayoutBlobPathInDir(layoutPath, desc.Digest) + ".cstream"
		checkedPaths = append(checkedPaths, compactStreamPath)
		if _, err := os.Stat(compactStreamPath); err != nil {
			continue
		}
		// The layout's blobs directory doubles as the content-addressed input
		// directory: CAS references (addressed by sha256) are resolved from
		// blobs/sha256/<hex> when present. casDirStore falls back to the disk /
		// remote cache for blobs the layout does not ship.
		casDirPath := filepath.Join(layoutPath, "blobs")
		return b.layerFromCompactStream(compactStreamPath, casDirPath, desc), nil
	}
	return blobEntry{}, &BlobSourceError{Source: "OCI layout compact stream", Digest: desc.Digest, Kind: BlobSourceBlobMissing, Message: fmt.Sprintf("not found in %d OCI layout(s) (checked: %s)", len(b.ociLayouts), strings.Join(checkedPaths, ", "))}
}

// layerFromRunfilesCompactStream reconstructs a layer from its compact stream (the
// .cstream in the runfiles sparse OCI layout). The layer's content-addressed input
// directory (.inputfilecas) is shipped for eager strategies but intentionally
// omitted for lazy ones; when it is absent the referenced blobs are fetched from
// the disk cache / remote cache instead (see casDirStore).
func (b *Builder) layerFromRunfilesCompactStream(operationIndex int, manifestIndex int, layerIndex int, desc api.Descriptor) (blobEntry, error) {
	compactStreamRunfilesPath := sparseLayoutBlobPath(operationIndex, desc.Digest) + ".cstream"
	compactStreamPath, err := b.rlocation(compactStreamRunfilesPath)
	if err != nil {
		return blobEntry{}, &BlobSourceError{Source: "compact stream", Digest: desc.Digest, Kind: BlobSourceOther, Message: fmt.Sprintf("rlocation(%s)", compactStreamRunfilesPath), Err: err}
	}
	if _, err := os.Stat(compactStreamPath); err != nil {
		return blobEntry{}, &BlobSourceError{Source: "compact stream", Digest: desc.Digest, Kind: BlobSourceBlobMissing, Message: compactStreamPath, Err: err}
	}

	// The content-addressed input directory (.inputfilecas) is shipped for eager
	// strategies, but intentionally omitted for lazy ones. When it is absent, the
	// referenced blobs are fetched from the disk cache / remote cache instead (see
	// casDirStore), so a missing directory is not an error here.
	casDirPath := ""
	casRunfilesPath := layerRunfilesPath(operationIndex, manifestIndex, layerIndex) + ".inputfilecas"
	if p, rerr := b.rlocation(casRunfilesPath); rerr == nil {
		if fi, serr := os.Stat(p); serr == nil && fi.IsDir() {
			casDirPath = p
		}
	}

	return b.layerFromCompactStream(compactStreamPath, casDirPath, desc), nil
}

// layerFromCompactStream creates a blobEntry that reconstructs the tar layer on-the-fly
// from a .cstream. CAS references are resolved against, in order: the
// content-addressed input directory (sha256/<hex>) when present (eager
// strategies), the Bazel disk cache, and the remote CAS (lazy strategies, where
// the input directory is omitted). casDirPath may be empty if no input directory
// was shipped.
func (b *Builder) layerFromCompactStream(compactStreamPath, casDirPath string, desc api.Descriptor) blobEntry {
	stats := b.stats
	builder := b
	return blobEntry{
		Descriptor: desc,
		Location:   "compact_stream",
		stats:      stats,
		Opener: func() (io.ReadCloser, error) {
			compactStreamFile, err := os.Open(compactStreamPath)
			if err != nil {
				return nil, fmt.Errorf("opening compact stream %s: %w", compactStreamPath, err)
			}

			shaDir := ""
			if casDirPath != "" {
				shaDir = filepath.Join(casDirPath, "sha256")
			}
			store := &casDirStore{
				shaDir:        shaDir,
				diskCachePath: builder.diskCachePath,
				casReader:     builder.casReader,
			}

			pr, pw := io.Pipe()
			go func() {
				err := compactstream.Reconstruct(builder.context(), compactStreamFile, store, pw)
				compactStreamFile.Close()
				pw.CloseWithError(err)
			}()

			stats.BlobsFromCompactStream.Add(1)
			return pr, nil
		},
	}
}

// casDirStore is a compactstream.BlobStore that resolves CAS references (addressed by
// the sha256 of their content) from multiple sources, in order:
//  1. a content-addressed input directory laid out as sha256/<hex> (shipped with
//     eager strategies);
//  2. the Bazel disk cache;
//  3. the remote CAS (used by lazy strategies, where the input directory is
//     omitted on purpose).
type casDirStore struct {
	shaDir        string // <inputfilecas>/sha256, or "" if no input directory was shipped
	diskCachePath string
	casReader     casReader
}

func (s *casDirStore) ReaderForBlob(ctx context.Context, digest []byte, size int64) (io.ReadCloser, error) {
	hexDigest := hex.EncodeToString(digest)

	// 1. Content-addressed input directory (eager). Files are addressed by their
	// content digest, so they are trusted without an extra size check.
	if s.shaDir != "" {
		if f, err := os.Open(filepath.Join(s.shaDir, hexDigest)); err == nil {
			return f, nil
		}
	}

	// 2. Bazel disk cache.
	if s.diskCachePath != "" {
		cachePath := diskCacheBlobPath(s.diskCachePath, "sha256:"+hexDigest)
		if f, err := os.Open(cachePath); err == nil {
			info, err := f.Stat()
			if err == nil && info.Size() == size {
				return f, nil
			}
			f.Close()
		}
	}

	// 3. Remote CAS.
	if s.casReader != nil {
		casDigest := cas.SHA256(digest, size)
		return s.casReader.ReaderForBlob(ctx, casDigest)
	}

	return nil, fmt.Errorf("blob sha256:%s (size %d) not found in input file CAS directory, disk cache, or remote cache", hexDigest, size)
}
