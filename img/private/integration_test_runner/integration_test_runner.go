package main

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

type commandLine struct {
	name string
	args []string
}

func prepareWorkspace(workspaceDir, sourceDir string) error {
	localBCR, err := runfiles.Rlocation("_main/img/private/release/bcr.local")
	if err != nil {
		return fmt.Errorf("failed to find local bcr: %v", err)
	}
	distdir, err := runfiles.Rlocation("_main/img/private/release/airgapped.distdir")
	if err != nil {
		return fmt.Errorf("failed to find distdir: %v", err)
	}
	bazelDepOverride, err := runfiles.Rlocation("_main/img/private/release/bcr_local_module_rules_img.bazel_dep")
	if err != nil {
		return fmt.Errorf("failed to find bazel dep override: %v", err)
	}

	if err := copyFSWithSymlinks(workspaceDir, sourceDir); err != nil {
		return fmt.Errorf("failed to copy source dir: %v", err)
	}

	// Detect if this is a workspace mode test (after copying files)
	workspaceFile := filepath.Join(workspaceDir, "WORKSPACE.bazel")
	moduleFile := filepath.Join(workspaceDir, "MODULE.bazel")
	isWorkspaceMode := false

	// Check if WORKSPACE.bazel exists and contains BAZEL_DEP markers
	if _, err := os.Stat(workspaceFile); err == nil {
		workspaceData, err := os.ReadFile(workspaceFile)
		if err == nil && strings.Contains(string(workspaceData), "# BEGIN BAZEL_DEP") {
			isWorkspaceMode = true
		}
	}

	// Also check that MODULE.bazel doesn't exist (additional validation)
	if _, err := os.Stat(moduleFile); err == nil && isWorkspaceMode {
		return fmt.Errorf("both WORKSPACE.bazel and MODULE.bazel found - this should not happen")
	}

	// Handle patching based on mode (MODULE.bazel vs WORKSPACE.bazel)
	if isWorkspaceMode {
		// For WORKSPACE mode, patch WORKSPACE.bazel with http_archive instead of local_repository
		workspaceData, err := os.ReadFile(workspaceFile)
		if err != nil {
			return fmt.Errorf("failed to read workspace file: %v", err)
		}

		// Read the bazel_dep override to get the version
		depData, err := os.ReadFile(bazelDepOverride)
		if err != nil {
			return fmt.Errorf("failed to read dep override file: %v", err)
		}

		// Extract version from bazel_dep content (simple regex would work but let's use string parsing)
		depString := string(depData)
		versionStart := strings.Index(depString, `version = "`) + len(`version = "`)
		versionEnd := strings.Index(depString[versionStart:], `"`) + versionStart
		version := depString[versionStart:versionEnd]

		// Create the local_repository replacement for WORKSPACE mode
		// Use the local BCR source tree which contains the extracted rules_img source
		localBCRSourcePath := filepath.Join(filepath.Dir(localBCR), "bcr.local", "contents", "rules_img", version, "src")
		// Convert to absolute path and use forward slashes for cross-platform compatibility
		absLocalBCRSourcePath, err := filepath.Abs(localBCRSourcePath)
		if err != nil {
			return fmt.Errorf("failed to get absolute path for local BCR source: %v", err)
		}
		// Use forward slashes for Bazel paths even on Windows
		bcrSourcePathForBazel := filepath.ToSlash(absLocalBCRSourcePath)

		workspaceOverride := fmt.Sprintf(`local_repository(
    name = "rules_img",
    path = "%s",
)`, bcrSourcePathForBazel)

		startMarker := "# BEGIN BAZEL_DEP"
		endMarker := "# END BAZEL_DEP"
		startIndex := strings.Index(string(workspaceData), startMarker)
		endIndex := strings.Index(string(workspaceData), endMarker)
		if startIndex == -1 || endIndex == -1 {
			return fmt.Errorf("failed to find markers in workspace file")
		}

		patchedWorkspaceData := bytes.NewBuffer(nil)
		patchedWorkspaceData.Write(workspaceData[:startIndex])
		patchedWorkspaceData.WriteString(workspaceOverride)
		patchedWorkspaceData.Write(workspaceData[endIndex+len(endMarker):])
		os.Remove(workspaceFile)
		if err := os.WriteFile(workspaceFile, patchedWorkspaceData.Bytes(), 0o644); err != nil {
			return fmt.Errorf("failed to write patched workspace file: %v", err)
		}
	} else {
		// replace parts of MODULE.bazel with dep override:
		// anything between the markers is replaced
		// with the contents of the dep override file
		moduleFile := filepath.Join(workspaceDir, "MODULE.bazel")
		// Check if MODULE.bazel exists (it should for MODULE mode)
		if _, err := os.Stat(moduleFile); err != nil {
			return fmt.Errorf("MODULE.bazel not found for MODULE mode test: %v", err)
		}

		moduleData, err := os.ReadFile(moduleFile)
		if err != nil {
			return fmt.Errorf("failed to read module file: %v", err)
		}
		depData, err := os.ReadFile(bazelDepOverride)
		if err != nil {
			return fmt.Errorf("failed to read dep override file: %v", err)
		}
		startMarker := "# BEGIN BAZEL_DEP"
		endMarker := "# END BAZEL_DEP"
		startIndex := strings.Index(string(moduleData), startMarker)
		endIndex := strings.Index(string(moduleData), endMarker)
		if startIndex == -1 || endIndex == -1 {
			return fmt.Errorf("failed to find markers in module file")
		}

		patchedModuleData := bytes.NewBuffer(nil)
		patchedModuleData.Write(moduleData[:startIndex])
		patchedModuleData.Write(depData)
		patchedModuleData.Write(moduleData[endIndex+len(endMarker):])
		os.Remove(moduleFile)
		if err := os.WriteFile(moduleFile, patchedModuleData.Bytes(), 0o644); err != nil {
			return fmt.Errorf("failed to write patched module file: %v", err)
		}
	}
	localBCRUrlPath := filepath.ToSlash(localBCR)
	if runtime.GOOS == "windows" {
		localBCRUrlPath = "file:///" + localBCRUrlPath
	} else {
		localBCRUrlPath = "file://" + localBCRUrlPath
	}

	var bazelrc string
	if isWorkspaceMode {
		// For WORKSPACE mode, disable bzlmod and enable workspace
		bazelrc = fmt.Sprintf(`common --noenable_bzlmod
common --enable_workspace
common --registry=%s --registry=https://bcr.bazel.build/
common --distdir=%s
`, localBCRUrlPath, filepath.ToSlash(distdir))
	} else {
		// For MODULE mode, include the local BCR registry
		bazelrc = fmt.Sprintf(`common --registry=%s --registry=https://bcr.bazel.build/
common --distdir=%s
`, localBCRUrlPath, filepath.ToSlash(distdir))
	}
	return os.WriteFile(filepath.Join(workspaceDir, ".bazelrc.generated"), []byte(bazelrc), 0o644)
}

