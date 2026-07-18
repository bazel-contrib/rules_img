package deploy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	registrytypes "github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/deployvfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/ocilayout"
)

// sinkKind identifies a --sink destination type.
type sinkKind int

const (
	sinkNone sinkKind = iota
	sinkOCITar
	sinkDockerSave
	sinkOCIDir
	sinkDistribution
	sinkDistributionFlat
)

// parseSink parses a --sink spec of the form "<type>:<path>".
func parseSink(spec string) (sinkKind, string, error) {
	typ, path, ok := strings.Cut(spec, ":")
	if !ok || path == "" {
		return sinkNone, "", fmt.Errorf("--sink must be in the form <type>:<path>, got %q", spec)
	}
	switch typ {
	case "oci-tar":
		return sinkOCITar, path, nil
	case "docker-save":
		return sinkDockerSave, path, nil
	case "oci":
		return sinkOCIDir, path, nil
	case "distribution":
		return sinkDistribution, path, nil
	case "distribution-flat":
		return sinkDistributionFlat, path, nil
	}
	return sinkNone, "", fmt.Errorf("unknown --sink type %q (want oci-tar, docker-save, oci, distribution, or distribution-flat)", typ)
}

// globalOnly reports whether a sink kind may only be set on the top-level
// command line (never inside a persistent-worker request). The incremental,
// dir-backed sinks are global-only; the isolated tar sinks may be per-request.
func (k sinkKind) globalOnly() bool {
	switch k {
	case sinkOCIDir, sinkDistribution, sinkDistributionFlat:
		return true
	}
	return false
}

// newSink opens/creates the sink for a parsed kind + path.
func newSink(kind sinkKind, path string) (sink, error) {
	switch kind {
	case sinkOCITar:
		return newTarSink(path, false), nil
	case sinkDockerSave:
		return newTarSink(path, true), nil
	case sinkOCIDir:
		return newOCIDirSink(path)
	case sinkDistribution:
		return newDistributionSink(path, false), nil
	case sinkDistributionFlat:
		return newDistributionSink(path, true), nil
	}
	return nil, fmt.Errorf("unknown --sink kind %d", kind)
}

// sink is a destination that captures push/load/registry_tag operations instead
// of pushing to a registry or loading into a daemon. Close finalizes the sink;
// for the incremental dir-backed sinks it is safe to call repeatedly (it flushes
// the current on-disk state), which the persistent worker relies on to keep the
// output valid between requests.
type sink interface {
	AddImage(ctx context.Context, img sinkImage) error
	Close() error
}

// resolvedRoot is one operation's root (image manifest or index) resolved from
// the VFS, with its child image manifests' blob sources.
type resolvedRoot struct {
	RootData     []byte
	MediaType    registrytypes.MediaType
	ArtifactType string
	IsIndex      bool
	Children     []ocilayout.ManifestInput
	Platform     *registryv1.Platform
}

// sinkImage is one operation routed to a sink: its resolved root plus the full
// image references (registry/repo:tag) it applies to. Registry/Repository carry
// the destination for distribution sinks when Refs is empty (a digest-only push).
type sinkImage struct {
	Refs       []string
	Registry   string
	Repository string
	Root       resolvedRoot
}

// sinkRouteOptions carries the deploy-time overrides and extra tags applied
// while routing operations into a sink.
type sinkRouteOptions struct {
	overrideRegistry   string
	overrideRepository string
	additionalTags     []string
}

