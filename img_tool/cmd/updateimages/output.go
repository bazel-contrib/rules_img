package updateimages

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

func writeJSON(w io.Writer, results []*result) error {
	if results == nil {
		results = []*result{}
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(results)
}

func writeText(w io.Writer, results []*result) {
	if len(results) == 0 {
		fmt.Fprintln(w, "No pulled images found in MODULE.bazel.")
		return
	}
	for i, r := range results {
		if i > 0 {
			fmt.Fprintln(w)
		}
		writeResult(w, r)
	}
}

func writeResult(w io.Writer, r *result) {
	entry := r.Entry
	fmt.Fprintf(w, "%s  %s %s\n", entry.Where(), entry.Kind, entry.DisplayName())
	line := func(format string, args ...any) { fmt.Fprintf(w, "    "+format+"\n", args...) }
	if entry.Kind == KindImagesReferrers {
		if r.NewManifest == "" {
			line("manifest: %s", entry.Manifest)
		}
	} else {
		line("%s", entry.Ref())
	}
	if r.Status == StatusListed {
		writeDetails(line, entry)
	}
	if r.NewTag != "" {
		line("tag:      %s -> %s", entry.Tag, r.NewTag)
	}
	if r.NewDigest != "" {
		old := entry.Digest
		if old == "" {
			old = "(none)"
		}
		line("digest:   %s -> %s", old, r.NewDigest)
	}
	if r.NewManifest != "" {
		line("manifest: %s -> %s", entry.Manifest, r.NewManifest)
	}
	if r.RemoveUnsafe {
		line("unsafe_allow_tag_without_digest: removed")
	}
	if r.SetReferrers != nil {
		line("referrer_digests:")
		for _, digest := range entry.ReferrerDigests {
			line("  - %s", digest)
		}
		for _, digest := range *r.SetReferrers {
			line("  + %s", digest)
		}
	}
	if r.referrersChecked {
		writeReferrers(line, r)
	}
	if len(r.AddReferrers) > 0 {
		line("add to referrer_digests:")
		for _, digest := range r.AddReferrers {
			line("  + %s", digest)
		}
	}
	for _, note := range r.Notes {
		line("note: %s", note)
	}
	switch r.Status {
	case StatusError:
		line("error: %s", r.Error)
	case StatusUpToDate:
		switch {
		case !r.referrersChecked:
			line("up to date")
		case len(r.Referrers) > 0:
			line("all of them are pinned")
		}
	}
}

func writeDetails(line func(string, ...any), entry *Entry) {
	sources := entry.Sources()
	if len(sources) > 1 {
		var names []string
		for _, src := range sources[1:] {
			names = append(names, src.String())
		}
		line("also from: %s", strings.Join(names, ", "))
	}
	if entry.Kind != KindImagesReferrers {
		switch {
		case entry.Digest == "" && entry.UnsafeAllowTagWithoutDigest:
			line("not pinned: follows the tag (unsafe_allow_tag_without_digest)")
		case entry.Digest == "":
			line("not pinned: no digest")
		}
	}
	if len(entry.Platforms) > 0 {
		line("platforms: %s", strings.Join(entry.Platforms, ", "))
	}
	if entry.LayerHandling != "" {
		line("layer_handling: %s", entry.LayerHandling)
	}
	if len(entry.ReferrerDigests) > 0 {
		line("referrer_digests:")
		for _, digest := range entry.ReferrerDigests {
			line("  %s", digest)
		}
	}
	if len(entry.ReferrerArtifactTypes) > 0 {
		line("referrer_artifact_types: %s", strings.Join(entry.ReferrerArtifactTypes, ", "))
	}
	if len(entry.ReferrerCosignTags) > 0 {
		line("referrer_cosign_tags: %s", strings.Join(entry.ReferrerCosignTags, ", "))
	}
	if entry.ReferrerBuildKit {
		line("referrer_buildkit_attestations: True")
	}
	if entry.HasReferrerArtifacts {
		line("referrer_artifacts: set")
	}
	if entry.DevDependency {
		line("dev dependency")
	}
}

// interestingLayerAnnotations are shown for every layer of a referrer; the
// rest (signatures, certificates, bundles) are only counted.
var interestingLayerAnnotations = []string{
	"predicateType",
	"dev.sigstore.bundle.predicateType",
	"in-toto.io/predicate-type",
	"org.opencontainers.image.title",
}

func writeReferrers(line func(string, ...any), r *result) {
	if len(r.Referrers) == 0 {
		line("no referrers found")
		return
	}
	var subjects []string
	bySubject := map[string][]Referrer{}
	platforms := map[string]string{}
	for _, referrer := range r.Referrers {
		if _, ok := bySubject[referrer.Subject]; !ok {
			subjects = append(subjects, referrer.Subject)
		}
		bySubject[referrer.Subject] = append(bySubject[referrer.Subject], referrer)
		platforms[referrer.Subject] = referrer.SubjectPlatform
	}
	for _, subj := range subjects {
		label := platforms[subj]
		if label == "" {
			label = "root"
		}
		line("referrers of %s (%s):", subj, label)
		for _, referrer := range bySubject[subj] {
			var facts []string
			if referrer.CosignTag != "" {
				facts = append(facts, "tag "+referrer.CosignTag)
			} else {
				facts = append(facts, referrer.Source)
			}
			facts = append(facts, humanSize(referrer.Size))
			if referrer.Pinned {
				facts = append(facts, "pinned")
			} else {
				facts = append(facts, "new")
			}
			line("  %s  %s  (%s)", referrer.Digest, referrer.Type, strings.Join(facts, ", "))
			if referrer.MediaType != "" && !strings.HasPrefix(referrer.MediaType, "application/vnd.oci.image.manifest") {
				line("      mediaType: %s", referrer.MediaType)
			}
			for _, key := range sortedKeys(referrer.Annotations) {
				line("      %s: %s", key, truncate(referrer.Annotations[key], 100))
			}
			for _, layer := range referrer.Layers {
				var shown []string
				for _, key := range interestingLayerAnnotations {
					if value, ok := layer.Annotations[key]; ok {
						shown = append(shown, key+"="+truncate(value, 100))
					}
				}
				if hidden := len(layer.Annotations) - len(shown); hidden > 0 {
					shown = append(shown, fmt.Sprintf("+%d annotations", hidden))
				}
				suffix := ""
				if len(shown) > 0 {
					suffix = " " + strings.Join(shown, " ")
				}
				line("      layer %s (%s)%s", layer.MediaType, humanSize(layer.Size), suffix)
			}
		}
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
