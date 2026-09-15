package tree

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"path"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fileopener"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree/runfiles"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree/treeartifact"
)

type Recorder struct {
	tf          api.TarCAS
	deduplicate bool
	metadata    MetadataProvider
}

// MetadataProvider is an interface for applying metadata to tar headers
type MetadataProvider interface {
	ApplyToHeader(hdr *tar.Header, pathInImage string) error
}

func NewRecorder(tf api.TarCAS) Recorder {
	return Recorder{
		tf:          tf,
		deduplicate: true,
		metadata:    nil,
	}
}

// WithMetadata returns a new Recorder with the given metadata provider
func (r Recorder) WithMetadata(metadata MetadataProvider) Recorder {
	r.metadata = metadata
	return r
}

// ImportTar records every entry of a tar file into the layer.
//
// When deduplicating, the archive is read twice: once to digest every regular
// entry, once to stream those entries into the layer. The digest of an entry
// decides whether it is written as a CAS object or as a hardlink to one, so it
// has to be known before the entry is written -- and a tar stream cannot be
// rewound. Digesting in an earlier pass is what keeps this from having to hold
// each entry in memory, which would cost as much as the largest file in the
// archive.
func (r Recorder) ImportTar(tarFile string) error {
	if !r.deduplicate {
		return r.importTarEntries(tarFile, nil)
	}

	digests, err := r.digestTarEntries(tarFile)
	if err != nil {
		return err
	}
	return r.importTarEntries(tarFile, digests)
}

// tarEntryDigest is the content digest of one regular tar entry, recorded by
// the digest pass for the write pass to consume.
type tarEntryDigest struct {
	size   int64
	digest []byte
}

// digestTarEntries reads the archive and returns the content digest of every
// regular entry, in the order the entries appear.
func (r Recorder) digestTarEntries(tarFile string) ([]tarEntryDigest, error) {
	tr, closeStream, err := openTarStream(tarFile)
	if err != nil {
		return nil, err
	}
	defer closeStream()

	var digests []tarEntryDigest
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		hasher := r.tf.ContentHasher()
		size, err := io.Copy(hasher, tr)
		if err != nil {
			return nil, fmt.Errorf("digesting %s: %w", hdr.Name, err)
		}
		digests = append(digests, tarEntryDigest{size: size, digest: hasher.Sum(nil)})
	}
	return digests, nil
}

// importTarEntries writes every entry of the archive into the layer. When
// deduplicating, digests must hold one digest per regular entry, in the order
// digestTarEntries recorded them.
func (r Recorder) importTarEntries(tarFile string, digests []tarEntryDigest) error {
	tr, closeStream, err := openTarStream(tarFile)
	if err != nil {
		return err
	}
	defer closeStream()

	var seen int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		if hdr.Typeflag != tar.TypeReg {
			if err := r.tf.WriteHeader(hdr); err != nil {
				return err
			}
			continue
		}

		if !r.deduplicate {
			if err := r.tf.WriteRegular(hdr, tr); err != nil {
				return fmt.Errorf("failed to write regular file %s: %w", hdr.Name, err)
			}
			continue
		}

		// A mismatch here means the archive changed underneath us between the
		// two passes, which would otherwise store content under the digest of
		// whatever used to be in its place.
		if seen >= len(digests) {
			return fmt.Errorf("%s gained entries between the digest and write passes", tarFile)
		}
		entry := digests[seen]
		seen++
		if entry.size != hdr.Size {
			return fmt.Errorf("%s changed between the digest and write passes: %s is %d bytes, was %d", tarFile, hdr.Name, hdr.Size, entry.size)
		}
		if err := r.tf.WriteRegularDeduplicatedKnownHash(hdr, tr, entry.digest); err != nil {
			return fmt.Errorf("failed to write regular file %s: %w", hdr.Name, err)
		}
	}
	if seen != len(digests) {
		return fmt.Errorf("%s lost entries between the digest and write passes", tarFile)
	}
	return nil
}

// openTarStream opens a tar file, transparently decompressing it. The returned
// function releases the decompressor -- zstd's owns worker goroutines, and the
// archive is opened once per pass -- and closes the file.
func openTarStream(tarFile string) (*tar.Reader, func(), error) {
	file, err := os.Open(tarFile)
	if err != nil {
		return nil, nil, err
	}

	compression, err := fileopener.LearnCompressionAlgorithm(file)
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	input, err := fileopener.CompressionReaderWithFormat(file, compression)
	if err != nil {
		file.Close()
		return nil, nil, err
	}

	closeStream := func() { file.Close() }
	if decompressor, ok := input.(io.Closer); ok && compression != api.Uncompressed {
		closeStream = func() {
			decompressor.Close()
			file.Close()
		}
	}
	return tar.NewReader(input), closeStream, nil
}

func (r Recorder) RegularFileFromPath(filePath, target string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("opening file %s: %w", filePath, err)
	}
	defer file.Close()

	fInfo, err := file.Stat()
	if err != nil {
		return err
	}

	realHdr, err := tar.FileInfoHeader(fInfo, "")
	if err != nil {
		return err
	}
	if realHdr.Typeflag != tar.TypeReg {
		return errors.New("recorder for regular files invoked on mismatching type")
	}

	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     target,
		Size:     realHdr.Size,
		Mode:     0o755,
		// leave out any extra metadata (for better reproducibility)
	}

	// Apply metadata if provider is set
	if r.metadata != nil {
		if err := r.metadata.ApplyToHeader(hdr, target); err != nil {
			return fmt.Errorf("applying metadata: %w", err)
		}
	}

	// Use optimized path-based methods
	if r.deduplicate {
		return r.tf.WriteRegularFromPathDeduplicated(hdr, filePath)
	} else {
		return r.tf.WriteRegularFromPath(hdr, filePath)
	}
}

