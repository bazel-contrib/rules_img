package grpcheaderinterceptor

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/credential"
	remoteexecution_proto "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/remote-apis/build/bazel/remote/execution/v2"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/version"
)

// requestMetadataHeaderKey is the binary metadata header name defined by the
// Remote Execution API for identifying the calling tool. See:
// https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto
const requestMetadataHeaderKey = "build.bazel.remote.execution.v2.requestmetadata-bin"

type requestMetadataContextKey struct{}

// RequestMetadata describes the rules_img-owned portion of REAPI request
// metadata. Fields not represented here (for example correlated_invocations_id
// and configuration_id) are preserved when callers already supplied metadata.
type RequestMetadata struct {
	ToolInvocationID string
	ActionID         string
	ActionMnemonic   string
	TargetID         string
}

// WithRequestMetadata returns a context carrying metadata for one logical image
// operation. The interceptor merges it with metadata already on the outgoing
// context and emits exactly one RequestMetadata header.
func WithRequestMetadata(ctx context.Context, requestMetadata RequestMetadata) context.Context {
	return context.WithValue(ctx, requestMetadataContextKey{}, requestMetadata)
}

// WithToolInvocationID returns a context whose outgoing REAPI requests identify
// the tool invocation that caused them. The context value overrides the default
// configured on the client connection, which allows persistent workers to use a
// different invocation ID for each work request.
func WithToolInvocationID(ctx context.Context, toolInvocationID string) context.Context {
	requestMetadata, _ := RequestMetadataFromContext(ctx)
	requestMetadata.ToolInvocationID = toolInvocationID
	return WithRequestMetadata(ctx, requestMetadata)
}

// RequestMetadataFromContext returns rules_img metadata stored on ctx.
func RequestMetadataFromContext(ctx context.Context) (RequestMetadata, bool) {
	requestMetadata, ok := ctx.Value(requestMetadataContextKey{}).(RequestMetadata)
	return requestMetadata, ok
}

func marshalRequestMetadata(requestMetadata *remoteexecution_proto.RequestMetadata) string {
	b, err := proto.Marshal(requestMetadata)
	if err != nil {
		panic(fmt.Sprintf("failed to marshal RequestMetadata: %v", err))
	}
	return string(b)
}

func mergeRequestMetadata(caller *remoteexecution_proto.RequestMetadata, rulesImg RequestMetadata) *remoteexecution_proto.RequestMetadata {
	merged := &remoteexecution_proto.RequestMetadata{}
	if caller != nil {
		merged = proto.Clone(caller).(*remoteexecution_proto.RequestMetadata)
	}

	// The client making these RPCs is rules_img. This field is intentionally
	// truthful; BuildBuddy attribution does not require tool_name="bazel".
	merged.ToolDetails = &remoteexecution_proto.ToolDetails{
		ToolName:    "rules_img",
		ToolVersion: version.Version,
	}
	if rulesImg.ToolInvocationID != "" {
		merged.ToolInvocationId = rulesImg.ToolInvocationID
	}
	// Caller-supplied logical action metadata is more specific than our
	// defaults, so only fill fields that are absent.
	if merged.ActionId == "" {
		merged.ActionId = rulesImg.ActionID
	}
	if merged.ActionMnemonic == "" {
		merged.ActionMnemonic = rulesImg.ActionMnemonic
	}
	if merged.TargetId == "" {
		merged.TargetId = rulesImg.TargetID
	}
	return merged
}

type authenticatingInterceptor struct {
	helper                 credential.Helper
	defaultRequestMetadata RequestMetadata
}

