package pkgfile

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// RPM on-disk constants (see rpm's lib/rpmlead.h and lib/header.c).
const (
	rpmLeadSize   = 96
	rpmHeaderSize = 16
	// rpmTagPayloadCompressor (1125) names the compression of the payload, e.g.
	// "gzip", "xz" or "zstd".
	rpmTagPayloadCompressor = 1125
	// rpmTagPayloadFormat (1124) names the archive format; only "cpio" exists
	// in practice.
	rpmTagPayloadFormat = 1124
	// rpmTypeString is the header entry type for a NUL-terminated string.
	rpmTypeString = 6
)

var (
	rpmLeadMagic   = []byte{0xed, 0xab, 0xee, 0xdb}
	rpmHeaderMagic = []byte{0x8e, 0xad, 0xe8}
)

// ExtractRPM returns the files of an .rpm package's payload that match.
//
// The layout read here is: a 96-byte lead, the signature header (padded to an
// 8-byte boundary), the main header, and then the compressed cpio payload. Only
// the payload compressor is read out of the header; nothing else about the
// package is interpreted.
func ExtractRPM(rpmPath string, match Matcher) ([]Entry, error) {
	data, err := os.ReadFile(rpmPath)
	if err != nil {
		return nil, fmt.Errorf("reading rpm: %w", err)
	}
	if len(data) < rpmLeadSize || !bytes.HasPrefix(data, rpmLeadMagic) {
		return nil, fmt.Errorf("%s: not an RPM package (bad lead magic)", rpmPath)
	}

	offset := rpmLeadSize
	// The signature header is padded so the header that follows starts on an
	// 8-byte boundary; the main header is not padded.
	signatureTags, signatureEnd, err := readRPMHeader(data, offset)
	if err != nil {
		return nil, fmt.Errorf("%s: reading signature header: %w", rpmPath, err)
	}
	_ = signatureTags
	offset = (signatureEnd + 7) &^ 7

	headerTags, headerEnd, err := readRPMHeader(data, offset)
	if err != nil {
		return nil, fmt.Errorf("%s: reading header: %w", rpmPath, err)
	}

	if format, ok := headerTags[rpmTagPayloadFormat]; ok && format != "cpio" && format != "" {
		return nil, fmt.Errorf("%s: payload format %q is not cpio", rpmPath, format)
	}

	payload := data[headerEnd:]
	switch headerTags[rpmTagPayloadCompressor] {
	case "xz", "lzma":
		return nil, &ErrUnsupportedCompression{Package: rpmPath, Compression: "xz"}
	case "bzip2":
		return nil, &ErrUnsupportedCompression{Package: rpmPath, Compression: "bzip2"}
	}

	reader, err := decompress(bytes.NewReader(payload), payload)
	if err != nil {
		// The compressor tag is advisory; fall back to naming what the magic
		// bytes actually say.
		return nil, &ErrUnsupportedCompression{Package: rpmPath, Compression: strings.TrimSuffix(strings.TrimPrefix(err.Error(), "payload is "), "-compressed")}
	}

	entries, err := extractCPIO(reader, match)
	if err != nil {
		return nil, fmt.Errorf("%s: reading payload: %w", rpmPath, err)
	}
	return entries, nil
}

