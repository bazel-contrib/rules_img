package erofs_test

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	erofs "github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs/internal/disk"
)

// TestDeviceTableOffset checks that devt_slotoff points at the device table.
//
// The field counts device-slot-sized units rather than bytes -- the kernel and
// erofs-utils both resolve it as devt_slotoff * EROFS_DEVT_SLOT_SIZE -- and the
// table follows the superblock, so the only correct value is
// (1024 + 128) / 128. Getting it wrong fails silently: a chunk index naming an
// explicit device_id never consults the slot, and a slot misread from the
// superblock's zeroed volume_name yields blocks = 0 and a mapped address of 0,
// which reads as "no unified mapping" rather than as an error.
func TestDeviceTableOffset(t *testing.T) {
	dataPath := filepath.Join(t.TempDir(), "data.bin")
	df, err := os.Create(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = df.Close() }()

	var metaBuf testBuffer
	fsys := erofs.Create(&metaBuf, erofs.WithDataFile(df))
	f, err := fsys.Create("/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	// Three blocks of payload, so the recorded block count is distinctive.
	if _, err := f.Write(bytes.Repeat([]byte{0xCD}, 3*4096)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}
	image := metaBuf.Bytes()

	var sb disk.SuperBlock
	if _, err := binary.Decode(image[disk.SuperBlockOffset:], binary.LittleEndian, &sb); err != nil {
		t.Fatal(err)
	}
	if sb.ExtraDevices != 1 {
		t.Fatalf("extra_devices = %d, want 1", sb.ExtraDevices)
	}

	gotOff := int64(sb.DevtSlotOff) * disk.SizeDeviceSlot
	wantOff := int64(disk.SuperBlockOffset + disk.SizeSuperBlock)
	if gotOff != wantOff {
		t.Errorf("devt_slotoff %d resolves to byte %d, want %d", sb.DevtSlotOff, gotOff, wantOff)
	}

	var slot disk.DeviceSlot
	if _, err := binary.Decode(image[gotOff:], binary.LittleEndian, &slot); err != nil {
		t.Fatal(err)
	}
	if slot.Blocks != 3 {
		t.Errorf("device slot at byte %d reports %d blocks, want 3: devt_slotoff does not point at the device table", gotOff, slot.Blocks)
	}
}
