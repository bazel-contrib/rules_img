package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

func TestTransportErrorType(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"no error", nil, ""},
		{
			// net/http wraps everything a RoundTripper returns in a url.Error, so
			// the classifier has to unwrap.
			name: "connection refused through the url.Error wrapper",
			err:  &url.Error{Op: "Get", URL: "https://registry.test/v2/", Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}},
			want: errConnectionRefused,
		},
		{"connection reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, errConnectionReset},
		{"connection aborted", &net.OpError{Op: "read", Err: syscall.ECONNABORTED}, errConnectionReset},
		// A client that hung up mid-response is not an upstream problem, so it
		// does not share a bucket with a reset.
		{"broken pipe", &net.OpError{Op: "write", Err: syscall.EPIPE}, errBrokenPipe},
		{"host unreachable", &net.OpError{Op: "dial", Err: syscall.EHOSTUNREACH}, errHostUnreachable},
		{"network unreachable", &net.OpError{Op: "dial", Err: syscall.ENETUNREACH}, errNetworkUnreachable},
		{"out of local ports", &net.OpError{Op: "dial", Err: syscall.EADDRNOTAVAIL}, errLocalResourceExhausted},
		{"out of file descriptors", &os.SyscallError{Syscall: "socket", Err: syscall.EMFILE}, errLocalResourceExhausted},
		{"context deadline", fmt.Errorf("forwarding: %w", context.DeadlineExceeded), errTimeout},
		// A timeout is attributed to the stage that timed out: a connect that
		// never completed is a dropped packet, a read that did is a stalled
		// upstream, and a write that did is backpressure.
		{"connect timeout", &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}, errDialTimeout},
		{"i/o timeout", &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}, errReadTimeout},
		{"write timeout", &net.OpError{Op: "write", Err: os.ErrDeadlineExceeded}, errWriteTimeout},
		{"handshake timeout", &url.Error{Op: "Get", Err: errors.New("net/http: TLS handshake timeout")}, errTLSHandshakeTimeout},
		{"client went away", fmt.Errorf("copying: %w", context.Canceled), errClientCanceled},
		{"name does not exist", &net.DNSError{Err: "no such host", Name: "registry.test", IsNotFound: true}, errDNSNotFound},
		{"resolver did not answer", &net.DNSError{Err: "i/o timeout", Name: "registry.test", IsTimeout: true}, errDNSTimeout},
		{"resolution failed", &net.DNSError{Err: "server misbehaving", Name: "registry.test"}, errDNS},
		// Reaching the proxy is a different address to debug than reaching the
		// registry, so it keeps its own type whatever the underlying cause.
		{
			name: "proxy unreachable",
			err:  &url.Error{Op: "Get", Err: &net.OpError{Op: "proxyconnect", Net: "tcp", Err: syscall.ECONNREFUSED}},
			want: errProxyConnect,
		},
		{
			name: "untrusted certificate",
			err:  &url.Error{Op: "Get", Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}},
			want: errTLSCertificateUntrusted,
		},
		{
			name: "expired certificate",
			err:  &tls.CertificateVerificationError{Err: x509.CertificateInvalidError{Reason: x509.Expired}},
			want: errTLSCertificateExpired,
		},
		{
			name: "otherwise invalid certificate",
			err:  x509.CertificateInvalidError{Reason: x509.IncompatibleUsage},
			want: errTLSCertificate,
		},
		{"certificate for the wrong host", x509.HostnameError{Host: "registry.test"}, errTLSCertificateHostname},
		{"not a TLS server", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, errTLSPlaintext},
		{
			// What net/http substitutes for the record header error when the
			// bytes it saw are an HTTP response.
			name: "plain HTTP on an HTTPS port",
			err:  &url.Error{Op: "Get", Err: http.ErrSchemeMismatch},
			want: errTLSPlaintext,
		},
		{
			// The far end rejecting our client certificate: the mTLS credential
			// on the gateway-to-gateway hop, which used to look like a generic
			// network failure.
			name: "peer rejected our client certificate",
			err:  &url.Error{Op: "Get", Err: &net.OpError{Op: "remote error", Err: errors.New("tls: bad certificate")}},
			want: errTLSClientCertRejected,
		},
		{
			name: "peer requires a client certificate",
			err:  &net.OpError{Op: "remote error", Err: errors.New("tls: certificate required")},
			want: errTLSClientCertRejected,
		},
		{
			name: "handshake alert that is not about a certificate",
			err:  &net.OpError{Op: "remote error", Err: errors.New("tls: handshake failure")},
			want: errTLS,
		},
		{"TLS alert", tls.AlertError(80), errTLS},
		{"truncated body", fmt.Errorf("reading body: %w", io.ErrUnexpectedEOF), errUnexpectedEOF},
		{
			// The connection a forwarding gateway pins to its peer, taken away
			// by a rolling restart or a service mesh's max connection age.
			name: "peer sent GOAWAY",
			err:  errors.New("http2: server sent GOAWAY and closed the connection; LastStreamID=1, ErrCode=NO_ERROR, debug=\"\""),
			want: errHTTP2GoAway,
		},
		{
			name: "stream refused",
			err:  errors.New("stream error: stream ID 3; REFUSED_STREAM"),
			want: errHTTP2Stream,
		},
		{"generic network failure", &net.OpError{Op: "dial", Err: errors.New("boom")}, errNetwork},
		{"upstream 401", &transport.Error{StatusCode: http.StatusUnauthorized}, errUpstreamUnauthorized},
		{"upstream 403", &transport.Error{StatusCode: http.StatusForbidden}, errUpstreamForbidden},
		{"upstream 429", &transport.Error{StatusCode: http.StatusTooManyRequests}, errUpstreamRateLimited},
		{"upstream 500", &transport.Error{StatusCode: http.StatusInternalServerError}, errUpstreamServerError},
		{"upstream 400", &transport.Error{StatusCode: http.StatusBadRequest}, errUpstreamClientError},
		{
			// A 404 is not an error in general, but a token exchange or ping that
			// 404s did fail, so it must still be classified as something.
			name: "upstream 404 during the handshake",
			err:  &transport.Error{StatusCode: http.StatusNotFound},
			want: errUpstreamClientError,
		},
		{
			name: "redirect to a private address",
			err:  &url.Error{Op: "Get", Err: fmt.Errorf("%w to private or link-local address %q", errRefusedRedirect, "169.254.169.254")},
			want: errRedirectRefused,
		},
		{"redirect loop", &url.Error{Op: "Get", Err: errRedirectsStopped}, errTooManyRedirects},
		{"unrecognized", errors.New("something else"), errTypeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := transportErrorType(tc.err); got != tc.want {
				t.Errorf("transportErrorType(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestStatusErrorType(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusOK, ""},
		{http.StatusCreated, ""},
		{http.StatusAccepted, ""},
		{http.StatusTemporaryRedirect, ""},
		// Probing for content that does not exist is normal registry traffic.
		{http.StatusNotFound, ""},
		{http.StatusUnauthorized, errUpstreamUnauthorized},
		{http.StatusForbidden, errUpstreamForbidden},
		{http.StatusTooManyRequests, errUpstreamRateLimited},
		{http.StatusBadRequest, errUpstreamClientError},
		{http.StatusRequestedRangeNotSatisfiable, errUpstreamClientError},
		{http.StatusInternalServerError, errUpstreamServerError},
		{http.StatusServiceUnavailable, errUpstreamServerError},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			if got := statusErrorType(tc.status); got != tc.want {
				t.Errorf("statusErrorType(%d) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

func TestAuthAndTransferErrorTypes(t *testing.T) {
	// A failure the transport classifier recognizes keeps its specific type...
	if got := authErrorType(&transport.Error{StatusCode: http.StatusUnauthorized}); got != errUpstreamUnauthorized {
		t.Errorf("authErrorType(401) = %q, want %q", got, errUpstreamUnauthorized)
	}
	// ...and anything else is attributed to the step that failed.
	if got := authErrorType(errors.New("resolving credentials: keychain empty")); got != errUpstreamAuth {
		t.Errorf("authErrorType(keychain error) = %q, want %q", got, errUpstreamAuth)
	}
	if got := transferErrorType(&net.OpError{Op: "write", Err: syscall.ECONNRESET}); got != errConnectionReset {
		t.Errorf("transferErrorType(reset) = %q, want %q", got, errConnectionReset)
	}
	// A body that stopped short of its Content-Length says the content is
	// truncated, which an aborted transfer alone does not.
	if got := transferErrorType(io.ErrUnexpectedEOF); got != errUnexpectedEOF {
		t.Errorf("transferErrorType(unexpected EOF) = %q, want %q", got, errUnexpectedEOF)
	}
	if got := transferErrorType(errors.New("unexpected EOF")); got != errTransferAborted {
		t.Errorf("transferErrorType(EOF) = %q, want %q", got, errTransferAborted)
	}
}

// TestTLSErrorTypesAgainstRealHandshakes runs the TLS classifications against
// what crypto/tls and net/http actually produce, rather than against errors the
// test constructed. That is what makes them worth asserting: the failures below
// arrive as unexported types or as plain errors, so the classifier matches on a
// message or on a net.OpError's Op, and only a real handshake can confirm the
// match still holds.
func TestTLSErrorTypesAgainstRealHandshakes(t *testing.T) {
	serverCA := newTestCA(t, "server-ca")
	serverCertPEM, serverKeyPEM, _ := serverCA.issue(t, leafOptions{commonName: "gateway.test", dnsNames: []string{"gateway.test"}, server: true})
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatalf("loading server keypair: %v", err)
	}
	serverRoots := x509.NewCertPool()
	serverRoots.AddCert(serverCA.cert)

	// A client certificate from a CA the server does not trust: the mTLS
	// credential mismatch of the gateway-to-gateway hop.
	clientCA := newTestCA(t, "client-ca")
	clientCertPEM, clientKeyPEM, _ := clientCA.issue(t, leafOptions{commonName: "forwarder"})
	clientCert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	if err != nil {
		t.Fatalf("loading client keypair: %v", err)
	}

	for _, tc := range []struct {
		name string
		// serve handles the server side of one connection.
		serve func(conn net.Conn)
		// client is the configuration the gateway would dial with.
		client *tls.Config
		want   string
	}{
		{
			name: "the peer rejects our client certificate",
			serve: func(conn net.Conn) {
				// Requires a client certificate, trusts only its own CA.
				server := tls.Server(conn, &tls.Config{
					Certificates: []tls.Certificate{serverCert},
					ClientAuth:   tls.RequireAndVerifyClientCert,
					ClientCAs:    x509.NewCertPool(),
				})
				_ = server.Handshake()
			},
			client: &tls.Config{
				RootCAs:      serverRoots,
				ServerName:   "gateway.test",
				Certificates: []tls.Certificate{clientCert},
			},
			want: errTLSClientCertRejected,
		},
		{
			name: "the peer demands a client certificate we do not have",
			serve: func(conn net.Conn) {
				server := tls.Server(conn, &tls.Config{
					Certificates: []tls.Certificate{serverCert},
					ClientAuth:   tls.RequireAnyClientCert,
				})
				_ = server.Handshake()
			},
			client: &tls.Config{RootCAs: serverRoots, ServerName: "gateway.test"},
			want:   errTLSClientCertRejected,
		},
		{
			name: "we do not trust the peer's certificate",
			serve: func(conn net.Conn) {
				server := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{serverCert}})
				_ = server.Handshake()
			},
			client: &tls.Config{RootCAs: x509.NewCertPool(), ServerName: "gateway.test"},
			want:   errTLSCertificateUntrusted,
		},
		{
			name: "the peer's certificate is for another name",
			serve: func(conn net.Conn) {
				server := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{serverCert}})
				_ = server.Handshake()
			},
			client: &tls.Config{RootCAs: serverRoots, ServerName: "elsewhere.test"},
			want:   errTLSCertificateHostname,
		},
		{
			name: "the endpoint is not speaking TLS",
			serve: func(conn net.Conn) {
				// Read the ClientHello and answer it with a plain HTTP
				// response, the way an http:// service on an https:// port
				// would. The connection stays open: closing it here would
				// fail the client's write before it ever read the reply.
				_, _ = conn.Read(make([]byte, 1024))
				_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
			},
			client: &tls.Config{RootCAs: serverRoots, ServerName: "gateway.test"},
			want:   errTLSPlaintext,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, server := memPipe("tls")
			// The server side never closes: under TLS 1.3 the client's
			// handshake completes before the server has even seen its
			// certificate, so the rejection alert is read during the request
			// that follows. A close here would race that read and surface as
			// "use of closed network connection" instead.
			t.Cleanup(func() { _, _ = clientConn.Close(), server.Close() })
			go tc.serve(server)

			// Dial through an http.Client, so the error is wrapped exactly as it
			// would be in the gateway: net/http substitutes its own error for
			// some of these before the caller ever sees the tls package's.
			client := &http.Client{Transport: &http.Transport{
				DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					conn := tls.Client(clientConn, tc.client)
					if err := conn.HandshakeContext(ctx); err != nil {
						return nil, err
					}
					return conn, nil
				},
			}}
			resp, err := client.Get("https://gateway.test/v2/")
			if err == nil {
				resp.Body.Close()
				t.Fatal("handshake succeeded, want a failure to classify")
			}
			if got := transportErrorType(err); got != tc.want {
				t.Errorf("transportErrorType(%v) = %q, want %q", err, got, tc.want)
			}
		})
	}
}
