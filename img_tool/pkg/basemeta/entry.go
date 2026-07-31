package basemeta

import (
	"archive/tar"
	"fmt"
	"strings"
	"time"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/proto/baselayer"
)

// SchilyXattrPrefix is the PAX record namespace tar uses for extended
// attributes.
const SchilyXattrPrefix = "SCHILY.xattr."

// NormalizePath turns a user-supplied image path into the tar convention used
// throughout a stream: no leading "/", no trailing "/", no "./" prefix.
func NormalizePath(path string) string {
	path = strings.TrimPrefix(path, "/")
	for strings.HasPrefix(path, "./") {
		path = strings.TrimPrefix(path, "./")
	}
	return strings.TrimSuffix(path, "/")
}

// Dir builds a directory entry.
func Dir(path string, mode int64) *baselayer.BaseEntry {
	return &baselayer.BaseEntry{
		Path: NormalizePath(path),
		Type: baselayer.EntryType_ENTRY_TYPE_DIRECTORY,
		Mode: mode,
	}
}

// File builds a regular file entry with inline content.
func File(path string, mode int64, content []byte) *baselayer.BaseEntry {
	return &baselayer.BaseEntry{
		Path:    NormalizePath(path),
		Type:    baselayer.EntryType_ENTRY_TYPE_REGULAR,
		Mode:    mode,
		Content: &baselayer.BaseEntry_Inline{Inline: content},
	}
}

// FileFromPath builds a regular file entry backed by a file on disk. srcPath is
// resolved relative to the execution root when the entry is turned into a layer.
func FileFromPath(path string, mode int64, srcPath string) *baselayer.BaseEntry {
	return &baselayer.BaseEntry{
		Path:    NormalizePath(path),
		Type:    baselayer.EntryType_ENTRY_TYPE_REGULAR,
		Mode:    mode,
		Content: &baselayer.BaseEntry_FilePath{FilePath: srcPath},
	}
}

// Symlink builds a symbolic link entry pointing at target.
func Symlink(path, target string) *baselayer.BaseEntry {
	return &baselayer.BaseEntry{
		Path:     NormalizePath(path),
		Type:     baselayer.EntryType_ENTRY_TYPE_SYMLINK,
		Mode:     0o777,
		Linkname: target,
	}
}

// WithOwner sets the numeric and symbolic owner on an entry and returns it, so
// it can be chained onto a constructor.
func WithOwner(entry *baselayer.BaseEntry, uid, gid int64, uname, gname string) *baselayer.BaseEntry {
	entry.Uid = uid
	entry.Gid = gid
	entry.Uname = uname
	entry.Gname = gname
	return entry
}

// WithProducer records the label of the rule that created the entry, used to
// name both sides of a conflict during [Merge].
func WithProducer(entries []*baselayer.BaseEntry, producer string) []*baselayer.BaseEntry {
	if producer == "" {
		return entries
	}
	for _, entry := range entries {
		entry.Producer = producer
	}
	return entries
}

// ToTarHeader converts an entry into the tar header it describes. The Size
// field is left at zero for regular files: the caller knows whether the content
// comes from an inline blob or a file on disk and fills it in.
func ToTarHeader(entry *baselayer.BaseEntry) (*tar.Header, error) {
	typeflag, err := tarTypeflag(entry.GetType())
	if err != nil {
		return nil, fmt.Errorf("entry %q: %w", entry.GetPath(), err)
	}

	name := NormalizePath(entry.GetPath())
	if name == "" {
		return nil, fmt.Errorf("entry has an empty path")
	}
	if typeflag == tar.TypeDir {
		name += "/"
	}

	hdr := &tar.Header{
		Typeflag: typeflag,
		Name:     name,
		Mode:     entry.GetMode(),
		Uid:      int(entry.GetUid()),
		Gid:      int(entry.GetGid()),
		Uname:    entry.GetUname(),
		Gname:    entry.GetGname(),
		Linkname: entry.GetLinkname(),
		Devmajor: entry.GetDevmajor(),
		Devminor: entry.GetDevminor(),
		ModTime:  time.Unix(0, entry.GetMtimeUnixNanos()).UTC(),
		Format:   tar.FormatPAX,
	}
	if entry.GetAtimeUnixNanos() != 0 {
		hdr.AccessTime = time.Unix(0, entry.GetAtimeUnixNanos()).UTC()
	}
	if entry.GetCtimeUnixNanos() != 0 {
		hdr.ChangeTime = time.Unix(0, entry.GetCtimeUnixNanos()).UTC()
	}

	records := entry.GetPaxRecords()
	xattrs := entry.GetXattrs()
	if len(records) > 0 || len(xattrs) > 0 {
		hdr.PAXRecords = make(map[string]string, len(records)+len(xattrs))
		for k, v := range records {
			hdr.PAXRecords[k] = v
		}
		// Extended attributes win over a hand-written SCHILY.xattr. record for
		// the same name: they are the typed way to say the same thing.
		for k, v := range xattrs {
			hdr.PAXRecords[SchilyXattrPrefix+k] = v
		}
	}

	return hdr, nil
}

func tarTypeflag(entryType baselayer.EntryType) (byte, error) {
	switch entryType {
	case baselayer.EntryType_ENTRY_TYPE_REGULAR:
		return tar.TypeReg, nil
	case baselayer.EntryType_ENTRY_TYPE_DIRECTORY:
		return tar.TypeDir, nil
	case baselayer.EntryType_ENTRY_TYPE_SYMLINK:
		return tar.TypeSymlink, nil
	case baselayer.EntryType_ENTRY_TYPE_HARDLINK:
		return tar.TypeLink, nil
	case baselayer.EntryType_ENTRY_TYPE_CHAR_DEVICE:
		return tar.TypeChar, nil
	case baselayer.EntryType_ENTRY_TYPE_BLOCK_DEVICE:
		return tar.TypeBlock, nil
	case baselayer.EntryType_ENTRY_TYPE_FIFO:
		return tar.TypeFifo, nil
	case baselayer.EntryType_ENTRY_TYPE_UNSPECIFIED:
		return 0, fmt.Errorf("entry type is unset")
	default:
		return 0, fmt.Errorf("unknown entry type %d", entryType)
	}
}
