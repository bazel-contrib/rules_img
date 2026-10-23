package imagearchive

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/containerd/platforms"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	specv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

// Platform identifies a container target platform (OS, CPU architecture, and optional CPU variant).
type Platform struct {
	OS           string
	Architecture string
	Variant      string
}

func (p Platform) canonical() specv1.Platform {
	return platforms.Normalize(specv1.Platform{
		OS:           p.OS,
		Architecture: p.Architecture,
		Variant:      p.Variant,
	})
}

// DescriptorPlatform returns the normalized OCI descriptor Platform, applying rules_img's
// output convention of spelling the default arm64 variant explicitly as "v8".
func (p Platform) DescriptorPlatform() specv1.Platform {
	n := p.canonical()
	if n.Architecture == "arm64" && n.Variant == "" {
		n.Variant = "v8"
	}
	return n
}

// Matches reports whether p and want refer to the same containerd-normalized platform.
func (p Platform) Matches(want Platform) bool {
	a, b := p.canonical(), want.canonical()
	return a.OS == b.OS && a.Architecture == b.Architecture && a.Variant == b.Variant
}

func (p Platform) String() string {
	d := p.DescriptorPlatform()
	if d.Variant != "" {
		return d.OS + "/" + d.Architecture + "/" + d.Variant
	}
	return d.OS + "/" + d.Architecture
}

func platformFromOCI(p *specv1.Platform) Platform {
	if p == nil {
		return Platform{}
	}
	return Platform{OS: p.OS, Architecture: p.Architecture, Variant: p.Variant}
}

// ValidateConfigPlatform checks that config matches non-empty OS, Architecture, and explicit Variant fields.
func ValidateConfigPlatform(config *specv1.Image, want Platform) error {
	w := want.canonical()
	c := (Platform{OS: config.OS, Architecture: config.Architecture, Variant: config.Variant}).canonical()
	if want.OS != "" && c.OS != w.OS {
		return fmt.Errorf("OS mismatch: config has %s, but %s was requested", config.OS, w.OS)
	}
	if want.Architecture != "" && c.Architecture != w.Architecture {
		return fmt.Errorf("architecture mismatch: config has %s, but %s was requested", config.Architecture, w.Architecture)
	}
	if config.Variant != "" && want.Architecture != "" && c.Variant != w.Variant {
		return fmt.Errorf("variant mismatch: config has %s, but %s was requested", config.Variant, want.DescriptorPlatform().Variant)
	}
	return nil
}

func validateStrictPlatform(prefix string, have, want Platform) error {
	h, w := have.canonical(), want.canonical()
	if h.OS != w.OS {
		return fmt.Errorf("%sOS mismatch: has %s, but %s was requested", prefix, have.OS, w.OS)
	}
	if h.Architecture != w.Architecture {
		return fmt.Errorf("%sarchitecture mismatch: has %s, but %s was requested", prefix, have.Architecture, w.Architecture)
	}
	if h.Variant != w.Variant {
		return fmt.Errorf("%svariant mismatch: has %s, but %s was requested", prefix, have.DescriptorPlatform().Variant, want.DescriptorPlatform().Variant)
	}
	return nil
}

// LoadedImage wraps a standard go-containerregistry v1.Image together with its parsed OCI metadata.
// ManifestDesc is non-nil for OCI layout/archive and explicit manifest sources, and nil for classic Docker save archives.
type LoadedImage struct {
	Image        v1.Image
	ManifestDesc *specv1.Descriptor
	RawManifest  []byte
	Manifest     specv1.Manifest
	RawConfig    []byte
	Config       specv1.Image
}

// Source provides unified single-platform and indexed multi-platform access to an OCI layout directory,
// an uncompressed OCI layout .tar archive, or a classic Docker save .tar archive.
type Source struct {
	tar           *IndexedTar
	v1Index       v1.ImageIndex
	ociIndex      specv1.Index
	dockerEntries tarball.Manifest
}

