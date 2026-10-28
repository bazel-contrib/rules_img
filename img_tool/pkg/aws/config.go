package aws

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

// Config carries what is needed to talk to AWS services.
type Config struct {
	// Region is the default region for service calls.
	Region string
	// Credentials resolves the credentials used to sign requests. Retrieval is
	// lazy and cached.
	Credentials Provider
	// HTTPClient defaults to a client with a 30 second timeout.
	HTTPClient *http.Client
	// BaseEndpoint overrides the endpoint of the S3 API (for example to point
	// at an S3-compatible store).
	BaseEndpoint string
}

func (c *Config) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return defaultHTTPClient
}

var defaultHTTPClient = &http.Client{Timeout: 30 * time.Second}

// WithRegion returns a copy of c using region.
func (c *Config) WithRegion(region string) *Config {
	cp := *c
	cp.Region = region
	return &cp
}

// LoadOption customizes [LoadDefaultConfig].
type LoadOption func(*loadOptions)

type loadOptions struct {
	region       string
	profile      string
	baseEndpoint string
	httpClient   *http.Client
	credentials  Provider
	env          func(string) string
}

func WithRegion(region string) LoadOption { return func(o *loadOptions) { o.region = region } }

// WithProfile selects a shared config profile. Unlike AWS_PROFILE it also takes
// precedence over credentials in the environment.
func WithProfile(profile string) LoadOption { return func(o *loadOptions) { o.profile = profile } }

func WithBaseEndpoint(endpoint string) LoadOption {
	return func(o *loadOptions) { o.baseEndpoint = endpoint }
}

func WithHTTPClient(c *http.Client) LoadOption { return func(o *loadOptions) { o.httpClient = c } }

// WithCredentials bypasses credential resolution.
func WithCredentials(p Provider) LoadOption { return func(o *loadOptions) { o.credentials = p } }

// WithEnv replaces os.Getenv, for tests.
func WithEnv(env func(string) string) LoadOption { return func(o *loadOptions) { o.env = env } }

// LoadDefaultConfig builds a [Config] from the environment and the shared AWS
// config files. No credentials are fetched until they are first needed.
func LoadDefaultConfig(_ context.Context, opts ...LoadOption) (*Config, error) {
	var o loadOptions
	for _, opt := range opts {
		opt(&o)
	}
	if o.env == nil {
		o.env = os.Getenv
	}
	env := o.env

	files, err := loadSharedFiles(env)
	if err != nil {
		return nil, fmt.Errorf("aws: load shared config: %w", err)
	}
	cfg := &Config{Region: o.region, HTTPClient: o.httpClient, BaseEndpoint: o.baseEndpoint}

	explicitProfile := o.profile != ""
	profileName := o.profile
	if profileName == "" {
		profileName = env("AWS_PROFILE")
		explicitProfile = profileName != ""
	}
	if profileName == "" {
		profileName = "default"
	}
	profile := files.profiles[profileName]
	if profile == nil && explicitProfile {
		return nil, fmt.Errorf("aws: profile %q not found in shared config", profileName)
	}

	if cfg.Region == "" {
		cfg.Region = firstNonEmpty(env("AWS_REGION"), env("AWS_DEFAULT_REGION"), profile["region"])
	}
	if cfg.BaseEndpoint == "" {
		cfg.BaseEndpoint = firstNonEmpty(env("AWS_ENDPOINT_URL_S3"), env("AWS_ENDPOINT_URL"))
	}

	if o.credentials != nil {
		cfg.Credentials = o.credentials
		return cfg, nil
	}
	r := &resolver{cfg: cfg, files: files, env: env}
	provider, err := r.defaultChain(profileName, o.profile != "", profile)
	if err != nil {
		return nil, err
	}
	cfg.Credentials = NewCachedProvider(provider)
	return cfg, nil
}

