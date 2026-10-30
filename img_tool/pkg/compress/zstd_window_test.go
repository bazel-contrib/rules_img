package compress

import (
	"bytes"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// TestZstdWindowLog checks that ZstdWindowLog sets the window advertised in the
// zstd frame header, which is what lets matches reach that far back, and that
// the result still decodes.
func TestZstdWindowLog(t *testing.T) {
	data := bytes.Repeat([]byte("rules_img zstd window "), 1<<16)
	for _, tc := range []struct {
		name string
		opts []Option
		want uint64
	}{
		{name: "level default window", opts: []Option{CompressionLevel(4)}, want: 8 << 20},
		{name: "window log 27", opts: []Option{CompressionLevel(4), ZstdWindowLog(27)}, want: 1 << 27},
		{name: "window log 25 without level", opts: []Option{ZstdWindowLog(25)}, want: 1 << 25},
	} {
		var compressed bytes.Buffer
		appender, err := AppenderFactory("sha256", "zstd", &compressed, tc.opts...)
		if err != nil {
			t.Fatalf("%s: creating appender: %v", tc.name, err)
		}
		if _, err := appender.Write(data); err != nil {
			t.Fatalf("%s: writing data: %v", tc.name, err)
		}
		if _, err := appender.Finalize(); err != nil {
			t.Fatalf("%s: finalizing: %v", tc.name, err)
		}

		var header zstd.Header
		if err := header.Decode(compressed.Bytes()); err != nil {
			t.Fatalf("%s: decoding frame header: %v", tc.name, err)
		}
		if header.WindowSize != tc.want {
			t.Errorf("%s: frame window size = %d, want %d", tc.name, header.WindowSize, tc.want)
		}

		decoder, err := zstd.NewReader(bytes.NewReader(compressed.Bytes()))
		if err != nil {
			t.Fatalf("%s: creating decoder: %v", tc.name, err)
		}
		decompressed, err := io.ReadAll(decoder)
		decoder.Close()
		if err != nil {
			t.Fatalf("%s: decompressing: %v", tc.name, err)
		}
		if !bytes.Equal(decompressed, data) {
			t.Fatalf("%s: decompressed output does not match the input", tc.name)
		}
	}
}
