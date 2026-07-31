package base

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"path"
	"strconv"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/basemeta"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/basemeta/elfinfo"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/basemeta/ldcache"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/proto/baselayer"
)

// systemLibrariesProcess implements `img base system-libraries`.
//
// Each --library flag names a shared object on the host together with the file
// name it should have in the image. The library is placed in the configured
// library directory, its DT_SONAME becomes a symlink next to it (which is how a
// dynamic linker finds it), and an /etc/ld.so.conf.d fragment records the
// directory.
func systemLibrariesProcess(_ context.Context, args []string) {
	libraries := make(kvFlag)
	var outputPath, producer, libDir, confPath, cachePath, byteOrderName string
	var writeConf, writeCache bool
	var defaultMetadataJSON string
	fileMetadata := make(kvFlag)
	var mode modeFlag

	flagSet := flag.NewFlagSet("base system-libraries", flag.ExitOnError)
	flagSet.Var(libraries, "library", "A shared library as NAME=<host path>. Can be repeated.")
	flagSet.StringVar(&outputPath, "output", "", "Path of the base metadata stream to write.")
	flagSet.StringVar(&producer, "producer", "", "Label of the rule producing this stream, used in conflict messages.")
	flagSet.StringVar(&libDir, "lib-dir", "/usr/lib", "Directory the libraries are placed in.")
	flagSet.BoolVar(&writeConf, "ld-so-conf", true, "Write an /etc/ld.so.conf.d fragment naming the library directory.")
	flagSet.StringVar(&confPath, "ld-so-conf-path", "/etc/ld.so.conf.d/00-rules-img.conf", "Path of the ld.so.conf fragment.")
	flagSet.BoolVar(&writeCache, "ld-so-cache", false, "Write a prebuilt /etc/ld.so.cache. glibc only.")
	flagSet.StringVar(&cachePath, "ld-so-cache-path", "/etc/ld.so.cache", "Path of the loader cache.")
	flagSet.StringVar(&byteOrderName, "byte-order", "little", `Byte order of the target, "little" or "big". Only used for --ld-so-cache.`)
	flagSet.StringVar(&defaultMetadataJSON, "default-metadata", "", "JSON file metadata applied to every library.")
	flagSet.Var(fileMetadata, "file-metadata", "Per-file metadata as PATH=<JSON>. Can be repeated.")
	flagSet.Var(&mode, "mode", "Octal mode of the placed libraries. Defaults to 0755.")
	if err := flagSet.Parse(args); err != nil {
		fail("system-libraries", err)
	}

	var byteOrder binary.ByteOrder
	switch byteOrderName {
	case "little":
		byteOrder = binary.LittleEndian
	case "big":
		byteOrder = binary.BigEndian
	default:
		fail("system-libraries", fmt.Errorf("--byte-order must be %q or %q, got %q", "little", "big", byteOrderName))
	}

	defaults, err := parseFileMetadata(defaultMetadataJSON)
	if err != nil {
		fail("system-libraries", fmt.Errorf("parsing --default-metadata: %w", err))
	}

	var entries []*baselayer.BaseEntry
	var cacheEntries []ldcache.Entry
	// A SONAME shared by several libraries would produce two symlinks at the
	// same path, which the merge would reject with a confusing message; catch
	// it here where the file names are still at hand.
	sonameOwners := make(map[string]string)

	for _, name := range libraries.keys() {
		hostPath := libraries[name]
		info, err := elfinfo.Read(hostPath)
		if err != nil {
			fail("system-libraries", err)
		}

		imagePath := path.Join(libDir, name)
		entry := basemeta.FileFromPath(imagePath, mode.or(0o755), hostPath)
		if err := applyFileMetadata(entry, defaults); err != nil {
			fail("system-libraries", fmt.Errorf("applying --default-metadata: %w", err))
		}
		if raw, ok := fileMetadata[imagePath]; ok {
			override, err := parseFileMetadata(raw)
			if err != nil {
				fail("system-libraries", fmt.Errorf("parsing metadata for %s: %w", imagePath, err))
			}
			if err := applyFileMetadata(entry, override); err != nil {
				fail("system-libraries", fmt.Errorf("applying metadata for %s: %w", imagePath, err))
			}
		}
		entries = append(entries, entry)

		// The dynamic linker looks a dependency up by its SONAME, which is
		// often a shorter, version-stable name than the real file
		// ("libssl.so.3" next to "libssl.so.3.0.14"). Link one to the other so
		// both resolve.
		soname := info.SONAME
		if soname == "" || soname == name {
			cacheEntries = append(cacheEntries, ldcache.Entry{SONAME: name, Path: imagePath})
			continue
		}
		if owner, taken := sonameOwners[soname]; taken {
			fail("system-libraries", fmt.Errorf("libraries %s and %s both declare SONAME %q", owner, name, soname))
		}
		sonameOwners[soname] = name

		entries = append(entries, basemeta.Symlink(path.Join(libDir, soname), name))
		cacheEntries = append(cacheEntries, ldcache.Entry{SONAME: soname, Path: path.Join(libDir, soname)})
	}

	if len(entries) == 0 {
		fail("system-libraries", fmt.Errorf("no libraries given: pass --library NAME=PATH"))
	}

	if writeConf {
		entries = append(entries, basemeta.File(confPath, 0o644, ldcache.ConfContent([]string{libDir})))
	}
	if writeCache {
		entries = append(entries, basemeta.File(cachePath, 0o644, ldcache.Write(cacheEntries, byteOrder)))
	}

	if err := writeStream(outputPath, producer, entries); err != nil {
		fail("system-libraries", err)
	}
}

// parseFileMetadata decodes the JSON that the file_metadata() Starlark helper
// produces. An empty string means "no metadata".
func parseFileMetadata(raw string) (*fileMetadataJSON, error) {
	if raw == "" || raw == "{}" {
		return nil, nil
	}
	var metadata fileMetadataJSON
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return nil, err
	}
	return &metadata, nil
}

// applyFileMetadata overlays the set fields of metadata onto an entry.
func applyFileMetadata(entry *baselayer.BaseEntry, metadata *fileMetadataJSON) error {
	if metadata == nil {
		return nil
	}
	if metadata.Mode != nil {
		parsed, err := strconv.ParseInt(*metadata.Mode, 8, 64)
		if err != nil {
			return fmt.Errorf("invalid octal mode %q: %w", *metadata.Mode, err)
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
	return nil
}
