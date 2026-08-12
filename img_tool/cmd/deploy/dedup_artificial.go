package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	registrytypes "github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/deployvfs"
)

// Artificial manifests: deduplicated_push_content=blobs_and_artificial_manifests.
//
// The deduplicated push puts a shared blob in one repository of a registry and asks
// the registry to serve it to the others. Some registries -- JFrog Artifactory among
// them -- will not do that for a blob that no manifest references: the upload sits in
// the repository it was sent to, invisible from every other one, and the cross-mount
// is refused. What they *do* honor is a blob a manifest points at: once a manifest
// references it, the blob is served to any repository the caller may read, which is
// what makes go-containerregistry's own existence check
// (HEAD /v2/<destination>/blobs/<digest>) answer 200 there and skip the upload
// entirely -- with or without a mount.
//
// So in this mode the home repository gets a manifest that references the blob: an
// ordinary single-layer image manifest, plus the config blob it needs. Nothing pulls
// it -- it exists to be a reference -- but it is a real manifest rather than a
// plausible-looking one: the layer descriptor is the blob's own, the config is a valid
// image config carrying the layer's uncompressed digest (diff id), and the media types
// are consistent, because a registry that refuses to store a blob without a manifest
// is not a registry to hand a malformed manifest to.
//
// It is written inside the same single-flight the blob upload runs in (see
// uploadDedupBlobs), so however many deploys of this process share the blob, one of
// them uploads it and writes the manifest, and the rest do neither. The manifest is
// pushed by digest and carries no tag, because it is not something anyone should
// resolve by name -- with the caveat that a registry policy which deletes untagged
// manifests can undo the blob's visibility along with it.

// artificialManifestAnnotation marks a manifest this deploy wrote only so that a
// registry would expose the layer it references to other repositories. It is there
// for whoever finds the manifest in a repository and wonders where it came from.
const artificialManifestAnnotation = "com.github.bazel-contrib.rules-img.artificial-blob-reference"

// emptyConfig is the config blob of an artificial manifest whose layer has no known
// diff id: the same "{}" this repo already pushes as the config of a SOCI index. A
// config that says nothing is preferable to one that says something untrue, and the
// manifest's job is to reference the layer either way.
const emptyConfig = "{}"

// publishArtificialManifest uploads a config blob for layer and creates a manifest in
// repo referencing both, so that a registry which only shares a blob a manifest
// references starts sharing this one.
//
// It runs after the layer's own upload, in the same single-flight: the manifest may
// only be created once the blob it references is in the repository, or the registry
// rejects it. diffID is the layer's uncompressed digest, or "" when it is not known
// (see diffIDIndex).
func publishArtificialManifest(ctx context.Context, pusher *remote.Pusher, repo name.Repository, layer registryv1.Layer, diffID string) error {
	desc, err := layerDescriptor(layer)
	if err != nil {
		return err
	}
	config, manifest, err := artificialManifest(desc, diffID)
	if err != nil {
		return err
	}

	// The config blob first: a manifest whose config is missing is not a manifest any
	// registry accepts.
	configLayer := deployvfs.NewLayer(config.descriptor, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(config.raw)), nil
	})
	if err := pusher.Upload(ctx, repo, configLayer); err != nil {
		return fmt.Errorf("uploading the artificial config blob %s: %w", config.descriptor.Digest, err)
	}
	ref := repo.Digest(manifest.digest.String())
	if err := pusher.Push(ctx, ref, manifest); err != nil {
		return fmt.Errorf("creating the artificial manifest %s: %w", ref, err)
	}
	return nil
}

// layerDescriptor reads a blob's descriptor off the layer serving it. The VFS answers
// all three from the descriptor recorded in the deploy manifest, so this reads nothing
// and hashes nothing.
func layerDescriptor(layer registryv1.Layer) (api.Descriptor, error) {
	digest, err := layer.Digest()
	if err != nil {
		return api.Descriptor{}, fmt.Errorf("reading blob digest: %w", err)
	}
	size, err := layer.Size()
	if err != nil {
		return api.Descriptor{}, fmt.Errorf("reading size of blob %s: %w", digest, err)
	}
	mediaType, err := layer.MediaType()
	if err != nil {
		return api.Descriptor{}, fmt.Errorf("reading media type of blob %s: %w", digest, err)
	}
	return api.Descriptor{MediaType: string(mediaType), Digest: digest.String(), Size: size}, nil
}

// artificialBlob is a blob built here rather than read from the VFS: its bytes and the
// descriptor they hash to.
type artificialBlob struct {
	raw        []byte
	descriptor api.Descriptor
}

