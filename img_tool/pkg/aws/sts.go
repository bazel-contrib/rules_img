package aws

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type stsCredentials struct {
	AccessKeyID     string    `xml:"AccessKeyId"`
	SecretAccessKey string    `xml:"SecretAccessKey"`
	SessionToken    string    `xml:"SessionToken"`
	Expiration      time.Time `xml:"Expiration"`
}

type stsResponse struct {
	AssumeRole struct {
		Credentials stsCredentials
	} `xml:"AssumeRoleResult"`
	WebIdentity struct {
		Credentials stsCredentials
	} `xml:"AssumeRoleWithWebIdentityResult"`
}

type stsError struct {
	Error struct {
		Code    string
		Message string
	}
}

// stsCall performs one STS Query API call. If signWith has keys the request is
// signed with them; AssumeRoleWithWebIdentity is sent unsigned.
func (c *Config) stsCall(ctx context.Context, params url.Values, signWith Credentials) (Credentials, error) {
	region := c.Region
	host := "sts.amazonaws.com"
	signRegion := "us-east-1"
	if region != "" {
		host = "sts." + region + "." + DNSSuffix(region)
		signRegion = region
	}
	params.Set("Version", "2011-06-15")
	body := params.Encode()
	sum := sha256Hex(body)

	resp, err := Do(ctx, c.httpClient(), func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, "https://"+host+"/", strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
		if signWith.HasKeys() {
			if err := Sign(req, signWith, SignOptions{Service: "sts", Region: signRegion, PayloadHash: sum}); err != nil {
				return nil, err
			}
		}
		return req, nil
	})
	if err != nil {
		var re *ResponseError
		if asResponseError(err, &re) {
			var e stsError
			if xml.Unmarshal([]byte(re.Body), &e) == nil && e.Error.Code != "" {
				return Credentials{}, fmt.Errorf("sts %s: %s: %s", params.Get("Action"), e.Error.Code, e.Error.Message)
			}
		}
		return Credentials{}, fmt.Errorf("sts %s: %w", params.Get("Action"), err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Credentials{}, err
	}
	var out stsResponse
	if err := xml.Unmarshal(data, &out); err != nil {
		return Credentials{}, fmt.Errorf("sts %s: decode response: %w", params.Get("Action"), err)
	}
	sc := out.AssumeRole.Credentials
	if sc.AccessKeyID == "" {
		sc = out.WebIdentity.Credentials
	}
	return Credentials{
		AccessKeyID: sc.AccessKeyID, SecretAccessKey: sc.SecretAccessKey,
		SessionToken: sc.SessionToken, Expires: sc.Expiration, Source: "sts " + params.Get("Action"),
	}, nil
}

type assumeRole struct {
	cfg         *Config
	base        Provider
	roleARN     string
	sessionName string
	externalID  string
	duration    time.Duration
}

func (a *assumeRole) Retrieve(ctx context.Context) (Credentials, error) {
	base, err := a.base.Retrieve(ctx)
	if err != nil {
		return Credentials{}, err
	}
	params := url.Values{"Action": {"AssumeRole"}, "RoleArn": {a.roleARN}, "RoleSessionName": {sessionName(a.sessionName)}}
	if a.externalID != "" {
		params.Set("ExternalId", a.externalID)
	}
	if a.duration > 0 {
		params.Set("DurationSeconds", strconv.Itoa(int(a.duration/time.Second)))
	}
	return a.cfg.stsCall(ctx, params, base)
}

type webIdentity struct {
	cfg         *Config
	tokenFile   string
	roleARN     string
	sessionName string
}

func (w *webIdentity) Retrieve(ctx context.Context) (Credentials, error) {
	token, err := readFile(w.tokenFile)
	if err != nil {
		return Credentials{}, fmt.Errorf("read web identity token: %w", err)
	}
	params := url.Values{
		"Action": {"AssumeRoleWithWebIdentity"}, "RoleArn": {w.roleARN},
		"RoleSessionName": {sessionName(w.sessionName)}, "WebIdentityToken": {strings.TrimSpace(token)},
	}
	return w.cfg.stsCall(ctx, params, Credentials{})
}

func sessionName(name string) string {
	if name != "" {
		return name
	}
	return "rules_img-" + strconv.FormatInt(time.Now().UnixNano(), 10)
}
