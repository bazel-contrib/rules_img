// Package s3 implements the two S3 operations the img tool needs: probing an
// object with HEAD and presigning a GET URL.
package s3

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/aws"
)

// ErrNotFound is returned by HeadObject when the object does not exist.
var ErrNotFound = errors.New("s3: object not found")

// Client talks to S3.
type Client struct {
	cfg *aws.Config
}

// New returns a client using cfg. cfg.Region is required unless
// cfg.BaseEndpoint is set.
func New(cfg *aws.Config) *Client { return &Client{cfg: cfg} }

var dnsLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

// objectURL builds the URL of an object. Against AWS the bucket goes in the
// host name when it can; with a custom endpoint, or for bucket names that are
// not valid DNS labels, path-style addressing is used.
func (c *Client) objectURL(bucket, key string) (*url.URL, error) {
	if bucket == "" || key == "" {
		return nil, errors.New("s3: bucket and key are required")
	}
	var u *url.URL
	if c.cfg.BaseEndpoint != "" {
		base, err := url.Parse(c.cfg.BaseEndpoint)
		if err != nil || base.Host == "" {
			return nil, fmt.Errorf("s3: invalid endpoint %q", c.cfg.BaseEndpoint)
		}
		u = &url.URL{Scheme: base.Scheme, Host: base.Host, Path: strings.TrimSuffix(base.Path, "/") + "/" + bucket + "/" + key}
	} else {
		if c.cfg.Region == "" {
			return nil, errors.New("s3: missing region (set AWS_REGION or a region in the shared config)")
		}
		host := "s3." + c.cfg.Region + "." + aws.DNSSuffix(c.cfg.Region)
		if dnsLabel.MatchString(bucket) && !strings.Contains(bucket, "--x-s3") && net.ParseIP(bucket) == nil {
			u = &url.URL{Scheme: "https", Host: bucket + "." + host, Path: "/" + key}
		} else {
			u = &url.URL{Scheme: "https", Host: host, Path: "/" + bucket + "/" + key}
		}
	}
	return u, nil
}

func (c *Client) signRegion() string {
	if c.cfg.Region == "" {
		return "us-east-1"
	}
	return c.cfg.Region
}

// HeadObject returns the size of an object, or [ErrNotFound].
func (c *Client) HeadObject(ctx context.Context, bucket, key string) (int64, error) {
	u, err := c.objectURL(bucket, key)
	if err != nil {
		return 0, err
	}
	creds, err := c.cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return 0, err
	}
	client := c.cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := aws.Do(ctx, client, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodHead, u.String(), nil)
		if err != nil {
			return nil, err
		}
		return req, aws.Sign(req, creds, aws.SignOptions{
			Service: "s3", Region: c.signRegion(), PayloadHash: aws.EmptyPayloadHash, S3: true,
		})
	})
	if err != nil {
		if aws.IsStatus(err, http.StatusNotFound) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("s3 HeadObject: %w", err)
	}
	defer resp.Body.Close()
	cl := resp.Header.Get("Content-Length")
	if cl == "" {
		return 0, errors.New("s3 HeadObject: response has no Content-Length")
	}
	return strconv.ParseInt(cl, 10, 64)
}

// PresignGetObject returns a URL that grants read access to the object for the
// given duration (at most 7 days, and no longer than the credentials live).
func (c *Client) PresignGetObject(ctx context.Context, bucket, key string, expires time.Duration) (string, error) {
	u, err := c.objectURL(bucket, key)
	if err != nil {
		return "", err
	}
	creds, err := c.cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	if err := aws.Sign(req, creds, aws.SignOptions{Service: "s3", Region: c.signRegion(), S3: true, Expires: expires}); err != nil {
		return "", err
	}
	return req.URL.String(), nil
}
