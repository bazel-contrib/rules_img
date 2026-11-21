// Package referrerartifacts implements `img referrer-artifacts`, which turns
// referrer artifact specs into OCI artifact manifests bound to a pulled image.
//
// The pull repository rule and the images module extension run it at fetch
// time, so artifacts from sources that are not OCI registries (an SBOM
// database behind an HTTP endpoint, say) become content-addressed referrers,
// indistinguishable from the ones fetched from a registry: an image manifest
// with an empty config, the spec's files as layers, and a `subject` descriptor
// pointing at the pulled manifest (or one of its children).
package referrerartifacts

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	specv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/blobstore"
)

// Spec is the format of a referrer artifact spec file.
type Spec struct {
	Artifacts []Artifact `json:"artifacts"`
}

// Artifact describes one artifact manifest to build.
type Artifact struct {
	// Subject is the digest of the manifest the artifact refers to. It defaults
	// to the manifest the spec is attached to.
	Subject string `json:"subject,omitempty"`
	// ArtifactType is the artifactType of the manifest. Required, since the
	// config is empty (see the OCI image spec's guidelines for artifact usage).
	ArtifactType string `json:"artifactType"`
	// Annotations are the manifest annotations.
	Annotations map[string]string `json:"annotations,omitempty"`
	// Files become the layers of the manifest, in order.
	Files []File `json:"files,omitempty"`
}

// File is one layer of an artifact.
type File struct {
	// Path of the file, relative to the directory of the spec file.
	Path string `json:"path"`
	// MediaType of the layer.
	MediaType string `json:"mediaType"`
	// Annotations of the layer descriptor. org.opencontainers.image.title
	// defaults to the base name of Path.
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Result is one built artifact, in spec order.
type Result struct {
	Digest  string `json:"digest"`
	Subject string `json:"subject"`
}

// Output is written to --result.
type Output struct {
	// Artifacts lists the built artifacts of all specs, in order.
	Artifacts []Result `json:"artifacts"`
	// Inputs lists the files the specs packaged, so the caller can watch them.
	Inputs []string `json:"inputs"`
}

func ReferrerArtifactsProcess(args []string) {
	var specPaths stringSliceFlag
	var subjectsPath string
	var defaultSubject string
	var outputDir string
	var resultPath string

	flagSet := flag.NewFlagSet("referrer-artifacts", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Builds OCI artifact manifests that refer to a pulled image from referrer artifact specs.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img referrer-artifacts [OPTIONS]\n")
		flagSet.PrintDefaults()
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		fmt.Fprintf(flagSet.Output(), "  $ img referrer-artifacts --spec sboms.json --subjects subjects.json --default-subject sha256:... --output . --result result.json\n")
	}
	flagSet.Var(&specPaths, "spec", "Path to a referrer artifact spec (can be specified multiple times)")
	flagSet.StringVar(&subjectsPath, "subjects", "", "Path to a JSON object mapping each digest an artifact may refer to onto its descriptor (required)")
	flagSet.StringVar(&defaultSubject, "default-subject", "", "Subject of artifacts that don't name one (required)")
	flagSet.StringVar(&outputDir, "output", ".", "Directory whose blobs/ directory receives the manifests and blobs")
	flagSet.StringVar(&resultPath, "result", "", "Path to write the JSON list of built artifacts to (required)")
	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}
	if subjectsPath == "" || defaultSubject == "" || resultPath == "" {
		fmt.Fprintf(os.Stderr, "Error: --subjects, --default-subject and --result are required\n")
		flagSet.Usage()
		os.Exit(1)
	}

	subjectsData, err := os.ReadFile(subjectsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: reading subjects: %v\n", err)
		os.Exit(1)
	}
	var subjects map[string]specv1.Descriptor
	if err := json.Unmarshal(subjectsData, &subjects); err != nil {
		fmt.Fprintf(os.Stderr, "Error: parsing subjects: %v\n", err)
		os.Exit(1)
	}

	store := blobstore.New(filepath.Join(outputDir, "blobs"))
	if err := store.Init(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: initializing blob store: %v\n", err)
		os.Exit(1)
	}

	output := Output{Artifacts: []Result{}, Inputs: []string{}}
	for _, specPath := range specPaths {
		spec, err := ReadSpec(specPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %s: %v\n", specPath, err)
			os.Exit(1)
		}
		built, err := Build(spec, filepath.Dir(specPath), subjects, defaultSubject, store)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %s: %v\n", specPath, err)
			os.Exit(1)
		}
		output.Artifacts = append(output.Artifacts, built...)
		for _, artifact := range spec.Artifacts {
			for _, file := range artifact.Files {
				output.Inputs = append(output.Inputs, filepath.Join(filepath.Dir(specPath), filepath.FromSlash(file.Path)))
			}
		}
	}

	resultData, err := json.Marshal(output)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: marshaling result: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(resultPath, resultData, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "Error: writing result: %v\n", err)
		os.Exit(1)
	}
}

