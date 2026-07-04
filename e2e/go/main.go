package main

import (
	"fmt"
	"os"
	"runtime"

	"github.com/bazel-contrib/rules_img/e2e/go/architecturevariant"
)

func main() {
	archString := runtime.GOARCH
	if architecturevariant.VARIANT != "" {
		archString = archString + "/" + architecturevariant.VARIANT
	}

	fmt.Printf("Hello, world from %s %s!\n", runtime.GOOS, archString)
	if greeting, ok := os.LookupEnv("ENV_GREETING"); ok {
		fmt.Println(greeting)
	}
	if len(os.Args) > 1 {
		fmt.Println(os.Args[1:])
	}
}
