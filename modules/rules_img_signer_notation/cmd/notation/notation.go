// Command notation is a rules_img signer plugin that produces Notary Project
// (Notation) signatures using notation-core-go. It implements the
// `sign-oci-artifact` protocol: it reads the subject descriptor from stdin and
// writes an OCI image layout tar (the signature artifact) to stdout. It never
// contacts a container registry.
//
// It signs with a local PEM private key and X.509 certificate chain, matching
// the common `notation sign` key-based flow. Key material comes from flags or
// the environment; Bazel never sees it.
//
// The flags mirror the real `notation sign` command wherever a flag shapes the
// signature envelope and is meaningful for a config-less, registry-less signer:
// --signature-format, --expiry/-e, --user-metadata/-m, --timestamp-url and
// --timestamp-root-cert use notation's names, shorthands and descriptions
// verbatim. Registry-auth, notation-config, referrers-storage and
// plugin-delegation flags (--username, --plugin, --force-referrers-tag,
// --oci-layout, ...) are intentionally omitted because this plugin never talks
// to a registry and keeps no notation config directory. See README.md.
package main

import (
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/notaryproject/notation-core-go/revocation"
	"github.com/notaryproject/notation-core-go/revocation/purpose"
	"github.com/notaryproject/notation-core-go/signature"
	"github.com/notaryproject/notation-core-go/signature/cose"
	"github.com/notaryproject/notation-core-go/signature/jws"
	"github.com/notaryproject/tspclient-go"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img_signer_notation/pkg/plugin"
	"github.com/bazel-contrib/rules_img_signer_notation/pkg/signerapi"
)

const (
	artifactTypeNotation = "application/vnd.cncf.notary.signature"
	payloadContentType   = "application/vnd.cncf.notary.payload.v1+json"
	annotationThumbprint = "io.cncf.notary.x509chain.thumbprint#S256"
	annotationCreated    = "org.opencontainers.image.created"
	signingAgent         = "rules_img-notation-plugin"

	// reservedAnnotationPrefix is the annotation-key prefix reserved by the
	// Notary Project; user metadata keys may not use it. Mirrors notation-go.
	reservedAnnotationPrefix = "io.cncf.notary"
)

func main() {
	if err := plugin.Dispatch(context.Background(), os.Args[1:], newSigner); err != nil {
		fmt.Fprintln(os.Stderr, "notation-plugin:", err)
		os.Exit(1)
	}
}

type notationSigner struct {
	signer                 signature.LocalSigner
	certs                  []*x509.Certificate
	envelopeType           string
	expiry                 time.Duration
	userMetadata           map[string]string
	timestamper            tspclient.Timestamper // nil unless --timestamp-url is set
	tsaRootCAs             *x509.CertPool        // nil unless --timestamp-url is set
	tsaRevocationValidator revocation.Validator  // nil unless --timestamp-url is set
}

// stringSlice is a repeatable string flag value (accumulates one entry per
// occurrence), mirroring notation's repeatable --user-metadata/-m flag.
type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

