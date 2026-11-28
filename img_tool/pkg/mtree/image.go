package mtree

import (
	"errors"
	"io"
	"os"
)

// OpenFilesystemImage returns random access to a filesystem image layer blob (EROFS
// ext4 or FAT32) or a cpio layer, which walking a filesystem image requires (the tar path streams
// because a tar can only be read in order anyway). An uncompressed blob is
// addressed in place. A compressed one is decompressed into a temporary file
// rather than into memory, so the cost does not grow with the layer; the
// returned closer removes it.
func OpenFilesystemImage(f *os.File) (io.ReaderAt, io.Closer, error) {
	var magic [4]byte
	n, err := f.ReadAt(magic[:], 0)
	if err != nil && err != io.EOF {
		return nil, nil, err
	}
	gzipped := n >= 2 && magic[0] == 0x1f && magic[1] == 0x8b
	zstded := n >= 4 && magic[0] == 0x28 && magic[1] == 0xb5 && magic[2] == 0x2f && magic[3] == 0xfd
	if !gzipped && !zstded {
		return f, closerFunc(func() error { return nil }), nil
	}

	uncompressed, err := Decompress(io.NewSectionReader(f, 0, 1<<62))
	if err != nil {
		return nil, nil, err
	}
	tmp, err := os.CreateTemp("", "fs-image-layer-*")
	if err != nil {
		return nil, nil, err
	}
	closer := closerFunc(func() error {
		return errors.Join(tmp.Close(), os.Remove(tmp.Name()))
	})
	if _, err := io.Copy(tmp, uncompressed); err != nil {
		_ = closer()
		return nil, nil, err
	}
	return tmp, closer, nil
}

type closerFunc func() error

func (c closerFunc) Close() error { return c() }
