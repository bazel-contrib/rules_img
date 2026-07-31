package base

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/basemeta"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/proto/baselayer"
)

// stringsFlag collects a repeatable string flag in the order given.
type stringsFlag []string

func (f *stringsFlag) String() string { return strings.Join(*f, ",") }

func (f *stringsFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

// kvFlag collects repeatable KEY=VALUE flags. Later values for the same key
// replace earlier ones, matching how Bazel string_dict attributes behave.
type kvFlag map[string]string

func (f kvFlag) String() string {
	pairs := make([]string, 0, len(f))
	for k, v := range f {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

func (f kvFlag) Set(value string) error {
	key, val, found := strings.Cut(value, "=")
	if !found {
		return fmt.Errorf("expected KEY=VALUE, got %q", value)
	}
	f[key] = val
	return nil
}

// keys returns the map's keys in sorted order, for deterministic output.
func (f kvFlag) keys() []string {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// modeFlag parses an octal file mode such as "0755".
type modeFlag struct {
	value int64
	set   bool
}

func (f *modeFlag) String() string {
	if !f.set {
		return ""
	}
	return "0" + strconv.FormatInt(f.value, 8)
}

func (f *modeFlag) Set(value string) error {
	mode, err := strconv.ParseInt(value, 8, 64)
	if err != nil {
		return fmt.Errorf("invalid octal mode %q: %w", value, err)
	}
	f.value = mode
	f.set = true
	return nil
}

// or returns the parsed mode, or fallback when the flag was not set.
func (f *modeFlag) or(fallback int64) int64 {
	if !f.set {
		return fallback
	}
	return f.value
}

// templateOverrides holds values that were expanded by `img expand-template`
// before the action ran. Rules that support templating write the expanded
// values to a JSON file and pass it via --templates; the verb then prefers
// those over the values given on the command line.
//
// The file's shape matches what expand-template emits: a JSON object whose
// values are strings, string lists, or string maps.
type templateOverrides map[string]json.RawMessage

func loadTemplateOverrides(path string) (templateOverrides, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading templates file: %w", err)
	}
	var overrides templateOverrides
	if err := json.Unmarshal(data, &overrides); err != nil {
		return nil, fmt.Errorf("parsing templates file %s: %w", path, err)
	}
	return overrides, nil
}

// stringMap returns the expanded value of a string-map template, or fallback
// when the template was not expanded.
func (t templateOverrides) stringMap(key string, fallback map[string]string) (map[string]string, error) {
	raw, ok := t[key]
	if !ok {
		return fallback, nil
	}
	var expanded map[string]string
	if err := json.Unmarshal(raw, &expanded); err != nil {
		return nil, fmt.Errorf("template value %q is not a string map: %w", key, err)
	}
	return expanded, nil
}

// stringList returns the expanded value of a string-list template, or fallback
// when the template was not expanded.
func (t templateOverrides) stringList(key string, fallback []string) ([]string, error) {
	raw, ok := t[key]
	if !ok {
		return fallback, nil
	}
	var expanded []string
	if err := json.Unmarshal(raw, &expanded); err != nil {
		return nil, fmt.Errorf("template value %q is not a string list: %w", key, err)
	}
	return expanded, nil
}

// basemetaFile wraps a single inline file entry in the slice shape that
// writeStream takes, for the verbs that describe exactly one file.
func basemetaFile(imagePath string, mode int64, content []byte) []*baselayer.BaseEntry {
	return []*baselayer.BaseEntry{basemeta.File(imagePath, mode, content)}
}

// sortedKeys returns a map's keys in sorted order, so rendered files are
// byte-stable across builds.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// writeStream writes entries to the output path as a base metadata stream,
// tagging each with the producing rule's label.
func writeStream(outputPath, producer string, entries []*baselayer.BaseEntry) error {
	if outputPath == "" {
		return fmt.Errorf("--output is required")
	}
	writer, closer, err := basemeta.Create(outputPath)
	if err != nil {
		return err
	}
	if err := writer.WriteAll(basemeta.WithProducer(entries, producer)); err != nil {
		closer.Close()
		return err
	}
	return closer.Close()
}

// fail prints an error and exits. Every verb funnels its errors through here so
// they look the same and always carry the subcommand name.
func fail(verb string, err error) {
	fmt.Fprintf(os.Stderr, "img base %s: %v\n", verb, err)
	os.Exit(1)
}
