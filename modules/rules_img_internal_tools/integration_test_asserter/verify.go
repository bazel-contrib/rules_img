package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	// cosignBundleArtifactType is the artifactType of the referrer the cosign
	// signer plugin attaches (a Sigstore bundle v0.3 carrying a DSSE in-toto
	// attestation).
	cosignBundleArtifactType = "application/vnd.dev.sigstore.bundle.v0.3+json"
	// cosignSignPredicateType is the in-toto predicate type the cosign plugin
	// records; cosign verify-attestation must be told to expect it.
	cosignSignPredicateType = "https://sigstore.dev/cosign/sign/v1"
	// notationSignatureArtifactType is the artifactType of a Notary Project
	// signature manifest.
	notationSignatureArtifactType = "application/vnd.cncf.notary.signature"
)

// notationTrustPolicyDoc is the trustpolicy.oci.json document.
type notationTrustPolicyDoc struct {
	Version       string            `json:"version"`
	TrustPolicies []json.RawMessage `json:"trustPolicies"`
}

// notationTrustPolicy is a single OCI trust policy entry.
type notationTrustPolicy struct {
	Name                  string   `json:"name"`
	RegistryScopes        []string `json:"registryScopes"`
	SignatureVerification struct {
		Level string `json:"level"`
	} `json:"signatureVerification"`
	TrustStores       []string `json:"trustStores"`
	TrustedIdentities []string `json:"trustedIdentities"`
}

// signatureChecker verifies signature referrers on the per-signer registries and
// then confirms them with the real upstream CLIs.
type signatureChecker struct {
	errs []string
}

func (sc *signatureChecker) errf(format string, args ...any) {
	sc.errs = append(sc.errs, fmt.Sprintf(format, args...))
}

// requireSignatureReferrer confirms a signature referrer with the expected
// artifactType is attached to repo:tag and returns the resolved subject digest.
func requireSignatureReferrer(reg *registryClient, repo, tag, artifactType string) (string, int, error) {
	digest, err := reg.headDigest(repo, tag)
	if err != nil {
		return "", 0, fmt.Errorf("resolving %s:%s: %w", repo, tag, err)
	}
	idx, err := reg.referrers(repo, digest)
	if err != nil {
		return digest, 0, fmt.Errorf("listing referrers of %s@%s: %w", repo, digest, err)
	}
	count := 0
	for _, d := range idx.Manifests {
		if reg.effectiveArtifactType(repo, d) == artifactType {
			count++
		}
	}
	return digest, count, nil
}

// verifyCosign confirms the cosign signature referrer exists and, when a CLI and
// public key are available, verifies it with the real cosign binary.
func (sc *signatureChecker) verifyCosign(reg *registryClient, hostPort string, a *SignatureAssertion, cosignCLI, pubKey string) {
	artifactType := a.ArtifactType
	if artifactType == "" {
		artifactType = cosignBundleArtifactType
	}
	_, count, err := requireSignatureReferrer(reg, a.Repository, a.Tag, artifactType)
	if err != nil {
		sc.errf("cosign: %v", err)
		return
	}
	if count == 0 {
		sc.errf("cosign: no signature referrer with artifact_type %q on %s:%s", artifactType, a.Repository, a.Tag)
		return
	}

	if cosignCLI == "" || pubKey == "" {
		sc.errf("cosign: signature referrer present but cannot verify (missing --cosign-cli/--cosign-pubkey)")
		return
	}

	// Keep cosign's TUF cache out of $HOME (belt-and-suspenders; key + ignore-tlog
	// avoids any TUF/Rekor access anyway).
	tuf, err := os.MkdirTemp("", "cosign-tuf-")
	if err != nil {
		sc.errf("cosign: temp dir: %v", err)
		return
	}
	defer os.RemoveAll(tuf)

	ref := fmt.Sprintf("%s/%s:%s", hostPort, a.Repository, a.Tag)
	cmd := exec.Command(cosignCLI, "verify-attestation",
		"--key", pubKey,
		"--type", cosignSignPredicateType,
		"--insecure-ignore-tlog",
		"--insecure-ignore-sct",
		"--allow-http-registry",
		"--allow-insecure-registry",
		ref,
	)
	cmd.Env = append(os.Environ(), "TUF_ROOT="+tuf)
	if out, err := cmd.CombinedOutput(); err != nil {
		sc.errf("cosign verify-attestation failed for %s: %v\n%s", ref, err, indent(out))
	}
}

