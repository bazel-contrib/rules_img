package grpcheaderinterceptor

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/credential"
	remoteexecution_proto "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/remote-apis/build/bazel/remote/execution/v2"
)

func TestRequestMetadataOnUnaryAndStreamingRPCs(t *testing.T) {
	conn, err := grpc.NewClient("dns:cache.example", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("creating test client connection: %v", err)
	}
	defer conn.Close()

	interceptor := &authenticatingInterceptor{
		helper: credential.NopHelper(),
		defaultRequestMetadata: RequestMetadata{
			ToolInvocationID: "default-invocation",
			ActionID:         "rules_img:deploy",
			ActionMnemonic:   "ImgDeploy",
			TargetID:         "//images:app",
		},
	}

	tests := []struct {
		name string
		ctx  context.Context
		call func(context.Context, func(context.Context)) error
		want string
	}{
		{
			name: "unary default",
			ctx:  context.Background(),
			call: func(ctx context.Context, capture func(context.Context)) error {
				return interceptor.unaryAddHeaders(ctx, "/example.CAS/FindMissingBlobs", nil, nil, conn,
					func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
						capture(ctx)
						return nil
					})
			},
			want: "default-invocation",
		},
		{
			name: "unary context override",
			ctx:  WithToolInvocationID(context.Background(), "request-invocation"),
			call: func(ctx context.Context, capture func(context.Context)) error {
				return interceptor.unaryAddHeaders(ctx, "/example.CAS/BatchReadBlobs", nil, nil, conn,
					func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
						capture(ctx)
						return nil
					})
			},
			want: "request-invocation",
		},
		{
			name: "stream default",
			ctx:  context.Background(),
			call: func(ctx context.Context, capture func(context.Context)) error {
				_, err := interceptor.streamAddHeaders(ctx, &grpc.StreamDesc{}, conn, "/google.bytestream.ByteStream/Read",
					func(ctx context.Context, _ *grpc.StreamDesc, _ *grpc.ClientConn, _ string, _ ...grpc.CallOption) (grpc.ClientStream, error) {
						capture(ctx)
						return nil, nil
					})
				return err
			},
			want: "default-invocation",
		},
		{
			name: "stream context override",
			ctx:  WithToolInvocationID(context.Background(), "request-invocation"),
			call: func(ctx context.Context, capture func(context.Context)) error {
				_, err := interceptor.streamAddHeaders(ctx, &grpc.StreamDesc{}, conn, "/google.bytestream.ByteStream/Write",
					func(ctx context.Context, _ *grpc.StreamDesc, _ *grpc.ClientConn, _ string, _ ...grpc.CallOption) (grpc.ClientStream, error) {
						capture(ctx)
						return nil, nil
					})
				return err
			},
			want: "request-invocation",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var captured context.Context
			if err := tc.call(tc.ctx, func(ctx context.Context) { captured = ctx }); err != nil {
				t.Fatalf("interceptor call failed: %v", err)
			}
			got := requestMetadataFromContext(t, captured)
			if got.GetToolInvocationId() != tc.want {
				t.Errorf("tool_invocation_id = %q, want %q", got.GetToolInvocationId(), tc.want)
			}
			if got.GetToolDetails().GetToolName() != "rules_img" {
				t.Errorf("tool_name = %q, want rules_img", got.GetToolDetails().GetToolName())
			}
			if got.GetActionId() != "rules_img:deploy" || got.GetActionMnemonic() != "ImgDeploy" || got.GetTargetId() != "//images:app" {
				t.Errorf("logical action metadata = (%q, %q, %q), want rules_img deploy metadata", got.GetActionId(), got.GetActionMnemonic(), got.GetTargetId())
			}
		})
	}
}

