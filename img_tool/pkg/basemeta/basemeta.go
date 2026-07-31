// Package basemeta reads and writes streams of base image content metadata.
//
// A stream describes tar entries without materializing a tar: it is a
// length-delimited sequence of baselayer.BaseEntry protobuf messages, zstd
// compressed as a whole. Rules that describe base image content (users, CA
// certificates, directory skeletons, ...) each emit one stream; the terminal
// layer rule reads all of them, merges the entries with [Merge], and writes a
// single flat layer.
//
// The format is internal to rules_img and carries no version: producer and
// consumer always ship in the same binary.
package basemeta

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
	"google.golang.org/protobuf/encoding/protodelim"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/proto/baselayer"
)

// Writer serializes BaseEntry messages into a compressed, length-delimited
// stream. Callers must call Close to flush the compressor.
type Writer struct {
	zw  *zstd.Encoder
	err error
}

// NewWriter returns a Writer emitting into w.
func NewWriter(w io.Writer) (*Writer, error) {
	// Base metadata streams are tiny (kilobytes) and read on every layer build,
	// so favour decode speed and a small dependency footprint over ratio.
	zw, err := zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, fmt.Errorf("creating zstd encoder: %w", err)
	}
	return &Writer{zw: zw}, nil
}

// Create opens path for writing and returns a Writer for it. The returned
// closer closes both the Writer and the file.
func Create(path string) (*Writer, io.Closer, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("opening base metadata output: %w", err)
	}
	w, err := NewWriter(f)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return w, closerFunc(func() error {
		if err := w.Close(); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}), nil
}

// Write appends one entry to the stream.
func (w *Writer) Write(entry *baselayer.BaseEntry) error {
	if w.err != nil {
		return w.err
	}
	if _, err := protodelim.MarshalTo(w.zw, entry); err != nil {
		w.err = fmt.Errorf("writing base metadata entry %q: %w", entry.GetPath(), err)
		return w.err
	}
	return nil
}

// WriteAll appends every entry to the stream, stopping at the first error.
func (w *Writer) WriteAll(entries []*baselayer.BaseEntry) error {
	for _, entry := range entries {
		if err := w.Write(entry); err != nil {
			return err
		}
	}
	return nil
}

// Close flushes the compressor. It does not close the underlying writer.
func (w *Writer) Close() error {
	if w.err != nil {
		w.zw.Close()
		return w.err
	}
	return w.zw.Close()
}

// Read decodes every entry of a compressed stream, in order.
func Read(r io.Reader) ([]*baselayer.BaseEntry, error) {
	zr, err := zstd.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("creating zstd reader: %w", err)
	}
	defer zr.Close()

	var entries []*baselayer.BaseEntry
	unmarshalOpts := protodelim.UnmarshalOptions{}
	// protodelim.Reader is an io.Reader that is also an io.ByteReader, so it can
	// read a message's varint length prefix without overshooting into the next
	// message. bufio.Reader provides both.
	reader := bufio.NewReader(zr.IOReadCloser())
	for {
		entry := &baselayer.BaseEntry{}
		if err := unmarshalOpts.UnmarshalFrom(reader, entry); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("reading base metadata entry: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// ReadFile decodes every entry of the compressed stream stored at path.
func ReadFile(path string) ([]*baselayer.BaseEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening base metadata stream: %w", err)
	}
	defer f.Close()
	entries, err := Read(f)
	if err != nil {
		return nil, fmt.Errorf("reading base metadata stream %s: %w", path, err)
	}
	return entries, nil
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }
