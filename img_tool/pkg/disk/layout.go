// Package disk plans and encodes the partition table of a disk image: a GUID
// partition table (GPT, the default) or a classic MBR partition table. It is
// pure computation on sizes and bytes, using only the standard library; copying
// the contents of the partitions is up to the caller.
//
// A [Spec] lists the partitions in order with the size of their contents.
// [Plan] places them (aligned, in order), decides the size of the disk and
// derives everything that was not given (GUIDs, disk signature). The resulting
// [Layout] encodes the bytes that precede the first partition ([Layout.Head])
// and, for GPT, the backup table at the end of the disk ([Layout.Tail]).
//
// # Sizes
//
// A partition without an explicit size is as large as its contents, rounded up
// to the alignment. A partition with an explicit size may be larger than its
// contents, which leaves free space at the end of the partition. The disk is
// as small as the partitions (and, for GPT, the backup table) allow, unless
// Size or MinSize ask for more; MaxSize limits it.
//
// # Reproducibility
//
// GUIDs that are not given are derived from the specification, so equal
// specifications give equal tables.
package disk

import (
	"errors"
	"fmt"
	"unicode/utf16"
)

// Table is the kind of partition table.
type Table string

const (
	GPT Table = "gpt"
	MBR Table = "mbr"
)

const (
	// DefaultSectorSize is the logical sector size in bytes.
	DefaultSectorSize = 512
	// DefaultAlignment is the alignment of partitions in bytes.
	DefaultAlignment = 1 << 20

	gptEntries     = 128
	gptEntrySize   = 128
	gptHeaderSize  = 92
	maxMBRPrimary  = 4
	maxBootCode    = 440
	maxGPTNameLen  = 36 // UTF-16 code units
	maxMBRSectors  = 1<<32 - 1
	gptArrayBytes  = gptEntries * gptEntrySize
	minAlignedGPT  = 2 // protective MBR and the primary header
	mbrSectorLimit = 1 << 32
)

// Spec describes a disk.
type Spec struct {
	// Table is GPT (the default) or MBR.
	Table Table
	// SectorSize is the logical sector size: 512 (the default) or 4096.
	SectorSize int
	// Alignment is the alignment of the start of every partition, in bytes: a
	// multiple of the sector size. The default is 1 MiB.
	Alignment int64
	// Size is the exact size of the disk in bytes. If zero, the disk is as
	// small as possible.
	Size int64
	// MinSize and MaxSize bound the size of the disk (zero means unbounded).
	// The disk grows to MinSize; a disk that needs more than MaxSize is an
	// error.
	MinSize, MaxSize int64
	// DiskGUID is the GUID of the disk (GPT). If zero, it is derived. For MBR,
	// the first four bytes of the derived value are the disk signature unless
	// DiskGUID is given.
	DiskGUID GUID
	// BootCode is placed at the start of the first sector (at most 440 bytes):
	// the boot code of the MBR, or of the protective MBR of a GPT.
	BootCode []byte
	// Partitions are the partitions in the order they are laid out on the disk.
	Partitions []Partition
}

// Partition describes one partition.
type Partition struct {
	// Label is the name of the partition (GPT, at most 36 UTF-16 characters).
	Label string
	// Type is the partition type: a name from [TypeNames] or a GUID. The
	// default is "linux".
	Type string
	// MBRType is the type byte of the partition in an MBR. If zero, it follows
	// from Type.
	MBRType byte
	// GUID is the unique GUID of the partition (GPT). If zero, it is derived.
	GUID GUID
	// Attributes are the GPT attribute bits.
	Attributes uint64
	// Active marks the partition as bootable in an MBR (at most one).
	Active bool
	// Size is the size of the partition in bytes. If zero, the partition is as
	// large as its contents, rounded up to the alignment. A partition without
	// contents needs a size.
	Size int64
	// ContentSize is the size of what goes into the partition: a file system
	// image. Zero for an empty partition.
	ContentSize int64
}

// PlacedPartition is a partition with its place on the disk.
type PlacedPartition struct {
	Partition
	// Index is the position in the Spec, starting at zero.
	Index int
	// GUID is the unique GUID of the partition, derived if it was not given.
	GUID GUID
	// TypeGUID is the resolved GPT partition type.
	TypeGUID GUID
	// MBRType is the resolved MBR partition type.
	MBRType byte
	// StartLBA and Sectors place the partition on the disk.
	StartLBA, Sectors uint64
}

