package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/pkg/blobcache"
	"go.podman.io/image/v5/signature"
	imgstorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

func insecurePolicyContext() (*signature.PolicyContext, error) {
	policy := &signature.Policy{Default: []signature.PolicyRequirement{signature.NewPRInsecureAcceptAnything()}}
	policyCtx, err := signature.NewPolicyContext(policy)
	if err != nil {
		return nil, fmt.Errorf("creating policy context: %w", err)
	}
	return policyCtx, nil
}

func openStore() (storage.Store, error) {
	opts, err := storage.DefaultStoreOptions()
	if err != nil {
		return nil, fmt.Errorf("resolving default store options: %w", err)
	}
	opts.GraphDriverName = "overlay"

	store, err := storage.GetStore(opts)
	if err != nil {
		return nil, fmt.Errorf("opening container store: %w", err)
	}
	return store, nil
}

func uniqueBaseName() (reference.Named, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, fmt.Errorf("generating unique base name: %w", err)
	}
	name := fmt.Sprintf("localhost/buildah-tool-base-%x:latest", buf)
	named, err := reference.ParseNormalizedNamed(name)
	if err != nil {
		return nil, fmt.Errorf("parsing base name %q: %w", name, err)
	}
	return named, nil
}

func importBaseImage(ctx context.Context, store storage.Store, named reference.Named, blobCacheDir string, src *baseImageSource) error {
	destRef, err := imgstorage.Transport.NewStoreReference(store, named, "")
	if err != nil {
		return fmt.Errorf("building store reference for %s: %w", named.String(), err)
	}

	cacheRef, err := blobcache.NewBlobCache(destRef, blobCacheDir, types.PreserveOriginal)
	if err != nil {
		return fmt.Errorf("wrapping store reference with blob cache %q: %w", blobCacheDir, err)
	}

	policyCtx, err := insecurePolicyContext()
	if err != nil {
		return err
	}
	defer func() { _ = policyCtx.Destroy() }()

	if _, err := copy.Image(ctx, policyCtx, cacheRef, src.ref, &copy.Options{}); err != nil {
		return fmt.Errorf("importing base image into store: %w", err)
	}
	return nil
}

var (
	_ types.ImageSource    = (*baseImageSource)(nil)
	_ types.ImageReference = (*baseImageReference)(nil)
	_ types.ImageTransport = baseImageTransport{}
)

type baseImageSource struct {
	ref          types.ImageReference
	manifest     []byte
	manifestType string
	localBlobs   map[string]string
	upstream     types.ImageSource
}

func newUpstreamSource(ctx context.Context, registryHost, repository string, manifestDigest digest.Digest) (types.ImageSource, error) {
	named, err := reference.WithName(registryHost + "/" + repository)
	if err != nil {
		return nil, fmt.Errorf("building upstream name: %w", err)
	}
	digested, err := reference.WithDigest(named, manifestDigest)
	if err != nil {
		return nil, fmt.Errorf("attaching upstream digest: %w", err)
	}
	ref, err := docker.NewReference(digested)
	if err != nil {
		return nil, fmt.Errorf("building upstream reference: %w", err)
	}
	src, err := ref.NewImageSource(ctx, &types.SystemContext{})
	if err != nil {
		return nil, fmt.Errorf("opening upstream image source: %w", err)
	}
	return src, nil
}

func newBaseImageSource(named reference.Named, manifest []byte, manifestType string, localBlobs map[string]string, upstream types.ImageSource) *baseImageSource {
	src := &baseImageSource{
		manifest:     manifest,
		manifestType: manifestType,
		localBlobs:   localBlobs,
		upstream:     upstream,
	}
	src.ref = &baseImageReference{named: named, src: src}
	return src
}

func buildBaseSource(named reference.Named, manifestPath, configPath, stagingDir string, localBlobs map[string]string, upstream types.ImageSource) (*baseImageSource, error) {
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("reading base manifest: %w", err)
	}
	var mf v1.Manifest
	if err := json.Unmarshal(manifestData, &mf); err != nil {
		return nil, fmt.Errorf("parsing base manifest: %w", err)
	}

	patchedConfigPath, patched, err := patchBaseConfigHistory(configPath, stagingDir, len(mf.Layers))
	if err != nil {
		return nil, fmt.Errorf("patching base config history: %w", err)
	}
	if patched {
		patchedBytes, err := os.ReadFile(patchedConfigPath)
		if err != nil {
			return nil, fmt.Errorf("reading patched base config: %w", err)
		}
		mf.Config.Digest = digest.FromBytes(patchedBytes)
		mf.Config.Size = int64(len(patchedBytes))
		manifestData, err = json.Marshal(&mf)
		if err != nil {
			return nil, fmt.Errorf("re-marshalling base manifest: %w", err)
		}
		configPath = patchedConfigPath
	}
	localBlobs[mf.Config.Digest.String()] = configPath

	manifestType := v1.MediaTypeImageManifest
	if mf.MediaType != "" {
		manifestType = mf.MediaType
	}
	return newBaseImageSource(named, manifestData, manifestType, localBlobs, upstream), nil
}

