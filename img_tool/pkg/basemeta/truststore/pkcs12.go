package truststore

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/asn1"
	"fmt"
	"strings"
	"unicode/utf16"
)

// PKCS#12 object identifiers (RFC 7292 appendix C, plus the Java-specific
// attribute below).
var (
	oidDataContentType = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}
	oidCertBag         = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 10, 1, 3}
	oidCertTypeX509    = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 22, 1}
	oidFriendlyName    = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 20}
	oidSHA256          = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}

	// oidJavaTrustedKeyUsage is Oracle's "TrustedKeyUsage" bag attribute. Its
	// presence is what makes the JDK treat a certBag as a *trusted certificate
	// entry* rather than an orphaned certificate; without it, `keytool -list`
	// shows nothing and the CA is not trusted. Sun encodes it under their
	// private arc 2.16.840.1.113894 ("sun"), and the value is the extended key
	// usage the certificate is trusted for.
	oidJavaTrustedKeyUsage = asn1.ObjectIdentifier{2, 16, 840, 1, 113894, 746875, 1, 1}
	// oidAnyExtendedKeyUsage says "trusted for every purpose", which is what a
	// CA bundle means.
	oidAnyExtendedKeyUsage = asn1.ObjectIdentifier{2, 5, 29, 37, 0}
)

// pkcs12MACIterations is the PBKDF iteration count for the integrity MAC.
// The JDK's own default is 10000; matching it keeps `keytool -importkeystore`
// from re-encoding the file.
const pkcs12MACIterations = 10000

// pfx is the outermost PKCS#12 structure (RFC 7292 section 4).
type pfx struct {
	Version  int
	AuthSafe contentInfoData
	MacData  macData `asn1:"optional"`
}

// contentInfoData is a ContentInfo restricted to the "data" content type, which
// is the only one this writer emits: the truststore holds no private keys, so
// nothing needs encrypting.
type contentInfoData struct {
	ContentType asn1.ObjectIdentifier
	Content     []byte `asn1:"explicit,tag:0"`
}

type macData struct {
	Mac        digestInfo
	MacSalt    []byte
	Iterations int
}

type digestInfo struct {
	Algorithm algorithmIdentifier
	Digest    []byte
}

type algorithmIdentifier struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

// safeBag is one entry of a SafeContents (RFC 7292 section 4.2).
//
// Value carries the [0] EXPLICIT wrapper in its own Class/Tag rather than via a
// struct tag: encoding/asn1 honours neither `explicit` nor `tag` on a RawValue,
// so an `asn1:"explicit,tag:0"` here would silently emit the bare bag SEQUENCE
// and the JDK would reject the file ("unsupported PKCS12 bag value type 48").
type safeBag struct {
	ID         asn1.ObjectIdentifier
	Value      asn1.RawValue
	Attributes []pkcs12Attribute `asn1:"set,optional"`
}

type pkcs12Attribute struct {
	ID     asn1.ObjectIdentifier
	Values asn1.RawValue `asn1:"set"`
}

// certBag wraps a single certificate inside a safeBag.
type certBag struct {
	ID   asn1.ObjectIdentifier
	Data []byte `asn1:"explicit,tag:0"`
}

