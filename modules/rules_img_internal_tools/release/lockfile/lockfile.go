// Generates a rules_img prebuilt lockfile: the file that tells a consumer of the
// module where to fetch the prebuilt tool binary for its platform.
//
// Two modes, matching the two download modes the lockfile format has (see
// img/private/prebuilt/lockfile.bzl in @rules_img):
//
//	url	--tag plus one --tool per platform. Hashes each binary and records the
//		release tag, so the consumer downloads it from a file server. This is
//		what a real release publishes.
//
//	oci	--registry/--repository plus one --layer per platform. Each layer is an
//		OCI descriptor (as written by @rules_img's layer rules) of a binary
//		already pushed to a registry as a blob, so the consumer fetches it from
//		there by digest. This is what a prerelease publishes, alongside the ORAS
//		artifact whose layers the descriptors belong to.
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// platformFiles collects repeated `<os>_<cpu>=<path>` flags.
type platformFiles []struct {
	os   string
	cpu  string
	path string
}

func (pb *platformFiles) String() string {
	return fmt.Sprintf("%v", *pb)
}

func (pb *platformFiles) Set(value string) error {
	kv := strings.SplitN(value, "=", 2)
	if len(kv) != 2 {
		return fmt.Errorf("invalid value: expected key value pair separated by =")
	}
	os_cpu := strings.SplitN(kv[0], "_", 2)
	if len(os_cpu) != 2 {
		return fmt.Errorf("invalid value: expected os and cpu separated by _")
	}
	*pb = append(*pb, struct {
		os   string
		cpu  string
		path string
	}{
		os:   os_cpu[0],
		cpu:  os_cpu[1],
		path: kv[1],
	})
	return nil
}

// lockfileItem is one entry of the lockfile: how to fetch the tool for one
// platform. Which fields are set depends on the download mode; a url mode entry
// carries version and integrity, an oci mode entry the blob's location.
type lockfileItem struct {
	DownloadMode  string            `json:"download_mode,omitempty"`
	Version       string            `json:"version,omitempty"`
	Integrity     string            `json:"integrity,omitempty"`
	SHA256        string            `json:"sha256,omitempty"`
	Registry      string            `json:"registry,omitempty"`
	Repository    string            `json:"repository,omitempty"`
	AuthChallenge map[string]string `json:"auth_challenge,omitempty"`
	OS            string            `json:"os"`
	CPU           string            `json:"cpu"`
}

// descriptor is the part of an OCI descriptor this tool reads out of the layer
// metadata @rules_img's layer rules write.
type descriptor struct {
	Digest string `json:"digest"`
}

var (
	tag         string
	binaries    platformFiles
	layers      platformFiles
	registry    string
	repository  string
	authRealm   string
	authService string
)

func main() {
	flag.StringVar(&tag, "tag", "", "Release tag the prebuilt binaries are published under (e.g. v0.3.19 or rules_img_signer_cosign-v0.0.1). Recorded verbatim as each entry's version so the prebuilt download URL resolves.")
	flag.Var(&binaries, "tool", "Key-value pairs of platform name to tool binary path.")
	flag.Var(&layers, "layer", "Key-value pairs of platform name to the path of an OCI descriptor of that platform's binary, as pushed to a registry.")
	flag.StringVar(&registry, "registry", "", "Registry the binaries are published to as blobs, e.g. ghcr.io. Selects the oci download mode.")
	flag.StringVar(&repository, "repository", "", "Repository the blobs belong to, e.g. bazel-contrib/rules_img/img.")
	flag.StringVar(&authRealm, "auth-realm", "", "Token endpoint of the registry, as advertised by its WWW-Authenticate challenge. Recording it lets a consumer skip probing GET /v2/.")
	flag.StringVar(&authService, "auth-service", "", "Service name the registry's token endpoint expects.")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "expected lockfile output")
		os.Exit(1)
	}

	lockfileItems, err := buildLockfile()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	lockfile, err := json.Marshal(lockfileItems)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshaling lockfile: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(flag.Arg(0), lockfile, os.ModePerm); err != nil {
		fmt.Fprintf(os.Stderr, "writing lockfile: %v\n", err)
		os.Exit(1)
	}
}

func buildLockfile() ([]lockfileItem, error) {
	ociMode := registry != "" || repository != "" || len(layers) > 0
	urlMode := tag != "" || len(binaries) > 0
	if ociMode && urlMode {
		return nil, fmt.Errorf("--registry/--repository/--layer describe the oci download mode and --tag/--tool the url one; use one set or the other")
	}
	if ociMode {
		return ociLockfile()
	}
	return urlLockfile()
}

func urlLockfile() ([]lockfileItem, error) {
	if tag == "" {
		return nil, fmt.Errorf("missing required --tag")
	}
	var items []lockfileItem
	for _, bin := range binaries {
		sri, err := fileSRI(bin.path)
		if err != nil {
			return nil, fmt.Errorf("calculating sri of %s: %w", bin.path, err)
		}
		items = append(items, lockfileItem{
			Version:   tag,
			Integrity: sri,
			OS:        bin.os,
			CPU:       bin.cpu,
		})
	}
	return items, nil
}

func ociLockfile() ([]lockfileItem, error) {
	if registry == "" {
		return nil, fmt.Errorf("missing required --registry")
	}
	if repository == "" {
		return nil, fmt.Errorf("missing required --repository")
	}
	if len(layers) == 0 {
		return nil, fmt.Errorf("missing required --layer")
	}
	var authChallenge map[string]string
	if authRealm != "" || authService != "" {
		authChallenge = map[string]string{}
		if authRealm != "" {
			authChallenge["realm"] = authRealm
		}
		if authService != "" {
			authChallenge["service"] = authService
		}
	}

	var items []lockfileItem
	for _, layer := range layers {
		hex, err := layerSHA256(layer.path)
		if err != nil {
			return nil, err
		}
		items = append(items, lockfileItem{
			DownloadMode:  "oci",
			SHA256:        hex,
			Registry:      registry,
			Repository:    repository,
			AuthChallenge: authChallenge,
			OS:            layer.os,
			CPU:           layer.cpu,
		})
	}

	// Order the entries independently of the order the flags arrived in, so the
	// lockfile is a function of its contents alone.
	sort.Slice(items, func(i, j int) bool {
		if items[i].OS != items[j].OS {
			return items[i].OS < items[j].OS
		}
		return items[i].CPU < items[j].CPU
	})
	return items, nil
}

// layerSHA256 reads an OCI descriptor and returns the hex sha256 of the blob it
// describes.
func layerSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading layer descriptor: %w", err)
	}
	var desc descriptor
	if err := json.Unmarshal(data, &desc); err != nil {
		return "", fmt.Errorf("parsing layer descriptor %s: %w", path, err)
	}
	hex, ok := strings.CutPrefix(desc.Digest, "sha256:")
	if !ok {
		return "", fmt.Errorf("layer descriptor %s: digest %q is not a sha256", path, desc.Digest)
	}
	if len(hex) != 64 {
		return "", fmt.Errorf("layer descriptor %s: digest %q is not 64 hex characters", path, desc.Digest)
	}
	return hex, nil
}

func fileSRI(source string) (string, error) {
	reader, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	hash := sha256.New()
	_, err = io.Copy(hash, reader)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256-%s", base64.StdEncoding.EncodeToString([]byte(hash.Sum(nil)))), nil
}
