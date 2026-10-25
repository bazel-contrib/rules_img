package overlayfs

import "testing"

func TestDevRoundTrip(t *testing.T) {
	for _, c := range [][2]int64{{0, 0}, {1, 3}, {8, 255}, {259, 70000}, {4095, 1048575}} {
		major, minor := DecodeDev(EncodeDev(c[0], c[1]))
		if major != c[0] || minor != c[1] {
			t.Errorf("round trip of %v = %d,%d", c, major, minor)
		}
	}
}
