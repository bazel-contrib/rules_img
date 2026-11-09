package containerd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

// dockerInfoTimeout bounds the `docker system info` probe so a wedged daemon
// cannot stall a load indefinitely.
const dockerInfoTimeout = 10 * time.Second

// DockerInfo represents the subset of docker system info we care about
type dockerInfo struct {
	DriverStatus [][]string `json:"DriverStatus"`
}

// DockerUsesContainerd reports whether Docker is installed and reads its images
// from containerd, returning an error describing why not otherwise. The answer
// describes whichever daemon the docker CLI resolves to, which is not
// necessarily the one on this machine.
func DockerUsesContainerd(ctx context.Context) error {
	// First, check if docker command exists
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		return fmt.Errorf("docker command not found in PATH")
	}

	ctx, cancel := context.WithTimeout(ctx, dockerInfoTimeout)
	defer cancel()

	// Run docker system info to check storage driver
	cmd := exec.CommandContext(ctx, dockerPath, "system", "info", "-f", "json")
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return fmt.Errorf("failed to run docker system info: %v: %s", err, bytes.TrimSpace(exitErr.Stderr))
		}
		return fmt.Errorf("failed to run docker system info: %v", err)
	}

	// Parse the JSON output
	var info dockerInfo
	if err := json.Unmarshal(output, &info); err != nil {
		return fmt.Errorf("failed to parse docker system info: %v", err)
	}

	// Check if containerd is mentioned in driver status
	hasContainerd := false
	for _, status := range info.DriverStatus {
		if len(status) >= 2 {
			// Look for containerd-related entries
			key := strings.ToLower(status[0])
			value := strings.ToLower(status[1])
			if strings.Contains(key, "containerd") || strings.Contains(value, "containerd") {
				hasContainerd = true
				break
			}
		}
	}

	if !hasContainerd {
		return fmt.Errorf("docker is not using containerd storage backend")
	}

	return nil
}

// FindContainerdSocket finds and tests containerd socket connectivity
// Returns the socket path if found and accessible, or ("", error)
func FindContainerdSocket() (string, error) {
	// Try common socket locations
	socketPaths := []string{
		"/run/containerd/containerd.sock",
		"/var/run/containerd/containerd.sock",
		"/run/docker/containerd/containerd.sock",
		"/var/run/docker/containerd/containerd.sock",
	}

	// Add home directory based socket path (e.g. macOS containerd-proxy)
	// See: https://github.com/bazel-contrib/rules_img/issues/199
	if u, err := user.Current(); err == nil {
		socketPaths = append(socketPaths, filepath.Join(u.HomeDir, ".docker/run/containerd.sock"))
	}

	if xdgRuntimeDir := os.Getenv("XDG_RUNTIME_DIR"); xdgRuntimeDir != "" {
		socketPaths = append(socketPaths, filepath.Join(xdgRuntimeDir, "containerd/containerd.sock"))
		socketPaths = append(socketPaths, filepath.Join(xdgRuntimeDir, "docker/containerd/containerd.sock"))
	}

	// Use CONTAINERD_ADDRESS env var if set
	if addr := os.Getenv("CONTAINERD_ADDRESS"); addr != "" {
		socketPaths = []string{addr}
	}

	var reasons []string
	for _, socket := range socketPaths {
		// Check if socket exists
		info, err := os.Stat(socket)
		if err != nil {
			if os.IsNotExist(err) {
				reasons = append(reasons, fmt.Sprintf("%s: does not exist", socket))
			} else {
				reasons = append(reasons, fmt.Sprintf("%s: %v", socket, err))
			}
			continue
		}

		// Check if it's actually a socket
		if info.Mode()&os.ModeSocket == 0 {
			reasons = append(reasons, fmt.Sprintf("%s: not a socket", socket))
			continue
		}

		// Try to connect to the socket
		conn, err := net.DialTimeout("unix", socket, 1*time.Second)
		if err != nil {
			if os.IsPermission(err) || strings.Contains(err.Error(), "permission denied") {
				reasons = append(reasons, fmt.Sprintf("%s: permission denied", socket))
			} else {
				reasons = append(reasons, fmt.Sprintf("%s: %v", socket, err))
			}
			continue
		}

		// Success! Close and return
		conn.Close()
		return socket, nil
	}

	// No suitable socket found, return all reasons
	if len(reasons) == 0 {
		return "", fmt.Errorf("no containerd sockets found to check")
	}
	return "", fmt.Errorf("no accessible containerd socket found:\n  %s", strings.Join(reasons, "\n  "))
}
