package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/bazel-contrib/rules_img_sbom/internal/bom"
)

// SetDestinationRequest is written by the toolchain's for_destination (see
// //sbom:toolchain.bzl), for the SBOM of an image manifest.
type SetDestinationRequest struct {
	Format string `json:"format"`
	// SBOM is the image's SBOM, as generated (it names the image by its label).
	SBOM string `json:"sbom"`
	// Descriptor is the image's descriptor.
	Descriptor string `json:"descriptor"`
	// Name is the image's label, which starts the name of the SBOM's primary
	// component.
	Name        string           `json:"name"`
	Destination DestinationFiles `json:"destination"`
}

func setDestinationMain(args []string) {
	flags := flag.NewFlagSet("set-destination", flag.ExitOnError)
	requestPath := flags.String("request", "", "Path to the JSON request written by the toolchain")
	outPath := flags.String("out", "", "Path to write the SBOM to")
	_ = flags.Parse(args)
	if *requestPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "Error: --request and --out are required")
		os.Exit(1)
	}
	if err := setDestination(*requestPath, *outPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func setDestination(requestPath, outPath string) error {
	var request SetDestinationRequest
	if err := readJSON(requestPath, &request); err != nil {
		return fmt.Errorf("reading the request: %w", err)
	}
	var descriptor bom.Descriptor
	if err := readJSON(request.Descriptor, &descriptor); err != nil {
		return fmt.Errorf("reading the image descriptor: %w", err)
	}
	destination, err := request.Destination.read()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(request.SBOM)
	if err != nil {
		return fmt.Errorf("reading the SBOM: %w", err)
	}
	out, err := bom.SetDestination(data, request.Format, destination, descriptor.Digest, request.Name)
	if err != nil {
		return fmt.Errorf("naming the image of %s where it is pushed: %w", request.SBOM, err)
	}
	return os.WriteFile(outPath, out, 0o644)
}
