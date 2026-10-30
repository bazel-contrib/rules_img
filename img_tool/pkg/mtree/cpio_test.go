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
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/cpio"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/cpio/cpiocstream"
)

func buildCpioLayer(t *testing.T) (archive, cstream []byte, big string) {
	t.Helper()
	b, err := cpio.NewBuilder(cpio.Options{DigestFiles: true, DigestArchive: true})
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
	add(&tar.Header{Typeflag: tar.TypeReg, Name: "etc/big", Mode: 0o644, Uid: 5, Gid: 6}, big)
	add(&tar.Header{Typeflag: tar.TypeReg, Name: "etc/small", Mode: 0o600}, "tiny\n")
	add(&tar.Header{Typeflag: tar.TypeLink, Name: "etc/hard", Linkname: "etc/small"}, "")
	add(&tar.Header{Typeflag: tar.TypeSymlink, Name: "etc/link", Linkname: "big"}, "")
	add(&tar.Header{Typeflag: tar.TypeChar, Name: "dev/null", Mode: 0o666, Devmajor: 1, Devminor: 3}, "")

	var buf bytes.Buffer
	info, err := b.Build(&buf)
	if err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	if err := cpiocstream.Write(info, bytes.NewReader(buf.Bytes()), &stream, cpiocstream.Options{}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), stream.Bytes(), big
}

func TestCpioMtree(t *testing.T) {
	archive, cstream, big := buildCpioLayer(t)

	var fromArchive bytes.Buffer
	if err := WriteMulti(&fromArchive, DefaultOptions(), []Input{{Kind: CpioInput, ReaderAt: bytes.NewReader(archive), Digester: HashContent}}); err != nil {
		t.Fatal(err)
	}
	sparse, err := compactstream.NewSparseReaderAt(bytes.NewReader(cstream))
	if err != nil {
		t.Fatal(err)
	}
	var fromStream bytes.Buffer
	if err := WriteMulti(&fromStream, DefaultOptions(), []Input{{Kind: CpioInput, ReaderAt: sparse, Digester: HashContent, RangeDigester: sparse.RefDigestInRange}}); err != nil {
		t.Fatal(err)
	}
	if fromArchive.String() != fromStream.String() {
		t.Errorf("mtree from the compact stream differs from the one from the archive.\n--- archive ---\n%s\n--- cstream ---\n%s", fromArchive.String(), fromStream.String())
	}

	sum := sha256.Sum256([]byte(big))
	small := sha256.Sum256([]byte("tiny\n"))
	for _, want := range []string{
		fmt.Sprintf("etc/big type=file size=%d mode=0644 uid=5 gid=6 sha256digest=%x", len(big), sum),
		"etc/small type=file size=5 mode=0600",
		"etc/hard type=file size=5 mode=0600",
		fmt.Sprintf("sha256digest=%x", small), // also for the hard link that carries no content
		"nlink=2",                             // small and its hard link
		"etc/link type=link",
		"link=big",
		"dev/null type=char",
	} {
		if !strings.Contains(fromArchive.String(), want) {
			t.Errorf("mtree lacks %q:\n%s", want, fromArchive.String())
		}
	}
	if n := strings.Count(fromArchive.String(), fmt.Sprintf("sha256digest=%x", small)); n != 2 {
		t.Errorf("%d entries have the digest of the hard linked file, want 2:\n%s", n, fromArchive.String())
	}
}
