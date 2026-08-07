/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package erofstest

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

func ConvertTarErofs(ctx context.Context, r io.Reader, layerPath, uuid string, mkfsExtraOpts []string) error {
	args := append([]string{"--tar=f", "--aufs", "--quiet", "-Enoinline_data"}, mkfsExtraOpts...)
	if uuid != "" {
		args = append(args, []string{"-U", uuid}...)
	}
	args = append(args, layerPath)
	cmd := exec.CommandContext(ctx, "mkfs.erofs", args...)
	cmd.Stdin = r
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("erofs apply failed: %s: %w", out, err)
	}
	return nil
}
