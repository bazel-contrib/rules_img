package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"go.podman.io/buildah"
	"go.podman.io/storage"
)

func acquireSharedLock(root string) (*os.File, error) {
	lockPath := filepath.Join(root, "prune.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("failed to create prune lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("failed to acquire prune lock: %w", err)
	}
	return f, nil
}

func tryPrune(lockFile *os.File, store storage.Store) {
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return
	}

	pruneStore(store)
	pruneTmpDirs(store.GraphRoot())
}

func pruneStore(store storage.Store) {
	const limitBytes int64 = 200 * 1024 * 1024 * 1024

	totalSize, err := storeLayersSize(store)
	if err != nil || totalSize <= limitBytes {
		return
	}

	deleteBuildContainers(store)

	images, err := store.Images()
	if err != nil || len(images) == 0 {
		return
	}
	slices.SortFunc(images, func(a, b storage.Image) int {
		return a.Created.Compare(b.Created)
	})

	for _, img := range images {
		totalSize, err = storeLayersSize(store)
		if err != nil || totalSize <= limitBytes {
			break
		}
		if _, err := store.DeleteImage(img.ID, true); err != nil {
			logf("Failed to prune image %s: %v", img.ID, err)
		}
	}
}

func deleteBuildContainers(store storage.Store) {
	builders, err := buildah.OpenAllBuilders(store)
	if err != nil {
		logf("Failed to read build containers: %v", err)
		return
	}
	for _, builder := range builders {
		if err := builder.Delete(); err != nil {
			logf("Failed to prune build container %s: %v", builder.ContainerID, err)
		}
	}
}

func removeAll(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("lstat %s: %w", path, err)
	}
	if !info.IsDir() {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		return nil
	}
	if err := os.Chmod(path, 0o777); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", path, err)
	}
	for _, entry := range entries {
		if err := removeAll(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

func pruneTmpDirs(graphRoot string) {
	tmpDir := filepath.Join(graphRoot, "tmp")
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = removeAll(filepath.Join(tmpDir, entry.Name()))
		}
	}
}

func storeLayersSize(store storage.Store) (int64, error) {
	layers, err := store.Layers()
	if err != nil {
		return 0, fmt.Errorf("listing layers: %w", err)
	}

	var total int64
	for _, layer := range layers {
		if layer.UncompressedSize > 0 {
			total += layer.UncompressedSize
		}
	}
	return total, nil
}
