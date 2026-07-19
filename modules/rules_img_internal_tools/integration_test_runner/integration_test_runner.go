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

	"github.com/bazelbuild/rules_go/go/runfiles"
)

// Runfiles paths, injected via x_defs in BUILD.bazel.
var (
	registryRlocationPath    string
	asserterRlocationPath    string
	cosignCLIRlocationPath   string
	notationCLIRlocationPath string
)

const signingEnvMarker = "RULES_IMG_E2E_SIGNING"

type commandLine struct {
	name string
	args []string
}

func prepareWorkspace(workspaceDir, sourceDir, mainRegistry string) error {
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
	// Redirect every push/load at the ephemeral local registry (go-containerregistry
	// dials localhost:<port> over plain HTTP automatically). Image targets whose
	// `registry` attribute is empty pick this up; per-image `registry` still wins.
	if mainRegistry != "" {
		bazelrc += fmt.Sprintf("common --@rules_img//img/settings:destination_registry=%s\n", mainRegistry)
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

func startupFlags() ([]string, func() error) {
	flags := []string{"--nosystem_rc", "--nohome_rc"}
	root, cleanupRoot := outputUserRoot()
	if len(root) > 0 {
		flags = append(flags, "--output_user_root="+root)
	}
	flags = append(flags, "--bazelrc="+filepath.Join(".bazelrc"))
	flags = append(flags, "--bazelrc="+filepath.Join(".bazelrc.generated"))
	if injectedBazelrc := os.Getenv("BAZEL_INTEGRATION_TEST_INJECT_BAZELRC"); injectedBazelrc != "" {
		flags = append(flags, "--bazelrc="+injectedBazelrc)
	}
	return flags, cleanupRoot
}

// runBazel runs a bazel command in workspaceDir with the given extra environment.
func runBazel(bazel, workspaceDir string, startup []string, extraEnv []string, label string, args ...string) error {
	full := append(append([]string{}, startup...), args...)
	fmt.Printf("\nrunning %s $ bazel %s\n", label, strings.Join(full, " "))
	cmd := exec.Command(bazel, full...)
	cmd.Dir = workspaceDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("bazel %s failed: %v", label, err)
	}
	return nil
}

// targetExists reports whether label resolves in the workspace (used to gate the
// push/deploy phases so e2e workspaces without them still pass).
func targetExists(bazel, workspaceDir string, startup []string, label string) bool {
	args := append(append([]string{}, startup...), "query", label)
	cmd := exec.Command(bazel, args...)
	cmd.Dir = workspaceDir
	return cmd.Run() == nil
}

func runBazelCommands(bazel, workspaceDir string, startup []string) error {
	moduleName := ""
	if workspaceDirEnv := os.Getenv("BIT_WORKSPACE_DIR"); workspaceDirEnv != "" {
		moduleName = filepath.Base(workspaceDirEnv)
	}
	var metadataFlag []string
	if moduleName != "" {
		metadataFlag = []string{fmt.Sprintf("--build_metadata=TAG_MODULE_NAME=%s", moduleName)}
	}

	if err := runBazel(bazel, workspaceDir, startup, nil, "setup info", "info"); err != nil {
		return err
	}
	if err := runBazel(bazel, workspaceDir, startup, nil, "setup build", append([]string{"build", "//..."}, metadataFlag...)...); err != nil {
		return err
	}
	// ensure all referenced BUILD files are included in the release tar
	if err := runBazel(bazel, workspaceDir, startup, nil, "query", "query", "@rules_img//..."); err != nil {
		return err
	}
	if err := runBazel(bazel, workspaceDir, startup, nil, "test", append([]string{"test", "//..."}, metadataFlag...)...); err != nil {
		return err
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
			_ = w.Close()
			return &os.PathError{Op: "Copy", Path: newPath, Err: err}
		}
		return w.Close()
	})
}

func main() {
	os.Exit(run())
}

func run() int {
	bazel := os.Getenv("BIT_BAZEL_BINARY")
	workspaceDir := os.Getenv("BIT_WORKSPACE_DIR") + ".scratch"
	defer os.RemoveAll(workspaceDir)

	if err := absolutifyEnvVars(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}

	// Start the main ephemeral registry and learn its port.
	mainReg, err := startRegistry()
	if err != nil {
		fmt.Fprintf(os.Stderr, "starting test registry: %v\n", err)
		return 1
	}
	defer mainReg.Close()

	if err := prepareWorkspace(workspaceDir, os.Getenv("BIT_WORKSPACE_DIR"), mainReg.hostPort); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}

	startup, cleanupRoot := startupFlags()
	defer cleanupRoot()

	// Shut down Bazel at the end to conserve memory.
	defer func() {
		_ = runBazel(bazel, workspaceDir, startup, nil, "shutdown", "shutdown")
	}()

	if err := runBazelCommands(bazel, workspaceDir, startup); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}

	if err := deployPhase(bazel, workspaceDir, startup, mainReg.hostPort); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}

	return 0
}

