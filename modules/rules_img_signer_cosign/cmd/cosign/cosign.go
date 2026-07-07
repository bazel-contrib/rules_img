// Command cosign is a rules_img signer plugin that produces Sigstore signatures
// using sigstore-go. It implements the `sign-oci-artifact` protocol: it reads
// the subject descriptor from stdin and writes an OCI image layout tar (a
// Sigstore-bundle signature artifact) to stdout. It never contacts a container
// registry (it may contact Fulcio/Rekor and an RFC3161 timestamp authority,
// which are signing infrastructure).
//
// The artifact matches what `cosign sign --new-bundle-format` produces: a
// Sigstore bundle v0.3 whose content is a DSSE envelope over an in-toto
// Statement (subject = the signed image's manifest digest, predicate type
// https://sigstore.dev/cosign/sign/v1). That is the only new-bundle shape a
// released `cosign verify` can verify (cosign has no MessageSignature image
// verification path).
//
// By default it signs keyless: an ephemeral key certified by Fulcio via an OIDC
// identity token, with the signature recorded in the public Rekor transparency
// log. Pass --key to sign with a local key instead (ECDSA/RSA/ED25519, including
// encrypted cosign keys decrypted with $COSIGN_PASSWORD), and --tlog-upload=false
// to skip the transparency log. Flag names, defaults, and descriptions mirror
// the real `cosign sign` CLI wherever they apply to a registry-less signer.
package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	"github.com/sigstore/sigstore-go/pkg/sign"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	sigsig "github.com/sigstore/sigstore/pkg/signature"
	"github.com/sigstore/sigstore/pkg/signature/kms"
	"google.golang.org/protobuf/encoding/protojson"

	// KMS providers register themselves so that awskms://, gcpkms://,
	// azurekms://, and hashivault:// key references work with --key. Other
	// schemes fall back to the sigstore KMS plugin protocol (sigstore-kms-*).
	_ "github.com/sigstore/sigstore/pkg/signature/kms/aws"
	_ "github.com/sigstore/sigstore/pkg/signature/kms/azure"
	_ "github.com/sigstore/sigstore/pkg/signature/kms/gcp"
	_ "github.com/sigstore/sigstore/pkg/signature/kms/hashivault"

	"github.com/bazel-contrib/rules_img_signer_cosign/pkg/plugin"
	"github.com/bazel-contrib/rules_img_signer_cosign/pkg/signerapi"
)

const (
	bundleMediaType = "application/vnd.dev.sigstore.bundle.v0.3+json"
	// dsseIntotoPayloadType is the DSSE payload type for an in-toto Statement.
	dsseIntotoPayloadType = "application/vnd.in-toto+json"
	// inTotoStatementType is the in-toto attestation framework v1 Statement type.
	inTotoStatementType = "https://in-toto.io/Statement/v1"
	// cosignSignPredicateType is the predicate type cosign records for
	// `cosign sign --new-bundle-format`; using it lets `cosign verify` recognize
	// and accept the attestation.
	cosignSignPredicateType = "https://sigstore.dev/cosign/sign/v1"

	annotationCreated = "org.opencontainers.image.created"
	// annotationBundleContent / annotationBundlePredicateType mirror the OCI
	// referrer annotations cosign writes so it can identify the bundle content.
	annotationBundleContent       = "dev.sigstore.bundle.content"
	annotationBundlePredicateType = "dev.sigstore.bundle.predicateType"

	defaultFulcioURL = "https://fulcio.sigstore.dev"
	defaultRekorURL  = "https://rekor.sigstore.dev"
)

func main() {
	if err := plugin.Dispatch(context.Background(), os.Args[1:], newSigner); err != nil {
		fmt.Fprintln(os.Stderr, "cosign-plugin:", err)
		os.Exit(1)
	}
}

type cosignSigner struct {
	keypair                 sign.Keypair
	bundleOpts              sign.BundleOptions
	annotations             map[string]string
	recordCreationTimestamp bool
}