// routeToSink captures every push, load and registry_tag operation into s and
// returns the sorted, de-duplicated list of full image references written. It
// does not Close s (the caller decides when to finalize).
func routeToSink(ctx context.Context, s sink, vfs *deployvfs.VFS, pushOps []api.IndexedPushDeployOperation, loadOps []api.IndexedLoadDeployOperation, tagOps []api.IndexedRegistryTagDeployOperation, opts sinkRouteOptions) ([]string, error) {
	var written []string
	add := func(registry, repository string, refs []string, rootKind, rootDigest string) error {
		root, err := resolveRoot(ctx, vfs, rootKind, rootDigest)
		if err != nil {
			return err
		}
		if err := s.AddImage(ctx, sinkImage{Refs: refs, Registry: registry, Repository: repository, Root: root}); err != nil {
			return err
		}
		written = append(written, refs...)
		return nil
	}

	for _, op := range pushOps {
		registry, repository := op.Registry, op.Repository
		if opts.overrideRegistry != "" {
			registry = opts.overrideRegistry
		}
		if opts.overrideRepository != "" {
			repository = opts.overrideRepository
		}
		bareTags := deduplicateAndSortTags(append(slices.Clone(op.Tags), opts.additionalTags...))
		refs := api.QualifyLoadTags(registry, repository, bareTags)
		if err := add(registry, repository, refs, op.RootKind, op.Root.Digest); err != nil {
			return nil, fmt.Errorf("push %s/%s: %w", registry, repository, err)
		}
	}

	for _, op := range loadOps {
		registry, repository := op.Registry, op.Repository
		if registry != "" && repository != "" {
			if opts.overrideRegistry != "" {
				registry = opts.overrideRegistry
			}
			if opts.overrideRepository != "" {
				repository = opts.overrideRepository
			}
		}
		refs := api.QualifyLoadTags(registry, repository, op.Tags)
		refs = append(refs, opts.additionalTags...)
		refs = deduplicateAndSortTags(refs)
		// distribution derives its repo from the parsed refs (rules_oci fallback)
		// or from registry/repository (split mode); pass whatever the op set.
		if err := add(registry, repository, refs, op.RootKind, op.Root.Digest); err != nil {
			return nil, fmt.Errorf("load %s: %w", op.Root.Digest, err)
		}
	}

	for _, op := range tagOps {
		registry, repository := op.Registry, op.Repository
		if opts.overrideRegistry != "" {
			registry = opts.overrideRegistry
		}
		if opts.overrideRepository != "" {
			repository = opts.overrideRepository
		}
		refs := api.QualifyLoadTags(registry, repository, deduplicateAndSortTags(op.Tags))
		if err := add(registry, repository, refs, op.RootKind, op.Root.Digest); err != nil {
			return nil, fmt.Errorf("registry_tag %s/%s: %w", registry, repository, err)
		}
	}

	return deduplicateAndSortTags(written), nil
}

// resolveRoot resolves an operation's root (and its children) from the VFS.
func resolveRoot(ctx context.Context, vfs *deployvfs.VFS, rootKind, rootDigest string) (resolvedRoot, error) {
	h, err := registryv1.NewHash(rootDigest)
	if err != nil {
		return resolvedRoot{}, err
	}
	src := vfsBlobSource{vfs: vfs}

	if rootKind == "index" {
		index, err := vfs.ImageIndex(h)
		if err != nil {
			return resolvedRoot{}, fmt.Errorf("getting image index %s: %w", rootDigest, err)
		}
		rawIndex, err := index.RawManifest()
		if err != nil {
			return resolvedRoot{}, err
		}
		im, err := index.IndexManifest()
		if err != nil {
			return resolvedRoot{}, err
		}
		root := resolvedRoot{RootData: rawIndex, MediaType: im.MediaType, IsIndex: true}
		for _, desc := range im.Manifests {
			img, err := vfs.Image(desc.Digest)
			if err != nil {
				return resolvedRoot{}, fmt.Errorf("getting image %s: %w", desc.Digest, err)
			}
			raw, err := img.RawManifest()
			if err != nil {
				return resolvedRoot{}, err
			}
			manifest, err := img.Manifest()
			if err != nil {
				return resolvedRoot{}, err
			}
			desc := desc
			root.Children = append(root.Children, ocilayout.ManifestInputFromVFS(src, manifest, raw, desc.Platform))
		}
		return root, nil
	}

	img, err := vfs.Image(h)
	if err != nil {
		return resolvedRoot{}, fmt.Errorf("getting image %s: %w", rootDigest, err)
	}
	raw, err := img.RawManifest()
	if err != nil {
		return resolvedRoot{}, err
	}
	manifest, err := img.Manifest()
	if err != nil {
		return resolvedRoot{}, err
	}
	return resolvedRoot{
		RootData:     raw,
		MediaType:    manifest.MediaType,
		ArtifactType: manifestArtifactType(manifest),
		IsIndex:      false,
		Children:     []ocilayout.ManifestInput{ocilayout.ManifestInputFromVFS(src, manifest, raw, nil)},
	}, nil
}

