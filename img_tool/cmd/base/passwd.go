package base

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/basemeta"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/proto/baselayer"
)

// user is one /etc/passwd record. The JSON field names match the passwd_entry()
// Starlark helper.
type user struct {
	Username string `json:"username"`
	UID      int64  `json:"uid"`
	GID      int64  `json:"gid"`
	Gecos    string `json:"gecos,omitempty"`
	Home     string `json:"home,omitempty"`
	Shell    string `json:"shell,omitempty"`
	// Password is the placeholder in the second passwd field. "x" (the default)
	// means "look in /etc/shadow".
	Password string `json:"password,omitempty"`
}

// group is one /etc/group record, matching the group_entry() Starlark helper.
type group struct {
	Name    string   `json:"name"`
	GID     int64    `json:"gid"`
	Users   []string `json:"users,omitempty"`
	Passwrd string   `json:"password,omitempty"`
}

// passwdProcess implements `img base etc passwd`.
//
// It writes /etc/passwd, /etc/group and (optionally) /etc/shadow, plus a
// directory entry for each user's home. Shadow entries are always locked
// ("!*"): this tool never accepts or writes a password hash, because anything
// it wrote would be readable in the image and in the Bazel cache.
func passwdProcess(_ context.Context, args []string) {
	var userJSON, groupJSON stringsFlag
	var passwdSrcs, groupSrcs, shadowSrcs stringsFlag
	var outputPath, producer, templatesPath string
	var passwdPath, groupPath, shadowPath string
	var createHomeDirectories, writeShadow bool
	var homeMode, passwdMode, shadowMode modeFlag

	flagSet := flag.NewFlagSet("base etc passwd", flag.ExitOnError)
	flagSet.Var(&userJSON, "user", "A user as a JSON object. Can be repeated.")
	flagSet.Var(&groupJSON, "group", "A group as a JSON object. Can be repeated.")
	flagSet.Var(&passwdSrcs, "passwd-src", "Path of an existing passwd file to merge. Can be repeated.")
	flagSet.Var(&groupSrcs, "group-src", "Path of an existing group file to merge. Can be repeated.")
	flagSet.Var(&shadowSrcs, "shadow-src", "Path of an existing shadow file to merge. Can be repeated.")
	flagSet.StringVar(&outputPath, "output", "", "Path of the base metadata stream to write.")
	flagSet.StringVar(&producer, "producer", "", "Label of the rule producing this stream, used in conflict messages.")
	flagSet.StringVar(&templatesPath, "templates", "", "Path of a JSON file with template-expanded values, as written by `img expand-template`.")
	flagSet.StringVar(&passwdPath, "passwd-path", "/etc/passwd", "Path of the passwd file inside the image.")
	flagSet.StringVar(&groupPath, "group-path", "/etc/group", "Path of the group file inside the image.")
	flagSet.StringVar(&shadowPath, "shadow-path", "/etc/shadow", "Path of the shadow file inside the image.")
	flagSet.BoolVar(&createHomeDirectories, "create-home-directories", true, "Create a directory entry for each user's home directory.")
	flagSet.BoolVar(&writeShadow, "write-shadow", true, "Write /etc/shadow with locked entries.")
	flagSet.Var(&homeMode, "home-mode", "Octal mode of created home directories. Defaults to 0750.")
	flagSet.Var(&passwdMode, "mode", "Octal mode of passwd and group. Defaults to 0644.")
	flagSet.Var(&shadowMode, "shadow-mode", "Octal mode of shadow. Defaults to 0640.")
	if err := flagSet.Parse(args); err != nil {
		fail("etc passwd", err)
	}

	overrides, err := loadTemplateOverrides(templatesPath)
	if err != nil {
		fail("etc passwd", err)
	}
	expandedUsers, err := overrides.stringList("users", userJSON)
	if err != nil {
		fail("etc passwd", err)
	}
	expandedGroups, err := overrides.stringList("groups", groupJSON)
	if err != nil {
		fail("etc passwd", err)
	}

	users, err := collectUsers(passwdSrcs, expandedUsers)
	if err != nil {
		fail("etc passwd", err)
	}
	groups, err := collectGroups(groupSrcs, expandedGroups)
	if err != nil {
		fail("etc passwd", err)
	}

	fileMode := passwdMode.or(0o644)
	entries := []*baselayer.BaseEntry{
		basemeta.File(passwdPath, fileMode, renderPasswd(users)),
		basemeta.File(groupPath, fileMode, renderGroup(groups)),
	}

	if writeShadow {
		locked, err := collectShadow(shadowSrcs, users)
		if err != nil {
			fail("etc passwd", err)
		}
		// 0640 root:shadow is the Debian convention; the shadow group does not
		// necessarily exist in a minimal base image, so only the mode is set.
		entries = append(entries, basemeta.File(shadowPath, shadowMode.or(0o640), locked))
	}

	if createHomeDirectories {
		entries = append(entries, homeDirectoryEntries(users, homeMode.or(0o750))...)
	}

	if err := writeStream(outputPath, producer, entries); err != nil {
		fail("etc passwd", err)
	}
}

