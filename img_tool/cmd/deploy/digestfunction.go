package deploy

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// envDigestFunction names the digest function the remote cache addresses blobs
// by. It mirrors Bazel's --digest_function startup option, and is also recorded
// in the deploy manifest (see [api.ToolSettings]).
const envDigestFunction = "IMG_DIGEST_FUNCTION"

const (
	digestFunctionSHA256 = "sha256"
	digestFunctionBLAKE3 = "blake3"
	// digestFunctionAuto asks the remote cache which digest functions it supports.
	digestFunctionAuto = "auto"
)

// capabilitiesTimeout bounds asking the remote cache for its capabilities. Not
// getting an answer is not an error -- the lookups then fall back to SHA256 --
// so it must not hold up a push for long.
const capabilitiesTimeout = 10 * time.Second

// digestFunctionSettingFromEnv returns the configured digest function setting:
// "sha256", "blake3", or "auto" (the default). Anything else is an error rather
// than a quiet fallback, because a typo here would only show up as layers
// missing from the cache.
func digestFunctionSettingFromEnv() (string, error) {
	setting := strings.ToLower(strings.TrimSpace(os.Getenv(envDigestFunction)))
	switch setting {
	case "", digestFunctionAuto:
		return digestFunctionAuto, nil
	case digestFunctionSHA256, digestFunctionBLAKE3:
		return setting, nil
	}
	return "", fmt.Errorf("unsupported %s %q: want %q, %q or %q", envDigestFunction, setting, digestFunctionSHA256, digestFunctionBLAKE3, digestFunctionAuto)
}

// resolveDigestFunctions decides which digest functions to look blobs up in the
// remote cache by, in the order to try them.
//
// Registries only ever see SHA256: the manifests, configs and layer descriptors
// of a push are never affected by this. It only decides how the remote cache is
// asked for a layer it holds.
//
//   - An explicit setting ("sha256" or "blake3", mirroring Bazel's
//     --digest_function) is used as given, without asking the cache anything.
//   - "auto" asks the cache's capabilities (supported). A cache that supports
//     only BLAKE3 is looked up by BLAKE3. One that supports both is tried as
//     SHA256 first and BLAKE3 second, because nothing says which of the two Bazel
//     stored its blobs under, and a layer is only ever looked up under a function
//     its digest in is known.
//   - Anything else -- the cache does not answer, supports only functions that
//     cannot be used -- is SHA256, which is what a push did before this setting
//     existed.
func resolveDigestFunctions(ctx context.Context, setting string, supported func(context.Context) ([]string, error), warn io.Writer) []string {
	if setting == digestFunctionSHA256 || setting == digestFunctionBLAKE3 {
		return []string{setting}
	}
	ctx, cancel := context.WithTimeout(ctx, capabilitiesTimeout)
	defer cancel()
	functions, err := supported(ctx)
	if err != nil {
		// A cache that predates the capabilities API is not worth a warning.
		if status.Code(err) != codes.Unimplemented {
			fmt.Fprintf(warn, "WARNING: could not ask the remote cache which digest function it uses, assuming %s: %v\n", digestFunctionSHA256, err)
		}
		return []string{digestFunctionSHA256}
	}
	hasSHA256 := slices.Contains(functions, digestFunctionSHA256)
	hasBLAKE3 := slices.Contains(functions, digestFunctionBLAKE3)
	switch {
	case hasSHA256 && hasBLAKE3:
		return []string{digestFunctionSHA256, digestFunctionBLAKE3}
	case hasBLAKE3:
		return []string{digestFunctionBLAKE3}
	}
	return []string{digestFunctionSHA256}
}
