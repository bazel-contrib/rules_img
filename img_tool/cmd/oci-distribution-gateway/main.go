// Command oci-distribution-gateway runs a container registry gateway that only
// forwards requests to real upstream registries.
//
// Clients connect anonymously and must set the X-rules_img-Original-Host header
// to select the upstream registry. The gateway authenticates to that upstream
// using the ambient registry credentials (docker config, cloud keychains, or an
// optional Bazel credential helper) and can allow or deny individual blob and
// manifest read/write operations. The upstreams a client may reach are
// restricted by a list of hostname regular expressions.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	reg "github.com/bazel-contrib/rules_img/img_tool/pkg/auth/registry"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/serve/gateway"
)

// newFlagSet builds the flag set with a usage banner and examples.
func newFlagSet() *flag.FlagSet {
	flagSet := flag.NewFlagSet("oci-distribution-gateway", flag.ContinueOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Run a forwarding OCI distribution gateway.\n\n")
		fmt.Fprintf(flagSet.Output(), "The gateway forwards requests to the upstream registry named in the\n")
		fmt.Fprintf(flagSet.Output(), "X-rules_img-Original-Host request header, subject to the policy below.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: oci-distribution-gateway [OPTIONS]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"oci-distribution-gateway --port 8080 --allowed-registry ghcr.io --allowed-registry gcr.io",
			"oci-distribution-gateway --port 8080 --allowed-registry-regex '.*\\.docker\\.io'",
			"oci-distribution-gateway --unix-socket /tmp/gw.sock --allowed-registry-regex '.*' --allow-blob-write --allow-manifest-write",
		}
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
	}
	return flagSet
}

func Run(ctx context.Context, args []string) {
	var (
		address              string
		port                 int
		unixSocket           string
		allowBlobRead        bool
		allowBlobWrite       bool
		allowManifestRead    bool
		allowManifestWrite   bool
		allowedRegistries    stringSliceFlag
		allowedRegistryRegex stringSliceFlag
		defaultRegistry      string
		credentialHelperPath string
	)

	flagSet := newFlagSet()
	flagSet.StringVar(&address, "address", "localhost", "Address to bind the gateway to (ignored when --unix-socket is set)")
	flagSet.IntVar(&port, "port", 0, "Port to bind the gateway to (0 picks a free port; ignored when --unix-socket is set)")
	flagSet.StringVar(&unixSocket, "unix-socket", "", "Path to a UNIX domain socket to listen on instead of TCP")
	flagSet.BoolVar(&allowBlobRead, "allow-blob-read", true, "Allow reading blobs (GET/HEAD on /v2/<name>/blobs)")
	flagSet.BoolVar(&allowBlobWrite, "allow-blob-write", false, "Allow writing blobs (POST/PATCH/PUT/DELETE on /v2/<name>/blobs)")
	flagSet.BoolVar(&allowManifestRead, "allow-manifest-read", true, "Allow reading manifests (GET/HEAD on /v2/<name>/manifests), tag listings and referrers")
	flagSet.BoolVar(&allowManifestWrite, "allow-manifest-write", false, "Allow writing manifests (PUT/DELETE on /v2/<name>/manifests)")
	flagSet.Var(&allowedRegistries, "allowed-registry", "Exact upstream hostname to allow (e.g. gcr.io). Matched literally, so dots need no escaping. Repeatable.")
	flagSet.Var(&allowedRegistryRegex, "allowed-registry-regex", "Regular expression matched against the upstream hostname (e.g. '.*\\.docker\\.io'). Anchored (full match). Repeatable.")
	flagSet.StringVar(&defaultRegistry, "default-registry", "", "Upstream registry to forward to when a request omits the X-rules_img-Original-Host header (must also be allowed via --allowed-registry or --allowed-registry-regex)")
	flagSet.StringVar(&credentialHelperPath, "credential-helper", "", "Path to a Bazel credential helper binary used to authenticate to upstream registries (optional)")

	if err := flagSet.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// flag already printed the usage.
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, err.Error())
		flagSet.Usage()
		os.Exit(1)
	}

	if len(allowedRegistries) == 0 && len(allowedRegistryRegex) == 0 {
		fmt.Fprintln(os.Stderr, "Error: at least one --allowed-registry or --allowed-registry-regex must be specified")
		flagSet.Usage()
		os.Exit(1)
	}

	allowed, err := gateway.CompileAllowlist(allowedRegistries, allowedRegistryRegex)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if credentialHelperPath != "" {
		// reg.Keychain() reads IMG_CREDENTIAL_HELPER; wire the flag through it.
		if err := os.Setenv("IMG_CREDENTIAL_HELPER", credentialHelperPath); err != nil {
			log.Fatalf("Failed to set credential helper: %v", err)
		}
	}

	handler := gateway.New(
		gateway.WithPolicy(gateway.Policy{
			AllowBlobRead:      allowBlobRead,
			AllowBlobWrite:     allowBlobWrite,
			AllowManifestRead:  allowManifestRead,
			AllowManifestWrite: allowManifestWrite,
		}),
		gateway.WithAllowedRegistries(allowed),
		gateway.WithDefaultRegistry(defaultRegistry),
		gateway.WithKeychain(reg.Keychain()),
	)

	listener, cleanup, err := listen(unixSocket, address, port)
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}
	defer cleanup()

	// Force HTTP/1.1: the registry protocol is request/response and some
	// clients (and unix-socket transports) do not negotiate h2.
	protocols := &http.Protocols{}
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(false)
	protocols.SetUnencryptedHTTP2(false)

	server := &http.Server{
		Handler:           handler,
		Protocols:         protocols,
		ReadHeaderTimeout: 30 * time.Second,
		// Generous body timeouts: blob uploads and downloads can be large.
		ReadTimeout:  30 * time.Minute,
		WriteTimeout: 30 * time.Minute,
		IdleTimeout:  5 * time.Minute,
	}

	fmt.Fprintf(os.Stderr, "oci-distribution-gateway listening on %s\n", listener.Addr())
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Failed to serve: %v", err)
	}
}

// listen opens the configured listener. When unixSocket is non-empty it listens
// on that socket (removing any stale socket file first); otherwise it listens on
// TCP. The returned cleanup removes the socket file on shutdown.
func listen(unixSocket, address string, port int) (net.Listener, func(), error) {
	if unixSocket != "" {
		// Remove a stale socket left by a previous run, if any.
		if _, err := os.Stat(unixSocket); err == nil {
			if err := os.Remove(unixSocket); err != nil {
				return nil, func() {}, fmt.Errorf("removing stale socket %q: %w", unixSocket, err)
			}
		}
		l, err := net.Listen("unix", unixSocket)
		if err != nil {
			return nil, func() {}, err
		}
		return l, func() { _ = os.Remove(unixSocket) }, nil
	}
	l, err := net.Listen("tcp", fmt.Sprintf("%s:%d", address, port))
	if err != nil {
		return nil, func() {}, err
	}
	return l, func() {}, nil
}

func main() {
	Run(context.Background(), os.Args)
}
