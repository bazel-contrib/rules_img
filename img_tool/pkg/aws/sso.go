package aws

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// ssoProvider resolves credentials for an IAM Identity Center profile from the
// token that `aws sso login` cached on disk.
type ssoProvider struct {
	cfg       *Config
	session   string // sso-session name; empty for legacy profiles
	startURL  string
	region    string
	accountID string
	roleName  string
}

func (s *ssoProvider) cachePath() string {
	key := s.startURL
	if s.session != "" {
		key = s.session
	}
	sum := sha1.Sum([]byte(key))
	return filepath.Join(awsDir(), "sso", "cache", hex.EncodeToString(sum[:])+".json")
}

func (s *ssoProvider) Retrieve(ctx context.Context) (Credentials, error) {
	token, err := s.accessToken(ctx)
	if err != nil {
		return Credentials{}, err
	}
	endpoint := "https://portal.sso." + s.region + "." + DNSSuffix(s.region) + "/federation/credentials?" +
		url.Values{"account_id": {s.accountID}, "role_name": {s.roleName}}.Encode()
	var out struct {
		RoleCredentials struct {
			AccessKeyID     string `json:"accessKeyId"`
			SecretAccessKey string `json:"secretAccessKey"`
			SessionToken    string `json:"sessionToken"`
			Expiration      int64  `json:"expiration"` // milliseconds since epoch
		} `json:"roleCredentials"`
	}
	if err := s.cfg.getJSON(ctx, endpoint, http.Header{"X-Amz-Sso_bearer_token": {token}}, &out); err != nil {
		return Credentials{}, fmt.Errorf("sso GetRoleCredentials: %w", err)
	}
	rc := out.RoleCredentials
	return Credentials{
		AccessKeyID: rc.AccessKeyID, SecretAccessKey: rc.SecretAccessKey, SessionToken: rc.SessionToken,
		Expires: time.UnixMilli(rc.Expiration), Source: "sso",
	}, nil
}

// accessToken returns a valid cached access token, refreshing it when the
// cache entry carries the client registration needed to do so.
func (s *ssoProvider) accessToken(ctx context.Context) (string, error) {
	path := s.cachePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("sso: no cached token (run `aws sso login`): %w", err)
	}
	var entry map[string]any
	if err := json.Unmarshal(data, &entry); err != nil {
		return "", fmt.Errorf("sso: parse %s: %w", path, err)
	}
	str := func(k string) string { v, _ := entry[k].(string); return v }
	expiresAt, _ := time.Parse(time.RFC3339, str("expiresAt"))
	if time.Now().Add(expiryWindow).Before(expiresAt) {
		return str("accessToken"), nil
	}

	if str("refreshToken") == "" || str("clientId") == "" || str("clientSecret") == "" {
		return "", errors.New("sso: cached token expired (run `aws sso login`)")
	}
	region := s.region
	if r := str("region"); r != "" {
		region = r
	}
	body, _ := json.Marshal(map[string]string{
		"clientId": str("clientId"), "clientSecret": str("clientSecret"),
		"grantType": "refresh_token", "refreshToken": str("refreshToken"),
	})
	resp, err := Do(ctx, s.cfg.httpClient(), func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, "https://oidc."+region+"."+DNSSuffix(region)+"/token", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	})
	if err != nil {
		return "", fmt.Errorf("sso: refresh token (run `aws sso login`): %w", err)
	}
	defer resp.Body.Close()
	var refreshed struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&refreshed); err != nil || refreshed.AccessToken == "" {
		return "", errors.New("sso: refresh token: unexpected response")
	}
	entry["accessToken"] = refreshed.AccessToken
	entry["expiresAt"] = time.Now().UTC().Add(time.Duration(refreshed.ExpiresIn) * time.Second).Format(time.RFC3339)
	if refreshed.RefreshToken != "" {
		entry["refreshToken"] = refreshed.RefreshToken
	}
	// Persisting is best effort; the refreshed token is valid regardless.
	if updated, err := json.Marshal(entry); err == nil {
		_ = os.WriteFile(path, updated, 0o600)
	}
	return refreshed.AccessToken, nil
}
