// Package ecr obtains registry credentials for Amazon ECR and ECR Public. It
// replaces amazon-ecr-credential-helper's ecr-login for in-process use.
package ecr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/aws"
	"github.com/docker/docker-credential-helpers/credentials"
)

const (
	ecrPublicName          = "public.ecr.aws"
	ecrPublicDualStackName = "ecr-public.aws.com"
)

var ecrPattern = regexp.MustCompile(`^(\d{12})\.dkr[\.\-]ecr(\-fips)?\.([a-zA-Z0-9][a-zA-Z0-9-_]*)\.(amazonaws\.(?:com(?:\.cn)?|eu)|on\.(?:aws|amazonwebservices\.com\.cn)|sc2s\.sgov\.gov|c2s\.ic\.gov|cloud\.adc-e\.uk|csp\.hci\.ic\.gov)$`)

// registry describes the ECR registry a server URL points at.
type registry struct {
	public bool
	id     string
	fips   bool
	region string
	// suffix is the DNS suffix of the registry host, e.g. amazonaws.com.
	suffix string
}

// parseRegistry extracts the registry from a server URL or bare host name.
func parseRegistry(serverURL string) (*registry, error) {
	serverURL = strings.TrimPrefix(serverURL, "https://")
	u, err := url.Parse("https://" + serverURL)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	if host == ecrPublicName || host == ecrPublicDualStackName {
		return &registry{public: true, region: "us-east-1", suffix: "amazonaws.com"}, nil
	}
	m := ecrPattern.FindStringSubmatch(host)
	if m == nil {
		return nil, errors.New("not an Amazon ECR registry")
	}
	return &registry{id: m[1], fips: m[2] == "-fips", region: m[3], suffix: m[4]}, nil
}

// apiSuffix maps a registry host suffix to the suffix of the ECR API hosts.
func apiSuffix(registrySuffix string) string {
	switch registrySuffix {
	case "on.aws":
		return "api.aws"
	case "on.amazonwebservices.com.cn":
		return "api.amazonwebservices.com.cn"
	}
	return registrySuffix
}

// Helper is a Docker credential helper (read-only) for Amazon ECR. If
// docker-credential-ecr-login is on the PATH it is run, which gives the full
// feature set of the upstream helper. Otherwise (or if it fails) credentials
// are fetched in process. The AWS configuration for the in-process path is
// loaded once and its credentials are shared by all lookups.
type Helper struct {
	load   func(context.Context) (*aws.Config, error)
	logger io.Writer
	// external runs the docker-credential-ecr-login binary if one is on the
	// PATH. It returns ok=false if there is none or it failed.
	external func(ctx context.Context, serverURL string) (user, password string, ok bool)

	once sync.Once
	cfg  *aws.Config
	err  error
}

// NewHelper returns a Helper that loads the default AWS configuration on first
// use. Diagnostics are written to logger, which may be nil.
func NewHelper(logger io.Writer) *Helper {
	if logger == nil {
		logger = io.Discard
	}
	h := &Helper{
		load:   func(ctx context.Context) (*aws.Config, error) { return aws.LoadDefaultConfig(ctx) },
		logger: logger,
	}
	h.external = h.runExternal
	return h
}

const externalHelper = "docker-credential-ecr-login"

// runExternal runs `docker-credential-ecr-login get` per the Docker credential
// helper protocol: the server URL on stdin, JSON on stdout.
func (h *Helper) runExternal(ctx context.Context, serverURL string) (string, string, bool) {
	path, err := exec.LookPath(externalHelper)
	if err != nil {
		return "", "", false
	}
	cmd := exec.CommandContext(ctx, path, "get")
	cmd.Stdin = strings.NewReader(serverURL)
	cmd.Env = os.Environ()
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(h.logger, "ecr: %s failed, using built-in implementation: %v\n", externalHelper, err)
		return "", "", false
	}
	var out struct{ Username, Secret string }
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || out.Secret == "" {
		fmt.Fprintf(h.logger, "ecr: %s returned unusable output, using built-in implementation\n", externalHelper)
		return "", "", false
	}
	return out.Username, out.Secret, true
}

