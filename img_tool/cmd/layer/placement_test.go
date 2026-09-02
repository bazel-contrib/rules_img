package layer

import "testing"

func TestPlaceFilesSpecPlace(t *testing.T) {
	tests := []struct {
		name      string
		spec      placeFilesSpec
		shortPath string
		count     int
		want      string
		wantErr   bool
	}{
		{
			name:      "single output collapses onto the path key",
			spec:      placeFilesSpec{Mode: "package_relative", Dest: "app", Anchor: "_main/pkg"},
			shortPath: "_main/pkg/config/properties.yaml",
			count:     1,
			want:      "app",
		},
		{
			name:      "single output collapses onto the path key when flattening",
			spec:      placeFilesSpec{Mode: "flatten", Dest: "app", Anchor: "_main/pkg"},
			shortPath: "_main/pkg/config/properties.yaml",
			count:     1,
			want:      "app",
		},
		{
			// A trailing slash is an explicit directory key: one output is laid
			// out inside it exactly like several would be.
			name:      "single output under a directory key stays package-relative",
			spec:      placeFilesSpec{Mode: "package_relative", Dest: "app/", Anchor: "_main/pkg"},
			shortPath: "_main/pkg/config/properties.yaml",
			count:     1,
			want:      "app/config/properties.yaml",
		},
		{
			name:      "multiple outputs under a directory key stay package-relative",
			spec:      placeFilesSpec{Mode: "package_relative", Dest: "app/", Anchor: "_main/pkg"},
			shortPath: "_main/pkg/config/properties.yaml",
			count:     2,
			want:      "app/config/properties.yaml",
		},
		{
			name:      "single output under a directory key flattens to the basename",
			spec:      placeFilesSpec{Mode: "flatten", Dest: "app/", Anchor: "_main/pkg"},
			shortPath: "_main/pkg/config/properties.yaml",
			count:     1,
			want:      "app/properties.yaml",
		},
		{
			name:      "multiple outputs flatten to the basename",
			spec:      placeFilesSpec{Mode: "flatten", Dest: "app", Anchor: "_main/pkg"},
			shortPath: "_main/pkg/config/properties.yaml",
			count:     2,
			want:      "app/properties.yaml",
		},
		{
			name:      "relative mode never collapses a single output",
			spec:      placeFilesSpec{Mode: "relative", Dest: "app", Anchor: "_main/pkg/bin"},
			shortPath: "_main/pkg/bin/data.txt",
			count:     1,
			want:      "app/data.txt",
		},
		{
			name:      "escaping the image root fails",
			spec:      placeFilesSpec{Mode: "package_relative", Dest: "", Anchor: "_main/pkg/deep"},
			shortPath: "_main/other.txt",
			count:     2,
			wantErr:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.spec.place(tc.shortPath, tc.count)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("place(%q, %d) = %q, want error", tc.shortPath, tc.count, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("place(%q, %d) unexpected error: %v", tc.shortPath, tc.count, err)
			}
			if got != tc.want {
				t.Errorf("place(%q, %d) = %q, want %q", tc.shortPath, tc.count, got, tc.want)
			}
		})
	}
}
