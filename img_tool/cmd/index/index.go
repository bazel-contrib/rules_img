package index

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	specsv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

var (
	manifestDescriptorArgs manifestDescriptors
	annotationArgs         annotations
	configTemplates        string
	digestOutput           string
	descriptorOutput       string
	subjectDescriptorInput string
)

func IndexProcess(ctx context.Context, args []string) {
	flagSet := flag.NewFlagSet("index", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Creates an image index based on a list of manifests.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img index [--manifest-descriptor descriptor] [output]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"img index --manifest-descriptor image_linux_amd64.json --manifest-descriptor image_linux_aarch64.json index.json",
		}
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
		os.Exit(1)
	}
	flagSet.Var(&manifestDescriptorArgs, "manifest-descriptor", `File containing a descriptor for a manifest.`)
	flagSet.Var(&annotationArgs, "annotation", `Key-value pair to add as an annotation`)
	flagSet.StringVar(&configTemplates, "config-templates", "", `A JSON file containing template-expanded annotations values.`)
	flagSet.StringVar(&digestOutput, "digest", "", `The (optional) output file for the digest of the manifest. This is useful for postprocessing.`)
	flagSet.StringVar(&descriptorOutput, "descriptor", "", `The output file for the descriptor of the index.`)
	flagSet.StringVar(&subjectDescriptorInput, "subject-descriptor", "", `A JSON file containing the descriptor of the subject manifest or index.`)

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}

	if flagSet.NArg() != 1 {
		flagSet.Usage()
		os.Exit(1)
	}

	indexPath := flagSet.Arg(0)

	// Read config templates if provided
	var templatesData *ConfigTemplates
	if configTemplates != "" {
		var err error
		templatesData, err = readConfigTemplates(configTemplates)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to read config templates: %v\n", err)
			os.Exit(1)
		}
	}

	// Use template annotations if available, otherwise use command line annotations
	annotations := map[string]string(annotationArgs)
	if templatesData != nil && templatesData.Annotations != nil {
		annotations = templatesData.Annotations
	}

	index := specsv1.Index{
		Versioned: specs.Versioned{
			SchemaVersion: 2,
		},
		MediaType:   specsv1.MediaTypeImageIndex,
		Manifests:   []specsv1.Descriptor(manifestDescriptorArgs),
		Annotations: annotations,
	}

	// Set subject descriptor if provided
	if subjectDescriptorInput != "" {
		subjectData, err := os.ReadFile(subjectDescriptorInput)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to read subject descriptor file %s: %v\n", subjectDescriptorInput, err)
			os.Exit(1)
		}
		var subjectDesc specsv1.Descriptor
		if err := json.Unmarshal(subjectData, &subjectDesc); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to decode subject descriptor: %v\n", err)
			os.Exit(1)
		}
		index.Subject = &subjectDesc
	}

	rawIndex, err := json.Marshal(index)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to marshal image index: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(indexPath, rawIndex, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write image index to %s: %v\n", indexPath, err)
		os.Exit(1)
	}

	indexSHA256 := sha256.Sum256(rawIndex)

	if digestOutput != "" {
		if err := os.WriteFile(digestOutput, []byte(fmt.Sprintf("sha256:%x", indexSHA256[:])), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to write digest to %s: %v\n", digestOutput, err)
			os.Exit(1)
		}
	}

	if descriptorOutput != "" {
		descriptor := specsv1.Descriptor{
			MediaType: specsv1.MediaTypeImageIndex,
			Digest:    digest.NewDigestFromBytes(digest.SHA256, indexSHA256[:]),
			Size:      int64(len(rawIndex)),
		}
		descriptorRaw, err := json.Marshal(descriptor)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to marshal index descriptor: %v\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(descriptorOutput, descriptorRaw, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to write index descriptor to %s: %v\n", descriptorOutput, err)
			os.Exit(1)
		}
	}
}

// ConfigTemplates represents the structure of the config templates JSON file
type ConfigTemplates struct {
	Annotations map[string]string `json:"annotations"`
}

// readConfigTemplates reads and parses the config templates JSON file
func readConfigTemplates(filePath string) (*ConfigTemplates, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("opening config templates file: %w", err)
	}
	defer file.Close()

	var templates ConfigTemplates
	if err := json.NewDecoder(file).Decode(&templates); err != nil {
		return nil, fmt.Errorf("decoding config templates file: %w", err)
	}

	return &templates, nil
}
