package diskimage

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/disk"
)

// partitionConfig is the description of a partition from the command line.
type partitionConfig struct {
	label, typ, guid, size string
	attrs                  []string
	mbrType                byte
	active                 bool
	image, cstream         string
	noHiddenSectors        bool
}

type config struct {
	table                             string
	sectorSize                        int
	alignment, size, minSize, maxSize string
	diskGUID, bootCode                string
	partitions                        []*partitionConfig
	casDirs                           []string
	output                            string
	compact                           bool
	outputCASDir                      string
	casSymlink                        bool
	inlineThreshold                   string
	layoutJSON                        string
}

// input is the file system image that goes into a partition: a file with the
// image, or a compact stream of it.
type input struct {
	file     *os.File
	path     string
	isStream bool
	size     int64
	kind     fsKind
	head     []byte // the first bytes of the image
}

func (in *input) close() { in.file.Close() }

// open opens what goes into the partition. In compact mode, a compact stream
// is preferred (its references are kept), otherwise the image itself.
func openInput(ctx context.Context, p *partitionConfig, compact bool) (*input, error) {
	path, stream := p.image, false
	switch {
	case p.image == "" && p.cstream == "":
		return nil, nil
	case p.cstream != "" && (compact || p.image == ""):
		path, stream = p.cstream, true
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	in := &input{file: f, path: path, isStream: stream}
	if stream {
		if in.size, err = compactstream.ReconstructedSize(f); err != nil {
			f.Close()
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	} else {
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		in.size = st.Size()
	}

	// What the file system is only needs its first bytes. For a compact stream
	// they are inline; the blobs are not needed.
	r, err := in.reader(ctx, compactstream.NullBlobStore{})
	if err != nil {
		f.Close()
		return nil, err
	}
	head, err := io.ReadAll(io.LimitReader(r, sniffLen))
	r.Close()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	in.head, in.kind = head, sniff(head)
	return in, nil
}

// reader returns a reader over the image, from its start.
func (in *input) reader(ctx context.Context, store compactstream.BlobStore) (io.ReadCloser, error) {
	if _, err := in.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if !in.isStream {
		return io.NopCloser(io.NewSectionReader(in.file, 0, in.size)), nil
	}
	return compactstream.NewReconstructingReader(ctx, in.file, store)
}

func parseOptionalSize(name, v string) (int64, error) {
	if v == "" || v == "auto" {
		return 0, nil
	}
	n, err := disk.ParseSize(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return n, nil
}

// buildSpec turns the command line into a disk specification and opens the
// inputs.
func buildSpec(ctx context.Context, cfg *config) (disk.Spec, []*input, error) {
	spec := disk.Spec{Table: disk.Table(cfg.table), SectorSize: cfg.sectorSize}
	var err error
	if spec.Alignment, err = parseOptionalSize("--alignment", cfg.alignment); err != nil {
		return spec, nil, err
	}
	if spec.Size, err = parseOptionalSize("--size", cfg.size); err != nil {
		return spec, nil, err
	}
	if spec.MinSize, err = parseOptionalSize("--min-size", cfg.minSize); err != nil {
		return spec, nil, err
	}
	if spec.MaxSize, err = parseOptionalSize("--max-size", cfg.maxSize); err != nil {
		return spec, nil, err
	}
	if cfg.diskGUID != "" {
		if spec.DiskGUID, err = disk.ParseGUID(cfg.diskGUID); err != nil {
			return spec, nil, err
		}
	}
	if cfg.bootCode != "" {
		if spec.BootCode, err = os.ReadFile(cfg.bootCode); err != nil {
			return spec, nil, fmt.Errorf("reading the boot code: %w", err)
		}
	}

	var inputs []*input
	closeAll := func() {
		for _, in := range inputs {
			if in != nil {
				in.close()
			}
		}
	}
	for i, pc := range cfg.partitions {
		part := disk.Partition{Label: pc.label, Type: pc.typ, MBRType: pc.mbrType, Active: pc.active}
		if pc.guid != "" {
			if part.GUID, err = disk.ParseGUID(pc.guid); err != nil {
				closeAll()
				return spec, nil, fmt.Errorf("partition %d: %w", i, err)
			}
		}
		for _, a := range pc.attrs {
			bit, err := disk.ParseAttribute(a)
			if err != nil {
				closeAll()
				return spec, nil, fmt.Errorf("partition %d: %w", i, err)
			}
			part.Attributes |= bit
		}
		if part.Size, err = parseOptionalSize(fmt.Sprintf("partition %d size", i), pc.size); err != nil {
			closeAll()
			return spec, nil, err
		}
		in, err := openInput(ctx, pc, cfg.compact)
		if err != nil {
			closeAll()
			return spec, nil, fmt.Errorf("partition %d (%q): %w", i, pc.label, err)
		}
		inputs = append(inputs, in)
		if in != nil {
			part.ContentSize = in.size
		}
		spec.Partitions = append(spec.Partitions, part)
	}
	return spec, inputs, nil
}

// checkAndPatch validates a partition's file system against its place on the
// disk and returns the patches it needs.
func checkAndPatch(l *disk.Layout, p disk.PlacedPartition, in *input, pc *partitionConfig) ([]compactstream.Patch, error) {
	if in == nil {
		return nil, nil
	}
	if p.Type == "esp" || p.TypeGUID.String() == "C12A7328-F81F-11D2-BA4B-00A0C93EC93B" {
		if in.kind != fsFAT32 {
			return nil, fmt.Errorf("partition %d (%q) is an EFI system partition, which has to be FAT32, but %s is a %s image", p.Index, p.Label, in.path, in.kind)
		}
	}
	if in.kind != fsFAT32 {
		return nil, nil
	}
	if bps, _ := fat32Geometry(in.head); bps != l.SectorSize {
		return nil, fmt.Errorf("partition %d (%q): the FAT32 file system in %s has %d byte sectors, but the disk has %d byte sectors; build the file system with the sector size of the disk", p.Index, p.Label, in.path, bps, l.SectorSize)
	}
	if pc.noHiddenSectors {
		return nil, nil
	}
	specs, err := hiddenSectorPatches(in.head, in.size, p.StartLBA)
	if err != nil {
		return nil, fmt.Errorf("partition %d (%q): %w", p.Index, p.Label, err)
	}
	patches := make([]compactstream.Patch, len(specs))
	for i, s := range specs {
		patches[i] = compactstream.Patch{Offset: s.offset, Data: s.data}
	}
	return patches, nil
}

func run(ctx context.Context, cfg *config) error {
	spec, inputs, err := buildSpec(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		for _, in := range inputs {
			if in != nil {
				in.close()
			}
		}
	}()
	layout, err := disk.Plan(spec)
	if err != nil {
		return err
	}
	patches := make([][]compactstream.Patch, len(inputs))
	for i, p := range layout.Partitions {
		if patches[i], err = checkAndPatch(layout, p, inputs[i], cfg.partitions[i]); err != nil {
			return err
		}
	}

	if cfg.compact {
		err = writeCompact(ctx, cfg, layout, inputs, patches)
	} else {
		err = writeRaw(ctx, cfg, layout, inputs, patches)
	}
	if err != nil {
		return err
	}
	if cfg.layoutJSON != "" {
		return writeLayoutJSON(cfg, layout, inputs)
	}
	return nil
}

// writeRaw writes the disk image as a sparse file.
func writeRaw(ctx context.Context, cfg *config, l *disk.Layout, inputs []*input, patches [][]compactstream.Patch) error {
	out, err := os.OpenFile(cfg.output, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	store := compactstream.NewDirStore(cfg.casDirs...)
	if _, err := out.WriteAt(l.Head(), 0); err != nil {
		return err
	}
	for i, p := range l.Partitions {
		in := inputs[i]
		if in == nil {
			continue
		}
		r, err := in.reader(ctx, store)
		if err != nil {
			return err
		}
		err = copySparse(out, p.Offset(l.SectorSize), r, in.size)
		r.Close()
		if err != nil {
			return fmt.Errorf("partition %d (%q): copying %s: %w", p.Index, p.Label, in.path, err)
		}
		for _, patch := range patches[i] {
			if _, err := out.WriteAt(patch.Data, p.Offset(l.SectorSize)+patch.Offset); err != nil {
				return err
			}
		}
	}
	if tail, off := l.Tail(); tail != nil {
		if _, err := out.WriteAt(tail, off); err != nil {
			return err
		}
	}
	if err := out.Truncate(l.Size()); err != nil {
		return err
	}
	return out.Close()
}

var zeroBlock = make([]byte, 4096)

// copySparse writes n bytes from src at off, and skips the blocks that are
// all zero, so the output stays sparse. The output has to read as zeros where
// nothing is written.
func copySparse(dst io.WriterAt, off int64, src io.Reader, n int64) error {
	buf := make([]byte, 1<<20)
	for done := int64(0); done < n; {
		chunk := buf[:min(int64(len(buf)), n-done)]
		if _, err := io.ReadFull(src, chunk); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		for i := 0; i < len(chunk); {
			end := min(i+len(zeroBlock), len(chunk))
			if bytes.Equal(chunk[i:end], zeroBlock[:end-i]) {
				i = end
				continue
			}
			j := end
			for j < len(chunk) {
				next := min(j+len(zeroBlock), len(chunk))
				if bytes.Equal(chunk[j:next], zeroBlock[:next-j]) {
					break
				}
				j = next
			}
			if _, err := dst.WriteAt(chunk[i:j], off+done+int64(i)); err != nil {
				return err
			}
			i = j
		}
		done += int64(len(chunk))
	}
	return nil
}

// writeCompact writes a compact stream of the disk image. Compact streams of
// file systems are spliced in with their references to blobs, the other
// images are placed inline or, if large, as one reference to the whole image,
// and everything else is the partition table and zeros.
func writeCompact(ctx context.Context, cfg *config, l *disk.Layout, inputs []*input, patches [][]compactstream.Patch) error {
	threshold, err := parseOptionalSize("--compact-stream-inline-threshold", cfg.inlineThreshold)
	if err != nil {
		return err
	}
	// The directory exists even if no image ends up in it, so that it can be a
	// declared output of a build action.
	if cfg.outputCASDir != "" {
		if err := os.MkdirAll(filepath.Join(cfg.outputCASDir, "sha256"), 0o755); err != nil {
			return err
		}
	}
	out, err := os.OpenFile(cfg.output, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	w := compactstream.NewWriter(out, compactstream.HashAlgoSHA256, 32, compactstream.StreamCompressionZstd, compactstream.OriginalCompressionInfo{}, threshold)
	if err := w.CompressIncrementally(); err != nil {
		return err
	}
	if err := w.WriteStreamBytes(l.Head()); err != nil {
		return err
	}
	for i, p := range l.Partitions {
		if gap := p.Offset(l.SectorSize) - w.Offset(); gap > 0 {
			if err := w.WriteZeroRun(gap); err != nil {
				return err
			}
		}
		in := inputs[i]
		if in != nil {
			if err := appendInput(cfg, w, in, patches[i], threshold); err != nil {
				return fmt.Errorf("partition %d (%q): %s: %w", p.Index, p.Label, in.path, err)
			}
		}
		// The rest of the partition is free space.
		if free := p.Offset(l.SectorSize) + p.Length(l.SectorSize) - w.Offset(); free > 0 {
			if err := w.WriteZeroRun(free); err != nil {
				return err
			}
		}
	}
	tail, tailOffset := l.Tail()
	end := l.Size()
	if tail != nil {
		end = tailOffset
	}
	if err := w.WriteZeroRun(end - w.Offset()); err != nil {
		return err
	}
	if tail != nil {
		if err := w.WriteStreamBytes(tail); err != nil {
			return err
		}
	}
	if w.Offset() != l.Size() {
		return fmt.Errorf("internal error: the compact stream has %d bytes, the disk %d", w.Offset(), l.Size())
	}
	if err := w.Close(); err != nil {
		return err
	}
	return out.Close()
}

func appendInput(cfg *config, w *compactstream.Writer, in *input, patches []compactstream.Patch, threshold int64) error {
	if _, err := in.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if in.isStream {
		_, err := compactstream.AppendStream(w, in.file, patches)
		return err
	}
	// A FAT32 image is patched, so its bytes have to be inline. Others are
	// referenced as a whole if they are large.
	if in.kind == fsFAT32 || in.size < threshold {
		return compactstream.AppendImage(w, in.file, in.size, patches)
	}
	digest, err := compactstream.AppendImageRef(w, in.file, in.size)
	if err != nil {
		return err
	}
	if cfg.outputCASDir == "" {
		return errors.New("the image is referenced by the compact stream, which needs --output-cas-dir to hold it")
	}
	return addToCAS(cfg.outputCASDir, digest, in.path, cfg.casSymlink)
}

// addToCAS makes the file available as sha256/<hex> in the directory.
func addToCAS(dir string, digest []byte, path string, symlink bool) error {
	dst := filepath.Join(dir, "sha256", hex.EncodeToString(digest))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err == nil {
		return nil // the same content is already there
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if symlink {
		absDst, err := filepath.Abs(dst)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(filepath.Dir(absDst), abs)
		if err != nil {
			return err
		}
		return os.Symlink(rel, dst)
	}
	if err := os.Link(abs, dst); err == nil {
		return nil
	}
	src, err := os.Open(abs)
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

type layoutJSON struct {
	Table         string          `json:"partition_table"`
	SectorSize    int             `json:"sector_size"`
	Alignment     int64           `json:"alignment"`
	Size          int64           `json:"size"`
	Sectors       uint64          `json:"sectors"`
	Compact       bool            `json:"compact_stream"`
	DiskGUID      string          `json:"disk_guid,omitempty"`
	DiskSignature uint32          `json:"disk_signature,omitempty"`
	FirstUsable   uint64          `json:"first_usable_lba,omitempty"`
	LastUsable    uint64          `json:"last_usable_lba,omitempty"`
	Partitions    []partitionJSON `json:"partitions"`
}

type partitionJSON struct {
	Index       int    `json:"index"`
	Label       string `json:"label"`
	TypeGUID    string `json:"type_guid,omitempty"`
	MBRType     byte   `json:"mbr_type,omitempty"`
	GUID        string `json:"guid,omitempty"`
	Attributes  uint64 `json:"attributes,omitempty"`
	StartLBA    uint64 `json:"start_lba"`
	Sectors     uint64 `json:"sectors"`
	Offset      int64  `json:"offset"`
	Length      int64  `json:"length"`
	ContentSize int64  `json:"content_size"`
	Filesystem  string `json:"filesystem,omitempty"`
}

func writeLayoutJSON(cfg *config, l *disk.Layout, inputs []*input) error {
	doc := layoutJSON{
		Table: string(l.Table), SectorSize: l.SectorSize, Alignment: l.Alignment,
		Size: l.Size(), Sectors: l.Sectors, Compact: cfg.compact,
	}
	if l.Table == disk.GPT {
		doc.DiskGUID = l.DiskGUID.String()
		doc.FirstUsable, doc.LastUsable = l.FirstUsableLBA, l.LastUsableLBA
	} else {
		doc.DiskSignature = l.DiskSignature
	}
	for i, p := range l.Partitions {
		pj := partitionJSON{
			Index: p.Index, Label: p.Label, StartLBA: p.StartLBA, Sectors: p.Sectors,
			Offset: p.Offset(l.SectorSize), Length: p.Length(l.SectorSize), ContentSize: p.ContentSize,
		}
		if l.Table == disk.GPT {
			pj.TypeGUID, pj.GUID, pj.Attributes = p.TypeGUID.String(), p.GUID.String(), p.Attributes
		} else {
			pj.MBRType = p.MBRType
		}
		if inputs[i] != nil {
			pj.Filesystem = string(inputs[i].kind)
		}
		doc.Partitions = append(doc.Partitions, pj)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cfg.layoutJSON, append(data, '\n'), 0o644)
}
