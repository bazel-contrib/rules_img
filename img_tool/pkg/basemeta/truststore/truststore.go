// Package truststore collects X.509 CA certificates from heterogeneous inputs
// and renders them in the layouts a container base image needs: a concatenated
// PEM bundle, an exploded /etc/ssl/certs directory with OpenSSL subject-hash
// links, and a PKCS#12 truststore for the JVM.
//
// Everything here is pure Go on the standard library. Parsing is deliberately
// strict: an input that cannot be understood is an error rather than something
// quietly dropped, because a base image silently missing a CA fails much later
// and much more confusingly.
package truststore

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"sort"
	"strings"
)

// Certificate is one collected CA certificate together with where it came
// from, so errors and aliases can name a real file.
type Certificate struct {
	// Parsed is the decoded certificate.
	Parsed *x509.Certificate
	// DER is the raw encoding, as it will be written back out.
	DER []byte
	// Origin describes the input the certificate was read from, e.g.
	// "certs/root.pem" or "ca-certificates.deb:usr/share/ca-certificates/x.crt".
	Origin string
}

// Collection accumulates certificates, discarding exact duplicates.
type Collection struct {
	certs []Certificate
	seen  map[[sha256.Size]byte]string
}

// NewCollection returns an empty Collection.
func NewCollection() *Collection {
	return &Collection{seen: make(map[[sha256.Size]byte]string)}
}

// AddDER adds a single DER-encoded certificate. A certificate already in the
// collection is ignored: the same root legitimately arrives from several
// sources (a raw file and a distribution bundle, say).
func (c *Collection) AddDER(der []byte, origin string) error {
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return fmt.Errorf("%s: parsing certificate: %w", origin, err)
	}
	fingerprint := sha256.Sum256(der)
	if _, duplicate := c.seen[fingerprint]; duplicate {
		return nil
	}
	c.seen[fingerprint] = origin
	c.certs = append(c.certs, Certificate{Parsed: parsed, DER: der, Origin: origin})
	return nil
}

// AddBytes adds every certificate found in data, which may be PEM (with any
// number of certificates, and any number of non-certificate blocks to skip),
// a bare DER certificate, or a DER PKCS#7 / degenerate CMS bundle.
func (c *Collection) AddBytes(data []byte, origin string) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return fmt.Errorf("%s: file is empty", origin)
	}

	if n, err := c.addPEM(data, origin); err != nil {
		return err
	} else if n > 0 {
		return nil
	}

	// Not PEM: try a bare DER certificate, then a PKCS#7 bundle. Both are
	// plausible contents of a ".crt" file, and neither is distinguishable from
	// the other without parsing.
	if err := c.AddDER(data, origin); err == nil {
		return nil
	}
	if n, err := c.addPKCS7(data, origin); err == nil && n > 0 {
		return nil
	}

	return fmt.Errorf("%s: not a PEM, DER, or PKCS#7 certificate file", origin)
}

// addPEM adds every CERTIFICATE block in data and returns how many it found.
// Other block types (a CRL or a key sitting in the same file) are skipped.
func (c *Collection) addPEM(data []byte, origin string) (int, error) {
	count := 0
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if err := c.AddDER(block.Bytes, origin); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// contentInfo is the outer PKCS#7 structure (RFC 2315 section 7).
type contentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     asn1.RawValue `asn1:"explicit,optional,tag:0"`
}

// signedData is the degenerate "certificates-only" form of PKCS#7 SignedData,
// which is how certificate bundles are distributed. Only the certificate list
// is of interest, so the signer fields are left as raw values.
type signedData struct {
	Version          int
	DigestAlgorithms asn1.RawValue `asn1:"set"`
	ContentInfo      asn1.RawValue
	Certificates     asn1.RawValue `asn1:"optional,tag:0"`
}

var oidSignedData = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}

// addPKCS7 adds every certificate in a DER PKCS#7 bundle.
func (c *Collection) addPKCS7(data []byte, origin string) (int, error) {
	var info contentInfo
	if _, err := asn1.Unmarshal(data, &info); err != nil {
		return 0, fmt.Errorf("%s: not a PKCS#7 structure: %w", origin, err)
	}
	if !info.ContentType.Equal(oidSignedData) {
		return 0, fmt.Errorf("%s: PKCS#7 content type %s is not signedData", origin, info.ContentType)
	}

	var signed signedData
	if _, err := asn1.Unmarshal(info.Content.Bytes, &signed); err != nil {
		return 0, fmt.Errorf("%s: parsing PKCS#7 signedData: %w", origin, err)
	}
	if len(signed.Certificates.Bytes) == 0 {
		return 0, fmt.Errorf("%s: PKCS#7 bundle contains no certificates", origin)
	}

	// x509.ParseCertificates walks a concatenation of DER certificates, which is
	// exactly the content of the [0] IMPLICIT certificates field.
	parsed, err := x509.ParseCertificates(signed.Certificates.Bytes)
	if err != nil {
		return 0, fmt.Errorf("%s: parsing PKCS#7 certificates: %w", origin, err)
	}
	for _, cert := range parsed {
		if err := c.AddDER(cert.Raw, origin); err != nil {
			return 0, err
		}
	}
	return len(parsed), nil
}