// Close releases any underlying archive file descriptor.
func (s *Source) Close() error {
	if s == nil || s.tar == nil {
		return nil
	}
	return s.tar.Close()
}

// OpenArchive opens an uncompressed OCI layout or classic Docker save .tar archive.
func OpenArchive(tarPath, dirHintCmd string) (*Source, error) {
	fi, err := os.Stat(tarPath)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", tarPath, err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("%s is a directory; use 'img %s' for OCI layout directories", tarPath, dirHintCmd)
	}
	return openTarSource(tarPath)
}

// OpenSource opens either an OCI layout directory or an uncompressed OCI / Docker save .tar archive.
func OpenSource(srcPath string) (*Source, error) {
	fi, err := os.Stat(srcPath)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", srcPath, err)
	}
	if fi.IsDir() {
		idx, err := newOCIStoreIndex(dirOCIStore(srcPath))
		if err != nil {
			return nil, fmt.Errorf("opening OCI layout directory %s: %w", srcPath, err)
		}
		return newOCISource(nil, idx)
	}
	return openTarSource(srcPath)
}

func openTarSource(tarPath string) (*Source, error) {
	tarIdx, err := OpenIndexedTar(tarPath)
	if err != nil {
		return nil, err
	}
	if tarIdx.Has("index.json") {
		idx, err := newOCIStoreIndex(tarOCIStore(tarIdx))
		if err != nil {
			tarIdx.Close()
			return nil, err
		}
		src, err := newOCISource(tarIdx, idx)
		if err != nil {
			tarIdx.Close()
			return nil, err
		}
		return src, nil
	}
	if tarIdx.Has("manifest.json") {
		raw, err := tarIdx.ReadBytes("manifest.json")
		if err != nil {
			tarIdx.Close()
			return nil, fmt.Errorf("reading manifest.json: %w", err)
		}
		var entries tarball.Manifest
		if err := json.Unmarshal(raw, &entries); err != nil {
			tarIdx.Close()
			return nil, fmt.Errorf("unmarshaling manifest.json: %w", err)
		}
		return &Source{tar: tarIdx, dockerEntries: entries}, nil
	}
	tarIdx.Close()
	return nil, fmt.Errorf("unsupported archive %s: neither index.json (OCI layout) nor manifest.json (Docker save) found", tarPath)
}

func newOCISource(tar *IndexedTar, idx v1.ImageIndex) (*Source, error) {
	const maxIndexNesting = 4
	for range maxIndexNesting {
		im, err := idx.IndexManifest()
		if err != nil {
			return nil, fmt.Errorf("parsing index.json: %w", err)
		}
		if len(im.Manifests) == 0 {
			break
		}
		first := im.Manifests[0]
		allSameNested := true
		for _, m := range im.Manifests {
			if !m.MediaType.IsIndex() || m.Digest != first.Digest {
				allSameNested = false
				break
			}
		}
		if !allSameNested {
			break
		}
		idx, err = idx.ImageIndex(first.Digest)
		if err != nil {
			return nil, fmt.Errorf("reading nested index %s: %w", first.Digest, err)
		}
	}

	rawIdx, err := idx.RawManifest()
	if err != nil {
		return nil, fmt.Errorf("reading raw index manifest: %w", err)
	}
	var ociIndex specv1.Index
	if err := json.Unmarshal(rawIdx, &ociIndex); err != nil {
		return nil, fmt.Errorf("unmarshaling index manifest: %w", err)
	}
	return &Source{tar: tar, v1Index: idx, ociIndex: ociIndex}, nil
}

// ManifestCount returns the number of platform/image entries in the source.
func (s *Source) ManifestCount() int {
	if s.v1Index != nil {
		return len(s.ociIndex.Manifests)
	}
	return len(s.dockerEntries)
}

// IndexAnnotations returns top-level OCI index annotations when present.
func (s *Source) IndexAnnotations() map[string]string { return s.ociIndex.Annotations }

