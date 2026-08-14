package protohelper

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	credhelper "github.com/bazel-contrib/rules_img/img_tool/pkg/auth/credential"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/grpcheaderinterceptor"
)

// EnvMaxRecvMsgSize overrides, in bytes, the largest gRPC message the clients built here
// will accept. It follows the IMG_REAPI_* convention of pkg/cas, though it reaches every
// connection [Client] builds, including the blob cache endpoint, which is not a remote
// execution API service.
const EnvMaxRecvMsgSize = "IMG_REAPI_MAX_RECV_MSG_SIZE"

// defaultMaxRecvMsgSize has to exceed grpc-go's own 4 MiB, because the remote execution
// API bounds ReadResponse size nowhere and GetCapabilities exposes no field to derive a
// bound from: a conforming server may answer an ordinary whole-blob read with a larger
// message. 100 MiB is bazelbuild/remote-apis-sdks' figure, kept finite even though the
// limit is per call and a deploy pools one connection per job, so concurrent allocation
// can reach this times the job count. Bazel keeps no ceiling at all
// (maxInboundMessageSize(Integer.MAX_VALUE)).
const defaultMaxRecvMsgSize = 100 * 1024 * 1024

// minMaxRecvMsgSize is the floor for EnvMaxRecvMsgSize, and grpc-go's own default. Below
// it, reads an unconfigured client would have completed fail instead, and pkg/cas spends a
// full retry budget per blob on the ResourceExhausted before reporting anything.
const minMaxRecvMsgSize = 4 * 1024 * 1024

// maxRecvMsgSize is the environment-derived limit [Client] applies, parsed once: a deploy
// builds one client per pooled connection, and a malformed value is worth one warning.
var maxRecvMsgSize = sync.OnceValue(maxRecvMsgSizeFromEnv)

func maxRecvMsgSizeFromEnv() int {
	raw := strings.TrimSpace(os.Getenv(EnvMaxRecvMsgSize))
	if raw == "" {
		return defaultMaxRecvMsgSize
	}
	size, err := strconv.Atoi(raw)
	if err != nil || size < minMaxRecvMsgSize {
		warnInvalidEnv(EnvMaxRecvMsgSize, raw, defaultMaxRecvMsgSize)
		return defaultMaxRecvMsgSize
	}
	return size
}

func Client(uri string, helper credhelper.Helper, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	return client(uri, helper, grpcheaderinterceptor.RequestMetadata{}, opts...)
}

// ClientWithToolInvocationID creates a gRPC client whose requests carry the
// supplied REAPI RequestMetadata.tool_invocation_id by default.
func ClientWithToolInvocationID(uri string, helper credhelper.Helper, toolInvocationID string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	return client(uri, helper, grpcheaderinterceptor.RequestMetadata{ToolInvocationID: toolInvocationID}, opts...)
}

// ClientWithRequestMetadata creates a gRPC client whose requests carry the
// supplied logical-operation metadata by default.
func ClientWithRequestMetadata(uri string, helper credhelper.Helper, requestMetadata RequestMetadata, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	return client(uri, helper, requestMetadata.interceptorMetadata(), opts...)
}

// RequestMetadata identifies one logical rules_img operation. The same values
// are attached to unary RPCs, ByteStream RPCs, lazy reads, and retries.
type RequestMetadata struct {
	ToolInvocationID string
	ActionID         string
	ActionMnemonic   string
	TargetID         string
}

func (m RequestMetadata) interceptorMetadata() grpcheaderinterceptor.RequestMetadata {
	return grpcheaderinterceptor.RequestMetadata{
		ToolInvocationID: m.ToolInvocationID,
		ActionID:         m.ActionID,
		ActionMnemonic:   m.ActionMnemonic,
		TargetID:         m.TargetID,
	}
}

// WithRequestMetadata returns a context that identifies one logical rules_img
// operation on every outgoing cache request.
func WithRequestMetadata(ctx context.Context, requestMetadata RequestMetadata) context.Context {
	return grpcheaderinterceptor.WithRequestMetadata(ctx, requestMetadata.interceptorMetadata())
}

// RequestMetadataFromContext returns rules_img metadata stored on ctx.
func RequestMetadataFromContext(ctx context.Context) (RequestMetadata, bool) {
	requestMetadata, ok := grpcheaderinterceptor.RequestMetadataFromContext(ctx)
	return RequestMetadata{
		ToolInvocationID: requestMetadata.ToolInvocationID,
		ActionID:         requestMetadata.ActionID,
		ActionMnemonic:   requestMetadata.ActionMnemonic,
		TargetID:         requestMetadata.TargetID,
	}, ok
}