func outputUserRoot() (string, func() error) {
	if runtime.GOOS != "windows" {
		return "", func() error { return nil }
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	tmpDir, err := os.MkdirTemp(cache, "bit-")
	if err != nil {
		panic(err)
	}
	return tmpDir, func() error {
		return os.RemoveAll(tmpDir)
	}
}

func bazelCommand(name string, command []string, startupFlags []string) commandLine {
	var args []string
	args = append(args, startupFlags...)
	args = append(args, command...)
	return commandLine{name: name, args: args}
}

func bazelCommands(bazel string, startupFlags []string, moduleName string) (setup []commandLine, tests []commandLine, shutdown []commandLine) {
	var setupCommands []commandLine

	// Build metadata flag to tag the module being tested
	var metadataFlag string
	if moduleName != "" {
		metadataFlag = fmt.Sprintf("--build_metadata=TAG_MODULE_NAME=%s", moduleName)
	}

	buildCmd := []string{"build", "//..."}
	if metadataFlag != "" {
		buildCmd = append(buildCmd, metadataFlag)
	}

	setupCommands = append(setupCommands, bazelCommand(bazel, []string{"info"}, startupFlags))
	setupCommands = append(setupCommands, bazelCommand(bazel, buildCmd, startupFlags))

	testCmd := []string{"test", "//..."}
	if metadataFlag != "" {
		testCmd = append(testCmd, metadataFlag)
	}

	return setupCommands, []commandLine{
		bazelCommand(bazel, []string{"query", "@rules_img//..."}, startupFlags), // ensure all referenced BUILD files are included in the release tar
		bazelCommand(bazel, testCmd, startupFlags),
	}, []commandLine{bazelCommand(bazel, []string{"shutdown"}, startupFlags)}
}

func runBazelCommands(bazel, workspaceDir string) error {
	startupFlags := []string{"--nosystem_rc", "--nohome_rc"}

	root, cleanupRoot := outputUserRoot()
	defer cleanupRoot()
	if len(root) > 0 {
		startupFlags = append(startupFlags, "--output_user_root="+root)
	}
	startupFlags = append(startupFlags, "--bazelrc="+filepath.Join(".bazelrc"))
	startupFlags = append(startupFlags, "--bazelrc="+filepath.Join(".bazelrc.generated"))

	// Inject additional bazelrc if specified via environment variable
	if injectedBazelrc := os.Getenv("BAZEL_INTEGRATION_TEST_INJECT_BAZELRC"); injectedBazelrc != "" {
		startupFlags = append(startupFlags, "--bazelrc="+injectedBazelrc)
	}

	// Extract module name from BIT_WORKSPACE_DIR for build metadata tagging
	moduleName := ""
	if workspaceDirEnv := os.Getenv("BIT_WORKSPACE_DIR"); workspaceDirEnv != "" {
		moduleName = filepath.Base(workspaceDirEnv)
	}

	setupCommands, testCommands, shutdownCommands := bazelCommands(bazel, startupFlags, moduleName)

	defer func() {
		// shut down Bazel after all tests to conserve memory
		for _, shutdownCmd := range shutdownCommands {
			cmd := exec.Command(shutdownCmd.name, shutdownCmd.args...)
			cmd.Dir = workspaceDir
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			_ = cmd.Run()
		}
	}()

	for _, command := range setupCommands {
		fmt.Printf("\nrunning setup command $ bazel %s\n", strings.Join(command.args, " "))
		cmd := exec.Command(command.name, command.args...)
		cmd.Dir = workspaceDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if err != nil {
			return fmt.Errorf("bazel integration test setup step failed for command %v: %v", command, err)
		}
	}

	fmt.Println("\nrunning tests")
	for _, command := range testCommands {
		cmd := exec.Command(command.name, command.args...)
		cmd.Dir = workspaceDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if err != nil {
			return fmt.Errorf("bazel integration test failed for command %v: %v", command, err)
		}
	}
	return nil
}

func absolutifyEnvVars() error {
	keys := strings.Fields(os.Getenv("ENV_VARS_TO_ABSOLUTIFY"))
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			absPath, err := filepath.Abs(value)
			if err != nil {
				return err
			}
			if err := os.Setenv(key, absPath); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyFSWithSymlinks(destination, source string) error {
	canonicalBase := filepath.Clean(source)
	return filepath.Walk(source, func(path string, currentInfo os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		canoncialPath := filepath.Clean(path)
		relativePath, err := filepath.Rel(canonicalBase, canoncialPath)
		if err != nil {
			return err
		}

		newPath := filepath.Join(destination, relativePath)
		if currentInfo.IsDir() {
			return os.MkdirAll(newPath, 0o777)
		}

		if currentInfo.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(target, newPath)
		}

		if !currentInfo.Mode().IsRegular() {
			return &os.PathError{Op: "CopyFS", Path: path, Err: os.ErrInvalid}
		}

		r, err := os.Open(path)
		if err != nil {
			return err
		}
		defer r.Close()
		info, err := r.Stat()
		if err != nil {
			return err
		}
		w, err := os.OpenFile(newPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666|info.Mode()&0o777)
		if err != nil {
			return err
		}

		if _, err := io.Copy(w, r); err != nil {
			w.Close()
			return &os.PathError{Op: "Copy", Path: newPath, Err: err}
		}
		return w.Close()
	})
}

func runCraneRegistry() (*exec.Cmd, error) {
	// Try to find crane in PATH first
	cranePath, err := exec.LookPath("crane")
	if err != nil {
		// If not in PATH, try runfiles location
		cranePath, err = runfiles.Rlocation("go-containerregistry/cmd/crane/crane_/crane")
		if err != nil {
			return nil, fmt.Errorf("failed to find crane binary: %v", err)
		}
	}

	cmd := exec.Command(cranePath, "registry", "serve", "--address", ":5001")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err = cmd.Start()
	if err != nil {
		return nil, err
	}

	fmt.Printf("crane registry started with PID: %d\n", cmd.Process.Pid)

	// Wait a bit for the registry to start
	time.Sleep(2 * time.Second)

	return cmd, nil
}

func main() {
	bazel := os.Getenv("BIT_BAZEL_BINARY")
	workspaceDir := os.Getenv("BIT_WORKSPACE_DIR") + ".scratch"
	defer os.RemoveAll(workspaceDir)

	if err := absolutifyEnvVars(); err != nil {
		panic(err)
	}

	// Start crane registry serve
	// registryCmd, err := runCraneRegistry()
	// if err != nil {
	// 	fmt.Fprintf(os.Stderr, "starting crane registry: %v\n", err)
	// 	os.Exit(1)
	// }
	// defer func() {
	// 	if registryCmd != nil && registryCmd.Process != nil {
	// 		registryCmd.Process.Kill()
	// 		registryCmd.Wait()
	// 	}
	// }()

	// Set registry environment variable for tests
	os.Setenv("REGISTRY_HOST", "localhost:5001")

	var failed bool

	if err := prepareWorkspace(workspaceDir, os.Getenv("BIT_WORKSPACE_DIR")); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	integrationTestErr := runBazelCommands(bazel, workspaceDir)
	if integrationTestErr != nil {
		fmt.Fprintln(os.Stderr, integrationTestErr.Error())
		failed = true
	}

	if failed {
		os.Exit(1)
	}
}
