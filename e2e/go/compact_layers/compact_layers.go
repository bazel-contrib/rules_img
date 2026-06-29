package main

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
)

//go:embed data.bin
var data []byte

func main() {
	hash := sha256.Sum256(data)
	fmt.Printf("SHA256: %x\n", hash)
}
