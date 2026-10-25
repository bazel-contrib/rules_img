package compactstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirStore(t *testing.T) {
	write := func(dir string, content string) []byte {
		sum := sha256.Sum256([]byte(content))
		p := filepath.Join(dir, "sha256", hex.EncodeToString(sum[:]))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return sum[:]
	}
	a, b := t.TempDir(), t.TempDir()
	inA, inB := write(a, "in a"), write(b, "in b")
	both := write(a, "in both")
	write(b, "in both")
	missing := sha256.Sum256([]byte("nowhere"))

	store := NewDirStore(a, b)
	for digest, want := range map[string]string{string(inA): "in a", string(inB): "in b", string(both): "in both"} {
		r, err := store.ReaderForBlob(context.Background(), []byte(digest), 0)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(r)
		r.Close()
		if string(got) != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	_, err := store.ReaderForBlob(context.Background(), missing[:], 7)
	if err == nil || !strings.Contains(err.Error(), hex.EncodeToString(missing[:])) {
		t.Errorf("missing blob: %v", err)
	}
	if _, err := NewDirStore().ReaderForBlob(context.Background(), missing[:], 7); err == nil {
		t.Error("an empty store found a blob")
	}
}
