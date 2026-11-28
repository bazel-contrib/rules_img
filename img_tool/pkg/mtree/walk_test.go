package mtree

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"
)

// walkToTar renders a filesystem image layer as a tar through WalkFilesystem,
// the way `img layer-tar` does.
func walkToTar(t *testing.T, kind InputKind, image io.ReaderAt) []byte {
	t.Helper()
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	err := WalkFilesystem(kind, image, func(hdr *tar.Header, content io.Reader) error {
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if content != nil {
			if _, err := io.Copy(tw, content); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the image: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// TestWalkFilesystemMatchesMtree checks that the tar WalkFilesystem renders
// describes the same files, with the same content, as the mtree of the image
// itself. nlink is left out: a hard link of a filesystem image is a second
// regular file in the tar, with a link count of its own.
func TestWalkFilesystemMatchesMtree(t *testing.T) {
	opts := DefaultOptions()
	opts.Keywords = []string{"type", "size", "mode", "uid", "gid", "sha256", "link"}

	ext4Image, _, _ := buildExt4Layer(t)
	cpioArchive, _, _ := buildCpioLayer(t)
	erofsImage, _ := buildErofsLayer(t, equivalenceEntries(t))
	for _, tc := range []struct {
		name  string
		kind  InputKind
		image io.ReaderAt
	}{
		{"ext4", Ext4Input, ext4Image},
		{"cpio", CpioInput, bytes.NewReader(cpioArchive)},
		{"erofs", ErofsInput, bytes.NewReader(erofsImage)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var direct bytes.Buffer
			if err := WriteMulti(&direct, opts, []Input{{Kind: tc.kind, ReaderAt: tc.image, Digester: HashContent}}); err != nil {
				t.Fatal(err)
			}
			viaTar := renderTar(t, walkToTar(t, tc.kind, tc.image), opts)
			if viaTar != direct.String() {
				t.Errorf("the tar describes other files than the image:\n--- tar ---\n%s\n--- image ---\n%s", viaTar, direct.String())
			}
			if !bytes.Contains([]byte(viaTar), []byte("sha256digest=")) {
				t.Errorf("no file content made it into the tar:\n%s", viaTar)
			}
		})
	}
}