// Offset is the byte offset of the start of the partition.
func (p PlacedPartition) Offset(sectorSize int) int64 { return int64(p.StartLBA) * int64(sectorSize) }

// Length is the size of the partition in bytes.
func (p PlacedPartition) Length(sectorSize int) int64 { return int64(p.Sectors) * int64(sectorSize) }

// Layout is a planned disk.
type Layout struct {
	Table      Table
	SectorSize int
	Alignment  int64
	// Sectors is the size of the disk in sectors.
	Sectors uint64
	// DiskGUID is the GUID of the disk, derived if it was not given.
	DiskGUID GUID
	// DiskSignature is the 32 bit signature of an MBR disk.
	DiskSignature uint32
	// FirstUsableLBA and LastUsableLBA bound the partitions of a GPT disk.
	FirstUsableLBA, LastUsableLBA uint64
	// Partitions are placed in order.
	Partitions []PlacedPartition

	bootCode []byte
}

// Size is the size of the disk in bytes.
func (l *Layout) Size() int64 { return int64(l.Sectors) * int64(l.SectorSize) }

func alignUp(v, a uint64) uint64 { return (v + a - 1) / a * a }

func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }

// Plan places the partitions and decides the size of the disk.
func Plan(spec Spec) (*Layout, error) {
	if spec.Table == "" {
		spec.Table = GPT
	}
	if spec.Table != GPT && spec.Table != MBR {
		return nil, fmt.Errorf("disk: unknown partition table %q: use gpt or mbr", spec.Table)
	}
	if spec.SectorSize == 0 {
		spec.SectorSize = DefaultSectorSize
	}
	if spec.SectorSize != 512 && spec.SectorSize != 4096 {
		return nil, fmt.Errorf("disk: unsupported sector size %d: use 512 or 4096", spec.SectorSize)
	}
	if spec.Alignment == 0 {
		spec.Alignment = DefaultAlignment
	}
	ss := int64(spec.SectorSize)
	if spec.Alignment < ss || spec.Alignment%ss != 0 {
		return nil, fmt.Errorf("disk: alignment %d is not a multiple of the sector size %d", spec.Alignment, ss)
	}
	if len(spec.BootCode) > maxBootCode {
		return nil, fmt.Errorf("disk: boot code is %d bytes, at most %d fit in front of the partition table", len(spec.BootCode), maxBootCode)
	}
	if spec.Size < 0 || spec.MinSize < 0 || spec.MaxSize < 0 {
		return nil, errors.New("disk: negative disk size")
	}
	if spec.Size != 0 && (spec.MinSize != 0 || spec.MaxSize != 0) {
		return nil, errors.New("disk: size cannot be combined with min_size or max_size")
	}
	if spec.MaxSize != 0 && spec.MinSize > spec.MaxSize {
		return nil, fmt.Errorf("disk: min size %d is larger than max size %d", spec.MinSize, spec.MaxSize)
	}
	limit := gptEntries
	if spec.Table == MBR {
		limit = maxMBRPrimary
	}
	if len(spec.Partitions) > limit {
		return nil, fmt.Errorf("disk: %d partitions do not fit in a %s partition table (at most %d)", len(spec.Partitions), spec.Table, limit)
	}

	l := &Layout{
		Table:      spec.Table,
		SectorSize: spec.SectorSize,
		Alignment:  spec.Alignment,
		bootCode:   append([]byte(nil), spec.BootCode...),
	}
	l.DiskGUID = spec.DiskGUID
	if l.DiskGUID == (GUID{}) {
		l.DiskGUID = deriveDiskGUID(spec)
	}
	l.DiskSignature = uint32(l.DiskGUID[0])<<24 | uint32(l.DiskGUID[1])<<16 | uint32(l.DiskGUID[2])<<8 | uint32(l.DiskGUID[3])
	if l.DiskSignature == 0 {
		l.DiskSignature = 1
	}

	alignSectors := uint64(spec.Alignment / ss)
	arraySectors := uint64(ceilDiv(gptArrayBytes, ss))
	var cursor uint64 = 1 // after the MBR
	if spec.Table == GPT {
		l.FirstUsableLBA = minAlignedGPT + arraySectors
		cursor = l.FirstUsableLBA
	}

	active := 0
	for i, p := range spec.Partitions {
		if p.Size < 0 || p.ContentSize < 0 {
			return nil, fmt.Errorf("disk: partition %d has a negative size", i)
		}
		typeName := p.Type
		if typeName == "" {
			typeName = "linux"
		}
		ti, err := LookupType(typeName)
		if err != nil {
			return nil, fmt.Errorf("disk: partition %d: %w", i, err)
		}
		if p.MBRType != 0 {
			ti.MBRType = p.MBRType
		}
		if spec.Table == GPT && len(utf16.Encode([]rune(p.Label))) > maxGPTNameLen {
			return nil, fmt.Errorf("disk: partition %d: label %q is longer than %d UTF-16 characters", i, p.Label, maxGPTNameLen)
		}
		if p.Active {
			active++
		}

		var sectors uint64
		switch {
		case p.Size > 0:
			sectors = uint64(ceilDiv(p.Size, ss))
			if p.ContentSize > p.Size {
				return nil, fmt.Errorf("disk: partition %d (%q): the contents are %d bytes, which do not fit the size of %d bytes", i, p.Label, p.ContentSize, p.Size)
			}
		case p.ContentSize > 0:
			sectors = alignUp(uint64(ceilDiv(p.ContentSize, ss)), alignSectors)
		default:
			return nil, fmt.Errorf("disk: partition %d (%q) has no contents and no size", i, p.Label)
		}

		start := alignUp(cursor, alignSectors)
		placed := PlacedPartition{Partition: p, Index: i, TypeGUID: ti.GUID, MBRType: ti.MBRType, StartLBA: start, Sectors: sectors}
		placed.GUID = p.GUID
		if placed.GUID == (GUID{}) {
			placed.GUID = derive([]byte("partition"), l.DiskGUID[:], []byte{byte(i), byte(i >> 8)})
		}
		l.Partitions = append(l.Partitions, placed)
		cursor = start + sectors
	}
	if active > 1 {
		return nil, errors.New("disk: only one partition can be active")
	}
	if spec.Table == GPT && active > 0 {
		return nil, errors.New("disk: the active flag only exists in an MBR partition table; use the legacy_bios_bootable attribute for a GPT")
	}

	// The disk ends after the last partition, and for GPT after the backup table.
	need := cursor
	if spec.Table == GPT {
		need += arraySectors + 1
	}
	switch {
	case spec.Size != 0:
		l.Sectors = uint64(ceilDiv(spec.Size, ss))
		if l.Sectors < need {
			return nil, fmt.Errorf("disk: the partitions need %d bytes, more than the disk size of %d bytes", need*uint64(ss), spec.Size)
		}
	default:
		l.Sectors = need
		if spec.MinSize != 0 {
			l.Sectors = max(l.Sectors, uint64(ceilDiv(spec.MinSize, ss)))
		}
		if spec.MaxSize != 0 && l.Sectors > uint64(spec.MaxSize/ss) {
			return nil, fmt.Errorf("disk: the partitions need %d bytes, more than the maximum disk size of %d bytes", need*uint64(ss), spec.MaxSize)
		}
	}
	if spec.Table == GPT {
		l.LastUsableLBA = l.Sectors - arraySectors - 2
	} else {
		if l.Sectors > mbrSectorLimit {
			return nil, fmt.Errorf("disk: an MBR partition table cannot describe a disk of %d sectors (at most 2^32)", l.Sectors)
		}
		for _, p := range l.Partitions {
			if p.StartLBA+p.Sectors > mbrSectorLimit {
				return nil, fmt.Errorf("disk: partition %d ends beyond the 2^32 sectors an MBR can address; use gpt", p.Index)
			}
		}
	}
	return l, nil
}

// deriveDiskGUID derives the GUID of the disk from the specification.
func deriveDiskGUID(spec Spec) GUID {
	parts := [][]byte{[]byte("disk"), []byte(spec.Table), {byte(spec.SectorSize >> 8), byte(spec.SectorSize)}}
	for _, v := range []int64{spec.Alignment, spec.Size, spec.MinSize, spec.MaxSize} {
		parts = append(parts, []byte(fmt.Sprint(v)))
	}
	parts = append(parts, spec.BootCode)
	for _, p := range spec.Partitions {
		parts = append(parts, []byte(fmt.Sprintf("%s|%s|%d|%x|%d|%d|%v", p.Label, p.Type, p.MBRType, p.GUID, p.Size, p.ContentSize, p.Active)))
	}
	return derive(parts...)
}
