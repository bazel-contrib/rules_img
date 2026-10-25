package deploy

import (
	"path/filepath"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
)

// clearCASCacheEnv unsets every variable configuring the local blob cache, so a
// test starts from the defaults regardless of the developer's environment.
func clearCASCacheEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{envCASCache, envCASCacheDir, envCASCacheMaxSize, envCASCacheBufferSize, "IMG_DISK_CACHE"} {
		t.Setenv(name, "")
	}
}

func TestCASCacheConfigDefaults(t *testing.T) {
	clearCASCacheEnv(t)
	config := casCacheConfigFromEnv()
	if !config.enabled {
		t.Fatal("the local blob cache should be on by default")
	}
	if config.dir != "" {
		t.Errorf("dir = %q, want the cache's own default", config.dir)
	}
	// Nothing else manages the default directory, so it must be bounded.
	if config.maxSize != cas.DefaultCacheMaxSize {
		t.Errorf("maxSize = %d, want %d", config.maxSize, cas.DefaultCacheMaxSize)
	}
	if config.bufferSize != cas.DefaultCacheBufferSize {
		t.Errorf("bufferSize = %d, want %d", config.bufferSize, cas.DefaultCacheBufferSize)
	}
}

func TestCASCacheConfigDisabled(t *testing.T) {
	for _, value := range []string{"0", "false", "off", "no", "OFF", " false "} {
		t.Run(value, func(t *testing.T) {
			clearCASCacheEnv(t)
			t.Setenv(envCASCache, value)
			if config := casCacheConfigFromEnv(); config.enabled {
				t.Errorf("%s=%q should disable the local blob cache", envCASCache, value)
			}
		})
	}
	for _, value := range []string{"", "1", "true", "on"} {
		t.Run("enabled/"+value, func(t *testing.T) {
			clearCASCacheEnv(t)
			t.Setenv(envCASCache, value)
			if config := casCacheConfigFromEnv(); !config.enabled {
				t.Errorf("%s=%q should leave the local blob cache on", envCASCache, value)
			}
		})
	}
}

func TestCASCacheConfigSharesBazelDiskCache(t *testing.T) {
	clearCASCacheEnv(t)
	diskCache := filepath.FromSlash("/bazel/disk/cache")
	t.Setenv("IMG_DISK_CACHE", diskCache)

	config := casCacheConfigFromEnv()
	if config.dir != diskCache {
		t.Errorf("dir = %q, want Bazel's disk cache %q", config.dir, diskCache)
	}
	// Bazel's disk cache GC owns that directory's size; we don't prune it.
	if config.maxSize != 0 {
		t.Errorf("maxSize = %d, want no limit for a directory the user manages", config.maxSize)
	}
}

func TestCASCacheConfigExplicitDirWins(t *testing.T) {
	clearCASCacheEnv(t)
	t.Setenv("IMG_DISK_CACHE", filepath.FromSlash("/bazel/disk/cache"))
	explicit := filepath.FromSlash("/blob/cache")
	t.Setenv(envCASCacheDir, explicit)

	if config := casCacheConfigFromEnv(); config.dir != explicit {
		t.Errorf("dir = %q, want %q", config.dir, explicit)
	}
}

func TestCASCacheConfigSizeOverrides(t *testing.T) {
	clearCASCacheEnv(t)
	t.Setenv(envCASCacheMaxSize, "2GiB")
	t.Setenv(envCASCacheBufferSize, "256KiB")

	config := casCacheConfigFromEnv()
	if config.maxSize != 2<<30 {
		t.Errorf("maxSize = %d, want %d", config.maxSize, 2<<30)
	}
	if config.bufferSize != 256<<10 {
		t.Errorf("bufferSize = %d, want %d", config.bufferSize, 256<<10)
	}

	// An explicit limit also applies to a directory that would otherwise be left
	// alone.
	clearCASCacheEnv(t)
	t.Setenv("IMG_DISK_CACHE", filepath.FromSlash("/bazel/disk/cache"))
	t.Setenv(envCASCacheMaxSize, "1024")
	if config := casCacheConfigFromEnv(); config.maxSize != 1024 {
		t.Errorf("maxSize = %d, want 1024", config.maxSize)
	}
}

func TestCASCacheConfigInvalidSizeFallsBackToDefault(t *testing.T) {
	clearCASCacheEnv(t)
	t.Setenv(envCASCacheMaxSize, "lots")
	t.Setenv(envCASCacheBufferSize, "-1MiB")

	config := casCacheConfigFromEnv()
	if config.maxSize != cas.DefaultCacheMaxSize {
		t.Errorf("maxSize = %d, want the default %d", config.maxSize, cas.DefaultCacheMaxSize)
	}
	if config.bufferSize != cas.DefaultCacheBufferSize {
		t.Errorf("bufferSize = %d, want the default %d", config.bufferSize, cas.DefaultCacheBufferSize)
	}
}
