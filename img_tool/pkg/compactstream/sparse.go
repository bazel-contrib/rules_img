package compactstream

import (
	"fmt"
	"io"
	"sort"
)

// SparseReaderAt is a random-access view of the reconstructed (uncompressed)
// stream in which the ranges stored as content-addressed references read as
// zeros, and everything else reads its real bytes.
//
// It exists for readers that need to interpret a layer's structure rather than
// rebuild its bytes, and can do so without the content store. That works for a
// format whose metadata is carried inline -- an EROFS layer keeps its
// superblock, inodes, xattrs and directory blocks in the stream and only its
// file payloads in the CAS -- so the whole directory tree is readable while the
// payloads are absent. It is the random-access counterpart of pairing
// [ReconstructUncompressed] with [NullBlobStore].
//
// Only the byte-stream section is held in memory (the omitted payloads cost
// nothing), so the footprint is the compact stream's own size, not the layer's.
type SparseReaderAt struct {
	stream   []byte
	segments []segment
	size     int64
	refs     map[int64]CASReference
	header   Header
}

// segment is one contiguous run of the reconstructed stream. streamOffset is
// where its bytes live in the byte-stream buffer, or -1 when the run is a
// CAS-referenced range and therefore reads as zeros.
type segment struct {
	offset       int64
	size         int64
	streamOffset int64
}

// NewSparseReaderAt reads a whole compact stream and returns a random-access
// view of the stream it describes.
func NewSparseReaderAt(index io.Reader) (*SparseReaderAt, error) {
	header, err := ReadHeader(index)
	if err != nil {
		return nil, err
	}
	refs, err := readRefTable(index, header)
	if err != nil {
		return nil, err
	}
	stream, err := streamReader(index, header.StreamCompression)
	if err != nil {
		return nil, err
	}
	if closer, ok := stream.(io.Closer); ok {
		defer closer.Close()
	}
	buf, err := io.ReadAll(stream)
	if err != nil {
		return nil, fmt.Errorf("decompressing byte stream: %w", err)
	}

	s := &SparseReaderAt{
		stream: buf,
		refs:   make(map[int64]CASReference, len(refs)),
		header: header,
	}
	var outPos, streamPos int64
	for _, ref := range refs {
		if gap := int64(ref.Offset) - outPos; gap > 0 {
			s.segments = append(s.segments, segment{offset: outPos, size: gap, streamOffset: streamPos})
			streamPos += gap
			outPos += gap
		}
		s.segments = append(s.segments, segment{offset: outPos, size: int64(ref.Size), streamOffset: -1})
		s.refs[outPos] = ref
		outPos += int64(ref.Size)
	}
	if rest := int64(len(buf)) - streamPos; rest > 0 {
		s.segments = append(s.segments, segment{offset: outPos, size: rest, streamOffset: streamPos})
		outPos += rest
	} else if rest < 0 {
		return nil, fmt.Errorf("compact stream: byte stream is %d bytes short of what the reference table implies", -rest)
	}
	s.size = outPos
	return s, nil
}

// Size is the length of the reconstructed stream.
func (s *SparseReaderAt) Size() int64 { return s.size }

// Header is the compact stream's parsed header, which records among other things
// what the reconstructed stream is (see [Header.ContentFormat]).
func (s *SparseReaderAt) Header() Header { return s.header }

// RefDigestAt reports the digest recorded for the range starting at offset and
// spanning size bytes, if that range is a CAS reference. For a range that is
// carried inline it returns (nil, false), and the caller can read and hash the
// real bytes instead.
func (s *SparseReaderAt) RefDigestAt(offset, size int64) ([]byte, bool) {
	ref, ok := s.refs[offset]
	if !ok || int64(ref.Size) != size {
		return nil, false
	}
	return ref.Digest, true
}

func (s *SparseReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("compact stream: negative offset %d", off)
	}
	if off >= s.size {
		return 0, io.EOF
	}
	var read int
	for read < len(p) {
		pos := off + int64(read)
		if pos >= s.size {
			return read, io.EOF
		}
		i := sort.Search(len(s.segments), func(i int) bool {
			return s.segments[i].offset+s.segments[i].size > pos
		})
		seg := s.segments[i]
		n := int(min(seg.offset+seg.size-pos, int64(len(p)-read)))
		if seg.streamOffset < 0 {
			clear(p[read : read+n])
		} else {
			copy(p[read:read+n], s.stream[seg.streamOffset+pos-seg.offset:])
		}
		read += n
	}
	return read, nil
}
