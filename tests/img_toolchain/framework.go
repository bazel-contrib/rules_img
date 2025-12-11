package img_toolchain

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

type TestCase struct {
	Name        string
	Description string
	Setup       SetupSpec
	Command     CommandSpec
	Assertions  []AssertionSpec
	Debug       DebugSpec
}

type SetupSpec struct {
	Files         map[string]string
	TestdataFiles map[string]string // Maps destination path -> testdata source path
	Symlinks      map[string]string // Maps destination path -> target
}

type CommandSpec struct {
	Subcommand string
	Args       []string
	ExpectExit int
	Stdin      string
	Verbose    bool
}

type AssertionSpec struct {
	Type     string
	Path     string
	Content  string
	Expected string // For expected values in assertions like json_field_equals
	Size     int64
	TarEntry string // For tar-specific assertions, the entry path within the tar
	Owner    string // For ownership assertions (uid:gid format)
	Mode     string // For file mode assertions (octal format)
	PaxKey   string // For pax extended attribute key
}

type DebugSpec struct {
	TarFiles []string // List of tar files to print contents for debugging
}

type TestFramework struct {
	imgBinaryPath string
	tempDir       string
	testdataDir   string
	t             *testing.T
	Verbose       bool
}

type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error
}

func NewTestFramework(t *testing.T) (*TestFramework, error) {
	rf, err := runfiles.New()
	if err != nil {
		return nil, fmt.Errorf("failed to create runfiles: %w", err)
	}

	imgBinaryPath, err := rf.Rlocation("rules_img_tool/cmd/img/img_/img")
	if err != nil {
		return nil, fmt.Errorf("failed to locate img binary: %w", err)
	}

	testdataDir, err := rf.Rlocation("_main/testdata")
	if err != nil {
		return nil, fmt.Errorf("failed to locate testdata directory: %w", err)
	}

	tempDir, err := os.MkdirTemp("", "img_toolchain_test_")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}

	return &TestFramework{
		imgBinaryPath: imgBinaryPath,
		tempDir:       tempDir,
		testdataDir:   testdataDir,
		t:             t,
	}, nil
}

func (tf *TestFramework) Cleanup() {
	if tf.tempDir != "" {
		os.RemoveAll(tf.tempDir)
	}
}

func (tf *TestFramework) LoadTestCase(filename string) (*TestCase, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to open test file %s: %w", filename, err)
	}
	defer file.Close()

	testCase := &TestCase{
		Setup: SetupSpec{
			Files:         make(map[string]string),
			TestdataFiles: make(map[string]string),
			Symlinks:      make(map[string]string),
		},
	}

	scanner := bufio.NewScanner(file)
	var currentSection string
	var fileContent strings.Builder
	var currentFileName string

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip comments and empty lines
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Handle sections
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			// Save previous file content if we were reading a file
			if currentSection == "file" && currentFileName != "" {
				content := strings.TrimSuffix(fileContent.String(), "\n")
				testCase.Setup.Files[currentFileName] = content
				fileContent.Reset()
				currentFileName = ""
			}
			currentSection = strings.Trim(line, "[]")
			continue
		}

		// Handle different sections
		switch currentSection {
		case "test":
			key, value := parseKeyValue(line)
			switch key {
			case "name":
				testCase.Name = value
			case "description":
				testCase.Description = value
			}
		case "debug":
			key, value := parseKeyValue(line)
			if key == "tar_debug" {
				testCase.Debug.TarFiles = append(testCase.Debug.TarFiles, value)
			}
		case "command":
			key, value := parseKeyValue(line)
			switch key {
			case "subcommand":
				testCase.Command.Subcommand = value
			case "args":
				testCase.Command.Args = parseArgs(value)
			case "expect_exit":
				fmt.Sscanf(value, "%d", &testCase.Command.ExpectExit)
			case "stdin":
				testCase.Command.Stdin = value
			case "verbose":
				switch strings.ToLower(value) {
				case "true", "1", "yes":
					testCase.Command.Verbose = true
				default:
					testCase.Command.Verbose = false
				}
			}
		case "file":
			key, value := parseKeyValue(line)
			if key == "name" {
				if currentFileName != "" {
					content := strings.TrimSuffix(fileContent.String(), "\n")
					testCase.Setup.Files[currentFileName] = content
					fileContent.Reset()
				}
				currentFileName = value
			} else {
				if fileContent.Len() > 0 {
					fileContent.WriteString("\n")
				}
				fileContent.WriteString(line)
			}
		case "symlinks":
			key, value := parseKeyValue(line)
			if key == "create" {
				// Format: create = dest_path=target
				parts := strings.SplitN(value, "=", 2)
				if len(parts) == 2 {
					destPath := strings.TrimSpace(parts[0])
					target := strings.TrimSpace(parts[1])
					testCase.Setup.Symlinks[destPath] = target
				}
			}
		case "testdata":
			key, value := parseKeyValue(line)
			if key == "copy" {
				// Format: copy = dest_path=src_path_in_testdata
				parts := strings.SplitN(value, "=", 2)
				if len(parts) == 2 {
					destPath := strings.TrimSpace(parts[0])
					srcPath := strings.TrimSpace(parts[1])
					testCase.Setup.TestdataFiles[destPath] = srcPath
				}
			}
		case "assert":
			assertion := parseAssertion(line)
			if assertion != nil {
				testCase.Assertions = append(testCase.Assertions, *assertion)
			}
		}
	}

	// Save last file if we were reading one
	if currentSection == "file" && currentFileName != "" {
		content := strings.TrimSuffix(fileContent.String(), "\n")
		testCase.Setup.Files[currentFileName] = content
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading test file %s: %w", filename, err)
	}

	return testCase, nil
}

