package treeartifact

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

type treeartifactFS string

func TreeArtifactFS(path string) treeartifactFS {
	return treeartifactFS(path)
}

func (t treeartifactFS) Stat(name string) (fs.FileInfo, error) {
	fullname := t.join(name)
	info, err := os.Stat(fullname)
	if err != nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
	}
	return &treeartifactFileInfo{
		name:     filepath.Base(fullname),
		realStat: info,
	}, nil
}

func (t treeartifactFS) Open(name string) (fs.File, error) {
	fullname := t.join(name)
	f, err := os.Open(fullname)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}

	return &treeartifactFile{
		File: f,
		name: filepath.Base(fullname),
	}, nil
}

func (t treeartifactFS) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(t.join(name))
}

func (t treeartifactFS) ReadDir(name string) ([]fs.DirEntry, error) {
	fullname := t.join(name)
	dirents, err := os.ReadDir(fullname)
	if err != nil {
		return nil, err
	}
	for i, entry := range dirents {
		if entry.Type()&fs.ModeSymlink != 0 {
			// If the entry is a symlink, we need to
			// resolve it to the real path.
			direntFullPath := filepath.Join(fullname, entry.Name())
			realpath, err := filepath.EvalSymlinks(direntFullPath)
			if err != nil {
				if runtime.GOOS == "windows" {
					// Exception: on Windows, we sometimes encounter nodes (Junctions? Symlinks? Who really knows?)
					// that report as fs.ModeSymlink, but on which filepath.EvalSymlinks fails.
					// We need to lie about the file type and report this as either a regular file or a directory.
					_, readDirErr := os.ReadDir(direntFullPath)
					behavesLikeADir := readDirErr == nil
					fInfo, err := os.Stat(direntFullPath)
					if err != nil {
						return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
					}
					dirents[i] = &windowsBrokenSymlinkDirEntry{
						name:            entry.Name(),
						behavesLikeADir: behavesLikeADir,
						DirEntry:        fs.FileInfoToDirEntry(fInfo),
					}
					continue
				}
				return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
			}
			fInfo, err := os.Stat(realpath)
			if err != nil {
				return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
			}
			dirents[i] = &treeArtifactDirEntry{
				name:     entry.Name(),
				DirEntry: fs.FileInfoToDirEntry(fInfo),
			}
		}
	}
	return dirents, nil
}

func (t treeartifactFS) join(name string) string {
	return filepath.Join(string(t), name)
}

type treeartifactFile struct {
	*os.File
	name string
}

func (f *treeartifactFile) Name() string {
	return f.name
}

func (f *treeartifactFile) Stat() (fs.FileInfo, error) {
	realStat, err := f.File.Stat()
	if err != nil {
		return nil, err
	}
	if realStat.Name() == f.name {
		return realStat, nil
	}
	return &treeartifactFileInfo{
		name:     f.name,
		realStat: realStat,
	}, nil
}

type treeartifactFileInfo struct {
	name     string
	realStat fs.FileInfo
}

func (f *treeartifactFileInfo) Name() string {
	return f.name
}

func (f *treeartifactFileInfo) Size() int64 {
	return f.realStat.Size()
}

func (f *treeartifactFileInfo) Mode() fs.FileMode {
	return f.realStat.Mode()
}

func (f *treeartifactFileInfo) ModTime() time.Time {
	return f.realStat.ModTime()
}

func (f *treeartifactFileInfo) IsDir() bool {
	return f.realStat.IsDir()
}

func (f *treeartifactFileInfo) Sys() any {
	return f.realStat.Sys()
}

type treeArtifactDirEntry struct {
	name string
	fs.DirEntry
}

func (d *treeArtifactDirEntry) Name() string {
	return d.name
}

type windowsBrokenSymlinkDirEntry struct {
	name            string
	behavesLikeADir bool
	fs.DirEntry
}

func (d *windowsBrokenSymlinkDirEntry) Name() string {
	return d.name
}

func (d *windowsBrokenSymlinkDirEntry) IsDir() bool {
	// If it quacks like a duck...
	return d.behavesLikeADir
}

func (d *windowsBrokenSymlinkDirEntry) Type() fs.FileMode {
	if d.behavesLikeADir {
		return fs.ModeDir
	}
	// Quacks like a regular file
	return 0
}