func newSigner(args []string) (signerapi.OCIArtifactSigner, error) {
	fs := flag.NewFlagSet(plugin.Subcommand, flag.ContinueOnError)
	keyPath := fs.String("key", "", "Path to a PEM-encoded private key (or $RULES_IMG_NOTATION_KEY). Unlike `notation sign --key`, this is a filesystem path to key material, not a named key from notation's key list.")
	certPath := fs.String("certificate-chain", "", "Path to a PEM-encoded X.509 certificate chain, leaf first (or $RULES_IMG_NOTATION_CERTIFICATE_CHAIN).")
	format := fs.String("signature-format", "jws", `signature envelope format, options: "jws", "cose"`)

	var expiry time.Duration
	const expiryUsage = `optional expiry that provides a "best by use" time for the artifact. The duration is specified in minutes(m) and/or hours(h). For example: 12h, 30m, 3h20m`
	fs.DurationVar(&expiry, "expiry", 0, expiryUsage)
	fs.DurationVar(&expiry, "e", 0, "shorthand for --expiry")

	var userMeta stringSlice
	const userMetaUsage = `{key}={value} pairs that are added to the signature payload`
	fs.Var(&userMeta, "user-metadata", userMetaUsage)
	fs.Var(&userMeta, "m", "shorthand for --user-metadata")

	tsaURL := fs.String("timestamp-url", "", "RFC 3161 Timestamping Authority (TSA) server URL")
	tsaRootCert := fs.String("timestamp-root-cert", "", "filepath of timestamp authority root certificate")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	keyFile := envOr(*keyPath, "RULES_IMG_NOTATION_KEY", "NOTATION_KEY")
	certFile := envOr(*certPath, "RULES_IMG_NOTATION_CERTIFICATE_CHAIN", "NOTATION_CERTIFICATE_CHAIN")
	if keyFile == "" || certFile == "" {
		return nil, fmt.Errorf("both --key and --certificate-chain (or $RULES_IMG_NOTATION_KEY/$RULES_IMG_NOTATION_CERTIFICATE_CHAIN) are required")
	}

	key, err := loadPrivateKey(keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading private key: %w", err)
	}
	certs, err := loadCertChain(certFile)
	if err != nil {
		return nil, fmt.Errorf("loading certificate chain: %w", err)
	}
	localSigner, err := signature.NewLocalSigner(certs, key)
	if err != nil {
		return nil, fmt.Errorf("creating signer: %w", err)
	}

	var envelopeType string
	switch *format {
	case "jws":
		envelopeType = jws.MediaTypeEnvelope
	case "cose":
		envelopeType = cose.MediaTypeEnvelope
	default:
		return nil, fmt.Errorf("unknown --signature-format %q (options: \"jws\", \"cose\")", *format)
	}

	// notation validates expiry as non-negative with second granularity.
	if expiry < 0 {
		return nil, fmt.Errorf("expiry duration cannot be a negative value")
	}
	if expiry%time.Second != 0 {
		return nil, fmt.Errorf("expiry duration supports minimum granularity of seconds")
	}

	userMetadata, err := parseUserMetadata(userMeta)
	if err != nil {
		return nil, err
	}

	timestamper, tsaRootCAs, tsaRevocationValidator, err := setupTimestamping(*tsaURL, *tsaRootCert)
	if err != nil {
		return nil, err
	}

	return &notationSigner{
		signer:                 localSigner,
		certs:                  certs,
		envelopeType:           envelopeType,
		expiry:                 expiry,
		userMetadata:           userMetadata,
		timestamper:            timestamper,
		tsaRootCAs:             tsaRootCAs,
		tsaRevocationValidator: tsaRevocationValidator,
	}, nil
}

// parseUserMetadata turns repeated {key}={value} flag entries into a map,
// applying notation's rules: each entry needs a non-empty key and value
// separated by "=", keys may not use the reserved io.cncf.notary prefix, and a
// key may not be given twice.
func parseUserMetadata(entries stringSlice) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	metadata := make(map[string]string, len(entries))
	for _, entry := range entries {
		k, v, ok := strings.Cut(entry, "=")
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("could not parse --user-metadata %q: key-value pair requires \"=\" as separator", entry)
		}
		if strings.HasPrefix(k, reservedAnnotationPrefix) {
			return nil, fmt.Errorf("error adding user metadata: metadata key %v has reserved prefix %v", k, reservedAnnotationPrefix)
		}
		if _, dup := metadata[k]; dup {
			return nil, fmt.Errorf("error adding user metadata: metadata key %v specified more than once", k)
		}
		metadata[k] = v
	}
	return metadata, nil
}

// setupTimestamping builds an RFC 3161 timestamper, TSA root cert pool, and
// TSA-chain revocation validator from the --timestamp-url / --timestamp-root-cert
// flags. The notation CLI marks these flags required-together
// (MarkFlagsRequiredTogether), and notation-go's signer rejects a Timestamper
// without TSARootCAs (and vice versa), so this enforces both-or-neither. Like
// `notation sign`, it also configures a timestamping-purpose revocation
// validator so a revoked TSA certificate is rejected at signing time.
func setupTimestamping(tsaURL, tsaRootCertPath string) (tspclient.Timestamper, *x509.CertPool, revocation.Validator, error) {
	if (tsaURL == "") != (tsaRootCertPath == "") {
		return nil, nil, nil, fmt.Errorf("--timestamp-url and --timestamp-root-cert must be set together")
	}
	if tsaURL == "" {
		return nil, nil, nil, nil
	}
	timestamper, err := tspclient.NewHTTPTimestamper(nil, tsaURL)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("creating timestamper for %q: %w", tsaURL, err)
	}
	data, err := os.ReadFile(tsaRootCertPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reading timestamp root certificate: %w", err)
	}
	tsaRootCAs := x509.NewCertPool()
	if !tsaRootCAs.AppendCertsFromPEM(data) {
		return nil, nil, nil, fmt.Errorf("no certificates found in timestamp root certificate %s", tsaRootCertPath)
	}
	revocationValidator, err := revocation.NewWithOptions(revocation.Options{CertChainPurpose: purpose.Timestamping})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("creating timestamp revocation validator: %w", err)
	}
	return timestamper, tsaRootCAs, revocationValidator, nil
}

