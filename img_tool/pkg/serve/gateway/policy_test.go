package gateway

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestMatchHost(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		host    string
		want    bool
	}{
		{"docker.acme.corp", "docker.acme.corp", true},
		{"docker.acme.corp", "DOCKER.ACME.CORP", true}, // hosts are case-insensitive
		{"docker.acme.corp", "docker.acme.cor", false},
		{"docker.acme.corp", "evil-docker.acme.corp", false},
		{"docker.acme.corp", "docker.acme.corp.evil", false},
		{"index.docker.io", "index.docker.io", true},
		{"*.docker.io", "index.docker.io", true},
		{"*.docker.io", "a.b.docker.io", true},
		{"*.docker.io", "docker.io", false},      // no leading label
		{"*.docker.io", "evil-docker.io", false}, // must be a full label before the dot
		{"*.docker.io", "xdocker.io", false},     // ditto
		{"*.docker.io", "docker.io.evil", false}, // suffix must be at the end
		{"*", "anything.example.com", true},
		{"*", "localhost", true},
	} {
		hp, err := compileHostPattern(tc.pattern)
		if err != nil {
			t.Fatalf("compileHostPattern(%q): %v", tc.pattern, err)
		}
		if got := hp.match(tc.host); got != tc.want {
			t.Errorf("host %q vs pattern %q = %v, want %v", tc.host, tc.pattern, got, tc.want)
		}
	}
}

func TestCompileHostPatternErrors(t *testing.T) {
	for _, bad := range []string{"*.", "foo*", "*foo", "foo.*", "foo*bar", "*.*"} {
		if _, err := compileHostPattern(bad); err == nil {
			t.Errorf("compileHostPattern(%q) = nil error, want error", bad)
		}
	}
}

func TestMatchRepo(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		repo    string
		want    bool
	}{
		// exact segment
		{"foo", "foo", true},
		{"foo", "foobar", false},
		{"foo", "foo-bar", false},
		{"foo", "foo/bar", false},
		// foo/** matches foo and everything under it
		{"foo/**", "foo", true},
		{"foo/**", "foo/bar", true},
		{"foo/**", "foo/bar/baz", true},
		{"foo/**", "foobar", false},
		{"foo/**", "other", false},
		// foo/* matches exactly one level below foo
		{"foo/*", "foo/bar", true},
		{"foo/*", "foo", false},
		{"foo/*", "foo/bar/baz", false},
		// ** matches everything
		{"**", "foo", true},
		{"**", "a/b/c", true},
		// within-segment wildcards
		{"foo*", "foobar", true},
		{"foo*", "foo", true},
		{"foo*", "foo/bar", false}, // * never crosses '/'
		{"fo?", "foo", true},
		{"fo?", "fo", false},
		{"fo?", "fooo", false},
		// middle **
		{"foo/**/bar", "foo/bar", true},
		{"foo/**/bar", "foo/x/bar", true},
		{"foo/**/bar", "foo/x/y/bar", true},
		{"foo/**/bar", "foo/x/y", false},
		// Docker Hub library namespace
		{"library/**", "library/ubuntu", true},
		{"library/**", "library", true},
		{"library/**", "notlibrary/ubuntu", false},
	} {
		rp := compileRepoPattern(tc.pattern)
		if got := rp.match(tc.repo); got != tc.want {
			t.Errorf("repo %q vs pattern %q = %v, want %v", tc.repo, tc.pattern, got, tc.want)
		}
	}
}

func TestParseOperations(t *testing.T) {
	all := opAll
	for _, tc := range []struct {
		tokens  []string
		want    opSet
		wantErr bool
	}{
		{[]string{"blob:read"}, 1 << opBlobRead, false},
		{[]string{"blob:write"}, 1 << opBlobWrite, false},
		{[]string{"manifest:read"}, 1 << opManifestRead, false},
		{[]string{"manifest:write"}, 1 << opManifestWrite, false},
		{[]string{"blob:read", "manifest:write"}, 1<<opBlobRead | 1<<opManifestWrite, false},
		{[]string{"*"}, all, false},
		{[]string{"blob:read", "*"}, all, false},
		{nil, 0, true},
		{[]string{}, 0, true},
		{[]string{"blobs:read"}, 0, true},
		{[]string{"blob:execute"}, 0, true},
		{[]string{"read"}, 0, true},
	} {
		got, err := parseOperations(tc.tokens)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseOperations(%v) err = %v, wantErr %v", tc.tokens, err, tc.wantErr)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("parseOperations(%v) = %b, want %b", tc.tokens, got, tc.want)
		}
	}
}

