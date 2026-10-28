package s3

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/aws"
)

type rt func(*http.Request) (*http.Response, error)

func (f rt) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testConfig(rtf rt) *aws.Config {
	return &aws.Config{
		Region:      "us-east-1",
		HTTPClient:  &http.Client{Transport: rtf},
		Credentials: aws.StaticProvider(aws.Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"}),
	}
}

func TestHeadObject(t *testing.T) {
	c := New(testConfig(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "bkt.s3.us-east-1.amazonaws.com" || r.URL.Path != "/sha256/abc" || r.Method != "HEAD" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		if r.URL.Path == "/sha256/abc" && r.Header.Get("Authorization") == "" {
			t.Error("unsigned request")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Length": {"42"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}))
	if n, err := c.HeadObject(context.Background(), "bkt", "sha256/abc"); err != nil || n != 42 {
		t.Fatalf("got %d, %v", n, err)
	}
}

func TestHeadObjectNotFound(t *testing.T) {
	c := New(testConfig(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}))
	if _, err := c.HeadObject(context.Background(), "bkt", "k"); err != ErrNotFound {
		t.Fatalf("got %v", err)
	}
}

func TestPathStyleAddressing(t *testing.T) {
	c := New(&aws.Config{BaseEndpoint: "http://localhost:9000", Credentials: aws.StaticProvider(aws.Credentials{AccessKeyID: "a", SecretAccessKey: "b"})})
	got, err := c.PresignGetObject(context.Background(), "bkt", "sha256/abc", time.Minute)
	if err != nil || !strings.HasPrefix(got, "http://localhost:9000/bkt/sha256/abc?X-Amz-Algorithm=") {
		t.Fatalf("got %q, %v", got, err)
	}
	c = New(&aws.Config{Region: "us-east-1", Credentials: aws.StaticProvider(aws.Credentials{AccessKeyID: "a", SecretAccessKey: "b"})})
	got, _ = c.PresignGetObject(context.Background(), "my.dotted.bucket", "k", time.Minute)
	if !strings.HasPrefix(got, "https://s3.us-east-1.amazonaws.com/my.dotted.bucket/k?") {
		t.Fatalf("got %q", got)
	}
}