// ReadSpec parses a spec file, rejecting unknown fields so a typo in a field
// name is an error instead of a silently missing value.
func ReadSpec(specPath string) (Spec, error) {
	data, err := os.ReadFile(specPath)
	if err != nil {
		return Spec{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var spec Spec
	if err := decoder.Decode(&spec); err != nil {
		return Spec{}, fmt.Errorf("parsing spec: %w", err)
	}
	return spec, nil
}

// Build writes the manifest of each artifact in spec, and the blobs it
// references, into store.
func Build(spec Spec, baseDir string, subjects map[string]specv1.Descriptor, defaultSubject string, store *blobstore.Store) ([]Result, error) {
	results := make([]Result, 0, len(spec.Artifacts))
	for i, artifact := range spec.Artifacts {
		result, err := buildArtifact(artifact, baseDir, subjects, defaultSubject, store)
		if err != nil {
			return nil, fmt.Errorf("artifact %d: %w", i, err)
		}
		results = append(results, result)
	}
	return results, nil
}

func buildArtifact(artifact Artifact, baseDir string, subjects map[string]specv1.Descriptor, defaultSubject string, store *blobstore.Store) (Result, error) {
	if artifact.ArtifactType == "" {
		return Result{}, errors.New("artifactType is required")
	}
	subjectDigest := artifact.Subject
	if subjectDigest == "" {
		subjectDigest = defaultSubject
	}
	subject, ok := subjects[subjectDigest]
	if !ok {
		return Result{}, fmt.Errorf("subject %s is neither the pulled manifest nor one of its children", subjectDigest)
	}
	// Only the fields that identify the subject: a referrer's subject is a
	// pointer, not a copy of the subject's descriptor (no platform, annotations).
	subject = specv1.Descriptor{
		MediaType: subject.MediaType,
		Digest:    subject.Digest,
		Size:      subject.Size,
	}
	if subject.MediaType == "" || subject.Digest.String() != subjectDigest || subject.Size <= 0 {
		return Result{}, fmt.Errorf("incomplete descriptor for subject %s", subjectDigest)
	}

	// Neither the config nor an empty layer list is ever fetched: both use the
	// well-known empty JSON blob, which is written here so it is available
	// locally like every other blob of the artifact.
	if err := store.WriteSmallWithDigest(specv1.DescriptorEmptyJSON.Digest.String(), specv1.DescriptorEmptyJSON.Data); err != nil {
		return Result{}, err
	}

	layers := make([]specv1.Descriptor, 0, len(artifact.Files))
	for j, file := range artifact.Files {
		layer, err := addFile(file, baseDir, store)
		if err != nil {
			return Result{}, fmt.Errorf("file %d: %w", j, err)
		}
		layers = append(layers, layer)
	}
	if len(layers) == 0 {
		// "If the artifact has no layers, a single empty descriptor SHOULD be used."
		layers = append(layers, specv1.DescriptorEmptyJSON)
	}

	manifest := specv1.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    specv1.MediaTypeImageManifest,
		ArtifactType: artifact.ArtifactType,
		Config:       specv1.DescriptorEmptyJSON,
		Layers:       layers,
		Subject:      &subject,
		Annotations:  nonEmpty(artifact.Annotations),
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return Result{}, err
	}
	manifestDigest, err := store.WriteSmall(raw)
	if err != nil {
		return Result{}, err
	}
	return Result{Digest: manifestDigest, Subject: subjectDigest}, nil
}

func addFile(file File, baseDir string, store *blobstore.Store) (specv1.Descriptor, error) {
	if file.MediaType == "" {
		return specv1.Descriptor{}, errors.New("mediaType is required")
	}
	relPath, err := cleanRelativePath(file.Path)
	if err != nil {
		return specv1.Descriptor{}, err
	}
	fullPath := filepath.Join(baseDir, filepath.FromSlash(relPath))

	dgst, size, err := hashFile(fullPath)
	if err != nil {
		return specv1.Descriptor{}, err
	}
	f, err := os.Open(fullPath)
	if err != nil {
		return specv1.Descriptor{}, err
	}
	defer f.Close()
	if err := store.WriteLarge(dgst.String(), f); err != nil {
		return specv1.Descriptor{}, err
	}

	annotations := map[string]string{}
	for k, v := range file.Annotations {
		annotations[k] = v
	}
	if _, ok := annotations[specv1.AnnotationTitle]; !ok {
		annotations[specv1.AnnotationTitle] = path.Base(relPath)
	}
	descriptor := specv1.Descriptor{
		MediaType:   file.MediaType,
		Digest:      dgst,
		Size:        size,
		Annotations: annotations,
	}
	if dgst == specv1.DescriptorEmptyJSON.Digest {
		descriptor.Data = specv1.DescriptorEmptyJSON.Data
	}
	return descriptor, nil
}

// cleanRelativePath rejects paths that leave the spec's directory, so a spec
// can only package files that live next to it.
func cleanRelativePath(p string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	slashed := filepath.ToSlash(p)
	if path.IsAbs(slashed) || filepath.IsAbs(p) || filepath.VolumeName(p) != "" {
		return "", fmt.Errorf("path %q must be relative to the spec", p)
	}
	cleaned := path.Clean(slashed)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path %q must stay within the directory of the spec", p)
	}
	return cleaned, nil
}

func hashFile(p string) (digest.Digest, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	digester := digest.SHA256.Digester()
	size, err := io.Copy(digester.Hash(), f)
	if err != nil {
		return "", 0, fmt.Errorf("reading %s: %w", p, err)
	}
	return digester.Digest(), size, nil
}

func nonEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return strings.Join(*s, ",") }

func (s *stringSliceFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}