// Certificates returns the collected certificates in a stable order: sorted by
// the SHA-256 of their DER encoding, so the output does not depend on the order
// Bazel happened to pass the inputs in.
func (c *Collection) Certificates() []Certificate {
	sorted := make([]Certificate, len(c.certs))
	copy(sorted, c.certs)
	sort.Slice(sorted, func(i, j int) bool {
		a := sha256.Sum256(sorted[i].DER)
		b := sha256.Sum256(sorted[j].DER)
		return bytes.Compare(a[:], b[:]) < 0
	})
	return sorted
}

// Len reports how many distinct certificates were collected.
func (c *Collection) Len() int { return len(c.certs) }

// Bundle renders the certificates as one concatenated PEM file, the format
// SSL_CERT_FILE and most TLS libraries expect.
func Bundle(certs []Certificate) ([]byte, error) {
	var buf bytes.Buffer
	for _, cert := range certs {
		// A comment line naming the subject makes the bundle greppable, and is
		// what ca-certificates itself does.
		fmt.Fprintf(&buf, "# %s\n", subjectLine(cert.Parsed))
		if err := pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: cert.DER}); err != nil {
			return nil, fmt.Errorf("encoding certificate %s: %w", cert.Origin, err)
		}
	}
	return buf.Bytes(), nil
}

// ExplodedEntry is one file of the exploded /etc/ssl/certs layout.
type ExplodedEntry struct {
	// Name is the file name within the certificate directory.
	Name string
	// PEM is the file content, or nil when the entry is a symlink.
	PEM []byte
	// LinkTarget is the name this entry links to, or "" for a real file.
	LinkTarget string
}

// Exploded renders the certificates as OpenSSL's hashed certificate directory:
// one PEM file per certificate, plus the <subject_hash>.<n> symlinks
// SSL_CERT_DIR lookups rely on.
//
// The real files are named after the subject hash too (rather than after the
// input file) so the layout is reproducible and independent of how the
// certificates were sourced.
func Exploded(certs []Certificate) ([]ExplodedEntry, error) {
	var entries []ExplodedEntry
	// OpenSSL disambiguates certificates that share a subject hash with an
	// increasing suffix, so count collisions as we go.
	counts := make(map[string]int)
	for _, cert := range certs {
		hash := SubjectHash(cert.Parsed)
		sequence := counts[hash]
		counts[hash] = sequence + 1

		fileName := fmt.Sprintf("%s.pem", certFileName(cert))
		var buf bytes.Buffer
		fmt.Fprintf(&buf, "# %s\n", subjectLine(cert.Parsed))
		if err := pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: cert.DER}); err != nil {
			return nil, fmt.Errorf("encoding certificate %s: %w", cert.Origin, err)
		}
		entries = append(entries,
			ExplodedEntry{Name: fileName, PEM: buf.Bytes()},
			ExplodedEntry{Name: fmt.Sprintf("%s.%d", hash, sequence), LinkTarget: fileName},
		)
	}
	return entries, nil
}

// certFileName derives a stable file name from the certificate's own content.
func certFileName(cert Certificate) string {
	fingerprint := sha256.Sum256(cert.DER)
	return hex.EncodeToString(fingerprint[:8])
}

// SubjectHash computes OpenSSL's X509_NAME_hash for the certificate's subject:
// the first four bytes of the SHA-1 over the canonical DER encoding of the
// name, read as a little-endian integer and rendered as eight hex digits. This
// is the file name OpenSSL looks for when resolving a certificate directory.
func SubjectHash(cert *x509.Certificate) string {
	sum := sha1.Sum(canonicalName(cert.RawSubject))
	return fmt.Sprintf("%08x", binary.LittleEndian.Uint32(sum[:4]))
}