func (i *authenticatingInterceptor) requestMetadataBin(ctx context.Context, md metadata.MD) string {
	requestMetadata := i.defaultRequestMetadata
	contextOverridesInvocation := false
	if override, ok := RequestMetadataFromContext(ctx); ok {
		if override.ToolInvocationID != "" {
			requestMetadata.ToolInvocationID = override.ToolInvocationID
			contextOverridesInvocation = true
		}
		if override.ActionID != "" {
			requestMetadata.ActionID = override.ActionID
		}
		if override.ActionMnemonic != "" {
			requestMetadata.ActionMnemonic = override.ActionMnemonic
		}
		if override.TargetID != "" {
			requestMetadata.TargetID = override.TargetID
		}
	}

	var caller *remoteexecution_proto.RequestMetadata
	values := md.Get(requestMetadataHeaderKey)
	// Proxy-style rules_img services (notably the blob-cache service) pass the
	// server request context directly to their downstream gRPC client. Preserve
	// the caller's RequestMetadata across that hop even though gRPC keeps
	// incoming and outgoing metadata in separate context values.
	if len(values) == 0 {
		if incomingMD, ok := metadata.FromIncomingContext(ctx); ok {
			values = incomingMD.Get(requestMetadataHeaderKey)
		}
	}
	if len(values) > 0 {
		parsed := &remoteexecution_proto.RequestMetadata{}
		if err := proto.Unmarshal([]byte(values[0]), parsed); err == nil {
			caller = parsed
		}
	}
	// A connection-level invocation ID is only a fallback. Proxy callers carry
	// the invocation associated with the current request, while an explicit
	// context value remains authoritative for direct client calls and workers.
	if caller.GetToolInvocationId() != "" && !contextOverridesInvocation {
		requestMetadata.ToolInvocationID = ""
	}
	return marshalRequestMetadata(mergeRequestMetadata(caller, requestMetadata))
}

// unaryAddHeaders injects headers into a unary gRPC call.
func (i *authenticatingInterceptor) unaryAddHeaders(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		md = metadata.New(nil)
	} else {
		md = md.Copy()
	}

	md = addCredentialsToMD(ctx, cc.Target(), method, md, i.helper)
	md.Set(requestMetadataHeaderKey, i.requestMetadataBin(ctx, md))
	ctx = metadata.NewOutgoingContext(ctx, md)

	return invoker(ctx, method, req, reply, cc, opts...)
}

// streamAddHeaders injects headers into a stream gRPC call.
func (i *authenticatingInterceptor) streamAddHeaders(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		md = metadata.New(nil)
	} else {
		md = md.Copy()
	}

	md = addCredentialsToMD(ctx, cc.Target(), method, md, i.helper)
	md.Set(requestMetadataHeaderKey, i.requestMetadataBin(ctx, md))
	ctx = metadata.NewOutgoingContext(ctx, md)

	return streamer(ctx, desc, cc, method, opts...)
}

func addCredentialsToMD(ctx context.Context, target, method string, md metadata.MD, helper credential.Helper) metadata.MD {
	hostname, ok := strings.CutPrefix(target, "dns:")
	if !ok {
		fmt.Fprintf(os.Stderr, "WARNING: authenticating gRPC: unknown target definition %s\n", target)
		return md
	}

	methodParts := strings.Split(method, "/")
	if len(methodParts) < 2 || len(methodParts[0]) != 0 {
		fmt.Fprintf(os.Stderr, "WARNING: authenticating gRPC: unknown method definition %s\n", method)
		return md
	}

	u := url.URL{
		Scheme: "https",
		Host:   hostname,
		Path:   "/" + methodParts[1],
	}
	headers, _, err := helper.Get(ctx, u.String())
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: authenticating gRPC: failed to get credentials for %s: %v\n", u.String(), err)
		return md
	}
	if len(headers) == 0 {
		fmt.Fprintf(os.Stderr, "WARNING: authenticating gRPC: credential helper found no headers for %s - trying unauthenticated connection\n", u.String())
		return md
	}

	for k, vs := range headers {
		md.Append(k, vs...)
	}
	return md
}

func DialOptions(helper credential.Helper) []grpc.DialOption {
	return DialOptionsWithRequestMetadata(helper, RequestMetadata{})
}

// DialOptionsWithToolInvocationID returns dial options that attach credentials
// and RequestMetadata to every unary and streaming RPC. A non-empty invocation
// ID is used by default and may be overridden per request with
// WithToolInvocationID.
func DialOptionsWithToolInvocationID(helper credential.Helper, toolInvocationID string) []grpc.DialOption {
	return DialOptionsWithRequestMetadata(helper, RequestMetadata{ToolInvocationID: toolInvocationID})
}

// DialOptionsWithRequestMetadata returns dial options that attach credentials
// and merged RequestMetadata to every unary and streaming RPC.
func DialOptionsWithRequestMetadata(helper credential.Helper, requestMetadata RequestMetadata) []grpc.DialOption {
	interceptor := &authenticatingInterceptor{
		helper:                 helper,
		defaultRequestMetadata: requestMetadata,
	}
	return []grpc.DialOption{
		grpc.WithUnaryInterceptor(interceptor.unaryAddHeaders),
		grpc.WithStreamInterceptor(interceptor.streamAddHeaders),
	}
}
