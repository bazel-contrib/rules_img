package containerd

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DockerInfo represents the subset of docker system info we care about
type dockerInfo struct {
	DriverStatus [][]string `json:"DriverStatus"`
}

// checkDockerUsesContainerd checks if Docker is installed and using containerd storage backend
func checkDockerUsesContainerd() error {
	// First, check if docker command exists
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		return fmt.Errorf("docker command not found in PATH")
	}

	// Run docker system info to check storage driver
	cmd := exec.Command(dockerPath, "system", "info", "-f", "json")
	output, err := cmd.Output()
	if err != nil {
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
		_ = conn.Close()
		return socket, nil
	}

	// No suitable socket found, return all reasons
	if len(reasons) == 0 {
		return "", fmt.Errorf("no containerd sockets found to check")
	}
	return "", fmt.Errorf("no accessible containerd socket found:\n  %s", strings.Join(reasons, "\n  "))
}

// DockerSupportsContainerd checks if Docker supports the containerd storage backend
// and if the containerd socket is accessible.
// Returns (socket path, nil) if supported, or ("", error) if not.
func DockerSupportsContainerd() (string, error) {
	// First check if Docker uses containerd
	if err := checkDockerUsesContainerd(); err != nil {
		return "", err
	}

	// Then find and test the containerd socket
	return FindContainerdSocket()
}
