package base

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
)

// defaultHosts is the loopback block a stock /etc/hosts carries. Debian and its
// derivatives ship exactly these entries in netbase.
var defaultHosts = []hostEntry{
	{address: "127.0.0.1", names: []string{"localhost"}},
	{address: "::1", names: []string{"localhost", "ip6-localhost", "ip6-loopback"}},
	{address: "fe00::0", names: []string{"ip6-localnet"}},
	{address: "ff00::0", names: []string{"ip6-mcastprefix"}},
	{address: "ff02::1", names: []string{"ip6-allnodes"}},
	{address: "ff02::2", names: []string{"ip6-allrouters"}},
}

// hostEntry is one line of /etc/hosts: an address and the names it resolves to.
type hostEntry struct {
	address string
	names   []string
}

// hostsProcess implements `img base etc hosts`.
func hostsProcess(_ context.Context, args []string) {
	hosts := make(kvFlag)
	var srcs stringsFlag
	var outputPath, producer, templatesPath, imagePath string
	var includeDefaults bool
	var mode modeFlag

	flagSet := flag.NewFlagSet("base etc hosts", flag.ExitOnError)
	flagSet.Var(hosts, "host", "Host mapping as ADDRESS=NAME[ NAME...]. Can be repeated.")
	flagSet.Var(&srcs, "src", "Path of an existing hosts file to merge. Can be repeated.")
	flagSet.StringVar(&outputPath, "output", "", "Path of the base metadata stream to write.")
	flagSet.StringVar(&producer, "producer", "", "Label of the rule producing this stream, used in conflict messages.")
	flagSet.StringVar(&templatesPath, "templates", "", "Path of a JSON file with template-expanded values, as written by `img expand-template`.")
	flagSet.StringVar(&imagePath, "path", "/etc/hosts", "Path of the file inside the image.")
	flagSet.BoolVar(&includeDefaults, "include-defaults", true, "Include the standard loopback entries.")
	flagSet.Var(&mode, "mode", "Octal file mode. Defaults to 0644.")
	if err := flagSet.Parse(args); err != nil {
		fail("etc hosts", err)
	}

	// Names accumulate per address rather than replacing each other: several
	// sources legitimately add names to 127.0.0.1.
	byAddress := make(map[string][]string)
	var order []string
	add := func(address string, names []string) error {
		if net.ParseIP(address) == nil {
			return fmt.Errorf("%q is not a valid IP address", address)
		}
		if _, seen := byAddress[address]; !seen {
			order = append(order, address)
		}
		byAddress[address] = appendNewNames(byAddress[address], names)
		return nil
	}

	if includeDefaults {
		for _, entry := range defaultHosts {
			if err := add(entry.address, entry.names); err != nil {
				fail("etc hosts", err)
			}
		}
	}
	for _, src := range srcs {
		fromFile, err := parseHostsFile(src)
		if err != nil {
			fail("etc hosts", err)
		}
		for _, entry := range fromFile {
			if err := add(entry.address, entry.names); err != nil {
				fail("etc hosts", fmt.Errorf("%s: %w", src, err))
			}
		}
	}

	overrides, err := loadTemplateOverrides(templatesPath)
	if err != nil {
		fail("etc hosts", err)
	}
	declared, err := overrides.stringMap("hosts", hosts)
	if err != nil {
		fail("etc hosts", err)
	}
	// Iterate the declared mappings in sorted order: Bazel string_dicts have no
	// meaningful order of their own, and the output has to be deterministic.
	declaredAddresses := make([]string, 0, len(declared))
	for address := range declared {
		declaredAddresses = append(declaredAddresses, address)
	}
	sort.Strings(declaredAddresses)
	for _, address := range declaredAddresses {
		if err := add(address, strings.Fields(declared[address])); err != nil {
			fail("etc hosts", err)
		}
	}

	entries := make([]hostEntry, 0, len(order))
	for _, address := range order {
		entries = append(entries, hostEntry{address: address, names: byAddress[address]})
	}

	if err := writeStream(outputPath, producer, basemetaFile(imagePath, mode.or(0o644), renderHosts(entries))); err != nil {
		fail("etc hosts", err)
	}
}

// appendNewNames appends the names that are not already present, preserving the
// order in which they were first seen.
func appendNewNames(existing, names []string) []string {
	for _, name := range names {
		found := false
		for _, have := range existing {
			if have == name {
				found = true
				break
			}
		}
		if !found {
			existing = append(existing, name)
		}
	}
	return existing
}

// renderHosts writes the entries as aligned "address<tab>name..." lines.
func renderHosts(entries []hostEntry) []byte {
	var buf bytes.Buffer
	for _, entry := range entries {
		if len(entry.names) == 0 {
			continue
		}
		buf.WriteString(entry.address)
		buf.WriteByte('\t')
		buf.WriteString(strings.Join(entry.names, " "))
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// parseHostsFile reads an existing /etc/hosts, in file order.
func parseHostsFile(path string) ([]hostEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening hosts file: %w", err)
	}
	defer f.Close()

	var entries []hostEntry
	scanner := bufio.NewScanner(f)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		// Trailing comments are legal in /etc/hosts.
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = line[:idx]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 2 {
			return nil, fmt.Errorf("%s:%d: address %q has no host names", path, lineNumber, fields[0])
		}
		entries = append(entries, hostEntry{address: fields[0], names: fields[1:]})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return entries, nil
}
