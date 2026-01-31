package protohelper

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"slices"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	credhelper "github.com/bazel-contrib/rules_img/img_tool/pkg/auth/credential"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/grpcheaderinterceptor"
)

func Client(uri string, helper credhelper.Helper, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	opts = slices.Clone(opts)

	parsed, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("invalid uri for grpc: %s: %w", uri, err)
	}

	switch parsed.Scheme {
	case "grpc":
		// unencrypted grpc
		warnUnencryptedGRPC(uri)
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	case "grpcs":
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(nil)))
	default:
		return nil, fmt.Errorf("unsupported scheme for grpc: %s", parsed.Scheme)
	}

	// If userinfo is present, add Basic auth credentials
	if parsed.User != nil && parsed.User.String() != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(basicAuthFromUserinfo(parsed.User)))
	}

	target := fmt.Sprintf("dns:%s", parsed.Host)

	opts = append(opts, grpcheaderinterceptor.DialOptions(helper)...)

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
