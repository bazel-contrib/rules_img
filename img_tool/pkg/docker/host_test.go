package docker

import "testing"

func TestRemoteDaemonHost(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dockerHost string
		wantRemote bool
	}{
		{name: "unset", dockerHost: "", wantRemote: false},
		{name: "unix socket", dockerHost: "unix:///var/run/docker.sock", wantRemote: false},
		{name: "rootless unix socket", dockerHost: "unix:///run/user/1000/docker.sock", wantRemote: false},
		{name: "uppercase scheme", dockerHost: "UNIX:///var/run/docker.sock", wantRemote: false},
		{name: "windows named pipe", dockerHost: "npipe:////./pipe/docker_engine", wantRemote: false},
		{name: "socket activation", dockerHost: "fd://", wantRemote: false},
		{name: "minikube over tcp", dockerHost: "tcp://192.168.49.2:2376", wantRemote: true},
		{name: "loopback over tcp", dockerHost: "tcp://127.0.0.1:2375", wantRemote: true},
		{name: "remote over ssh", dockerHost: "ssh://user@example.com", wantRemote: true},
		{name: "https", dockerHost: "https://example.com:2376", wantRemote: true},
		{name: "no scheme", dockerHost: "/var/run/docker.sock", wantRemote: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", tc.dockerHost)
			host, remote := RemoteDaemonHost()
			if remote != tc.wantRemote {
				t.Errorf("RemoteDaemonHost() remote = %v, want %v", remote, tc.wantRemote)
			}
			if remote && host != tc.dockerHost {
				t.Errorf("RemoteDaemonHost() host = %q, want %q", host, tc.dockerHost)
			}
		})
	}
}
