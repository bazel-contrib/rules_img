package cst

import (
	"fmt"
	"os"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/mtree"
)

// result is the outcome of a single assertion.
type result struct {
	name string
	pass bool
	msg  string // failure detail (empty on pass)
}

// unsupportedCategories returns the CST test categories present (non-empty) in st
// that cannot be validated from the image config JSON + mtree. A non-empty return
// means the whole config must be rejected with a clear error.
func unsupportedCategories(st *StructureTest) []string {
	var cats []string
	if len(st.CommandTests) > 0 {
		cats = append(cats, fmt.Sprintf("commandTests (%d): require running the container", len(st.CommandTests)))
	}
	if len(st.FileContentTests) > 0 {
		cats = append(cats, fmt.Sprintf("fileContentTests (%d): require reading file contents, which the mtree does not carry (only content digests)", len(st.FileContentTests)))
	}
	if len(st.LicenseTests) > 0 {
		cats = append(cats, fmt.Sprintf("licenseTests (%d): require scanning files inside a running container", len(st.LicenseTests)))
	}
	return cats
}

// checkMetadata validates the metadataTest assertions against the image config.
func checkMetadata(cf *v1.ConfigFile, mt MetadataTest) []result {
	var results []result
	cfg := cf.Config

	envMap := splitEnv(cfg.Env)
	for _, e := range mt.mergedEnv() {
		name := fmt.Sprintf("env %q", e.Key)
		actual, ok := envMap[e.Key]
		if !ok {
			results = append(results, result{name, false, fmt.Sprintf("env var %q not set", e.Key)})
			continue
		}
		results = append(results, matchResult(name, e.Value, actual, e.IsRegex))
	}

	for _, l := range mt.Labels {
		name := fmt.Sprintf("label %q", l.Key)
		actual, ok := cfg.Labels[l.Key]
		if !ok {
			results = append(results, result{name, false, fmt.Sprintf("label %q not set", l.Key)})
			continue
		}
		results = append(results, matchResult(name, l.Value, actual, l.IsRegex))
	}

	if mt.Entrypoint != nil {
		results = append(results, sliceResult("entrypoint", *mt.Entrypoint, cfg.Entrypoint))
	}
	if mt.Cmd != nil {
		results = append(results, sliceResult("cmd", *mt.Cmd, cfg.Cmd))
	}

	for _, p := range mt.ExposedPorts {
		results = append(results, membershipResult(fmt.Sprintf("exposedPort %q", p), p, exposedPortKeys(cfg.ExposedPorts), portAliases(p)))
	}
	for _, v := range mt.Volumes {
		results = append(results, membershipResult(fmt.Sprintf("volume %q", v), v, mapKeys(cfg.Volumes), []string{v}))
	}
	if mt.Workdir != "" {
		results = append(results, equalResult("workdir", mt.Workdir, cfg.WorkingDir))
	}
	if mt.User != "" {
		results = append(results, equalResult("user", mt.User, cfg.User))
	}
	return results
}

// checkFileExistence validates fileExistenceTests against the parsed mtree.
// entries is keyed by canonical path (no leading "./" or "/"). When the mtree is
// unavailable (nil) every test fails with a clear message.
func checkFileExistence(entries map[string]mtree.ParsedEntry, tests []FileExistenceTest) []result {
	var results []result
	for _, t := range tests {
		name := t.Name
		if name == "" {
			name = t.Path
		}
		name = "file " + name
		if entries == nil {
			results = append(results, result{name, false, "no mtree is available for this image; file existence/metadata cannot be checked"})
			continue
		}
		entry, present := entries[canonicalPath(t.Path)]
		if !t.ShouldExist {
			if present {
				results = append(results, result{name, false, fmt.Sprintf("%q exists but shouldExist is false", t.Path)})
			} else {
				results = append(results, result{name, true, ""})
			}
			continue
		}
		if !present {
			results = append(results, result{name, false, fmt.Sprintf("%q does not exist", t.Path)})
			continue
		}
		if msg := checkFileMetadata(t, entry); msg != "" {
			results = append(results, result{name, false, msg})
		} else {
			results = append(results, result{name, true, ""})
		}
	}
	return results
}

