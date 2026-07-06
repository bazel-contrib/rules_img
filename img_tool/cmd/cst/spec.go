package cst

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bazelbuild/rules_go/go/runfiles"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/structuretest"
)

// The JSON wire types live in pkg/structuretest so the producer
// (`img oci-layout-metadata`) and this consumer share one definition.
type (
	Request   = structuretest.Request
	Spec      = structuretest.Spec
	ImageSpec = structuretest.ImageSpec
	Platform  = structuretest.Platform
)

// resolvedImage is an image with its config + mtree resolved to absolute,
// readable paths.
type resolvedImage struct {
	platform   Platform
	configPath string
	mtreePath  string // "" when no mtree is available
	complete   bool
}

// loadRequest reads the request file (an absolute path provided by the launcher).
func loadRequest(path string) (*Request, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading request %s: %w", path, err)
	}
	var req Request
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("parsing request %s: %w", path, err)
	}
	return &req, nil
}

// loadImages resolves every image referenced by the spec to absolute paths,
// reading each LayoutTree's images.json to discover its platforms.
func loadImages(spec *Spec) ([]resolvedImage, error) {
	var images []resolvedImage
	for _, img := range spec.Images {
		configPath, err := runfiles.Rlocation(img.Config)
		if err != nil {
			return nil, fmt.Errorf("resolving config %q: %w", img.Config, err)
		}
		mtreePath := ""
		if img.Mtree != "" {
			mtreePath, err = runfiles.Rlocation(img.Mtree)
			if err != nil {
				return nil, fmt.Errorf("resolving mtree %q: %w", img.Mtree, err)
			}
		}
		images = append(images, resolvedImage{
			platform:   img.Platform,
			configPath: configPath,
			mtreePath:  mtreePath,
			complete:   img.Complete,
		})
	}
	for _, treeRef := range spec.LayoutTrees {
		treeDir, err := runfiles.Rlocation(treeRef)
		if err != nil {
			return nil, fmt.Errorf("resolving layout tree %q: %w", treeRef, err)
		}
		layoutImages, err := loadLayoutTree(treeDir)
		if err != nil {
			return nil, fmt.Errorf("reading layout metadata tree %q: %w", treeRef, err)
		}
		images = append(images, layoutImages...)
	}
	if len(images) == 0 {
		return nil, fmt.Errorf("spec references no images")
	}
	return images, nil
}

// loadLayoutTree reads the images.json produced by `img oci-layout-metadata`
// inside treeDir; its Config/Mtree paths are relative to treeDir.
func loadLayoutTree(treeDir string) ([]resolvedImage, error) {
	data, err := os.ReadFile(filepath.Join(treeDir, "images.json"))
	if err != nil {
		return nil, fmt.Errorf("reading images.json: %w", err)
	}
	var spec Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parsing images.json: %w", err)
	}
	var images []resolvedImage
	for _, img := range spec.Images {
		mtreePath := ""
		if img.Mtree != "" {
			mtreePath = filepath.Join(treeDir, img.Mtree)
		}
		images = append(images, resolvedImage{
			platform:   img.Platform,
			configPath: filepath.Join(treeDir, img.Config),
			mtreePath:  mtreePath,
			complete:   img.Complete,
		})
	}
	return images, nil
}