// IndexSubject returns the top-level OCI index subject descriptor when present.
func (s *Source) IndexSubject() *specv1.Descriptor { return s.ociIndex.Subject }

// ImageAt loads the image at manifestIdx and verifies that its platform matches want.
func (s *Source) ImageAt(manifestIdx int, want Platform) (*LoadedImage, error) {
	prefix := fmt.Sprintf("manifest index %d: platform ", manifestIdx)
	if s.v1Index != nil {
		if manifestIdx < 0 || manifestIdx >= len(s.ociIndex.Manifests) {
			return nil, fmt.Errorf("manifest index %d out of range (OCI index has %d manifests)", manifestIdx, len(s.ociIndex.Manifests))
		}
		desc := &s.ociIndex.Manifests[manifestIdx]
		if desc.Platform == nil {
			return nil, fmt.Errorf("manifest at index %d in OCI index has no platform information", manifestIdx)
		}
		if err := validateStrictPlatform(prefix, platformFromOCI(desc.Platform), want); err != nil {
			return nil, err
		}
		return s.loadOCIManifest(desc, want)
	}

	if manifestIdx < 0 || manifestIdx >= len(s.dockerEntries) {
		return nil, fmt.Errorf("manifest index %d out of range (manifest.json has %d entries)", manifestIdx, len(s.dockerEntries))
	}
	rawCfg, cfg, err := s.readDockerConfig(&s.dockerEntries[manifestIdx])
	if err != nil {
		return nil, err
	}
	have := Platform{OS: cfg.OS, Architecture: cfg.Architecture, Variant: cfg.Variant}
	if err := validateStrictPlatform(prefix, have, want); err != nil {
		return nil, err
	}
	return s.buildDockerImage(&s.dockerEntries[manifestIdx], rawCfg, cfg)
}

// ImageForPlatform finds and loads the image matching want.
func (s *Source) ImageForPlatform(want Platform) (*LoadedImage, error) {
	if s.v1Index != nil {
		desc, err := s.findOCIManifestForPlatform(want)
		if err != nil {
			return nil, err
		}
		return s.loadOCIManifest(desc, want)
	}

	if len(s.dockerEntries) == 0 {
		return nil, fmt.Errorf("manifest.json in archive contains no image entries")
	}
	if len(s.dockerEntries) == 1 {
		rawCfg, cfg, err := s.readDockerConfig(&s.dockerEntries[0])
		if err != nil {
			return nil, err
		}
		if err := ValidateConfigPlatform(&cfg, want); err != nil {
			return nil, err
		}
		return s.buildDockerImage(&s.dockerEntries[0], rawCfg, cfg)
	}
	for i := range s.dockerEntries {
		rawCfg, cfg, err := s.readDockerConfig(&s.dockerEntries[i])
		if err != nil {
			return nil, err
		}
		if (Platform{OS: cfg.OS, Architecture: cfg.Architecture, Variant: cfg.Variant}).Matches(want) {
			return s.buildDockerImage(&s.dockerEntries[i], rawCfg, cfg)
		}
	}
	return nil, fmt.Errorf("no image entry in manifest.json matches platform %s", want)
}

func (s *Source) findOCIManifestForPlatform(want Platform) (*specv1.Descriptor, error) {
	manifests := s.ociIndex.Manifests
	for i := range manifests {
		if manifests[i].Platform != nil && platformFromOCI(manifests[i].Platform).Matches(want) {
			return &manifests[i], nil
		}
	}
	if len(manifests) == 1 {
		return &manifests[0], nil
	}

	seen := make(map[string]bool)
	var unplatformed []*specv1.Descriptor
	hasPlatformed := false
	for i := range manifests {
		if manifests[i].Platform != nil {
			hasPlatformed = true
			continue
		}
		key := manifests[i].Digest.String()
		if !seen[key] {
			seen[key] = true
			unplatformed = append(unplatformed, &manifests[i])
		}
	}
	if !hasPlatformed && len(unplatformed) == 1 {
		return unplatformed[0], nil
	}
	for _, desc := range unplatformed {
		if img, err := s.loadOCIManifest(desc, Platform{}); err == nil {
			if (Platform{OS: img.Config.OS, Architecture: img.Config.Architecture, Variant: img.Config.Variant}).Matches(want) {
				return desc, nil
			}
		}
	}
	return nil, fmt.Errorf("no manifest found for platform %s", want)
}

