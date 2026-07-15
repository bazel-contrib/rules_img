package ocitar

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"sort"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

const OCILayoutVersion = "1.0.0"

type BlobSource interface {
	OpenBlob(ctx context.Context, hexDigest string) (io.ReadCloser, int64, error)
}

type ManifestFilter func(manifests []ManifestDescriptor) (included []int, defaultIdx int)

type ManifestDescriptor struct {
	Platform *v1.Platform
	Digest   v1.Hash
	Size     int64
}

type Options struct {
	Tags              []string
	OCITags           []string
	ProgressFunc      func(ctx context.Context, size int64, name string) io.Writer
	ManifestFilter    ManifestFilter
	OCIRefNameTagOnly bool // if true, org.opencontainers.image.ref.name is set to just the tag (OCI spec); default is full reference (compatible with skopeo/rules_oci)
}

type ManifestInfo struct {
	ManifestData []byte
	ConfigDigest string
	LayerDigests []string
	MediaType    types.MediaType
}

type dockerManifest struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

func hashBytes(data []byte) v1.Hash {
	h, _, _ := v1.SHA256(bytes.NewReader(data))
	return h
}

// DescriptorsForTags generates descriptors for the given image manifest and tags.
// Tooling that ingests OCI layouts (containerd loading, docker image load, container load, ...)
// can recover image names from the descriptors of the root index.json file based on some well-known annotations:
//   - Containerd uses "io.containerd.image.name" to refer to the full image name (<registry>/<repository>:<tag>)
//   - Apple Containerization uses "com.apple.containerization.image.name" to refer to the full image name (<registry>/<repository>:<tag>)
//   - The OCI image spec mentions "org.opencontainers.image.ref.name" to refer to the tag only (i.e. "latest"),
//     but we set it to the full image reference by default because tools like skopeo require a fully-qualified reference.
//     Pass tagOnly=true to use the OCI-spec-compliant short tag form instead.
//
// Note that the "org.opencontainers.image.ref.name" may not be unique within the index.json file.
// This is surprising, but allowed by the OCI image spec. Other tools also generate duplicate ref.name attributes.
// Tooling that consumes the index and needs to select images based on tags SHOULD select the first matching manifest.
// See also this upstream discussion: https://github.com/opencontainers/image-spec/issues/581
//
// Annotations from the referenced content (data) are copied into the produced descriptors.
// Tag annotations take precedence over content annotations.
func DescriptorsForTags(ociTags []string, mediaType types.MediaType, data []byte, digest v1.Hash, artifactType string, tagOnly bool) []v1.Descriptor {
	size := int64(len(data))

	var parsed struct {
		Annotations map[string]string `json:"annotations,omitempty"`
	}
	json.Unmarshal(data, &parsed)

	if len(ociTags) == 0 {
		desc := v1.Descriptor{MediaType: mediaType, Digest: digest, Size: size, Annotations: parsed.Annotations}
		if artifactType != "" {
			desc.ArtifactType = artifactType
		}
		return []v1.Descriptor{desc}
	}

	descs := make([]v1.Descriptor, 0, len(ociTags))
	for _, repoTag := range ociTags {
		annotations := make(map[string]string)
		maps.Copy(annotations, parsed.Annotations)
		annotations[api.AnnotationContainerdImageName] = repoTag
		annotations[api.AnnotationAppleContainerizationImageName] = repoTag
		if tagOnly {
			if ref, err := name.NewTag(repoTag, name.WithDefaultTag("")); err == nil && ref.TagStr() != "" {
				annotations[api.AnnotationOCIImageRefName] = ref.TagStr()
			}
		} else {
			annotations[api.AnnotationOCIImageRefName] = repoTag
		}
		desc := v1.Descriptor{
			MediaType:   mediaType,
			Digest:      digest,
			Size:        size,
			Annotations: annotations,
		}
		if artifactType != "" {
			desc.ArtifactType = artifactType
		}
		descs = append(descs, desc)
	}
	return descs
}

