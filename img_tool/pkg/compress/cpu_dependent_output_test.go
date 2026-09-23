package compress

import (
	"bytes"
	"encoding/hex"
	"os"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

// TestCPUDependentOutputRegression is a regression test for
// https://github.com/malt3/pgzip-cpu-dependent-output-repro: compressing the
// same input at the same level previously produced different (though both
// valid) compressed bytes on amd64 vs arm64, because of an FMA-contraction
// rounding difference in EstimatedBits (used to decide whether to reuse the
// previous Huffman table). This affects both the stdlib
// compress/flate encoder (as of Go 1.27, see
// https://go-review.googlesource.com/c/go/+/834885) and klauspost/compress's
// flate, used by pgzip (see
// https://github.com/klauspost/compress/pull/1224), independent of Go
// version.
//
// The testdata is a real input known to hit the divergent code path at
// level 3. Golden sha256 hashes are of the compressed bytes; if compression
// ever becomes CPU-architecture-dependent again (a stdlib/klauspost
// regression, or a new divergent input on a CI runner's architecture), this
// test will fail with a hash mismatch on whichever architecture disagrees.
func TestCPUDependentOutputRegression(t *testing.T) {
	data, err := os.ReadFile("testdata/cpu-dependent-output-regression.bin")
	if err != nil {
		t.Fatal(err)
	}

	type testCase struct {
		name string
		// newAppender builds the appender under test. Most cases go through
		// AppenderFactory (the real compressor-selection pipeline used in
		// production); the "pgzip/jobs=1" case bypasses it and constructs the
		// pgzip appender directly, since AppenderFactory only selects pgzip
		// when jobs > 1, but pgzip's flate encoder is exercised (and was
		// found to diverge) at jobs=1 too.
		newAppender   func(w *bytes.Buffer) (api.Appender, error)
		wantSHA256Hex string
	}

	factoryCase := func(name string, level int, jobs *int, wantSHA256Hex string) testCase {
		return testCase{
			name: name,
			newAppender: func(w *bytes.Buffer) (api.Appender, error) {
				opts := []Option{CompressionLevel(level)}
				if jobs != nil {
					opts = append(opts, CompressorJobs(*jobs))
				}
				return AppenderFactory("sha256", "gzip", w, opts...)
			},
			wantSHA256Hex: wantSHA256Hex,
		}
	}
	jobs := func(n int) *int { return &n }

	cases := []testCase{
		// jobs unset or 1: AppenderFactory selects the stdlib-backed gzip
		// appender.
		factoryCase("gzip/level=1", 1, nil, "b2c9e0bfd19669b7ffe1e1829f51ce960dab5e53ea8e38eeeb3f91d4d01f0003"),
		factoryCase("gzip/level=3", 3, nil, "2d606bb2c62d860193f8c29dcee4131bed24cdc0845e3280e2f0680e09b29fd5"),
		factoryCase("gzip/level=6", 6, nil, "7249a47bc627eb07c009427aabb3c0640c9079bd28294b11b20d8d4c63c42aac"),
		factoryCase("gzip/level=9", 9, nil, "7d1ca8d46c2837ffcc631c501732a0e95825f4230c34d1d4c5fd5d81f0f3063f"),
		// jobs > 1: AppenderFactory selects the klauspost/pgzip appender.
		factoryCase("pgzip/level=1,jobs=2", 1, jobs(2), "6551f4ed51656eb6a53fd4a0c7bbe2b86ca6445de75bb833746c3c1cdf3de388"),
		factoryCase("pgzip/level=3,jobs=2", 3, jobs(2), "5b9cc20bb02558027778edf7d71d7ed6e4742f6215690ce0a69c8476345a2e98"),
		factoryCase("pgzip/level=6,jobs=2", 6, jobs(2), "b57427314526306b9211ad4e52ffb9bc2a2d15a5c69d3056185d659bf36defa4"),
		factoryCase("pgzip/level=9,jobs=2", 9, jobs(2), "2af2e0a8683845c4d3110969fa7500a903db12d43d1fa264effe6decc5ef4dcd"),
		factoryCase("pgzip/level=3,jobs=4", 3, jobs(4), "5b9cc20bb02558027778edf7d71d7ed6e4742f6215690ce0a69c8476345a2e98"),
		{
			// The exact scenario from the upstream repro: pgzip at level 3
			// with SetConcurrency(1<<20, 1), constructed directly since
			// AppenderFactory would otherwise route jobs=1 to plain gzip.
			// This sha256 matches the amd64 value reported at
			// https://github.com/malt3/pgzip-cpu-dependent-output-repro for
			// the same input and settings: the fix makes arm64 converge to
			// amd64's (already portable) result rather than the other way
			// around.
			name: "pgzip/level=3,jobs=1",
			newAppender: func(w *bytes.Buffer) (api.Appender, error) {
				return NewSHA256PGzipAppender(w, CompressionLevel(3), CompressorJobs(1))
			},
			wantSHA256Hex: "5b9cc20bb02558027778edf7d71d7ed6e4742f6215690ce0a69c8476345a2e98",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			appender, err := tc.newAppender(&buf)
			if err != nil {
				t.Fatalf("failed to create appender: %v", err)
			}
			if _, err := appender.Write(data); err != nil {
				t.Fatalf("failed to write data: %v", err)
			}
			state, err := appender.Finalize()
			if err != nil {
				t.Fatalf("failed to finalize appender: %v", err)
			}
			gotSHA256Hex := hex.EncodeToString(state.OuterHash)
			if gotSHA256Hex != tc.wantSHA256Hex {
				t.Errorf("sha256 of compressed output = %s, want %s\n"+
					"compressed output is not reproducible across CPU architectures; see https://github.com/malt3/pgzip-cpu-dependent-output-repro",
					gotSHA256Hex, tc.wantSHA256Hex)
			}
		})
	}
}