func parseKeyValue(line string) (string, string) {
	parts := strings.SplitN(line, "=", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
}

func parseArgs(argsStr string) []string {
	// Simple space-based splitting for now
	if argsStr == "" {
		return nil
	}
	return strings.Fields(argsStr)
}

func parseAssertion(line string) *AssertionSpec {
	key, value := parseKeyValue(line)
	if key == "" {
		return nil
	}

	assertion := &AssertionSpec{Type: key}

	// Parse assertion value based on type
	switch key {
	case "file_exists", "file_not_exists":
		assertion.Path = value
	case "file_contains", "file_not_contains":
		parts := strings.SplitN(value, ",", 2)
		if len(parts) == 2 {
			assertion.Path = strings.TrimSpace(parts[0])
			assertion.Content = strings.Trim(strings.TrimSpace(parts[1]), `"`)
		}
	case "file_size_gt", "file_size_lt":
		parts := strings.SplitN(value, ",", 2)
		if len(parts) == 2 {
			assertion.Path = strings.TrimSpace(parts[0])
			fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &assertion.Size)
		}
	case "stdout_contains", "stdout_not_contains", "stderr_contains", "stderr_not_contains":
		assertion.Content = strings.Trim(value, `"`)
	case "exit_code":
		fmt.Sscanf(value, "%d", &assertion.Size)
	case "file_sha256":
		parts := strings.SplitN(value, ",", 2)
		if len(parts) == 2 {
			assertion.Path = strings.TrimSpace(parts[0])
			assertion.Content = strings.Trim(strings.TrimSpace(parts[1]), `"`)
		}
	case "file_valid_json", "file_valid_gzip", "file_valid_tar":
		assertion.Path = value
	case "json_field_equals", "json_field_exists":
		parts := strings.SplitN(value, ",", 3)
		if len(parts) >= 2 {
			assertion.Path = strings.TrimSpace(parts[0])
			assertion.Content = strings.TrimSpace(parts[1])
			if len(parts) == 3 {
				assertion.Expected = strings.TrimSpace(parts[2])
			}
		}
	case "stdout_matches_regex", "stderr_matches_regex":
		assertion.Content = strings.Trim(value, `"`)
	case "tar_entry_exists", "tar_entry_not_exists":
		// Format: tar_entry_exists = tarfile.tar.gz, /path/in/tar
		parts := strings.SplitN(value, ",", 2)
		if len(parts) == 2 {
			assertion.Path = strings.TrimSpace(parts[0])
			assertion.TarEntry = strings.TrimSpace(parts[1])
		}
	case "tar_entry_type":
		// Format: tar_entry_type = tarfile.tar.gz, /path/in/tar, regular|dir|symlink|link
		parts := strings.SplitN(value, ",", 3)
		if len(parts) == 3 {
			assertion.Path = strings.TrimSpace(parts[0])
			assertion.TarEntry = strings.TrimSpace(parts[1])
			assertion.Content = strings.TrimSpace(parts[2])
		}
	case "tar_entry_size":
		// Format: tar_entry_size = tarfile.tar.gz, /path/in/tar, 1024
		parts := strings.SplitN(value, ",", 3)
		if len(parts) == 3 {
			assertion.Path = strings.TrimSpace(parts[0])
			assertion.TarEntry = strings.TrimSpace(parts[1])
			fmt.Sscanf(strings.TrimSpace(parts[2]), "%d", &assertion.Size)
		}
	case "tar_entry_sha256":
		// Format: tar_entry_sha256 = tarfile.tar.gz, /path/in/tar, "hash"
		parts := strings.SplitN(value, ",", 3)
		if len(parts) == 3 {
			assertion.Path = strings.TrimSpace(parts[0])
			assertion.TarEntry = strings.TrimSpace(parts[1])
			assertion.Content = strings.Trim(strings.TrimSpace(parts[2]), `"`)
		}
	case "tar_entry_owner":
		// Format: tar_entry_owner = tarfile.tar.gz, /path/in/tar, 1000:1000
		parts := strings.SplitN(value, ",", 3)
		if len(parts) == 3 {
			assertion.Path = strings.TrimSpace(parts[0])
			assertion.TarEntry = strings.TrimSpace(parts[1])
			assertion.Owner = strings.TrimSpace(parts[2])
		}
	case "tar_entry_mode":
		// Format: tar_entry_mode = tarfile.tar.gz, /path/in/tar, 0644
		parts := strings.SplitN(value, ",", 3)
		if len(parts) == 3 {
			assertion.Path = strings.TrimSpace(parts[0])
			assertion.TarEntry = strings.TrimSpace(parts[1])
			assertion.Mode = strings.TrimSpace(parts[2])
		}
	case "tar_entry_pax":
		// Format: tar_entry_pax = tarfile.tar.gz, /path/in/tar, key, "expected_value"
		parts := strings.SplitN(value, ",", 4)
		if len(parts) == 4 {
			assertion.Path = strings.TrimSpace(parts[0])
			assertion.TarEntry = strings.TrimSpace(parts[1])
			assertion.PaxKey = strings.TrimSpace(parts[2])
			assertion.Content = strings.Trim(strings.TrimSpace(parts[3]), `"`)
		}
	case "layer_invariants_intact":
		// Format: layer_invariants_intact = tarfile.tar.gz
		assertion.Path = value
	}

	return assertion
}

