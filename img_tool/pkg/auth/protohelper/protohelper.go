package protohelper

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	credhelper "github.com/bazel-contrib/rules_img/img_tool/pkg/auth/credential"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/grpcheaderinterceptor"
)

func Client(uri string, helper credhelper.Helper, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	opts = slices.Clone(opts)

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

	opts = append(opts, grpcheaderinterceptor.DialOptions(helper)...)

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
	fmt.Fprintf(os.Stderr, "WARNING: using unencrypted grpc connection to %s - please consider using grpcs instead", uri)
}

// WarnedURIs is a set of URIs that have already been warned about.
// It is protected by warnMutex, which must be held when accessing it.
var (
	WarnedURIs = make(map[string]struct{})
	warnMutex  sync.Mutex
)
