package compactstream

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/casdirlayout"
)

// DirStore is a [BlobStore] backed by content-addressed directories, as written
// by `img cas-dir`: a blob is the file sha256/<first two hex digits>/<hex of its
// SHA-256> in a directory (the flat sha256/<hex> layout is read as well). With several directories, a blob is taken from the first one that
// has it.
type DirStore struct {
	dirs []string
}

// NewDirStore returns a store over the content-addressed directories, which are
// searched in the order given.
func NewDirStore(casDirs ...string) *DirStore {
	dirs := make([]string, len(casDirs))
	for i, d := range casDirs {
		dirs[i] = filepath.Join(d, "sha256")
	}
	return &DirStore{dirs: dirs}
}

// ReaderForBlob opens the blob with the given digest.
func (s *DirStore) ReaderForBlob(_ context.Context, digest []byte, size int64) (io.ReadCloser, error) {
	name := hex.EncodeToString(digest)
	var firstErr error
	for _, dir := range s.dirs {
		f, err := casdirlayout.Open(dir, name)
		if err == nil {
			return f, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = os.ErrNotExist
	}
	return nil, fmt.Errorf("blob sha256:%s (size %d) not found in content-addressed directory: %w", name, size, firstErr)
}