// canonicalName applies OpenSSL's name canonicalization (X509_NAME_canon)
// before hashing. Two properties of that encoding are easy to get wrong and
// both change the hash:
//
//   - Every string attribute value is retagged as UTF8String and its bytes are
//     trimmed, internally whitespace-collapsed and ASCII-lowercased. Without
//     this, a name encoded as PrintableString would hash differently from the
//     same name encoded as UTF8String and the links would not resolve.
//   - The result is the bare concatenation of the canonicalized RDN SETs, with
//     *no* enclosing SEQUENCE header. OpenSSL builds it with i2d_name_canon,
//     which serializes each SET in turn and never writes an outer tag.
func canonicalName(rawSubject []byte) []byte {
	var rdnSequence []asn1.RawValue
	if _, err := asn1.Unmarshal(rawSubject, &rdnSequence); err != nil {
		// A subject that does not parse as an RDNSequence cannot be
		// canonicalized; hashing the raw bytes at least stays deterministic.
		return rawSubject
	}

	canonicalRDNs := make([]byte, 0, len(rawSubject))
	for _, rdn := range rdnSequence {
		canonical, err := canonicalRDN(rdn)
		if err != nil {
			return rawSubject
		}
		canonicalRDNs = append(canonicalRDNs, canonical...)
	}
	return canonicalRDNs
}

// attributeTypeAndValue is one element of a relative distinguished name.
type attributeTypeAndValue struct {
	Type  asn1.ObjectIdentifier
	Value asn1.RawValue
}

// canonicalRDN canonicalizes one SET OF AttributeTypeAndValue.
func canonicalRDN(rdn asn1.RawValue) ([]byte, error) {
	var attributes []attributeTypeAndValue
	// The "set" parameter is required: without it encoding/asn1 expects a
	// SEQUENCE OF for a slice and refuses the SET an RDN actually is.
	if _, err := asn1.UnmarshalWithParams(rdn.FullBytes, &attributes, "set"); err != nil {
		return nil, err
	}

	encodedAttributes := make([][]byte, 0, len(attributes))
	for _, attribute := range attributes {
		value := attribute.Value
		if isCanonicalizedTag(value.Tag) && value.Class == asn1.ClassUniversal {
			value = asn1.RawValue{
				Class: asn1.ClassUniversal,
				Tag:   asn1.TagUTF8String,
				Bytes: normalizeNameString(value.Bytes),
			}
		}
		encoded, err := asn1.Marshal(attributeTypeAndValue{Type: attribute.Type, Value: value})
		if err != nil {
			return nil, err
		}
		encodedAttributes = append(encodedAttributes, encoded)
	}

	// DER requires the elements of a SET OF to be sorted by their encoding.
	// Multi-valued RDNs are rare, but a mis-sorted one would hash differently
	// from what OpenSSL computes.
	sort.Slice(encodedAttributes, func(i, j int) bool {
		return bytes.Compare(encodedAttributes[i], encodedAttributes[j]) < 0
	})

	return asn1.Marshal(asn1.RawValue{
		Class:      asn1.ClassUniversal,
		Tag:        asn1.TagSet,
		IsCompound: true,
		Bytes:      bytes.Join(encodedAttributes, nil),
	})
}

// isCanonicalizedTag reports whether a universal ASN.1 string tag is one that
// OpenSSL canonicalizes (its ASN1_MASK_CANON). GeneralString is deliberately
// absent: OpenSSL leaves it untouched.
func isCanonicalizedTag(tag int) bool {
	const (
		tagVisibleString   = 26
		tagUniversalString = 28
	)
	switch tag {
	case asn1.TagUTF8String, asn1.TagPrintableString, asn1.TagT61String,
		asn1.TagIA5String, tagVisibleString, tagUniversalString, asn1.TagBMPString:
		return true
	}
	return false
}

// normalizeNameString trims leading and trailing whitespace, collapses runs of
// internal whitespace to a single space, and lowercases ASCII letters. It works
// on bytes rather than runes to stay byte-identical to OpenSSL's
// asn1_string_canon, which leaves any byte with the high bit set alone.
func normalizeNameString(value []byte) []byte {
	value = bytes.TrimFunc(value, func(r rune) bool { return r < 0x80 && isASCIISpace(byte(r)) })

	out := make([]byte, 0, len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 0x80:
			out = append(out, c)
		case isASCIISpace(c):
			out = append(out, ' ')
			// Trailing space was trimmed above, so this cannot run past the end.
			for i+1 < len(value) && isASCIISpace(value[i+1]) {
				i++
			}
		default:
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			out = append(out, c)
		}
	}
	return out
}

// isASCIISpace matches OpenSSL's ossl_isspace for single bytes.
func isASCIISpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\v', '\f', '\r':
		return true
	}
	return false
}

// subjectLine renders a short human-readable description of a certificate's
// subject for the comment above its PEM block.
func subjectLine(cert *x509.Certificate) string {
	if cert.Subject.CommonName != "" {
		return cert.Subject.CommonName
	}
	if len(cert.Subject.Organization) > 0 {
		return strings.Join(cert.Subject.Organization, ", ")
	}
	return cert.Subject.String()
}
