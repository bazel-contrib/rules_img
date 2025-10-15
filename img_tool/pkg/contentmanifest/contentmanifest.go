package contentmanifest

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

type fileManifest struct {
	algorithm    api.HashAlgorithm
	manifestPath string
	fs           vfs
}

func New(manifestPath string, algorithm api.HashAlgorithm) *fileManifest {
	return &fileManifest{
		manifestPath: manifestPath,
		algorithm:    algorithm,
		fs:           osFS{},
	}
}

func (f *fileManifest) BlobHashes() iter.Seq2[[]byte, error] {
	// open the file for reading
	r, err := f.fs.OpenFile(f.manifestPath, os.O_RDONLY, 0)
	if err != nil {
		return func(yield func([]byte, error) bool) {
			yield(nil, err)
		}
	}

	// read the magic and TOC
	rawHeader := make([]byte, maxHeaderSize)
	if _, err := io.ReadFull(r, rawHeader); err != nil {
		return func(yield func([]byte, error) bool) {
			yield(nil, err)
		}
	}
	header, err := parseHeader([maxHeaderSize]byte(rawHeader))
	if err != nil {
		return func(yield func([]byte, error) bool) {
			yield(nil, err)
		}
	}
	expectMagic := fmt.Sprintf("%s+%s", magicPrefix, f.algorithm)
	if header.magic != expectMagic {
		return func(yield func([]byte, error) bool) {
			yield(nil, fmt.Errorf("invalid content manifest: expected magic %s, but got %s", expectMagic, header.magic))
		}
	}
	if header.sizeBlobs == 0 {
		return func(yield func([]byte, error) bool) {
			yield(nil, nil)
		}
	}

	blobReader, ok := r.(randomAccessReader)
	if !ok {
		return func(yield func([]byte, error) bool) {
			yield(nil, errors.New("contenmanifest source file doesn't support random access"))
		}
	}
	if _, err := blobReader.Seek(header.offsetBlobs, io.SeekStart); err != nil {
		return func(yield func([]byte, error) bool) {
			yield(nil, err)
		}
	}

	return f.readHashes(newHashReader(blobReader, header.sizeBlobs))
}

func (f *fileManifest) NodeHashes() iter.Seq2[[]byte, error] {
	// open the file for reading
	r, err := f.fs.OpenFile(f.manifestPath, os.O_RDONLY, 0)
	if err != nil {
		return func(yield func([]byte, error) bool) {
			yield(nil, err)
		}
	}

	// read the magic and TOC
	rawHeader := make([]byte, maxHeaderSize)
	if _, err := io.ReadFull(r, rawHeader); err != nil {
		return func(yield func([]byte, error) bool) {
			yield(nil, err)
		}
	}
	header, err := parseHeader([maxHeaderSize]byte(rawHeader))
	if err != nil {
		return func(yield func([]byte, error) bool) {
			yield(nil, err)
		}
	}
	expectMagic := fmt.Sprintf("%s+%s", magicPrefix, f.algorithm)
	if header.magic != expectMagic {
		return func(yield func([]byte, error) bool) {
			yield(nil, fmt.Errorf("invalid content manifest: expected magic %s, but got %s", expectMagic, header.magic))
		}
	}
	if header.sizeNodes == 0 {
		return func(yield func([]byte, error) bool) {
			yield(nil, nil)
		}
	}

	nodeReader, ok := r.(randomAccessReader)
	if !ok {
		return func(yield func([]byte, error) bool) {
			yield(nil, errors.New("contenmanifest source file doesn't support random access"))
		}
	}
	if _, err := nodeReader.Seek(header.offsetNodes, io.SeekStart); err != nil {
		return func(yield func([]byte, error) bool) {
			yield(nil, err)
		}
	}
	return f.readHashes(newHashReader(nodeReader, header.sizeNodes))
}