func (h *Helper) config(ctx context.Context) (*aws.Config, error) {
	h.once.Do(func() { h.cfg, h.err = h.load(ctx) })
	return h.cfg, h.err
}

// Get implements [github.com/google/go-containerregistry/pkg/authn.Helper].
// Anything that goes wrong is reported as "credentials not found" so callers
// fall through to the next keychain; the reason is written to the logger.
func (h *Helper) Get(serverURL string) (string, string, error) {
	reg, err := parseRegistry(serverURL)
	if err != nil {
		fmt.Fprintf(h.logger, "ecr: %s: %v\n", serverURL, err)
		return "", "", credentials.NewErrCredentialsNotFound()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if h.external != nil {
		if user, password, ok := h.external(ctx, serverURL); ok {
			return user, password, nil
		}
	}
	cfg, err := h.config(ctx)
	if err != nil {
		fmt.Fprintf(h.logger, "ecr: load AWS configuration: %v\n", err)
		return "", "", credentials.NewErrCredentialsNotFound()
	}
	user, password, err := getLogin(ctx, cfg, reg)
	if err != nil {
		fmt.Fprintf(h.logger, "ecr: retrieve credentials for %s: %v\n", serverURL, err)
		return "", "", credentials.NewErrCredentialsNotFound()
	}
	return user, password, nil
}

// getLogin fetches an authorization token for the registry from the ECR API
// and splits it into a username and password.
func getLogin(ctx context.Context, cfg *aws.Config, reg *registry) (string, string, error) {
	var (
		host, service, target, region string
		body                          any = struct{}{}
	)
	suffix := apiSuffix(reg.suffix)
	if reg.public {
		// The ECR Public API only exists in us-east-1.
		region, service = "us-east-1", "ecr-public"
		host = "api.ecr-public." + region + "." + suffix
		target = "SpencerFrontendService.GetAuthorizationToken"
	} else {
		region, service = reg.region, "ecr"
		host = "api.ecr." + region + "." + suffix
		if reg.fips {
			host = "ecr-fips." + region + "." + suffix
		}
		target = "AmazonEC2ContainerRegistry_V20150921.GetAuthorizationToken"
		body = map[string][]string{"registryIds": {reg.id}}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", "", err
	}
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return "", "", err
	}
	hash := sha256Hex(payload)

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := aws.Do(ctx, client, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, "https://"+host+"/", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-amz-json-1.1")
		req.Header.Set("X-Amz-Target", target)
		return req, aws.Sign(req, creds, aws.SignOptions{Service: service, Region: region, PayloadHash: hash})
	})
	if err != nil {
		return "", "", fmt.Errorf("ecr: failed to get authorization token: %w", err)
	}
	defer resp.Body.Close()

	// The ECR API returns a list, ECR Public a single object.
	var out struct {
		AuthorizationData json.RawMessage `json:"authorizationData"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", "", fmt.Errorf("ecr: decode response: %w", err)
	}
	type authData struct {
		AuthorizationToken string `json:"authorizationToken"`
	}
	var entries []authData
	if reg.public {
		var one authData
		if err := json.Unmarshal(out.AuthorizationData, &one); err == nil {
			entries = append(entries, one)
		}
	} else if err := json.Unmarshal(out.AuthorizationData, &entries); err != nil {
		return "", "", fmt.Errorf("ecr: decode authorizationData: %w", err)
	}
	for _, e := range entries {
		if e.AuthorizationToken != "" {
			return extractToken(e.AuthorizationToken)
		}
	}
	return "", "", errors.New("ecr: no authorization token in response")
}

func extractToken(token string) (string, string, error) {
	decoded, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return "", "", fmt.Errorf("ecr: invalid token: %w", err)
	}
	user, password, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return "", "", errors.New("ecr: invalid token: expected user:password")
	}
	return user, password, nil
}