// readRPMHeader parses one header structure, returning its string-valued tags
// and the offset just past the header.
func readRPMHeader(data []byte, offset int) (map[int32]string, int, error) {
	if offset+rpmHeaderSize > len(data) {
		return nil, 0, fmt.Errorf("truncated header at offset %d", offset)
	}
	header := data[offset : offset+rpmHeaderSize]
	if !bytes.HasPrefix(header, rpmHeaderMagic) {
		return nil, 0, fmt.Errorf("bad header magic at offset %d", offset)
	}

	indexCount := int(binary.BigEndian.Uint32(header[8:12]))
	storeSize := int(binary.BigEndian.Uint32(header[12:16]))
	indexStart := offset + rpmHeaderSize
	storeStart := indexStart + indexCount*16
	headerEnd := storeStart + storeSize
	if headerEnd > len(data) || storeStart > len(data) {
		return nil, 0, fmt.Errorf("header at offset %d claims more bytes than the file holds", offset)
	}

	store := data[storeStart:headerEnd]
	tags := make(map[int32]string)
	for i := 0; i < indexCount; i++ {
		entry := data[indexStart+i*16 : indexStart+(i+1)*16]
		tag := int32(binary.BigEndian.Uint32(entry[0:4]))
		entryType := binary.BigEndian.Uint32(entry[4:8])
		entryOffset := int(binary.BigEndian.Uint32(entry[8:12]))

		// Only the handful of string tags this package cares about are decoded;
		// every other type is skipped rather than parsed.
		if entryType != rpmTypeString || entryOffset >= len(store) {
			continue
		}
		if tag != rpmTagPayloadCompressor && tag != rpmTagPayloadFormat {
			continue
		}
		value := store[entryOffset:]
		if end := bytes.IndexByte(value, 0); end >= 0 {
			value = value[:end]
		}
		tags[tag] = string(value)
	}

	return tags, headerEnd, nil
}

// cpio "new ASCII" (newc) format constants, as used by every modern RPM.
const (
	cpioNewcMagic    = "070701"
	cpioNewcCRCMagic = "070702"
	cpioHeaderSize   = 110
	cpioTrailer      = "TRAILER!!!"
	cpioModeFileType = 0o170000
	cpioModeRegular  = 0o100000
)

// extractCPIO returns the regular files of a newc cpio stream that match.
func extractCPIO(r io.Reader, match Matcher) ([]Entry, error) {
	// The stream is read whole: RPM payloads holding certificates are small,
	// and random access keeps the padding arithmetic simple.
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	var entries []Entry
	offset := 0
	for offset+cpioHeaderSize <= len(data) {
		header := string(data[offset : offset+cpioHeaderSize])
		magic := header[0:6]
		if magic != cpioNewcMagic && magic != cpioNewcCRCMagic {
			return nil, fmt.Errorf("bad cpio magic %q at offset %d", magic, offset)
		}

		mode, err := parseCPIOHex(header[14:22])
		if err != nil {
			return nil, fmt.Errorf("cpio entry at offset %d: bad mode: %w", offset, err)
		}
		fileSize, err := parseCPIOHex(header[54:62])
		if err != nil {
			return nil, fmt.Errorf("cpio entry at offset %d: bad size: %w", offset, err)
		}
		nameSize, err := parseCPIOHex(header[94:102])
		if err != nil {
			return nil, fmt.Errorf("cpio entry at offset %d: bad name size: %w", offset, err)
		}

		nameStart := offset + cpioHeaderSize
		nameEnd := nameStart + int(nameSize)
		if nameEnd > len(data) {
			return nil, fmt.Errorf("cpio entry at offset %d claims a name past the end of the payload", offset)
		}
		name := strings.TrimSuffix(string(data[nameStart:nameEnd]), "\x00")
		if name == cpioTrailer {
			break
		}

		// The name and the file content are each padded to a 4-byte boundary,
		// counted from the start of the header.
		contentStart := align4(nameEnd-offset) + offset
		contentEnd := contentStart + int(fileSize)
		if contentEnd > len(data) {
			return nil, fmt.Errorf("cpio entry %q claims %d bytes past the end of the payload", name, fileSize)
		}

		if mode&cpioModeFileType == cpioModeRegular {
			normalized := normalizeMemberPath(name)
			if match(normalized) {
				content := make([]byte, fileSize)
				copy(content, data[contentStart:contentEnd])
				entries = append(entries, Entry{Path: normalized, Content: content})
			}
		}

		offset = offset + align4(contentEnd-offset)
	}

	return entries, nil
}

// parseCPIOHex reads one of the fixed-width, zero-padded hexadecimal fields of
// a newc header.
func parseCPIOHex(field string) (uint64, error) {
	return strconv.ParseUint(field, 16, 64)
}

// align4 rounds up to the next multiple of four.
func align4(n int) int { return (n + 3) &^ 3 }