func TestCompilePolicyValidation(t *testing.T) {
	valid := ruleConfig{Action: "allow", Registry: "gcr.io", Repository: "**", Operations: []string{"*"}}
	for _, tc := range []struct {
		name    string
		cfg     policyConfig
		wantErr bool
	}{
		{"ok", policyConfig{Version: 1, Rules: []ruleConfig{valid}}, false},
		{"ok empty rules default deny", policyConfig{Version: 1}, false},
		{"version 0", policyConfig{Version: 0, Rules: []ruleConfig{valid}}, true},
		{"version 2", policyConfig{Version: 2, Rules: []ruleConfig{valid}}, true},
		{"bad defaultAction", policyConfig{Version: 1, DefaultAction: "permit"}, true},
		{"defaultAction allow ok", policyConfig{Version: 1, DefaultAction: "allow"}, false},
		{"bad action", policyConfig{Version: 1, Rules: []ruleConfig{{Action: "permit", Registry: "gcr.io", Repository: "**", Operations: []string{"*"}}}}, true},
		{"empty action", policyConfig{Version: 1, Rules: []ruleConfig{{Registry: "gcr.io", Repository: "**", Operations: []string{"*"}}}}, true},
		{"missing registry", policyConfig{Version: 1, Rules: []ruleConfig{{Action: "allow", Repository: "**", Operations: []string{"*"}}}}, true},
		{"missing repository", policyConfig{Version: 1, Rules: []ruleConfig{{Action: "allow", Registry: "gcr.io", Operations: []string{"*"}}}}, true},
		{"empty operations", policyConfig{Version: 1, Rules: []ruleConfig{{Action: "allow", Registry: "gcr.io", Repository: "**"}}}, true},
		{"bad host pattern", policyConfig{Version: 1, Rules: []ruleConfig{{Action: "allow", Registry: "foo*bar", Repository: "**", Operations: []string{"*"}}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compilePolicy(tc.cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("compilePolicy err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestCompilePolicyRegistryUnion(t *testing.T) {
	cp, err := compilePolicy(policyConfig{Version: 1, Rules: []ruleConfig{
		{Action: "allow", Registry: "docker.acme.corp", Repository: "foo/**", Operations: []string{"*"}},
		{Action: "deny", Registry: "docker.acme.corp", Repository: "bar", Operations: []string{"*"}},
		{Action: "allow", Registry: "index.docker.io", Repository: "library/**", Operations: []string{"blob:read"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cp.registries) != 2 {
		t.Fatalf("registry union = %d entries, want 2 (deduped)", len(cp.registries))
	}
	if !cp.RegistryAllowed("docker.acme.corp") || !cp.RegistryAllowed("index.docker.io") {
		t.Error("both rule registries should be allowed for the version check")
	}
	if cp.RegistryAllowed("gcr.io") {
		t.Error("a registry no rule mentions must not be allowed")
	}
}

func TestRegistryAllowedDefaultAllow(t *testing.T) {
	cp, err := compilePolicy(policyConfig{Version: 1, DefaultAction: "allow"})
	if err != nil {
		t.Fatal(err)
	}
	if !cp.RegistryAllowed("anything.example.com") {
		t.Error("defaultAction=allow should permit the version check for any registry")
	}
}

// examplePolicy is the documented example: bar denied, foo/** fully writable,
// everything else on docker.acme.corp read-only, Docker Hub library read-only.
func examplePolicy(t *testing.T) *CompiledPolicy {
	t.Helper()
	cp, err := compilePolicy(policyConfig{Version: 1, DefaultAction: "deny", Rules: []ruleConfig{
		{Action: "deny", Registry: "docker.acme.corp", Repository: "bar", Operations: []string{"*"}},
		{Action: "allow", Registry: "docker.acme.corp", Repository: "foo/**", Operations: []string{"blob:read", "blob:write", "manifest:read", "manifest:write"}},
		{Action: "allow", Registry: "docker.acme.corp", Repository: "**", Operations: []string{"blob:read", "manifest:read"}},
		{Action: "allow", Registry: "index.docker.io", Repository: "library/**", Operations: []string{"blob:read", "manifest:read"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return cp
}

func TestDecide(t *testing.T) {
	cp := examplePolicy(t)
	for _, tc := range []struct {
		name string
		host string
		repo string
		req  requirement
		want bool
	}{
		{"foo manifest write allowed", "docker.acme.corp", "foo/app", reqManifestWrite, true},
		{"foo blob write allowed", "docker.acme.corp", "foo/app", reqBlobWrite, true},
		{"bar blob read denied", "docker.acme.corp", "bar", reqBlobRead, false},
		{"bar manifest read denied", "docker.acme.corp", "bar", reqManifestRead, false},
		{"other manifest read allowed", "docker.acme.corp", "other", reqManifestRead, true},
		{"other manifest write denied", "docker.acme.corp", "other", reqManifestWrite, false},
		{"other blob write denied", "docker.acme.corp", "other", reqBlobWrite, false},
		{"hub library read allowed", "index.docker.io", "library/ubuntu", reqBlobRead, true},
		{"hub library write denied", "index.docker.io", "library/ubuntu", reqManifestWrite, false},
		{"unknown registry denied", "gcr.io", "whatever", reqBlobRead, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, _ := cp.Decide(tc.host, tc.repo, tc.req)
			if got != tc.want {
				t.Errorf("Decide(%q,%q,%v) = %v, want %v", tc.host, tc.repo, tc.req, got, tc.want)
			}
		})
	}
}

func TestDecideHeadReadOrWrite(t *testing.T) {
	cp := examplePolicy(t)
	// "other" is read-only: a HEAD (read-or-write) is allowed via the read side.
	if ok, _, _ := cp.Decide("docker.acme.corp", "other", reqBlobReadOrWrite); !ok {
		t.Error("HEAD on a readable blob should be allowed")
	}
	// "bar" is fully denied: a HEAD must be denied because neither read nor
	// write is permitted.
	if ok, _, _ := cp.Decide("docker.acme.corp", "bar", reqBlobReadOrWrite); ok {
		t.Error("HEAD on a fully-denied repo must be denied")
	}

	// A write-only rule: HEAD is allowed (push flow) but a plain GET is not.
	writeOnly, err := compilePolicy(policyConfig{Version: 1, Rules: []ruleConfig{
		{Action: "allow", Registry: "gcr.io", Repository: "**", Operations: []string{"blob:write", "manifest:write"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := writeOnly.Decide("gcr.io", "app", reqBlobReadOrWrite); !ok {
		t.Error("write-only policy should allow a blob HEAD (skip-reupload check)")
	}
	if ok, _, _ := writeOnly.Decide("gcr.io", "app", reqBlobRead); ok {
		t.Error("write-only policy should deny a plain blob GET")
	}
}

func TestDecideFirstMatchWins(t *testing.T) {
	// A broad allow placed before a narrow deny shadows the deny (documented
	// footgun): first match wins.
	shadowed, err := compilePolicy(policyConfig{Version: 1, Rules: []ruleConfig{
		{Action: "allow", Registry: "docker.acme.corp", Repository: "**", Operations: []string{"*"}},
		{Action: "deny", Registry: "docker.acme.corp", Repository: "bar", Operations: []string{"*"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := shadowed.Decide("docker.acme.corp", "bar", reqBlobRead); !ok {
		t.Error("broad allow before a deny should shadow it (first-match-wins)")
	}

	// Reversing the order makes the deny effective.
	ordered, err := compilePolicy(policyConfig{Version: 1, Rules: []ruleConfig{
		{Action: "deny", Registry: "docker.acme.corp", Repository: "bar", Operations: []string{"*"}},
		{Action: "allow", Registry: "docker.acme.corp", Repository: "**", Operations: []string{"*"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if ok, idx, _ := ordered.Decide("docker.acme.corp", "bar", reqBlobRead); ok {
		t.Errorf("deny before allow should block bar; got allow from rule %d", idx)
	}
}

func TestDecideDefaultAction(t *testing.T) {
	deny, err := compilePolicy(policyConfig{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := deny.Decide("gcr.io", "app", reqBlobRead); ok {
		t.Error("empty policy with default deny must deny everything")
	}

	allow, err := compilePolicy(policyConfig{Version: 1, DefaultAction: "allow"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := allow.Decide("gcr.io", "app", reqManifestWrite); !ok {
		t.Error("default allow with no rules must allow everything")
	}
}

func TestLoadPolicyFileJSONAndYAMLParity(t *testing.T) {
	jsonBody := `{
  "version": 1,
  "defaultAction": "deny",
  "rules": [
    {"action": "allow", "registry": "docker.acme.corp", "repository": "foo/**", "operations": ["blob:read", "blob:write", "manifest:read", "manifest:write"]},
    {"action": "allow", "registry": "docker.acme.corp", "repository": "**", "operations": ["blob:read", "manifest:read"]}
  ]
}`
	yamlBody := `version: 1
defaultAction: deny
rules:
  - action: allow
    registry: docker.acme.corp
    repository: "foo/**"
    operations: [blob:read, blob:write, manifest:read, manifest:write]
  - action: allow
    registry: docker.acme.corp
    repository: "**"
    operations: [blob:read, manifest:read]
`
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "policy.json")
	yamlPath := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(jsonPath, []byte(jsonBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(yamlPath, []byte(yamlBody), 0o600); err != nil {
		t.Fatal(err)
	}
	fromJSON, err := LoadPolicyFile(jsonPath)
	if err != nil {
		t.Fatalf("LoadPolicyFile(json): %v", err)
	}
	fromYAML, err := LoadPolicyFile(yamlPath)
	if err != nil {
		t.Fatalf("LoadPolicyFile(yaml): %v", err)
	}
	// The two forms should decide identically.
	for _, probe := range []struct {
		host, repo string
		req        requirement
	}{
		{"docker.acme.corp", "foo/app", reqManifestWrite},
		{"docker.acme.corp", "other", reqManifestWrite},
		{"docker.acme.corp", "other", reqBlobRead},
	} {
		j, _, _ := fromJSON.Decide(probe.host, probe.repo, probe.req)
		y, _, _ := fromYAML.Decide(probe.host, probe.repo, probe.req)
		if j != y {
			t.Errorf("JSON/YAML disagree on %+v: json=%v yaml=%v", probe, j, y)
		}
	}
}

func TestLoadPolicyFileRejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(jsonPath, []byte(`{"version":1,"ruls":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicyFile(jsonPath); err == nil {
		t.Error("a typo'd top-level field must be rejected, not silently ignored")
	}
	yamlPath := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(yamlPath, []byte("version: 1\nruls: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicyFile(yamlPath); err == nil {
		t.Error("a typo'd YAML field must be rejected under strict decoding")
	}
}

func TestLoadPolicyFileErrors(t *testing.T) {
	dir := t.TempDir()
	// Missing file.
	if _, err := LoadPolicyFile(filepath.Join(dir, "nope.json")); err == nil {
		t.Error("missing file should error")
	}
	// Invalid JSON.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicyFile(bad); err == nil {
		t.Error("invalid JSON should error")
	}
	// Valid syntax, invalid policy (empty operations).
	invalid := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalid, []byte(`{"version":1,"rules":[{"action":"allow","registry":"gcr.io","repository":"**","operations":[]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicyFile(invalid); err == nil {
		t.Error("empty operations should error")
	}
}

func TestLoadPolicyFileRejectsTrailingContent(t *testing.T) {
	dir := t.TempDir()
	obj := `{"version":1,"rules":[{"action":"allow","registry":"gcr.io","repository":"**","operations":["*"]}]}`

	// A second JSON document must not be silently dropped.
	twoJSON := filepath.Join(dir, "two.json")
	if err := os.WriteFile(twoJSON, []byte(obj+"\n"+obj), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicyFile(twoJSON); err == nil {
		t.Error("two concatenated JSON documents must be rejected")
	}

	// Trailing garbage after a valid object must not be ignored.
	garbage := filepath.Join(dir, "garbage.json")
	if err := os.WriteFile(garbage, []byte(obj+"  not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicyFile(garbage); err == nil {
		t.Error("trailing content after the JSON object must be rejected")
	}

	// A second YAML document (after '---') must not be silently dropped.
	twoYAML := filepath.Join(dir, "two.yaml")
	body := "version: 1\nrules: []\n---\nversion: 1\nrules: []\n"
	if err := os.WriteFile(twoYAML, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicyFile(twoYAML); err == nil {
		t.Error("a multi-document YAML file must be rejected")
	}
}

// newTestHandler is defined in gateway_test.go.

func TestServeHTTPPolicyFile(t *testing.T) {
	cp := examplePolicy(t)
	h := newTestHandler(cp, &fakeUpstreamRT{})

	for _, tc := range []struct {
		name   string
		method string
		host   string
		path   string
		want   int
	}{
		{"foo manifest push allowed", http.MethodPut, "docker.acme.corp", "/v2/foo/app/manifests/latest", http.StatusOK},
		{"foo blob upload allowed", http.MethodPost, "docker.acme.corp", "/v2/foo/app/blobs/uploads/", http.StatusAccepted},
		{"bar read denied", http.MethodGet, "docker.acme.corp", "/v2/bar/manifests/latest", http.StatusForbidden},
		{"other read allowed", http.MethodGet, "docker.acme.corp", "/v2/other/manifests/latest", http.StatusOK},
		{"other manifest write denied", http.MethodPut, "docker.acme.corp", "/v2/other/manifests/latest", http.StatusForbidden},
		{"hub library read allowed via bare name", http.MethodGet, "docker.io", "/v2/ubuntu/manifests/latest", http.StatusOK},
		{"gcr not in policy denied", http.MethodGet, "gcr.io", "/v2/app/manifests/latest", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(h, tc.method, tc.host, tc.path)
			if resp.StatusCode != tc.want {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d (body %s)", resp.StatusCode, tc.want, body)
			}
		})
	}
}

func TestServeHTTPCrossRepoMount(t *testing.T) {
	cp, err := compilePolicy(policyConfig{Version: 1, DefaultAction: "deny", Rules: []ruleConfig{
		{Action: "allow", Registry: "docker.acme.corp", Repository: "dest/**", Operations: []string{"blob:read", "blob:write", "manifest:read", "manifest:write"}},
		{Action: "allow", Registry: "docker.acme.corp", Repository: "readable/**", Operations: []string{"blob:read"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(cp, &fakeUpstreamRT{})

	for _, tc := range []struct {
		name string
		path string
		want int
	}{
		{"mount from readable source allowed", "/v2/dest/app/blobs/uploads/?mount=sha256:abc&from=readable/base", http.StatusAccepted},
		{"mount from unreadable source denied", "/v2/dest/app/blobs/uploads/?mount=sha256:abc&from=secret/base", http.StatusForbidden},
		{"no mount is a plain upload", "/v2/dest/app/blobs/uploads/", http.StatusAccepted},
		{"mount without from is a plain upload", "/v2/dest/app/blobs/uploads/?mount=sha256:abc", http.StatusAccepted},
		{"malformed from denied", "/v2/dest/app/blobs/uploads/?mount=sha256:abc&from=BAD", http.StatusForbidden},
		// Ambiguous queries the upstream might parse differently are refused.
		{"semicolon in from rejected", "/v2/dest/app/blobs/uploads/?mount=sha256:abc&from=readable/base;evil", http.StatusBadRequest},
		{"duplicate from rejected", "/v2/dest/app/blobs/uploads/?mount=sha256:abc&from=readable/base&from=secret/base", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(h, http.MethodPost, "docker.acme.corp", tc.path)
			if resp.StatusCode != tc.want {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d (body %s)", resp.StatusCode, tc.want, body)
			}
		})
	}
}

func TestHandlerReloadKeepsOldPolicyOnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(`{"version":1,"rules":[{"action":"allow","registry":"docker.acme.corp","repository":"foo/**","operations":["manifest:read"]}]}`)
	cp, err := LoadPolicyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(cp, &fakeUpstreamRT{})

	// Initially foo is readable, bar is not.
	if resp := do(h, http.MethodGet, "docker.acme.corp", "/v2/foo/app/manifests/latest"); resp.StatusCode != http.StatusOK {
		t.Fatalf("foo read status = %d, want 200", resp.StatusCode)
	}
	if resp := do(h, http.MethodGet, "docker.acme.corp", "/v2/bar/manifests/latest"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bar read status = %d, want 403", resp.StatusCode)
	}

	// Reload with a policy that also allows bar.
	write(`{"version":1,"rules":[{"action":"allow","registry":"docker.acme.corp","repository":"**","operations":["manifest:read"]}]}`)
	if _, err := h.Reload(path); err != nil {
		t.Fatalf("Reload(good): %v", err)
	}
	if resp := do(h, http.MethodGet, "docker.acme.corp", "/v2/bar/manifests/latest"); resp.StatusCode != http.StatusOK {
		t.Fatalf("after reload bar read status = %d, want 200", resp.StatusCode)
	}

	// Reload with a broken file: the error is returned and the previous policy
	// is kept (bar stays readable, the gateway does not fall open or shut).
	write(`{ this is not valid json`)
	if _, err := h.Reload(path); err == nil {
		t.Fatal("Reload(bad) should return an error")
	}
	if resp := do(h, http.MethodGet, "docker.acme.corp", "/v2/bar/manifests/latest"); resp.StatusCode != http.StatusOK {
		t.Fatalf("after failed reload bar read status = %d, want 200 (old policy kept)", resp.StatusCode)
	}
}
