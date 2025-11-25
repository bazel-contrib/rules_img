package layer

import (
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree/treeartifact"
)

type addFile struct {
	PathInImage string
	File        string
	FileType    api.FileType
}

func (a addFile) Type() api.FileType {
	return a.FileType
}

func (a addFile) Open() (fs.File, error) {
	return os.Open(a.File)
}

func (a addFile) Tree() (fs.FS, error) {
	if a.FileType != api.Directory {
		return nil, fmt.Errorf("cannot get tree for non-directory file: %s", a.File)
	}
	// TODO: consider using a special
	// file system for tree artifacts
	// that filters out non-regular files.
	return treeartifact.TreeArtifactFS(a.File), nil
}

func (a addFile) Readlink() (string, error) {
	if a.FileType != api.Symlink {
		return "", fmt.Errorf("cannot get link target for non-symlink file: %s", a.File)
	}
	return os.Readlink(a.File)
}

func (a addFile) Path() string {
	return a.File
}

type addFiles []addFile

func (a *addFiles) String() string {
	var sb strings.Builder
	for i, a := range *a {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(a.PathInImage)
		sb.WriteString("=")
		sb.WriteString(a.File)
	}
	return sb.String()
}

func (a *addFiles) Set(value string) error {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid format for --add: %s", value)
	}
	if len(parts[0]) == 0 {
		return fmt.Errorf("path in image cannot be empty: %s", value)
	}
	if parts[0][0] == '/' {
		// remove leading slash in target
		parts[0] = parts[0][1:]
	}
	fInfo, err := os.Stat(parts[1])
	if err != nil {
		return fmt.Errorf("file %s does not exist: %w", parts[1], err)
	}
	var fileType api.FileType
	if fInfo.Mode() & fs.ModeSymlink != 0 {
		fileType = api.Symlink
	} else if fInfo.IsDir() {
		fileType = api.Directory
	} else {
		fileType = api.RegularFile
	}
	*a = append(*a, addFile{
		PathInImage: parts[0],
		File:        parts[1],
		FileType:    fileType,
	})
	return nil
}

type addFromFileArgs []string

func (a *addFromFileArgs) String() string {
	return strings.Join(*a, ", ")
}

func (a *addFromFileArgs) Set(value string) error {
	if _, err := os.Stat(value); err != nil {
		return fmt.Errorf("file %s does not exist: %w", value, err)
	}
	*a = append(*a, value)
	return nil
}

type importTars []string

func (i *importTars) String() string {
	return strings.Join(*i, ", ")
}

func (i *importTars) Set(value string) error {
	if _, err := os.Stat(value); err != nil {
		return fmt.Errorf("file %s does not exist: %w", value, err)
	}
	*i = append(*i, value)
	return nil
}

type runfilesForExecutable struct {
	Executable       string
	RunfilesFromFile string
}

type executable struct {
	PathInImage           string
	Executable            string
	RunfilesParameterFile string
}

type runfilesForExecutables []runfilesForExecutable

func (r *runfilesForExecutables) String() string {
	var sb strings.Builder
	for i, r := range *r {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(r.Executable)
		sb.WriteString("=")
		sb.WriteString(r.RunfilesFromFile)
	}
	return sb.String()
}

func (r *runfilesForExecutables) Set(value string) error {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid format for --runfiles: %s", value)
	}
	if _, err := os.Stat(parts[1]); err != nil {
		return fmt.Errorf("parameter file %s does not exist: %w", parts[1], err)
	}
	*r = append(*r, runfilesForExecutable{
		Executable:       parts[0],
		RunfilesFromFile: parts[1],
	})
	return nil
}

type executables []executable

func (e *executables) String() string {
	var sb strings.Builder
	for i, e := range *e {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(e.PathInImage)
		sb.WriteString("=")
		sb.WriteString(e.Executable)
	}
	return sb.String()
}

func (e *executables) Set(value string) error {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid format for --executable: %s", value)
	}
	if _, err := os.Stat(parts[1]); err != nil {
		return fmt.Errorf("executable %s does not exist: %w", parts[1], err)
	}
	if len(parts[0]) == 0 {
		return fmt.Errorf("path in image cannot be empty: %s", value)
	}
	if parts[0][0] == '/' {
		// remove leading slash in target
		parts[0] = parts[0][1:]
	}
	*e = append(*e, executable{
		PathInImage: parts[0],
		Executable:  parts[1],
	})
	return nil
}

type symlinksFromFileArgs []string

func (s *symlinksFromFileArgs) String() string {
	return strings.Join(*s, ", ")
}

func (s *symlinksFromFileArgs) Set(value string) error {
	if _, err := os.Stat(value); err != nil {
		return fmt.Errorf("file %s does not exist: %w", value, err)
	}
	*s = append(*s, value)
	return nil
}

type symlink struct {
	LinkName string
	Target   string
}

type symlinks []symlink

func (s *symlinks) String() string {
	var sb strings.Builder
	for i, s := range *s {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(s.LinkName)
		sb.WriteString(" → ")
		sb.WriteString(s.Target)
	}
	return sb.String()
}

func (s *symlinks) Set(value string) error {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid format for --symlink: %s", value)
	}
	if len(parts[0]) == 0 {
		return fmt.Errorf("link name cannot be empty: %s", value)
	}
	if parts[0][0] == '/' {
		// remove leading slash in link name
		parts[0] = parts[0][1:]
	}
	*s = append(*s, symlink{
		LinkName: parts[0],
		Target:   parts[1],
	})
	return nil
}

type contentManifests []string

func (m *contentManifests) String() string {
	return strings.Join(*m, ", ")
}

func (m *contentManifests) Set(value string) error {
	if _, err := os.Stat(value); err != nil {
		return fmt.Errorf("file %s does not exist: %w", value, err)
	}
	*m = append(*m, value)
	return nil
}

// annotationsFlag implements flag.Value for key-value pairs
type annotationsFlag map[string]string

func (a annotationsFlag) String() string {
	var keys []string
	for k := range a {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var pairs []string
	for _, k := range keys {
		pairs = append(pairs, fmt.Sprintf("%s=%s", k, a[k]))
	}
	return strings.Join(pairs, ",")
}

func (a annotationsFlag) Set(value string) error {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("annotation must be in format key=value, got: %s", value)
	}
	key := strings.TrimSpace(parts[0])
	val := strings.TrimSpace(parts[1])
	if key == "" {
		return fmt.Errorf("annotation key cannot be empty")
	}
	a[key] = val
	return nil
}

// fileMetadataFlag implements flag.Value for path=json metadata pairs
type fileMetadataFlag map[string]string

func (f fileMetadataFlag) String() string {
	var keys []string
	for k := range f {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var pairs []string
	for _, k := range keys {
		pairs = append(pairs, fmt.Sprintf("%s=%s", k, f[k]))
	}
	return strings.Join(pairs, ",")
}

func (f fileMetadataFlag) Set(value string) error {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("file metadata must be in format path=json, got: %s", value)
	}
	path := strings.TrimSpace(parts[0])
	jsonMetadata := strings.TrimSpace(parts[1])
	if path == "" {
		return fmt.Errorf("file path cannot be empty")
	}
	f[path] = jsonMetadata
	return nil
}
