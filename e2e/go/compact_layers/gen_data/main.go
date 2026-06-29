// gen_data generates pseudorandom bytes deterministically from a seed and writes them to a file.
package main

import (
	"flag"
	"math/rand"
	"os"
)

func main() {
	seed := flag.Int64("seed", 42, "random seed")
	size := flag.Int64("size", 1024, "output size in bytes")
	output := flag.String("output", "", "output file path (required)")
	flag.Parse()

	if *output == "" {
		flag.Usage()
		os.Exit(1)
	}

	rng := rand.New(rand.NewSource(*seed))
	data := make([]byte, *size)
	for i := range data {
		data[i] = byte(rng.Intn(256))
	}

	if err := os.WriteFile(*output, data, 0o644); err != nil {
		os.Stderr.WriteString("gen_data: " + err.Error() + "\n")
		os.Exit(1)
	}
}
