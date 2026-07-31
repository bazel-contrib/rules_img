package base

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"sort"
	"strconv"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/basemeta"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/proto/baselayer"
)

// skeletonGroup is a named set of directories that can be enabled or disabled
// as a unit, matching the attributes of the linux_skeleton rule.
type skeletonGroup struct {
	name string
	// dirs are created when the group is enabled and the image is not usr-merged,
	// or in both cases when usrMergedDirs/usrMergedLinks are empty.
	dirs []skeletonDir
	// usrMergedDirs replaces dirs when the image is usr-merged.
	usrMergedDirs []skeletonDir
	// usrMergedLinks are symlinks created only when the image is usr-merged.
	usrMergedLinks []skeletonLink
}

type skeletonDir struct {
	path string
	mode int64
}

type skeletonLink struct {
	path   string
	target string
}

// skeletonGroups defines the default Linux directory skeleton. Modes follow the
// Filesystem Hierarchy Standard and a stock Debian install:
//   - 0755 for anything the system owns and users only read
//   - 1777 (sticky, world-writable) for shared scratch space
//   - 0700 for /root
//   - 0555 for the kernel pseudo-filesystems, which are read-only mount points
var skeletonGroups = []skeletonGroup{
	{
		name: "etc",
		dirs: []skeletonDir{{"/etc", 0o755}},
	},
	{
		name: "bin_and_lib",
		// Without usr-merge, /bin and /usr/bin are distinct real directories.
		dirs: []skeletonDir{
			{"/bin", 0o755}, {"/sbin", 0o755}, {"/lib", 0o755}, {"/lib64", 0o755},
			{"/usr", 0o755}, {"/usr/bin", 0o755}, {"/usr/sbin", 0o755},
			{"/usr/lib", 0o755}, {"/usr/lib64", 0o755},
			{"/usr/local", 0o755}, {"/usr/local/bin", 0o755}, {"/usr/local/sbin", 0o755},
			{"/usr/local/lib", 0o755}, {"/usr/share", 0o755}, {"/usr/include", 0o755},
		},
		// With usr-merge, the top-level names are symlinks into /usr.
		usrMergedDirs: []skeletonDir{
			{"/usr", 0o755}, {"/usr/bin", 0o755}, {"/usr/sbin", 0o755},
			{"/usr/lib", 0o755}, {"/usr/lib64", 0o755},
			{"/usr/local", 0o755}, {"/usr/local/bin", 0o755}, {"/usr/local/sbin", 0o755},
			{"/usr/local/lib", 0o755}, {"/usr/share", 0o755}, {"/usr/include", 0o755},
		},
		usrMergedLinks: []skeletonLink{
			{"/bin", "usr/bin"},
			{"/sbin", "usr/sbin"},
			{"/lib", "usr/lib"},
			{"/lib64", "usr/lib64"},
		},
	},
	{
		name: "home",
		dirs: []skeletonDir{{"/home", 0o755}},
	},
	{
		name: "root",
		dirs: []skeletonDir{{"/root", 0o700}},
	},
	{
		name: "tmp",
		dirs: []skeletonDir{{"/tmp", 0o1777}},
	},
	{
		name: "var",
		dirs: []skeletonDir{
			{"/var", 0o755}, {"/var/log", 0o755}, {"/var/tmp", 0o1777},
			{"/var/cache", 0o755}, {"/var/lib", 0o755}, {"/var/spool", 0o755},
		},
	},
	{
		name: "run",
		dirs: []skeletonDir{{"/run", 0o755}, {"/run/lock", 0o1777}},
	},
	{
		name: "mount_points",
		dirs: []skeletonDir{
			{"/dev", 0o755}, {"/proc", 0o555}, {"/sys", 0o555},
			{"/mnt", 0o755}, {"/media", 0o755},
		},
	},
	{
		name: "opt_srv",
		dirs: []skeletonDir{{"/opt", 0o755}, {"/srv", 0o755}},
	},
}

// varRunLinks point the legacy /var/run and /var/lock at /run, as every modern
// distribution does. They are emitted when both the var and run groups are on.
var varRunLinks = []skeletonLink{
	{"/var/run", "../run"},
	{"/var/lock", "../run/lock"},
}

