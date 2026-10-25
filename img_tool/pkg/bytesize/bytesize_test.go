package bytesize

import "testing"

func TestParse(t *testing.T) {
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
			got, err := Parse(test.raw)
			if test.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = %d, want an error", test.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", test.raw, err)
			}
			if got != test.want {
				t.Errorf("Parse(%q) = %d, want %d", test.raw, got, test.want)
			}
		})
	}
}

func TestFormat(t *testing.T) {
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
		if got := Format(size); got != want {
			t.Errorf("Format(%d) = %q, want %q", size, got, want)
		}
	}
}
