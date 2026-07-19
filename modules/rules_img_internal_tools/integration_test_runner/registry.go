package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

// testRegistry is a running instance of the integration_test_registry binary.
type testRegistry struct {
	cmd      *exec.Cmd
	hostPort string // e.g. "localhost:42133"
}

// startRegistry launches an ephemeral in-memory OCI registry (with referrers
// support) and reads the host:port it reports on its first stdout line.
func startRegistry() (*testRegistry, error) {
	bin, err := runfiles.Rlocation(registryRlocationPath)
	if err != nil {
		return nil, fmt.Errorf("locating test registry: %v", err)
	}
	cmd := exec.Command(bin)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	reader := bufio.NewReader(stdout)
	line, err := readLineWithTimeout(reader, 20*time.Second)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("reading registry port: %v", err)
	}
	hostPort := strings.TrimSpace(line)
	if hostPort == "" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("registry did not report a host:port")
	}

	// Keep draining stdout so the registry never blocks on a full pipe.
	go func() { _, _ = io.Copy(os.Stdout, reader) }()
	fmt.Printf("test registry listening on %s (pid %d)\n", hostPort, cmd.Process.Pid)
	return &testRegistry{cmd: cmd, hostPort: hostPort}, nil
}

// Close terminates the registry process.
func (r *testRegistry) Close() {
	if r == nil || r.cmd == nil || r.cmd.Process == nil {
		return
	}
	_ = r.cmd.Process.Kill()
	_ = r.cmd.Wait()
}

func readLineWithTimeout(reader *bufio.Reader, timeout time.Duration) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := reader.ReadString('\n')
		ch <- result{line: line, err: err}
	}()
	select {
	case res := <-ch:
		return res.line, res.err
	case <-time.After(timeout):
		return "", fmt.Errorf("timed out after %s waiting for output", timeout)
	}
}
