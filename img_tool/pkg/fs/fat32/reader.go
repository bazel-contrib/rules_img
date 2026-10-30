package fat32

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"
	"unicode/utf16"
)

// Entry is a file or directory of an image, as read by [Reader].
type Entry struct {
	// Path is the slash-separated path without leading slash. The root
	// directory has the empty path.
	Path string
	// Mode holds the type and permission bits. FAT stores no permissions
	// except a read-only flag, so files are 0644 (0444 if read-only) and
	// directories 0755.
	Mode fs.FileMode
	// Mtime and Atime are the modification and access time. The access time
	// only has a date.
	Mtime, Atime time.Time
	// Size is the size of the content of a regular file.
	Size int64
	// Offset is the position of the content of a regular file in the image,
	// or zero for a file without content. The writer stores each file as a
	// contiguous run of clusters.
	Offset int64
}

// Reader reads the images written by this package: a read-only view of the
// directory tree and the location of file content. It rejects files that
// are not stored contiguously, the only layout the writer produces. The FAT
// is read, but nothing is verified.
//
// The reader works on any [io.ReaderAt], including one in which the content of
// files reads as zeros (the sparse view of a compact stream): it only reads
// the metadata, unless asked for file content with [Reader.Content].
type Reader struct {
	r           io.ReaderAt
	sectorSize  int64
	clusterSize int64
	dataStart   int64 // byte offset of cluster 2
	clusters    uint32
	fat         []byte
	rootCluster uint32
}

// OpenReader opens the image in r.
func OpenReader(r io.ReaderAt) (*Reader, error) {
	boot := make([]byte, 512)
	if err := readFull(r, boot, 0); err != nil {
		return nil, fmt.Errorf("fat32: reading the boot sector: %w", err)
	}
	if boot[510] != 0x55 || boot[511] != 0xAA || string(boot[82:90]) != "FAT32   " {
		return nil, errors.New("fat32: not a FAT32 image")
	}
	sectorSize := int64(binary.LittleEndian.Uint16(boot[11:]))
	spc := int64(boot[13])
	reserved := int64(binary.LittleEndian.Uint16(boot[14:]))
	numFATs := int64(boot[16])
	total := int64(binary.LittleEndian.Uint32(boot[32:]))
	fatSectors := int64(binary.LittleEndian.Uint32(boot[36:]))
	if sectorSize < minSectorSize || sectorSize > maxSectorSize || sectorSize&(sectorSize-1) != 0 || spc == 0 || numFATs == 0 {
		return nil, errors.New("fat32: invalid boot sector")
	}
	dataSectors := total - reserved - numFATs*fatSectors
	if dataSectors <= 0 {
		return nil, errors.New("fat32: invalid boot sector: no data region")
	}
	rd := &Reader{
		r:           r,
		sectorSize:  sectorSize,
		clusterSize: sectorSize * spc,
		dataStart:   (reserved + numFATs*fatSectors) * sectorSize,
		clusters:    uint32(min(dataSectors/spc, maxClusters)),
		rootCluster: binary.LittleEndian.Uint32(boot[44:]),
	}
	rd.fat = make([]byte, (int64(rd.clusters)+2)*4)
	if err := readFull(r, rd.fat, reserved*sectorSize); err != nil {
		return nil, fmt.Errorf("fat32: reading the FAT: %w", err)
	}
	return rd, nil
}

// readFull reads len(p) bytes at off. Bytes past the end of r read as zeros,
// like in a sparse file.
func readFull(r io.ReaderAt, p []byte, off int64) error {
	n, err := r.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	clear(p[n:])
	return nil
}

// Content returns a reader for the content of a regular file.
func (rd *Reader) Content(e *Entry) io.Reader {
	return io.NewSectionReader(rd.r, e.Offset, e.Size)
}

func (rd *Reader) clusterOffset(cluster uint32) int64 {
	return rd.dataStart + int64(cluster-2)*rd.clusterSize
}

// chain returns the clusters of the chain that starts at first, which has to
// be contiguous.
func (rd *Reader) chain(first uint32) (count uint32, err error) {
	cluster := first
	for {
		if cluster < 2 || cluster >= rd.clusters+2 {
			return 0, fmt.Errorf("cluster %d is out of range", cluster)
		}
		count++
		next := binary.LittleEndian.Uint32(rd.fat[cluster*4:]) & 0x0FFFFFFF
		if next >= 0x0FFFFFF8 {
			return count, nil
		}
		if next != cluster+1 {
			return 0, fmt.Errorf("the chain at cluster %d is fragmented", first)
		}
		if count > rd.clusters {
			return 0, errors.New("cluster chain loops")
		}
		cluster = next
	}
}

