package layer

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/basemeta"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/proto/baselayer"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree"
)

type baseEntries struct {
	emptyDirs   []string
	streamPaths []string
}

func (b *baseEntries) addEmptyDir(value string) error {
	name := basemeta.NormalizePath(value)
	if name == "." || !fs.ValidPath(name) || strings.ContainsRune(name, 0) {
		return fmt.Errorf("invalid image directory %q", value)
	}
	b.emptyDirs = append(b.emptyDirs, name)
	return nil
}

// writeBaseEntries writes directories and base metadata in parent-first order.
// Later streams take precedence. Each entry keeps its mode and ownership unless
// --file-metadata overrides them. Entries without an mtime inherit the default mtime.
func writeBaseEntries(recorder tree.Recorder, base baseEntries, createParentDirectories bool, layerMetadata *LayerMetadata) error {
	dirs := make([]*baselayer.BaseEntry, 0, len(base.emptyDirs))
	for _, name := range base.emptyDirs {
		dirs = append(dirs, basemeta.Dir(name, 0o755))
	}
	streams := [][]*baselayer.BaseEntry{dirs}
	for _, streamPath := range base.streamPaths {
		entries, err := basemeta.ReadFile(streamPath)
		if err != nil {
			return err
		}
		streams = append(streams, entries)
	}

	merged, err := basemeta.Merge(streams, basemeta.MergeOptions{
		CreateParentDirectories: createParentDirectories,
		ParentDirectoryMode:     0o755,
	})
	if err != nil {
		return err
	}

	for _, entry := range merged {
		if err := writeBaseEntry(recorder, entry, layerMetadata); err != nil {
			return fmt.Errorf("writing base entry %q: %w", entry.GetPath(), err)
		}
	}
	return nil
}

// writeBaseEntry records a single entry.
func writeBaseEntry(recorder tree.Recorder, entry *baselayer.BaseEntry, layerMetadata *LayerMetadata) error {
	header, err := basemeta.ToTarHeader(entry)
	if err != nil {
		return err
	}
	if layerMetadata != nil {
		if err := applyBaseEntryOverrides(header, entry, layerMetadata); err != nil {
			return err
		}
	}

	if entry.GetType() != baselayer.EntryType_ENTRY_TYPE_REGULAR {
		return recorder.Header(header)
	}

	switch content := entry.GetContent().(type) {
	case *baselayer.BaseEntry_Inline:
		header.Size = int64(len(content.Inline))
		return recorder.RegularFromHeader(header, bytes.NewReader(content.Inline))
	case *baselayer.BaseEntry_FilePath:
		info, err := os.Stat(content.FilePath)
		if err != nil {
			return fmt.Errorf("stat %s: %w", content.FilePath, err)
		}
		header.Size = info.Size()
		return recorder.RegularFromHeaderAndPath(header, content.FilePath)
	default:
		// A regular file with no content is an empty file, which is a normal
		// thing for a base image to contain (a marker or a placeholder).
		header.Size = 0
		return recorder.RegularFromHeader(header, bytes.NewReader(nil))
	}
}

// applyBaseEntryOverrides folds the layer's own metadata into a base entry's
// header: the default mtime fills in for an entry that has none, and a
// per-path --file-metadata override replaces whatever the entry chose.
func applyBaseEntryOverrides(header *tar.Header, entry *baselayer.BaseEntry, layerMetadata *LayerMetadata) error {
	if entry.GetMtimeUnixNanos() == 0 && layerMetadata.Defaults != nil && layerMetadata.Defaults.Mtime != nil {
		if err := applyFileMetadata(header, &FileMetadata{Mtime: layerMetadata.Defaults.Mtime}); err != nil {
			return err
		}
	}
	if override, ok := layerMetadata.FileOverrides[entry.GetPath()]; ok {
		layerMetadata.markUsed(entry.GetPath())
		if err := applyFileMetadata(header, override); err != nil {
			return err
		}
	}
	return nil
}