func TestRequestMetadataMergesCallerMetadataIntoSingleHeader(t *testing.T) {
	caller := &remoteexecution_proto.RequestMetadata{
		ToolDetails:             &remoteexecution_proto.ToolDetails{ToolName: "caller", ToolVersion: "1"},
		ActionId:                "caller-action",
		ActionMnemonic:          "CallerMnemonic",
		TargetId:                "//caller:target",
		ToolInvocationId:        "caller-invocation",
		CorrelatedInvocationsId: "correlated",
		ConfigurationId:         "configuration",
	}
	callerBytes, err := proto.Marshal(caller)
	if err != nil {
		t.Fatal(err)
	}
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(requestMetadataHeaderKey, string(callerBytes)))
	ctx = WithRequestMetadata(ctx, RequestMetadata{
		ToolInvocationID: "rules-img-invocation",
		ActionID:         "rules_img:deploy",
		ActionMnemonic:   "ImgDeploy",
		TargetID:         "rules_img",
	})

	interceptor := &authenticatingInterceptor{helper: credential.NopHelper()}
	conn, err := grpc.NewClient("dns:cache.example", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var captured context.Context
	if err := interceptor.unaryAddHeaders(ctx, "/example.CAS/BatchReadBlobs", nil, nil, conn,
		func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
			captured = ctx
			return nil
		}); err != nil {
		t.Fatal(err)
	}

	got := requestMetadataFromContext(t, captured)
	if got.GetToolDetails().GetToolName() != "rules_img" {
		t.Errorf("tool_name = %q, want rules_img", got.GetToolDetails().GetToolName())
	}
	if got.GetToolInvocationId() != "rules-img-invocation" {
		t.Errorf("tool_invocation_id = %q, want rules-img-invocation", got.GetToolInvocationId())
	}
	if got.GetActionId() != caller.GetActionId() || got.GetActionMnemonic() != caller.GetActionMnemonic() || got.GetTargetId() != caller.GetTargetId() {
		t.Errorf("caller action metadata was not preserved: %v", got)
	}
	if got.GetCorrelatedInvocationsId() != "correlated" || got.GetConfigurationId() != "configuration" {
		t.Errorf("caller correlation/configuration metadata was not preserved: %v", got)
	}
}

func TestRequestMetadataForwardsIncomingCallerMetadata(t *testing.T) {
	caller := &remoteexecution_proto.RequestMetadata{
		ToolInvocationId:        "caller-invocation",
		ActionId:                "caller-action",
		ActionMnemonic:          "CallerMnemonic",
		TargetId:                "//caller:target",
		CorrelatedInvocationsId: "correlated",
	}
	callerBytes, err := proto.Marshal(caller)
	if err != nil {
		t.Fatal(err)
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(requestMetadataHeaderKey, string(callerBytes)))

	interceptor := &authenticatingInterceptor{
		helper: credential.NopHelper(),
		defaultRequestMetadata: RequestMetadata{
			ToolInvocationID: "server-startup-invocation",
			ActionID:         "rules_img:registry",
			ActionMnemonic:   "ImgRegistry",
			TargetID:         "rules_img",
		},
	}
	conn, err := grpc.NewClient("dns:cache.example", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var captured context.Context
	if err := interceptor.unaryAddHeaders(ctx, "/example.CAS/FindMissingBlobs", nil, nil, conn,
		func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
			captured = ctx
			return nil
		}); err != nil {
		t.Fatal(err)
	}

	got := requestMetadataFromContext(t, captured)
	if got.GetToolInvocationId() != caller.GetToolInvocationId() {
		t.Errorf("tool_invocation_id = %q, want %q", got.GetToolInvocationId(), caller.GetToolInvocationId())
	}
	if got.GetActionId() != caller.GetActionId() || got.GetActionMnemonic() != caller.GetActionMnemonic() || got.GetTargetId() != caller.GetTargetId() {
		t.Errorf("caller action metadata was not preserved: %v", got)
	}
	if got.GetCorrelatedInvocationsId() != caller.GetCorrelatedInvocationsId() {
		t.Errorf("correlated_invocations_id = %q, want %q", got.GetCorrelatedInvocationsId(), caller.GetCorrelatedInvocationsId())
	}
	if got.GetToolDetails().GetToolName() != "rules_img" {
		t.Errorf("tool_name = %q, want rules_img", got.GetToolDetails().GetToolName())
	}
}

func requestMetadataFromContext(t *testing.T, ctx context.Context) *remoteexecution_proto.RequestMetadata {
	t.Helper()
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		t.Fatal("outgoing metadata is missing")
	}
	values := md.Get(requestMetadataHeaderKey)
	if len(values) != 1 {
		t.Fatalf("request metadata header has %d values, want 1", len(values))
	}
	requestMetadata := &remoteexecution_proto.RequestMetadata{}
	if err := proto.Unmarshal([]byte(values[0]), requestMetadata); err != nil {
		t.Fatalf("decoding request metadata: %v", err)
	}
	return requestMetadata
}

type stubHelper struct {
	headers map[string][]string
}

func (s *stubHelper) Get(_ context.Context, _ string) (map[string][]string, time.Time, error) {
	return s.headers, time.Time{}, nil
}

// captureStderr swaps the process-wide os.Stderr, so tests using it cannot call t.Parallel.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("creating stderr capture file: %v", err)
	}
	previousStderr := os.Stderr
	t.Cleanup(func() {
		os.Stderr = previousStderr
		file.Close()
	})
	os.Stderr = file
	return func() string {
		t.Helper()
		data, err := os.ReadFile(file.Name())
		if err != nil {
			t.Fatalf("reading stderr capture: %v", err)
		}
		return string(data)
	}
}

