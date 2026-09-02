package protohelper

import (
	"context"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"

	bytestream_proto "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/credential"
)

func TestBasicAuthCredentials(t *testing.T) {
	tests := []struct {
		name             string
		username         string
		password         string
		expectedAuth     string
		expectedEncoding string
	}{
		{
			name:             "simple credentials",
			username:         "user",
			password:         "pass",
			expectedAuth:     "Basic dXNlcjpwYXNz",
			expectedEncoding: "user:pass",
		},
		{
			name:             "empty password",
			username:         "user",
			password:         "",
			expectedAuth:     "Basic dXNlcjo=",
			expectedEncoding: "user:",
		},
		{
			name:             "special characters",
			username:         "bazel",
			password:         "secret$key!",
			expectedAuth:     "Basic YmF6ZWw6c2VjcmV0JGtleSE=",
			expectedEncoding: "bazel:secret$key!",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			creds := &basicAuthCredentials{
				username: tt.username,
				password: tt.password,
			}

			metadata, err := creds.GetRequestMetadata(context.Background())
			if err != nil {
				t.Fatalf("GetRequestMetadata returned error: %v", err)
			}

			auth, ok := metadata["authorization"]
			if !ok {
				t.Fatal("authorization header not found in metadata")
			}

			if auth != tt.expectedAuth {
				t.Errorf("expected authorization %q, got %q", tt.expectedAuth, auth)
			}

			if creds.RequireTransportSecurity() {
				t.Error("RequireTransportSecurity should return false")
			}
		})
	}
}

func TestBasicAuthFromUserinfo(t *testing.T) {
	tests := []struct {
		name         string
		url          string
		wantUsername string
		wantPassword string
	}{
		{
			name:         "username and password",
			url:          "grpc://user:pass@host:9092",
			wantUsername: "user",
			wantPassword: "pass",
		},
		{
			name:         "username only",
			url:          "grpc://user@host:9092",
			wantUsername: "user",
			wantPassword: "",
		},
		{
			name:         "url-encoded password",
			url:          "grpc://bazel:secret%24key@host:9092",
			wantUsername: "bazel",
			wantPassword: "secret$key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := url.Parse(tt.url)
			if err != nil {
				t.Fatalf("failed to parse URL: %v", err)
			}

			creds := basicAuthFromUserinfo(parsed.User)

			if creds.username != tt.wantUsername {
				t.Errorf("expected username %q, got %q", tt.wantUsername, creds.username)
			}
			if creds.password != tt.wantPassword {
				t.Errorf("expected password %q, got %q", tt.wantPassword, creds.password)
			}
		})
	}
}

func TestParseGRPCURL(t *testing.T) {
	tests := []struct {
		name       string
		url        string
		wantHost   string
		wantScheme string
		hasUser    bool
	}{
		{
			name:       "simple grpc URL",
			url:        "grpc://host.example.com:9092",
			wantHost:   "host.example.com:9092",
			wantScheme: "grpc",
			hasUser:    false,
		},
		{
			name:       "grpcs URL",
			url:        "grpcs://host.example.com:443",
			wantHost:   "host.example.com:443",
			wantScheme: "grpcs",
			hasUser:    false,
		},
		{
			name:       "grpc URL with userinfo",
			url:        "grpc://bazel:secret@host.amazonaws.com:9092",
			wantHost:   "host.amazonaws.com:9092",
			wantScheme: "grpc",
			hasUser:    true,
		},
		{
			name:       "grpcs URL with userinfo",
			url:        "grpcs://user:pass@host.example.com:443",
			wantHost:   "host.example.com:443",
			wantScheme: "grpcs",
			hasUser:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := url.Parse(tt.url)
			if err != nil {
				t.Fatalf("failed to parse URL: %v", err)
			}

			if parsed.Host != tt.wantHost {
				t.Errorf("expected host %q, got %q", tt.wantHost, parsed.Host)
			}
			if parsed.Scheme != tt.wantScheme {
				t.Errorf("expected scheme %q, got %q", tt.wantScheme, parsed.Scheme)
			}

			hasUser := parsed.User != nil && parsed.User.String() != ""
			if hasUser != tt.hasUser {
				t.Errorf("expected hasUser=%v, got %v", tt.hasUser, hasUser)
			}
		})
	}
}

func TestClientTarget(t *testing.T) {
	tests := []struct {
		name       string
		uri        string
		wantTarget string
	}{
		{
			name:       "grpc endpoint",
			uri:        "grpc://example.com:9092",
			wantTarget: "dns:example.com:9092",
		},
		{
			name:       "grpcs endpoint",
			uri:        "grpcs://example.com:443",
			wantTarget: "dns:example.com:443",
		},
		{
			name:       "unix endpoint",
			uri:        "unix:///mnt/ephemeral/buildbarn/.cache/bb_clientd/grpc",
			wantTarget: "unix:///mnt/ephemeral/buildbarn/.cache/bb_clientd/grpc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := Client(tt.uri, credential.NopHelper())
			if err != nil {
				t.Fatalf("Client(%q) returned error: %v", tt.uri, err)
			}
			defer conn.Close()

			if got := conn.Target(); got != tt.wantTarget {
				t.Fatalf("Client(%q).Target() = %q, want %q", tt.uri, got, tt.wantTarget)
			}
		})
	}
}