type resolver struct {
	cfg   *Config
	files *sharedFiles
	env   func(string) string
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// profileHasCredentialSettings reports whether a profile configures any way of
// obtaining credentials.
func profileHasCredentialSettings(p map[string]string) bool {
	for _, k := range []string{"aws_access_key_id", "role_arn", "credential_process", "sso_session", "sso_start_url", "web_identity_token_file"} {
		if p[k] != "" {
			return true
		}
	}
	return false
}

// defaultChain mirrors the AWS SDK's default credential provider chain.
func (r *resolver) defaultChain(profileName string, optionProfile bool, profile map[string]string) (Provider, error) {
	env := r.env
	envCreds := Credentials{
		AccessKeyID:     firstNonEmpty(env("AWS_ACCESS_KEY_ID"), env("AWS_ACCESS_KEY")),
		SecretAccessKey: firstNonEmpty(env("AWS_SECRET_ACCESS_KEY"), env("AWS_SECRET_KEY")),
		SessionToken:    env("AWS_SESSION_TOKEN"),
		Source:          "environment",
	}
	if envCreds.HasKeys() && !optionProfile {
		return StaticProvider(envCreds), nil
	}
	if profileHasCredentialSettings(profile) || optionProfile || env("AWS_PROFILE") != "" {
		return r.profileProvider(profileName, map[string]bool{})
	}
	if file, arn := env("AWS_WEB_IDENTITY_TOKEN_FILE"), env("AWS_ROLE_ARN"); file != "" && arn != "" {
		return &webIdentity{cfg: r.cfg, tokenFile: file, roleARN: arn, sessionName: env("AWS_ROLE_SESSION_NAME")}, nil
	}
	if env("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") != "" || env("AWS_CONTAINER_CREDENTIALS_FULL_URI") != "" {
		return r.cfg.containerProvider(env), nil
	}
	imds := r.cfg.imdsProvider(env)
	return ProviderFunc(func(ctx context.Context) (Credentials, error) {
		creds, err := imds.Retrieve(ctx)
		if err != nil {
			return Credentials{}, fmt.Errorf("aws: no credentials found (checked environment, shared config profile %q, web identity, container credentials and EC2 instance metadata): %w", profileName, err)
		}
		return creds, nil
	}), nil
}

// profileProvider resolves the credentials a named profile describes.
func (r *resolver) profileProvider(name string, visited map[string]bool) (Provider, error) {
	if visited[name] {
		return nil, fmt.Errorf("aws: source_profile cycle at profile %q", name)
	}
	visited[name] = true
	p := r.files.profiles[name]
	if p == nil {
		return nil, fmt.Errorf("aws: profile %q not found in shared config", name)
	}
	static := Credentials{
		AccessKeyID: p["aws_access_key_id"], SecretAccessKey: p["aws_secret_access_key"],
		SessionToken: p["aws_session_token"], Source: "shared config profile " + name,
	}

	switch {
	case p["sso_session"] != "" || p["sso_start_url"] != "":
		return r.ssoProfile(name, p)
	case p["role_arn"] != "" && (p["source_profile"] != "" || p["credential_source"] != "" || p["web_identity_token_file"] == ""):
		return r.assumeRoleProfile(name, p, static, visited)
	case static.HasKeys():
		return StaticProvider(static), nil
	case p["credential_process"] != "":
		return processProvider(p["credential_process"]), nil
	case p["web_identity_token_file"] != "" && p["role_arn"] != "":
		return &webIdentity{cfg: r.cfg, tokenFile: p["web_identity_token_file"], roleARN: p["role_arn"], sessionName: p["role_session_name"]}, nil
	}
	return nil, fmt.Errorf("aws: profile %q does not configure any credentials", name)
}

func (r *resolver) ssoProfile(name string, p map[string]string) (Provider, error) {
	s := &ssoProvider{
		cfg: r.cfg, startURL: p["sso_start_url"], region: p["sso_region"],
		accountID: p["sso_account_id"], roleName: p["sso_role_name"],
	}
	if session := p["sso_session"]; session != "" {
		sess := r.files.ssoSessions[session]
		if sess == nil {
			return nil, fmt.Errorf("aws: profile %q refers to unknown sso-session %q", name, session)
		}
		s.session = session
		s.startURL = firstNonEmpty(sess["sso_start_url"], s.startURL)
		s.region = firstNonEmpty(sess["sso_region"], s.region)
	}
	if s.region == "" || s.accountID == "" || s.roleName == "" || (s.startURL == "" && s.session == "") {
		return nil, fmt.Errorf("aws: profile %q has incomplete SSO configuration (need sso_region, sso_account_id, sso_role_name and sso_session or sso_start_url)", name)
	}
	return NewCachedProvider(s), nil
}

func (r *resolver) assumeRoleProfile(name string, p map[string]string, static Credentials, visited map[string]bool) (Provider, error) {
	if p["mfa_serial"] != "" {
		return nil, fmt.Errorf("aws: profile %q requires MFA (mfa_serial), which is not supported", name)
	}
	var base Provider
	var err error
	switch {
	case p["source_profile"] != "":
		base, err = r.profileProvider(p["source_profile"], visited)
		if err != nil {
			return nil, err
		}
	case p["credential_source"] != "":
		switch p["credential_source"] {
		case "Environment":
			base = ProviderFunc(func(context.Context) (Credentials, error) {
				c := Credentials{
					AccessKeyID: r.env("AWS_ACCESS_KEY_ID"), SecretAccessKey: r.env("AWS_SECRET_ACCESS_KEY"),
					SessionToken: r.env("AWS_SESSION_TOKEN"), Source: "environment",
				}
				if !c.HasKeys() {
					return Credentials{}, errors.New("credential_source = Environment but no credentials are set in the environment")
				}
				return c, nil
			})
		case "Ec2InstanceMetadata":
			base = r.cfg.imdsProvider(r.env)
		case "EcsContainer":
			base = r.cfg.containerProvider(r.env)
		default:
			return nil, fmt.Errorf("aws: profile %q has unsupported credential_source %q", name, p["credential_source"])
		}
	case static.HasKeys():
		base = StaticProvider(static)
	default:
		return nil, fmt.Errorf("aws: profile %q sets role_arn without a credential source", name)
	}
	a := &assumeRole{
		cfg: r.cfg, base: NewCachedProvider(base), roleARN: p["role_arn"],
		sessionName: p["role_session_name"], externalID: p["external_id"],
	}
	if d := p["duration_seconds"]; d != "" {
		secs, err := strconv.Atoi(d)
		if err != nil {
			return nil, fmt.Errorf("aws: profile %q: invalid duration_seconds %q", name, d)
		}
		a.duration = time.Duration(secs) * time.Second
	}
	return NewCachedProvider(a), nil
}