// collectUsers merges existing passwd files with declared users, rejecting
// duplicate names and UIDs.
func collectUsers(srcs []string, declared []string) ([]user, error) {
	var users []user
	for _, src := range srcs {
		parsed, err := parsePasswdFile(src)
		if err != nil {
			return nil, err
		}
		users = append(users, parsed...)
	}
	for _, raw := range declared {
		var u user
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			return nil, fmt.Errorf("parsing user %s: %w", raw, err)
		}
		if u.Username == "" {
			return nil, fmt.Errorf("user %s has no username", raw)
		}
		users = append(users, u)
	}

	byName := make(map[string]bool, len(users))
	byUID := make(map[int64]string, len(users))
	for i := range users {
		u := &users[i]
		if byName[u.Username] {
			return nil, fmt.Errorf("duplicate user %q", u.Username)
		}
		byName[u.Username] = true
		if other, taken := byUID[u.UID]; taken {
			return nil, fmt.Errorf("users %q and %q both use uid %d", other, u.Username, u.UID)
		}
		byUID[u.UID] = u.Username

		if u.Password == "" {
			u.Password = "x"
		}
		if u.Shell == "" {
			u.Shell = "/sbin/nologin"
		}
		if u.Home == "" {
			u.Home = "/"
		}
	}

	sort.Slice(users, func(i, j int) bool { return users[i].UID < users[j].UID })
	return users, nil
}

// collectGroups merges existing group files with declared groups, rejecting
// duplicate names and GIDs.
func collectGroups(srcs []string, declared []string) ([]group, error) {
	var groups []group
	for _, src := range srcs {
		parsed, err := parseGroupFile(src)
		if err != nil {
			return nil, err
		}
		groups = append(groups, parsed...)
	}
	for _, raw := range declared {
		var g group
		if err := json.Unmarshal([]byte(raw), &g); err != nil {
			return nil, fmt.Errorf("parsing group %s: %w", raw, err)
		}
		if g.Name == "" {
			return nil, fmt.Errorf("group %s has no name", raw)
		}
		groups = append(groups, g)
	}

	byName := make(map[string]bool, len(groups))
	byGID := make(map[int64]string, len(groups))
	for i := range groups {
		g := &groups[i]
		if byName[g.Name] {
			return nil, fmt.Errorf("duplicate group %q", g.Name)
		}
		byName[g.Name] = true
		if other, taken := byGID[g.GID]; taken {
			return nil, fmt.Errorf("groups %q and %q both use gid %d", other, g.Name, g.GID)
		}
		byGID[g.GID] = g.Name

		if g.Passwrd == "" {
			g.Passwrd = "x"
		}
	}

	sort.Slice(groups, func(i, j int) bool { return groups[i].GID < groups[j].GID })
	return groups, nil
}

