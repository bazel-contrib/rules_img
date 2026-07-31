package layer

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/basemeta"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/proto/baselayer"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree"
)

// writeBaseEntries records the merged entries of a set of base metadata streams
// into the layer.
//
// The streams are read in the order given, which is the dependency order of the
// base_image_layer's srcs: for a path described more than once, the later
// stream wins. Entry metadata is authoritative -- the whole point of the rules
// producing these streams is to decide modes and ownership -- with two
// exceptions applied by the layer's own metadata provider: an entry whose mtime
// is unset picks up the layer's default mtime, and a --file-metadata override
// for the entry's path wins outright.
func writeBaseEntries(recorder tree.Recorder, streamPaths []string, createParentDirectories bool, layerMetadata *LayerMetadata) error {
	streams := make([][]*baselayer.BaseEntry, 0, len(streamPaths))
	for _, streamPath := range streamPaths {
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