// skeletonProcess implements `img base skeleton`.
func skeletonProcess(_ context.Context, args []string) {
	enabled := make(kvFlag)
	extraDirs := make(kvFlag)
	var outputPath, producer string
	var usrMerged bool

	flagSet := flag.NewFlagSet("base skeleton", flag.ExitOnError)
	flagSet.Var(enabled, "group", "Enable or disable a directory group, as NAME=enabled|disabled. Can be repeated.")
	flagSet.Var(extraDirs, "directory", "An extra directory as PATH=<file metadata JSON>. Can be repeated.")
	flagSet.StringVar(&outputPath, "output", "", "Path of the base metadata stream to write.")
	flagSet.StringVar(&producer, "producer", "", "Label of the rule producing this stream, used in conflict messages.")
	flagSet.BoolVar(&usrMerged, "usr-merged", true, "Make /bin, /sbin, /lib and /lib64 symlinks into /usr.")
	if err := flagSet.Parse(args); err != nil {
		fail("skeleton", err)
	}

	known := make(map[string]bool, len(skeletonGroups))
	for _, g := range skeletonGroups {
		known[g.name] = true
	}
	for _, name := range enabled.keys() {
		if !known[name] {
			fail("skeleton", fmt.Errorf("unknown directory group %q", name))
		}
	}

	var entries []*baselayer.BaseEntry
	groupOn := make(map[string]bool, len(skeletonGroups))
	for _, g := range skeletonGroups {
		// Groups default to enabled: the point of the rule is a working
		// skeleton, and a caller who wants less says so explicitly.
		if enabled[g.name] == "disabled" {
			continue
		}
		groupOn[g.name] = true

		dirs := g.dirs
		if usrMerged && len(g.usrMergedDirs) > 0 {
			dirs = g.usrMergedDirs
		}
		for _, d := range dirs {
			entries = append(entries, basemeta.Dir(d.path, d.mode))
		}
		if usrMerged {
			for _, l := range g.usrMergedLinks {
				entries = append(entries, basemeta.Symlink(l.path, l.target))
			}
		}
	}

	if groupOn["var"] && groupOn["run"] {
		for _, l := range varRunLinks {
			entries = append(entries, basemeta.Symlink(l.path, l.target))
		}
	}

	extra, err := extraDirectoryEntries(extraDirs)
	if err != nil {
		fail("skeleton", err)
	}
	entries = append(entries, extra...)

	if err := writeStream(outputPath, producer, entries); err != nil {
		fail("skeleton", err)
	}
}

// fileMetadataJSON mirrors the file_metadata() Starlark helper, so extra
// directories are configured the same way as files in an image_layer.
type fileMetadataJSON struct {
	Mode  *string `json:"mode,omitempty"`
	UID   *int64  `json:"uid,omitempty"`
	GID   *int64  `json:"gid,omitempty"`
	Uname *string `json:"uname,omitempty"`
	Gname *string `json:"gname,omitempty"`
}

// extraDirectoryEntries turns the --directory flags into entries, sorted by
// path for determinism.
func extraDirectoryEntries(dirs kvFlag) ([]*baselayer.BaseEntry, error) {
	paths := make([]string, 0, len(dirs))
	for path := range dirs {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	entries := make([]*baselayer.BaseEntry, 0, len(paths))
	for _, path := range paths {
		mode := int64(0o755)
		entry := basemeta.Dir(path, mode)
		raw := dirs[path]
		if raw != "" && raw != "{}" {
			var metadata fileMetadataJSON
			if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
				return nil, fmt.Errorf("parsing metadata for directory %s: %w", path, err)
			}
			if metadata.Mode != nil {
				parsed, err := strconv.ParseInt(*metadata.Mode, 8, 64)
				if err != nil {
					return nil, fmt.Errorf("directory %s: invalid octal mode %q: %w", path, *metadata.Mode, err)
				}
				entry.Mode = parsed
			}
			if metadata.UID != nil {
				entry.Uid = *metadata.UID
			}
			if metadata.GID != nil {
				entry.Gid = *metadata.GID
			}
			if metadata.Uname != nil {
				entry.Uname = *metadata.Uname
			}
			if metadata.Gname != nil {
				entry.Gname = *metadata.Gname
			}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