// stringSliceFlag is a repeatable string flag (like cosign's -a/--annotations).
type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func newSigner(args []string) (signerapi.OCIArtifactSigner, error) {
	fs := flag.NewFlagSet(plugin.Subcommand, flag.ContinueOnError)
	keyPath := fs.String("key", "", "Path to a PEM-encoded private key file (ECDSA, RSA, or ED25519), or a KMS URI (awskms://, gcpkms://, azurekms://, hashivault://) (or $RULES_IMG_COSIGN_KEY). Encrypted cosign/sigstore private keys are decrypted with $COSIGN_PASSWORD. If unset, sign keyless via Fulcio/OIDC.")
	idToken := fs.String("identity-token", "", "identity token to use for certificate from fulcio. the token or a path to a file containing the token is accepted (or $SIGSTORE_ID_TOKEN).")
	fulcioURL := fs.String("fulcio-url", defaultFulcioURL, "address of sigstore PKI server (or $SIGSTORE_FULCIO_URL). Used for keyless signing.")
	rekorURL := fs.String("rekor-url", defaultRekorURL, "address of rekor transparency log server (or $SIGSTORE_REKOR_URL).")
	tlogUpload := fs.Bool("tlog-upload", true, "whether to upload the signature to the Rekor transparency log (default true).")
	tsaURL := fs.String("timestamp-server-url", "", "URL of an RFC3161 timestamp authority. When set, a signed timestamp is obtained and embedded in the bundle.")
	certPath := fs.String("certificate", "", "path to the X.509 certificate in PEM format to include in the OCI signature (used with --key).")
	certChainPath := fs.String("certificate-chain", "", "path to a list of CA X.509 certificates in PEM format used to build the certificate chain for the signing certificate, ordered from the intermediate CA that issued the signing certificate towards (but not including) the root. Included in the OCI signature (used with --key).")
	signingAlg := fs.String("signing-algorithm", "", `signing algorithm for the ephemeral keyless key: one of ecdsa-p256, ecdsa-p384, ecdsa-p521, rsa-2048, rsa-3072, rsa-4096, ed25519 (default "ecdsa-p256").`)
	recordCreationTimestamp := fs.Bool("record-creation-timestamp", false, "set the org.opencontainers.image.created annotation on the signature artifact to the signing time. Off by default for reproducible signatures; honors $SOURCE_DATE_EPOCH.")
	var annotations stringSliceFlag
	fs.Var(&annotations, "annotations", "extra key=value pairs to sign (repeatable; recorded in the signed in-toto statement subject).")
	fs.Var(&annotations, "a", "extra key=value pairs to sign (shorthand for --annotations).")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	setFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	annotationMap, err := parseAnnotations(annotations)
	if err != nil {
		return nil, err
	}

	keyRef := envOr(*keyPath, "RULES_IMG_COSIGN_KEY")

	var (
		keypair sign.Keypair
		opts    sign.BundleOptions
	)
	if keyRef != "" {
		keypair, err = loadKeypair(keyRef)
		if err != nil {
			return nil, fmt.Errorf("loading signing key: %w", err)
		}
		// Optionally embed a caller-supplied certificate (chain) instead of a
		// bare public key, mirroring cosign's --certificate/--certificate-chain.
		certProvider, chainProvider, err := certificateProviders(*certPath, *certChainPath)
		if err != nil {
			return nil, err
		}
		switch {
		case chainProvider != nil:
			opts.CertificateChainProvider = chainProvider
		case certProvider != nil:
			opts.CertificateProvider = certProvider
		}
	} else {
		if *certPath != "" || *certChainPath != "" {
			return nil, fmt.Errorf("--certificate/--certificate-chain require --key (keyless signing obtains its certificate from Fulcio)")
		}
		alg, err := parseSigningAlgorithm(*signingAlg)
		if err != nil {
			return nil, err
		}
		keypair, err = sign.NewEphemeralKeypair(&sign.EphemeralKeypairOptions{Algorithm: alg})
		if err != nil {
			return nil, fmt.Errorf("generating ephemeral key: %w", err)
		}
		token, err := resolveIdentityToken(*idToken)
		if err != nil {
			return nil, err
		}
		if token == "" {
			return nil, fmt.Errorf("keyless signing requires an OIDC token via --identity-token or $SIGSTORE_ID_TOKEN (or pass --key for key-based signing)")
		}
		fulcio := *fulcioURL
		if !setFlags["fulcio-url"] {
			if e := os.Getenv("SIGSTORE_FULCIO_URL"); e != "" {
				fulcio = e
			}
		}
		opts.CertificateProvider = sign.NewFulcio(&sign.FulcioOptions{BaseURL: fulcio})
		opts.CertificateProviderOptions = &sign.CertificateProviderOptions{IDToken: token}
	}

	if *tsaURL != "" {
		opts.TimestampAuthorities = []*sign.TimestampAuthority{
			sign.NewTimestampAuthority(&sign.TimestampAuthorityOptions{URL: *tsaURL}),
		}
	}

	if *tlogUpload {
		rekor := *rekorURL
		if !setFlags["rekor-url"] {
			if e := os.Getenv("SIGSTORE_REKOR_URL"); e != "" {
				rekor = e
			}
		}
		opts.TransparencyLogs = []sign.Transparency{sign.NewRekor(&sign.RekorOptions{BaseURL: rekor})}
	}

	return &cosignSigner{
		keypair:                 keypair,
		bundleOpts:              opts,
		annotations:             annotationMap,
		recordCreationTimestamp: *recordCreationTimestamp,
	}, nil
}

