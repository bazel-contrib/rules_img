package ecr

import (
	"crypto/sha256"
	"encoding/hex"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
