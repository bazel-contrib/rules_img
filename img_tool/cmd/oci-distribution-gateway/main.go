// Command oci-distribution-gateway runs a container registry gateway that only
// forwards requests to real upstream registries.
//
// Clients connect anonymously and must set the X-rules_img-Original-Host header
// to select the upstream registry. The gateway authenticates to that upstream
// using the ambient registry credentials (docker config, cloud keychains, or an
// optional Bazel credential helper) and authorizes every request against a
// policy file: an ordered list of allow/deny rules matched on the upstream
// registry host, repository path, and operation (blob/manifest read/write). The
// policy file can be reloaded at runtime by sending the process a SIGHUP.
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
	"os/signal"
	"syscall"
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
		fmt.Fprintf(flagSet.Output(), "X-rules_img-Original-Host request header, subject to the policy file.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: oci-distribution-gateway --policy-file <path> [OPTIONS]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"oci-distribution-gateway --port 8080 --policy-file /etc/img/gateway-policy.json",
			"oci-distribution-gateway --unix-socket /run/gw.sock --policy-file /etc/img/gateway-policy.yaml",
			"oci-distribution-gateway --validate-policy --policy-file /etc/img/gateway-policy.json",
			"oci-distribution-gateway --unix-socket /run/gw.sock --dangerously-allow-all",
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
		defaultRegistry      string
		credentialHelperPath string
		policyFile           string
		validatePolicy       bool
		dangerouslyAllowAll  bool
	)

	flagSet := newFlagSet()
	flagSet.StringVar(&address, "address", "localhost", "Address to bind the gateway to (ignored when --unix-socket is set)")
	flagSet.IntVar(&port, "port", 0, "Port to bind the gateway to (0 picks a free port; ignored when --unix-socket is set)")
	flagSet.StringVar(&unixSocket, "unix-socket", "", "Path to a UNIX domain socket to listen on instead of TCP")
	flagSet.StringVar(&defaultRegistry, "default-registry", "", "Upstream registry to forward to when a request omits the X-rules_img-Original-Host header (must also be allowed by the policy)")
	flagSet.StringVar(&credentialHelperPath, "credential-helper", "", "Path to a Bazel credential helper binary used to authenticate to upstream registries (optional)")
	flagSet.StringVar(&policyFile, "policy-file", "", "Path to a JSON (or YAML) policy file with per-repository allow/deny rules. Required unless --dangerously-allow-all is set. Reloadable at runtime with SIGHUP.")
	flagSet.BoolVar(&validatePolicy, "validate-policy", false, "Load and validate --policy-file, then exit (0 if valid, non-zero otherwise). Does not start the gateway.")
	flagSet.BoolVar(&dangerouslyAllowAll, "dangerously-allow-all", false, "Allow every request to every upstream, ignoring the policy file. DANGEROUS: only for trusted, isolated environments.")

	if err := flagSet.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// flag already printed the usage.
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, err.Error())
		flagSet.Usage()
		os.Exit(1)
	}

	if validatePolicy {
		if policyFile == "" {
			fmt.Fprintln(os.Stderr, "Error: --validate-policy requires --policy-file")
			os.Exit(1)
		}
		cp, err := gateway.LoadPolicyFile(policyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "policy %s is valid (%s)\n", policyFile, cp.Summary())
		os.Exit(0)
	}

	// Resolve the authorization policy. --dangerously-allow-all overrides (and
	// ignores) any policy file; otherwise a policy file is required.
	var authz *gateway.CompiledPolicy
	switch {
	case dangerouslyAllowAll:
		if policyFile != "" {
			log.Printf("warning: --dangerously-allow-all is set; ignoring --policy-file %s", policyFile)
		}
		log.Printf("WARNING: --dangerously-allow-all is set; the gateway will forward EVERY request to any upstream without policy checks")
		authz = gateway.AllowAll()
	case policyFile == "":
		fmt.Fprintln(os.Stderr, "Error: --policy-file is required (or pass --dangerously-allow-all to disable policy checks)")
		flagSet.Usage()
		os.Exit(1)
	default:
		cp, err := gateway.LoadPolicyFile(policyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		log.Printf("loaded policy from %s (%s)", policyFile, cp.Summary())
		authz = cp
	}

	if credentialHelperPath != "" {
		// reg.Keychain() resolves the OCI-registry credential helper from the
		// environment; wire the flag through it as the registry-scoped helper.
		if err := os.Setenv(reg.EnvCredentialHelperOCIRegistry, credentialHelperPath); err != nil {
			log.Fatalf("Failed to set credential helper: %v", err)
		}
	}

	handler := gateway.New(
		gateway.WithAuthorizer(authz),
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

	// Reload the policy file on SIGHUP. A failed reload keeps the previous
	// policy, so a bad edit never widens access or takes the gateway down.
	// (With --dangerously-allow-all there is no file to reload.)
	if policyFile != "" && !dangerouslyAllowAll {
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		go func() {
			for range hup {
				if cp, err := handler.Reload(policyFile); err != nil {
					log.Printf("policy reload FAILED, keeping previous policy: %v", err)
				} else {
					log.Printf("reloaded policy from %s (%s)", policyFile, cp.Summary())
				}
			}
		}()
	}

	// Shut down gracefully on SIGINT/SIGTERM so in-flight uploads/downloads can
	// finish (or the deadline forces them closed).
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	fmt.Fprintf(os.Stderr, "oci-distribution-gateway listening on %s\n", listener.Addr())

	select {
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to serve: %v", err)
		}
	case sig := <-shutdown:
		log.Printf("received %s, shutting down gracefully...", sig)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown error: %v", err)
		}
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