func (s *cosignSigner) Sign(ctx context.Context, subject v1.Descriptor) (v1.Image, error) {
	statement, err := inTotoStatement(subject, s.annotations)
	if err != nil {
		return nil, err
	}

	opts := s.bundleOpts
	opts.Context = ctx
	// DSSE-sign the in-toto statement. The signature is over the DSSE PAE, so the
	// keypair's normal hash-then-sign is used (no pre-hashing), and Fulcio/Rekor
	// work unchanged.
	content := &sign.DSSEData{Data: statement, PayloadType: dsseIntotoPayloadType}
	bundle, err := sign.Bundle(content, s.keypair, opts)
	if err != nil {
		return nil, fmt.Errorf("creating sigstore bundle: %w", err)
	}
	bundleJSON, err := protojson.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("marshalling sigstore bundle: %w", err)
	}

	annotations := map[string]string{
		annotationBundleContent:       "dsse-envelope",
		annotationBundlePredicateType: cosignSignPredicateType,
	}
	// Reproducible by default: only stamp a creation time when explicitly asked,
	// matching cosign's --record-creation-timestamp (default false).
	if s.recordCreationTimestamp {
		ts, err := creationTimestamp()
		if err != nil {
			return nil, err
		}
		annotations[annotationCreated] = ts
	}
	return plugin.BuildArtifact(
		bundleMediaType,
		[]plugin.ArtifactLayer{{MediaType: bundleMediaType, Data: bundleJSON}},
		&subject,
		annotations,
	)
}

// inTotoStatement builds the in-toto Statement that cosign's new bundle format
// signs: the subject is the image manifest digest, tagged with cosign's sign
// predicate type. Signing this (as DSSE) is exactly what `cosign sign
// --new-bundle-format` does, so `cosign verify` accepts the result. Optional
// --annotations are recorded on the subject (matching cosign's -a behavior).
func inTotoStatement(subject v1.Descriptor, annotations map[string]string) ([]byte, error) {
	type resourceDescriptor struct {
		Digest      map[string]string `json:"digest"`
		Annotations map[string]string `json:"annotations,omitempty"`
	}
	statement := struct {
		Type          string               `json:"_type"`
		PredicateType string               `json:"predicateType"`
		Subject       []resourceDescriptor `json:"subject"`
		Predicate     struct{}             `json:"predicate"`
	}{
		Type:          inTotoStatementType,
		PredicateType: cosignSignPredicateType,
		Subject: []resourceDescriptor{{
			Digest:      map[string]string{subject.Digest.Algorithm: subject.Digest.Hex},
			Annotations: annotations,
		}},
	}
	return json.Marshal(statement)
}

// parseAnnotations turns repeated key=value flags into a map.
func parseAnnotations(kvs []string) (map[string]string, error) {
	if len(kvs) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		i := strings.Index(kv, "=")
		if i < 0 {
			return nil, fmt.Errorf("unable to parse annotation %q (want key=value)", kv)
		}
		m[kv[:i]] = kv[i+1:]
	}
	return m, nil
}

// parseSigningAlgorithm maps a cosign-style algorithm name to sigstore-go's
// PublicKeyDetails used for the ephemeral keyless key.
func parseSigningAlgorithm(name string) (protocommon.PublicKeyDetails, error) {
	switch name {
	case "", "ecdsa-p256":
		return protocommon.PublicKeyDetails_PKIX_ECDSA_P256_SHA_256, nil
	case "ecdsa-p384":
		return protocommon.PublicKeyDetails_PKIX_ECDSA_P384_SHA_384, nil
	case "ecdsa-p521":
		return protocommon.PublicKeyDetails_PKIX_ECDSA_P521_SHA_512, nil
	case "rsa-2048":
		return protocommon.PublicKeyDetails_PKIX_RSA_PKCS1V15_2048_SHA256, nil
	case "rsa-3072":
		return protocommon.PublicKeyDetails_PKIX_RSA_PKCS1V15_3072_SHA256, nil
	case "rsa-4096":
		return protocommon.PublicKeyDetails_PKIX_RSA_PKCS1V15_4096_SHA256, nil
	case "ed25519":
		return protocommon.PublicKeyDetails_PKIX_ED25519, nil
	default:
		return protocommon.PublicKeyDetails_PUBLIC_KEY_DETAILS_UNSPECIFIED, fmt.Errorf("unknown --signing-algorithm %q (want ecdsa-p256, ecdsa-p384, ecdsa-p521, rsa-2048, rsa-3072, rsa-4096, or ed25519)", name)
	}
}

