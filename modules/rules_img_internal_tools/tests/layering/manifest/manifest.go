// Package manifest reads and writes the exhaustive ndjson manifests used by the
// layering test suite, and verifies layer tar files against them.
//
// A manifest is newline-delimited JSON (ndjson) where every line is one tar
// entry: all of the tar header fields the Go archive/tar reader exposes, plus
// the sha256 of the content for regular files.
package manifest

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Entry is the exhaustive description of a single tar entry. Field order and
// json tags define the on-disk manifest schema.
type Entry struct {
	Name       string            `json:"name"`
	Typeflag   string            `json:"typeflag"`
	Linkname   string            `json:"linkname,omitempty"`
	Size       int64             `json:"size"`
	Mode       int64             `json:"mode"`
	UID        int               `json:"uid"`
	GID        int               `json:"gid"`
	Uname      string            `json:"uname,omitempty"`
	Gname      string            `json:"gname,omitempty"`
	ModTime    string            `json:"modtime,omitempty"`
	AccessTime string            `json:"accesstime,omitempty"`
	ChangeTime string            `json:"changetime,omitempty"`
	Devmajor   int64             `json:"devmajor,omitempty"`
	Devminor   int64             `json:"devminor,omitempty"`
	PAXRecords map[string]string `json:"pax_records,omitempty"`
	SHA256     string            `json:"sha256,omitempty"`
}

// Dump writes the ndjson manifest of the layer tar at blobPath to w.
func Dump(blobPath string, w io.Writer) error {
	tr, err := openLayer(blobPath)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		e, err := entryFromHeader(hdr, tr)
		if err != nil {
			return err
		}
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// Read parses an ndjson manifest file into a list of entries.
func Read(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []Entry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		var e Entry
		dec := json.NewDecoder(bytes.NewReader(text))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&e); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		entries = append(entries, e)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// VerifyLayer reads the tar entries of the layer at blobPath and the manifest
// lines of manifestPath and checks that they describe exactly the same entries,
// in the same order.
func VerifyLayer(blobPath, manifestPath string) error {
	expected, err := Read(manifestPath)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}

	tr, err := openLayer(blobPath)
	if err != nil {
		return err
	}

	idx := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar entry %d: %w", idx, err)
		}
		actual, err := entryFromHeader(hdr, tr)
		if err != nil {
			return fmt.Errorf("entry %d (%s): %w", idx, hdr.Name, err)
		}
		if idx >= len(expected) {
			return fmt.Errorf("tar has more entries than manifest (%d); unexpected entry %d: %s", len(expected), idx, mustJSON(actual))
		}
		want := mustJSON(expected[idx])
		got := mustJSON(actual)
		if want != got {
			return fmt.Errorf("entry %d mismatch:\n  manifest: %s\n  tar:      %s", idx, want, got)
		}
		idx++
	}
	if idx < len(expected) {
		return fmt.Errorf("manifest has %d entries but tar has only %d; first missing entry: %s", len(expected), idx, mustJSON(expected[idx]))
	}
	return nil
}

// entryFromHeader converts a tar header into an Entry, computing the content
// sha256 for regular files (which also advances the reader past the content).
func entryFromHeader(hdr *tar.Header, r io.Reader) (Entry, error) {
	e := Entry{
		Name:       hdr.Name,
		Typeflag:   string(rune(hdr.Typeflag)),
		Linkname:   hdr.Linkname,
		Size:       hdr.Size,
		Mode:       hdr.Mode,
		UID:        hdr.Uid,
		GID:        hdr.Gid,
		Uname:      hdr.Uname,
		Gname:      hdr.Gname,
		ModTime:    formatTime(hdr.ModTime),
		AccessTime: formatTime(hdr.AccessTime),
		ChangeTime: formatTime(hdr.ChangeTime),
		Devmajor:   hdr.Devmajor,
		Devminor:   hdr.Devminor,
		PAXRecords: hdr.PAXRecords,
	}
	if hdr.Typeflag == tar.TypeReg {
		h := sha256.New()
		if _, err := io.Copy(h, r); err != nil {
			return Entry{}, fmt.Errorf("hashing content: %w", err)
		}
		e.SHA256 = hex.EncodeToString(h.Sum(nil))
	}
	return e, nil
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// openLayer reads the layer blob at path and returns a tar reader over its
// uncompressed contents. The compression format is detected by trying, in
// order, gzip, then zstd, then an uncompressed tar stream.
func openLayer(path string) (*tar.Reader, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading layer %s: %w", path, err)
	}

	// 1. gzip — gzip.NewReader validates the magic header eagerly.
	if gz, err := gzip.NewReader(bytes.NewReader(data)); err == nil {
		return tar.NewReader(gz), nil
	}

	// 2. zstd — DecodeAll fails cleanly on a non-zstd stream.
	if dec, err := zstd.NewReader(nil); err == nil {
		defer dec.Close()
		if decoded, err := dec.DecodeAll(data, nil); err == nil {
			return tar.NewReader(bytes.NewReader(decoded)), nil
		}
	}

	// 3. uncompressed tar.
	return tar.NewReader(bytes.NewReader(data)), nil
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<json error: %v>", err)
	}
	return string(b)
}
