package aws

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The "get-vanilla" case of the AWS Signature Version 4 test suite.
func TestSignGetVanilla(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://example.amazonaws.com/", nil)
	creds := Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	err := Sign(req, creds, SignOptions{
		Service: "service", Region: "us-east-1", PayloadHash: EmptyPayloadHash,
		Time: time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

func TestPresignShape(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://bucket.s3.us-east-1.amazonaws.com/sha256/ab cd", nil)
	creds := Credentials{AccessKeyID: "AK", SecretAccessKey: "SK", SessionToken: "tok"}
	err := Sign(req, creds, SignOptions{Service: "s3", Region: "us-east-1", S3: true, Expires: time.Hour,
		Time: time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	u := req.URL.String()
	for _, want := range []string{"/sha256/ab%20cd?", "X-Amz-Expires=3600", "X-Amz-Security-Token=tok",
		"X-Amz-Credential=AK%2F20150830%2Fus-east-1%2Fs3%2Faws4_request", "X-Amz-Signature="} {
		if !strings.Contains(u, want) {
			t.Errorf("presigned URL %q lacks %q", u, want)
		}
	}
}
