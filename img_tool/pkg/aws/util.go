package aws

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	return string(data), err
}

func asResponseError(err error, target **ResponseError) bool { return errors.As(err, target) }
