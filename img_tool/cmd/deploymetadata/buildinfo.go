package deploymetadata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/argfile"
)

// BuildInfo is the build info file of a deploy: the Bazel targets it was built
// from, for signer plugins that attest to them. It is written by `img build-info`
// from what the img_build_info_aspect collected, and handed to protocol 2 plugins
// as Deploy.BuildInfoFile. Plugins ignore fields they do not know.
type BuildInfo struct {
	SchemaVersion int `json:"schema_version"`
	// Deploy is the deploy rule (image_push, multi_deploy) itself.
	Deploy BuildInfoTarget `json:"deploy"`
	// Images are the image manifests and indexes below the deploy rule.
	Images []BuildInfoImage `json:"images,omitempty"`
	// Layers are the layer targets below the deploy rule.
	Layers []BuildInfoLayer `json:"layers,omitempty"`
	// BaseImages are the pulled images some image is based on.
	BaseImages []BuildInfoBaseImage `json:"base_images,omitempty"`
}

// BuildInfoTarget identifies a configured Bazel target. Configuration is the
// name of its output directory, such as "darwin_arm64-fastbuild".
type BuildInfoTarget struct {
	Label         string `json:"label"`
	Configuration string `json:"configuration,omitempty"`
}

// BuildInfoImage is an image_manifest or image_index target.
type BuildInfoImage struct {
	BuildInfoTarget
	// Kind is "image" or "index".
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
	// Platform is the platform of an image (not of an index).
	Platform *BuildInfoPlatform `json:"platform,omitempty"`
	// Base is the label of the target the image is based on, if any. When it is
	// a pulled image there is an entry for it in BaseImages.
	Base string `json:"base,omitempty"`
	// Layers are the digests of the layers of an image, in order.
	Layers []string `json:"layers,omitempty"`
	// Manifests are the digests of the images of an index.
	Manifests []string `json:"manifests,omitempty"`
}

// BuildInfoPlatform is the platform an image runs on.
type BuildInfoPlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

// BuildInfoLayer is a layer target.
type BuildInfoLayer struct {
	BuildInfoTarget
	Digest string `json:"digest"`
}

// BuildInfoBaseImage is an image pulled from a registry.
type BuildInfoBaseImage struct {
	Label      string   `json:"label"`
	Registries []string `json:"registries,omitempty"`
	Repository string   `json:"repository,omitempty"`
	Tag        string   `json:"tag,omitempty"`
	Digest     string   `json:"digest,omitempty"`
}

// buildInfoEntry is one line of the aspect's output.
type buildInfoEntry struct {
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Config string `json:"config"`
	File   string `json:"file,omitempty"`
	Base   string `json:"base,omitempty"`
	// Platform is set for images.
	Platform *BuildInfoPlatform `json:"platform,omitempty"`
	Pull     *struct {
		Registries []string `json:"registries"`
		Repository string   `json:"repository"`
		Tag        string   `json:"tag"`
		Digest     string   `json:"digest"`
	} `json:"pull,omitempty"`
}

func BuildInfoProcess(ctx context.Context, args []string) {
	args, err := argfile.Expand(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing argfile: %v\n", err)
		os.Exit(1)
	}
	flagSet := flag.NewFlagSet("build-info", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Writes the build info file of a deploy from the targets the aspect collected.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img build-info --label LABEL --configuration CONFIG --entry JSON... --output FILE\n")
		flagSet.PrintDefaults()
		os.Exit(1)
	}
	var label, configuration, output string
	var entries []string
	flagSet.StringVar(&label, "label", "", "label of the deploy rule")
	flagSet.StringVar(&configuration, "configuration", "", "configuration of the deploy rule")
	flagSet.StringVar(&output, "output", "", "path to write the build info JSON to")
	flagSet.Func("entry", "(repeatable) JSON record of a collected target", func(v string) error {
		entries = append(entries, v)
		return nil
	})
	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
	}
	if label == "" || output == "" {
		flagSet.Usage()
	}

	info, err := buildBuildInfo(BuildInfoTarget{Label: label, Configuration: configuration}, entries)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	data, err := json.Marshal(info)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(output, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func buildBuildInfo(deploy BuildInfoTarget, rawEntries []string) (*BuildInfo, error) {
	info := &BuildInfo{SchemaVersion: 1, Deploy: deploy}
	seen := map[string]bool{}
	for _, raw := range rawEntries {
		if seen[raw] {
			continue
		}
		seen[raw] = true
		var e buildInfoEntry
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			return nil, fmt.Errorf("parsing entry %q: %w", raw, err)
		}
		target := BuildInfoTarget{Label: e.Label, Configuration: e.Config}
		switch e.Kind {
		case "pull":
			if e.Pull == nil {
				return nil, fmt.Errorf("pull entry %s has no pull info", e.Label)
			}
			info.BaseImages = append(info.BaseImages, BuildInfoBaseImage{
				Label: e.Label, Registries: e.Pull.Registries, Repository: e.Pull.Repository, Tag: e.Pull.Tag, Digest: e.Pull.Digest,
			})
		case "layer":
			var meta struct {
				Digest string `json:"digest"`
			}
			if err := readJSON(e.File, &meta); err != nil {
				return nil, fmt.Errorf("layer %s: %w", e.Label, err)
			}
			info.Layers = append(info.Layers, BuildInfoLayer{BuildInfoTarget: target, Digest: meta.Digest})
		case "image", "index":
			raw, err := os.ReadFile(e.File)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", e.Kind, e.Label, err)
			}
			sum := sha256.Sum256(raw)
			var content struct {
				Layers    []struct{ Digest string } `json:"layers"`
				Manifests []struct{ Digest string } `json:"manifests"`
			}
			if err := json.Unmarshal(raw, &content); err != nil {
				return nil, fmt.Errorf("%s %s: %w", e.Kind, e.Label, err)
			}
			image := BuildInfoImage{BuildInfoTarget: target, Kind: e.Kind, Digest: "sha256:" + hex.EncodeToString(sum[:]), Base: e.Base, Platform: e.Platform}
			for _, l := range content.Layers {
				image.Layers = append(image.Layers, l.Digest)
			}
			for _, m := range content.Manifests {
				image.Manifests = append(image.Manifests, m.Digest)
			}
			info.Images = append(info.Images, image)
		default:
			return nil, fmt.Errorf("unknown entry kind %q", e.Kind)
		}
	}
	// Bazel's depset order is not stable across builds; the file is a pure
	// function of what it describes.
	sort.Slice(info.Images, func(i, j int) bool { return lessTarget(info.Images[i].BuildInfoTarget, info.Images[j].BuildInfoTarget) })
	sort.Slice(info.Layers, func(i, j int) bool { return lessTarget(info.Layers[i].BuildInfoTarget, info.Layers[j].BuildInfoTarget) })
	sort.Slice(info.BaseImages, func(i, j int) bool { return info.BaseImages[i].Label < info.BaseImages[j].Label })
	return info, nil
}

func lessTarget(a, b BuildInfoTarget) bool {
	if a.Label != b.Label {
		return a.Label < b.Label
	}
	return a.Configuration < b.Configuration
}

func readJSON(path string, into any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, into)
}