func (s *Source) loadOCIManifest(manifestDesc *specv1.Descriptor, want Platform) (*LoadedImage, error) {
	if manifestDesc.Platform != nil && want.Architecture != "" {
		if err := validateStrictPlatform("manifest descriptor ", platformFromOCI(manifestDesc.Platform), want); err != nil {
			return nil, err
		}
	}
	h, err := v1.NewHash(manifestDesc.Digest.String())
	if err != nil {
		return nil, fmt.Errorf("invalid manifest digest %s: %w", manifestDesc.Digest, err)
	}
	v1Img, err := s.v1Index.Image(h)
	if err != nil {
		return nil, fmt.Errorf("loading image %s: %w", h, err)
	}
	return wrapV1Image(v1Img, manifestDesc, want)
}

func wrapV1Image(v1Img v1.Image, manifestDesc *specv1.Descriptor, want Platform) (*LoadedImage, error) {
	rawManifest, err := v1Img.RawManifest()
	if err != nil {
		return nil, fmt.Errorf("reading manifest blob: %w", err)
	}
	var manifest specv1.Manifest
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		return nil, fmt.Errorf("unmarshaling manifest: %w", err)
	}

	rawConfig, err := v1Img.RawConfigFile()
	if err != nil {
		return nil, fmt.Errorf("reading config blob: %w", err)
	}
	var config specv1.Image
	if err := json.Unmarshal(rawConfig, &config); err != nil {
		return nil, fmt.Errorf("unmarshaling config: %w", err)
	}

	if err := ValidateConfigPlatform(&config, want); err != nil {
		return nil, err
	}
	if len(config.RootFS.DiffIDs) != len(manifest.Layers) {
		return nil, fmt.Errorf("config rootfs.diff_ids count (%d) does not match manifest layer count (%d)", len(config.RootFS.DiffIDs), len(manifest.Layers))
	}

	descCopy := *manifestDesc
	return &LoadedImage{
		Image:        v1Img,
		ManifestDesc: &descCopy,
		RawManifest:  rawManifest,
		Manifest:     manifest,
		RawConfig:    rawConfig,
		Config:       config,
	}, nil
}

// ValidateTarLayers rejects images whose layers are not standard OCI/Docker tar changesets.
// Non-tar layers (for example EROFS blobs) cannot be extracted or merged: go-containerregistry
// reads them as an uncompressed tar stream, which silently yields an empty filesystem.
// Classic Docker save archives carry no layer media types and are always tars, so they pass.
func ValidateTarLayers(img *LoadedImage) error {
	for i, desc := range img.Manifest.Layers {
		if WireCompression(desc.MediaType) == "" {
			return fmt.Errorf("layer %d has media type %q, which is not a tar changeset: only standard OCI/Docker tar layers can be flattened", i, desc.MediaType)
		}
	}
	return nil
}

