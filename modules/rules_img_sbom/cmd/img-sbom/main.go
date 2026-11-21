// Command img-sbom writes the SBOM of an image manifest or index built by
// rules_img. The automatic referrers toolchain of rules_img_sbom runs it; see
// //sbom:toolchain.bzl for the request it reads.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/bazel-contrib/rules_img_sbom/internal/bom"
	"github.com/bazel-contrib/rules_img_sbom/internal/graph"
)

// Request is written by the toolchain (see //sbom:toolchain.bzl).
type Request struct {
	Kind       string `json:"kind"`
	Format     string `json:"format"`
	Name       string `json:"name"`
	Descriptor string `json:"descriptor"`
	// Timestamp says which creation time to record (see bom.Created).
	Timestamp struct {
		Mode        string   `json:"mode"`
		Key         string   `json:"key"`
		StatusFiles []string `json:"status_files"`
	} `json:"timestamp"`

	// Manifest requests.
	Graph    string `json:"graph"`
	BaseMode string `json:"base_mode"`
	Base     *struct {
		Image *bom.BaseImage `json:"image"`
		SBOMs []bom.BaseSBOM `json:"sboms"`
	} `json:"base"`

	// Index requests.
	IndexMode string `json:"index_mode"`
	// ReferenceChildren says whether the children's SBOMs are published, so the
	// index's SBOM can refer to them.
	ReferenceChildren bool `json:"reference_children"`
	Children          []struct {
		Descriptor             string `json:"descriptor"`
		SBOM                   string `json:"sbom"`
		SBOMReferrerDescriptor string `json:"sbom_referrer_descriptor"`
	} `json:"children"`
}

func main() {
	requestPath := flag.String("request", "", "Path to the JSON request written by the toolchain")
	outPath := flag.String("out", "", "Path to write the SBOM to")
	flag.Parse()
	if *requestPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "Error: --request and --out are required")
		os.Exit(1)
	}
	if err := run(*requestPath, *outPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run(requestPath, outPath string) error {
	var request Request
	if err := readJSON(requestPath, &request); err != nil {
		return fmt.Errorf("reading the request: %w", err)
	}
	var descriptor bom.Descriptor
	if err := readJSON(request.Descriptor, &descriptor); err != nil {
		return fmt.Errorf("reading the image descriptor: %w", err)
	}
	created, err := bom.Created(request.Timestamp.Mode, request.Timestamp.StatusFiles, request.Timestamp.Key, time.Now)
	if err != nil {
		return err
	}

	var doc bom.Document
	switch request.Kind {
	case "manifest":
		var g graph.GraphConfig
		if err := readJSON(request.Graph, &g); err != nil {
			return fmt.Errorf("reading the package metadata graph: %w", err)
		}
		manifest := bom.Manifest{
			Name:       request.Name,
			Descriptor: descriptor,
			Graph:      g,
			BaseMode:   request.BaseMode,
			Format:     request.Format,
			Created:    created,
			Log:        os.Stderr,
		}
		if request.Base != nil {
			manifest.BaseImage = request.Base.Image
			manifest.BaseSBOMs = request.Base.SBOMs
		}
		doc, err = bom.ComposeManifest(manifest)
	case "index":
		index := bom.Index{Name: request.Name, Descriptor: descriptor, Mode: request.IndexMode, OmitChildReferences: !request.ReferenceChildren, Created: created}
		for _, c := range request.Children {
			var child bom.Child
			if err := readJSON(c.Descriptor, &child.Descriptor); err != nil {
				return fmt.Errorf("reading a child descriptor: %w", err)
			}
			child.SBOM = c.SBOM
			if c.SBOMReferrerDescriptor != "" {
				var referrer bom.Descriptor
				if err := readJSON(c.SBOMReferrerDescriptor, &referrer); err != nil {
					return fmt.Errorf("reading a child SBOM descriptor: %w", err)
				}
				child.SBOMArtifact = referrer.Digest
			}
			index.Children = append(index.Children, child)
		}
		doc, err = bom.ComposeIndex(index)
	default:
		return fmt.Errorf("unknown request kind %q", request.Kind)
	}
	if err != nil {
		return err
	}

	// Derived from what the document describes (the image's name and digest),
	// so rebuilding an unchanged image produces the same SBOM.
	var data []byte
	switch request.Format {
	case "cyclonedx":
		data, err = bom.WriteCycloneDX(doc, "urn:uuid:"+uuid.NewSHA1(uuid.NameSpaceURL, []byte(doc.Primary.Ref)).String())
	case "spdx":
		data, err = bom.WriteSPDX(doc, bom.SPDXNamespace(doc.Primary.Ref))
	default:
		return fmt.Errorf("unknown format %q", request.Format)
	}
	if err != nil {
		return err
	}
	return os.WriteFile(outPath, data, 0o644)
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
