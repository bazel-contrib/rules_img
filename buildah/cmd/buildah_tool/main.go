package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/homedir"
	"go.podman.io/storage/pkg/reexec"
	"go.podman.io/storage/pkg/unshare"
)

type layerMetadata struct {
	DiffID      digest.Digest     `json:"diff_id,omitempty"`
	MediaType   string            `json:"mediaType"`
	Digest      digest.Digest     `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
	History     []v1.History      `json:"history,omitempty"`
}

func findBazelBuildPID() (int, bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, false, fmt.Errorf("read /proc: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()
		pid, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", name, "cmdline"))
		if err != nil {
			continue
		}

		cmdline := strings.ReplaceAll(string(data), "\x00", " ")
		cmdline = strings.TrimSpace(cmdline)
		if strings.HasPrefix(cmdline, "bazel build") {
			return pid, true, nil
		}
	}

	return 0, false, nil
}

func setupEnv() error {
	_ = os.Setenv("PATH", "/usr/bin")
	if u, err := user.Current(); err == nil {
		_ = os.Setenv("HOME", u.HomeDir)
	}

	opts, err := storage.DefaultStoreOptions()
	if err != nil {
		return fmt.Errorf("resolving default store options: %w", err)
	}
	tmpDir := filepath.Join(opts.GraphRoot, "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return fmt.Errorf("creating buildah tmp dir %q: %w", tmpDir, err)
	}
	_ = os.Setenv("TMPDIR", tmpDir)
	return nil
}

func main() {
	if reexec.Init() {
		return
	}

	flag.Parse()

	if os.Getenv(unshare.UsernsEnvName) == "" {
		if err := setupEnv(); err != nil {
			fmt.Fprintf(os.Stderr, "buildah_tool: %v\n", err)
			os.Exit(1)
		}

		if *flagVerbose {
			tryStealBazelOutputForStreaming()
		}
	}

	unshare.MaybeReexecUsingUserNamespace(false)

	if err := setupLog(); err != nil {
		fmt.Fprintf(os.Stderr, "buildah_tool: %v\n", err)
		os.Exit(1)
	}

	err := run()
	closeLogPipe()
	if err != nil {
		flushBuildLog()
		fmt.Fprintf(os.Stderr, "buildah_tool: %v\n", err)
		os.Exit(1)
	}
}

func setRuntimeDir() error {
	if !unshare.IsRootless() || os.Getenv("XDG_RUNTIME_DIR") != "" {
		return nil
	}
	runtimeDir, err := homedir.GetRuntimeDir()
	if err != nil {
		return fmt.Errorf("resolving rootless runtime dir: %w", err)
	}
	if err := os.Setenv("XDG_RUNTIME_DIR", runtimeDir); err != nil {
		return fmt.Errorf("setting XDG_RUNTIME_DIR to %q: %w", runtimeDir, err)
	}
	return nil
}

func run() error {
	if err := setRuntimeDir(); err != nil {
		return err
	}

	store, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _, _ = store.Shutdown(false) }()

	checkStorageBackend(store)

	lockFile, err := acquireSharedLock(store.GraphRoot())
	if err != nil {
		return err
	}

	defer func() {
		tryPrune(lockFile, store)
		_ = lockFile.Close()
	}()

	if err := os.MkdirAll(*flagStagingDir, 0o755); err != nil {
		return fmt.Errorf("creating staging dir: %w", err)
	}

	ctxDir := filepath.Join(*flagStagingDir, "context")
	if err := assembleContext(ctxDir, flagContextFiles); err != nil {
		return fmt.Errorf("assembling context: %w", err)
	}
	defer func() { _ = os.RemoveAll(ctxDir) }()

	tmpDir := filepath.Join(*flagStagingDir, "tmp")
	blobCacheDir := filepath.Join(tmpDir, "blob-cache")
	if err := os.MkdirAll(blobCacheDir, 0o755); err != nil {
		return fmt.Errorf("creating blob-cache dir: %w", err)
	}
	//	defer func() { _ = os.RemoveAll(tmpDir) }()

	localBlobs, localMetas, err := loadBlobIndex(flagLocalLayerBlobs, flagLocalLayerMeta)
	if err != nil {
		return err
	}
	baseLayerList, baseLayers, err := loadBaseLayers(flagBaseLayerMetadata)
	if err != nil {
		return err
	}
	blobSeeds, err := collectBlobSeeds(localMetas, flagLocalLayerCacheRepos, baseLayers, localBlobs, *flagUpstreamRegistry, *flagUpstreamRepository)
	if err != nil {
		return err
	}

	ctx := context.Background()
	buildArgs := flagBuildArgs
	if *flagBaseManifest != "" {
		var upstreamSrc types.ImageSource
		if *flagUpstreamRegistry != "" {
			upstreamDigest, err := digest.Parse(*flagUpstreamDigest)
			if err != nil {
				return fmt.Errorf("parsing upstream digest %q: %w", *flagUpstreamDigest, err)
			}
			upstreamSrc, err = newUpstreamSource(ctx, *flagUpstreamRegistry, *flagUpstreamRepository, upstreamDigest)
			if err != nil {
				return err
			}
		}
		baseName, err := uniqueBaseName()
		if err != nil {
			return err
		}
		baseSrc, err := buildBaseSource(baseName, *flagBaseManifest, *flagBaseConfig, tmpDir, localBlobs, upstreamSrc)
		if err != nil {
			return err
		}
		if err := importBaseImage(ctx, store, baseName, blobCacheDir, baseSrc); err != nil {
			return err
		}
		buildArgs = append(buildArgs, fmt.Sprintf("BASE_IMAGE=%s", baseName.String()))
	}

	imageDigest, err := buildImage(ctx, store, buildRequest{
		dockerfile:   *flagDockerfile,
		contextDir:   ctxDir,
		blobCacheDir: blobCacheDir,
		buildArgs:    buildArgs,
		cacheFrom:    *flagCacheFrom,
		cacheTo:      *flagCacheTo,
		blobSeeds:    blobSeeds,
	})
	if err != nil {
		return err
	}

	dst, err := exportImage(ctx, store, imageDigest, blobCacheDir, tmpDir, *flagEstargz, baseLayerList, baseLayers)
	if err != nil {
		return err
	}

	return writeOutputs(dst.manifest, dst.blobFiles, localBlobs, baseLayerList, baseLayers)
}

func loadBlobIndex(blobPaths, metaPaths []string) (map[string]string, []layerMetadata, error) {
	index := map[string]string{}
	metas := make([]layerMetadata, 0, len(metaPaths))
	for i, metaPath := range metaPaths {
		raw, err := os.ReadFile(metaPath)
		if err != nil {
			return nil, nil, fmt.Errorf("reading local layer metadata %d: %w", i, err)
		}
		var meta layerMetadata
		if err := json.Unmarshal(raw, &meta); err != nil {
			return nil, nil, fmt.Errorf("parsing local layer metadata %d: %w", i, err)
		}
		index[meta.Digest.String()] = blobPaths[i]
		metas = append(metas, meta)
	}
	return index, metas, nil
}

func loadBaseLayers(metaPaths []string) ([]layerMetadata, map[string]layerMetadata, error) {
	list := make([]layerMetadata, 0, len(metaPaths))
	set := map[string]layerMetadata{}
	for _, metaPath := range metaPaths {
		raw, err := os.ReadFile(metaPath)
		if err != nil {
			return nil, nil, fmt.Errorf("reading base layer metadata: %w", err)
		}
		var meta layerMetadata
		if err := json.Unmarshal(raw, &meta); err != nil {
			return nil, nil, fmt.Errorf("parsing base layer metadata: %w", err)
		}
		list = append(list, meta)
		if meta.Digest != "" {
			set[meta.Digest.String()] = meta
		}
		if meta.DiffID != "" {
			set[meta.DiffID.String()] = meta
		}
	}
	return list, set, nil
}

func assembleContext(ctxDir string, contextFiles []string) error {
	if err := os.MkdirAll(ctxDir, 0o755); err != nil {
		return err //nolint:wrapcheck
	}

	for _, entry := range contextFiles {
		src, shortPath, ok := strings.Cut(entry, "=")
		if !ok {
			return fmt.Errorf("invalid context-file %q, expected SRC=SHORTPATH", entry)
		}
		dst := filepath.Join(ctxDir, shortPath)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err //nolint:wrapcheck
		}
		if err := copyFile(src, dst); err != nil {
			return err
		}
	}

	return nil
}

func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err //nolint:wrapcheck
	}
	in, err := os.Open(src)
	if err != nil {
		return err //nolint:wrapcheck
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err //nolint:wrapcheck
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err //nolint:wrapcheck
}
