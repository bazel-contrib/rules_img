package layer

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/iso9660"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/iso9660/isocstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree"
)

// iso9660Image lets the layer sink drive an ISO 9660 builder.
type iso9660Image struct{ *iso9660.Builder }

func (b iso9660Image) WriteRegularFromTarEntry(hdr *tar.Header, archive tree.TarArchive, index int, knownHash []byte) error {
	return b.Builder.WriteRegularFromTarEntry(hdr, archive, index, knownHash)
}

// iso9660Settings are the ISO 9660-only knobs of a layer action (--iso9660-*).
type iso9660Settings struct {
	volumeID       string
	systemID       string
	volumeSetID    string
	publisher      string
	dataPreparer   string
	applicationID  string
	rockRidge      bool
	rationalizeRR  bool
	joliet         bool
	jolietLongName bool
	udf            bool
	deduplicate    bool
	size           int64
	boot           []iso9660.BootEntry
	// deduplicateTrees comes from --layer-tree-artifact-handling, which is not
	// ISO 9660-specific.
	deduplicateTrees bool
}

// options turns the settings into the options of the ISO 9660 writer. The
// writer validates them.
func (s iso9660Settings) options() iso9660.Options {
	return iso9660.Options{
		VolumeID:             s.volumeID,
		SystemID:             s.systemID,
		VolumeSetID:          s.volumeSetID,
		PublisherID:          s.publisher,
		DataPreparerID:       s.dataPreparer,
		ApplicationID:        s.applicationID,
		RockRidge:            s.rockRidge,
		RationalizeRockRidge: s.rationalizeRR,
		Joliet:               s.joliet,
		JolietLongNames:      s.jolietLongName,
		UDF:                  s.udf,
		Deduplicate:          s.deduplicate,
		Size:                 s.size,
		Boot:                 s.boot,
	}
}

// bootEntrySpec is the JSON form of an El Torito boot entry, as
// --iso9660-boot takes it.
type bootEntrySpec struct {
	// Image is the path of the boot image in the layer.
	Image string `json:"image"`
	// Platform is "bios", "powerpc", "mac" or "uefi". The default is "bios".
	Platform string `json:"platform,omitempty"`
	// Emulation is "none", "floppy120", "floppy144", "floppy288" or
	// "harddisk". The default is "none", which is what a current boot loader
	// expects.
	Emulation string `json:"emulation,omitempty"`
	// LoadSize is how many 512 byte sectors the firmware loads. Zero loads the
	// whole image.
	LoadSize uint16 `json:"load_size,omitempty"`
	// LoadSegment is the real mode segment a BIOS entry is loaded at.
	LoadSegment uint16 `json:"load_segment,omitempty"`
	// BootInfoTable patches the boot information table isolinux expects into
	// the boot image.
	BootInfoTable bool `json:"boot_info_table,omitempty"`
	// ID is the identifier recorded on the section header of an entry other
	// than the first.
	ID string `json:"id,omitempty"`
}

var bootPlatforms = map[string]iso9660.BootPlatform{
	"":        iso9660.BootPlatformX86,
	"bios":    iso9660.BootPlatformX86,
	"x86":     iso9660.BootPlatformX86,
	"powerpc": iso9660.BootPlatformPowerPC,
	"mac":     iso9660.BootPlatformMac,
	"uefi":    iso9660.BootPlatformEFI,
	"efi":     iso9660.BootPlatformEFI,
}

var bootEmulations = map[string]iso9660.BootEmulation{
	"":          iso9660.BootNoEmulation,
	"none":      iso9660.BootNoEmulation,
	"floppy120": iso9660.BootFloppy120,
	"floppy144": iso9660.BootFloppy144,
	"floppy288": iso9660.BootFloppy288,
	"harddisk":  iso9660.BootHardDisk,
}

// iso9660BootFlag collects the --iso9660-boot entries, in the order they are
// given: the first one becomes the default boot entry of the catalog.
type iso9660BootFlag []iso9660.BootEntry

func (f *iso9660BootFlag) String() string { return "" }

func (f *iso9660BootFlag) Set(value string) error {
	var spec bootEntrySpec
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return fmt.Errorf("parsing the boot entry %s: %w", value, err)
	}
	platform, ok := bootPlatforms[spec.Platform]
	if !ok {
		return fmt.Errorf("unknown boot platform %q: supported are bios, powerpc, mac and uefi", spec.Platform)
	}
	emulation, ok := bootEmulations[spec.Emulation]
	if !ok {
		return fmt.Errorf("unknown boot emulation %q: supported are none, floppy120, floppy144, floppy288 and harddisk", spec.Emulation)
	}
	*f = append(*f, iso9660.BootEntry{
		Image:         spec.Image,
		Platform:      platform,
		Emulation:     emulation,
		LoadSize:      spec.LoadSize,
		LoadSegment:   spec.LoadSegment,
		BootInfoTable: spec.BootInfoTable,
		ID:            spec.ID,
	})
	return nil
}

// writeIso9660Layer writes the layer as an ISO 9660 image to out, which must be
// a new, empty file. If compactStream is set, a compact stream of the image is
// written to it. Options that only apply to tar layers (compression, estargz,
// ztoc) have no effect. Without --iso9660-rock-ridge, layers with symbolic
// links, device nodes or FIFOs fail: plain ISO 9660 cannot describe them.
func writeIso9660Layer(
	out *os.File, settings iso9660Settings, addFiles addFiles, importTars importTars, addExecutables executables, addSymlinks symlinks, emptyFiles []string,
	baseMetadataPaths []string, createParentDirectories bool, layerMetadata *LayerMetadata,
	compactStream io.Writer, compactStreamInlineThreshold uint64,
) (*iso9660.Info, error) {
	opts := settings.options()
	// The digest of the image is needed for the metadata, and is computed
	// while the image is written.
	opts.DigestImage = true
	opts.DigestFiles = compactStream != nil
	builder, err := iso9660.NewBuilder(opts)
	if err != nil {
		return nil, err
	}
	defer builder.Close()

	sink := &imageSink{builder: iso9660Image{builder}, metadata: layerMetadata, deduplicateTrees: settings.deduplicateTrees}
	recorder := tree.NewRecorder(sink).WithDeduplicate(settings.deduplicate)
	if layerMetadata != nil {
		recorder = recorder.WithMetadata(layerMetadata)
	}
	if err := writeLayer(recorder, addFiles, importTars, addExecutables, addSymlinks, emptyFiles, baseMetadataPaths, createParentDirectories, layerMetadata); err != nil {
		return nil, err
	}
	if layerMetadata != nil {
		if err := layerMetadata.VerifyAllFileMetadataUsed(); err != nil {
			return nil, err
		}
	}
	if compactStream == nil {
		return builder.Build(out)
	}
	// Like for tar layers, files smaller than the inline threshold stay in the
	// compact stream. Zero disables inlining.
	return isocstream.Build(builder, out, compactStream, isocstream.Options{MinSize: max(int64(compactStreamInlineThreshold), 1)})
}