// collectShadow renders /etc/shadow: entries carried over from source files,
// plus a locked entry for every user that has none.
func collectShadow(srcs []string, users []user) ([]byte, error) {
	existing := make(map[string]string)
	for _, src := range srcs {
		lines, err := parseShadowFile(src)
		if err != nil {
			return nil, err
		}
		for name, line := range lines {
			existing[name] = line
		}
	}

	var buf bytes.Buffer
	for _, u := range users {
		if line, ok := existing[u.Username]; ok {
			buf.WriteString(line)
			buf.WriteByte('\n')
			continue
		}
		// "!*" is a locked account with no password set: no value can hash to
		// it, so the account can only be entered by switching to it as root.
		// The remaining fields (last change, min/max age, warn, inactive,
		// expire) are left empty, which means "no ageing policy".
		buf.WriteString(u.Username)
		buf.WriteString(":!*:::::::\n")
	}
	return buf.Bytes(), nil
}

// homeDirectoryEntries builds a directory entry for each distinct home
// directory, owned by the user it belongs to.
func homeDirectoryEntries(users []user, mode int64) []*baselayer.BaseEntry {
	var entries []*baselayer.BaseEntry
	seen := make(map[string]bool)
	for _, u := range users {
		home := basemeta.NormalizePath(u.Home)
		// Skip users whose home is the root of the image (the convention for
		// system accounts that have none) and duplicates such as several
		// daemons sharing /nonexistent.
		if home == "" || seen[home] {
			continue
		}
		seen[home] = true
		// root's home is private by convention, regardless of the default.
		homeMode := mode
		if u.UID == 0 {
			homeMode = 0o700
		}
		entries = append(entries, basemeta.WithOwner(basemeta.Dir(home, homeMode), u.UID, u.GID, u.Username, ""))
	}
	return entries
}

func renderPasswd(users []user) []byte {
	var buf bytes.Buffer
	for _, u := range users {
		fmt.Fprintf(&buf, "%s:%s:%d:%d:%s:%s:%s\n", u.Username, u.Password, u.UID, u.GID, u.Gecos, u.Home, u.Shell)
	}
	return buf.Bytes()
}

func renderGroup(groups []group) []byte {
	var buf bytes.Buffer
	for _, g := range groups {
		fmt.Fprintf(&buf, "%s:%s:%d:%s\n", g.Name, g.Passwrd, g.GID, strings.Join(g.Users, ","))
	}
	return buf.Bytes()
}

func parsePasswdFile(path string) ([]user, error) {
	var users []user
	err := forEachColonRecord(path, 7, func(lineNumber int, fields []string) error {
		uid, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return fmt.Errorf("line %d: invalid uid %q: %w", lineNumber, fields[2], err)
		}
		gid, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return fmt.Errorf("line %d: invalid gid %q: %w", lineNumber, fields[3], err)
		}
		users = append(users, user{
			Username: fields[0],
			Password: fields[1],
			UID:      uid,
			GID:      gid,
			Gecos:    fields[4],
			Home:     fields[5],
			Shell:    fields[6],
		})
		return nil
	})
	return users, err
}

func parseGroupFile(path string) ([]group, error) {
	var groups []group
	err := forEachColonRecord(path, 4, func(lineNumber int, fields []string) error {
		gid, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return fmt.Errorf("line %d: invalid gid %q: %w", lineNumber, fields[2], err)
		}
		var members []string
		if fields[3] != "" {
			members = strings.Split(fields[3], ",")
		}
		groups = append(groups, group{
			Name:    fields[0],
			Passwrd: fields[1],
			GID:     gid,
			Users:   members,
		})
		return nil
	})
	return groups, err
}

// parseShadowFile returns the verbatim line of each shadow record, keyed by
// username. Shadow entries are carried over untouched rather than re-rendered:
// the ageing fields are meaningful and this tool has no opinion on them.
func parseShadowFile(path string) (map[string]string, error) {
	lines := make(map[string]string)
	err := forEachColonRecord(path, 9, func(_ int, fields []string) error {
		lines[fields[0]] = strings.Join(fields, ":")
		return nil
	})
	return lines, err
}

// forEachColonRecord parses a colon-separated database file, calling fn for
// each record. Records with fewer than minFields fields are an error; extra
// fields are preserved and passed through.
func forEachColonRecord(path string, minFields int, fn func(lineNumber int, fields []string) error) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < minFields {
			return fmt.Errorf("%s:%d: expected at least %d colon-separated fields, got %d", path, lineNumber, minFields, len(fields))
		}
		if err := fn(lineNumber, fields); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	return nil
}
