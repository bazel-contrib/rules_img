// Package diskimage implements `img disk-image`: it assembles a disk image with
// a GPT or MBR partition table from file system images.
//
// The output is either the disk image itself (a sparse file), or, with
// --compact, a compact stream of it. A compact stream is cheap to produce for a
// disk of any size: the partitions that are compact streams themselves (of
// EROFS, ext4 or FAT32 images, for example) are spliced in, so the stream of
// the disk references the blobs of their files directly, and only the partition
// table, the file system metadata and the free space are inline.
package diskimage

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
)

// DiskImageProcess runs `img disk-image`.
func DiskImageProcess(ctx context.Context, args []string) {
	cfg := &config{}
	flagSet := flag.NewFlagSet("disk-image", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Assembles a disk image with a GPT or MBR partition table from file system images.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img disk-image [OPTIONS] --output <disk image|compact stream>\n\n")
		fmt.Fprintf(flagSet.Output(), "Each --partition starts a partition. The --partition-* flags that follow it describe it.\n\n")
		flagSet.PrintDefaults()
		examples := []string{
			"img disk-image --output disk.img --partition ESP --partition-type esp --partition-image esp.fat32 --partition root --partition-type root-x86-64 --partition-image root.ext4",
			"img disk-image --compact --output disk.cstream --output-cas-dir disk-cas --cas-dir layer-cas --partition root --partition-cstream root.cstream",
		}
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, e := range examples {
			fmt.Fprintf(flagSet.Output(), "  $ %s\n", e)
		}
	}

	flagSet.StringVar(&cfg.table, "partition-table", "gpt", `The kind of partition table: "gpt" or "mbr".`)
	flagSet.IntVar(&cfg.sectorSize, "sector-size", 512, `Logical sector size of the disk in bytes: 512 or 4096.`)
	flagSet.StringVar(&cfg.alignment, "alignment", "1MiB", `Alignment of the start of every partition (a multiple of the sector size).`)
	flagSet.StringVar(&cfg.size, "size", "", `Exact size of the disk. By default it is as small as the partitions allow.`)
	flagSet.StringVar(&cfg.minSize, "min-size", "", `Minimum size of the disk; the disk is extended with free space to reach it.`)
	flagSet.StringVar(&cfg.maxSize, "max-size", "", `Maximum size of the disk; it is an error if the partitions need more.`)
	flagSet.StringVar(&cfg.diskGUID, "disk-guid", "", `GUID of the disk. By default one is derived from the contents of the specification.`)
	flagSet.StringVar(&cfg.bootCode, "boot-code", "", `File with up to 440 bytes of boot code for the first sector (the MBR, or the protective MBR of a GPT).`)
	flagSet.Func("cas-dir", `Content-addressed directory (sha256/<hex>) with the blobs that compact stream partitions reference. Needed to write the disk image itself. Can be repeated.`, func(v string) error {
		cfg.casDirs = append(cfg.casDirs, v)
		return nil
	})
	flagSet.StringVar(&cfg.output, "output", "", `Path to write the disk image, or with --compact the compact stream (required).`)
	flagSet.BoolVar(&cfg.compact, "compact", false, `Write a compact stream of the disk image instead of the disk image.`)
	flagSet.StringVar(&cfg.outputCASDir, "output-cas-dir", "", `With --compact: directory (sha256/<hex>) that is filled with the file system images the compact stream references as a whole.`)
	flagSet.BoolVar(&cfg.casSymlink, "output-cas-symlink", false, `Fill --output-cas-dir with relative symbolic links instead of hard links or copies.`)
	flagSet.StringVar(&cfg.inlineThreshold, "compact-stream-inline-threshold", "1MiB", `With --compact: file system images below this size are stored inline instead of being referenced as a whole (FAT32 images are always inline, because their boot sector is patched).`)
	flagSet.StringVar(&cfg.layoutJSON, "layout-json", "", `Path to write a JSON description of the layout of the disk (offsets, sizes, GUIDs of the partitions).`)

	flagSet.Func("partition", `Start a partition with this label (GPT partition name). The --partition-* flags that follow describe it.`, func(v string) error {
		cfg.partitions = append(cfg.partitions, &partitionConfig{label: v})
		return nil
	})
	partFlag := func(name, usage string, set func(p *partitionConfig, v string) error) {
		flagSet.Func(name, usage, func(v string) error {
			if len(cfg.partitions) == 0 {
				return fmt.Errorf("--%s needs a --partition before it", name)
			}
			return set(cfg.partitions[len(cfg.partitions)-1], v)
		})
	}
	partBool := func(name, usage string, set func(p *partitionConfig)) {
		flagSet.BoolFunc(name, usage, func(string) error {
			if len(cfg.partitions) == 0 {
				return fmt.Errorf("--%s needs a --partition before it", name)
			}
			set(cfg.partitions[len(cfg.partitions)-1])
			return nil
		})
	}
	partFlag("partition-type", `Type of the partition: a name (esp, linux, root-x86-64, root-arm64, swap, home, xbootldr, bios-boot, ...) or a GUID.`, func(p *partitionConfig, v string) error { p.typ = v; return nil })
	partFlag("partition-guid", `Unique GUID of the partition. By default one is derived.`, func(p *partitionConfig, v string) error { p.guid = v; return nil })
	partFlag("partition-size", `Size of the partition. By default it is as large as its contents, padded to the alignment. A larger size leaves free space at the end of the partition.`, func(p *partitionConfig, v string) error { p.size = v; return nil })
	partFlag("partition-attribute", `GPT attribute of the partition: required, no_block_io, legacy_bios_bootable, growfs, read_only, hidden, no_automount, or a number. Can be repeated.`, func(p *partitionConfig, v string) error {
		p.attrs = append(p.attrs, v)
		return nil
	})
	partFlag("partition-mbr-type", `Type byte of the partition in an MBR (a number). By default it follows from the type.`, func(p *partitionConfig, v string) error {
		n, err := strconv.ParseUint(v, 0, 8)
		if err != nil || n == 0 {
			return fmt.Errorf("--partition-mbr-type %q is not a type byte like 0x83", v)
		}
		p.mbrType = byte(n)
		return nil
	})
	partBool("partition-active", `Mark the partition as bootable in an MBR.`, func(p *partitionConfig) { p.active = true })
	partFlag("partition-image", `File system image to place in the partition.`, func(p *partitionConfig, v string) error { p.image = v; return nil })
	partFlag("partition-cstream", `Compact stream of a file system image to place in the partition. With --compact it is preferred over --partition-image, and its references are kept; otherwise the --cas-dir directories have to have its blobs.`, func(p *partitionConfig, v string) error { p.cstream = v; return nil })
	partBool("partition-no-hidden-sectors", `Do not set the hidden sectors of a FAT32 file system to the first sector of the partition.`, func(p *partitionConfig) { p.noHiddenSectors = true })

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}
	if flagSet.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "Error: unexpected arguments %v\n", flagSet.Args())
		flagSet.Usage()
		os.Exit(1)
	}
	if cfg.output == "" {
		fmt.Fprintf(os.Stderr, "Error: --output is required\n")
		flagSet.Usage()
		os.Exit(1)
	}
	if err := run(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
