package main

import (
	"fmt"
	"runtime"

	"github.com/tweag/rules_img/e2e/go/architecturevariant"
)

func main() {
	archString := runtime.GOARCH
	if architecturevariant.VARIANT != "" {
		archString = archString + "/" + architecturevariant.VARIANT
	}

	fmt.Printf("Hello, world from %s %s!\n", runtime.GOOS, archString)
}
