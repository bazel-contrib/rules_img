package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/malt3/go-containerregistry/pkg/registry"
	registryv1 "github.com/malt3/go-containerregistry/pkg/v1"
	"google.golang.org/grpc"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/credential"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/protohelper"
	blobcache_proto "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/blobcache"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/serve/blobcache"
	combined "github.com/bazel-contrib/rules_img/img_tool/pkg/serve/registry"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/serve/registry/reapi"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/serve/registry/s3"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/serve/registry/upstream"
)

func Run(ctx context.Context, args []string) {
	var registryAddress string
	var httpPort int
	var grpcPort int
	var enableBlobCache bool
	var blobStores blobStores
	var upstreamURL string
	var reapiEndpoint string
	var s3Bucket string
	var s3endpoint string
	var s3Region string
	var s3profile string
	var credentialHelperPath string

	flagSet := flag.NewFlagSet("registry", flag.ExitOnError)
	flagSet.Usage = func() {
		_, _ = fmt.Fprintf(flagSet.Output(), "Serve a container registry\n\n")
		_, _ = fmt.Fprintf(flagSet.Output(), "Usage: registry [OPTIONS]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"registry --address 0.0.0.0 --port 8080",
			"registry --blob-store s3 --blob-store reapi",
		}
		_, _ = fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			_, _ = fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
		os.Exit(1)
	}
	flagSet.StringVar(&registryAddress, "address", "localhost", "Address to bind the registry server to")
	flagSet.IntVar(&httpPort, "port", 0, "Port to bind the registry HTTP server to")
	flagSet.IntVar(&grpcPort, "grpc-port", 0, "Port to bind the gRPC server to")
	flagSet.BoolVar(&enableBlobCache, "enable-blobcache", false, "Enable gRPC blob cache service")
	flagSet.Var(&blobStores, "blob-store", `Blob store to use for the registry. Can be specified multiple times. One of "s3", "reapi", or "upstream".`)
	flagSet.StringVar(&upstreamURL, "upstream-url", "", "URL of the registry to use for the upstream blob store")
	flagSet.StringVar(&reapiEndpoint, "reapi-endpoint", "", "REAPI endpoint to use for the remote cache")
	flagSet.StringVar(&s3Bucket, "s3-bucket", "", "S3 bucket to use for the S3 blob store")
	flagSet.StringVar(&s3endpoint, "s3-endpoint", "", "S3 endpoint to use for the S3 blob store (optional, defaults to AWS S3)")
	flagSet.StringVar(&s3Region, "s3-region", "", "S3 region to use for the S3 blob store (optional, defaults to auto detect)")
	flagSet.StringVar(&s3profile, "s3-profile", "", "AWS profile to use for the S3 blob store (optional, defaults to default profile)")
	flagSet.StringVar(&credentialHelperPath, "credential-helper", "", "Path to credential helper binary (optional, defaults to no helper)")

	if err := flagSet.Parse(args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err.Error())
		flagSet.Usage()
		os.Exit(1)
	}
	if len(blobStores) == 0 {
		fmt.Fprintln(os.Stderr, "Error: at least one blob store must be specified")
		flagSet.Usage()
		os.Exit(1)
	}

	var credentialHelper credential.Helper
	if len(credentialHelperPath) > 0 {
		credentialHelper = credential.New(credentialHelperPath)
	} else {
		credentialHelper = credential.NopHelper()
	}

	var grpcClientConn *grpc.ClientConn
	if reapiEndpoint != "" {
		var err error
		grpcClientConn, err = protohelper.Client(reapiEndpoint, credentialHelper)
		if err != nil {
			log.Fatalf("Failed to create gRPC client connection: %v", err)
		}
	}

	var s3Opts []func(*awsconfig.LoadOptions) error
	if s3endpoint != "" {
		s3Opts = append(s3Opts, func(o *awsconfig.LoadOptions) error {
			o.BaseEndpoint = s3endpoint
			return nil
		})
	}
	if s3Region != "" {
		s3Opts = append(s3Opts, awsconfig.WithRegion(s3Region))
	}
	if s3profile != "" {
		s3Opts = append(s3Opts, awsconfig.WithSharedConfigProfile(s3profile))
	}

	blobSizeCache := combined.NewBlobSizeCache()
	var stores []combined.Handler
	var nonREAPIStores []combined.Handler
	var wantREAPI bool
	var reapiIndex int
	for _, store := range blobStores {
		switch store {
		case "s3":
			s3Store, err := s3.New(
				ctx,
				30*time.Minute, // expires
				15*time.Minute, // minLifetime
				func(repo string, hash registryv1.Hash) (bucket string, key string, err error) {
					return s3Bucket, fmt.Sprintf("%s/%s", hash.Algorithm, hash.Hex), nil
				},
				s3Opts...,
			)
			if err != nil {
				log.Fatalf("Failed to create S3 blob store: %v", err)
			}
			stores = append(stores, s3Store)
			nonREAPIStores = append(nonREAPIStores, s3Store)
		case "upstream":
			stores = append(stores, upstream.New(upstreamURL))
			nonREAPIStores = append(nonREAPIStores, upstream.New(upstreamURL))
		case "reapi":
			if reapiEndpoint == "" || grpcClientConn == nil {
				log.Fatalln("REAPI endpoint must be specified when using the reapi blob store")
			}
			if wantREAPI {
				log.Fatalln("Only one reapi blob store can be specified")
			}
			wantREAPI = true
			reapiIndex = len(stores)
			stores = append(stores, nil) // Placeholder for reapi store, will be set later
		}
	}
	var blobWriter combined.Writer
	if wantREAPI {
		var reapiUpstream = combined.NewCombinedBlobStore(blobSizeCache, nil /* writer */, nonREAPIStores...).(combined.Handler)
		reapiStore, err := reapi.New(reapiUpstream, grpcClientConn, blobSizeCache)
		if err != nil {
			log.Fatalf("Failed to create REAPI blob store: %v", err)
		}
		stores[reapiIndex] = reapiStore
		blobWriter = reapiStore
	}
	if enableBlobCache {
		if grpcClientConn == nil {
			log.Fatalln("gRPC client connection must be provided to enable blob cache")
		}
		service := blobcache.NewServer(grpcClientConn, blobSizeCache)
		grpcListener, err := net.Listen("tcp", fmt.Sprintf(":%d", grpcPort))
		if err != nil {
			log.Fatalf("Failed to start gRPC server: %v", err)
		}
		fmt.Fprintf(os.Stderr, "gRPC blob cache server listening on %d\n", grpcPort)
		go func() {
			// TOODO: Handle errors and shutdown gracefully.
			grpcServer := grpc.NewServer()
			blobcache_proto.RegisterBlobsServer(grpcServer, service)
			if err := grpcServer.Serve(grpcListener); err != nil {
				log.Fatalf("Failed to serve gRPC server: %v", err)
			}
		}()
	}

	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", registryAddress, httpPort))
	if err != nil {
		log.Fatalln(err)
	}
	porti := listener.Addr().(*net.TCPAddr).Port

	combinedStore := combined.NewCombinedBlobStore(blobSizeCache, blobWriter, stores...)
	callbacker := combined.NewBlobSizeCacheCallback(blobSizeCache, combinedStore.(combined.Handler))
	protos := &http.Protocols{}
	protos.SetHTTP1(true)
	protos.SetHTTP2(false)
	protos.SetUnencryptedHTTP2(false)
	server := &http.Server{
		Handler: registry.New(
			registry.WithBlobHandler(combinedStore),
			registry.WithManifestPutCallback(callbacker.ManifestPutCallback),
		),
		IdleTimeout:       30 * time.Minute,
		ReadTimeout:       30 * time.Minute,
		WriteTimeout:      30 * time.Minute,
		ReadHeaderTimeout: 30 * time.Minute,
		Protocols:         protos,
	}
	fmt.Fprintf(os.Stderr, "Listening on %d\n", porti)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Failed to serve HTTP server: %v", err)
	}
}

func main() {
	ctx := context.Background()
	Run(ctx, os.Args)
}
