// Package imagesizetest implements the `img image-size-test`
// subcommand: it replays an image's per-layer mtrees in layer order and fails
// when wasted bytes (shipped in a layer but shadowed by an overwrite,
// whiteout, or opaque whiteout in a later layer) or duplicated bytes
// (identical file content stored at multiple paths) exceed a budget. It is
// invoked at `bazel test` time by the image_size_test rule via the hermetic
// launcher, with a single --request file that points (by runfiles rlocation
// path) at the aspect-produced images spec. Layer blobs are never read.
package imagesizetest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/bazelbuild/rules_go/go/runfiles"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/bytesize"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/mtree"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/structuretest"
)

// maxReported caps the per-image waste / duplicate listings.
const maxReported = 10

// Request is the single file the image_size_test rule hands to
// `img image-size-test`. Budgets are nil when not set on the rule.
type Request struct {
	Spec               string `json:"spec"`
	MaxWastedBytes     *int64 `json:"max_wasted_bytes"`
	MaxDuplicatedBytes *int64 `json:"max_duplicated_bytes"`
}

// Spec is the set of images to check, written by the image_size_test aspect.
type Spec struct {
	Images []ImageSpec `json:"images"`
}

// ImageSpec describes a single image (one platform) as its ordered layers.
type ImageSpec struct {
	Platform structuretest.Platform `json:"platform"`
	Layers   []LayerSpec            `json:"layers"`
}

// LayerSpec is one layer of an image, bottom (base) to top. Exactly one of
// Mtree, Missing, or NonTar describes it: a tar layer with an mtree, a layer
// whose filesystem content is unknown (incomplete metadata), or a layer of a
// media type that positively carries no filesystem entries (and is therefore
// safe to skip).
type LayerSpec struct {
	Mtree    string `json:"mtree,omitempty"`    // runfiles rlocation path
	Metadata string `json:"metadata,omitempty"` // runfiles rlocation path of the layer metadata JSON
	Missing  bool   `json:"missing,omitempty"`
	NonTar   bool   `json:"non_tar,omitempty"`
}

// Process is the entry point for `img image-size-test`.
func Process(_ context.Context, args []string) {
	requestPath := structuretest.RequestPathFlag(
		"image-size-test",
		"Checks an image's wasted and duplicated layer bytes against budgets.",
		args,
	)

	ok, err := run(requestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "image-size-test: %v\n", err)
		os.Exit(1)
	}
	if !ok {
		os.Exit(1)
	}
}

func run(requestPath string) (bool, error) {
	req, err := loadJSON[Request](requestPath)
	if err != nil {
		return false, err
	}
	specPath, err := runfiles.Rlocation(req.Spec)
	if err != nil {
		return false, fmt.Errorf("resolving spec %q: %w", req.Spec, err)
	}
	spec, err := loadJSON[Spec](specPath)
	if err != nil {
		return false, err
	}
	if len(spec.Images) == 0 {
		return false, fmt.Errorf("spec references no images")
	}
	if req.MaxWastedBytes == nil && req.MaxDuplicatedBytes == nil {
		return false, fmt.Errorf("request sets neither max_wasted_bytes nor max_duplicated_bytes")
	}

	ok := true
	for _, img := range spec.Images {
		pass, err := checkImage(img, req)
		if err != nil {
			return false, fmt.Errorf("image %s: %w", img.Platform, err)
		}
		if !pass {
			ok = false
		}
	}
	return ok, nil
}