func TestMaxRecvMsgSizeFromEnv(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		unset    bool
		want     int
		wantWarn bool
	}{
		{
			name:  "unset",
			unset: true,
			want:  defaultMaxRecvMsgSize,
		},
		{
			name:  "empty",
			value: "",
			want:  defaultMaxRecvMsgSize,
		},
		{
			name:  "valid size",
			value: "8388608",
			want:  8388608,
		},
		{
			name:  "surrounding whitespace",
			value: "  8388608  ",
			want:  8388608,
		},
		{
			// The floor itself is honored: it is what grpc-go would have applied anyway.
			name:  "exactly the floor",
			value: "4194304",
			want:  minMaxRecvMsgSize,
		},
		{
			name:     "unparseable",
			value:    "lots",
			want:     defaultMaxRecvMsgSize,
			wantWarn: true,
		},
		{
			name:     "zero",
			value:    "0",
			want:     defaultMaxRecvMsgSize,
			wantWarn: true,
		},
		{
			name:     "negative",
			value:    "-1",
			want:     defaultMaxRecvMsgSize,
			wantWarn: true,
		},
		{
			name:     "below the floor",
			value:    "1024",
			want:     defaultMaxRecvMsgSize,
			wantWarn: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// t.Setenv registers the restore, so unsetting after it exercises a
			// genuinely absent variable and still leaves the environment clean.
			t.Setenv(EnvMaxRecvMsgSize, tt.value)
			if tt.unset {
				if err := os.Unsetenv(EnvMaxRecvMsgSize); err != nil {
					t.Fatalf("unsetting %s: %v", EnvMaxRecvMsgSize, err)
				}
			}
			stderr := captureStderr(t)

			// Not the sync.OnceValue wrapper: it would parse the environment once for
			// the whole test binary.
			if got := maxRecvMsgSizeFromEnv(); got != tt.want {
				t.Errorf("maxRecvMsgSizeFromEnv() = %d, want %d", got, tt.want)
			}

			warned := strings.Contains(stderr.String(), EnvMaxRecvMsgSize)
			if warned != tt.wantWarn {
				t.Errorf("warned = %v, want %v (stderr: %q)", warned, tt.wantWarn, stderr.String())
			}
		})
	}
}

// bigReadResponseServer answers any ByteStream.Read with a single response of a fixed
// size, leaving the client's receive limit as the only thing deciding whether it succeeds.
type bigReadResponseServer struct {
	bytestream_proto.UnimplementedByteStreamServer
	payload []byte
}

func (s *bigReadResponseServer) Read(_ *bytestream_proto.ReadRequest, stream bytestream_proto.ByteStream_ReadServer) error {
	return stream.Send(&bytestream_proto.ReadResponse{Data: s.payload})
}

func serveBigReadResponses(t *testing.T, payloadSize int) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening on loopback: %v", err)
	}
	server := grpc.NewServer()
	bytestream_proto.RegisterByteStreamServer(server, &bigReadResponseServer{payload: make([]byte, payloadSize)})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

// readOneResponse reports the size of the first ReadResponse, not of the whole blob.
func readOneResponse(conn *grpc.ClientConn) (int, error) {
	stream, err := bytestream_proto.NewByteStreamClient(conn).Read(
		context.Background(),
		&bytestream_proto.ReadRequest{ResourceName: "blobs/test/1"},
	)
	if err != nil {
		return 0, err
	}
	response, err := stream.Recv()
	if err != nil {
		return 0, err
	}
	return len(response.Data), nil
}

// TestClientAcceptsLargeResponses covers the dial option itself, which the parsing test
// above does not reach.
func TestClientAcceptsLargeResponses(t *testing.T) {
	// Above grpc-go's 4 MiB default, and above the 4194309-byte response in #720.
	const payloadSize = 5 * 1024 * 1024
	address := serveBigReadResponses(t, payloadSize)

	t.Run("Client raises the limit", func(t *testing.T) {
		conn, err := Client("grpc://"+address, credential.NopHelper())
		if err != nil {
			t.Fatalf("Client returned error: %v", err)
		}
		defer conn.Close()

		got, err := readOneResponse(conn)
		if err != nil {
			t.Fatalf("reading a %d byte response: %v", payloadSize, err)
		}
		if got != payloadSize {
			t.Errorf("received %d bytes, want %d", got, payloadSize)
		}
	})

	t.Run("an unconfigured connection does not", func(t *testing.T) {
		// Confirms the server really does send something a default client rejects, so
		// the case above is not passing for an unrelated reason.
		conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatalf("grpc.NewClient returned error: %v", err)
		}
		defer conn.Close()

		if _, err := readOneResponse(conn); status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("error = %v (code %s), want %s", err, status.Code(err), codes.ResourceExhausted)
		}
	})

	t.Run("a caller's own limit still wins", func(t *testing.T) {
		// The dial option in [Client] is prepended so this holds.
		conn, err := Client("grpc://"+address, credential.NopHelper(),
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(1024)))
		if err != nil {
			t.Fatalf("Client returned error: %v", err)
		}
		defer conn.Close()

		if _, err := readOneResponse(conn); status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("error = %v (code %s), want %s", err, status.Code(err), codes.ResourceExhausted)
		}
	})
}

// captureStderr redirects stderr to a file for the duration of the test. It swaps the
// process-wide os.Stderr, so no test in this package may call t.Parallel while holding one.
func captureStderr(t *testing.T) *stderrCapture {
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
	return &stderrCapture{t: t, file: file}
}

type stderrCapture struct {
	t    *testing.T
	file *os.File
}

func (c *stderrCapture) String() string {
	c.t.Helper()
	data, err := os.ReadFile(c.file.Name())
	if err != nil {
		c.t.Fatalf("reading stderr capture: %v", err)
	}
	return string(data)
}