func WriteSingleManifest(ctx context.Context, w io.Writer, manifest *v1.Manifest, manifestData []byte, blobSource BlobSource, opts Options) error {
	tw := tar.NewWriter(w)
	defer tw.Close()

	manifestDigest := hashBytes(manifestData)

	// Build OCI index.json
	var artifactType string
	if manifest.Config.MediaType != "" && !manifest.Config.MediaType.IsConfig() {
		artifactType = string(manifest.Config.MediaType)
	}
	indexManifests := DescriptorsForTags(opts.OCITags, manifest.MediaType, manifestData, manifestDigest, artifactType, opts.OCIRefNameTagOnly)
	ociIndex := v1.IndexManifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests:     indexManifests,
	}

	// Build Docker manifest.json
	var dockerLayers []string
	for _, layerDesc := range manifest.Layers {
		dockerLayers = append(dockerLayers, "blobs/sha256/"+layerDesc.Digest.Hex)
	}
	dockerMfst := dockerManifest{
		Config:   "blobs/sha256/" + manifest.Config.Digest.Hex,
		RepoTags: opts.Tags,
		Layers:   dockerLayers,
	}

	// Write metadata files
	if err := writeJSON(tw, "oci-layout", map[string]string{"imageLayoutVersion": OCILayoutVersion}); err != nil {
		return fmt.Errorf("writing oci-layout: %w", err)
	}
	if err := writeJSON(tw, "index.json", ociIndex); err != nil {
		return fmt.Errorf("writing index.json: %w", err)
	}
	if err := writeJSON(tw, "manifest.json", []dockerManifest{dockerMfst}); err != nil {
		return fmt.Errorf("writing manifest.json: %w", err)
	}

	// Write blob directories
	if err := writeDir(tw, "blobs/"); err != nil {
		return err
	}
	if err := writeDir(tw, "blobs/sha256/"); err != nil {
		return err
	}

	// Collect all blobs to write (deduplication and deterministic order)
	blobs := make(map[string][]byte) // hex -> in-memory data (for small blobs)
	var streamBlobs []string         // hex digests to stream from BlobSource

	// Config blob
	streamBlobs = append(streamBlobs, manifest.Config.Digest.Hex)

	// Manifest blob (write from memory)
	blobs[manifestDigest.Hex] = manifestData

	// Layer blobs
	for _, layerDesc := range manifest.Layers {
		streamBlobs = append(streamBlobs, layerDesc.Digest.Hex)
	}

	// Write in-memory blobs first (sorted)
	memDigests := make([]string, 0, len(blobs))
	for d := range blobs {
		memDigests = append(memDigests, d)
	}
	sort.Strings(memDigests)
	for _, d := range memDigests {
		if err := writeFile(tw, filepath.Join("blobs", "sha256", d), blobs[d]); err != nil {
			return fmt.Errorf("writing blob %s: %w", d, err)
		}
	}

	// Stream blobs from source (deduplicated, sorted)
	written := make(map[string]bool)
	for _, d := range memDigests {
		written[d] = true
	}

	// Deduplicate stream blobs and sort
	var uniqueStreamBlobs []string
	for _, d := range streamBlobs {
		if !written[d] {
			uniqueStreamBlobs = append(uniqueStreamBlobs, d)
			written[d] = true
		}
	}
	sort.Strings(uniqueStreamBlobs)

	for _, d := range uniqueStreamBlobs {
		if err := streamBlob(ctx, tw, d, blobSource, opts.ProgressFunc); err != nil {
			return fmt.Errorf("streaming blob %s: %w", d, err)
		}
	}

	return nil
}

