package pull

import (
	"testing"

	registryv1 "github.com/google/go-containerregistry/pkg/v1"
)

func TestFilterManifests(t *testing.T) {
	descriptor := func(os, arch, variant string) registryv1.Descriptor {
		if os == "" {
			return registryv1.Descriptor{}
		}
		return registryv1.Descriptor{
			Platform: &registryv1.Platform{OS: os, Architecture: arch, Variant: variant},
		}
	}

	amd64 := descriptor("linux", "amd64", "")
	amd64v3 := descriptor("linux", "amd64", "v3")
	arm64v8 := descriptor("linux", "arm64", "v8")
	arm64 := descriptor("linux", "arm64", "")
	i386 := descriptor("linux", "386", "")
	windows := descriptor("windows", "amd64", "")
	attestation := descriptor("unknown", "unknown", "")
	noPlatform := descriptor("", "", "")

	all := []registryv1.Descriptor{amd64, amd64v3, arm64v8, arm64, i386, windows, attestation, noPlatform}

	for _, tc := range []struct {
		name  string
		specs []string
		want  []registryv1.Descriptor
	}{
		{
			name:  "no filter keeps everything",
			specs: nil,
			want:  all,
		},
		{
			// The variant-less arm64 entry and the v8 one normalize to the same platform.
			name:  "arm64 matches the v8 variant",
			specs: []string{"linux/arm64"},
			want:  []registryv1.Descriptor{arm64v8, arm64},
		},
		{
			name:  "explicit variant is respected",
			specs: []string{"linux/amd64/v3"},
			want:  []registryv1.Descriptor{amd64v3},
		},
		{
			// A spec that names no variant covers the whole architecture, so the filter
			// cannot take away a child the build would otherwise have selected.
			name:  "amd64 covers every amd64 variant",
			specs: []string{"linux/amd64"},
			want:  []registryv1.Descriptor{amd64, amd64v3},
		},
		{
			// containerd's Only() would also match linux/386 here.
			name:  "amd64 does not match other architectures",
			specs: []string{"linux/amd64", "windows/amd64"},
			want:  []registryv1.Descriptor{amd64, amd64v3, windows},
		},
		{
			name:  "attestations are dropped unless requested",
			specs: []string{"unknown/unknown"},
			want:  []registryv1.Descriptor{attestation},
		},
		{
			name:  "nothing matches",
			specs: []string{"linux/riscv64"},
			want:  []registryv1.Descriptor{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			matchers, err := parsePlatforms(tc.specs)
			if err != nil {
				t.Fatalf("parsePlatforms(%v) = %v", tc.specs, err)
			}
			got := filterManifests(all, matchers)
			if len(got) != len(tc.want) {
				t.Fatalf("filterManifests() kept %d descriptors, want %d: %v", len(got), len(tc.want), got)
			}
			for i := range got {
				if platformString(got[i]) != platformString(tc.want[i]) {
					t.Errorf("descriptor %d: got %s, want %s", i, platformString(got[i]), platformString(tc.want[i]))
				}
			}
		})
	}
}

func platformString(desc registryv1.Descriptor) string {
	if desc.Platform == nil {
		return "<no platform>"
	}
	return desc.Platform.String()
}

func TestParsePlatformsRejectsInvalidSpec(t *testing.T) {
	if _, err := parsePlatforms([]string{"linux/amd64/v3/extra"}); err == nil {
		t.Error("parsePlatforms() accepted a spec with too many components")
	}
}