// deployPhase runs `bazel run //:push` against the local registry, verifies the
// resulting registry state, and — for signing-capable tests — repeats signed
// pushes against dedicated per-signer registries and verifies the signatures.
func deployPhase(bazel, workspaceDir string, startup []string, mainRegistry string) error {
	// FIXME: only run the registry test on Linux — some images we push currently have no explicit platform and their base image is only available for Linux, so //:push is incompatible on macOS/Windows.
	if runtime.GOOS != "linux" {
		fmt.Printf("\nhost OS is %s, not linux; skipping deploy phase (registry test)\n", runtime.GOOS)
		return nil
	}

	if !targetExists(bazel, workspaceDir, startup, "//:push") {
		fmt.Println("\nno //:push target in this workspace; skipping deploy phase")
		return nil
	}

	if err := runBazel(bazel, workspaceDir, startup, nil, "push", "run", "//:push"); err != nil {
		return err
	}
	if err := runAsserter(workspaceDir, mainRegistry, "", "", nil); err != nil {
		return err
	}

	if os.Getenv(signingEnvMarker) != "1" {
		return nil
	}

	keys, err := generateSigningKeys()
	if err != nil {
		return fmt.Errorf("generating signing keys: %v", err)
	}
	defer os.RemoveAll(keys.dir)

	cosignReg, err := startRegistry()
	if err != nil {
		return fmt.Errorf("starting cosign registry: %v", err)
	}
	defer cosignReg.Close()

	notationReg, err := startRegistry()
	if err != nil {
		return fmt.Errorf("starting notation registry: %v", err)
	}
	defer notationReg.Close()

	signEnv := []string{
		"RULES_IMG_COSIGN_KEY=" + keys.cosignKey,
		"COSIGN_PASSWORD=",
		"RULES_IMG_NOTATION_KEY=" + keys.notationKey,
		"RULES_IMG_NOTATION_CERTIFICATE_CHAIN=" + keys.notationCert,
	}

	if err := runBazel(bazel, workspaceDir, startup, signEnv, "push (cosign)",
		"run", "//:push", "--config=cosign",
		"--@rules_img//img/settings:destination_registry="+cosignReg.hostPort); err != nil {
		return err
	}
	if err := runBazel(bazel, workspaceDir, startup, signEnv, "push (notation)",
		"run", "//:push", "--config=notation",
		"--@rules_img//img/settings:destination_registry="+notationReg.hostPort); err != nil {
		return err
	}

	// Verify signature referrers on the per-signer registries with the real CLIs.
	return runAsserter(workspaceDir, mainRegistry, cosignReg.hostPort, notationReg.hostPort, keys)
}

// runAsserter runs the registry-state asserter against the given registries. When
// the workspace has no registry_assertions.json, it is skipped.
func runAsserter(workspaceDir, mainRegistry, cosignRegistry, notationRegistry string, keys *signingKeys) error {
	specPath := filepath.Join(workspaceDir, "registry_assertions.json")
	if _, err := os.Stat(specPath); err != nil {
		fmt.Println("\nno registry_assertions.json in this workspace; skipping registry assertions")
		return nil
	}

	asserter, err := runfiles.Rlocation(asserterRlocationPath)
	if err != nil {
		return fmt.Errorf("locating asserter: %v", err)
	}

	args := []string{"--registry", mainRegistry, "--spec", specPath}
	if cosignRegistry != "" {
		cosignCLI, err := runfiles.Rlocation(cosignCLIRlocationPath)
		if err != nil {
			return fmt.Errorf("locating cosign CLI: %v", err)
		}
		args = append(args,
			"--cosign-registry", cosignRegistry,
			"--cosign-cli", cosignCLI,
			"--cosign-pubkey", keys.cosignPub)
	}
	if notationRegistry != "" {
		notationCLI, err := runfiles.Rlocation(notationCLIRlocationPath)
		if err != nil {
			return fmt.Errorf("locating notation CLI: %v", err)
		}
		args = append(args,
			"--notation-registry", notationRegistry,
			"--notation-cli", notationCLI,
			"--notation-cert", keys.notationCert)
	}

	fmt.Printf("\nrunning registry asserter $ %s %s\n", filepath.Base(asserter), strings.Join(args, " "))
	cmd := exec.Command(asserter, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("registry assertions failed: %v", err)
	}
	return nil
}
