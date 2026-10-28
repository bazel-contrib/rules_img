package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"
)

// processProvider runs a credential_process command, which prints a JSON
// document with Version 1 credentials to stdout.
func processProvider(command string) Provider {
	return ProviderFunc(func(ctx context.Context) (Credentials, error) {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.CommandContext(ctx, "cmd.exe", "/C", command)
		} else {
			cmd = exec.CommandContext(ctx, "sh", "-c", command)
		}
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return Credentials{}, fmt.Errorf("credential_process: %w", err)
		}
		var out struct {
			Version         int
			AccessKeyID     string `json:"AccessKeyId"`
			SecretAccessKey string
			SessionToken    string
			Expiration      time.Time
		}
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			return Credentials{}, fmt.Errorf("credential_process: decode output: %w", err)
		}
		if out.Version != 1 {
			return Credentials{}, fmt.Errorf("credential_process: unsupported version %d", out.Version)
		}
		return Credentials{
			AccessKeyID: out.AccessKeyID, SecretAccessKey: out.SecretAccessKey,
			SessionToken: out.SessionToken, Expires: out.Expiration, Source: "credential_process",
		}, nil
	})
}
