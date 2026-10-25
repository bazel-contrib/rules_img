package extentstreamcmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/extentstream"
)

func TestReconstructKeepsHolesSparse(t *testing.T) {
	dir := t.TempDir()
	blob := []byte("blob content")
	sum := sha256.Sum256(blob)
	cas := filepath.Join(dir, "cas")
	blobPath := filepath.Join(cas, "sha256", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobPath, blob, 0o644); err != nil {
		t.Fatal(err)
	}

	var stream bytes.Buffer
	w := extentstream.NewWriter(&stream)
	w.AddData([]byte("head"))
	w.AddRef(extentstream.KindCAS, sum[:], int64(len(blob)), 0, int64(len(blob)))
	w.AddHole(64 << 20)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	streamPath := filepath.Join(dir, "x.xstream")
	if err := os.WriteFile(streamPath, stream.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "out")
	if err := reconstruct(context.Background(), streamPath, out, []string{cas}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(4 + len(blob) + 64<<20); st.Size() != want {
		t.Fatalf("the output has %d bytes, want %d", st.Size(), want)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got, append([]byte("head"), blob...)) || !bytes.Equal(got[4+len(blob):], make([]byte, 64<<20)) {
		t.Error("the content is wrong")
	}

	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	if err := list(streamPath, devnull); err != nil {
		t.Fatal(err)
	}
}
