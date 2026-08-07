/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package erofstest

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"time"
)

// WriterToTar is an type which writes to a tar writer
type WriterToTar interface {
	WriteTo(*tar.Writer) error
}

type writerToFn func(*tar.Writer) error

func (w writerToFn) WriteTo(tw *tar.Writer) error {
	return w(tw)
}

// TarAll creates a WriterToTar which calls all the provided writers
// in the order in which they are provided.
func TarAll(wt ...WriterToTar) WriterToTar {
	return writerToFn(func(tw *tar.Writer) error {
		for _, w := range wt {
			if err := w.WriteTo(tw); err != nil {
				return err
			}
		}
		return nil
	})
}

// TarStream creates a WriterToTar which calls the provided
// writers until the channel is closed.
func TarStream(wc chan WriterToTar) WriterToTar {
	return writerToFn(func(tw *tar.Writer) error {
		for w := range wc {
			if err := w.WriteTo(tw); err != nil {
				return err
			}
		}
		return nil
	})
}

// TarFromWriterTo is used to create a tar stream from a tar record
// creator. This can be used to manufacture more specific tar records
// which allow testing specific tar cases which may be encountered
// by the untar process.
func TarFromWriterTo(wt WriterToTar) io.ReadCloser {
	r, w := io.Pipe()
	go func() {
		tw := tar.NewWriter(w)
		if err := wt.WriteTo(tw); err != nil {
			w.CloseWithError(err)
			return
		}
		w.CloseWithError(tw.Close())
	}()

	return r
}

// TarContext is used to create tar records
type TarContext struct {
	UID int
	GID int

	// ModTime sets the modtimes for all files, if nil the current time
	// is used for each file when it was written
	ModTime *time.Time

	Xattrs map[string]string
}

func (tc TarContext) newHeader(mode os.FileMode, name, link string, size int64) *tar.Header {
	ti := tarInfo{
		name: name,
		mode: mode,
		size: size,
		modt: tc.ModTime,
		hdr: &tar.Header{
			Uid:    tc.UID,
			Gid:    tc.GID,
			Xattrs: tc.Xattrs,
		},
	}

	if mode&os.ModeSymlink == 0 && link != "" {
		ti.hdr.Typeflag = tar.TypeLink
		ti.hdr.Linkname = link
	}
	hdr, err := tar.FileInfoHeader(ti, link)
	if err != nil {
		// Only returns an error on bad input mode
		panic(err)
	}

	return hdr
}

type tarInfo struct {
	name string
	mode os.FileMode
	size int64
	modt *time.Time
	hdr  *tar.Header
}

func (ti tarInfo) Name() string {
	return ti.name
}

func (ti tarInfo) Size() int64 {
	return ti.size
}
func (ti tarInfo) Mode() os.FileMode {
	return ti.mode
}

func (ti tarInfo) ModTime() time.Time {
	if ti.modt != nil {
		return *ti.modt
	}
	return time.Now().UTC()
}

func (ti tarInfo) IsDir() bool {
	return (ti.mode & os.ModeDir) != 0
}
func (ti tarInfo) Sys() interface{} {
	return ti.hdr
}

// WithUIDGID sets the UID and GID for tar entries
func (tc TarContext) WithUIDGID(uid, gid int) TarContext {
	ntc := tc
	ntc.UID = uid
	ntc.GID = gid
	return ntc
}

// WithModTime sets the ModTime for tar entries
func (tc TarContext) WithModTime(modtime time.Time) TarContext {
	ntc := tc
	ntc.ModTime = &modtime
	return ntc
}

// WithXattrs adds these xattrs to all files, merges with any
// previously added xattrs
func (tc TarContext) WithXattrs(xattrs map[string]string) TarContext {
	ntc := tc
	if ntc.Xattrs == nil {
		ntc.Xattrs = map[string]string{}
	}
	for k, v := range xattrs {
		ntc.Xattrs[k] = v
	}
	return ntc
}

// File returns a regular file tar entry using the provided bytes
func (tc TarContext) File(name string, content []byte, perm os.FileMode) WriterToTar {
	return writerToFn(func(tw *tar.Writer) error {
		return writeHeaderAndContent(tw, tc.newHeader(perm, name, "", int64(len(content))), content)
	})
}

// SparseFile returns a tar entry for a file of the given size filled with
// zeros, with optional data written at dataOffset. The content is streamed
// to avoid allocating the full size in memory.
func (tc TarContext) SparseFile(name string, size int64, data []byte, dataOffset int64, perm os.FileMode) WriterToTar {
	return writerToFn(func(tw *tar.Writer) error {
		hdr := tc.newHeader(perm, name, "", size)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		zeros := make([]byte, 64*1024)
		written := int64(0)
		for written < size {
			chunk := size - written
			if chunk > int64(len(zeros)) {
				chunk = int64(len(zeros))
			}
			buf := zeros[:chunk]
			// Overlay data at the correct offset.
			if data != nil && written+chunk > dataOffset && written < dataOffset+int64(len(data)) {
				buf = make([]byte, chunk)
				dStart := max(dataOffset-written, 0)
				sStart := max(written-dataOffset, 0)
				copy(buf[dStart:], data[sStart:])
			}
			n, err := tw.Write(buf)
			written += int64(n)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// Dir returns a directory tar entry
func (tc TarContext) Dir(name string, perm os.FileMode) WriterToTar {
	return writerToFn(func(tw *tar.Writer) error {
		return writeHeaderAndContent(tw, tc.newHeader(perm|os.ModeDir, name, "", 0), nil)
	})
}

// Symlink returns a symlink tar entry
func (tc TarContext) Symlink(oldname, newname string) WriterToTar {
	return writerToFn(func(tw *tar.Writer) error {
		return writeHeaderAndContent(tw, tc.newHeader(0777|os.ModeSymlink, newname, oldname, 0), nil)
	})
}

// Link returns a hard link tar entry
func (tc TarContext) Link(oldname, newname string) WriterToTar {
	return writerToFn(func(tw *tar.Writer) error {
		return writeHeaderAndContent(tw, tc.newHeader(0777, newname, oldname, 0), nil)
	})
}

func (tc TarContext) Device(name string, ftype os.FileMode, major int64, minor int64) WriterToTar {
	return writerToFn(func(tw *tar.Writer) error {
		hdr := tc.newHeader(0600, name, "", 0)
		hdr.Typeflag = typeFlag(ftype)
		hdr.Devmajor = major
		hdr.Devminor = minor
		return writeHeaderAndContent(tw, hdr, nil)
	})
}

func typeFlag(ftype os.FileMode) byte {
	if ftype&os.ModeCharDevice != 0 {
		return tar.TypeChar
	}
	if ftype&os.ModeNamedPipe != 0 {
		return tar.TypeFifo
	}
	return tar.TypeBlock
}

func writeHeaderAndContent(tw *tar.Writer, h *tar.Header, b []byte) error {
	if h.Size != int64(len(b)) {
		return errors.New("bad content length")
	}
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	if len(b) > 0 {
		if _, err := tw.Write(b); err != nil {
			return err
		}
	}
	return nil
}