func (f *fileManifest) TreeHashes() iter.Seq2[[]byte, error] {
	// open the file for reading
	r, err := f.fs.OpenFile(f.manifestPath, os.O_RDONLY, 0)
	if err != nil {
		return func(yield func([]byte, error) bool) {
			yield(nil, err)
		}
	}

	// read the magic and TOC
	rawHeader := make([]byte, maxHeaderSize)
	if _, err := io.ReadFull(r, rawHeader); err != nil {
		return func(yield func([]byte, error) bool) {
			yield(nil, err)
		}
	}
	header, err := parseHeader([maxHeaderSize]byte(rawHeader))
	if err != nil {
		return func(yield func([]byte, error) bool) {
			yield(nil, err)
		}
	}
	expectMagic := fmt.Sprintf("%s+%s", magicPrefix, f.algorithm)
	if header.magic != expectMagic {
		return func(yield func([]byte, error) bool) {
			yield(nil, fmt.Errorf("invalid content manifest: expected magic %s, but got %s", expectMagic, header.magic))
		}
	}
	if header.sizeTrees == 0 {
		return func(yield func([]byte, error) bool) {
			yield(nil, nil)
		}
	}

	treeReader, ok := r.(randomAccessReader)
	if !ok {
		return func(yield func([]byte, error) bool) {
			yield(nil, errors.New("contenmanifest source file doesn't support random access"))
		}
	}
	if _, err := treeReader.Seek(header.offsetTrees, io.SeekStart); err != nil {
		return func(yield func([]byte, error) bool) {
			yield(nil, err)
		}
	}
	return f.readHashes(newHashReader(treeReader, header.sizeTrees))
}

func (f *fileManifest) Export(state api.CASStateSupplier) error {
	// open the file for writing
	w, err := f.fs.OpenFile(f.manifestPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := w.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "Error closing manifest file: %v\n", closeErr)
		}
	}()

	randomAccessWriter, ok := w.(randomAccessWriter)
	if !ok {
		return errors.New("file does not support random access")
	}

	return f.exportInto(randomAccessWriter, state)
}

func (f *fileManifest) exportInto(w randomAccessWriter, state api.CASStateSupplier) error {
	// we skip the first record which would contain the magic and TOC.
	// instead, we come back later and write the magic and TOC
	// after we learned the offsets needed for the TOC.
	offsetBlobs := int64(maxHeaderSize)
	if _, err := w.Seek(offsetBlobs, io.SeekStart); err != nil {
		return err
	}
	sizeBlobs, err := f.exportHashes(w, state.BlobHashes())
	if err != nil {
		return err
	}

	offsetNodes := offsetBlobs + sizeBlobs
	// round up to the next record size
	if offsetNodes%recordSize != 0 {
		offsetNodes += recordSize - (offsetNodes % recordSize)
	}
	if _, err := w.Seek(offsetNodes, io.SeekStart); err != nil {
		return err
	}
	sizeNodes, err := f.exportHashes(w, state.NodeHashes())
	if err != nil {
		return err
	}

	offsetTrees := offsetNodes + sizeNodes
	// round up to the next record size
	if offsetTrees%recordSize != 0 {
		offsetTrees += recordSize - (offsetTrees % recordSize)
	}
	if _, err := w.Seek(offsetTrees, io.SeekStart); err != nil {
		return err
	}
	sizeTrees, err := f.exportHashes(w, state.TreeHashes())
	if err != nil {
		return err
	}

	// write the magic and TOC
	magic := fmt.Sprintf("%s+%s", magicPrefix, string(f.algorithm))
	header := make([]byte, maxHeaderSize)
	offset := copy(header, magic)
	header[offset] = byte(0)
	offset += 1

	if _, err := w.Seek(0, io.SeekStart); err != nil {
		return err
	}
	// TOC: one byte type, 8 byte offset, 8 byte hash size
	offset += copy(header[offset:], []byte{typeBlobs})
	offset += copy(header[offset:], binary.BigEndian.AppendUint64(nil, uint64(offsetBlobs)))
	offset += copy(header[offset:], binary.BigEndian.AppendUint64(nil, uint64(sizeBlobs)))
	offset += copy(header[offset:], []byte{typeNode})
	offset += copy(header[offset:], binary.BigEndian.AppendUint64(nil, uint64(offsetNodes)))
	offset += copy(header[offset:], binary.BigEndian.AppendUint64(nil, uint64(sizeNodes)))
	offset += copy(header[offset:], []byte{typeTree})
	offset += copy(header[offset:], binary.BigEndian.AppendUint64(nil, uint64(offsetTrees)))
	_ = copy(header[offset:], binary.BigEndian.AppendUint64(nil, uint64(sizeTrees)))

	_, err = w.Write(header)
	return err
}

