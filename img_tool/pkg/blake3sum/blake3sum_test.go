package blake3sum

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Test vectors from the BLAKE3 reference implementation (256-bit output).
const (
	emptyDigest = "af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262"
	abcDigest   = "6437b3ac38465133ffb63b75273a8db548c558465d79db03fd359c6cd5bd9d85"
)

func TestSum(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty", input: "", want: emptyDigest},
		{name: "abc", input: "abc", want: abcDigest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			digest, size, err := Sum(strings.NewReader(tc.input))
			if err != nil {
				t.Fatalf("Sum: %v", err)
			}
			if got := hex.EncodeToString(digest); got != tc.want {
				t.Errorf("digest = %s, want %s", got, tc.want)
			}
			if size != int64(len(tc.input)) {
				t.Errorf("size = %d, want %d", size, len(tc.input))
			}
		})
	}
}

func TestFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	digest, size, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if got := hex.EncodeToString(digest); got != abcDigest || size != 3 {
		t.Errorf("File = %s, %d; want %s, 3", got, size, abcDigest)
	}
	if _, _, err := File(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("File of a missing path succeeded")
	}
}

// A hasher that is fed in pieces must agree with one that sees the whole input,
// which is how the cache verifies a blob it streams.
func TestNewIsIncremental(t *testing.T) {
	h := New()
	h.Write([]byte("a"))
	h.Write([]byte("bc"))
	if got := hex.EncodeToString(h.Sum(nil)); got != abcDigest {
		t.Errorf("incremental digest = %s, want %s", got, abcDigest)
	}
}