// WithToolInvocationID returns a context that overrides the connection's
// default RequestMetadata.tool_invocation_id for RPCs made with that context.
func WithToolInvocationID(ctx context.Context, toolInvocationID string) context.Context {
	return grpcheaderinterceptor.WithToolInvocationID(ctx, toolInvocationID)
}

func client(uri string, helper credhelper.Helper, requestMetadata grpcheaderinterceptor.RequestMetadata, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	// Ours goes first so a caller passing its own MaxCallRecvMsgSize wins: grpc
	// applies call options in order and the last one set takes effect.
	opts = append([]grpc.DialOption{
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxRecvMsgSize())),
	}, opts...)

	parsed, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("invalid uri for grpc: %s: %w", uri, err)
	}

	var target string
	switch parsed.Scheme {
	case "grpc":
		// unencrypted grpc
		warnUnencryptedGRPC(uri)
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
		target = fmt.Sprintf("dns:%s", parsed.Host)
	case "grpcs":
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(nil)))
		target = fmt.Sprintf("dns:%s", parsed.Host)
	case "unix":
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
		target = uri
	default:
		return nil, fmt.Errorf("unsupported scheme for grpc: %s", parsed.Scheme)
	}

	// If userinfo is present, add Basic auth credentials
	if parsed.User != nil && parsed.User.String() != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(basicAuthFromUserinfo(parsed.User)))
	}

	opts = append(opts, grpcheaderinterceptor.DialOptionsWithRequestMetadata(helper, requestMetadata)...)

	// Keepalive, so a peer that goes away during a long transfer is noticed rather
	// than leaving a read blocked forever: with a stream in flight and no data
	// arriving for Time, the client pings, and closes the connection if the ping
	// is not acked within Timeout. pkg/cas then resumes the read from the offset
	// it reached. Any data the server sends resets the timer, so this only fires
	// while a transfer is genuinely stalled.
	//
	// Time must not go below the 5 minutes gRPC servers enforce by default
	// (keepalive.EnforcementPolicy.MinTime, grpc-go's
	// defaultKeepalivePolicyMinTime). A server counts every ping that arrives
	// sooner as a strike and, after three, sends
	// GOAWAY(ENHANCE_YOUR_CALM, "too_many_pings") — killing the whole connection
	// and failing every stream on it, which is far worse than the single
	// RST_STREAM the keepalive was added to avoid. A server only clears the
	// strike counter when it sends data, so a stalled transfer is exactly when a
	// too-frequent ping trips it.
	//
	// Timeout does double duty: on Linux grpc-go also applies it as
	// TCP_USER_TIMEOUT, so it bounds how long *any* unacknowledged write may go
	// before the kernel drops the connection, not just an unacked keepalive ping.
	// 20 seconds matches Bazel's default and leaves room for a slow link; a
	// tighter value kills healthy connections on a congested one.
	//
	// PermitWithoutStream stays at its default of false: a connection with no RPCs
	// in flight is not worth pinging, and a server treats pings without an active
	// stream far more harshly still.
	opts = append(opts, grpc.WithKeepaliveParams(keepalive.ClientParameters{
		Time:    5 * time.Minute,
		Timeout: 20 * time.Second,
	}))

	return grpc.NewClient(target, opts...)
}

// basicAuthCredentials implements grpc.PerRPCCredentials for Basic auth.
type basicAuthCredentials struct {
	username string
	password string
}

func basicAuthFromUserinfo(userinfo *url.Userinfo) *basicAuthCredentials {
	password, _ := userinfo.Password()
	return &basicAuthCredentials{
		username: userinfo.Username(),
		password: password,
	}
}

func (c *basicAuthCredentials) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	auth := c.username + ":" + c.password
	encoded := base64.StdEncoding.EncodeToString([]byte(auth))
	return map[string]string{
		"authorization": "Basic " + encoded,
	}, nil
}

func (c *basicAuthCredentials) RequireTransportSecurity() bool {
	return false
}

func warnUnencryptedGRPC(uri string) {
	warnMutex.Lock()
	defer warnMutex.Unlock()

	if _, warned := WarnedURIs[uri]; warned {
		return
	}
	WarnedURIs[uri] = struct{}{}
	fmt.Fprintf(os.Stderr, "WARNING: using unencrypted grpc connection to %s - please consider using grpcs instead\n", uri)
}

// warnInvalidEnv is duplicated from pkg/cas, where the same helper is unexported.
func warnInvalidEnv(name, raw string, fallback any) {
	fmt.Fprintf(os.Stderr, "WARNING: ignoring invalid %s=%q, using %v\n", name, raw, fallback)
}

// WarnedURIs is a set of URIs that have already been warned about.
// It is protected by warnMutex, which must be held when accessing it.
var (
	WarnedURIs = make(map[string]struct{})
	warnMutex  sync.Mutex
)
