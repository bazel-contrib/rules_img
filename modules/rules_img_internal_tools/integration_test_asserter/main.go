// Command integration_test_asserter checks the state of an OCI registry after a
// `bazel run //:push` in a rules_img e2e workspace against a declarative spec
// (registry_assertions.json).
//
// It asserts structural properties (index vs single-arch, platforms, layer
// counts, config media type, annotations, labels), tag aliases (two tags
// resolving to the same digest), transitive-closure integrity (every referenced
// manifest/config/layer/referrer is present), and referrer expectations. For
// signing-capable tests it additionally confirms signature referrers on the
// per-signer registries and verifies them with the real cosign/notation CLIs.
//
// Images are named `<repository>:<tag>` in the spec — with no registry, since
// the registry is injected at test time and supplied here via --registry.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("integration_test_asserter", flag.ContinueOnError)
	specPath := fs.String("spec", "", "Path to registry_assertions.json (required).")
	mainReg := fs.String("registry", "", "host:port of the main registry (required).")
	cosignReg := fs.String("cosign-registry", "", "host:port of the cosign signature registry (optional).")
	notationReg := fs.String("notation-registry", "", "host:port of the notation signature registry (optional).")
	cosignCLI := fs.String("cosign-cli", "", "Path to the cosign binary for signature verification (optional).")
	notationCLI := fs.String("notation-cli", "", "Path to the notation binary for signature verification (optional).")
	cosignPubKey := fs.String("cosign-pubkey", "", "Path to the cosign public key PEM (optional).")
	notationCert := fs.String("notation-cert", "", "Path to the notation signing certificate PEM (optional).")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *specPath == "" || *mainReg == "" {
		fmt.Fprintln(os.Stderr, "integration_test_asserter: --spec and --registry are required")
		return 2
	}

	spec, err := loadSpec(*specPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration_test_asserter: %v\n", err)
		return 1
	}

	ck := &checker{c: newRegistryClient(*mainReg)}
	for _, img := range spec.Images {
		ck.checkImage(img)
	}

	sc := &signatureChecker{}
	if spec.Signatures != nil {
		// Signature checks run only when the corresponding signer registry is
		// provided. The runner asserts structure against the main registry first
		// (no signer flags — signatures skipped here), then re-runs with the
		// per-signer registries after the signed pushes to verify signatures.
		if s := spec.Signatures.Cosign; s != nil && *cosignReg != "" {
			sc.verifyCosign(newRegistryClient(*cosignReg), *cosignReg, s, *cosignCLI, *cosignPubKey)
		}
		if s := spec.Signatures.Notation; s != nil && *notationReg != "" {
			sc.verifyNotation(newRegistryClient(*notationReg), *notationReg, s, *notationCLI, *notationCert)
		}
	}

	errs := append(ck.errs, sc.errs...)
	if len(errs) > 0 {
		fmt.Fprintf(os.Stderr, "registry assertions FAILED (%d):\n", len(errs))
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "  - %s\n", e)
		}
		return 1
	}

	fmt.Printf("registry assertions passed: %d image(s) checked against %s\n", len(spec.Images), *mainReg)
	return 0
}