// LoadFromManifestAndBlobs loads an image from explicit manifest, config, and ordered layer blob files.
func LoadFromManifestAndBlobs(manifestPath, configPath string, layerPaths []string) (*LoadedImage, error) {
	rawManifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("reading source manifest %s: %w", manifestPath, err)
	}
	var v1Manifest v1.Manifest
	if err := json.Unmarshal(rawManifest, &v1Manifest); err != nil {
		return nil, fmt.Errorf("unmarshaling source manifest %s: %w", manifestPath, err)
	}
	if len(layerPaths) != len(v1Manifest.Layers) {
		return nil, fmt.Errorf("layer count mismatch: manifest has %d layers, but %d --layer flags were given", len(v1Manifest.Layers), len(layerPaths))
	}

	rawConfig, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("reading source config %s: %w", configPath, err)
	}

	byDigest := make(map[v1.Hash]partial.CompressedLayer, len(layerPaths))
	for i, desc := range v1Manifest.Layers {
		p := layerPaths[i]
		byDigest[desc.Digest] = &diskBlobLayer{desc: desc, path: p}
	}
	mt := v1Manifest.MediaType
	if mt == "" {
		mt = types.OCIManifestSchema1
	}
	v1Img, err := partial.CompressedToImage(&explicitImageCore{
		mediaType:   mt,
		rawManifest: rawManifest,
		rawConfig:   rawConfig,
		byDigest:    byDigest,
	})
	if err != nil {
		return nil, err
	}

	desc := specv1.Descriptor{
		MediaType: api.ImageManifestMediaType(string(mt)),
		Size:      int64(len(rawManifest)),
	}
	return wrapV1Image(v1Img, &desc, Platform{})
}

func (s *Source) readDockerConfig(entry *tarball.Descriptor) ([]byte, specv1.Image, error) {
	rawCfg, err := s.tar.ReadBytes(entry.Config)
	if err != nil {
		return nil, specv1.Image{}, fmt.Errorf("reading config %s: %w", entry.Config, err)
	}
	var cfg specv1.Image
	if err := json.Unmarshal(rawCfg, &cfg); err != nil {
		return nil, specv1.Image{}, fmt.Errorf("unmarshaling config %s: %w", entry.Config, err)
	}
	return rawCfg, cfg, nil
}

func (s *Source) buildDockerImage(entry *tarball.Descriptor, rawCfg []byte, cfg specv1.Image) (*LoadedImage, error) {
	if len(cfg.RootFS.DiffIDs) != len(entry.Layers) {
		return nil, fmt.Errorf("config rootfs.diff_ids count (%d) does not match manifest.json layer count (%d)", len(cfg.RootFS.DiffIDs), len(entry.Layers))
	}
	byDiffID := make(map[v1.Hash]partial.UncompressedLayer, len(entry.Layers))
	for i, layerPath := range entry.Layers {
		_, size, err := s.tar.OpenEntry(layerPath)
		if err != nil {
			return nil, fmt.Errorf("layer %d (%s) referenced by manifest.json not found in archive: %w", i, layerPath, err)
		}
		diffHash, err := v1.NewHash(cfg.RootFS.DiffIDs[i].String())
		if err != nil {
			return nil, fmt.Errorf("invalid diff_id %s: %w", cfg.RootFS.DiffIDs[i], err)
		}
		v1Layer, err := partial.CompressedToLayer(&dockerSaveBlob{
			tar:    s.tar,
			path:   layerPath,
			size:   size,
			diffID: diffHash,
		})
		if err != nil {
			return nil, err
		}
		byDiffID[diffHash] = v1Layer
	}
	v1Img, err := partial.UncompressedToImage(&dockerSaveImageCore{
		rawConfig: rawCfg,
		byDiffID:  byDiffID,
	})
	if err != nil {
		return nil, err
	}
	return &LoadedImage{
		Image:     v1Img,
		RawConfig: rawCfg,
		Config:    cfg,
	}, nil
}

// --- go-containerregistry v1.ImageIndex / partial adapters for OCI directories & zero-extraction tar archives ---
// Note: go-containerregistry/pkg/v1/layout rejects symlinked blobs via Lstat, which breaks Bazel sandbox
// TreeArtifacts and sparse_oci_layout symlink trees. Backing partial.CompressedToImage with ociStore
// shares one v1.ImageIndex implementation across OCI directories and .tar archives while allowing sandbox symlinks.

type ociStore struct {
	readBytes func(relPath string) ([]byte, error)
	openFile  func(relPath string) (io.ReadCloser, error)
}

