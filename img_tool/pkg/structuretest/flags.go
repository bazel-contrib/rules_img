package structuretest

import (
	"flag"
	"fmt"
	"os"
)

// RequestPathFlag parses the single `--request <file>` flag shared by the
// image test subcommands (`img image-structure-test`, `img image-size-test`)
// and returns the request file's path. A bare positional argument is accepted
// as an alias for --request. It exits with status 1 (after printing usage) when
// no request file is given, so callers can use the result directly.
func RequestPathFlag(name, summary string, args []string) string {
	var requestPath string
	flagSet := flag.NewFlagSet(name, flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "%s\n\n", summary)
		fmt.Fprintf(flagSet.Output(), "Usage: img %s --request <request.json>\n", name)
		fmt.Fprintf(flagSet.Output(), "       img %s <request.json>\n", name)
		flagSet.PrintDefaults()
	}
	flagSet.StringVar(&requestPath, "request", "", "Path to the JSON request file (required).")
	if err := flagSet.Parse(args); err != nil {
		os.Exit(1)
	}
	if requestPath == "" && flagSet.NArg() > 0 {
		requestPath = flagSet.Arg(0)
	}
	if requestPath == "" {
		fmt.Fprintln(os.Stderr, "Error: --request is required")
		flagSet.Usage()
		os.Exit(1)
	}
	return requestPath
}
