package signer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	"github.com/bazelbuild/rules_go/go/runfiles"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

// Subprocess is the single api.OCIArtifactSigner implementation used by
// `img deploy`. It delegates signing to an external plugin invoked as
// `<tool> sign-oci-artifact [args...]`, feeding the subject descriptor JSON on
// stdin and reading an OCI image layout tar on stdout.
type Subprocess struct {
	toolPath string
	args     []string // ["sign-oci-artifact", <plugin args>...]
	env      []string
}

var _ api.OCIArtifactSigner = (*Subprocess)(nil)

// maxSignerStdout caps how much of a plugin's stdout we buffer. Signature
// artifacts are at most a few KiB; 64 MiB is a generous safety limit that
// bounds memory use if a misbehaving plugin streams unbounded output.
const maxSignerStdout = 64 << 20 // 64 MiB

// NewSubprocess resolves the plugin executable (from runfiles or PATH) and
// prepares the command line and environment. The child inherits the current
// environment (for secrets: KMS creds, SIGSTORE_*/NOTATION_*, etc.) plus the
// runfiles environment (so a Bazel-built plugin can locate its own runfiles),
// plus any non-secret env declared in the sign_setting config.
func NewSubprocess(cfg SignSettingConfig, rf *runfiles.Runfiles) (*Subprocess, error) {
	var toolPath string
	switch cfg.Mode {
	case "rlocation":
		if rf == nil {
			return nil, fmt.Errorf("sign_setting uses an rlocation plugin %q but runfiles are unavailable", cfg.Tool)
		}
		p, err := rf.Rlocation(cfg.Tool)
		if err != nil {
			return nil, fmt.Errorf("resolving signer plugin %q from runfiles: %w", cfg.Tool, err)
		}
		toolPath = p
	case "command":
		p, err := exec.LookPath(cfg.Tool)
		if err != nil {
			return nil, fmt.Errorf("locating signer command %q on PATH: %w", cfg.Tool, err)
		}
		toolPath = p
	default:
		return nil, fmt.Errorf("unknown sign_setting mode %q (want \"rlocation\" or \"command\")", cfg.Mode)
	}

	env := os.Environ()
	if rf != nil {
		env = append(env, rf.Env()...)
	}
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}

	return &Subprocess{
		toolPath: toolPath,
		args:     append([]string{"sign-oci-artifact"}, cfg.Args...),
		env:      env,
	}, nil
}

// SignArtifacts runs the plugin for subject and returns every artifact image in
// its OCI-layout output (a plugin may emit more than one, e.g. a signature plus
// an attestation).
func (s *Subprocess) SignArtifacts(ctx context.Context, subject v1.Descriptor) ([]v1.Image, error) {
	subjJSON, err := json.Marshal(subject)
	if err != nil {
		return nil, fmt.Errorf("marshalling subject descriptor: %w", err)
	}

	cmd := exec.CommandContext(ctx, s.toolPath, s.args...)
	cmd.Stdin = bytes.NewReader(subjJSON) // os/exec closes the write end at EOF
	cmd.Env = s.env
	// Stream stderr to the user so interactive prompts (security-key touch, PIN,
	// OIDC device flow) are visible.
	cmd.Stderr = os.Stderr
	stdout := &limitedBuffer{limit: maxSignerStdout}
	cmd.Stdout = stdout

	if err := cmd.Run(); err != nil {
		if stdout.over {
			return nil, fmt.Errorf("signer plugin %q produced more than %d bytes of output", s.toolPath, maxSignerStdout)
		}
		return nil, fmt.Errorf("signer plugin %q failed: %w", s.toolPath, err)
	}

	imgs, err := ReadArtifactLayout(stdout.buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("reading signer plugin %q output: %w", s.toolPath, err)
	}
	if len(imgs) == 0 {
		return nil, fmt.Errorf("signer plugin %q produced an empty OCI layout", s.toolPath)
	}
	return imgs, nil
}

// Sign implements api.OCIArtifactSigner. It returns the primary (first) artifact
// image; callers that need every manifest in the plugin's layout should use
// SignArtifacts.
func (s *Subprocess) Sign(ctx context.Context, subject v1.Descriptor) (v1.Image, error) {
	imgs, err := s.SignArtifacts(ctx, subject)
	if err != nil {
		return nil, err
	}
	return imgs[0], nil
}

// limitedBuffer is a bytes.Buffer that refuses to grow beyond limit, so a
// runaway plugin cannot exhaust memory. Once the limit is exceeded, over is set
// and further writes error (which fails cmd.Run).
type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.limit {
		b.over = true
		return 0, fmt.Errorf("output exceeds %d byte limit", b.limit)
	}
	return b.buf.Write(p)
}