func (tf *TestFramework) SetupFiles(setup SetupSpec) error {
	// Setup regular files
	for filename, content := range setup.Files {
		fullPath := filepath.Join(tf.tempDir, filename)
		dir := filepath.Dir(fullPath)

		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}

		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			return fmt.Errorf("failed to write file %s: %w", fullPath, err)
		}
	}

	// Setup testdata files
	for destPath, srcPath := range setup.TestdataFiles {
		srcFullPath := filepath.Join(tf.testdataDir, srcPath)
		destFullPath := filepath.Join(tf.tempDir, destPath)
		destDir := filepath.Dir(destFullPath)

		if err := os.MkdirAll(destDir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", destDir, err)
		}

		srcData, err := os.ReadFile(srcFullPath)
		if err != nil {
			return fmt.Errorf("failed to read testdata file %s: %w", srcFullPath, err)
		}

		if err := os.WriteFile(destFullPath, srcData, 0644); err != nil {
			return fmt.Errorf("failed to write testdata file %s: %w", destFullPath, err)
		}
	}

	// Setup symlinks
	for destPath, target := range setup.Symlinks {
		destFullPath := filepath.Join(tf.tempDir, destPath)
		destDir := filepath.Dir(destFullPath)

		if err := os.MkdirAll(destDir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", destDir, err)
		}

		if err := os.Symlink(target, destFullPath); err != nil {
			return fmt.Errorf("failed to create symlink %s to %s: %w", destFullPath, target, err)
		}
	}

	return nil
}

func (tf *TestFramework) RunCommand(ctx context.Context, cmd CommandSpec) (*CommandResult, error) {
	args := append([]string{cmd.Subcommand}, cmd.Args...)
	execCmd := exec.CommandContext(ctx, tf.imgBinaryPath, args...)
	execCmd.Dir = tf.tempDir

	if cmd.Stdin != "" {
		execCmd.Stdin = strings.NewReader(cmd.Stdin)
	}

	stdout, stderr, err := tf.runCommand(execCmd)

	result := &CommandResult{
		Stdout: string(stdout),
		Stderr: string(stderr),
		Err:    err,
	}

	if cmd.Verbose {
		tf.t.Logf("Command: %s %s", tf.imgBinaryPath, strings.Join(args, " "))
		tf.t.Logf("Stdout:\n%s", result.Stdout)
		tf.t.Logf("Stderr:\n%s", result.Stderr)
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = 1
		}
	} else {
		result.ExitCode = 0
	}

	if result.ExitCode != cmd.ExpectExit {
		return result, fmt.Errorf("expected exit code %d, got %d", cmd.ExpectExit, result.ExitCode)
	}

	return result, nil
}

func (tf *TestFramework) runCommand(cmd *exec.Cmd) (stdout, stderr []byte, err error) {
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}

	stdout, readErr1 := tf.readAll(stdoutPipe)
	stderr, readErr2 := tf.readAll(stderrPipe)

	err = cmd.Wait()

	if readErr1 != nil {
		return nil, nil, readErr1
	}
	if readErr2 != nil {
		return nil, nil, readErr2
	}

	return stdout, stderr, err
}

func (tf *TestFramework) readAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var result []byte
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			result = append(result, buf[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, err
		}
	}
	return result, nil
}

func (tf *TestFramework) CheckAssertions(assertions []AssertionSpec, result *CommandResult) error {
	for _, assertion := range assertions {
		if err := tf.checkAssertion(assertion, result); err != nil {
			return fmt.Errorf("assertion failed (%s): %w", assertion.Type, err)
		}
	}
	return nil
}

// TarEntryInfo holds information about a tar entry
type TarEntryInfo struct {
	Index   int
	Header  *tar.Header
	Content []byte
}

// readTarEntries reads all entries from a tar file (optionally gzipped)
func (tf *TestFramework) readTarEntries(tarPath string) (map[string]*TarEntryInfo, error) {
	fullPath := filepath.Join(tf.tempDir, tarPath)
	file, err := os.Open(fullPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open tar file %s: %w", tarPath, err)
	}
	defer file.Close()

	var reader io.Reader = file

	// Try to detect if it's gzipped
	file.Seek(0, 0)
	gzHeader := make([]byte, 2)
	file.Read(gzHeader)
	file.Seek(0, 0)

	if gzHeader[0] == 0x1f && gzHeader[1] == 0x8b {
		gzReader, err := gzip.NewReader(file)
		if err != nil {
			return nil, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer gzReader.Close()
		reader = gzReader
	}

	tarReader := tar.NewReader(reader)
	entries := make(map[string]*TarEntryInfo)

	index := 0
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("error reading tar: %w", err)
		}

		var content []byte
		if header.Typeflag == tar.TypeReg {
			content, err = io.ReadAll(tarReader)
			if err != nil {
				return nil, fmt.Errorf("error reading file content for %s: %w", header.Name, err)
			}
		}

		entries[header.Name] = &TarEntryInfo{
			Index:   index,
			Header:  header,
			Content: content,
		}
		index++
	}

	return entries, nil
}