// manifestArtifactType mirrors ocilayout's internal artifactTypeOf: the config
// media type when it is not a standard image config, else "".
func manifestArtifactType(m *registryv1.Manifest) string {
	if m.Config.MediaType != "" && !m.Config.MediaType.IsConfig() {
		return string(m.Config.MediaType)
	}
	return ""
}

// vfsBlobSource adapts the deploy VFS to ocilayout.BlobSource. It mirrors the
// load pipeline's vfsBlobSource: layers first, then manifest/index blobs.
type vfsBlobSource struct {
	vfs *deployvfs.VFS
}

func (v vfsBlobSource) OpenBlob(ctx context.Context, hexDigest string) (io.ReadCloser, int64, error) {
	hash := registryv1.Hash{Algorithm: "sha256", Hex: hexDigest}
	layer, err := v.vfs.Layer(hash)
	if err != nil {
		layer, err = v.vfs.ManifestBlob(hash)
		if err != nil {
			return nil, 0, fmt.Errorf("blob %s not found in VFS", hexDigest)
		}
	}
	size, err := layer.Size()
	if err != nil {
		return nil, 0, fmt.Errorf("getting size for blob %s: %w", hexDigest, err)
	}
	rc, err := layer.Compressed()
	if err != nil {
		return nil, 0, fmt.Errorf("opening blob %s: %w", hexDigest, err)
	}
	return rc, size, nil
}

// tarSink accumulates every root and writes one OCI layout tar (optionally with
// a Docker manifest.json) at Close. index.json references all roots.
type tarSink struct {
	b        *ocilayout.Builder
	path     string
	docker   bool
	rootRefs []rootRef
}

type rootRef struct {
	isIndex bool
	refs    []string
}

func newTarSink(path string, docker bool) *tarSink {
	format := ocilayout.OCILayout().WithIndexStyle(ocilayout.IndexMultiRoot)
	if docker {
		format = ocilayout.DockerSave().WithIndexStyle(ocilayout.IndexMultiRoot)
	}
	return &tarSink{
		b:      ocilayout.New(format).WithMissingBlobsHint(ocilayout.OutputGroupTarball),
		path:   path,
		docker: docker,
	}
}

func (s *tarSink) AddImage(ctx context.Context, img sinkImage) error {
	root := img.Root
	s.b.AddRoot(ocilayout.RootInput{
		ManifestData: root.RootData,
		MediaType:    root.MediaType,
		ArtifactType: root.ArtifactType,
		IsIndex:      root.IsIndex,
		OCITags:      img.Refs,
		Children:     root.Children,
		Platform:     root.Platform,
	})
	if s.docker {
		s.rootRefs = append(s.rootRefs, rootRef{isIndex: root.IsIndex, refs: img.Refs})
	}
	return nil
}

func (s *tarSink) Close() error {
	if s.docker {
		s.b.WithTags(s.dockerRepoTags())
	}
	return s.b.WriteTar(context.Background(), s.path)
}

// dockerRepoTags picks the RepoTags for manifest.json: the refs of the first
// single-architecture image root (mirroring firstSingleArchManifest), else the
// first root's refs.
func (s *tarSink) dockerRepoTags() []string {
	for _, r := range s.rootRefs {
		if !r.isIndex && len(r.refs) > 0 {
			return r.refs
		}
	}
	for _, r := range s.rootRefs {
		if len(r.refs) > 0 {
			return r.refs
		}
	}
	return nil
}

// ociDirSink merges images into an OCI layout directory via ocilayout.Editor.
type ociDirSink struct {
	e *ocilayout.Editor
}

