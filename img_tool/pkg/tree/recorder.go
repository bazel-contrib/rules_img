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

func (r Recorder) ImportTar(tarFile string) error {
	file, err := os.Open(tarFile)
	if err != nil {
		return err
	}
	defer file.Close()

	input, err := fileopener.CompressionReader(file)
	if err != nil {
		return err
	}

	tr := tar.NewReader(input)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		if hdr.Typeflag == tar.TypeReg {
			var err error
			if r.deduplicate {
				err = r.tf.WriteRegularDeduplicated(hdr, tr)
			} else {
				err = r.tf.WriteRegular(hdr, tr)
			}
			if err != nil {
				return fmt.Errorf("failed to write regular file %s: %w", hdr.Name, err)
			}
		} else {
			if err := r.tf.WriteHeader(hdr); err != nil {
				return err
			}
		}
	}
	return nil
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
	linkPath, err := r.tf.StoreTree(fsys)
	if err != nil {
		return err
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