// checkFileMetadata verifies permissions/uid/gid/isExecutableBy for a present
// entry, returning a failure message or "" on success.
func checkFileMetadata(t FileExistenceTest, entry mtree.ParsedEntry) string {
	perm, hasMode, err := parseMode(entry.Keywords["mode"])
	if err != nil {
		return fmt.Sprintf("cannot parse mode %q from mtree: %v", entry.Keywords["mode"], err)
	}
	if t.Permissions != "" {
		if !hasMode {
			return "mtree entry has no mode; cannot check permissions"
		}
		got := modeString(entry.Keywords["type"], perm)
		if got != t.Permissions {
			return fmt.Sprintf("permissions %q, want %q", got, t.Permissions)
		}
	}
	if t.Uid != nil {
		got, err := strconv.ParseInt(entry.Keywords["uid"], 10, 64)
		if err != nil {
			return fmt.Sprintf("mtree entry has no/invalid uid (%q); cannot check uid", entry.Keywords["uid"])
		}
		if got != *t.Uid {
			return fmt.Sprintf("uid %d, want %d", got, *t.Uid)
		}
	}
	if t.Gid != nil {
		got, err := strconv.ParseInt(entry.Keywords["gid"], 10, 64)
		if err != nil {
			return fmt.Sprintf("mtree entry has no/invalid gid (%q); cannot check gid", entry.Keywords["gid"])
		}
		if got != *t.Gid {
			return fmt.Sprintf("gid %d, want %d", got, *t.Gid)
		}
	}
	if t.IsExecutableBy != "" {
		if !hasMode {
			return "mtree entry has no mode; cannot check isExecutableBy"
		}
		if !isExecutableBy(perm, t.IsExecutableBy) {
			return fmt.Sprintf("not executable by %q (mode %s)", t.IsExecutableBy, modeString(entry.Keywords["type"], perm))
		}
	}
	return ""
}

// --- helpers ---

func splitEnv(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			m[k] = v
		} else {
			m[e] = ""
		}
	}
	return m
}

func matchResult(name, want, actual string, isRegex bool) result {
	if isRegex {
		re, err := regexp.Compile(want)
		if err != nil {
			return result{name, false, fmt.Sprintf("invalid regex %q: %v", want, err)}
		}
		if re.MatchString(actual) {
			return result{name, true, ""}
		}
		return result{name, false, fmt.Sprintf("value %q does not match regex %q", actual, want)}
	}
	if actual == want {
		return result{name, true, ""}
	}
	return result{name, false, fmt.Sprintf("value %q, want %q", actual, want)}
}

func equalResult(name, want, actual string) result {
	if actual == want {
		return result{name, true, ""}
	}
	return result{name, false, fmt.Sprintf("%q, want %q", actual, want)}
}

func sliceResult(name string, want, actual []string) result {
	// Treat nil and empty as equal (an image with no cmd has a nil slice).
	if len(want) == 0 && len(actual) == 0 {
		return result{name, true, ""}
	}
	if reflect.DeepEqual(want, actual) {
		return result{name, true, ""}
	}
	return result{name, false, fmt.Sprintf("%v, want %v", actual, want)}
}

func membershipResult(name, want string, have []string, aliases []string) result {
	set := make(map[string]struct{}, len(have))
	for _, h := range have {
		set[h] = struct{}{}
	}
	for _, a := range aliases {
		if _, ok := set[a]; ok {
			return result{name, true, ""}
		}
	}
	return result{name, false, fmt.Sprintf("%q not found in %v", want, have)}
}

func mapKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func exposedPortKeys(m map[string]struct{}) []string { return mapKeys(m) }

// portAliases returns the forms an exposed port might take in the config: the
// value as-written plus, for a bare number, the "<n>/tcp" default.
func portAliases(p string) []string {
	if strings.Contains(p, "/") {
		return []string{p}
	}
	return []string{p, p + "/tcp"}
}

// canonicalPath normalizes a CST path (typically absolute, e.g. "/bin/sh") to the
// mtree's canonical form: no leading "/" or "./", no trailing "/".
func canonicalPath(p string) string {
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	p = path.Clean(strings.TrimSuffix(p, "/"))
	if p == "." || p == "/" {
		return "."
	}
	return p
}

// parseMode parses an mtree mode value (octal, optionally "0o"-prefixed) into its
// numeric permission bits (including setuid/setgid/sticky in the high bits).
func parseMode(s string) (int64, bool, error) {
	if s == "" {
		return 0, false, nil
	}
	s = strings.TrimPrefix(s, "0o")
	v, err := strconv.ParseInt(s, 8, 64)
	if err != nil {
		return 0, false, err
	}
	return v, true, nil
}

// modeString renders an mtree type + permission bits as an os.FileMode string
// (e.g. "-rwxr-xr-x", "drwxr-xr-x", "Lrwxrwxrwx"), matching the format
// container-structure-test compares against.
func modeString(typeKeyword string, perm int64) string {
	fm := os.FileMode(perm & 0o777)
	if perm&0o4000 != 0 {
		fm |= os.ModeSetuid
	}
	if perm&0o2000 != 0 {
		fm |= os.ModeSetgid
	}
	if perm&0o1000 != 0 {
		fm |= os.ModeSticky
	}
	switch typeKeyword {
	case "dir":
		fm |= os.ModeDir
	case "link":
		fm |= os.ModeSymlink
	case "char":
		fm |= os.ModeDevice | os.ModeCharDevice
	case "block":
		fm |= os.ModeDevice
	case "fifo":
		fm |= os.ModeNamedPipe
	}
	return fm.String()
}

// isExecutableBy reports whether perm grants execute to the given class
// ("owner", "group", "other"/"any").
func isExecutableBy(perm int64, class string) bool {
	switch strings.ToLower(class) {
	case "owner":
		return perm&0o100 != 0
	case "group":
		return perm&0o010 != 0
	case "other":
		return perm&0o001 != 0
	case "any":
		return perm&0o111 != 0
	default:
		return false
	}
}