// rawManifest is a manifest built here, pushed as-is. It is the minimum
// remote.Taggable: go-containerregistry hashes RawManifest to address it and reads
// MediaType for the Content-Type it is stored under.
type rawManifest struct {
	raw       []byte
	mediaType registrytypes.MediaType
	digest    registryv1.Hash
}

func (m rawManifest) RawManifest() ([]byte, error) { return m.raw, nil }

func (m rawManifest) MediaType() (registrytypes.MediaType, error) { return m.mediaType, nil }

// artificialManifest builds the config blob and the manifest that reference one layer.
//
// Everything about them is derived from the layer descriptor and the diff id, with no
// timestamp and no counter, so the same blob yields the same manifest on every run and
// in every process: re-running a deploy finds the manifest already there (a HEAD
// go-containerregistry does before every manifest PUT) and writes nothing.
func artificialManifest(layer api.Descriptor, diffID string) (artificialBlob, rawManifest, error) {
	config, err := artificialConfigBlob(layer, diffID)
	if err != nil {
		return artificialBlob{}, rawManifest{}, err
	}

	layerDescriptor, err := ociDescriptor(layer)
	if err != nil {
		return artificialBlob{}, rawManifest{}, fmt.Errorf("describing blob %s: %w", layer.Digest, err)
	}
	configDescriptor, err := ociDescriptor(config.descriptor)
	if err != nil {
		return artificialBlob{}, rawManifest{}, fmt.Errorf("describing the artificial config blob: %w", err)
	}
	manifestMediaType, _ := manifestMediaTypesFor(layer.MediaType)
	raw, err := json.Marshal(registryv1.Manifest{
		SchemaVersion: 2,
		MediaType:     manifestMediaType,
		Config:        configDescriptor,
		Layers:        []registryv1.Descriptor{layerDescriptor},
		Annotations:   map[string]string{artificialManifestAnnotation: layer.Digest},
	})
	if err != nil {
		return artificialBlob{}, rawManifest{}, fmt.Errorf("marshalling the artificial manifest for blob %s: %w", layer.Digest, err)
	}
	digest, _, err := registryv1.SHA256(bytes.NewReader(raw))
	if err != nil {
		return artificialBlob{}, rawManifest{}, fmt.Errorf("hashing the artificial manifest for blob %s: %w", layer.Digest, err)
	}
	return config, rawManifest{raw: raw, mediaType: manifestMediaType, digest: digest}, nil
}

// artificialConfigBlob builds the image config of an artificial manifest: a config
// whose single diff id is the layer's uncompressed digest.
//
// The platform is "unknown/unknown", the convention for a manifest that is not meant
// to be run (it is what buildkit writes for the attestation manifests it puts in an
// index), so a client that does resolve this manifest is told as much rather than
// being offered a broken image for its own architecture.
//
// Without a diff id there is nothing true to put in rootfs, so the config is "{}"
// instead of a made-up one.
func artificialConfigBlob(layer api.Descriptor, diffID string) (artificialBlob, error) {
	_, configMediaType := manifestMediaTypesFor(layer.MediaType)
	raw := []byte(emptyConfig)
	if diffID != "" {
		uncompressed, err := registryv1.NewHash(diffID)
		if err != nil {
			return artificialBlob{}, fmt.Errorf("parsing the diff id %q of blob %s: %w", diffID, layer.Digest, err)
		}
		raw, err = json.Marshal(struct {
			Architecture string            `json:"architecture"`
			OS           string            `json:"os"`
			Config       struct{}          `json:"config"`
			RootFS       registryv1.RootFS `json:"rootfs"`
		}{
			Architecture: "unknown",
			OS:           "unknown",
			RootFS: registryv1.RootFS{
				Type:    "layers",
				DiffIDs: []registryv1.Hash{uncompressed},
			},
		})
		if err != nil {
			return artificialBlob{}, fmt.Errorf("marshalling the artificial config for blob %s: %w", layer.Digest, err)
		}
	}
	digest, size, err := registryv1.SHA256(bytes.NewReader(raw))
	if err != nil {
		return artificialBlob{}, fmt.Errorf("hashing the artificial config for blob %s: %w", layer.Digest, err)
	}
	return artificialBlob{
		raw:        raw,
		descriptor: api.Descriptor{MediaType: string(configMediaType), Digest: digest.String(), Size: size},
	}, nil
}

