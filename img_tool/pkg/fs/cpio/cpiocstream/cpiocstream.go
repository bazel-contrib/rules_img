// Package cpiocstream writes the compact stream of a cpio archive built by
// [cpio] (see docs/compact-stream.md).
//
// The compact stream represents the archive as a small file plus the contents
// of the files inside it: every file of at least a minimum size is replaced by
// a reference to its content in a content-addressed store, and everything else
// (headers, names, symbolic link targets, small files and padding) stays
// inline. Because [cpio] stores the content of each file contiguously, the
// references line up with the files of the archive.
//
// An archive that is compressed on its way to the layer file is described by
// the compact stream of the uncompressed archive, which records how to
// compress it again, and the digest and size of the compressed file.
package cpiocstream

import (
	"errors"
	"fmt"
	"io"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/cpio"
)

// DefaultMinSize is the size from which files are replaced by references
// unless [Options.MinSize] says otherwise. It matches the default inline
// threshold of layers.
const DefaultMinSize = 4096

// Options configures the compact stream.
type Options struct {
	// MinSize is the smallest file that is replaced by a reference. Smaller
	// files stay inline. Zero means [DefaultMinSize].
	MinSize int64
	// Original describes how the archive was compressed to make the layer
	// file. The zero value is an uncompressed layer.
	Original compactstream.OriginalCompressionInfo
	// CompressedDigest and CompressedSize are the SHA-256 digest and the size
	// of the compressed layer file. They are required if Original has a
	// compression, and ignored otherwise (the archive itself is the file).
	CompressedDigest []byte
	CompressedSize   uint64
}

// Write writes the compact stream of an archive to stream. info describes the
// archive and comes from [cpio.Builder.Build] of a builder created with
// [cpio.Options.DigestFiles] and [cpio.Options.DigestArchive]. archive holds
// the complete, uncompressed archive. Because the digest of the archive is
// known, the files that become references are not read back.
//
// The compact stream is only complete together with the content-addressed
// blobs of the referenced files, which the caller already has: they are the
// files that went into the archive.
func Write(info *cpio.Info, archive io.ReaderAt, stream io.Writer, opts Options) error {
	minSize := opts.MinSize
	if minSize <= 0 {
		minSize = DefaultMinSize
	}
	if info.Digest == nil {
		return errors.New("cpiocstream: the builder does not record the archive digest, set cpio.Options.DigestArchive or Info.Digest")
	}
	ranges := make([]compactstream.Range, len(info.Files))
	for i, f := range info.Files {
		if f.Digest == nil {
			return errors.New("cpiocstream: the builder does not record file digests, set cpio.Options.DigestFiles")
		}
		ranges[i] = compactstream.Range{Offset: f.Offset, Size: f.Size, Digest: f.Digest}
	}

	w := compactstream.NewWriter(stream, compactstream.HashAlgoSHA256, 32, compactstream.StreamCompressionZstd, opts.Original, minSize)
	defer w.Abort() // releases the incremental compressor if we return early
	w.SetContentFormat(compactstream.ContentFormatCpio)
	if err := w.CompressIncrementally(); err != nil {
		return err
	}
	if opts.Original.Compression != compactstream.OriginalCompressionNone {
		if err := w.SetCompressedStreamInfo(opts.CompressedDigest, opts.CompressedSize); err != nil {
			return err
		}
	}
	if err := compactstream.WriteImageKnownDigest(w, io.NewSectionReader(archive, 0, info.Size), info.Size, ranges, minSize, info.Digest); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("cpiocstream: writing compact stream: %w", err)
	}
	return nil
}
