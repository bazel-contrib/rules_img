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
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/iso9660"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/iso9660/isocstream"
)

func buildIso9660Layer(t *testing.T, opts iso9660.Options) (image, cstream []byte, big string) {
	t.Helper()
	opts.DigestFiles, opts.DigestImage = true, true
	b, err := iso9660.NewBuilder(opts)
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
	if opts.RockRidge {
		add(&tar.Header{Typeflag: tar.TypeSymlink, Name: "etc/link", Mode: 0o777, Linkname: "big"}, "")
		add(&tar.Header{Typeflag: tar.TypeChar, Name: "dev/null", Mode: 0o666, Devmajor: 1, Devminor: 3}, "")
	}

	f := &memImage{}
	var stream bytes.Buffer
	if _, err := isocstream.Build(b, f, &stream, isocstream.Options{}); err != nil {
		t.Fatal(err)
	}
	return f.b, stream.Bytes(), big
}

// TestIso9660Mtree checks that the mtree of an image and the mtree of its
// compact stream (read as a sparse image whose payloads are absent) are the
// same, and that the Rock Ridge metadata reaches the spec.
func TestIso9660Mtree(t *testing.T) {
	image, cstream, big := buildIso9660Layer(t, iso9660.Options{RockRidge: true})

	var fromImage bytes.Buffer
	if err := WriteMulti(&fromImage, DefaultOptions(), []Input{{Kind: Iso9660Input, ReaderAt: bytes.NewReader(image), Digester: HashContent}}); err != nil {
		t.Fatal(err)
	}
	sparse, err := compactstream.NewSparseReaderAt(bytes.NewReader(cstream))
	if err != nil {
		t.Fatal(err)
	}
	var fromStream bytes.Buffer
	if err := WriteMulti(&fromStream, DefaultOptions(), []Input{{Kind: Iso9660Input, ReaderAt: sparse, Digester: HashContent, RangeDigester: sparse.RefDigestInRange}}); err != nil {
		t.Fatal(err)
	}
	if fromImage.String() != fromStream.String() {
		t.Errorf("the mtree of the compact stream differs from the one of the image.\n--- image ---\n%s\n--- cstream ---\n%s", fromImage.String(), fromStream.String())
	}

	sum := sha256.Sum256([]byte(big))
	small := sha256.Sum256([]byte("tiny\n"))
	for _, want := range []string{
		fmt.Sprintf("etc/big type=file size=%d mode=0644", len(big)),
		fmt.Sprintf("sha256digest=%x", sum),
		"etc/small type=file size=5 mode=0444",
		"etc/copy type=file size=5 mode=0444",
		fmt.Sprintf("sha256digest=%x", small),
		"etc type=dir mode=0755",
		"etc/link type=link size=3 mode=0777",
		"link=big",
		"dev/null type=char size=0 mode=0666",
		// The two names of the shared extent are reported as hard links.
		"nlink=2",
	} {
		if !strings.Contains(fromImage.String(), want) {
			t.Errorf("the mtree lacks %q:\n%s", want, fromImage.String())
		}
	}
}

// TestIso9660MtreeWithoutRockRidge checks the plain image: the mangled
// identifiers and the read-only modes ISO 9660 implies.
func TestIso9660MtreeWithoutRockRidge(t *testing.T) {
	image, _, big := buildIso9660Layer(t, iso9660.Options{})

	var spec bytes.Buffer
	if err := WriteMulti(&spec, DefaultOptions(), []Input{{Kind: Iso9660Input, ReaderAt: bytes.NewReader(image), Digester: HashContent}}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		fmt.Sprintf("ETC/BIG type=file size=%d mode=0444", len(big)),
		"ETC type=dir mode=0555",
	} {
		if !strings.Contains(spec.String(), want) {
			t.Errorf("the mtree lacks %q:\n%s", want, spec.String())
		}
	}
}
