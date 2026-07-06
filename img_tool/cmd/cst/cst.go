// Package cst implements the `img image-structure-test`
// subcommand: a container-structure-test-compatible validator that checks an
// image using ONLY its config JSON and its mtree filesystem listing (never the
// layer blobs). It is invoked at `bazel test` time by the image_structure_test
// rule via the hermetic launcher, with a single --request file that points (by
// runfiles rlocation path) at the aspect-produced images spec and the CST config
// files.
package cst

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/bazelbuild/rules_go/go/runfiles"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/mtree"
)

// Process is the entry point for `img image-structure-test`.
func Process(_ context.Context, args []string) {
	var requestPath string
	flagSet := flag.NewFlagSet("image-structure-test", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Validates an image's structure using its config JSON and mtree.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img image-structure-test --request <request.json>\n")
		flagSet.PrintDefaults()
	}
	flagSet.StringVar(&requestPath, "request", "", "Path to the JSON request file (required).")
	if err := flagSet.Parse(args); err != nil {
		os.Exit(1)
	}
	if requestPath == "" && flagSet.NArg() > 0 {
		requestPath = flagSet.Arg(0)
	}
	if requestPath == "" {
		fmt.Fprintln(os.Stderr, "Error: --request is required")
		flagSet.Usage()
		os.Exit(1)
	}

	ok, err := run(requestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "image-structure-test: %v\n", err)
		os.Exit(1)
	}
	if !ok {
		os.Exit(1)
	}
}

// run loads the request, validates every image against every config, prints a
// report, and reports whether all assertions passed.
func run(requestPath string) (bool, error) {
	req, err := loadRequest(requestPath)
	if err != nil {
		return false, err
	}
	specPath, err := runfiles.Rlocation(req.Spec)
	if err != nil {
		return false, fmt.Errorf("resolving spec %q: %w", req.Spec, err)
	}
	spec, err := loadSpec(specPath)
	if err != nil {
		return false, err
	}
	images, err := loadImages(spec)
	if err != nil {
		return false, err
	}

	// Load and pre-validate every config once. A config that uses an unsupported
	// CST category fails the whole run with a clear message.
	type loadedConfig struct {
		label string
		st    *StructureTest
	}
	var configs []loadedConfig
	failed := false
	for _, ref := range req.Configs {
		p, err := runfiles.Rlocation(ref)
		if err != nil {
			return false, fmt.Errorf("resolving config %q: %w", ref, err)
		}
		st, err := parseConfig(p)
		if err != nil {
			return false, err
		}
		if cats := unsupportedCategories(st); len(cats) > 0 {
			failed = true
			fmt.Fprintf(os.Stderr, "FAIL config %s: unsupported test categories for the mtree+config driver:\n", ref)
			for _, c := range cats {
				fmt.Fprintf(os.Stderr, "  - %s\n", c)
			}
		}
		configs = append(configs, loadedConfig{label: ref, st: st})
	}
	if failed {
		return false, nil
	}

	total, passed := 0, 0
	for _, img := range images {
		cf, err := loadConfigFile(img.configPath)
		if err != nil {
			return false, fmt.Errorf("image %s: %w", img.platform, err)
		}
		entries, err := loadMtree(img.mtreePath)
		if err != nil {
			return false, fmt.Errorf("image %s: %w", img.platform, err)
		}
		fmt.Fprintf(os.Stderr, "== image %s ==\n", img.platform)
		if !img.complete && entries != nil {
			fmt.Fprintf(os.Stderr, "  warning: the image mtree is a partial view (some layers contributed no mtree); "+
				"file-existence results -- especially shouldExist: false -- may be unreliable\n")
		}
		for _, c := range configs {
			var results []result
			results = append(results, checkMetadata(cf, c.st.MetadataTest)...)
			results = append(results, checkFileExistence(entries, c.st.FileExistenceTests)...)
			for _, r := range results {
				total++
				if r.pass {
					passed++
					fmt.Fprintf(os.Stderr, "  PASS [%s] %s\n", c.label, r.name)
				} else {
					failed = true
					fmt.Fprintf(os.Stderr, "  FAIL [%s] %s: %s\n", c.label, r.name, r.msg)
				}
			}
		}
	}

	fmt.Fprintf(os.Stderr, "\n%d/%d assertions passed across %d image(s) and %d config(s)\n",
		passed, total, len(images), len(configs))
	return !failed, nil
}

// loadSpec reads and parses the aspect-produced images spec file.
func loadSpec(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading spec %s: %w", path, err)
	}
	var spec Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parsing spec %s: %w", path, err)
	}
	return &spec, nil
}

// loadConfigFile parses an OCI/Docker image config JSON.
func loadConfigFile(path string) (*v1.ConfigFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening config %s: %w", path, err)
	}
	defer f.Close()
	cf, err := v1.ParseConfigFile(f)
	if err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	return cf, nil
}

// loadMtree parses an mtree file into a map keyed by canonical path. A path of ""
// (no mtree available for the image) returns a nil map, which checkFileExistence
// treats as "cannot check".
func loadMtree(path string) (map[string]mtree.ParsedEntry, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening mtree %s: %w", path, err)
	}
	defer f.Close()
	entries, err := mtree.ParseEntries(f)
	if err != nil {
		return nil, fmt.Errorf("parsing mtree %s: %w", path, err)
	}
	m := make(map[string]mtree.ParsedEntry, len(entries))
	for _, e := range entries {
		m[e.Path] = e
	}
	return m, nil
}