func (tf *TestFramework) checkAssertion(assertion AssertionSpec, result *CommandResult) error {
	switch assertion.Type {
	case "file_exists":
		fullPath := filepath.Join(tf.tempDir, assertion.Path)
		if _, err := os.Stat(fullPath); os.IsNotExist(err) {
			return fmt.Errorf("file %s does not exist", assertion.Path)
		}
	case "file_not_exists":
		fullPath := filepath.Join(tf.tempDir, assertion.Path)
		if _, err := os.Stat(fullPath); err == nil {
			return fmt.Errorf("file %s exists but should not", assertion.Path)
		}
	case "file_contains":
		fullPath := filepath.Join(tf.tempDir, assertion.Path)
		content, err := os.ReadFile(fullPath)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %w", assertion.Path, err)
		}
		if !strings.Contains(string(content), assertion.Content) {
			errMsg := fmt.Sprintf("file %s does not contain %q", assertion.Path, assertion.Content)
			if tf.Verbose {
				errMsg += fmt.Sprintf("\nGot:\n%s", string(content))
			}
			return fmt.Errorf("%s", errMsg)
		}
	case "file_not_contains":
		fullPath := filepath.Join(tf.tempDir, assertion.Path)
		content, err := os.ReadFile(fullPath)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %w", assertion.Path, err)
		}
		if strings.Contains(string(content), assertion.Content) {
			errMsg := fmt.Sprintf("file %s contains %q but should not", assertion.Path, assertion.Content)
			if tf.Verbose {
				errMsg += fmt.Sprintf("\nGot:\n%s", string(content))
			}
			return fmt.Errorf("%s", errMsg)
		}
	case "file_size_gt":
		fullPath := filepath.Join(tf.tempDir, assertion.Path)
		info, err := os.Stat(fullPath)
		if err != nil {
			return fmt.Errorf("failed to stat file %s: %w", assertion.Path, err)
		}
		if info.Size() <= assertion.Size {
			return fmt.Errorf("file %s size %d not greater than %d", assertion.Path, info.Size(), assertion.Size)
		}
	case "file_size_lt":
		fullPath := filepath.Join(tf.tempDir, assertion.Path)
		info, err := os.Stat(fullPath)
		if err != nil {
			return fmt.Errorf("failed to stat file %s: %w", assertion.Path, err)
		}
		if info.Size() >= assertion.Size {
			return fmt.Errorf("file %s size %d not less than %d", assertion.Path, info.Size(), assertion.Size)
		}
	case "stdout_contains":
		if !strings.Contains(result.Stdout, assertion.Content) {
			errMsg := fmt.Sprintf("stdout does not contain %q", assertion.Content)
			if tf.Verbose {
				errMsg += fmt.Sprintf("\nGot:\n%s", result.Stdout)
			}
			return fmt.Errorf("%s", errMsg)
		}
	case "stdout_not_contains":
		if strings.Contains(result.Stdout, assertion.Content) {
			errMsg := fmt.Sprintf("stdout contains %q but should not", assertion.Content)
			if tf.Verbose {
				errMsg += fmt.Sprintf("\nGot:\n%s", result.Stdout)
			}
			return fmt.Errorf("%s", errMsg)
		}
	case "stderr_contains":
		if !strings.Contains(result.Stderr, assertion.Content) {
			errMsg := fmt.Sprintf("stderr does not contain %q", assertion.Content)
			if tf.Verbose {
				errMsg += fmt.Sprintf("\nGot:\n%s", result.Stderr)
			}
			return fmt.Errorf("%s", errMsg)
		}
	case "stderr_not_contains":
		if strings.Contains(result.Stderr, assertion.Content) {
			errMsg := fmt.Sprintf("stderr contains %q but should not", assertion.Content)
			if tf.Verbose {
				errMsg += fmt.Sprintf("\nGot:\n%s", result.Stderr)
			}
			return fmt.Errorf("%s", errMsg)
		}
	case "exit_code":
		// This is already checked in RunCommand, but we can add it here for completeness
		expectedCode := int(assertion.Size) // Reuse Size field for exit code
		if result.ExitCode != expectedCode {
			return fmt.Errorf("expected exit code %d, got %d", expectedCode, result.ExitCode)
		}
	case "file_sha256":
		fullPath := filepath.Join(tf.tempDir, assertion.Path)
		content, err := os.ReadFile(fullPath)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %w", assertion.Path, err)
		}
		hash := sha256.Sum256(content)
		actualHash := hex.EncodeToString(hash[:])
		expectedHash := strings.ToLower(assertion.Content)
		if actualHash != expectedHash {
			errMsg := fmt.Sprintf("file %s hash mismatch: expected %s, got %s", assertion.Path, expectedHash, actualHash)
			if tf.Verbose {
				errMsg += fmt.Sprintf("\nFile contents:\n%s", string(content))
			}
			return fmt.Errorf("%s", errMsg)
		}
	case "file_valid_json":
		fullPath := filepath.Join(tf.tempDir, assertion.Path)
		content, err := os.ReadFile(fullPath)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %w", assertion.Path, err)
		}
		var jsonData interface{}
		if err := json.Unmarshal(content, &jsonData); err != nil {
			return fmt.Errorf("file %s is not valid JSON: %w", assertion.Path, err)
		}
	case "file_valid_gzip":
		fullPath := filepath.Join(tf.tempDir, assertion.Path)
		file, err := os.Open(fullPath)
		if err != nil {
			return fmt.Errorf("failed to open file %s: %w", assertion.Path, err)
		}
		defer file.Close()
		gzReader, err := gzip.NewReader(file)
		if err != nil {
			return fmt.Errorf("file %s is not valid gzip: %w", assertion.Path, err)
		}
		gzReader.Close()
	case "json_field_equals":
		fullPath := filepath.Join(tf.tempDir, assertion.Path)
		content, err := os.ReadFile(fullPath)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %w", assertion.Path, err)
		}
		var jsonData map[string]interface{}
		if err := json.Unmarshal(content, &jsonData); err != nil {
			return fmt.Errorf("file %s is not valid JSON: %w", assertion.Path, err)
		}
		field := assertion.Content
		value, exists := jsonData[field]
		if !exists {
			errMsg := fmt.Sprintf("JSON field %s does not exist in file %s", field, assertion.Path)
			if tf.Verbose {
				errMsg += fmt.Sprintf("\nFile contents:\n%s", string(content))
			}
			return fmt.Errorf("%s", errMsg)
		}
		// Convert to string and compare
		actualValue := fmt.Sprintf("%v", value)
		expectedValue := assertion.Expected
		if actualValue != expectedValue {
			errMsg := fmt.Sprintf("JSON field %s in file %s: expected %s, got %s", field, assertion.Path, expectedValue, actualValue)
			if tf.Verbose {
				errMsg += fmt.Sprintf("\nFile contents:\n%s", string(content))
			}
			return fmt.Errorf("%s", errMsg)
		}
	case "json_field_exists":
		fullPath := filepath.Join(tf.tempDir, assertion.Path)
		content, err := os.ReadFile(fullPath)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %w", assertion.Path, err)
		}
		var jsonData map[string]interface{}
		if err := json.Unmarshal(content, &jsonData); err != nil {
			return fmt.Errorf("file %s is not valid JSON: %w", assertion.Path, err)
		}
		field := assertion.Content
		if _, exists := jsonData[field]; !exists {
			return fmt.Errorf("JSON field %s does not exist in file %s", field, assertion.Path)
		}
	case "stdout_matches_regex", "stderr_matches_regex":
		var text string
		if assertion.Type == "stdout_matches_regex" {
			text = result.Stdout
		} else {
			text = result.Stderr
		}
		matched, err := regexp.MatchString(assertion.Content, text)
		if err != nil {
			return fmt.Errorf("invalid regex %s: %w", assertion.Content, err)
		}
		if !matched {
			return fmt.Errorf("text does not match regex %s", assertion.Content)
		}
	case "tar_entry_exists":
		entries, err := tf.readTarEntries(assertion.Path)
		if err != nil {
			return fmt.Errorf("failed to read tar file %s: %w", assertion.Path, err)
		}
		if _, exists := entries[assertion.TarEntry]; !exists {
			tf.PrintTarContents(assertion.Path)
			return fmt.Errorf("tar entry %s does not exist in %s", assertion.TarEntry, assertion.Path)
		}
	case "tar_entry_not_exists":
		entries, err := tf.readTarEntries(assertion.Path)
		if err != nil {
			return fmt.Errorf("failed to read tar file %s: %w", assertion.Path, err)
		}
		if _, exists := entries[assertion.TarEntry]; exists {
			tf.PrintTarContents(assertion.Path)
			return fmt.Errorf("tar entry %s exists in %s but should not", assertion.TarEntry, assertion.Path)
		}
	case "tar_entry_type":
		entries, err := tf.readTarEntries(assertion.Path)
		if err != nil {
			return fmt.Errorf("failed to read tar file %s: %w", assertion.Path, err)
		}
		entry, exists := entries[assertion.TarEntry]
		if !exists {
			tf.PrintTarContents(assertion.Path)
			return fmt.Errorf("tar entry %s does not exist in %s", assertion.TarEntry, assertion.Path)
		}

		switch assertion.Content {
		case "regular":
			if entry.Header.Typeflag != tar.TypeReg {
				tf.PrintTarContents(assertion.Path)
				return fmt.Errorf("tar entry %s is not a regular file (typeflag: %d)", assertion.TarEntry, entry.Header.Typeflag)
			}
		case "dir":
			if entry.Header.Typeflag != tar.TypeDir {
				tf.PrintTarContents(assertion.Path)
				return fmt.Errorf("tar entry %s is not a directory (typeflag: %d)", assertion.TarEntry, entry.Header.Typeflag)
			}
		case "symlink":
			if entry.Header.Typeflag != tar.TypeSymlink {
				tf.PrintTarContents(assertion.Path)
				return fmt.Errorf("tar entry %s is not a symlink (typeflag: %d)", assertion.TarEntry, entry.Header.Typeflag)
			}
		case "link":
			if entry.Header.Typeflag != tar.TypeLink {
				tf.PrintTarContents(assertion.Path)
				return fmt.Errorf("tar entry %s is not a hardlink (typeflag: %d)", assertion.TarEntry, entry.Header.Typeflag)
			}
		default:
			return fmt.Errorf("unknown tar entry type: %s", assertion.Content)
		}
	case "tar_entry_size":
		entries, err := tf.readTarEntries(assertion.Path)
		if err != nil {
			return fmt.Errorf("failed to read tar file %s: %w", assertion.Path, err)
		}
		entry, exists := entries[assertion.TarEntry]
		if !exists {
			tf.PrintTarContents(assertion.Path)
			return fmt.Errorf("tar entry %s does not exist in %s", assertion.TarEntry, assertion.Path)
		}
		if entry.Header.Size != assertion.Size {
			tf.PrintTarContents(assertion.Path)
			return fmt.Errorf("tar entry %s size mismatch: expected %d, got %d", assertion.TarEntry, assertion.Size, entry.Header.Size)
		}
	case "tar_entry_sha256":
		entries, err := tf.readTarEntries(assertion.Path)
		if err != nil {
			return fmt.Errorf("failed to read tar file %s: %w", assertion.Path, err)
		}
		entry, exists := entries[assertion.TarEntry]
		if !exists {
			tf.PrintTarContents(assertion.Path)
			return fmt.Errorf("tar entry %s does not exist in %s", assertion.TarEntry, assertion.Path)
		}
		if entry.Header.Typeflag != tar.TypeReg {
			tf.PrintTarContents(assertion.Path)
			return fmt.Errorf("tar entry %s is not a regular file, cannot check SHA256", assertion.TarEntry)
		}
		hash := sha256.Sum256(entry.Content)
		actualHash := hex.EncodeToString(hash[:])
		expectedHash := strings.ToLower(assertion.Content)
		if actualHash != expectedHash {
			tf.PrintTarContents(assertion.Path)
			return fmt.Errorf("tar entry %s SHA256 mismatch: expected %s, got %s", assertion.TarEntry, expectedHash, actualHash)
		}
	case "tar_entry_owner":
		entries, err := tf.readTarEntries(assertion.Path)
		if err != nil {
			return fmt.Errorf("failed to read tar file %s: %w", assertion.Path, err)
		}
		entry, exists := entries[assertion.TarEntry]
		if !exists {
			tf.PrintTarContents(assertion.Path)
			return fmt.Errorf("tar entry %s does not exist in %s", assertion.TarEntry, assertion.Path)
		}

		parts := strings.SplitN(assertion.Owner, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid owner format: %s (expected uid:gid)", assertion.Owner)
		}

		expectedUID, err := strconv.Atoi(parts[0])
		if err != nil {
			return fmt.Errorf("invalid UID in owner: %s", parts[0])
		}
		expectedGID, err := strconv.Atoi(parts[1])
		if err != nil {
			return fmt.Errorf("invalid GID in owner: %s", parts[1])
		}

		if entry.Header.Uid != expectedUID {
			tf.PrintTarContents(assertion.Path)
			return fmt.Errorf("tar entry %s UID mismatch: expected %d, got %d", assertion.TarEntry, expectedUID, entry.Header.Uid)
		}
		if entry.Header.Gid != expectedGID {
			tf.PrintTarContents(assertion.Path)
			return fmt.Errorf("tar entry %s GID mismatch: expected %d, got %d", assertion.TarEntry, expectedGID, entry.Header.Gid)
		}
	case "tar_entry_mode":
		entries, err := tf.readTarEntries(assertion.Path)
		if err != nil {
			return fmt.Errorf("failed to read tar file %s: %w", assertion.Path, err)
		}
		entry, exists := entries[assertion.TarEntry]
		if !exists {
			tf.PrintTarContents(assertion.Path)
			return fmt.Errorf("tar entry %s does not exist in %s", assertion.TarEntry, assertion.Path)
		}

		expectedMode, err := strconv.ParseInt(assertion.Mode, 8, 64)
		if err != nil {
			return fmt.Errorf("invalid mode format: %s (expected octal)", assertion.Mode)
		}

		// Compare only the permission bits (lower 12 bits)
		actualMode := int64(entry.Header.Mode) & 0o7777
		expectedMode = expectedMode & 0o7777

		if actualMode != expectedMode {
			tf.PrintTarContents(assertion.Path)
			return fmt.Errorf("tar entry %s mode mismatch: expected %o, got %o", assertion.TarEntry, expectedMode, actualMode)
		}
	case "tar_entry_pax":
		entries, err := tf.readTarEntries(assertion.Path)
		if err != nil {
			return fmt.Errorf("failed to read tar file %s: %w", assertion.Path, err)
		}
		entry, exists := entries[assertion.TarEntry]
		if !exists {
			tf.PrintTarContents(assertion.Path)
			return fmt.Errorf("tar entry %s does not exist in %s", assertion.TarEntry, assertion.Path)
		}

		if entry.Header.PAXRecords == nil {
			tf.PrintTarContents(assertion.Path)
			return fmt.Errorf("tar entry %s has no PAX extended attributes", assertion.TarEntry)
		}

		actualValue, exists := entry.Header.PAXRecords[assertion.PaxKey]
		if !exists {
			tf.PrintTarContents(assertion.Path)
			return fmt.Errorf("tar entry %s does not have PAX attribute %s", assertion.TarEntry, assertion.PaxKey)
		}

		if actualValue != assertion.Content {
			tf.PrintTarContents(assertion.Path)
			return fmt.Errorf("tar entry %s PAX attribute %s mismatch: expected %q, got %q",
				assertion.TarEntry, assertion.PaxKey, assertion.Content, actualValue)
		}
	case "layer_invariants_intact":
		// Check various layer invariants to ensure the tar file is well-formed
		if err := tf.checkLayerInvariants(assertion.Path); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown assertion type: %s", assertion.Type)
	}
	return nil
}

