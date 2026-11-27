// Package updateimages implements `img update-images`, which maintains the
// images that a workspace pulls: the calls of the pull repository rule and the
// images.pull and images.referrers tags of the images module extension.
//
// It lists them, pins tags to digests, discovers referrers to pin, and moves
// images to newer versions. Changes are made with buildozer, so they can also
// just be printed. Every command has a Bazel target in @rules_img//img/pull.
package updateimages

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/gateway"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/registryopts"
)

// Process runs `img update-images`.
func Process(ctx context.Context, args []string) {
	if err := run(ctx, args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	opts, err := parseArgs(args, stdout, stderr)
	if err == flag.ErrHelp {
		return nil
	}
	if err != nil {
		return err
	}
	allowed, err := parseBump(opts.maxBump)
	if err != nil {
		return err
	}
	if opts.keepTags {
		allowed = bumpNone
	}
	ws, err := ScanWorkspace(opts.workspace)
	if err != nil {
		return err
	}
	all := ws.Entries
	if opts.inventory != "" && !opts.rootOnly {
		inv, err := readInventory(opts.inventory)
		if err != nil {
			return err
		}
		var warnings []string
		all, warnings = inv.merge(ws)
		for _, warning := range warnings {
			fmt.Fprintf(stderr, "Warning: %s\n", warning)
		}
	}
	selected, err := selectEntries(all, opts.patterns)
	if err != nil {
		return err
	}

	p := &planner{all: all, selected: selected, jobs: opts.jobs, pinCommand: invocation("pin", opts.bazelTarget)}
	var results []*result
	switch opts.command {
	case "list":
		results = p.list()
	default:
		applySettings(ws)
		client, err := newRegistryClient()
		if err != nil {
			return err
		}
		p.client = client
		switch opts.command {
		case "pin":
			results = p.pin(ctx)
		case "update":
			results = p.update(ctx, updateOptions{maxBump: allowed, latestTag: opts.latestTag})
		case "referrers":
			results = p.referrers(ctx, newTypeFilter(opts.artifactTypes))
		}
	}
	for _, r := range results {
		readOnlyNote(r.Entry, r)
	}
	commands := addCommands(ws, results)

	failed := 0
	for _, r := range results {
		if r.Status == StatusError {
			failed++
		}
	}
	switch opts.mode {
	case "commands":
		for _, c := range commands {
			fmt.Fprintln(stdout, c)
		}
		for _, r := range results {
			for _, note := range r.Notes {
				fmt.Fprintf(stderr, "%s (%s): %s\n", r.Entry.DisplayName(), r.Entry.Where(), note)
			}
			if r.Status == StatusError {
				fmt.Fprintf(stderr, "%s (%s): error: %s\n", r.Entry.DisplayName(), r.Entry.Where(), r.Error)
			}
		}
	default:
		if opts.format == "json" {
			if err := writeJSON(stdout, results); err != nil {
				return err
			}
		} else {
			writeText(stdout, results)
		}
	}
	if opts.mode == "apply" {
		if len(commands) == 0 {
			fmt.Fprintln(stderr, "Nothing to change.")
		} else {
			if err := runBuildozer(opts.buildozer, ws.Root, commands); err != nil {
				return err
			}
			fmt.Fprintf(stderr, "Applied %d buildozer commands. The lockfile is updated on the next build.\n", len(commands))
		}
	} else if opts.mode == "report" && len(commands) > 0 && opts.format == "text" {
		changed := 0
		for _, r := range results {
			if len(r.Commands) > 0 {
				changed++
			}
		}
		fmt.Fprintf(stderr, "\n%d of %d would change. To make the changes, run:\n\n  %s\n", changed, len(results), opts.applyCommand())
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d images failed", failed, len(results))
	}
	return nil
}

// selectEntries returns the entries that match any of the patterns (or all
// of them, without patterns).
func selectEntries(entries []*Entry, patterns []string) ([]*Entry, error) {
	if len(patterns) == 0 {
		return entries, nil
	}
	var regexps []*regexp.Regexp
	for _, pattern := range patterns {
		quoted := strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*")
		regexps = append(regexps, regexp.MustCompile("^"+quoted+"$"))
	}
	var selected []*Entry
	for _, entry := range entries {
		for _, re := range regexps {
			if re.MatchString(entry.Name) || re.MatchString(entry.Repository) || re.MatchString(entry.Digest) || re.MatchString(entry.Manifest) {
				selected = append(selected, entry)
				break
			}
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("no pulled image matches %s", strings.Join(patterns, ", "))
	}
	return selected, nil
}

// applySettings makes the registry settings of the root module's
// images.settings tag apply, unless the environment overrides them.
func applySettings(ws *Workspace) {
	if ws.Settings.CredentialHelper != "" && os.Getenv("IMG_CREDENTIAL_HELPER") == "" {
		os.Setenv("IMG_CREDENTIAL_HELPER", ws.Settings.CredentialHelper)
	}
	if os.Getenv("REGISTRY_AUTH_FILE") != "" {
		return
	}
	switch {
	case ws.Settings.Config != "":
		os.Setenv("REGISTRY_AUTH_FILE", filepath.Join(ws.Root, filepath.FromSlash(ws.Settings.Config)))
	case ws.Settings.DockerConfigPath != "":
		os.Setenv("REGISTRY_AUTH_FILE", ws.Settings.DockerConfigPath)
	}
}

var nameOptions = func() []name.Option { return registryopts.NameOptions() }

func newRegistryClient() (*registryClient, error) {
	transport, err := registryopts.Transport(gateway.ModePull)
	if err != nil {
		return nil, fmt.Errorf("configuring registry access: %w", err)
	}
	return &registryClient{
		remote:    registryopts.Default().WithTransport(transport).Remote(),
		referrers: registryopts.Referrers(transport),
	}, nil
}
