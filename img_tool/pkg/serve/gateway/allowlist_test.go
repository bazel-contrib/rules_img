package gateway

import "testing"

func TestCompileAllowlist(t *testing.T) {
	allowed, err := CompileAllowlist([]string{"gcr.io", "ghcr.io"}, []string{`.*\.docker\.io`})
	if err != nil {
		t.Fatalf("CompileAllowlist: %v", err)
	}

	match := func(host string) bool {
		for _, re := range allowed {
			if re.MatchString(host) {
				return true
			}
		}
		return false
	}

	// Exact hostnames match literally.
	if !match("gcr.io") {
		t.Error("gcr.io should match its exact allow entry")
	}
	if !match("ghcr.io") {
		t.Error("ghcr.io should match its exact allow entry")
	}
	// The dot in an exact hostname is literal, not a regex wildcard.
	if match("gcrXio") {
		t.Error("exact 'gcr.io' must not match 'gcrXio' (dot is literal)")
	}
	// Exact entries are anchored: no substring/suffix matches.
	if match("evil-gcr.io.attacker.example") {
		t.Error("exact 'gcr.io' must not match a superstring host")
	}
	if match("gcr.io.evil.example") {
		t.Error("exact 'gcr.io' must not match a subdomain-suffixed host")
	}
	// Regex entry matches per its pattern, still anchored.
	if !match("index.docker.io") {
		t.Error("index.docker.io should match the docker.io regex")
	}
	if match("index.docker.io.evil.example") {
		t.Error("regex entry must be anchored (no trailing garbage)")
	}
}

func TestCompileAllowlistBadRegex(t *testing.T) {
	if _, err := CompileAllowlist(nil, []string{"["}); err == nil {
		t.Fatal("expected error for invalid regex")
	}
}