// checkLayerInvariants verifies that a layer tar file satisfies required invariants
func (tf *TestFramework) checkLayerInvariants(tarPath string) error {
	fullPath := filepath.Join(tf.tempDir, tarPath)
	file, err := os.Open(fullPath)
	if err != nil {
		return fmt.Errorf("failed to open tar file %s: %w", tarPath, err)
	}
	defer file.Close()

	var reader io.Reader = file

	// Try to detect if it's gzipped
	file.Seek(0, 0)
	gzHeader := make([]byte, 2)
	file.Read(gzHeader)
	file.Seek(0, 0)

	if gzHeader[0] == 0x1f && gzHeader[1] == 0x8b {
		gzReader, err := gzip.NewReader(file)
		if err != nil {
			return fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer gzReader.Close()
		reader = gzReader
	}

	tarReader := tar.NewReader(reader)

	// Track entries we've seen for various checks
	seenNames := make(map[string]bool)
	seenRegularFiles := make(map[string]bool)
	var allEntries []string // Track all entries in order for topological check
	var duplicates []string
	var hardlinkErrors []string
	var normalizationErrors []string
	var deduplicationErrors []string
	var topologicalErrors []string
	// Track content+metadata hashes for deduplication check
	// Maps hash -> first entry name with that hash
	seenContentHashes := make(map[string]string)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("error reading tar: %w", err)
		}

		// Check 1: No duplicate entry names
		if seenNames[header.Name] {
			duplicates = append(duplicates, header.Name)
		}
		seenNames[header.Name] = true
		allEntries = append(allEntries, header.Name)

		// Track regular files
		if header.Typeflag == tar.TypeReg {
			seenRegularFiles[header.Name] = true
		}

		// Check 2: Hardlink validation
		if header.Typeflag == tar.TypeLink {
			linkTarget := header.Linkname

			// Check if the target exists in entries we've already seen
			if !seenNames[linkTarget] {
				hardlinkErrors = append(hardlinkErrors,
					fmt.Sprintf("hardlink %s points to %s which does not exist or appears later in tar",
						header.Name, linkTarget))
			} else if !seenRegularFiles[linkTarget] {
				hardlinkErrors = append(hardlinkErrors,
					fmt.Sprintf("hardlink %s points to %s which is not a regular file",
						header.Name, linkTarget))
			}
		}

		// Check 3: Entry name normalization
		entryName := header.Name
		isDir := header.Typeflag == tar.TypeDir

		// 3a. Check for absolute paths or current/parent directory references at start
		if strings.HasPrefix(entryName, "/") {
			normalizationErrors = append(normalizationErrors,
				fmt.Sprintf("entry %s starts with '/' (absolute path not allowed)", entryName))
		} else if entryName == "." || entryName == ".." {
			normalizationErrors = append(normalizationErrors,
				fmt.Sprintf("entry name cannot be %q", entryName))
		} else if strings.HasPrefix(entryName, "./") || strings.HasPrefix(entryName, "../") {
			normalizationErrors = append(normalizationErrors,
				fmt.Sprintf("entry %s starts with './' or '../' (relative path references not allowed)", entryName))
		}

		// 3b. Check that no path component is "." or ".."
		// Remove trailing slash for directory check to avoid empty component
		pathToCheck := entryName
		if isDir && strings.HasSuffix(pathToCheck, "/") {
			pathToCheck = strings.TrimSuffix(pathToCheck, "/")
		}
		components := strings.Split(pathToCheck, "/")
		for _, component := range components {
			if component == "." || component == ".." {
				normalizationErrors = append(normalizationErrors,
					fmt.Sprintf("entry %s contains path component %q", entryName, component))
				break
			}
		}

		// 3c. Check trailing slash convention
		if isDir {
			if !strings.HasSuffix(entryName, "/") {
				normalizationErrors = append(normalizationErrors,
					fmt.Sprintf("directory entry %s must end with '/'", entryName))
			}
		} else {
			if strings.HasSuffix(entryName, "/") {
				normalizationErrors = append(normalizationErrors,
					fmt.Sprintf("non-directory entry %s must not end with '/'", entryName))
			}
		}

		// Check 4: Deduplication - regular files with identical content+metadata should use hardlinks
		if header.Typeflag == tar.TypeReg {
			// Read the file content
			content, err := io.ReadAll(tarReader)
			if err != nil {
				return fmt.Errorf("error reading content for %s: %w", header.Name, err)
			}

			// Create a hash of metadata + content
			// Include: size, mode, uid, gid, and content
			hasher := sha256.New()
			// Write metadata
			fmt.Fprintf(hasher, "size:%d|mode:%d|uid:%d|gid:%d|",
				header.Size, header.Mode, header.Uid, header.Gid)
			// Write content
			hasher.Write(content)
			contentHash := hex.EncodeToString(hasher.Sum(nil))

			// Check if we've seen this exact content+metadata before
			if firstEntry, seen := seenContentHashes[contentHash]; seen {
				deduplicationErrors = append(deduplicationErrors,
					fmt.Sprintf("entry %s has identical content and metadata to %s but is not a hardlink",
						header.Name, firstEntry))
			} else {
				seenContentHashes[contentHash] = header.Name
			}
		}

		// Check 5: Topological ordering - directories must come before their children
		if header.Typeflag == tar.TypeDir {
			// For each directory entry, check if we've already seen any of its children
			dirPath := header.Name
			for _, prevEntry := range allEntries {
				// Skip the directory itself
				if prevEntry == dirPath {
					continue
				}
				// Check if prevEntry is a child of this directory
				if strings.HasPrefix(prevEntry, dirPath) {
					topologicalErrors = append(topologicalErrors,
						fmt.Sprintf("directory %s appears after its child %s",
							dirPath, prevEntry))
					break // Only report first violation for this directory
				}
			}
		}
	}

	// Report any errors found
	if len(duplicates) > 0 {
		tf.PrintTarContents(tarPath)
		return fmt.Errorf("tar file %s contains duplicate entries: %v", tarPath, duplicates)
	}

	if len(hardlinkErrors) > 0 {
		tf.PrintTarContents(tarPath)
		return fmt.Errorf("tar file %s has hardlink invariant violations:\n  - %s",
			tarPath, strings.Join(hardlinkErrors, "\n  - "))
	}

	if len(normalizationErrors) > 0 {
		tf.PrintTarContents(tarPath)
		return fmt.Errorf("tar file %s has entry name normalization violations:\n  - %s",
			tarPath, strings.Join(normalizationErrors, "\n  - "))
	}

	if len(deduplicationErrors) > 0 {
		tf.PrintTarContents(tarPath)
		return fmt.Errorf("tar file %s has deduplication violations:\n  - %s",
			tarPath, strings.Join(deduplicationErrors, "\n  - "))
	}

	if len(topologicalErrors) > 0 {
		tf.PrintTarContents(tarPath)
		return fmt.Errorf("tar file %s has topological ordering violations:\n  - %s",
			tarPath, strings.Join(topologicalErrors, "\n  - "))
	}

	// Future checks can be added here, such as:
	// - Check for hardlink cycles (e.g., A -> B -> C -> A)
	// - Check for valid permissions/ownership ranges
	// - Check for suspicious file names (e.g., containing null bytes)
	// etc.

	return nil
}