func WriteIndex(ctx context.Context, w io.Writer, indexData []byte, manifestInfos []ManifestInfo, blobSource BlobSource, opts Options) error {
	tw := tar.NewWriter(w)
	defer tw.Close()

	// Parse index to get manifest descriptors for ManifestFilter
	var parsedIndex v1.IndexManifest
	if err := json.Unmarshal(indexData, &parsedIndex); err != nil {
		return fmt.Errorf("parsing index: %w", err)
	}

	// Build ManifestDescriptor list for the filter
	manifestDescs := make([]ManifestDescriptor, len(parsedIndex.Manifests))
	for i, desc := range parsedIndex.Manifests {
		manifestDescs[i] = ManifestDescriptor{
			Platform: desc.Platform,
			Digest:   desc.Digest,
			Size:     desc.Size,
		}
	}

	// Apply ManifestFilter
	var included []int
	var defaultIdx int
	if opts.ManifestFilter != nil {
		included, defaultIdx = opts.ManifestFilter(manifestDescs)
	} else {
		// Default: include all, first is default
		included = make([]int, len(manifestInfos))
		for i := range manifestInfos {
			included[i] = i
		}
		defaultIdx = 0
	}

	if len(included) == 0 {
		return fmt.Errorf("ManifestFilter returned no manifests to include")
	}

	// Compute index digest and store as blob
	indexDigest := hashBytes(indexData)

	// Build root OCI index.json (wraps the original index blob)
	rootManifests := DescriptorsForTags(opts.OCITags, types.OCIImageIndex, indexData, indexDigest, "", opts.OCIRefNameTagOnly)
	rootIndex := v1.IndexManifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests:     rootManifests,
	}

	// Build Docker manifest.json from the default manifest
	defaultInfo := manifestInfos[defaultIdx]
	var dockerLayers []string
	for _, layerHex := range defaultInfo.LayerDigests {
		dockerLayers = append(dockerLayers, "blobs/sha256/"+layerHex)
	}
	dockerMfst := dockerManifest{
		Config:   "blobs/sha256/" + defaultInfo.ConfigDigest,
		RepoTags: opts.Tags,
		Layers:   dockerLayers,
	}

	// Write metadata files
	if err := writeJSON(tw, "oci-layout", map[string]string{"imageLayoutVersion": OCILayoutVersion}); err != nil {
		return fmt.Errorf("writing oci-layout: %w", err)
	}
	if err := writeJSON(tw, "index.json", rootIndex); err != nil {
		return fmt.Errorf("writing index.json: %w", err)
	}
	if err := writeJSON(tw, "manifest.json", []dockerManifest{dockerMfst}); err != nil {
		return fmt.Errorf("writing manifest.json: %w", err)
	}

	// Write blob directories
	if err := writeDir(tw, "blobs/"); err != nil {
		return err
	}
	if err := writeDir(tw, "blobs/sha256/"); err != nil {
		return err
	}

	// Collect blobs to write
	written := make(map[string]bool)

	// Write index blob from memory
	if err := writeFile(tw, filepath.Join("blobs", "sha256", indexDigest.Hex), indexData); err != nil {
		return fmt.Errorf("writing index blob: %w", err)
	}
	written[indexDigest.Hex] = true

	// Write manifest blobs from memory for included manifests
	for _, i := range included {
		info := manifestInfos[i]
		mfstDigest := hashBytes(info.ManifestData)
		if !written[mfstDigest.Hex] {
			if err := writeFile(tw, filepath.Join("blobs", "sha256", mfstDigest.Hex), info.ManifestData); err != nil {
				return fmt.Errorf("writing manifest blob: %w", err)
			}
			written[mfstDigest.Hex] = true
		}
	}

	// Stream config and layer blobs for included manifests
	var streamBlobs []string
	for _, i := range included {
		info := manifestInfos[i]
		if !written[info.ConfigDigest] {
			streamBlobs = append(streamBlobs, info.ConfigDigest)
			written[info.ConfigDigest] = true
		}
		for _, layerHex := range info.LayerDigests {
			if !written[layerHex] {
				streamBlobs = append(streamBlobs, layerHex)
				written[layerHex] = true
			}
		}
	}

	sort.Strings(streamBlobs)
	for _, d := range streamBlobs {
		if err := streamBlob(ctx, tw, d, blobSource, opts.ProgressFunc); err != nil {
			return fmt.Errorf("streaming blob %s: %w", d, err)
		}
	}

	return nil
}

func streamBlob(ctx context.Context, tw *tar.Writer, hexDigest string, source BlobSource, progressFunc func(ctx context.Context, size int64, name string) io.Writer) error {
	rc, size, err := source.OpenBlob(ctx, hexDigest)
	if err != nil {
		return err
	}
	defer rc.Close()

	hdr := &tar.Header{
		Name: "blobs/sha256/" + hexDigest,
		Mode: 0o644,
		Size: size,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("writing tar header: %w", err)
	}

	var reader io.Reader = rc
	if progressFunc != nil {
		pw := progressFunc(ctx, size, hexDigest[:12])
		if pw != nil {
			reader = io.TeeReader(rc, pw)
		}
	}

	if _, err := io.Copy(tw, reader); err != nil {
		return fmt.Errorf("copying blob data: %w", err)
	}

	return nil
}

func writeJSON(tw *tar.Writer, name string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", name, err)
	}
	return writeFile(tw, name, data)
}

func writeFile(tw *tar.Writer, name string, data []byte) error {
	hdr := &tar.Header{
		Name: filepath.ToSlash(name),
		Mode: 0o644,
		Size: int64(len(data)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func writeDir(tw *tar.Writer, name string) error {
	hdr := &tar.Header{
		Name:     name,
		Mode:     0o755,
		Typeflag: tar.TypeDir,
	}
	return tw.WriteHeader(hdr)
}
