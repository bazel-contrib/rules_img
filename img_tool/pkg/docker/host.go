package docker

import (
	"os"
	"strings"
)

// RemoteDaemonHost reports whether the Docker client environment points at a
// daemon that does not share this machine's containerd content store, returning
// the configured DOCKER_HOST alongside the verdict.
//
// Unix sockets, Windows named pipes and systemd socket activation all address a
// daemon running on this machine. Anything reached over the network (tcp://,
// ssh://, http://, https://) may live in a VM or on another host, as Tilt
// configures for minikube. Every network address counts as remote, loopback
// included: writing into the local containerd when the daemon does not read it
// makes the image disappear, while taking the slower `docker load` path is
// always correct.
//
// See https://github.com/bazel-contrib/rules_img/issues/766.
func RemoteDaemonHost() (string, bool) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		return "", false
	}
	scheme, _, ok := strings.Cut(host, "://")
	if !ok {
		// Not a value the docker CLI accepts; leave the loading to docker so it
		// can report the problem itself.
		return host, true
	}
	switch strings.ToLower(scheme) {
	case "unix", "npipe", "fd":
		return host, false
	}
	return host, true
}
