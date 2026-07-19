package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// signingKeys holds paths to short-lived key material generated for a signing
// run. The whole directory is removed by the caller.
type signingKeys struct {
	dir          string
	cosignKey    string // PEM (PKCS#8) private key path -> $RULES_IMG_COSIGN_KEY
	cosignPub    string // PEM (PKIX) public key path (for cosign verify)
	notationKey  string // PEM (PKCS#8) private key path -> $RULES_IMG_NOTATION_KEY
	notationCert string // PEM self-signed leaf cert -> $RULES_IMG_NOTATION_CERTIFICATE_CHAIN
}

// generateSigningKeys creates short-lived ECDSA P-256 key material for cosign
// (a bare key) and notation (a key plus a self-signed code-signing certificate).
func generateSigningKeys() (*signingKeys, error) {
	dir, err := os.MkdirTemp("", "rules-img-e2e-keys-")
	if err != nil {
		return nil, err
	}
	keys := &signingKeys{
		dir:          dir,
		cosignKey:    filepath.Join(dir, "cosign.key"),
		cosignPub:    filepath.Join(dir, "cosign.pub"),
		notationKey:  filepath.Join(dir, "notation.key"),
		notationCert: filepath.Join(dir, "notation.crt"),
	}

	// cosign: an unencrypted ECDSA P-256 key ($COSIGN_PASSWORD="" at deploy time).
	cosignPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := writePKCS8Key(keys.cosignKey, cosignPriv); err != nil {
		return nil, err
	}
	if err := writePKIXPublicKey(keys.cosignPub, &cosignPriv.PublicKey); err != nil {
		return nil, err
	}

	// notation: an ECDSA P-256 key plus a self-signed code-signing certificate,
	// which doubles as the trust anchor during verification.
	notationPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := writePKCS8Key(keys.notationKey, notationPriv); err != nil {
		return nil, err
	}
	if err := writeSelfSignedCert(keys.notationCert, notationPriv); err != nil {
		return nil, err
	}
	return keys, nil
}

func writePKCS8Key(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	return writePEM(path, "PRIVATE KEY", der)
}

func writePKIXPublicKey(path string, pub *ecdsa.PublicKey) error {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return err
	}
	return writePEM(path, "PUBLIC KEY", der)
}

func writeSelfSignedCert(path string, key *ecdsa.PrivateKey) error {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "rules_img-e2e-signer"},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	return writePEM(path, "CERTIFICATE", der)
}

func writePEM(path, blockType string, der []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{Type: blockType, Bytes: der})
}
