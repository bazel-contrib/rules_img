// Package tarmeta holds the canonical definitions of a tar entry's normalized
// form and its content hash, shared by every content-addressed layer sink.
//
// The hashes here are what a content manifest records, so two sinks that agree
// on them produce interchangeable manifests: a layer's deduplication state
// stays meaningful across the tar and EROFS layer formats.
package tarmeta

import (
	"archive/tar"
	"encoding/binary"
	"hash"
	"maps"
	"slices"
	"strings"
	"time"
)

func hash32(h hash.Hash, i uint32) {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], i)
	h.Write(encoded[:])
}

func hash64(h hash.Hash, i uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], i)
	h.Write(encoded[:])
}

func hashString(h hash.Hash, s string) {
	hash64(h, uint64(len(s)))
	h.Write([]byte(s))
}

func hashMapStrStr(h hash.Hash, m map[string]string) {
	hash64(h, uint64(len(m)))
	keys := make([]string, len(m))
	i := 0
	for k := range m {
		keys[i] = k
		i++
	}
	slices.Sort(keys)

	for _, k := range keys {
		hashString(h, k)
		hashString(h, m[k])
	}
}

// Clone returns a copy of th carrying only the fields that describe the entry,
// so a caller can normalize or hash it without disturbing the original.
func Clone(th *tar.Header) tar.Header {
	return tar.Header{
		Typeflag:   th.Typeflag,
		Name:       th.Name,
		Linkname:   th.Linkname,
		Size:       th.Size,
		Mode:       th.Mode,
		Uid:        th.Uid,
		Gid:        th.Gid,
		Uname:      th.Uname,
		Gname:      th.Gname,
		ModTime:    th.ModTime,
		AccessTime: th.AccessTime,
		ChangeTime: th.ChangeTime,
		Devmajor:   th.Devmajor,
		Devminor:   th.Devminor,
		Xattrs:     maps.Clone(th.Xattrs),
		PAXRecords: maps.Clone(th.PAXRecords),
		Format:     th.Format,
	}
}

// Normalize rewrites th into the canonical form the content hash is defined
// over: PAX format, directory type inferred from a trailing slash, link names
// and device numbers cleared for types that have none, timestamps rounded to
// whole UTC seconds, and extended attributes folded into their SCHILY.xattr.*
// PAX records.
func Normalize(th *tar.Header) {
	th.Format = tar.FormatPAX
	if strings.HasSuffix(th.Name, "/") && th.Typeflag == tar.TypeReg {
		th.Typeflag = tar.TypeDir
	}
	if th.Typeflag != tar.TypeLink && th.Typeflag != tar.TypeSymlink {
		th.Linkname = ""
	}
	if !th.ModTime.IsZero() {
		th.ModTime = th.ModTime.UTC().Round(time.Second)
	}
	if !th.AccessTime.IsZero() {
		th.AccessTime = th.AccessTime.UTC().Round(time.Second)
	}
	if !th.ChangeTime.IsZero() {
		th.ChangeTime = th.ChangeTime.UTC().Round(time.Second)
	}
	if th.Typeflag != tar.TypeChar && th.Typeflag != tar.TypeBlock {
		th.Devmajor = 0
		th.Devminor = 0
	}
	if len(th.Xattrs) > 0 {
		if th.PAXRecords == nil {
			th.PAXRecords = make(map[string]string)
		}
		for k, v := range th.Xattrs {
			th.PAXRecords["SCHILY.xattr."+k] = v
		}
		th.Xattrs = nil
	}
}

// Hash writes the normalized form of th into h. Combined with the digest of the
// entry's content it forms the entry's node hash, which identifies an entry with
// both its metadata and its content.
func Hash(h hash.Hash, th tar.Header) {
	th = Clone(&th)
	Normalize(&th)

	h.Write([]byte{th.Typeflag})
	hashString(h, th.Name)
	hashString(h, th.Linkname)
	hash64(h, uint64(th.Size))
	hash64(h, uint64(th.Mode))
	hash64(h, uint64(th.Uid))
	hash64(h, uint64(th.Gid))
	hashString(h, th.Uname)
	hashString(h, th.Gname)
	hash64(h, uint64(th.ModTime.Unix()))
	hash64(h, uint64(th.AccessTime.Unix()))
	hash64(h, uint64(th.ChangeTime.Unix()))
	hash64(h, uint64(th.Devmajor))
	hash64(h, uint64(th.Devminor))
	hashMapStrStr(h, th.Xattrs)
	hashMapStrStr(h, th.PAXRecords)
	hash64(h, uint64(th.Format))
}

// IsBlob reports whether an entry carries no metadata beyond its path and
// content, and can therefore be identified by its content digest alone. Such an
// entry can be deduplicated against any other entry with the same content,
// where an entry with metadata of its own needs its full node hash.
func IsBlob(hdr *tar.Header) bool {
	if hdr.Typeflag != tar.TypeReg {
		return false
	}
	if strings.HasSuffix(hdr.Name, "/") {
		return false
	}
	// Ignore linkname, it doesn't matter for TypeReg
	// which we already checked for.
	if hdr.Mode != 0o755 {
		// if mode is not (rwxr-xr-x)
		// we cannot store this node as a blob.
		return false
	}
	if hdr.Uid != 0 || hdr.Gid != 0 {
		return false
	}
	if hdr.Uname != "" || hdr.Gname != "" {
		return false
	}
	if !hdr.ModTime.IsZero() {
		return false
	}
	if !hdr.AccessTime.IsZero() {
		return false
	}
	if !hdr.ChangeTime.IsZero() {
		return false
	}
	if hdr.Devmajor != 0 || hdr.Devminor != 0 {
		return false
	}
	if hdr.Xattrs != nil || hdr.PAXRecords != nil {
		return false
	}
	return true
}
