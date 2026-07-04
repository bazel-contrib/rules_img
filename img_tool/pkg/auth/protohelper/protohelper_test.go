package protohelper

import (
	"context"
	"net/url"
	"testing"

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
