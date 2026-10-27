package mtree

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/ext4"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/ext4/ext4cstream"
)

func buildExt4Layer(t *testing.T) (image *os.File, cstream []byte, big string) {
	t.Helper()
	b, err := ext4.NewBuilder(ext4.Options{DigestFiles: true, DigestImage: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	big = strings.Repeat("shared payload\n", 500)
	mod := time.Unix(1_700_000_000, 0)
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
	add(&tar.Header{Typeflag: tar.TypeReg, Name: "etc/big", Mode: 0o644, Uid: 5, Gid: 6,
		PAXRecords: map[string]string{"SCHILY.xattr.user.k": "v"}}, big)
	add(&tar.Header{Typeflag: tar.TypeReg, Name: "etc/small", Mode: 0o600}, "tiny\n")
	add(&tar.Header{Typeflag: tar.TypeLink, Name: "etc/hard", Linkname: "etc/small"}, "")
	add(&tar.Header{Typeflag: tar.TypeSymlink, Name: "etc/link", Linkname: "big"}, "")
	add(&tar.Header{Typeflag: tar.TypeChar, Name: "dev/null", Mode: 0o666, Devmajor: 1, Devminor: 3}, "")

	f, err := os.Create(filepath.Join(t.TempDir(), "image"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	var stream bytes.Buffer
	if _, err := ext4cstream.Build(b, f, &stream, ext4cstream.Options{}); err != nil {
		t.Fatal(err)
	}
	return f, stream.Bytes(), big
}

func TestExt4Mtree(t *testing.T) {
	image, cstream, big := buildExt4Layer(t)

	var fromImage bytes.Buffer
	if err := WriteMulti(&fromImage, DefaultOptions(), []Input{{Kind: Ext4Input, ReaderAt: image, Digester: HashContent}}); err != nil {
		t.Fatal(err)
	}
	sparse, err := compactstream.NewSparseReaderAt(bytes.NewReader(cstream))
	if err != nil {
		t.Fatal(err)
	}
	var fromStream bytes.Buffer
	if err := WriteMulti(&fromStream, DefaultOptions(), []Input{{Kind: Ext4Input, ReaderAt: sparse, Digester: HashContent, RangeDigester: sparse.RefDigestInRange}}); err != nil {
		t.Fatal(err)
	}
	if fromImage.String() != fromStream.String() {
		t.Errorf("mtree from the compact stream differs from the one from the image.\n--- image ---\n%s\n--- cstream ---\n%s", fromImage.String(), fromStream.String())
	}

	sum := sha256.Sum256([]byte(big))
	for _, want := range []string{
		fmt.Sprintf("etc/big type=file size=%d mode=0644 uid=5 gid=6 sha256digest=%x", len(big), sum),
		"etc/small type=file size=5 mode=0600",
		"nlink=2", // small and its hard link
		"etc/link type=link",
		"link=big",
		"dev/null type=char",
		"lost+found type=dir mode=0700",
	} {
		if !strings.Contains(fromImage.String(), want) {
			t.Errorf("mtree lacks %q:\n%s", want, fromImage.String())
		}
	}
}