// verifyNotation confirms the notation signature referrer exists and, when a CLI
// and certificate are available, verifies it with the real notation binary. It
// writes a throwaway trust store + trust policy into notation's config dir rooted
// at a temporary $HOME, scoped to exactly the test registry.
func (sc *signatureChecker) verifyNotation(reg *registryClient, hostPort string, a *SignatureAssertion, notationCLI, certPath string) {
	artifactType := a.ArtifactType
	if artifactType == "" {
		artifactType = notationSignatureArtifactType
	}
	_, count, err := requireSignatureReferrer(reg, a.Repository, a.Tag, artifactType)
	if err != nil {
		sc.errf("notation: %v", err)
		return
	}
	if count == 0 {
		sc.errf("notation: no signature referrer with artifact_type %q on %s:%s", artifactType, a.Repository, a.Tag)
		return
	}

	if notationCLI == "" || certPath == "" {
		sc.errf("notation: signature referrer present but cannot verify (missing --notation-cli/--notation-cert)")
		return
	}

	// Scope the trust policy to exactly the repository we push to during testing
	// (host:port/repository), so it can never affect any other image.
	scope := fmt.Sprintf("%s/%s", hostPort, a.Repository)
	home, err := setupNotationHome(certPath, scope)
	if err != nil {
		sc.errf("notation: %v", err)
		return
	}
	defer os.RemoveAll(home)

	ref := fmt.Sprintf("%s/%s:%s", hostPort, a.Repository, a.Tag)
	cmd := exec.Command(notationCLI, "verify", "--insecure-registry", ref)
	// notation resolves its config dir as filepath.Join(os.UserConfigDir(),
	// "notation") — there is no config-path env override, and the developer's real
	// config dir is not writable under the bazel sandbox — so redirect the env var
	// os.UserConfigDir() reads to our throwaway root. That var differs per OS:
	// $HOME on darwin, $XDG_CONFIG_HOME/$HOME on unix, and %AppData% on Windows
	// (where $HOME is ignored). notationEnv sets all of them so it works uniformly.
	cmd.Env = notationEnv(os.Environ(), home)
	if out, err := cmd.CombinedOutput(); err != nil {
		sc.errf("notation verify failed for %s: %v\n%s", ref, err, indent(out))
	}
}

// setupNotationHome creates a temporary directory whose platform-specific
// notation config dir contains the trust store certificate and a permissive
// trust policy scoped to the given registry scope (host:port/repository). It
// returns the root; the caller removes it. The verification level is permissive:
// authenticity and integrity are enforced (a forged/mismatched signature fails),
// while expiry and revocation — which a throwaway self-signed test cert cannot
// satisfy — are only logged.
func setupNotationHome(certPath, scope string) (string, error) {
	home, err := os.MkdirTemp("", "notation-home-")
	if err != nil {
		return "", fmt.Errorf("temp dir: %w", err)
	}
	cfg := notationConfigDir(home)
	storeDir := filepath.Join(cfg, "truststore", "x509", "ca", "e2e")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		os.RemoveAll(home)
		return "", err
	}
	cert, err := os.ReadFile(certPath)
	if err != nil {
		os.RemoveAll(home)
		return "", fmt.Errorf("reading cert %s: %w", certPath, err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "cert.pem"), cert, 0o644); err != nil {
		os.RemoveAll(home)
		return "", err
	}

	policy := notationTrustPolicy{
		Name:              "e2e",
		RegistryScopes:    []string{scope},
		TrustStores:       []string{"ca:e2e"},
		TrustedIdentities: []string{"*"},
	}
	policy.SignatureVerification.Level = "permissive"
	doc := notationTrustPolicyDoc{Version: "1.0", TrustPolicies: []json.RawMessage{mustJSON(policy)}}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		os.RemoveAll(home)
		return "", err
	}
	// Write both the OCI trust policy (trustpolicy.oci.json, used by newer
	// notation) and the legacy trustpolicy.json — notation-go v1.3.2's
	// trustpolicy.LoadDocument() reads the legacy path (dir.PathTrustPolicy).
	for _, name := range []string{"trustpolicy.oci.json", "trustpolicy.json"} {
		if err := os.WriteFile(filepath.Join(cfg, name), body, 0o644); err != nil {
			os.RemoveAll(home)
			return "", err
		}
	}
	return home, nil
}

// notationConfigDir mirrors filepath.Join(os.UserConfigDir(), "notation") for
// the given root, per platform, assuming notationEnv has pointed the relevant
// env var at root. On Windows os.UserConfigDir() is %AppData% (which we set to
// root), so the config dir is root/notation; on darwin/unix it nests under root
// the way os.UserConfigDir() derives from $HOME.
func notationConfigDir(root string) string {
	switch runtime.GOOS {
	case "windows":
		// os.UserConfigDir() == %AppData%; notationEnv sets APPDATA=root.
		return filepath.Join(root, "notation")
	case "darwin":
		return filepath.Join(root, "Library", "Application Support", "notation")
	default: // linux and other unix
		return filepath.Join(root, ".config", "notation")
	}
}

// notationEnv returns env adjusted so the notation CLI resolves os.UserConfigDir()
// to root. os.UserConfigDir() reads a different variable per OS — %AppData% on
// Windows, $HOME on darwin, $XDG_CONFIG_HOME/$HOME on unix — so we drop any
// pre-existing values of all of them and set HOME and APPDATA to root (each is
// ignored on the platforms that don't use it). Dropping XDG_CONFIG_HOME forces
// unix to fall back to $HOME/.config.
func notationEnv(env []string, root string) []string {
	out := make([]string, 0, len(env)+2)
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		// Windows environment variable names are case-insensitive.
		if strings.EqualFold(key, "HOME") ||
			strings.EqualFold(key, "APPDATA") ||
			strings.EqualFold(key, "XDG_CONFIG_HOME") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "HOME="+root, "APPDATA="+root)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func indent(b []byte) string {
	lines := ""
	for _, line := range splitLines(string(b)) {
		lines += "    " + line + "\n"
	}
	return lines
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