func (f *fileManifest) exportHashes(w io.Writer, hashes iter.Seq2[[]byte, error]) (int64, error) {
	expectedSize := f.algorithm.Len()
	var written int64
	bufferedWriter := bufio.NewWriter(w)
	for hash, err := range hashes {
		if err != nil {
			return written, err
		}
		if len(hash) != expectedSize {
			return 0, errors.New("hash length mismatch during export")
		}
		n, err := bufferedWriter.Write(hash)
		if err != nil {
			return written + int64(n), err
		}
		written += int64(n)
	}

	return written, bufferedWriter.Flush()
}

func (f *fileManifest) readHashes(r io.ReadCloser) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		defer func() {
			if closeErr := r.Close(); closeErr != nil {
				fmt.Fprintf(os.Stderr, "Error closing reader: %v\n", closeErr)
			}
		}()
		hashSize := f.algorithm.Len()
		bufferedReader := bufio.NewReader(r)
		// Let's allocate a fresh byte slice for each hash.
		// While recycling would be more optimal, we don't want to
		// prevent the consumer from holding on to the slices we hand out.
		for {
			hash := make([]byte, hashSize)
			if _, err := io.ReadFull(bufferedReader, hash); err != nil {
				if err == io.EOF {
					break
				}
				yield(nil, err)
				return
			}
			if !yield(hash, nil) {
				return
			}
		}
	}
}

func parseHeader(header [maxHeaderSize]byte) (manifestHeader, error) {
	magic, toc, err := consumeMagic(header[:])
	if err != nil {
		return manifestHeader{}, err
	}
	if !strings.HasPrefix(magic, magicPrefix) {
		return manifestHeader{}, errors.New("invalid magic: " + magic)
	}

	if toc[0] != typeBlobs {
		return manifestHeader{}, errors.New("invalid TOC: expected blobs info")
	}
	offsetBlobs := int64(binary.BigEndian.Uint64(toc[1:9]))
	sizeBlobs := int64(binary.BigEndian.Uint64(toc[9:17]))
	if toc[17] != typeNode {
		return manifestHeader{}, errors.New("invalid TOC: expected nodes info")
	}
	offsetNodes := int64(binary.BigEndian.Uint64(toc[18:26]))
	sizeNodes := int64(binary.BigEndian.Uint64(toc[26:34]))
	if toc[34] != typeTree {
		return manifestHeader{}, errors.New("invalid TOC: expected trees info")
	}
	offsetTrees := int64(binary.BigEndian.Uint64(toc[35:43]))
	sizeTrees := int64(binary.BigEndian.Uint64(toc[43:51]))

	return manifestHeader{
		magic,
		offsetBlobs,
		sizeBlobs,
		offsetNodes,
		sizeNodes,
		offsetTrees,
		sizeTrees,
	}, nil
}

type manifestHeader struct {
	magic       string
	offsetBlobs int64
	sizeBlobs   int64
	offsetNodes int64
	sizeNodes   int64
	offsetTrees int64
	sizeTrees   int64
}

func consumeMagic(b []byte) (string, []byte, error) {
	// read the magic string (ends with a null byte)
	magicEnd := 0
	for i := 0; i < len(b); i++ {
		if b[i] == 0 {
			magicEnd = i
			break
		}
	}
	if magicEnd == 0 {
		return "", nil, errors.New("invalid magic")
	}
	magic := string(b[:magicEnd])
	b = b[magicEnd+1:]
	return magic, b, nil
}

type vfs interface {
	fs.FS
	OpenFile(name string, flag int, perm fs.FileMode) (fs.File, error)
}

type osFS struct{}

func (osFS) Open(name string) (fs.File, error) {
	return os.Open(name)
}

func (osFS) OpenFile(name string, flag int, perm fs.FileMode) (fs.File, error) {
	return os.OpenFile(name, flag, perm)
}

type randomAccessWriter interface {
	io.Writer
	io.Seeker
	io.WriterAt
}

type randomAccessReader interface {
	io.Reader
	io.Seeker
	io.ReaderAt
}

type hashReader struct {
	r      io.Reader
	closer io.Closer
}

func (h *hashReader) Read(p []byte) (int, error) {
	return h.r.Read(p)
}

func (h *hashReader) Close() error {
	return h.closer.Close()
}

func newHashReader(r io.Reader, hashSize int64) io.ReadCloser {
	closer := r.(io.Closer)
	return &hashReader{
		r:      io.LimitReader(r, hashSize),
		closer: closer,
	}
}

const (
	magicPrefix   = "imgv1+contentmanifest"
	typeBlobs     = byte('b')
	typeNode      = byte('n')
	typeTree      = byte('t')
	recordSize    = 0x80
	maxHeaderSize = 0x80
)