func patchBaseConfigHistory(configPath, stagingDir string, expectedNonEmpty int) (string, bool, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return "", false, fmt.Errorf("reading base config: %w", err)
	}
	var cfg v1.Image
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", false, fmt.Errorf("parsing base config: %w", err)
	}
	actual := 0
	for _, h := range cfg.History {
		if !h.EmptyLayer {
			actual++
		}
	}
	if actual >= expectedNonEmpty {
		return configPath, false, nil
	}
	epoch := time.Unix(0, 0).UTC()
	for i := actual; i < expectedNonEmpty; i++ {
		cfg.History = append(cfg.History, v1.History{
			Created:   &epoch,
			CreatedBy: "buildah_tool: synthesized non-empty history entry",
		})
	}
	patched, err := json.Marshal(&cfg)
	if err != nil {
		return "", false, fmt.Errorf("marshalling patched base config: %w", err)
	}
	out := filepath.Join(stagingDir, "patched_base_config.json")
	if err := os.WriteFile(out, patched, 0o644); err != nil {
		return "", false, fmt.Errorf("writing patched base config: %w", err)
	}
	return out, true, nil
}

func (s *baseImageSource) Reference() types.ImageReference { return s.ref }
func (s *baseImageSource) HasThreadSafeGetBlob() bool      { return true }

func (s *baseImageSource) Close() error {
	if s.upstream != nil {
		if err := s.upstream.Close(); err != nil {
			return fmt.Errorf("closing upstream image source: %w", err)
		}
	}
	return nil
}

func (s *baseImageSource) GetManifest(_ context.Context, instanceDigest *digest.Digest) ([]byte, string, error) {
	if instanceDigest != nil {
		return nil, "", errors.New("base image source has no manifest list instances")
	}
	return s.manifest, s.manifestType, nil
}

func (s *baseImageSource) GetBlob(ctx context.Context, info types.BlobInfo, cache types.BlobInfoCache) (io.ReadCloser, int64, error) {
	if path, ok := s.localBlobs[info.Digest.String()]; ok {
		rc, size, err := getLocalBlob(path, info)
		if err != nil {
			return nil, 0, err
		}
		return newBlobDoneLogger(rc, info.Digest, false), size, nil
	}
	if s.upstream != nil {
		rc, size, err := s.upstream.GetBlob(ctx, info, cache)
		if err != nil {
			return nil, 0, fmt.Errorf("getting blob %s from upstream: %w", info.Digest, err)
		}
		return newBlobDoneLogger(rc, info.Digest, true), size, nil
	}
	return nil, 0, fmt.Errorf("base blob %s not found locally and no upstream configured", info.Digest)
}

type blobDoneLogger struct {
	io.ReadCloser
	digest   digest.Digest
	upstream bool
	logged   bool
}

func newBlobDoneLogger(rc io.ReadCloser, blobDigest digest.Digest, upstream bool) *blobDoneLogger {
	return &blobDoneLogger{ReadCloser: rc, digest: blobDigest, upstream: upstream}
}

func (l *blobDoneLogger) Close() error {
	closeErr := l.ReadCloser.Close()
	if !l.logged {
		l.logged = true
		if l.upstream {
			logf("Pulled %s", l.digest.Hex())
		} else {
			logf("Imported %s", l.digest.Hex())
		}
	}
	if closeErr != nil {
		return fmt.Errorf("closing base blob reader %s: %w", l.digest, closeErr)
	}
	return nil
}

func getLocalBlob(path string, info types.BlobInfo) (io.ReadCloser, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("opening local base blob %s: %w", info.Digest, err)
	}
	size := info.Size
	if size < 0 {
		fi, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, 0, fmt.Errorf("stat local base blob %s: %w", info.Digest, err)
		}
		size = fi.Size()
	}
	return f, size, nil
}

func (s *baseImageSource) GetSignatures(context.Context, *digest.Digest) ([][]byte, error) {
	return nil, nil
}

func (s *baseImageSource) LayerInfosForCopy(context.Context, *digest.Digest) ([]types.BlobInfo, error) {
	return nil, nil
}

type baseImageReference struct {
	named reference.Named
	src   *baseImageSource
}

func (r *baseImageReference) Transport() types.ImageTransport         { return baseImageTransport{} }
func (r *baseImageReference) StringWithinTransport() string           { return r.named.String() }
func (r *baseImageReference) DockerReference() reference.Named        { return r.named }
func (r *baseImageReference) PolicyConfigurationIdentity() string     { return "" }
func (r *baseImageReference) PolicyConfigurationNamespaces() []string { return nil }

func (r *baseImageReference) NewImageSource(context.Context, *types.SystemContext) (types.ImageSource, error) {
	return r.src, nil
}

func (r *baseImageReference) NewImage(context.Context, *types.SystemContext) (types.ImageCloser, error) {
	return nil, errors.New("base image reference is source-only")
}

func (r *baseImageReference) NewImageDestination(context.Context, *types.SystemContext) (types.ImageDestination, error) {
	return nil, errors.New("base image reference is source-only")
}

func (r *baseImageReference) DeleteImage(context.Context, *types.SystemContext) error {
	return errors.New("base image reference is source-only")
}

type baseImageTransport struct{}

func (baseImageTransport) Name() string { return "buildah-tool-base" }

func (baseImageTransport) ParseReference(string) (types.ImageReference, error) {
	return nil, errors.New("buildah-tool-base transport does not support ParseReference")
}

func (baseImageTransport) ValidatePolicyConfigurationScope(string) error { return nil }