// WritePKCS12 renders the certificates as a PKCS#12 truststore, the format a
// modern JDK uses for its `cacerts` file.
//
// The store holds trusted certificate entries only: there are no private keys,
// so the SafeContents is left unencrypted (exactly as the JDK writes cacerts)
// and the password protects only the integrity MAC. Each entry gets a
// friendlyName alias and Oracle's TrustedKeyUsage attribute, without which the
// JDK ignores the certificate.
func WritePKCS12(certs []Certificate, password string) ([]byte, error) {
	if len(certs) == 0 {
		return nil, fmt.Errorf("cannot build a PKCS#12 truststore with no certificates")
	}

	bags := make([]safeBag, 0, len(certs))
	aliases := newAliasAllocator()
	for _, cert := range certs {
		bag, err := buildCertBag(cert, aliases.allocate(cert))
		if err != nil {
			return nil, err
		}
		bags = append(bags, bag)
	}

	safeContents, err := asn1.Marshal(bags)
	if err != nil {
		return nil, fmt.Errorf("encoding PKCS#12 safe contents: %w", err)
	}

	// The AuthenticatedSafe is a SEQUENCE OF ContentInfo; a truststore needs
	// exactly one, holding the plaintext SafeContents.
	authenticatedSafe, err := asn1.Marshal([]contentInfoData{{
		ContentType: oidDataContentType,
		Content:     safeContents,
	}})
	if err != nil {
		return nil, fmt.Errorf("encoding PKCS#12 authenticated safe: %w", err)
	}

	mac := computeMAC(authenticatedSafe, password)

	encoded, err := asn1.Marshal(pfx{
		Version: 3,
		AuthSafe: contentInfoData{
			ContentType: oidDataContentType,
			Content:     authenticatedSafe,
		},
		MacData: mac,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding PKCS#12 file: %w", err)
	}
	return encoded, nil
}

// buildCertBag wraps one certificate as a trusted certificate entry.
func buildCertBag(cert Certificate, alias string) (safeBag, error) {
	bagValue, err := asn1.Marshal(certBag{ID: oidCertTypeX509, Data: cert.DER})
	if err != nil {
		return safeBag{}, fmt.Errorf("encoding certificate bag for %s: %w", cert.Origin, err)
	}

	friendlyName, err := marshalBMPStringSet(alias)
	if err != nil {
		return safeBag{}, fmt.Errorf("encoding alias %q: %w", alias, err)
	}
	trustedKeyUsage, err := marshalOIDSet(oidAnyExtendedKeyUsage)
	if err != nil {
		return safeBag{}, fmt.Errorf("encoding trusted key usage for %s: %w", cert.Origin, err)
	}

	return safeBag{
		ID: oidCertBag,
		Value: asn1.RawValue{
			Class:      asn1.ClassContextSpecific,
			Tag:        0,
			IsCompound: true,
			Bytes:      bagValue,
		},
		Attributes: []pkcs12Attribute{
			{ID: oidFriendlyName, Values: friendlyName},
			{ID: oidJavaTrustedKeyUsage, Values: trustedKeyUsage},
		},
	}, nil
}

// computeMAC builds the integrity MacData over the authenticated safe.
func computeMAC(authenticatedSafe []byte, password string) macData {
	salt := deriveSalt(authenticatedSafe)
	key := pkcs12KDF(password, salt, kdfIDMACKey, pkcs12MACIterations, sha256.Size)
	mac := hmac.New(sha256.New, key)
	mac.Write(authenticatedSafe)

	return macData{
		Mac: digestInfo{
			Algorithm: algorithmIdentifier{
				Algorithm:  oidSHA256,
				Parameters: asn1.NullRawValue,
			},
			Digest: mac.Sum(nil),
		},
		MacSalt:    salt,
		Iterations: pkcs12MACIterations,
	}
}

// saltDomainSeparator keeps the derived salt from colliding with any other
// hash of the same content.
const saltDomainSeparator = "rules_img pkcs12 mac salt\x00"

// deriveSalt computes the MAC salt from the store's own contents rather than
// from a random source.
//
// A random salt would make the output differ on every build, which Bazel
// requires it not to: the same inputs must produce the same bytes, or the layer
// digest changes on every rebuild and nothing caches. Deriving it from the
// content keeps that property while still giving distinct stores distinct
// salts.
//
// The usual reason to want an unpredictable salt -- forcing an attacker to
// redo password precomputation per file -- does not apply here. This store
// holds public CA certificates and no private keys; the password guards only
// the integrity check, is `changeit` by convention, and ships inside an image
// anyone can pull.
func deriveSalt(authenticatedSafe []byte) []byte {
	sum := sha256.Sum256(append([]byte(saltDomainSeparator), authenticatedSafe...))
	// 20 bytes is what the JDK writes.
	return sum[:20]
}

// kdfIDMACKey is the diversifier that RFC 7292 appendix B.3 assigns to MAC key
// material (1 is encryption keys, 2 is IVs).
const kdfIDMACKey byte = 3

// pkcs12KDF implements the key derivation of RFC 7292 appendix B.2.
//
// PBKDF2 would be the obvious modern choice, but the JDK derives the MAC key
// with this legacy function even for a SHA-256 keystore (SunJCE's
// HmacPBESHA256 is HmacPKCS12PBECore), so a store built with PBKDF2 fails
// keytool's integrity check.
func pkcs12KDF(password string, salt []byte, id byte, iterations, keyLen int) []byte {
	hash := sha256.New()
	u := hash.Size()      // hash output length
	v := hash.BlockSize() // hash block length

	// The password is a BMPString terminated by a NUL code unit. An empty
	// password contributes nothing at all, not even the terminator.
	var passwordBytes []byte
	if password != "" {
		passwordBytes = append(bmpString(password), 0, 0)
	}

	// D is the diversifier filled to one block; S and P are the salt and
	// password each repeated up to a whole number of blocks.
	d := bytes.Repeat([]byte{id}, v)
	i := append(fillToBlock(salt, v), fillToBlock(passwordBytes, v)...)

	var out []byte
	for len(out) < keyLen {
		hash.Reset()
		hash.Write(d)
		hash.Write(i)
		a := hash.Sum(nil)
		for iteration := 1; iteration < iterations; iteration++ {
			hash.Reset()
			hash.Write(a)
			a = hash.Sum(nil)
		}
		out = append(out, a...)
		if len(out) >= keyLen {
			break
		}

		// Advance I for the next block: every v-byte chunk of I is incremented
		// by B+1, where B is A repeated to v bytes, as a big-endian bignum.
		b := fillToBlock(a[:u], v)
		for offset := 0; offset < len(i); offset += v {
			addBlock(i[offset:offset+v], b)
		}
	}
	return out[:keyLen]
}

// fillToBlock repeats data until it fills a whole number of blockSize-byte
// blocks. Empty input yields empty output, as appendix B.2 specifies.
func fillToBlock(data []byte, blockSize int) []byte {
	if len(data) == 0 {
		return nil
	}
	blocks := (len(data) + blockSize - 1) / blockSize
	out := make([]byte, blocks*blockSize)
	for i := range out {
		out[i] = data[i%len(data)]
	}
	return out
}

// addBlock computes dst = (dst + src + 1) mod 2^(8*len(dst)), treating both as
// big-endian integers of equal length.
func addBlock(dst, src []byte) {
	carry := 1
	for i := len(dst) - 1; i >= 0; i-- {
		sum := int(dst[i]) + int(src[i]) + carry
		dst[i] = byte(sum)
		carry = sum >> 8
	}
}

// marshalBMPStringSet encodes a string as the SET OF BMPString that a
// friendlyName attribute value is.
func marshalBMPStringSet(value string) (asn1.RawValue, error) {
	encoded, err := asn1.Marshal(asn1.RawValue{
		Class: asn1.ClassUniversal,
		Tag:   asn1.TagBMPString,
		Bytes: bmpString(value),
	})
	if err != nil {
		return asn1.RawValue{}, err
	}
	return asn1.RawValue{
		Class:      asn1.ClassUniversal,
		Tag:        asn1.TagSet,
		IsCompound: true,
		Bytes:      encoded,
	}, nil
}

// marshalOIDSet encodes an object identifier as a SET OF OBJECT IDENTIFIER.
func marshalOIDSet(oid asn1.ObjectIdentifier) (asn1.RawValue, error) {
	encoded, err := asn1.Marshal(oid)
	if err != nil {
		return asn1.RawValue{}, err
	}
	return asn1.RawValue{
		Class:      asn1.ClassUniversal,
		Tag:        asn1.TagSet,
		IsCompound: true,
		Bytes:      encoded,
	}, nil
}

// bmpString encodes a string as big-endian UTF-16, the content of an ASN.1
// BMPString. Characters outside the basic multilingual plane become surrogate
// pairs, which is what the JDK does as well.
func bmpString(value string) []byte {
	units := utf16.Encode([]rune(value))
	out := make([]byte, 0, len(units)*2)
	for _, unit := range units {
		out = append(out, byte(unit>>8), byte(unit))
	}
	return out
}

// aliasAllocator hands out the unique per-entry names a keystore needs.
type aliasAllocator struct {
	used map[string]int
}

func newAliasAllocator() *aliasAllocator {
	return &aliasAllocator{used: make(map[string]int)}
}

// allocate derives a keytool-style alias from a certificate: the lowercased
// common name (or organization, or subject hash) with spaces turned into
// underscores. Repeats get a numeric suffix, so a bundle carrying two roots
// with the same common name still yields two distinct entries.
func (a *aliasAllocator) allocate(cert Certificate) string {
	base := strings.ToLower(subjectLine(cert.Parsed))
	base = strings.Join(strings.Fields(base), "_")
	if base == "" {
		base = SubjectHash(cert.Parsed)
	}

	count := a.used[base]
	a.used[base] = count + 1
	if count == 0 {
		return base
	}
	return fmt.Sprintf("%s_%d", base, count+1)
}
