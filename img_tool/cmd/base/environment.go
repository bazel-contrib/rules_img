package base

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
)

// environmentProcess implements `img base etc environment`.
//
// /etc/environment is read by PAM (pam_env) and holds one KEY=VALUE assignment
// per line, with no shell expansion and no `export`. Values are conventionally
// double-quoted:
//
//	PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
func environmentProcess(_ context.Context, args []string) {
	env := make(kvFlag)
	var srcs stringsFlag
	var outputPath, producer, templatesPath, imagePath string
	var quote bool
	var mode modeFlag

	flagSet := flag.NewFlagSet("base etc environment", flag.ExitOnError)
	flagSet.Var(env, "env", "Environment variable as KEY=VALUE. Can be repeated.")
	flagSet.Var(&srcs, "src", "Path of an existing environment file to merge. Can be repeated; later files win.")
	flagSet.StringVar(&outputPath, "output", "", "Path of the base metadata stream to write.")
	flagSet.StringVar(&producer, "producer", "", "Label of the rule producing this stream, used in conflict messages.")
	flagSet.StringVar(&templatesPath, "templates", "", "Path of a JSON file with template-expanded values, as written by `img expand-template`.")
	flagSet.StringVar(&imagePath, "path", "/etc/environment", "Path of the file inside the image.")
	flagSet.BoolVar(&quote, "quote", true, "Wrap values in double quotes.")
	flagSet.Var(&mode, "mode", "Octal file mode. Defaults to 0644.")
	if err := flagSet.Parse(args); err != nil {
		fail("etc environment", err)
	}

	// Values from --src files come first so that --env (and anything the
	// templater expanded into it) overrides them.
	merged := make(map[string]string)
	for _, src := range srcs {
		fromFile, err := parseEnvironmentFile(src)
		if err != nil {
			fail("etc environment", err)
		}
		for k, v := range fromFile {
			merged[k] = v
		}
	}

	overrides, err := loadTemplateOverrides(templatesPath)
	if err != nil {
		fail("etc environment", err)
	}
	declared, err := overrides.stringMap("env", env)
	if err != nil {
		fail("etc environment", err)
	}
	for k, v := range declared {
		merged[k] = v
	}

	content := renderEnvironment(merged, quote)
	entry := basemetaFile(imagePath, mode.or(0o644), content)
	if err := writeStream(outputPath, producer, entry); err != nil {
		fail("etc environment", err)
	}
}

// renderEnvironment writes KEY=VALUE lines in sorted key order.
func renderEnvironment(env map[string]string, quote bool) []byte {
	var buf bytes.Buffer
	for _, key := range sortedKeys(env) {
		value := env[key]
		if quote {
			// Values are written verbatim inside the quotes: pam_env does not
			// process escape sequences, so a literal quote in a value cannot be
			// represented and is rejected rather than silently mangled.
			buf.WriteString(fmt.Sprintf("%s=%q\n", key, value))
			continue
		}
		buf.WriteString(key)
		buf.WriteByte('=')
		buf.WriteString(value)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// parseEnvironmentFile reads an existing /etc/environment. Blank lines and
// comments are skipped; values may be single- or double-quoted.
func parseEnvironmentFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening environment file: %w", err)
	}
	defer f.Close()

	env := make(map[string]string)
	scanner := bufio.NewScanner(f)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// pam_env tolerates a leading "export", so accept it too.
		line = strings.TrimPrefix(line, "export ")
		key, value, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE, got %q", path, lineNumber, line)
		}
		env[strings.TrimSpace(key)] = unquote(strings.TrimSpace(value))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return env, nil
}

// unquote strips one layer of matching single or double quotes.
func unquote(value string) string {
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if first == last && (first == '"' || first == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}
