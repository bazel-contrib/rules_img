// Package ddcmd implements `img dd`: it writes the bytes of a disk image to
// stdout, a file or a block device, a bit like dd(1).
//
// The input is a raw image, or an (experimental) extent stream or a compact stream
// that describes one. For a stream, the blobs and the streams it references are
// read from content-addressed directories (--cas-dir), the Bazel disk cache and
// the remote cache, in this order, so the image can be reconstructed lazily:
// nothing needs to be downloaded or shipped before it is needed. The holes of an
// extent stream are not written to a file or device that is known to read as
// zeros there.
package ddcmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// operands are the dd-style operands (name=value) that are also accepted as
// flags.
var operands = map[string]bool{
	"if": true, "of": true, "bs": true, "skip": true, "seek": true, "count": true,
	"status": true, "conv": true, "iflag": true, "oflag": true, "cas-dir": true, "blob-descriptor": true,
	"cas-dir-manifest": true,
}

// normalizeArgs turns dd-style operands (bs=4M) into flags (--bs=4M).
func normalizeArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if name, _, ok := strings.Cut(a, "="); ok && !strings.HasPrefix(a, "-") && operands[name] {
			a = "--" + a
		}
		out = append(out, a)
	}
	return out
}

type stringsFlag []string

func (f *stringsFlag) String() string { return strings.Join(*f, ",") }

func (f *stringsFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

const usage = `Usage: img dd if=<image|stream> [of=<file|device>] [OPERANDS]
       img dd blob-descriptor=<json> [of=<file|device>] [OPERANDS]

Writes the bytes of a disk image, like dd(1). Operands can also be given as flags
(--bs=4M). Without of=, the bytes go to stdout.

The input is a raw image, an extent stream (experimental) or a compact stream.
The blobs and streams a stream references are read from the --cas-dir
directories, the Bazel disk cache ($IMG_DISK_CACHE) and the remote cache
($IMG_REAPI_ENDPOINT), in this order: without --cas-dir, they are fetched lazily.

Operands:
  if=FILE          the image or stream to read (required, unless blob-descriptor is
                   given; it can only be given once)
  blob-descriptor=FILE
                   instead of if=: a JSON file with the "digest" (sha256:<hex>) and the
                   "size" of the image, which is fetched from the --cas-dir directories,
                   the disk cache or the remote cache, as the blobs of a stream are
  of=FILE          where to write: a file, a block device, or - for stdout (default)
  bs=BYTES         block size for reading, writing and for detecting holes (default 1MiB)
  skip=N           skip N input blocks
  seek=N           skip N output blocks, leaving them as they are
  count=N          copy only N input blocks
  status=LEVEL     none, noxfer, or progress (default: a summary at the end)
  conv=CONVS       comma-separated list of:
                     sparse   don't write holes and blocks of zeros, seek over them (the
                              default for an output file that is created or truncated;
                              only right if the output already reads as zeros there)
                     nosparse always write zeros
                     notrunc  don't truncate the output file
                     fsync    synchronize the output before finishing
  iflag=FLAGS      skip_bytes, count_bytes: skip= and count= are bytes
  oflag=FLAGS      seek_bytes: seek= is in bytes
  cas-dir=DIR      content-addressed directory (sha256/<hex>) with referenced blobs and
                   streams (can be repeated)
  cas-dir-manifest=FILE
                   a file with one runfiles path of such a directory per line, added to
                   the cas-dir directories. A target built by image_dd uses it: a
                   launcher embeds only a few arguments, however many directories the
                   image needs

Numbers can have a suffix: c=1, w=2, b=512, kB/KB=1000, K/KiB=1024, MB, M/MiB, GB, G/GiB, ...`

type options struct {
	in, out    string
	descriptor string
	bs         int64
	skip, seek int64
	count      int64 // -1: until the end
	status     string
	notrunc    bool
	sparse     bool // conv=sparse
	nosparse   bool
	fsync      bool
	skipBytes  bool
	countBytes bool
	seekBytes  bool
	casDirs    []string
	// casDirManifest is a file with one runfiles path of a content-addressed
	// directory per line, resolved when the run starts.
	casDirManifest string
}

// DDProcess runs `img dd`.
func DDProcess(ctx context.Context, args []string) {
	opts, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "img dd: %v\n\n%s\n", err, usage)
		os.Exit(1)
	}
	if err := run(ctx, opts, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "img dd: %v\n", err)
		os.Exit(1)
	}
}

