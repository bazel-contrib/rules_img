package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// metadataCredentials is the JSON document served by IMDS, the ECS task
// credentials endpoint and EKS Pod Identity.
type metadataCredentials struct {
	Code            string
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string
	Token           string
	Expiration      time.Time
}

func (m metadataCredentials) credentials(source string) (Credentials, error) {
	if m.Code != "" && m.Code != "Success" {
		return Credentials{}, fmt.Errorf("%s returned code %q", source, m.Code)
	}
	return Credentials{
		AccessKeyID: m.AccessKeyID, SecretAccessKey: m.SecretAccessKey,
		SessionToken: m.Token, Expires: m.Expiration, Source: source,
	}, nil
}

func (c *Config) getJSON(ctx context.Context, url string, header http.Header, out any) error {
	resp, err := Do(ctx, c.httpClient(), func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		for k, vs := range header {
			req.Header[k] = vs
		}
		return req, nil
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// containerProvider reads credentials from the ECS / EKS Pod Identity endpoint
// described by AWS_CONTAINER_CREDENTIALS_RELATIVE_URI or _FULL_URI.
func (c *Config) containerProvider(env func(string) string) Provider {
	return ProviderFunc(func(ctx context.Context) (Credentials, error) {
		endpoint := env("AWS_CONTAINER_CREDENTIALS_FULL_URI")
		if rel := env("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"); rel != "" {
			endpoint = "http://169.254.170.2" + rel
		}
		if endpoint == "" {
			return Credentials{}, errors.New("no container credentials endpoint configured")
		}
		header := http.Header{}
		token := env("AWS_CONTAINER_AUTHORIZATION_TOKEN")
		if file := env("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"); file != "" {
			data, err := readFile(file)
			if err != nil {
				return Credentials{}, fmt.Errorf("read container authorization token: %w", err)
			}
			token = strings.TrimSpace(data)
		}
		if token != "" {
			header.Set("Authorization", token)
		}
		var m metadataCredentials
		if err := c.getJSON(ctx, endpoint, header, &m); err != nil {
			return Credentials{}, fmt.Errorf("container credentials: %w", err)
		}
		return m.credentials("container credentials")
	})
}

// imdsProvider reads role credentials from the EC2 instance metadata service,
// preferring IMDSv2 and falling back to IMDSv1 if token requests are refused.
func (c *Config) imdsProvider(env func(string) string) Provider {
	return ProviderFunc(func(ctx context.Context) (Credentials, error) {
		if strings.EqualFold(env("AWS_EC2_METADATA_DISABLED"), "true") {
			return Credentials{}, errors.New("EC2 instance metadata is disabled")
		}
		base := env("AWS_EC2_METADATA_SERVICE_ENDPOINT")
		if base == "" {
			base = "http://169.254.169.254"
		}
		base = strings.TrimSuffix(base, "/")
		// Off EC2 nothing answers; do not hang the caller for long.
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		header := http.Header{}
		resp, err := Do(ctx, c.httpClient(), func() (*http.Request, error) {
			req, err := http.NewRequest(http.MethodPut, base+"/latest/api/token", nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "21600")
			return req, nil
		})
		switch {
		case err == nil:
			token, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			header.Set("X-aws-ec2-metadata-token", string(token))
		case IsStatus(err, http.StatusForbidden), IsStatus(err, http.StatusNotFound), IsStatus(err, http.StatusMethodNotAllowed):
			// IMDSv2 unavailable; use IMDSv1.
		default:
			return Credentials{}, fmt.Errorf("EC2 instance metadata: %w", err)
		}

		role := env("AWS_EC2_INSTANCE_PROFILE_NAME")
		if role == "" {
			listResp, err := Do(ctx, c.httpClient(), func() (*http.Request, error) {
				req, err := http.NewRequest(http.MethodGet, base+"/latest/meta-data/iam/security-credentials/", nil)
				if err != nil {
					return nil, err
				}
				req.Header = header.Clone()
				return req, nil
			})
			if err != nil {
				return Credentials{}, fmt.Errorf("EC2 instance metadata: list roles: %w", err)
			}
			data, _ := io.ReadAll(io.LimitReader(listResp.Body, 4096))
			listResp.Body.Close()
			role = strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
			if role == "" {
				return Credentials{}, errors.New("EC2 instance metadata: no IAM role attached to the instance")
			}
		}
		var m metadataCredentials
		if err := c.getJSON(ctx, base+"/latest/meta-data/iam/security-credentials/"+role, header, &m); err != nil {
			return Credentials{}, fmt.Errorf("EC2 instance metadata: %w", err)
		}
		return m.credentials("EC2 instance metadata")
	})
}
