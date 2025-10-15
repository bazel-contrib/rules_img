package tarcas

import (
	"archive/tar"
	"encoding/binary"
	"hash"
	"maps"
	"slices"
	"strings"
	"time"
)

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

func cloneTarHeader(th *tar.Header) tar.Header {
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
		PAXRecords: maps.Clone(th.PAXRecords),
		Format:     th.Format,
	}
}

func normalizeTarHeader(th *tar.Header) {
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
}

func hashTarHeader(h hash.Hash, th tar.Header) {
	th = cloneTarHeader(&th)
	normalizeTarHeader(&th)

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
	hashMapStrStr(h, th.PAXRecords)
	hash64(h, uint64(th.Format))
}

func isBlobTarHeader(hdr *tar.Header) bool {
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
	if hdr.PAXRecords != nil {
		return false
	}
	return true
}