func checkImage(img ImageSpec, req *Request) (bool, error) {
	layers := make([]layerInput, 0, len(img.Layers))
	measured := 0
	for _, spec := range img.Layers {
		layer, err := loadLayer(spec)
		if err != nil {
			return false, err
		}
		if !spec.NonTar {
			measured++
		}
		layers = append(layers, layer)
	}

	rep, err := analyze(layers, req.MaxDuplicatedBytes != nil)
	if err != nil {
		// An analysis error is a configuration problem, not a budget
		// violation, so it is reported as an error rather than a FAIL.
		return false, err
	}

	scope := fmt.Sprintf("%d of %d layers", measured, len(layers))
	pass := true
	if req.MaxWastedBytes != nil && rep.WastedBytes > *req.MaxWastedBytes {
		pass = false
		fmt.Fprintf(os.Stderr, "FAIL image %s: wasted bytes %d (%s uncompressed) exceeds budget %d (%s)\n",
			img.Platform, rep.WastedBytes, bytesize.Format(rep.WastedBytes), *req.MaxWastedBytes, bytesize.Format(*req.MaxWastedBytes))
		if len(rep.Waste) > 0 {
			fmt.Fprintf(os.Stderr, "  largest waste:\n")
			for _, w := range rep.Waste[:min(len(rep.Waste), maxReported)] {
				fmt.Fprintf(os.Stderr, "      %-10s /%s  written by %s, %s %s\n",
					bytesize.Format(w.Size), w.Path, layerRef(layers, w.WrittenLayer), w.Reason.Phrase(), layerRef(layers, w.ShadowedLayer))
			}
			fmt.Fprintf(os.Stderr, "  hint: remove the file from the earlier layer, or avoid overwriting it in the later one.\n")
		}
	}
	if req.MaxDuplicatedBytes != nil && rep.DuplicatedBytes > *req.MaxDuplicatedBytes {
		pass = false
		fmt.Fprintf(os.Stderr, "FAIL image %s: duplicated bytes %d (%s uncompressed) exceeds budget %d (%s)\n",
			img.Platform, rep.DuplicatedBytes, bytesize.Format(rep.DuplicatedBytes), *req.MaxDuplicatedBytes, bytesize.Format(*req.MaxDuplicatedBytes))
		if len(rep.Duplicates) > 0 {
			fmt.Fprintf(os.Stderr, "  largest duplicates:\n")
			for _, g := range rep.Duplicates[:min(len(rep.Duplicates), maxReported)] {
				paths := make([]string, len(g.Paths))
				for i, p := range g.Paths {
					paths[i] = "/" + p
				}
				fmt.Fprintf(os.Stderr, "      %-10s extra for %d copies of %s content: %s\n",
					bytesize.Format(g.ExtraBytes()), len(g.Paths), bytesize.Format(g.Size), strings.Join(paths, ", "))
			}
			fmt.Fprintf(os.Stderr, "  hint: keep one copy and link or reference it from the other paths.\n")
		}
	}
	if pass {
		fmt.Fprintf(os.Stderr, "PASS image %s (%s): wasted bytes %s, duplicated bytes %s\n",
			img.Platform, scope,
			budgetSummary(rep.WastedBytes, true, req.MaxWastedBytes),
			budgetSummary(rep.DuplicatedBytes, rep.DuplicatesMeasured, req.MaxDuplicatedBytes))
	}
	return pass, nil
}

// budgetSummary renders a measured value against its budget. A value that was
// never measured is said so rather than reported as zero.
func budgetSummary(value int64, measured bool, budget *int64) string {
	if !measured {
		return "not measured"
	}
	if budget == nil {
		return fmt.Sprintf("%d (%s, no budget)", value, bytesize.Format(value))
	}
	return fmt.Sprintf("%d (%s, budget %s)", value, bytesize.Format(value), bytesize.Format(*budget))
}

// layerRef renders "layer 2" or "layer 2 (//app:deps)" for a 0-based index.
func layerRef(layers []layerInput, i int) string {
	ref := fmt.Sprintf("layer %d", i+1)
	if layers[i].name != "" {
		ref += " (" + layers[i].name + ")"
	}
	return ref
}

func loadLayer(spec LayerSpec) (layerInput, error) {
	layer := layerInput{missing: spec.Missing}
	if spec.Metadata != "" {
		metaPath, err := runfiles.Rlocation(spec.Metadata)
		if err != nil {
			return layer, fmt.Errorf("resolving layer metadata %q: %w", spec.Metadata, err)
		}
		name, err := layerName(metaPath)
		if err != nil {
			return layer, err
		}
		layer.name = name
	}
	if spec.Missing || spec.NonTar {
		return layer, nil
	}
	mtreePath, err := runfiles.Rlocation(spec.Mtree)
	if err != nil {
		return layer, fmt.Errorf("resolving layer mtree %q: %w", spec.Mtree, err)
	}
	f, err := os.Open(mtreePath)
	if err != nil {
		return layer, fmt.Errorf("opening layer mtree %s: %w", mtreePath, err)
	}
	defer f.Close()
	entries, err := mtree.ParseEntries(f)
	if err != nil {
		return layer, fmt.Errorf("parsing layer mtree %s: %w", mtreePath, err)
	}
	layer.entries = entries
	return layer, nil
}

// layerName attributes a layer to its producing target where possible: the
// created_by of the history entry that actually shipped filesystem bytes, with
// the "bazel build " prefix the layer rules add stripped back to the bare
// label. A pulled image's layer carries the metadata-only steps that preceded
// it (ENV, WORKDIR, ...) as empty_layer history entries, which named no bytes
// and so must not be reported as the layer's origin.
func layerName(metadataPath string) (string, error) {
	desc, err := loadJSON[api.Descriptor](metadataPath)
	if err != nil {
		return "", fmt.Errorf("reading layer metadata: %w", err)
	}
	for _, skipEmpty := range []bool{true, false} {
		for _, h := range desc.History {
			if skipEmpty && h.EmptyLayer {
				continue
			}
			name := strings.TrimSpace(strings.TrimPrefix(h.CreatedBy, api.BazelBuildHistoryPrefix))
			if name == "" || name == api.MissingHistoryMarker {
				continue
			}
			if runes := []rune(name); len(runes) > 80 {
				name = string(runes[:80]) + "…"
			}
			return name, nil
		}
	}
	return "", nil
}

func loadJSON[T any](path string) (*T, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &v, nil
}
