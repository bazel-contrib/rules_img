package erofstest

import (
	"io/fs"
	"path"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs"
)

// CheckXattrs verifies that the named path has exactly the expected xattrs.
func CheckXattrs(t testing.TB, fsys fs.FS, name string, expected map[string]string) {
	t.Helper()

	fi, err := fs.Stat(fsys, name)
	if err != nil {
		t.Errorf("stat %s: %v", name, err)
		return
	}

	st, ok := fi.Sys().(*erofs.Stat)
	if !ok {
		t.Errorf("%s: expected *erofs.Stat from Sys(), got %T", name, fi.Sys())
		return
	}

	if len(st.Xattrs) != len(expected) {
		t.Errorf("%s: xattr count %d, want %d", name, len(st.Xattrs), len(expected))
		return
	}

	for k, v := range expected {
		if actual, ok := st.Xattrs[k]; !ok {
			t.Errorf("%s: missing xattr %q: %v", name, k, st.Xattrs)
		} else if actual != v {
			t.Errorf("%s: xattr %q: got %q, want %q", name, k, actual, v)
		}
	}
}

// CheckDevice verifies that the named path is a device/fifo with the expected
// type and rdev.
func CheckDevice(t testing.TB, fsys fs.FS, name string, ftype fs.FileMode, rdev uint32) {
	t.Helper()

	f, err := fsys.Open(name)
	if err != nil {
		t.Errorf("open %s: %v", name, err)
		return
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		t.Errorf("stat %s: %v", name, err)
		return
	}

	st, ok := fi.Sys().(*erofs.Stat)
	if !ok {
		t.Errorf("%s: expected *erofs.Stat from Sys(), got %T", name, fi.Sys())
		return
	}

	if st.Mode&fs.ModeType != ftype {
		t.Errorf("%s: type %v, want %v", name, st.Mode&fs.ModeType, ftype)
	}
	if st.Rdev != rdev {
		t.Errorf("%s: rdev %d, want %d", name, st.Rdev, rdev)
	}
}

// CheckMode verifies that the named path has the expected [fs.FileMode],
// including the setuid, setgid and sticky bits. Symlinks are not followed.
//
// The mode is checked on every path that reports one: Lstat, Stat, the
// parent directory's [fs.DirEntry] (as used by fs.WalkDir) and
// Sys().(*erofs.Stat).Mode, which must all agree.
func CheckMode(t testing.TB, fsys fs.FS, name string, want fs.FileMode) {
	t.Helper()

	lfs, ok := fsys.(lstatFS)
	if !ok {
		t.Errorf("FS does not implement Lstat")
		return
	}
	fi, err := lfs.Lstat(name)
	if err != nil {
		t.Errorf("lstat %s: %v", name, err)
		return
	}
	checkInfoMode(t, "Lstat("+name+")", fi, want)

	// Stat follows symlinks, so it reports the target's mode instead.
	if want&fs.ModeSymlink == 0 {
		fi, err := fs.Stat(fsys, name)
		if err != nil {
			t.Errorf("stat %s: %v", name, err)
		} else {
			checkInfoMode(t, "Stat("+name+")", fi, want)
		}
	}

	ents, err := fs.ReadDir(fsys, path.Dir(name))
	if err != nil {
		t.Errorf("readdir %s: %v", path.Dir(name), err)
		return
	}
	base := path.Base(name)
	for _, ent := range ents {
		if ent.Name() != base {
			continue
		}
		if got := ent.Type(); got != want&fs.ModeType {
			t.Errorf("DirEntry(%s).Type() = %v, want %v", name, got, want&fs.ModeType)
		}
		fi, err := ent.Info()
		if err != nil {
			t.Errorf("DirEntry(%s).Info(): %v", name, err)
			return
		}
		checkInfoMode(t, "DirEntry("+name+").Info()", fi, want)
		return
	}
	t.Errorf("%s: not listed in %s", name, path.Dir(name))
}

// checkInfoMode verifies fi.Mode() against want and against the raw mode in
// Sys(), which callers may use instead.
func checkInfoMode(t testing.TB, what string, fi fs.FileInfo, want fs.FileMode) {
	t.Helper()

	got := fi.Mode()
	if got != want {
		t.Errorf("%s: mode %v (%#o), want %v (%#o)", what, got, uint32(got), want, uint32(want))
	}
	if fi.IsDir() != want.IsDir() {
		t.Errorf("%s: IsDir() = %v, want %v", what, fi.IsDir(), want.IsDir())
	}
	st, ok := fi.Sys().(*erofs.Stat)
	if !ok {
		t.Errorf("%s: expected *erofs.Stat from Sys(), got %T", what, fi.Sys())
		return
	}
	if st.Mode != got {
		t.Errorf("%s: Mode() = %v (%#o) disagrees with Sys().Mode = %v (%#o)",
			what, got, uint32(got), st.Mode, uint32(st.Mode))
	}
}

// Stat returns the *erofs.Stat for the named path, or calls t.Fatal if it
// cannot be obtained.
func Stat(t testing.TB, fsys fs.FS, name string) *erofs.Stat {
	t.Helper()

	fi, err := fs.Stat(fsys, name)
	if err != nil {
		t.Fatalf("stat %s: %v", name, err)
	}

	st, ok := fi.Sys().(*erofs.Stat)
	if !ok {
		t.Fatalf("%s: expected *erofs.Stat from Sys(), got %T", name, fi.Sys())
	}
	return st
}

// lstatFS is the interface for Lstat only, without requiring ReadLink.
type lstatFS interface {
	Lstat(name string) (fs.FileInfo, error)
}

// Lstat returns the *erofs.Stat for the named path without following symlinks,
// or calls t.Fatal if it cannot be obtained.
func Lstat(t testing.TB, fsys fs.FS, name string) *erofs.Stat {
	t.Helper()

	lfs, ok := fsys.(lstatFS)
	if !ok {
		t.Fatalf("FS does not implement Lstat")
	}

	fi, err := lfs.Lstat(name)
	if err != nil {
		t.Fatalf("lstat %s: %v", name, err)
	}

	st, ok := fi.Sys().(*erofs.Stat)
	if !ok {
		t.Fatalf("%s: expected *erofs.Stat from Sys(), got %T", name, fi.Sys())
	}
	return st
}