// Walk calls fn for every file and directory of the image, starting with the
// root. A directory comes before its content, and the entries of a directory
// are visited in the order of the directory.
func (rd *Reader) Walk(fn func(*Entry) error) error {
	root := &Entry{Mode: fs.ModeDir | 0o755}
	if err := fn(root); err != nil {
		return err
	}
	return rd.walkDir(rd.rootCluster, "", 0, fn)
}

func (rd *Reader) walkDir(cluster uint32, dirPath string, depth int, fn func(*Entry) error) error {
	if depth > 128 {
		return errors.New("fat32: directories are nested too deep")
	}
	count, err := rd.chain(cluster)
	if err != nil {
		return fmt.Errorf("fat32: directory %q: %w", dirPath, err)
	}
	data := make([]byte, int64(count)*rd.clusterSize)
	if err := readFull(rd.r, data, rd.clusterOffset(cluster)); err != nil {
		return err
	}

	var long []uint16 // UTF-16 units of the pending long name, in reverse order of the entries
	for pos := 0; pos+32 <= len(data); pos += 32 {
		d := data[pos : pos+32]
		switch {
		case d[0] == 0:
			return nil
		case d[0] == 0xE5:
			long = nil
			continue
		}
		attr := d[11]
		if attr&0x3F == 0x0F { // long name entry
			if d[0]&0x40 != 0 {
				long = nil
			}
			var part []uint16
			for _, r := range [][2]int{{1, 11}, {14, 26}, {28, 32}} {
				for i := r[0]; i < r[1]; i += 2 {
					part = append(part, binary.LittleEndian.Uint16(d[i:]))
				}
			}
			long = append(part, long...)
			continue
		}
		name := shortDisplayName(d[:11])
		if long != nil {
			for i, u := range long {
				if u == 0 || u == 0xFFFF {
					long = long[:i]
					break
				}
			}
			name = string(utf16.Decode(long))
		}
		long = nil
		if attr&attrVolumeID != 0 || name == "." || name == ".." {
			continue
		}

		first := uint32(binary.LittleEndian.Uint16(d[20:]))<<16 | uint32(binary.LittleEndian.Uint16(d[26:]))
		e := &Entry{
			Path:  strings.TrimPrefix(dirPath+"/"+name, "/"),
			Mtime: fatTimestamp(binary.LittleEndian.Uint16(d[24:]), binary.LittleEndian.Uint16(d[22:]), d[13]),
			Atime: fatTimestamp(binary.LittleEndian.Uint16(d[18:]), 0, 0),
		}
		if attr&attrDirectory != 0 {
			e.Mode = fs.ModeDir | 0o755
			if err := fn(e); err != nil {
				return err
			}
			if err := rd.walkDir(first, e.Path, depth+1, fn); err != nil {
				return err
			}
			continue
		}
		e.Mode = 0o644
		if attr&attrReadOnly != 0 {
			e.Mode = 0o444
		}
		e.Size = int64(binary.LittleEndian.Uint32(d[28:]))
		if e.Size > 0 {
			count, err := rd.chain(first)
			if err != nil {
				return fmt.Errorf("fat32: %s: %w", e.Path, err)
			}
			if int64(count)*rd.clusterSize < e.Size {
				return fmt.Errorf("fat32: %s: %d clusters cannot hold %d bytes", e.Path, count, e.Size)
			}
			e.Offset = rd.clusterOffset(first)
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}

// shortDisplayName turns the 11 bytes of a short name into "NAME.EXT".
func shortDisplayName(b []byte) string {
	base := strings.TrimRight(string(b[:8]), " ")
	ext := strings.TrimRight(string(b[8:11]), " ")
	if ext == "" {
		return base
	}
	return base + "." + ext
}

// fatTimestamp decodes a directory entry timestamp. The tenth is in units of
// 10 ms, up to 199 to carry the odd second of the time field.
func fatTimestamp(date, tm uint16, tenth byte) time.Time {
	if date == 0 {
		return time.Time{}
	}
	return time.Date(
		1980+int(date>>9), time.Month(date>>5&0xF), int(date&0x1F),
		int(tm>>11), int(tm>>5&0x3F), int(tm&0x1F)*2+int(tenth)/100,
		int(tenth)%100*10_000_000, time.UTC)
}