// PrintTarContents prints the contents of a tar file to stdout for debugging
func (tf *TestFramework) PrintTarContents(tarPath string) error {
	entries, err := tf.readTarEntries(tarPath)
	if err != nil {
		return fmt.Errorf("failed to read tar file %s: %w", tarPath, err)
	}

	fmt.Printf("\n=== Contents of %s ===\n", tarPath)
	fmt.Printf("%-10s %-10s %-10s %-20s %s\n", "Mode", "UID/GID", "Size", "Type", "Name")
	fmt.Println(strings.Repeat("-", 80))

	// Sort entries by index
	type indexedEntry struct {
		name  string
		entry *TarEntryInfo
	}
	sorted := make([]indexedEntry, 0, len(entries))
	for name, entry := range entries {
		sorted = append(sorted, indexedEntry{name, entry})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].entry.Index < sorted[j].entry.Index
	})

	for _, item := range sorted {
		name := item.name
		entry := item.entry
		header := entry.Header

		// Format mode
		mode := fmt.Sprintf("%04o", header.Mode&0o7777)

		// Format ownership
		ownership := fmt.Sprintf("%d:%d", header.Uid, header.Gid)

		// Format type
		var typeStr string
		switch header.Typeflag {
		case tar.TypeReg:
			typeStr = "regular"
		case tar.TypeDir:
			typeStr = "dir"
		case tar.TypeSymlink:
			typeStr = fmt.Sprintf("symlink -> %s", header.Linkname)
		case tar.TypeLink:
			typeStr = fmt.Sprintf("link -> %s", header.Linkname)
		default:
			typeStr = fmt.Sprintf("type-%d", header.Typeflag)
		}

		fmt.Printf("%-10s %-10s %-10d %-20s %s\n",
			mode, ownership, header.Size, typeStr, name)

		// Print PAX records if present
		if header.PAXRecords != nil && len(header.PAXRecords) > 0 {
			for key, value := range header.PAXRecords {
				fmt.Printf("  PAX: %s = %s\n", key, value)
			}
		}
	}

	fmt.Printf("Total entries: %d\n\n", len(entries))
	return nil
}

func (tf *TestFramework) RunTestCase(ctx context.Context, testCase *TestCase) error {
	tf.t.Run(testCase.Name, func(t *testing.T) {
		if err := tf.SetupFiles(testCase.Setup); err != nil {
			t.Fatalf("Setup failed: %v", err)
		}

		result, err := tf.RunCommand(ctx, testCase.Command)
		if err != nil {
			t.Fatalf("Command execution failed: %v\nStdout: %s\nStderr: %s",
				err, result.Stdout, result.Stderr)
		}

		// Print tar contents for debugging if requested
		for _, tarFile := range testCase.Debug.TarFiles {
			if err := tf.PrintTarContents(tarFile); err != nil {
				t.Logf("Warning: could not print tar contents for %s: %v", tarFile, err)
			}
		}

		if err := tf.CheckAssertions(testCase.Assertions, result); err != nil {
			t.Fatalf("Assertions failed: %v\nStdout: %s\nStderr: %s",
				err, result.Stdout, result.Stderr)
		}
	})
	return nil
}