func newOCIDirSink(dir string) (*ociDirSink, error) {
	if _, err := os.Stat(filepath.Join(dir, "oci-layout")); err == nil {
		e, err := ocilayout.OpenDir(dir)
		if err != nil {
			return nil, err
		}
		return &ociDirSink{e: e}, nil
	}
	e, err := ocilayout.CreateDir(dir, ocilayout.OCILayout())
	if err != nil {
		return nil, err
	}
	return &ociDirSink{e: e}, nil
}

func (s *ociDirSink) AddImage(ctx context.Context, img sinkImage) error {
	root := img.Root
	if !root.IsIndex {
		if len(root.Children) == 0 {
			return fmt.Errorf("manifest root has no image manifest")
		}
		return s.e.AddManifest(ctx, root.Children[0], img.Refs...)
	}
	for i := range root.Children {
		if err := s.e.AddManifestBlobs(ctx, root.Children[i]); err != nil {
			return err
		}
	}
	indexHash, _, err := registryv1.SHA256(bytes.NewReader(root.RootData))
	if err != nil {
		return err
	}
	if err := s.e.AddBlob(ctx, indexHash, ocilayout.BlobFromBytes(root.RootData)); err != nil {
		return err
	}
	for _, d := range ocilayout.DescriptorsForTags(img.Refs, root.MediaType, root.RootData, indexHash, "", false) {
		if err := s.e.AddIndexEntry(d); err != nil {
			return err
		}
	}
	return nil
}

func (s *ociDirSink) Close() error { return s.e.Close() }

// distributionSink writes images into a static distribution-spec layout.
type distributionSink struct {
	w *ocilayout.DistributionWriter
}

func newDistributionSink(dir string, flat bool) *distributionSink {
	return &distributionSink{w: ocilayout.NewDistributionWriter(dir, flat)}
}

func (s *distributionSink) AddImage(ctx context.Context, img sinkImage) error {
	targets := make(map[ocilayout.DistributionRef][]string)
	var order []ocilayout.DistributionRef
	addTarget := func(ref ocilayout.DistributionRef, tag string) {
		if _, ok := targets[ref]; !ok {
			order = append(order, ref)
			targets[ref] = nil
		}
		if tag != "" {
			targets[ref] = append(targets[ref], tag)
		}
	}

	if len(img.Refs) == 0 {
		if img.Registry == "" || img.Repository == "" {
			return nil // no destination to place a digest-only image
		}
		addTarget(ocilayout.DistributionRef{Registry: img.Registry, Name: img.Repository}, "")
	} else {
		for _, ref := range img.Refs {
			dref, tag, err := parseDistributionRef(ref)
			if err != nil {
				return err
			}
			addTarget(dref, tag)
		}
	}

	for _, ref := range order {
		if err := s.w.AddImage(ctx, ocilayout.DistributionImage{
			Ref:      ref,
			RootData: img.Root.RootData,
			Children: img.Root.Children,
			Tags:     deduplicateAndSortTags(targets[ref]),
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *distributionSink) Close() error { return s.w.Close() }

// parseDistributionRef splits a full image reference into its distribution
// repository and tag components.
func parseDistributionRef(ref string) (ocilayout.DistributionRef, string, error) {
	parsed, err := name.ParseReference(ref, name.WithDefaultRegistry(""))
	if err != nil {
		return ocilayout.DistributionRef{}, "", fmt.Errorf("parsing image reference %q: %w", ref, err)
	}
	dref := ocilayout.DistributionRef{
		Registry: parsed.Context().RegistryStr(),
		Name:     parsed.Context().RepositoryStr(),
	}
	tag := ""
	if t, ok := parsed.(name.Tag); ok {
		tag = t.TagStr()
	}
	return dref, tag, nil
}

func deduplicateAndSortTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	out := slices.Clone(tags)
	sort.Strings(out)
	out = slices.Compact(out)
	filtered := out[:0]
	for _, t := range out {
		if t != "" {
			filtered = append(filtered, t)
		}
	}
	return filtered
}
