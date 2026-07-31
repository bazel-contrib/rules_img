package base

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/basemeta"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/proto/baselayer"
)

// releaseProcess implements `img base etc release`.
//
// os-release (os-release(5)) is a shell-compatible KEY=VALUE file. systemd
// specifies /usr/lib/os-release as the real file with /etc/os-release as a
// symlink to it, so that the identity travels with /usr; --usr-lib-symlink
// follows that convention. /etc/lsb-release is the older LSB equivalent and is
// written as a plain file.
func releaseProcess(_ context.Context, args []string) {
	osRelease := make(kvFlag)
	lsbRelease := make(kvFlag)
	var osReleaseSrcs, lsbReleaseSrcs stringsFlag
	var outputPath, producer, templatesPath string
	var osReleasePath, lsbReleasePath, usrLibPath string
	var writeLSBRelease, usrLibSymlink bool
	var mode modeFlag

	flagSet := flag.NewFlagSet("base etc release", flag.ExitOnError)
	flagSet.Var(osRelease, "os-release", "os-release entry as KEY=VALUE. Can be repeated.")
	flagSet.Var(lsbRelease, "lsb-release", "lsb-release entry as KEY=VALUE. Can be repeated.")
	flagSet.Var(&osReleaseSrcs, "os-release-src", "Path of an existing os-release file to merge. Can be repeated; later files win.")
	flagSet.Var(&lsbReleaseSrcs, "lsb-release-src", "Path of an existing lsb-release file to merge. Can be repeated; later files win.")
	flagSet.StringVar(&outputPath, "output", "", "Path of the base metadata stream to write.")
	flagSet.StringVar(&producer, "producer", "", "Label of the rule producing this stream, used in conflict messages.")
	flagSet.StringVar(&templatesPath, "templates", "", "Path of a JSON file with template-expanded values, as written by `img expand-template`.")
	flagSet.StringVar(&osReleasePath, "os-release-path", "/etc/os-release", "Path of the os-release file inside the image.")
	flagSet.StringVar(&usrLibPath, "usr-lib-path", "/usr/lib/os-release", "Path of the real os-release file when --usr-lib-symlink is set.")
	flagSet.StringVar(&lsbReleasePath, "lsb-release-path", "/etc/lsb-release", "Path of the lsb-release file inside the image.")
	flagSet.BoolVar(&writeLSBRelease, "write-lsb-release", false, "Also write /etc/lsb-release.")
	flagSet.BoolVar(&usrLibSymlink, "usr-lib-symlink", true, "Write the real file at --usr-lib-path and make --os-release-path a symlink to it.")
	flagSet.Var(&mode, "mode", "Octal file mode. Defaults to 0644.")
	if err := flagSet.Parse(args); err != nil {
		fail("etc release", err)
	}

	overrides, err := loadTemplateOverrides(templatesPath)
	if err != nil {
		fail("etc release", err)
	}

	osValues, err := mergeShellKeyValues(osReleaseSrcs, osRelease, overrides, "os_release")
	if err != nil {
		fail("etc release", err)
	}
	if len(osValues) == 0 && !writeLSBRelease {
		fail("etc release", fmt.Errorf("no os-release entries: pass --os-release KEY=VALUE or --os-release-src"))
	}

	fileMode := mode.or(0o644)
	var entries []*baselayer.BaseEntry
	if len(osValues) > 0 {
		content := renderShellKeyValues(osValues)
		if usrLibSymlink {
			entries = append(entries,
				basemeta.File(usrLibPath, fileMode, content),
				basemeta.Symlink(osReleasePath, relativeSymlinkTarget(osReleasePath, usrLibPath)),
			)
		} else {
			entries = append(entries, basemeta.File(osReleasePath, fileMode, content))
		}
	}

	if writeLSBRelease {
		lsbValues, err := mergeShellKeyValues(lsbReleaseSrcs, lsbRelease, overrides, "lsb_release")
		if err != nil {
			fail("etc release", err)
		}
		entries = append(entries, basemeta.File(lsbReleasePath, fileMode, renderShellKeyValues(lsbValues)))
	}

	if err := writeStream(outputPath, producer, entries); err != nil {
		fail("etc release", err)
	}
}

// mergeShellKeyValues folds source files and declared values into one map,
// with declared values (after template expansion) taking precedence.
func mergeShellKeyValues(srcs []string, declared kvFlag, overrides templateOverrides, templateKey string) (map[string]string, error) {
	merged := make(map[string]string)
	for _, src := range srcs {
		fromFile, err := parseShellKeyValueFile(src)
		if err != nil {
			return nil, err
		}
		for k, v := range fromFile {
			merged[k] = v
		}
	}
	expanded, err := overrides.stringMap(templateKey, declared)
	if err != nil {
		return nil, err
	}
	for k, v := range expanded {
		merged[k] = v
	}
	return merged, nil
}

// renderShellKeyValues writes KEY="value" lines in sorted key order. Values are
// always quoted: os-release(5) allows unquoted values only for a restricted
// character set, and quoting everything is always valid.
func renderShellKeyValues(values map[string]string) []byte {
	var buf bytes.Buffer
	for _, key := range sortedKeys(values) {
		buf.WriteString(key)
		buf.WriteByte('=')
		buf.WriteString(shellQuote(values[key]))
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// shellQuote renders a value as a double-quoted POSIX shell string.
func shellQuote(value string) string {
	var buf strings.Builder
	buf.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"', '\\', '$', '`':
			buf.WriteByte('\\')
		}
		buf.WriteRune(r)
	}
	buf.WriteByte('"')
	return buf.String()
}

// parseShellKeyValueFile reads an existing os-release or lsb-release file.
func parseShellKeyValueFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening release file: %w", err)
	}
	defer f.Close()

	values := make(map[string]string)
	scanner := bufio.NewScanner(f)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE, got %q", path, lineNumber, line)
		}
		values[strings.TrimSpace(key)] = shellUnquote(strings.TrimSpace(value))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return values, nil
}

// shellUnquote removes one layer of quoting and resolves the backslash escapes
// that os-release(5) permits inside a double-quoted value.
func shellUnquote(value string) string {
	if len(value) < 2 {
		return value
	}
	first, last := value[0], value[len(value)-1]
	if first != last || (first != '"' && first != '\'') {
		return value
	}
	inner := value[1 : len(value)-1]
	if first == '\'' {
		// Single quotes are literal in POSIX shell.
		return inner
	}
	var buf strings.Builder
	for i := 0; i < len(inner); i++ {
		if inner[i] == '\\' && i+1 < len(inner) {
			switch inner[i+1] {
			case '"', '\\', '$', '`':
				i++
			}
		}
		buf.WriteByte(inner[i])
	}
	return buf.String()
}

// relativeSymlinkTarget builds the target of a symlink at linkPath pointing at
// targetPath. Both are absolute image paths; the result is relative so the link
// keeps working when the image tree is mounted somewhere else.
func relativeSymlinkTarget(linkPath, targetPath string) string {
	linkParts := strings.Split(strings.Trim(basemeta.NormalizePath(linkPath), "/"), "/")
	targetParts := strings.Split(strings.Trim(basemeta.NormalizePath(targetPath), "/"), "/")

	common := 0
	for common < len(linkParts)-1 && common < len(targetParts)-1 && linkParts[common] == targetParts[common] {
		common++
	}

	var parts []string
	for range linkParts[common : len(linkParts)-1] {
		parts = append(parts, "..")
	}
	parts = append(parts, targetParts[common:]...)
	return strings.Join(parts, "/")
}