// manifestMediaTypesFor returns the manifest and config media types to build an
// artificial manifest around a layer of the given media type.
//
// The families are not mixed: a Docker layer -- which is what a shallow base image's
// layers usually are -- goes into a Docker schema 2 manifest with a Docker config, an
// OCI layer (or anything else) into an OCI manifest with an OCI config. A registry
// strict enough to demand a manifest before sharing a blob is strict enough to have an
// opinion about that.
func manifestMediaTypesFor(layerMediaType string) (registrytypes.MediaType, registrytypes.MediaType) {
	if strings.HasPrefix(layerMediaType, "application/vnd.docker.") {
		return registrytypes.DockerManifestSchema2, registrytypes.DockerConfigJSON
	}
	return registrytypes.OCIManifestSchema1, registrytypes.OCIConfigJSON
}

// ociDescriptor converts a deploy manifest descriptor into the go-containerregistry
// one that is marshalled into a manifest.
func ociDescriptor(desc api.Descriptor) (registryv1.Descriptor, error) {
	digest, err := registryv1.NewHash(desc.Digest)
	if err != nil {
		return registryv1.Descriptor{}, err
	}
	return registryv1.Descriptor{
		MediaType: registrytypes.MediaType(desc.MediaType),
		Digest:    digest,
		Size:      desc.Size,
	}, nil
}

// diffIDIndex maps a layer's compressed digest to its uncompressed one, for the blobs
// that get an artificial manifest.
//
// The diff ids are not in the deploy manifest: they live in the config of the image
// that references the layer, which the VFS holds like any other blob. So they are read
// from there -- the config of a still-missing manifest whose layers this deploy is
// about to upload -- and aligned with the manifest's layer order, which is the order
// rootfs.diff_ids is defined in.
//
// A config that cannot be read or parsed costs its layers their diff id rather than
// the deploy: the manifest still references the blob, which is what the registry cares
// about. So does a manifest with no diff ids at all -- an artifact manifest whose
// config is "{}" (a SOCI index, an SBOM), whose "layers" are not filesystem changesets
// and have no uncompressed digest to record.
type diffIDIndex struct {
	byLayer map[string]string
}

// newDiffIDIndex reads the diff ids of the blobs the plan gives an artificial manifest
// to, from the configs of the working set's still-missing manifests. Each config is
// read once however many of its layers are involved.
func newDiffIDIndex(vfs *deployvfs.VFS, working []manifestDestination, plan *dedupPlan) *diffIDIndex {
	index := &diffIDIndex{byLayer: make(map[string]string)}

	// The blobs worth a config read: the ones an artificial manifest is written for.
	wanted := make(map[string]struct{})
	for key, flags := range plan.flags {
		if flags.artificial {
			wanted[key.digest] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return index
	}

	read := make(map[string]struct{})
	for _, manifest := range working {
		if _, done := read[manifest.dest.digest]; done {
			continue
		}
		if !index.needsAnyOf(manifest.layers, wanted) {
			continue
		}
		read[manifest.dest.digest] = struct{}{}
		if err := index.addFromManifest(vfs, manifest); err != nil {
			// The manifest still references the blob, so the mount still works; only the
			// config is less informative.
			fmt.Fprintf(os.Stderr, "warning: not recording diff ids of %s: %v\n", manifest.dest, err)
		}
	}
	return index
}

// needsAnyOf reports whether any of the layers is a blob whose diff id is wanted and
// still unknown.
func (i *diffIDIndex) needsAnyOf(layers []api.LayerBlob, wanted map[string]struct{}) bool {
	for _, layer := range layers {
		if _, want := wanted[layer.Digest]; !want {
			continue
		}
		if _, known := i.byLayer[layer.Digest]; !known {
			return true
		}
	}
	return false
}

// addFromManifest records the diff ids of one manifest's layers, read from its config.
func (i *diffIDIndex) addFromManifest(vfs *deployvfs.VFS, manifest manifestDestination) error {
	root, err := registryv1.NewHash(manifest.dest.digest)
	if err != nil {
		return fmt.Errorf("parsing manifest digest: %w", err)
	}
	image, err := vfs.Image(root)
	if err != nil {
		return err
	}
	parsed, err := image.Manifest()
	if err != nil {
		return err
	}
	config, err := image.ConfigFile()
	if err != nil {
		return err
	}
	for at, layer := range parsed.Layers {
		if at >= len(config.RootFS.DiffIDs) {
			break
		}
		diffID := config.RootFS.DiffIDs[at]
		if diffID.String() == "" {
			continue
		}
		i.byLayer[layer.Digest.String()] = diffID.String()
	}
	return nil
}

// diffIDFor returns the uncompressed digest of a layer, or "" when it is not known.
// A nil index knows nothing, which is what a deploy that writes no artificial
// manifests passes.
func (i *diffIDIndex) diffIDFor(digest string) string {
	if i == nil {
		return ""
	}
	return i.byLayer[digest]
}
