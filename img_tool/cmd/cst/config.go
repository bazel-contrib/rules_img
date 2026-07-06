package cst

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	goyaml "github.com/goccy/go-yaml"
)

// StructureTest is our own definition of the container-structure-test (CST) v2
// config schema (github.com/GoogleContainerTools/container-structure-test,
// pkg/types/v2). Every field CST supports is parsed so users can migrate their
// existing YAML/JSON config files verbatim; categories that cannot be validated
// from only the image config JSON and the image mtree (commandTests,
// fileContentTests, licenseTests) are rejected with a clear error at run time --
// see unsupportedCategories.
//
// Fields carry both `yaml` and `json` struct tags with the CST key names, so the
// same struct parses either format.
type StructureTest struct {
	SchemaVersion      string              `yaml:"schemaVersion" json:"schemaVersion"`
	GlobalEnvVars      []EnvVar            `yaml:"globalEnvVars" json:"globalEnvVars"`
	CommandTests       []CommandTest       `yaml:"commandTests" json:"commandTests"`
	FileExistenceTests []FileExistenceTest `yaml:"fileExistenceTests" json:"fileExistenceTests"`
	FileContentTests   []FileContentTest   `yaml:"fileContentTests" json:"fileContentTests"`
	MetadataTest       MetadataTest        `yaml:"metadataTest" json:"metadataTest"`
	LicenseTests       []LicenseTest       `yaml:"licenseTests" json:"licenseTests"`
}

// EnvVar is a single environment-variable assertion (CST metadataTest.env /
// .envVars and globalEnvVars).
type EnvVar struct {
	Key     string `yaml:"key" json:"key"`
	Value   string `yaml:"value" json:"value"`
	IsRegex bool   `yaml:"isRegex" json:"isRegex"`
}

// Label is a single image-label assertion (CST metadataTest.labels).
type Label struct {
	Key     string `yaml:"key" json:"key"`
	Value   string `yaml:"value" json:"value"`
	IsRegex bool   `yaml:"isRegex" json:"isRegex"`
}

// MetadataTest asserts image config metadata. All of these are validatable from
// the image config JSON alone.
type MetadataTest struct {
	// Env and EnvVars are two names CST has used for the same thing; both are
	// honored and merged.
	Env     []EnvVar `yaml:"env" json:"env"`
	EnvVars []EnvVar `yaml:"envVars" json:"envVars"`
	Labels  []Label  `yaml:"labels" json:"labels"`
	// Entrypoint and Cmd are pointers so an omitted field (nil) means "do not
	// check", while an explicit value (including an empty list) is asserted
	// exactly.
	Entrypoint   *[]string `yaml:"entrypoint" json:"entrypoint"`
	Cmd          *[]string `yaml:"cmd" json:"cmd"`
	ExposedPorts []string  `yaml:"exposedPorts" json:"exposedPorts"`
	Volumes      []string  `yaml:"volumes" json:"volumes"`
	Workdir      string    `yaml:"workdir" json:"workdir"`
	User         string    `yaml:"user" json:"user"`
}

// FileExistenceTest asserts a path's presence and metadata. All fields are
// validatable from the image mtree.
//
// Uid and Gid are pointers (unlike CST's uint32) so they are only checked when
// explicitly set -- a plain uint32 cannot distinguish "unset" from "0" and would
// spuriously fail on non-root files whenever the field is omitted.
type FileExistenceTest struct {
	Name           string `yaml:"name" json:"name"`
	Path           string `yaml:"path" json:"path"`
	ShouldExist    bool   `yaml:"shouldExist" json:"shouldExist"`
	Permissions    string `yaml:"permissions" json:"permissions"`
	Uid            *int64 `yaml:"uid" json:"uid"`
	Gid            *int64 `yaml:"gid" json:"gid"`
	IsExecutableBy string `yaml:"isExecutableBy" json:"isExecutableBy"`
}

// FileContentTest is parsed for migration compatibility but cannot be validated
// from the mtree (which records content digests, not bytes) -- see
// unsupportedCategories.
type FileContentTest struct {
	Name             string   `yaml:"name" json:"name"`
	Path             string   `yaml:"path" json:"path"`
	ExpectedContents []string `yaml:"expectedContents" json:"expectedContents"`
	ExcludedContents []string `yaml:"excludedContents" json:"excludedContents"`
}

// CommandTest is parsed for migration compatibility but requires running the
// container -- see unsupportedCategories.
type CommandTest struct {
	Name           string   `yaml:"name" json:"name"`
	Command        string   `yaml:"command" json:"command"`
	Args           []string `yaml:"args" json:"args"`
	ExpectedOutput []string `yaml:"expectedOutput" json:"expectedOutput"`
	ExcludedOutput []string `yaml:"excludedOutput" json:"excludedOutput"`
	ExpectedError  []string `yaml:"expectedError" json:"expectedError"`
	ExcludedError  []string `yaml:"excludedError" json:"excludedError"`
	ExitCode       int      `yaml:"exitCode" json:"exitCode"`
}

// LicenseTest is parsed for migration compatibility but requires scanning files
// inside a running container -- see unsupportedCategories.
type LicenseTest struct {
	Debian bool     `yaml:"debian" json:"debian"`
	Files  []string `yaml:"files" json:"files"`
}

// parseConfig reads and parses a CST config file. JSON (by extension or leading
// '{') is parsed with encoding/json; everything else is parsed as YAML with
// goccy/go-yaml (which also accepts JSON, since JSON is a subset of YAML).
func parseConfig(path string) (*StructureTest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	var st StructureTest
	if looksLikeJSON(path, data) {
		if err := json.Unmarshal(data, &st); err != nil {
			return nil, fmt.Errorf("parsing JSON config %s: %w", path, err)
		}
	} else {
		if err := goyaml.Unmarshal(data, &st); err != nil {
			return nil, fmt.Errorf("parsing YAML config %s: %w", path, err)
		}
	}
	return &st, nil
}

func looksLikeJSON(path string, data []byte) bool {
	if strings.HasSuffix(strings.ToLower(path), ".json") {
		return true
	}
	trimmed := strings.TrimSpace(string(data))
	return strings.HasPrefix(trimmed, "{")
}

// mergedEnv returns metadataTest.env and metadataTest.envVars combined (CST has
// used both names for the same assertion list).
func (m MetadataTest) mergedEnv() []EnvVar {
	if len(m.Env) == 0 {
		return m.EnvVars
	}
	if len(m.EnvVars) == 0 {
		return m.Env
	}
	out := make([]EnvVar, 0, len(m.Env)+len(m.EnvVars))
	out = append(out, m.Env...)
	out = append(out, m.EnvVars...)
	return out
}
