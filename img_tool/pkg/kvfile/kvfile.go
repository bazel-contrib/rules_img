// Package kvfile parses key/value data — such as image labels, annotations,
// and environment variables — that may be supplied either as JSON or as
// newline-delimited text.
//
// The canonical internal representation is map[string][]string: each key maps
// to the list of values supplied for it. Callers that need a flat
// map[string]string (labels, annotations) use [Flatten]; callers that need an
// OCI-style "KEY=VALUE" list (environment variables) use [ToEnv].
//
// # Recognized formats
//
// The format is auto-detected from the content. If the first non-whitespace
// byte is '{' or '[' the data is parsed as JSON, otherwise it falls back to
// newline-delimited text:
//
//	{"key": "value"}                 // JSON object, string values
//	{"key": ["value1", "value2"]}    // JSON object, array values
//	["key=value", "other=value"]     // JSON array of "KEY=VALUE" strings
//	key=value                        // newline-delimited "KEY=VALUE" text
//
// JSON object values are preserved verbatim, so they can encode arbitrary
// strings including values that contain '=' or newline characters. The
// "KEY=VALUE" forms (JSON array and newline-delimited text) split on the first
// '=' and trim surrounding whitespace from both the key and the value.
package kvfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// ErrNotKeyValue is returned by [Parse] and [ParseFile] when the input is a
// non-empty plain list of strings (no "KEY=VALUE" pairs) rather than key/value
// data.
var ErrNotKeyValue = errors.New("kvfile: data is a plain list, not KEY=VALUE pairs")

// Kind classifies how a file's contents were interpreted by [ParseFlexible].
type Kind int

const (
	// KindEmpty means the input contained no meaningful entries.
	KindEmpty Kind = iota
	// KindKeyValue means the input was parsed as KEY=VALUE pairs (Pairs is set).
	KindKeyValue
	// KindList means the input was parsed as a plain list of strings (List is set).
	KindList
)

// Parsed is the result of [ParseFlexible]. Exactly one of Pairs or List carries
// data, selected by Kind (neither is set when Kind is KindEmpty).
type Parsed struct {
	Kind  Kind
	Pairs map[string][]string
	List  []string
}

// stringOrSlice unmarshals from either a single JSON string or an array of
// strings, normalizing both into a slice.
type stringOrSlice []string

func (s *stringOrSlice) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*s = []string{single}
		return nil
	}
	var arr []string
	if err := json.Unmarshal(data, &arr); err != nil {
		return fmt.Errorf("expected string or array of strings, got: %s", string(data))
	}
	*s = arr
	return nil
}

// ParseFlexible parses data that may hold either KEY=VALUE pairs or a plain
// list of strings, encoded as JSON or newline-delimited text. It is used by
// callers (such as template expansion) that accept both list-valued keys (e.g.
// image tags) and key/value-valued keys (e.g. labels and annotations).
//
// Most callers want the simpler [Parse] instead.
func ParseFlexible(data []byte) (Parsed, error) {
	// Strip a leading UTF-8 byte-order mark so BOM-prefixed files are still
	// detected as JSON and don't corrupt the first key in the text form.
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return Parsed{Kind: KindEmpty}, nil
	}

	switch trimmed[0] {
	case '{':
		// JSON object: {"key": "value"} or {"key": ["v1", "v2"]}.
		var obj map[string]stringOrSlice
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return Parsed{}, fmt.Errorf("parsing JSON object: %w", err)
		}
		pairs := make(map[string][]string, len(obj))
		for k, v := range obj {
			if k == "" {
				return Parsed{}, errors.New("empty key in JSON object")
			}
			pairs[k] = []string(v)
		}
		return Parsed{Kind: KindKeyValue, Pairs: pairs}, nil
	case '[':
		// JSON array of strings: ["key=value", ...] or ["item", ...].
		var arr []string
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return Parsed{}, fmt.Errorf("parsing JSON array: %w", err)
		}
		// Trim entries and drop empty ones, mirroring the text form where
		// blank lines are skipped and surrounding whitespace is trimmed.
		items := make([]string, 0, len(arr))
		for _, item := range arr {
			if item = strings.TrimSpace(item); item != "" {
				items = append(items, item)
			}
		}
		return classify(items)
	default:
		// Newline-delimited text.
		return classify(textLines(trimmed))
	}
}

// Parse parses KEY=VALUE data (JSON or newline-delimited text) and returns the
// values keyed by name. Empty input yields an empty map. A non-empty plain list
// of strings (no "=") returns [ErrNotKeyValue].
func Parse(data []byte) (map[string][]string, error) {
	parsed, err := ParseFlexible(data)
	if err != nil {
		return nil, err
	}
	switch parsed.Kind {
	case KindKeyValue:
		return parsed.Pairs, nil
	case KindEmpty:
		return map[string][]string{}, nil
	default:
		return nil, ErrNotKeyValue
	}
}

// ParseFile reads the file at path and parses it with [Parse].
func ParseFile(path string) (map[string][]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return Parse(data)
}

// Flatten collapses key/value data into a map[string]string, keeping the last
// value supplied for each key. Keys with no values are dropped. This is the
// representation used for image labels and annotations, where duplicate keys
// overwrite each other.
func Flatten(pairs map[string][]string) map[string]string {
	out := make(map[string]string, len(pairs))
	for k, values := range pairs {
		if len(values) == 0 {
			continue
		}
		out[k] = values[len(values)-1]
	}
	return out
}

// ToEnv converts key/value data into a sorted slice of "KEY=VALUE" entries, one
// per key (keeping the last value for each key, like [Flatten]). This is the
// representation used for container environment variables.
func ToEnv(pairs map[string][]string) []string {
	flat := Flatten(pairs)
	keys := make([]string, 0, len(flat))
	for k := range flat {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k + "=" + flat[k]
	}
	return out
}

// textLines splits newline-delimited text into trimmed, non-empty lines,
// skipping blank lines and '#'-prefixed comments.
func textLines(data []byte) []string {
	var lines []string
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

// classify interprets a list of strings as either KEY=VALUE pairs or a plain
// list. Every entry must agree: if any entry contains '=' they all must, and
// vice versa. The "KEY=VALUE" entries are split on the first '=' with both
// sides trimmed.
func classify(items []string) (Parsed, error) {
	if len(items) == 0 {
		return Parsed{Kind: KindEmpty}, nil
	}

	hasKV, hasPlain := false, false
	for _, item := range items {
		if strings.Contains(item, "=") {
			hasKV = true
		} else {
			hasPlain = true
		}
	}
	if hasKV && hasPlain {
		return Parsed{}, errors.New("mixed KEY=VALUE pairs and plain entries")
	}
	if hasPlain {
		return Parsed{Kind: KindList, List: items}, nil
	}

	pairs := make(map[string][]string)
	for _, item := range items {
		key, value, _ := strings.Cut(item, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return Parsed{}, fmt.Errorf("empty key in entry %q", item)
		}
		pairs[key] = append(pairs[key], value)
	}
	return Parsed{Kind: KindKeyValue, Pairs: pairs}, nil
}
