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

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		raw     string
		want    int64
		wantErr bool
	}{
		{raw: "0", want: 0},
		{raw: "1024", want: 1024},
		{raw: "  4096  ", want: 4096},
		{raw: "1K", want: 1 << 10},
		{raw: "1kb", want: 1 << 10},
		{raw: "1KiB", want: 1 << 10},
		{raw: "10MiB", want: 10 << 20},
		{raw: "10 MB", want: 10 << 20},
		{raw: "2GiB", want: 2 << 30},
		{raw: "1.5GiB", want: 1610612736},
		{raw: "1TiB", want: 1 << 40},
		{raw: "512B", want: 512},
		{raw: "", wantErr: true},
		{raw: "lots", wantErr: true},
		{raw: "-1MiB", wantErr: true},
		{raw: "1EiB", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.raw, func(t *testing.T) {
			got, err := parseByteSize(test.raw)
			if test.wantErr {
				if err == nil {
					t.Fatalf("parseByteSize(%q) = %d, want an error", test.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseByteSize(%q): %v", test.raw, err)
			}
			if got != test.want {
				t.Errorf("parseByteSize(%q) = %d, want %d", test.raw, got, test.want)
			}
		})
	}
}

func TestHumanizeBytes(t *testing.T) {
	tests := map[int64]string{
		0:          "0 B",
		512:        "512 B",
		1024:       "1.0 KiB",
		1536:       "1.5 KiB",
		10 << 20:   "10.0 MiB",
		3 << 30:    "3.0 GiB",
		5 << 40:    "5.0 TiB",
		2048 << 40: "2.0 PiB",
	}
	for size, want := range tests {
		if got := humanizeBytes(size); got != want {
			t.Errorf("humanizeBytes(%d) = %q, want %q", size, got, want)
		}
	}
}
