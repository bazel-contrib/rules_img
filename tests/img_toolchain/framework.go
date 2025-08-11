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
}

type SetupSpec struct {
	Files         map[string]string
	TestdataFiles map[string]string // Maps destination path -> testdata source path
}

type CommandSpec struct {
	Subcommand string
	Args       []string
	ExpectExit int
	Stdin      string
}

type AssertionSpec struct {
	Type       string
	Path       string
	Content    string
	Size       int64
	TarEntry   string // For tar-specific assertions, the entry path within the tar
	Owner      string // For ownership assertions (uid:gid format)
	Mode       string // For file mode assertions (octal format)
	PaxKey     string // For pax extended attribute key
}

type TestFramework struct {
	imgBinaryPath string
	tempDir       string
	testdataDir   string
	t             *testing.T
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

	imgBinaryPath, err := rf.Rlocation("_main/cmd/img/img_/img")
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
				assertion.Size = int64(len(strings.TrimSpace(parts[2]))) // Store expected value in Size field as length
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
			Header:  header,
			Content: content,
		}
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
			return fmt.Errorf("file %s does not contain %q", assertion.Path, assertion.Content)
		}
	case "file_not_contains":
		fullPath := filepath.Join(tf.tempDir, assertion.Path)
		content, err := os.ReadFile(fullPath)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %w", assertion.Path, err)
		}
		if strings.Contains(string(content), assertion.Content) {
			return fmt.Errorf("file %s contains %q but should not", assertion.Path, assertion.Content)
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
			return fmt.Errorf("stdout does not contain %q", assertion.Content)
		}
	case "stdout_not_contains":
		if strings.Contains(result.Stdout, assertion.Content) {
			return fmt.Errorf("stdout contains %q but should not", assertion.Content)
		}
	case "stderr_contains":
		if !strings.Contains(result.Stderr, assertion.Content) {
			return fmt.Errorf("stderr does not contain %q", assertion.Content)
		}
	case "stderr_not_contains":
		if strings.Contains(result.Stderr, assertion.Content) {
			return fmt.Errorf("stderr contains %q but should not", assertion.Content)
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
			return fmt.Errorf("file %s hash mismatch: expected %s, got %s", assertion.Path, expectedHash, actualHash)
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
		if value, exists := jsonData[field]; !exists {
			return fmt.Errorf("JSON field %s does not exist in file %s", field, assertion.Path)
		} else {
			// For now, just convert to string and compare
			actualValue := fmt.Sprintf("%v", value)
			expectedValue := string(rune(assertion.Size)) // This is a hack - we need a better way to store expected values
			if actualValue != expectedValue {
				return fmt.Errorf("JSON field %s in file %s: expected %s, got %s", field, assertion.Path, expectedValue, actualValue)
			}
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
			return fmt.Errorf("tar entry %s does not exist in %s", assertion.TarEntry, assertion.Path)
		}
	case "tar_entry_not_exists":
		entries, err := tf.readTarEntries(assertion.Path)
		if err != nil {
			return fmt.Errorf("failed to read tar file %s: %w", assertion.Path, err)
		}
		if _, exists := entries[assertion.TarEntry]; exists {
			return fmt.Errorf("tar entry %s exists in %s but should not", assertion.TarEntry, assertion.Path)
		}
	case "tar_entry_type":
		entries, err := tf.readTarEntries(assertion.Path)
		if err != nil {
			return fmt.Errorf("failed to read tar file %s: %w", assertion.Path, err)
		}
		entry, exists := entries[assertion.TarEntry]
		if !exists {
			return fmt.Errorf("tar entry %s does not exist in %s", assertion.TarEntry, assertion.Path)
		}

		switch assertion.Content {
		case "regular":
			if entry.Header.Typeflag != tar.TypeReg {
				return fmt.Errorf("tar entry %s is not a regular file (typeflag: %d)", assertion.TarEntry, entry.Header.Typeflag)
			}
		case "dir":
			if entry.Header.Typeflag != tar.TypeDir {
				return fmt.Errorf("tar entry %s is not a directory (typeflag: %d)", assertion.TarEntry, entry.Header.Typeflag)
			}
		case "symlink":
			if entry.Header.Typeflag != tar.TypeSymlink {
				return fmt.Errorf("tar entry %s is not a symlink (typeflag: %d)", assertion.TarEntry, entry.Header.Typeflag)
			}
		case "link":
			if entry.Header.Typeflag != tar.TypeLink {
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
			return fmt.Errorf("tar entry %s does not exist in %s", assertion.TarEntry, assertion.Path)
		}
		if entry.Header.Size != assertion.Size {
			return fmt.Errorf("tar entry %s size mismatch: expected %d, got %d", assertion.TarEntry, assertion.Size, entry.Header.Size)
		}
	case "tar_entry_sha256":
		entries, err := tf.readTarEntries(assertion.Path)
		if err != nil {
			return fmt.Errorf("failed to read tar file %s: %w", assertion.Path, err)
		}
		entry, exists := entries[assertion.TarEntry]
		if !exists {
			return fmt.Errorf("tar entry %s does not exist in %s", assertion.TarEntry, assertion.Path)
		}
		if entry.Header.Typeflag != tar.TypeReg {
			return fmt.Errorf("tar entry %s is not a regular file, cannot check SHA256", assertion.TarEntry)
		}
		hash := sha256.Sum256(entry.Content)
		actualHash := hex.EncodeToString(hash[:])
		expectedHash := strings.ToLower(assertion.Content)
		if actualHash != expectedHash {
			return fmt.Errorf("tar entry %s SHA256 mismatch: expected %s, got %s", assertion.TarEntry, expectedHash, actualHash)
		}
	case "tar_entry_owner":
		entries, err := tf.readTarEntries(assertion.Path)
		if err != nil {
			return fmt.Errorf("failed to read tar file %s: %w", assertion.Path, err)
		}
		entry, exists := entries[assertion.TarEntry]
		if !exists {
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
			return fmt.Errorf("tar entry %s UID mismatch: expected %d, got %d", assertion.TarEntry, expectedUID, entry.Header.Uid)
		}
		if entry.Header.Gid != expectedGID {
			return fmt.Errorf("tar entry %s GID mismatch: expected %d, got %d", assertion.TarEntry, expectedGID, entry.Header.Gid)
		}
	case "tar_entry_mode":
		entries, err := tf.readTarEntries(assertion.Path)
		if err != nil {
			return fmt.Errorf("failed to read tar file %s: %w", assertion.Path, err)
		}
		entry, exists := entries[assertion.TarEntry]
		if !exists {
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
			return fmt.Errorf("tar entry %s mode mismatch: expected %o, got %o", assertion.TarEntry, expectedMode, actualMode)
		}
	case "tar_entry_pax":
		entries, err := tf.readTarEntries(assertion.Path)
		if err != nil {
			return fmt.Errorf("failed to read tar file %s: %w", assertion.Path, err)
		}
		entry, exists := entries[assertion.TarEntry]
		if !exists {
			return fmt.Errorf("tar entry %s does not exist in %s", assertion.TarEntry, assertion.Path)
		}

		if entry.Header.PAXRecords == nil {
			return fmt.Errorf("tar entry %s has no PAX extended attributes", assertion.TarEntry)
		}

		actualValue, exists := entry.Header.PAXRecords[assertion.PaxKey]
		if !exists {
			return fmt.Errorf("tar entry %s does not have PAX attribute %s", assertion.TarEntry, assertion.PaxKey)
		}

		if actualValue != assertion.Content {
			return fmt.Errorf("tar entry %s PAX attribute %s mismatch: expected %q, got %q",
				assertion.TarEntry, assertion.PaxKey, assertion.Content, actualValue)
		}
	default:
		return fmt.Errorf("unknown assertion type: %s", assertion.Type)
	}
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

		if err := tf.CheckAssertions(testCase.Assertions, result); err != nil {
			t.Fatalf("Assertions failed: %v\nStdout: %s\nStderr: %s",
				err, result.Stdout, result.Stderr)
		}
	})
	return nil
}
