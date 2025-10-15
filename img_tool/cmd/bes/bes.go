package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/credential"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/protohelper"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
	bes_proto "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/build_event_service"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/serve/bes"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/serve/bes/syncer"
)

func Run(ctx context.Context, args []string) {
	var address string
	var port int
	var commitMode string
	var casEndpoint string
	var credentialHelperPath string

	flagSet := flag.NewFlagSet("bes", flag.ExitOnError)
	flagSet.Usage = func() {
		_, _ = fmt.Fprintf(flagSet.Output(), "Serve a Build Event Service gRPC server\n\n")
		_, _ = fmt.Fprintf(flagSet.Output(), "Usage: bes [OPTIONS]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"bes --cas-endpoint grpcs://remote.buildbuddy.io",
			"bes --address 0.0.0.0 --port 9090 --cas-endpoint grpcs://remote.buildbuddy.io",
			"bes --commit-mode per-stream --credential-helper tweag-credential-helper --cas-endpoint grpcs://remote.buildbuddy.io",
		}
		_, _ = fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			_, _ = fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
		os.Exit(1)
	}
	flagSet.StringVar(&address, "address", "localhost", "Address to bind the BES gRPC server to")
	flagSet.IntVar(&port, "port", 9090, "Port to bind the BES gRPC server to")
	flagSet.StringVar(&commitMode, "commit-mode", "background", "Commit mode: 'background' or 'per-stream'")
	flagSet.StringVar(&casEndpoint, "cas-endpoint", "", "CAS gRPC endpoint (required)")
	flagSet.StringVar(&credentialHelperPath, "credential-helper", "", "Path to credential helper binary (optional, defaults to no helper)")

	if err := flagSet.Parse(args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err.Error())
		flagSet.Usage()
		os.Exit(1)
	}

	if casEndpoint == "" {
		fmt.Fprintln(os.Stderr, "Error: --cas-endpoint is required")
		flagSet.Usage()
		os.Exit(1)
	}

	var mode bes.CommitMode
	switch commitMode {
	case "background":
		mode = bes.CommitModeBackground
	case "per-stream":
		mode = bes.CommitModePerStream
	default:
		fmt.Fprintf(os.Stderr, "Error: invalid commit mode '%s', must be 'background' or 'per-stream'\n", commitMode)
		flagSet.Usage()
		os.Exit(1)
	}

	var credentialHelper credential.Helper
	if len(credentialHelperPath) > 0 {
		credentialHelper = credential.New(credentialHelperPath)
		log.Printf("Using credential helper: %s", credentialHelperPath)
	} else {
		credentialHelper = credential.NopHelper()
		log.Println("No credential helper configured")
	}

	grpcClientConn, err := protohelper.Client(casEndpoint, credentialHelper)
	if err != nil {
		log.Fatalf("Failed to create gRPC client connection to CAS: %v", err)
	}
	defer func() {
		if err := grpcClientConn.Close(); err != nil {
			log.Printf("Failed to close gRPC client connection: %v", err)
		}
	}()

	casClient, err := cas.New(grpcClientConn, cas.WithLearnCapabilities(true))
	if err != nil {
		log.Fatalf("Failed to create CAS client: %v", err)
	}

	s := syncer.New(casClient)

	besService := bes.New(s, mode)

	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", address, port))
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	bes_proto.RegisterPublishBuildEventServer(grpcServer, besService)

	actualPort := listener.Addr().(*net.TCPAddr).Port
	log.Printf("BES gRPC server listening on %s:%d (commit-mode: %s)", address, actualPort, commitMode)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		log.Println("Received shutdown signal, gracefully stopping...")

		shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		grpcServer.GracefulStop()

		if err := besService.Shutdown(shutdownCtx); err != nil {
			log.Printf("Shutdown completed with errors: %v", err)
		}

		s.Shutdown()

		log.Println("Server shutdown complete")
		os.Exit(0)
	}()

	if err := grpcServer.Serve(listener); err != nil {
		log.Fatalf("Failed to serve gRPC server: %v", err)
	}
}

func main() {
	ctx := context.Background()
	Run(ctx, os.Args)
}
