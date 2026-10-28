package aws

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func reply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

func envFrom(m map[string]string) LoadOption { return WithEnv(func(k string) string { return m[k] }) }

func TestEnvCredentials(t *testing.T) {
	cfg, err := LoadDefaultConfig(context.Background(), envFrom(map[string]string{
		"AWS_ACCESS_KEY_ID": "AK", "AWS_SECRET_ACCESS_KEY": "SK", "AWS_SESSION_TOKEN": "T", "AWS_REGION": "eu-west-1",
		"AWS_CONFIG_FILE": "/nonexistent", "AWS_SHARED_CREDENTIALS_FILE": "/nonexistent",
	}))
	if err != nil {
		t.Fatal(err)
	}
	c, err := cfg.Credentials.Retrieve(context.Background())
	if err != nil || c.AccessKeyID != "AK" || c.SessionToken != "T" || cfg.Region != "eu-west-1" {
		t.Fatalf("got %+v region %q err %v", c, cfg.Region, err)
	}
}

func TestAssumeRoleViaSourceProfile(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config")
	os.WriteFile(cfgFile, []byte("[profile app]\nrole_arn=arn:aws:iam::123:role/r\nsource_profile=base\nregion=us-east-2\n[profile base]\naws_access_key_id=BASEAK\naws_secret_access_key=BASESK\n"), 0o600)
	var calls int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if r.URL.Host != "sts.us-east-2.amazonaws.com" || !strings.Contains(string(body), "Action=AssumeRole") ||
			!strings.Contains(string(body), "RoleArn=arn%3Aaws%3Aiam%3A%3A123%3Arole%2Fr") {
			t.Errorf("unexpected request %s %s", r.URL, body)
		}
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=BASEAK/") {
			t.Errorf("not signed with base creds: %q", r.Header.Get("Authorization"))
		}
		return reply(200, `<AssumeRoleResponse><AssumeRoleResult><Credentials><AccessKeyId>ASIA</AccessKeyId><SecretAccessKey>S</SecretAccessKey><SessionToken>TOK</SessionToken><Expiration>2999-01-01T00:00:00Z</Expiration></Credentials></AssumeRoleResult></AssumeRoleResponse>`), nil
	})}
	cfg, err := LoadDefaultConfig(context.Background(), WithHTTPClient(client), envFrom(map[string]string{
		"AWS_CONFIG_FILE": cfgFile, "AWS_SHARED_CREDENTIALS_FILE": "/nonexistent", "AWS_PROFILE": "app",
	}))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		c, err := cfg.Credentials.Retrieve(context.Background())
		if err != nil || c.AccessKeyID != "ASIA" || c.SessionToken != "TOK" {
			t.Fatalf("got %+v err %v", c, err)
		}
	}
	if calls != 1 {
		t.Errorf("STS called %d times, want 1 (cached)", calls)
	}
}

func TestIMDSv2(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == "PUT":
			return reply(200, "tok"), nil
		case r.Header.Get("X-aws-ec2-metadata-token") != "tok":
			return reply(401, ""), nil
		case strings.HasSuffix(r.URL.Path, "/security-credentials/"):
			return reply(200, "myrole\n"), nil
		case strings.HasSuffix(r.URL.Path, "/security-credentials/myrole"):
			return reply(200, `{"Code":"Success","AccessKeyId":"AK","SecretAccessKey":"SK","Token":"T","Expiration":"2999-01-01T00:00:00Z"}`), nil
		}
		return reply(404, ""), nil
	})}
	cfg, err := LoadDefaultConfig(context.Background(), WithHTTPClient(client), envFrom(map[string]string{
		"AWS_CONFIG_FILE": "/nonexistent", "AWS_SHARED_CREDENTIALS_FILE": "/nonexistent",
	}))
	if err != nil {
		t.Fatal(err)
	}
	c, err := cfg.Credentials.Retrieve(context.Background())
	if err != nil || c.AccessKeyID != "AK" || c.SessionToken != "T" {
		t.Fatalf("got %+v err %v", c, err)
	}
}

func TestMissingProfileIsAnError(t *testing.T) {
	_, err := LoadDefaultConfig(context.Background(), WithProfile("nope"), envFrom(map[string]string{
		"AWS_CONFIG_FILE": "/nonexistent", "AWS_SHARED_CREDENTIALS_FILE": "/nonexistent",
	}))
	if err == nil {
		t.Fatal("want error")
	}
}
