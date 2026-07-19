// Command integration_test_registry serves a throwaway, in-memory OCI registry
// for the rules_img integration tests.
//
// It is a thin wrapper around //pkg/registry (a fork of go-containerregistry's
// in-memory registry), modeled on go-containerregistry's `crane registry serve`
// but with a minimal flag surface tailored to the integration test runner.
//
// The one thing the test runner needs is a deterministic way to learn which
// port the registry bound to: after the listener is up, the process prints
// exactly one line to STDOUT — the host:port to dial (e.g. "localhost:42133").
// All request logging goes to STDERR, so STDOUT stays clean for parsing.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/registry"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("integration_test_registry", flag.ContinueOnError)
	fs.SetOutput(stderr)
	address := fs.String("address", "localhost", "Address to bind the registry to.")
	port := fs.Int("port", 0, "Port to bind to (0 lets the OS choose a free port).")
	referrers := fs.Bool("referrers", true, "Enable the OCI 1.1 referrers API.")
	readyFile := fs.String("ready-file", "", "Optional path to write the chosen host:port to once listening.")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", *address, *port))
	if err != nil {
		fmt.Fprintf(stderr, "integration_test_registry: listen: %v\n", err)
		return 1
	}

	boundPort := listener.Addr().(*net.TCPAddr).Port
	hostPort := fmt.Sprintf("%s:%d", dialHost(*address), boundPort)

	// FIRST line of STDOUT is the host:port to dial. The runner parses this;
	// registry request logs go to STDERR (registry.New's default logger).
	fmt.Fprintln(stdout, hostPort)
	_ = stdout.Sync()
	if *readyFile != "" {
		if err := os.WriteFile(*readyFile, []byte(hostPort+"\n"), 0o644); err != nil {
			fmt.Fprintf(stderr, "integration_test_registry: write ready file: %v\n", err)
			return 1
		}
	}

	handler := registry.New(registry.WithReferrersSupport(*referrers))

	// HTTP/1.1 only: go-containerregistry's default transport talks HTTP/1.1 to
	// plain-HTTP (localhost) registries; avoid any h2c negotiation surprises.
	protocols := &http.Protocols{}
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(false)
	protocols.SetUnencryptedHTTP2(false)
	server := &http.Server{
		Handler:           handler,
		ReadTimeout:       30 * time.Minute,
		ReadHeaderTimeout: 30 * time.Minute,
		WriteTimeout:      30 * time.Minute,
		IdleTimeout:       30 * time.Minute,
		Protocols:         protocols,
	}

	// Shut down cleanly on SIGINT/SIGTERM (the runner also just kills us).
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		_ = server.Close()
	}()

	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(stderr, "integration_test_registry: serve: %v\n", err)
		return 1
	}
	return 0
}

// dialHost maps a bind address to the host a client should dial. Wildcard/empty
// bind addresses are reachable via localhost.
func dialHost(address string) string {
	switch address {
	case "", "0.0.0.0", "::", "[::]":
		return "localhost"
	default:
		return address
	}
}