func (r Recorder) RegularFile(f io.Reader, info fs.FileInfo, target string) error {
	realHdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	if realHdr.Typeflag != tar.TypeReg {
		return errors.New("recorder for regular files invoked on mismatching type")
	}

	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     target,
		Size:     realHdr.Size,
		Mode:     0o755,
		// leave out any extra metadata (for better reproducibility)
	}

	// Apply metadata if provider is set
	if r.metadata != nil {
		if err := r.metadata.ApplyToHeader(hdr, target); err != nil {
			return fmt.Errorf("applying metadata: %w", err)
		}
	}
	if r.deduplicate {
		err = r.tf.WriteRegularDeduplicated(hdr, f)
	} else {
		err = r.tf.WriteRegular(hdr, f)
	}
	if err != nil {
		return err
	}
	return nil
}

func (r Recorder) TreeFromPath(dirPath, target string) error {
	fsys := treeartifact.TreeArtifactFS(dirPath)
	return r.Tree(fsys, target)
}

// Tree records a directory tree (including all files and subdirectories).
// It creates a symlink in the tar file that points to the root of the tree.
func (r Recorder) Tree(fsys fs.FS, target string) error {
	linkPath, err := r.tf.StoreTree(fsys, target)
	if err != nil {
		return err
	}

	if linkPath == "" {
		return nil
	}

	hdr := &tar.Header{
		Typeflag: tar.TypeSymlink,
		Name:     target,
		Linkname: relativeSymlinkTarget(linkPath, target),
	}
	return r.tf.WriteHeader(hdr)
}

func (r Recorder) Executable(binaryPath, target string, accessor runfilesSupplier) error {
	// First, record the executable itself.
	if err := r.RegularFileFromPath(binaryPath, target); err != nil {
		return err
	}

	// Finally, record the contents of the runfiles tree.
	for p, node := range accessor.Items() {
		switch node.Type() {
		case api.RegularFile:
			// Try to use optimized path-based method if available
			if pathNode, ok := node.(runfiles.PathNode); ok {
				if err := r.RegularFileFromPath(pathNode.Path(), path.Join(target+".runfiles", p)); err != nil {
					return err
				}
			} else {
				// Fallback to original method
				f, err := node.Open()
				if err != nil {
					return err
				}
				info, err := f.Stat()
				if err != nil {
					f.Close()
					return err
				}
				if err := r.RegularFile(f, info, path.Join(target+".runfiles", p)); err != nil {
					f.Close()
					return err
				}
				f.Close()
			}
		case api.Directory:
			fsys, err := node.Tree()
			if err != nil {
				return err
			}
			if err := r.Tree(fsys, path.Join(target+".runfiles", p)); err != nil {
				return err
			}
		case api.Symlink:
			link, err := node.Readlink()
			if err != nil {
				return err
			}
			if err := r.Symlink(link, path.Join(target+".runfiles", p)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported runfiles node type: %s", node.Type())
		}
	}

	return nil
}

func (r Recorder) Symlink(target, linkName string) error {
	hdr := &tar.Header{
		Typeflag: tar.TypeSymlink,
		Name:     linkName,
		Linkname: target,
	}
	return r.tf.WriteHeader(hdr)
}

func (r Recorder) EmptyFile(target string) error {
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     target,
		Size:     0,
		Mode:     0o755,
	}
	if r.metadata != nil {
		if err := r.metadata.ApplyToHeader(hdr, target); err != nil {
			return fmt.Errorf("applying metadata: %w", err)
		}
	}
	return r.tf.WriteRegular(hdr, strings.NewReader(""))
}

// Header records a non-regular entry (directory, symlink, device, ...) from a
// caller-supplied tar header.
//
// Unlike the other Recorder methods, the header is used as given: no metadata
// provider is consulted and no fields are defaulted. This is for callers that
// have already decided every field, such as base image content rules whose
// whole purpose is to specify modes and ownership.
func (r Recorder) Header(hdr *tar.Header) error {
	return r.tf.WriteHeader(hdr)
}

// RegularFromHeader records a regular file from a caller-supplied tar header
// and an in-memory body. As with Header, the header is used verbatim.
func (r Recorder) RegularFromHeader(hdr *tar.Header, content io.Reader) error {
	if r.deduplicate {
		return r.tf.WriteRegularDeduplicated(hdr, content)
	}
	return r.tf.WriteRegular(hdr, content)
}

// RegularFromHeaderAndPath records a regular file from a caller-supplied tar
// header, with the content read from a file on disk. As with Header, the header
// is used verbatim.
func (r Recorder) RegularFromHeaderAndPath(hdr *tar.Header, filePath string) error {
	if r.deduplicate {
		return r.tf.WriteRegularFromPathDeduplicated(hdr, filePath)
	}
	return r.tf.WriteRegularFromPath(hdr, filePath)
}

func relativeSymlinkTarget(target, linkName string) string {
	sourceDir := path.Dir(linkName)
	if sourceDir == "." {
		// special case: symlinks from the root should use target as-is
		return target
	}
	sourceParts := strings.Split(path.Clean(sourceDir), "/")
	targetParts := strings.Split(path.Clean(target), "/")

	// remove common prefix
	for i := 0; i < len(sourceParts) && i < len(targetParts); i++ {
		if sourceParts[i] != targetParts[i] {
			sourceParts = sourceParts[i:]
			targetParts = targetParts[i:]
			break
		}
	}

	var relParts []string

	for range sourceParts {
		relParts = append(relParts, "..")
	}
	relParts = append(relParts, targetParts...)

	return strings.Join(relParts, "/")
}

type runfilesSupplier interface {
	Items() iter.Seq2[string, runfiles.Node]
}
