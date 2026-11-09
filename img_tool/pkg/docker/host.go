package docker

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

const contextProbeTimeout = 10 * time.Second

// DaemonEndpoint describes the Docker daemon the CLI will talk to.
type DaemonEndpoint struct {
	// Host is the endpoint address, empty when it could not be determined.
	Host string
	// Source names where Host came from, phrased for use in diagnostics.
	Source string
	// Remote reports whether Host addresses a daemon that does not share this
	// machine's containerd content store.
	Remote bool
}

// EffectiveDaemonHost reports the endpoint the docker CLI resolves to, applying
// the precedence the CLI itself does: DOCKER_HOST wins, otherwise the active
// docker context decides. An endpoint that cannot be determined is reported as
// local and unnamed, leaving the problem for the docker CLI to report.
func EffectiveDaemonHost(ctx context.Context) DaemonEndpoint {
	if host, remote := RemoteDaemonHost(); host != "" {
		return DaemonEndpoint{Host: host, Source: "DOCKER_HOST", Remote: remote}
	}
	host, err := activeContextEndpoint(ctx)
	if err != nil || host == "" {
		return DaemonEndpoint{}
	}
	return DaemonEndpoint{Host: host, Source: "the active docker context", Remote: remoteEndpoint(host)}
}

func activeContextEndpoint(ctx context.Context) (string, error) {
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, contextProbeTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, dockerPath, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// RemoteDaemonHost reports whether DOCKER_HOST points at a daemon that does not
// share this machine's containerd content store, returning the configured value
// alongside the verdict. An unset DOCKER_HOST yields ("", false); use
// [EffectiveDaemonHost] to also account for the active docker context.
//
// See https://github.com/bazel-contrib/rules_img/issues/766.
func RemoteDaemonHost() (string, bool) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		return "", false
	}
	return host, remoteEndpoint(host)
}

// remoteEndpoint classifies a docker daemon endpoint address.
//
// Unix sockets, Windows named pipes and systemd socket activation all address a
// daemon running on this machine. Anything reached over the network (tcp://,
// ssh://, http://, https://) may live in a VM or on another host, as Tilt
// configures for minikube. Every network address counts as remote, loopback
// included: writing into the local containerd when the daemon does not read it
// makes the image disappear, while taking the slower `docker load` path is
// always correct.
func remoteEndpoint(host string) bool {
	scheme, _, ok := strings.Cut(host, "://")
	if !ok {
		// Not a value the docker CLI accepts; leave the loading to docker so it
		// can report the problem itself.
		return true
	}
	switch strings.ToLower(scheme) {
	case "unix", "npipe", "fd":
		return false
	}
	return true
}
