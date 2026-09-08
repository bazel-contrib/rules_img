package layer

import (
	"slices"
	"testing"
)

func TestDedupeSymlinks(t *testing.T) {
	tests := []struct {
		name string
		in   symlinks
		want symlinks
	}{
		{
			name: "empty",
			in:   nil,
			want: nil,
		},
		{
			// The shape a shared runfiles tree produces: "_main" is named by the
			// group's files and again by a non-root symlink, which resolves under
			// "_main" whatever its own first segment says.
			name: "exact repeat collapses",
			in: symlinks{
				{LinkName: "app/bin/launcher.runfiles/_main", Target: "../../../.shared_runfiles/_main"},
				{LinkName: "app/bin/launcher.runfiles/external_tool", Target: "../../../.shared_runfiles/external_tool"},
				{LinkName: "app/bin/launcher.runfiles/_main", Target: "../../../.shared_runfiles/_main"},
			},
			want: symlinks{
				{LinkName: "app/bin/launcher.runfiles/_main", Target: "../../../.shared_runfiles/_main"},
				{LinkName: "app/bin/launcher.runfiles/external_tool", Target: "../../../.shared_runfiles/external_tool"},
			},
		},
		{
			// Same name, different target: a conflict rather than a repeat, so
			// both survive and the layer shows it.
			name: "same name different target survives",
			in: symlinks{
				{LinkName: "link", Target: "first"},
				{LinkName: "link", Target: "second"},
			},
			want: symlinks{
				{LinkName: "link", Target: "first"},
				{LinkName: "link", Target: "second"},
			},
		},
		{
			name: "order of the survivors is kept",
			in: symlinks{
				{LinkName: "c", Target: "t"},
				{LinkName: "a", Target: "t"},
				{LinkName: "c", Target: "t"},
				{LinkName: "b", Target: "t"},
			},
			want: symlinks{
				{LinkName: "c", Target: "t"},
				{LinkName: "a", Target: "t"},
				{LinkName: "b", Target: "t"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dedupeSymlinks(tc.in)
			if !slices.Equal(got, tc.want) {
				t.Errorf("dedupeSymlinks() = %v, want %v", got, tc.want)
			}
		})
	}
}
