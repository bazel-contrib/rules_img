package ecr

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/aws"
	"github.com/docker/docker-credential-helpers/credentials"
)

type rt func(*http.Request) (*http.Response, error)

func (f rt) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestParseRegistry(t *testing.T) {
	for host, want := range map[string]registry{
		"123456789012.dkr.ecr.us-east-1.amazonaws.com":                  {id: "123456789012", region: "us-east-1", suffix: "amazonaws.com"},
		"https://123456789012.dkr.ecr-fips.us-gov-west-1.amazonaws.com": {id: "123456789012", region: "us-gov-west-1", fips: true, suffix: "amazonaws.com"},
		"123456789012.dkr-ecr.cn-north-1.on.amazonwebservices.com.cn":   {id: "123456789012", region: "cn-north-1", suffix: "on.amazonwebservices.com.cn"},
		"public.ecr.aws": {public: true, region: "us-east-1", suffix: "amazonaws.com"},
	} {
		got, err := parseRegistry(host)
		if err != nil || *got != want {
			t.Errorf("%s: got %+v, %v; want %+v", host, got, err, want)
		}
	}
	if _, err := parseRegistry("docker.io"); err == nil {
		t.Error("docker.io parsed as ECR")
	}
}

func TestHelperGet(t *testing.T) {
	token := base64.StdEncoding.EncodeToString([]byte("AWS:secret"))
	client := &http.Client{Transport: rt(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Host != "api.ecr.us-west-2.amazonaws.com" ||
			r.Header.Get("X-Amz-Target") != "AmazonEC2ContainerRegistry_V20150921.GetAuthorizationToken" ||
			!strings.Contains(string(body), `"123456789012"`) ||
			!strings.Contains(r.Header.Get("Authorization"), "/us-west-2/ecr/aws4_request") {
			t.Errorf("unexpected request: %s %v %s", r.URL, r.Header, body)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(
			`{"authorizationData":[{"authorizationToken":"` + token + `","expiresAt":1.7e9,"proxyEndpoint":"https://x"}]}`))}, nil
	})}
	h := NewHelper(nil)
	h.load = func(context.Context) (*aws.Config, error) {
		return &aws.Config{HTTPClient: client, Credentials: aws.StaticProvider(aws.Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"})}, nil
	}
	user, pass, err := h.Get("123456789012.dkr.ecr.us-west-2.amazonaws.com")
	if err != nil || user != "AWS" || pass != "secret" {
		t.Fatalf("got %q %q %v", user, pass, err)
	}
	if _, _, err := h.Get("ghcr.io"); !credentials.IsErrCredentialsNotFound(err) {
		t.Fatalf("ghcr.io: got %v, want not found", err)
	}
}