func dirOCIStore(dir string) *ociStore {
	resolve := func(relPath string) (string, error) {
		cleaned, err := CleanTarPath(relPath)
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, filepath.FromSlash(cleaned)), nil
	}
	return &ociStore{
		readBytes: func(relPath string) ([]byte, error) {
			full, err := resolve(relPath)
			if err != nil {
				return nil, err
			}
			return os.ReadFile(full)
		},
		openFile: func(relPath string) (io.ReadCloser, error) {
			full, err := resolve(relPath)
			if err != nil {
				return nil, err
			}
			return os.Open(full)
		},
	}
}

func tarOCIStore(tar *IndexedTar) *ociStore {
	return &ociStore{
		readBytes: tar.ReadBytes,
		openFile: func(relPath string) (io.ReadCloser, error) {
			sr, _, err := tar.OpenEntry(relPath)
			if err != nil {
				return nil, err
			}
			return io.NopCloser(sr), nil
		},
	}
}

type ociStoreIndex struct {
	store     *ociStore
	mediaType types.MediaType
	rawIndex  []byte
}

func newOCIStoreIndex(store *ociStore) (v1.ImageIndex, error) {
	raw, err := store.readBytes("index.json")
	if err != nil {
		return nil, fmt.Errorf("reading index.json: %w", err)
	}
	return &ociStoreIndex{store: store, mediaType: types.OCIImageIndex, rawIndex: raw}, nil
}

func (i *ociStoreIndex) MediaType() (types.MediaType, error) { return i.mediaType, nil }
func (i *ociStoreIndex) Digest() (v1.Hash, error)            { return partial.Digest(i) }
func (i *ociStoreIndex) Size() (int64, error)                { return partial.Size(i) }
func (i *ociStoreIndex) RawManifest() ([]byte, error)        { return i.rawIndex, nil }
func (i *ociStoreIndex) IndexManifest() (*v1.IndexManifest, error) {
	var im v1.IndexManifest
	return &im, json.Unmarshal(i.rawIndex, &im)
}

func (i *ociStoreIndex) findDesc(h v1.Hash) (v1.Descriptor, error) {
	im, err := i.IndexManifest()
	if err != nil {
		return v1.Descriptor{}, err
	}
	for _, d := range im.Manifests {
		if d.Digest == h {
			return d, nil
		}
	}
	return v1.Descriptor{}, fmt.Errorf("descriptor %s not found in OCI index", h)
}

func (i *ociStoreIndex) Image(h v1.Hash) (v1.Image, error) {
	desc, err := i.findDesc(h)
	if err != nil {
		return nil, err
	}
	if desc.MediaType.IsIndex() {
		return nil, fmt.Errorf("descriptor %s is an index (%s), not an image manifest", h, desc.MediaType)
	}
	return partial.CompressedToImage(&ociStoreImage{store: i.store, desc: desc})
}

func (i *ociStoreIndex) ImageIndex(h v1.Hash) (v1.ImageIndex, error) {
	desc, err := i.findDesc(h)
	if err != nil {
		return nil, err
	}
	raw, err := i.store.readBytes(path.Join("blobs", h.Algorithm, h.Hex))
	if err != nil {
		return nil, err
	}
	return &ociStoreIndex{store: i.store, mediaType: desc.MediaType, rawIndex: raw}, nil
}

type ociStoreImage struct {
	store       *ociStore
	desc        v1.Descriptor
	rawManifest []byte
}

func (img *ociStoreImage) MediaType() (types.MediaType, error) { return img.desc.MediaType, nil }
func (img *ociStoreImage) RawManifest() ([]byte, error) {
	if img.rawManifest != nil {
		return img.rawManifest, nil
	}
	b, err := img.store.readBytes(path.Join("blobs", img.desc.Digest.Algorithm, img.desc.Digest.Hex))
	if err != nil {
		return nil, err
	}
	img.rawManifest = b
	return b, nil
}

func (img *ociStoreImage) RawConfigFile() ([]byte, error) {
	m, err := partial.Manifest(img)
	if err != nil {
		return nil, err
	}
	return img.store.readBytes(path.Join("blobs", m.Config.Digest.Algorithm, m.Config.Digest.Hex))
}

