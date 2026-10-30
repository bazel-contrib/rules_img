package mtree

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/fat32"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/fat32/fat32cstream"
)

func buildFat32Layer(t *testing.T) (image, cstream []byte, big string) {
	t.Helper()
	b, err := fat32.NewBuilder(fat32.Options{DigestFiles: true, DigestImage: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	big = strings.Repeat("shared payload\n", 500)
	mod := time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC)
	add := func(hdr *tar.Header, content string) {
		t.Helper()
		hdr.ModTime = mod
		var err error
		if hdr.Typeflag == tar.TypeReg {
			hdr.Size = int64(len(content))
			err = b.WriteRegular(hdr, strings.NewReader(content))
		} else {
			err = b.WriteHeader(hdr)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	add(&tar.Header{Typeflag: tar.TypeDir, Name: "etc/", Mode: 0o755}, "")
	add(&tar.Header{Typeflag: tar.TypeReg, Name: "etc/big", Mode: 0o644}, big)
	add(&tar.Header{Typeflag: tar.TypeReg, Name: "etc/small", Mode: 0o444}, "tiny\n")
	add(&tar.Header{Typeflag: tar.TypeLink, Name: "etc/copy", Linkname: "etc/small"}, "")

	f := &memImage{}
	var stream bytes.Buffer
	if _, err := fat32cstream.Build(b, f, &stream, fat32cstream.Options{}); err != nil {
		t.Fatal(err)
	}
	return f.b, stream.Bytes(), big
}

// memImage is an in-memory target that reads zeros where nothing was written.
type memImage struct{ b []byte }

func (m *memImage) WriteAt(p []byte, off int64) (int, error) {
	if end := off + int64(len(p)); end > int64(len(m.b)) {
		m.b = append(m.b, make([]byte, end-int64(len(m.b)))...)
	}
	return copy(m.b[off:], p), nil
}

func (m *memImage) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(m.b)) {
		return 0, fmt.Errorf("EOF")
	}
	return copy(p, m.b[off:]), nil
}

func (m *memImage) Truncate(size int64) error {
	if size > int64(len(m.b)) {
		m.b = append(m.b, make([]byte, size-int64(len(m.b)))...)
	}
	m.b = m.b[:size]
	return nil
}

func TestFat32Mtree(t *testing.T) {
	image, cstream, big := buildFat32Layer(t)

	var fromImage bytes.Buffer
	if err := WriteMulti(&fromImage, DefaultOptions(), []Input{{Kind: Fat32Input, ReaderAt: bytes.NewReader(image), Digester: HashContent}}); err != nil {
		t.Fatal(err)
	}
	sparse, err := compactstream.NewSparseReaderAt(bytes.NewReader(cstream))
	if err != nil {
		t.Fatal(err)
	}
	var fromStream bytes.Buffer
	if err := WriteMulti(&fromStream, DefaultOptions(), []Input{{Kind: Fat32Input, ReaderAt: sparse, Digester: HashContent, RangeDigester: sparse.RefDigestInRange}}); err != nil {
		t.Fatal(err)
	}
	if fromImage.String() != fromStream.String() {
		t.Errorf("mtree from the compact stream differs from the one from the image.\n--- image ---\n%s\n--- cstream ---\n%s", fromImage.String(), fromStream.String())
	}

	sum := sha256.Sum256([]byte(big))
	small := sha256.Sum256([]byte("tiny\n"))
	for _, want := range []string{
		fmt.Sprintf("etc/big type=file size=%d mode=0644", len(big)),
		fmt.Sprintf("sha256digest=%x", sum),
		"etc/small type=file size=5 mode=0444",
		"etc/copy type=file size=5 mode=0444",
		fmt.Sprintf("sha256digest=%x", small),
		"etc type=dir",
	} {
		if !strings.Contains(fromImage.String(), want) {
			t.Errorf("mtree lacks %q:\n%s", want, fromImage.String())
		}
	}
}