func parseArgs(args []string) (*options, error) {
	o := &options{}
	var bs, skip, seek, count, conv, iflag, oflag string
	var casDirs stringsFlag
	flagSet := flag.NewFlagSet("dd", flag.ContinueOnError)
	flagSet.SetOutput(io.Discard)
	// The input is counted, not just assigned: a target built by `image_dd`
	// passes it to the tool, and a second if= on the command line would
	// silently write something else than the target's image.
	inputs := 0
	flagSet.Func("if", "", func(v string) error { o.in, inputs = v, inputs+1; return nil })
	flagSet.Func("blob-descriptor", "", func(v string) error { o.descriptor, inputs = v, inputs+1; return nil })
	flagSet.StringVar(&o.out, "of", "", "")
	flagSet.StringVar(&bs, "bs", "1MiB", "")
	flagSet.StringVar(&skip, "skip", "0", "")
	flagSet.StringVar(&seek, "seek", "0", "")
	flagSet.StringVar(&count, "count", "", "")
	flagSet.StringVar(&o.status, "status", "", "")
	flagSet.StringVar(&conv, "conv", "", "")
	flagSet.StringVar(&iflag, "iflag", "", "")
	flagSet.StringVar(&oflag, "oflag", "", "")
	flagSet.Var(&casDirs, "cas-dir", "")
	flagSet.StringVar(&o.casDirManifest, "cas-dir-manifest", "", "")
	if err := flagSet.Parse(normalizeArgs(args)); err != nil {
		return nil, err
	}
	if flagSet.NArg() != 0 {
		return nil, fmt.Errorf("unexpected argument %q", flagSet.Arg(0))
	}
	if inputs > 1 {
		return nil, fmt.Errorf("the input is given more than once: if= and blob-descriptor= can each appear once, and only one of them")
	}
	if (o.in == "") == (o.descriptor == "") {
		return nil, fmt.Errorf("exactly one of if= and blob-descriptor= is required")
	}
	o.casDirs = casDirs

	var err error
	if o.bs, err = parseNumber(bs); err != nil || o.bs <= 0 || o.bs > maxBlockSize {
		return nil, fmt.Errorf("invalid bs=%q", bs)
	}
	if o.skip, err = parseNumber(skip); err != nil {
		return nil, fmt.Errorf("invalid skip=%q", skip)
	}
	if o.seek, err = parseNumber(seek); err != nil {
		return nil, fmt.Errorf("invalid seek=%q", seek)
	}
	o.count = -1
	if count != "" {
		if o.count, err = parseNumber(count); err != nil {
			return nil, fmt.Errorf("invalid count=%q", count)
		}
	}
	switch o.status {
	case "", "none", "noxfer", "progress":
	default:
		return nil, fmt.Errorf("invalid status=%q (none, noxfer or progress)", o.status)
	}
	for _, c := range splitList(conv) {
		switch c {
		case "sparse":
			o.sparse = true
		case "nosparse":
			o.nosparse = true
		case "notrunc":
			o.notrunc = true
		case "fsync", "fdatasync":
			o.fsync = true
		default:
			return nil, fmt.Errorf("invalid conv=%q", c)
		}
	}
	if o.sparse && o.nosparse {
		return nil, fmt.Errorf("conv=sparse and conv=nosparse contradict each other")
	}
	for _, f := range splitList(iflag) {
		switch f {
		case "skip_bytes":
			o.skipBytes = true
		case "count_bytes":
			o.countBytes = true
		default:
			return nil, fmt.Errorf("invalid iflag=%q", f)
		}
	}
	for _, f := range splitList(oflag) {
		if f != "seek_bytes" {
			return nil, fmt.Errorf("invalid oflag=%q", f)
		}
		o.seekBytes = true
	}
	return o, nil
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// maxBlockSize bounds bs, as a block of that size is held in memory.
const maxBlockSize = 1 << 30

// parseNumber parses a dd number: digits with an optional multiplier suffix.
func parseNumber(s string) (int64, error) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("not a number")
	}
	var n int64
	for _, c := range s[:i] {
		if n > (1<<62)/10 {
			return 0, fmt.Errorf("too large")
		}
		n = n*10 + int64(c-'0')
	}
	mult, ok := map[string]int64{
		"": 1, "c": 1, "w": 2, "b": 512,
		"kB": 1000, "KB": 1000, "K": 1 << 10, "KiB": 1 << 10, "k": 1 << 10,
		"MB": 1000 * 1000, "M": 1 << 20, "MiB": 1 << 20,
		"GB": 1000 * 1000 * 1000, "G": 1 << 30, "GiB": 1 << 30,
		"TB": 1000 * 1000 * 1000 * 1000, "T": 1 << 40, "TiB": 1 << 40,
	}[s[i:]]
	if !ok {
		return 0, fmt.Errorf("unknown suffix %q", s[i:])
	}
	if n > (1<<62)/mult {
		return 0, fmt.Errorf("too large")
	}
	return n * mult, nil
}