func (s *notationSigner) Sign(ctx context.Context, subject v1.Descriptor) (v1.Image, error) {
	// The signed payload's targetArtifact carries any annotations already on the
	// subject plus the --user-metadata pairs, matching notation-go's
	// addUserMetadataToDescriptor + SanitizeTargetArtifact behavior.
	targetAnnotations := map[string]string{}
	for k, v := range subject.Annotations {
		targetAnnotations[k] = v
	}
	for k, v := range s.userMetadata {
		if _, ok := targetAnnotations[k]; ok {
			return nil, fmt.Errorf("error adding user metadata: metadata key %v is already present in the target artifact", k)
		}
		targetAnnotations[k] = v
	}
	if len(targetAnnotations) == 0 {
		targetAnnotations = nil
	}

	payloadDesc := ocispec.Descriptor{
		MediaType:   string(subject.MediaType),
		Digest:      digest.Digest(subject.Digest.String()),
		Size:        subject.Size,
		Annotations: targetAnnotations,
	}
	payloadBytes, err := json.Marshal(struct {
		TargetArtifact ocispec.Descriptor `json:"targetArtifact"`
	}{payloadDesc})
	if err != nil {
		return nil, fmt.Errorf("marshalling notary payload: %w", err)
	}

	env, err := signature.NewEnvelope(s.envelopeType)
	if err != nil {
		return nil, fmt.Errorf("creating %s envelope: %w", s.envelopeType, err)
	}

	now := time.Now()
	req := &signature.SignRequest{
		Payload:       signature.Payload{ContentType: payloadContentType, Content: payloadBytes},
		Signer:        s.signer,
		SigningTime:   now,
		SigningScheme: signature.SigningSchemeX509,
		SigningAgent:  signingAgent,
	}
	if s.expiry > 0 {
		req.Expiry = now.Add(s.expiry)
	}
	if s.timestamper != nil {
		req.Timestamper = s.timestamper
		req.TSARootCAs = s.tsaRootCAs
		req.TSARevocationValidator = s.tsaRevocationValidator
	}
	envelopeBytes, err := env.Sign(req.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("signing notary envelope: %w", err)
	}

	annotations := map[string]string{
		annotationThumbprint: thumbprints(s.certs),
		annotationCreated:    now.UTC().Format(time.RFC3339),
	}
	return plugin.BuildArtifact(
		artifactTypeNotation,
		[]plugin.ArtifactLayer{{MediaType: s.envelopeType, Data: envelopeBytes}},
		&subject,
		annotations,
	)
}

// thumbprints returns the JSON array of lowercase-hex SHA-256 thumbprints of the
// certificate chain (leaf first), the value of the required
// io.cncf.notary.x509chain.thumbprint#S256 annotation.
func thumbprints(certs []*x509.Certificate) string {
	tps := make([]string, len(certs))
	for i, c := range certs {
		sum := sha256.Sum256(c.Raw)
		tps[i] = hex.EncodeToString(sum[:])
	}
	b, _ := json.Marshal(tps)
	return string(b)
}

// envOr returns flagValue if non-empty, otherwise the first non-empty value
// among the named environment variables (in order). Later names act as
// deprecated fallbacks for earlier ones.
func envOr(flagValue string, envNames ...string) string {
	if flagValue != "" {
		return flagValue
	}
	for _, name := range envNames {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}

func loadPrivateKey(path string) (crypto.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in %s", path)
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("unsupported private key format in %s", path)
}

func loadCertChain(path string) ([]*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var certs []*x509.Certificate
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("no certificates found in %s", path)
	}
	return certs, nil
}
