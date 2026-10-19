package compress

import (
	"bytes"
	"encoding/hex"
	"io"
	"math/rand/v2"
	"runtime"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// zstdJobSpanningInput returns deterministic, compressible data that is large
// enough to span several of the zstd encoder's parallel jobs. A job is four
// window sizes of input: 32 MiB at the largest window the encoder picks (8 MiB,
// for every level but the fastest), so this input always crosses at least one
// job boundary and ends in a partial job.
func zstdJobSpanningInput(t *testing.T) []byte {
	t.Helper()
	const size = 48 << 20
	words := []string{
		"usr/local/bin", "etc/ssl/certs", "var/log", "lib/x86_64-linux-gnu",
		"rules_img", "layer.tar", "sha256:", "application/vnd.oci.image",
	}
	rng := rand.New(rand.NewPCG(1, 2))
	var buf bytes.Buffer
	buf.Grow(size)
	for buf.Len() < size {
		buf.WriteString(words[rng.IntN(len(words))])
		buf.WriteByte('/')
		if rng.IntN(16) == 0 {
			// Occasional incompressible run, so the encoder also has to emit
			// raw literals and not only matches.
			var noise [64]byte
			for i := range noise {
				noise[i] = byte(rng.Uint32())
			}
			buf.Write(noise[:])
		}
	}
	return buf.Bytes()[:size]
}

// TestZstdOutputIndependentOfConcurrency pins down the determinism guarantee of
// the parallel zstd stream encoding enabled in zstdEncoderOptions (see
// https://github.com/bazel-contrib/rules_img/issues/806): compressing the same
// input at the same level must produce the same bytes no matter how many cores
// the machine has or how many compressor jobs were requested, so that a layer
// built on a laptop matches one built on a 128-core CI machine and remote cache
// hits survive.
//
// The failure mode this guards against is subtle: the encoder silently falls
// back to sequential encoding - which emits different (equally valid) bytes -
// when its concurrency is <= 1 or when an option it cannot combine with
// parallel jobs (such as a dictionary) is set.
func TestZstdOutputIndependentOfConcurrency(t *testing.T) {
	data := zstdJobSpanningInput(t)

	original := runtime.GOMAXPROCS(0)
	t.Cleanup(func() { runtime.GOMAXPROCS(original) })

	// Levels in the range accepted by `img layer --compression-level` that the
	// encoder maps to distinct window, overlap and block sizes - and therefore
	// to distinct job layouts - plus the unset case (which uses the encoder's
	// default level, and a different code path to construct the writer).
	levels := []int{-1, 1, 3, 4}
	// Configurations that all have to agree: a single core, fewer cores than
	// requested jobs, more cores than this machine has, more jobs than cores,
	// a job count below the floor, and "all cores". GOMAXPROCS=1 and jobs=1 are
	// the cases that would disable parallel encoding inside the encoder if
	// zstdMinEncoderConcurrency did not compensate for them.
	type config struct {
		name  string
		procs int
		jobs  *CompressorJobs
	}
	jobs := func(n int) *CompressorJobs { j := CompressorJobs(n); return &j }
	allCores := CompressorJobs(-1)
	configs := []config{
		{name: "procs=1", procs: 1},
		{name: "procs=2", procs: 2},
		{name: "procs=3", procs: 3},
		{name: "procs=17", procs: 17},
		{name: "procs=4,jobs=1", procs: 4, jobs: jobs(1)},
		{name: "procs=4,jobs=2", procs: 4, jobs: jobs(2)},
		{name: "procs=1,jobs=7", procs: 1, jobs: jobs(7)},
		{name: "procs=4,jobs=24", procs: 4, jobs: jobs(24)},
		{name: "procs=4,jobs=nproc", procs: 4, jobs: &allCores},
	}

	for _, level := range levels {
		var reference, referenceName string
		for _, cfg := range configs {
			runtime.GOMAXPROCS(cfg.procs)

			var compressed bytes.Buffer
			var opts []Option
			if level >= 0 {
				opts = append(opts, CompressionLevel(level))
			}
			if cfg.jobs != nil {
				opts = append(opts, *cfg.jobs)
			}
			appender, err := AppenderFactory("sha256", "zstd", &compressed, opts...)
			if err != nil {
				t.Fatalf("level=%d %s: creating appender: %v", level, cfg.name, err)
			}
			if _, err := appender.Write(data); err != nil {
				t.Fatalf("level=%d %s: writing data: %v", level, cfg.name, err)
			}
			state, err := appender.Finalize()
			if err != nil {
				t.Fatalf("level=%d %s: finalizing: %v", level, cfg.name, err)
			}
			digest := hex.EncodeToString(state.OuterHash)

			// The compressed bytes must still decode to the input: a job
			// boundary that drops or duplicates the overlap between jobs would
			// otherwise show up as a digest that is merely self-consistent.
			decoder, err := zstd.NewReader(bytes.NewReader(compressed.Bytes()))
			if err != nil {
				t.Fatalf("level=%d %s: creating decoder: %v", level, cfg.name, err)
			}
			decompressed, err := io.ReadAll(decoder)
			decoder.Close()
			if err != nil {
				t.Fatalf("level=%d %s: decompressing: %v", level, cfg.name, err)
			}
			if !bytes.Equal(decompressed, data) {
				t.Fatalf("level=%d %s: decompressed output does not match the input (%d bytes vs %d)",
					level, cfg.name, len(decompressed), len(data))
			}

			if reference == "" {
				reference, referenceName = digest, cfg.name
				continue
			}
			if digest != reference {
				t.Errorf("level=%d: compressed output depends on the number of cores or jobs: %s gives sha256 %s, %s gives %s",
					level, cfg.name, digest, referenceName, reference)
			}
		}
	}
}

// TestZstdEncoderConcurrency checks how the worker count is derived from
// --compressor-jobs. The floor is load-bearing: below 2 the encoder drops out
// of the parallel path and changes the output bytes. Nothing is capped here -
// only the job count recorded in a compact stream is bounded, see
// maxRecordedCompressorJobs in cmd/layer.
func TestZstdEncoderConcurrency(t *testing.T) {
	jobs := func(n int) []Option { return []Option{CompressorJobs(n)} }
	for _, tc := range []struct {
		name string
		opts []Option
		want int
	}{
		{name: "unset", want: zstdMinEncoderConcurrency},
		{name: "jobs below the floor", opts: jobs(1), want: zstdMinEncoderConcurrency},
		{name: "jobs zero", opts: jobs(0), want: zstdMinEncoderConcurrency},
		{name: "jobs", opts: jobs(6), want: 6},
		{name: "many jobs", opts: jobs(512), want: 512},
		{name: "all cores", opts: jobs(-1), want: max(runtime.NumCPU(), zstdMinEncoderConcurrency)},
	} {
		if got := zstdEncoderConcurrency(collectOptions(tc.opts...)); got != tc.want {
			t.Errorf("%s: zstd encoder concurrency = %d, want %d", tc.name, got, tc.want)
		}
	}
}
