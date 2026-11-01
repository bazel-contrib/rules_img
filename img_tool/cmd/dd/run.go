package ddcmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/bazel-contrib/rules_img/img_tool/cmd/deploy"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/bytesize"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/deployvfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/extentstream"
)

// stats describe what a run did.
type stats struct {
	bytes, written, skipped int64
	elapsed                 time.Duration
}

func run(ctx context.Context, o *options, stdout *os.File, stderr io.Writer) error {
	var closers []io.Closer
	defer func() {
		for _, c := range closers {
			c.Close()
		}
	}()
	// The remote cache is only opened for a stream: a raw image has nothing to
	// fetch.
	store := func() (compactstream.BlobStore, error) {
		reader, closer, err := deploy.OpenCASFromEnv(ctx, 4)
		if err != nil {
			return nil, fmt.Errorf("opening the remote cache: %w", err)
		}
		closers = append(closers, closer)
		return &hintStore{deployvfs.NewBlobStore(o.casDirs, deploy.DiskCachePathFromEnv(), reader), len(o.casDirs) > 0 || reader != nil}, nil
	}
	var src source
	var err error
	if o.descriptor != "" {
		src, err = openBlobSource(o.descriptor, store)
	} else {
		src, err = openSource(o.in, store)
	}
	if err != nil {
		return err
	}
	defer src.close()

	skip, err := toBytes(o.skip, o.bs, o.skipBytes)
	if err != nil {
		return fmt.Errorf("skip: %w", err)
	}
	seek, err := toBytes(o.seek, o.bs, o.seekBytes)
	if err != nil {
		return fmt.Errorf("seek: %w", err)
	}
	n := int64(-1)
	if o.count >= 0 {
		if n, err = toBytes(o.count, o.bs, o.countBytes); err != nil {
			return fmt.Errorf("count: %w", err)
		}
	}
	if size := src.size(); size >= 0 {
		avail := max(0, size-skip)
		if n < 0 || n > avail {
			n = avail
		}
		skip = min(skip, size)
	}

	f, mode, err := openOutput(o, stdout, seek)
	if err != nil {
		return err
	}
	if f != stdout {
		defer f.Close()
	}
	if mode.sparse && !mode.seekOK {
		mode.sparse = false
	}
	if o.sparse && !mode.seekOK {
		fmt.Fprintln(stderr, "img dd: warning: the output cannot be seeked, conv=sparse is ignored")
	}
	out := newSink(f, o.bs, seek, mode.seekOK, mode.sparse, mode.regular)

	start := time.Now()
	stopProgress := func() {}
	if o.status == "progress" {
		stopProgress = reportProgress(stderr, &out.advanced, n)
	}
	copyErr := src.copyRange(ctx, out, skip, n)
	if err := out.finish(); copyErr == nil {
		copyErr = err
	}
	if copyErr == nil && o.fsync {
		copyErr = f.Sync()
	}
	stopProgress()
	if copyErr != nil {
		return copyErr
	}
	if f != stdout {
		if err := f.Close(); err != nil {
			return err
		}
	}
	st := stats{bytes: out.advanced.Load(), written: out.written, skipped: out.skipped, elapsed: time.Since(start)}
	switch o.status {
	case "none":
	case "noxfer":
		fmt.Fprintf(stderr, "%d bytes copied\n", st.bytes)
	default:
		fmt.Fprintln(stderr, summary(st, mode.sparse))
	}
	return nil
}

// hintStore adds a hint to the error of a blob that no source has.
type hintStore struct {
	compactstream.MultiBlobStore
	configured bool
}

func (s *hintStore) ReaderForBlob(ctx context.Context, digest []byte, size int64) (io.ReadCloser, error) {
	r, err := s.MultiBlobStore.ReaderForBlob(ctx, digest, size)
	if err != nil && !s.configured {
		err = fmt.Errorf("%w; pass --cas-dir or set IMG_REAPI_ENDPOINT to read it from a remote cache", err)
	}
	return r, err
}

var _ compactstream.BlobStore = (*hintStore)(nil)
var _ extentstream.HoleWriter = (*sink)(nil)

func toBytes(n, bs int64, inBytes bool) (int64, error) {
	if inBytes {
		return n, nil
	}
	if bs != 0 && n > (1<<62)/bs {
		return 0, fmt.Errorf("too large")
	}
	return n * bs, nil
}

type outputMode struct {
	seekOK, regular, sparse bool
}

// openOutput opens the output and decides whether it is written sparsely: holes
// are only skipped where nothing but zeros can be, which is a file that is
// created or truncated, or if conv=sparse says so.
func openOutput(o *options, stdout *os.File, seek int64) (*os.File, outputMode, error) {
	if o.out == "" || o.out == "-" {
		if seek > 0 {
			return nil, outputMode{}, fmt.Errorf("seek needs an output file (of=)")
		}
		return stdout, outputMode{}, nil
	}
	flags := os.O_WRONLY | os.O_CREATE
	st, statErr := os.Stat(o.out)
	fresh := statErr != nil
	truncate := (fresh || st.Mode().IsRegular()) && !o.notrunc && seek == 0
	if truncate {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(o.out, flags, 0o666)
	if err != nil {
		return nil, outputMode{}, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, outputMode{}, err
	}
	m := outputMode{regular: fi.Mode().IsRegular()}
	m.seekOK = m.regular || fi.Mode()&os.ModeDevice != 0
	switch {
	case o.nosparse:
	case o.sparse:
		m.sparse = m.seekOK
	default:
		m.sparse = m.regular && (truncate || fresh)
	}
	if seek > 0 {
		if _, err := f.Seek(seek, io.SeekStart); err != nil {
			f.Close()
			return nil, outputMode{}, fmt.Errorf("seeking in %s: %w", o.out, err)
		}
	}
	return f, m, nil
}

func rate(bytes int64, d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	return bytesize.Format(int64(float64(bytes)/d.Seconds())) + "/s"
}

func summary(st stats, sparse bool) string {
	s := fmt.Sprintf("%d bytes (%s) copied, %.2f s, %s", st.bytes, bytesize.Format(st.bytes), st.elapsed.Seconds(), rate(st.bytes, st.elapsed))
	if sparse {
		s += fmt.Sprintf("; wrote %s, skipped %s of holes and zeros", bytesize.Format(st.written), bytesize.Format(st.skipped))
	}
	return s
}
