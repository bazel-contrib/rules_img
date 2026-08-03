package api

import "testing"

func TestIsImageConfigMediaType(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mediaType string
		want      bool
	}{
		{
			name:      "unset defaults to the OCI image config",
			mediaType: "",
			want:      true,
		},
		{
			name:      "OCI image config",
			mediaType: MediaTypeOCIImageConfig,
			want:      true,
		},
		{
			name:      "Docker image config",
			mediaType: MediaTypeDockerImageConfig,
			want:      true,
		},
		{
			name:      "empty config of an ORAS artifact",
			mediaType: MediaTypeEmptyJSON,
			want:      false,
		},
		{
			name:      "Helm chart config",
			mediaType: "application/vnd.cncf.helm.config.v1+json",
			want:      false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsImageConfigMediaType(tc.mediaType); got != tc.want {
				t.Errorf("IsImageConfigMediaType(%q) = %v, want %v", tc.mediaType, got, tc.want)
			}
		})
	}
}
