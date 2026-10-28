package aws

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// sharedFiles holds the parsed contents of the AWS shared config and
// credentials files.
type sharedFiles struct {
	profiles    map[string]map[string]string
	ssoSessions map[string]map[string]string
}

func awsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".aws")
}

func loadSharedFiles(env func(string) string) (*sharedFiles, error) {
	configPath := env("AWS_CONFIG_FILE")
	if configPath == "" {
		configPath = filepath.Join(awsDir(), "config")
	}
	credsPath := env("AWS_SHARED_CREDENTIALS_FILE")
	if credsPath == "" {
		credsPath = filepath.Join(awsDir(), "credentials")
	}
	f := &sharedFiles{profiles: map[string]map[string]string{}, ssoSessions: map[string]map[string]string{}}
	// The credentials file takes precedence over the config file.
	for _, src := range []struct {
		path     string
		isConfig bool
	}{{configPath, true}, {credsPath, false}} {
		if err := f.parseFile(src.path, src.isConfig); err != nil {
			return nil, err
		}
	}
	return f, nil
}

func (f *sharedFiles) parseFile(path string, isConfig bool) error {
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) || path == "" {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()

	var section map[string]string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		raw := scanner.Text()
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		if line[0] == '[' {
			section = nil
			end := strings.IndexByte(line, ']')
			if end < 0 {
				continue
			}
			section = f.section(strings.Join(strings.Fields(line[1:end]), " "), isConfig)
			continue
		}
		// Indented lines are sub-properties of the previous key (for example
		// `s3 =` blocks); none of them matter here.
		if section == nil || raw[0] == ' ' || raw[0] == '\t' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		section[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return scanner.Err()
}

// section returns the map to fill for a section header, or nil to ignore it.
func (f *sharedFiles) section(header string, isConfig bool) map[string]string {
	var target map[string]map[string]string
	name := header
	switch {
	case !isConfig:
		target = f.profiles
	case header == "default":
		target = f.profiles
	case strings.HasPrefix(header, "profile "):
		target, name = f.profiles, strings.TrimPrefix(header, "profile ")
	case strings.HasPrefix(header, "sso-session "):
		target, name = f.ssoSessions, strings.TrimPrefix(header, "sso-session ")
	default:
		return nil
	}
	if target[name] == nil {
		target[name] = map[string]string{}
	}
	return target[name]
}

// stripComment removes a trailing comment introduced by # or ; at the start of
// the line or after whitespace.
func stripComment(s string) string {
	for i := 0; i < len(s); i++ {
		if (s[i] == '#' || s[i] == ';') && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t') {
			return s[:i]
		}
	}
	return s
}
