package updateimages

import (
	"encoding/json"
	"fmt"
	"os"
)

// inventory is what the images module extension knows: the images.pull and
// images.referrers tags of every module. It is written to the extension's hub
// repository as inventory.json.
type inventory struct {
	Images    []inventoryTag `json:"images"`
	Referrers []inventoryTag `json:"referrers"`
}

type inventoryTag struct {
	Module                      string   `json:"module"`
	Version                     string   `json:"version"`
	IsRoot                      bool     `json:"is_root"`
	Name                        string   `json:"name"`
	Registry                    string   `json:"registry"`
	Registries                  []string `json:"registries"`
	Repository                  string   `json:"repository"`
	Tag                         string   `json:"tag"`
	Digest                      string   `json:"digest"`
	Manifest                    string   `json:"manifest"`
	ReferrerDigests             []string `json:"referrer_digests"`
	ReferrerArtifactTypes       []string `json:"referrer_artifact_types"`
	ReferrerCosignTags          []string `json:"referrer_cosign_tags"`
	ReferrerBuildKit            bool     `json:"referrer_buildkit_attestations"`
	HasReferrerArtifacts        bool     `json:"has_referrer_artifacts"`
	LayerHandling               string   `json:"layer_handling"`
	UnsafeAllowTagWithoutDigest bool     `json:"unsafe_allow_tag_without_digest"`
}

func readInventory(path string) (*inventory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the images extension inventory: %w", err)
	}
	var inv inventory
	if err := json.Unmarshal(data, &inv); err != nil {
		return nil, fmt.Errorf("parsing the images extension inventory %s: %w", path, err)
	}
	return &inv, nil
}

func (t inventoryTag) entry(kind Kind) *Entry {
	return &Entry{
		Kind:                        kind,
		Module:                      t.Module,
		Version:                     t.Version,
		Name:                        t.Name,
		Registry:                    t.Registry,
		Registries:                  t.Registries,
		Repository:                  t.Repository,
		Tag:                         t.Tag,
		Digest:                      t.Digest,
		Manifest:                    t.Manifest,
		ReferrerDigests:             t.ReferrerDigests,
		ReferrerArtifactTypes:       t.ReferrerArtifactTypes,
		ReferrerCosignTags:          t.ReferrerCosignTags,
		ReferrerBuildKit:            t.ReferrerBuildKit,
		HasReferrerArtifacts:        t.HasReferrerArtifacts,
		LayerHandling:               t.LayerHandling,
		UnsafeAllowTagWithoutDigest: t.UnsafeAllowTagWithoutDigest,
	}
}

// merge adds the tags of other modules to the entries of the workspace. The
// root module's tags are already there, from its files; any the extension
// knows but the files didn't yield are reported, since they can't be edited.
func (inv *inventory) merge(ws *Workspace) (entries []*Entry, warnings []string) {
	entries = append(entries, ws.Entries...)
	found := map[string]bool{}
	for _, entry := range ws.Entries {
		found[inventoryKey(entry.Kind, entry.Name, entry.Repository, entry.Manifest)] = true
	}
	add := func(kind Kind, tags []inventoryTag) {
		for _, tag := range tags {
			if tag.IsRoot {
				if !found[inventoryKey(kind, tag.Name, tag.Repository, tag.Manifest)] {
					warnings = append(warnings, fmt.Sprintf("the images extension knows %s %q of the root module, but it was not found in its MODULE.bazel files (is the extension proxy created in an unusual way?)", kind, tag.entry(kind).DisplayName()))
				}
				continue
			}
			entries = append(entries, tag.entry(kind))
		}
	}
	add(KindImagesPull, inv.Images)
	add(KindImagesReferrers, inv.Referrers)
	return entries, warnings
}

func inventoryKey(kind Kind, name, repository, manifest string) string {
	if kind == KindImagesReferrers {
		return string(kind) + "|" + manifest
	}
	if name == "" {
		name = repository
	}
	return string(kind) + "|" + name
}