func TestNopAndNilHelperDoNotWarn(t *testing.T) {
	tests := []struct {
		name   string
		helper credential.Helper
		target string
	}{
		{
			name:   "nop helper with dns target",
			helper: credential.NopHelper(),
			target: "dns:cache.example:50051",
		},
		{
			name:   "nop helper with unix target",
			helper: credential.NopHelper(),
			target: "unix:///tmp/cache.sock",
		},
		{
			name:   "nil helper with dns target",
			helper: nil,
			target: "dns:cache.example:50051",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			readStderr := captureStderr(t)
			interceptor := &authenticatingInterceptor{helper: tc.helper}
			conn, err := grpc.NewClient(tc.target, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatalf("creating test client connection: %v", err)
			}
			defer conn.Close()

			var unaryCtx, streamCtx context.Context
			if err := interceptor.unaryAddHeaders(context.Background(), "/example.CAS/FindMissingBlobs", nil, nil, conn,
				func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
					unaryCtx = ctx
					return nil
				}); err != nil {
				t.Fatalf("unary interceptor failed: %v", err)
			}
			if _, err := interceptor.streamAddHeaders(context.Background(), &grpc.StreamDesc{}, conn, "/google.bytestream.ByteStream/Read",
				func(ctx context.Context, _ *grpc.StreamDesc, _ *grpc.ClientConn, _ string, _ ...grpc.CallOption) (grpc.ClientStream, error) {
					streamCtx = ctx
					return nil, nil
				}); err != nil {
				t.Fatalf("stream interceptor failed: %v", err)
			}

			if got := readStderr(); got != "" {
				t.Errorf("unexpected stderr output: %q", got)
			}
			// RequestMetadata is still attached when no credential helper is configured.
			if got := requestMetadataFromContext(t, unaryCtx); got.GetToolDetails().GetToolName() != "rules_img" {
				t.Errorf("unary tool_name = %q, want rules_img", got.GetToolDetails().GetToolName())
			}
			if got := requestMetadataFromContext(t, streamCtx); got.GetToolDetails().GetToolName() != "rules_img" {
				t.Errorf("stream tool_name = %q, want rules_img", got.GetToolDetails().GetToolName())
			}
		})
	}
}

func TestConfiguredHelperAttachesHeadersWithoutWarning(t *testing.T) {
	readStderr := captureStderr(t)
	interceptor := &authenticatingInterceptor{
		helper: &stubHelper{
			headers: map[string][]string{
				"authorization": {"Bearer secret-token"},
			},
		},
	}
	conn, err := grpc.NewClient("dns:cache.example:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("creating test client connection: %v", err)
	}
	defer conn.Close()

	var captured context.Context
	if err := interceptor.unaryAddHeaders(context.Background(), "/example.CAS/BatchReadBlobs", nil, nil, conn,
		func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
			captured = ctx
			return nil
		}); err != nil {
		t.Fatalf("unary interceptor failed: %v", err)
	}

	if got := readStderr(); got != "" {
		t.Errorf("unexpected stderr output: %q", got)
	}
	md, ok := metadata.FromOutgoingContext(captured)
	if !ok {
		t.Fatal("outgoing metadata is missing")
	}
	if got := md.Get("authorization"); len(got) != 1 || got[0] != "Bearer secret-token" {
		t.Errorf("authorization header = %v, want [Bearer secret-token]", got)
	}
}

func TestConfiguredHelperWithNoHeadersWarns(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string][]string
	}{
		{name: "nil headers", headers: nil},
		{name: "empty headers", headers: map[string][]string{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			readStderr := captureStderr(t)
			interceptor := &authenticatingInterceptor{helper: &stubHelper{headers: tc.headers}}
			conn, err := grpc.NewClient("dns:cache.example:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatalf("creating test client connection: %v", err)
			}
			defer conn.Close()

			if err := interceptor.unaryAddHeaders(context.Background(), "/example.CAS/BatchReadBlobs", nil, nil, conn,
				func(_ context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
					return nil
				}); err != nil {
				t.Fatalf("unary interceptor failed: %v", err)
			}

			want := "WARNING: authenticating gRPC: credential helper found no headers for https://cache.example:50051/example.CAS - trying unauthenticated connection"
			if got := readStderr(); !strings.Contains(got, want) {
				t.Errorf("stderr = %q, want warning %q", got, want)
			}
		})
	}
}