func (img *ociStoreImage) LayerByDigest(h v1.Hash) (partial.CompressedLayer, error) {
	m, err := partial.Manifest(img)
	if err != nil {
		return nil, err
	}
	for _, d := range m.Layers {
		if d.Digest == h {
			return &ociStoreBlob{store: img.store, desc: d}, nil
		}
	}
	return nil, fmt.Errorf("layer %s not found in manifest", h)
}

type ociStoreBlob struct {
	store *ociStore
	desc  v1.Descriptor
}

func (b *ociStoreBlob) Digest() (v1.Hash, error)            { return b.desc.Digest, nil }
func (b *ociStoreBlob) Size() (int64, error)                { return b.desc.Size, nil }
func (b *ociStoreBlob) MediaType() (types.MediaType, error) { return b.desc.MediaType, nil }
func (b *ociStoreBlob) Compressed() (io.ReadCloser, error) {
	return b.store.openFile(path.Join("blobs", b.desc.Digest.Algorithm, b.desc.Digest.Hex))
}

type explicitImageCore struct {
	mediaType   types.MediaType
	rawManifest []byte
	rawConfig   []byte
	byDigest    map[v1.Hash]partial.CompressedLayer
}

func (c *explicitImageCore) MediaType() (types.MediaType, error) { return c.mediaType, nil }
func (c *explicitImageCore) RawManifest() ([]byte, error)        { return c.rawManifest, nil }
func (c *explicitImageCore) RawConfigFile() ([]byte, error)      { return c.rawConfig, nil }
func (c *explicitImageCore) LayerByDigest(h v1.Hash) (partial.CompressedLayer, error) {
	l, ok := c.byDigest[h]
	if !ok {
		return nil, fmt.Errorf("layer %s not found", h)
	}
	return l, nil
}

type diskBlobLayer struct {
	desc v1.Descriptor
	path string
}

func (l *diskBlobLayer) Digest() (v1.Hash, error)            { return l.desc.Digest, nil }
func (l *diskBlobLayer) Size() (int64, error)                { return l.desc.Size, nil }
func (l *diskBlobLayer) MediaType() (types.MediaType, error) { return l.desc.MediaType, nil }
func (l *diskBlobLayer) Compressed() (io.ReadCloser, error)  { return os.Open(l.path) }

type dockerSaveImageCore struct {
	rawConfig []byte
	byDiffID  map[v1.Hash]partial.UncompressedLayer
}

func (c *dockerSaveImageCore) MediaType() (types.MediaType, error) {
	return types.DockerManifestSchema2, nil
}
func (c *dockerSaveImageCore) RawConfigFile() ([]byte, error) { return c.rawConfig, nil }
func (c *dockerSaveImageCore) LayerByDiffID(h v1.Hash) (partial.UncompressedLayer, error) {
	l, ok := c.byDiffID[h]
	if !ok {
		return nil, fmt.Errorf("layer with diff_id %s not found", h)
	}
	return l, nil
}

type dockerSaveBlob struct {
	tar    *IndexedTar
	path   string
	size   int64
	diffID v1.Hash
}

func (b *dockerSaveBlob) DiffID() (v1.Hash, error)            { return b.diffID, nil }
func (b *dockerSaveBlob) Size() (int64, error)                { return b.size, nil }
func (b *dockerSaveBlob) MediaType() (types.MediaType, error) { return types.DockerLayer, nil }
func (b *dockerSaveBlob) Compressed() (io.ReadCloser, error) {
	sr, _, err := b.tar.OpenEntry(b.path)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(sr), nil
}
func (b *dockerSaveBlob) Digest() (v1.Hash, error) {
	rc, err := b.Compressed()
	if err != nil {
		return v1.Hash{}, err
	}
	defer rc.Close()
	h, _, err := v1.SHA256(rc)
	return h, err
}