// creationTimestamp returns the RFC3339 UTC timestamp to record, honoring
// SOURCE_DATE_EPOCH for reproducible builds.
func creationTimestamp() (string, error) {
	if sde := os.Getenv("SOURCE_DATE_EPOCH"); sde != "" {
		secs, err := strconv.ParseInt(sde, 10, 64)
		if err != nil {
			return "", fmt.Errorf("invalid SOURCE_DATE_EPOCH %q: %w", sde, err)
		}
		return time.Unix(secs, 0).UTC().Format(time.RFC3339), nil
	}
	return time.Now().UTC().Format(time.RFC3339), nil
}

// resolveIdentityToken returns the OIDC token from the flag or $SIGSTORE_ID_TOKEN.
// Like cosign, the value may be the token itself or a path to a file holding it.
func resolveIdentityToken(flagValue string) (string, error) {
	tok := envOr(flagValue, "SIGSTORE_ID_TOKEN")
	if tok == "" {
		return "", nil
	}
	if info, err := os.Stat(tok); err == nil && !info.IsDir() {
		data, err := os.ReadFile(tok)
		if err != nil {
			return "", fmt.Errorf("reading identity token file %q: %w", tok, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	return strings.TrimSpace(tok), nil
}

func envOr(flagValue, envName string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv(envName)
}

// loadKeypair loads a signing key into a sign.Keypair. keyRef is either a KMS
// URI (awskms://, gcpkms://, azurekms://, hashivault://, or a sigstore-kms-*
// plugin scheme) or a path to a PEM file. PEM files may hold plain
// PKCS#8/PKCS#1/EC keys or encrypted cosign/sigstore keys (decrypted with
// $COSIGN_PASSWORD), covering ECDSA, RSA, and ED25519.
func loadKeypair(keyRef string) (sign.Keypair, error) {
	if isKMSKeyRef(keyRef) {
		return loadKMSKeypair(keyRef)
	}
	data, err := os.ReadFile(keyRef)
	if err != nil {
		return nil, err
	}
	pf := func(bool) ([]byte, error) { return []byte(os.Getenv("COSIGN_PASSWORD")), nil }
	key, err := cryptoutils.UnmarshalPEMToPrivateKey(data, pf)
	if err != nil {
		return nil, fmt.Errorf("parsing private key in %s: %w", keyRef, err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("key in %s is not a crypto.Signer", keyRef)
	}
	return newPEMKeypair(signer)
}

// isKMSKeyRef reports whether keyRef is a KMS/plugin reference rather than a
// file path. Any scheme-qualified reference (containing "://") is treated as a
// KMS reference, matching cosign's handling of --key.
func isKMSKeyRef(keyRef string) bool {
	return strings.Contains(keyRef, "://")
}

// loadKMSKeypair resolves a KMS URI to a sign.Keypair. The built-in providers
// (aws/gcp/azure/hashivault) are registered via blank imports; any other scheme
// is delegated to a sigstore-kms-* plugin binary on PATH.
func loadKMSKeypair(keyRef string) (sign.Keypair, error) {
	if strings.HasPrefix(keyRef, "k8s://") {
		return nil, fmt.Errorf("--key %q: Kubernetes secret references are not supported by this plugin", keyRef)
	}
	ctx := context.Background()
	// The hash passed here is only the provider's default; pemKeypair derives the
	// actual hash from the key and passes it per Sign call.
	sv, err := kms.Get(ctx, keyRef, crypto.SHA256)
	if err != nil {
		return nil, fmt.Errorf("initializing KMS signer for %q: %w", keyRef, err)
	}
	signer, _, err := sv.CryptoSigner(ctx, func(err error) {
		fmt.Fprintln(os.Stderr, "cosign-plugin: kms:", err)
	})
	if err != nil {
		return nil, fmt.Errorf("obtaining KMS signer for %q: %w", keyRef, err)
	}
	return newPEMKeypair(signer)
}

// certificateProviders builds the sigstore-go providers for caller-supplied
// certificates. The leaf comes from --certificate (or the first cert in the
// chain); self-signed roots are stripped because sigstore-go rejects them in an
// embedded chain (the root is supplied by the verifier's trust root).
func certificateProviders(certPath, chainPath string) (sign.CertificateProvider, sign.CertificateChainProvider, error) {
	if certPath == "" && chainPath == "" {
		return nil, nil, nil
	}
	var ders [][]byte
	if certPath != "" {
		leaf, err := loadCertsDER(certPath)
		if err != nil {
			return nil, nil, fmt.Errorf("loading --certificate: %w", err)
		}
		ders = append(ders, leaf...)
	}
	if chainPath != "" {
		chain, err := loadCertsDER(chainPath)
		if err != nil {
			return nil, nil, fmt.Errorf("loading --certificate-chain: %w", err)
		}
		ders = append(ders, chain...)
	}
	if len(ders) == 0 {
		return nil, nil, fmt.Errorf("no certificates found in --certificate/--certificate-chain")
	}

	filtered := [][]byte{ders[0]} // leaf
	for _, der := range ders[1:] {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing certificate chain: %w", err)
		}
		if bytes.Equal(c.RawSubject, c.RawIssuer) {
			continue // self-signed root, not embedded
		}
		filtered = append(filtered, der)
	}
	if len(filtered) == 1 {
		return &staticCertProvider{der: filtered[0]}, nil, nil
	}
	return nil, &staticChainProvider{ders: filtered}, nil
}

// loadCertsDER reads all PEM CERTIFICATE blocks from a file as DER bytes.
func loadCertsDER(path string) ([][]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ders [][]byte
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		ders = append(ders, block.Bytes)
	}
	if len(ders) == 0 {
		return nil, fmt.Errorf("no PEM CERTIFICATE blocks in %s", path)
	}
	return ders, nil
}

// staticCertProvider serves a fixed leaf certificate (DER).
type staticCertProvider struct{ der []byte }

func (p *staticCertProvider) GetCertificate(context.Context, sign.Keypair, *sign.CertificateProviderOptions) ([]byte, error) {
	return p.der, nil
}

// staticChainProvider serves a fixed certificate chain (leaf first, DER).
type staticChainProvider struct{ ders [][]byte }

func (p *staticChainProvider) GetCertificateChain(context.Context, sign.Keypair, *sign.CertificateProviderOptions) ([][]byte, error) {
	return p.ders, nil
}

// pemKeypair adapts a loaded crypto.Signer to sign.Keypair, mirroring
// sigstore-go's EphemeralKeypair. ECDSA, RSA, and ED25519 keys are supported.
type pemKeypair struct {
	signer     crypto.Signer
	algDetails sigsig.AlgorithmDetails
	hint       []byte
}

func newPEMKeypair(signer crypto.Signer) (*pemKeypair, error) {
	algDetails, err := sigsig.GetDefaultAlgorithmDetails(signer.Public())
	if err != nil {
		return nil, fmt.Errorf("unsupported signing key: %w", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(pubDER)
	return &pemKeypair{
		signer:     signer,
		algDetails: algDetails,
		hint:       []byte(base64.StdEncoding.EncodeToString(sum[:])),
	}, nil
}

func (k *pemKeypair) GetHashAlgorithm() protocommon.HashAlgorithm {
	return k.algDetails.GetProtoHashType()
}

func (k *pemKeypair) GetSigningAlgorithm() protocommon.PublicKeyDetails {
	return k.algDetails.GetSignatureAlgorithm()
}

func (k *pemKeypair) GetHint() []byte { return k.hint }

func (k *pemKeypair) GetKeyAlgorithm() string {
	switch k.algDetails.GetKeyType() {
	case sigsig.ECDSA:
		return "ECDSA"
	case sigsig.RSA:
		return "RSA"
	case sigsig.ED25519:
		return "ED25519"
	default:
		return ""
	}
}

func (k *pemKeypair) GetPublicKey() crypto.PublicKey { return k.signer.Public() }

func (k *pemKeypair) GetPublicKeyPem() (string, error) {
	b, err := cryptoutils.MarshalPublicKeyToPEM(k.signer.Public())
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (k *pemKeypair) SignData(_ context.Context, data []byte) ([]byte, []byte, error) {
	hf := k.algDetails.GetHashType()
	dataToSign := data
	if hf != crypto.Hash(0) {
		hasher := hf.New()
		hasher.Write(data)
		dataToSign = hasher.Sum(nil)
	}
	sig, err := k.signer.Sign(nil, dataToSign, hf)
	if err != nil {
		return nil, nil, err
	}
	return sig, dataToSign, nil
}
