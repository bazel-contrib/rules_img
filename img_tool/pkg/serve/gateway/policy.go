package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	goyaml "github.com/goccy/go-yaml"
)

// This file implements the file-based authorization policy for the gateway. A
// policy is an ordered list of allow/deny rules matched against the *resolved*
// upstream registry host, the *resolved* repository path, and the classified
// operation (blob/manifest read/write). The first matching rule decides;
// requests that match no rule fall back to a fail-closed default action.
//
// The config is plain data (JSON, or the same schema authored as YAML) so it is
// trivially diffable and reviewable and cannot smuggle logic. Pattern matching
// is a small hand-rolled glob (no regexp is compiled from user input, so there
// is no ReDoS surface, and no glob dependency to audit).

// policyConfig is the on-disk policy schema. The same struct parses JSON or YAML
// (JSON is a subset of YAML); both key spellings carry struct tags.
type policyConfig struct {
	// Version is the schema version. Only 1 is understood; any other value is a
	// hard load error (fail closed rather than guess).
	Version int `json:"version" yaml:"version"`
	// DefaultAction is applied to requests that match no rule. "allow" or "deny";
	// empty defaults to "deny".
	DefaultAction string `json:"defaultAction" yaml:"defaultAction"`
	// Rules are evaluated in order; the first match wins.
	Rules []ruleConfig `json:"rules" yaml:"rules"`
}

// ruleConfig is a single allow/deny rule.
type ruleConfig struct {
	// Description is free text surfaced in decision logs. Optional.
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	// Action is "allow" or "deny". Required.
	Action string `json:"action" yaml:"action"`
	// Registry is a host pattern matched against the resolved upstream host:
	// an exact host ("docker.acme.corp", "index.docker.io"), a single leading
	// "*." wildcard matching one or more leading labels ("*.docker.io" matches
	// index.docker.io and a.b.docker.io but not bare docker.io), or "*" to match
	// any host. Required.
	Registry string `json:"registry" yaml:"registry"`
	// Repository is a glob matched against the resolved repository path:
	// "*" matches within one path segment, "**" matches across segments
	// (including zero, with the joining "/" optional so "foo/**" also matches
	// exactly "foo"), and "?" matches a single non-"/" character. Use "**" to
	// match every repository. Required.
	Repository string `json:"repository" yaml:"repository"`
	// Operations lists the operations this rule speaks to. Each token is one of
	// "blob:read", "blob:write", "manifest:read", "manifest:write", or the
	// single sugar token "*" meaning all four. Required and non-empty: an
	// omitted or empty list is a load error, never a silent blanket grant.
	Operations []string `json:"operations" yaml:"operations"`
}

// operation enumerates the four gated operations.
type operation uint8

const (
	opBlobRead operation = iota
	opBlobWrite
	opManifestRead
	opManifestWrite
)

// opSet is a bitset over the four operations.
type opSet uint8

const opAll opSet = 1<<opBlobRead | 1<<opBlobWrite | 1<<opManifestRead | 1<<opManifestWrite

func (s opSet) has(op operation) bool { return s&(1<<op) != 0 }

// hostPattern matches a resolved registry host (port already stripped).
type hostPattern struct {
	// exactly one of the following is set.
	any    bool   // "*": match any host.
	suffix string // ".docker.io" for "*.docker.io": match one-or-more leading labels.
	exact  string // literal host, lower-cased.
}

func (p hostPattern) match(host string) bool {
	host = strings.ToLower(host)
	switch {
	case p.any:
		return true
	case p.suffix != "":
		// Require at least one non-empty label before the suffix, so
		// "*.docker.io" matches "index.docker.io" but not bare "docker.io".
		return len(host) > len(p.suffix) && strings.HasSuffix(host, p.suffix)
	default:
		return host == p.exact
	}
}

// compileHostPattern validates and compiles a registry host pattern.
func compileHostPattern(pattern string) (hostPattern, error) {
	switch {
	case pattern == "*":
		return hostPattern{any: true}, nil
	case strings.HasPrefix(pattern, "*."):
		rest := pattern[len("*."):]
		if rest == "" || strings.Contains(rest, "*") {
			return hostPattern{}, fmt.Errorf("invalid registry pattern %q (want an exact host, \"*.suffix\", or \"*\")", pattern)
		}
		return hostPattern{suffix: "." + strings.ToLower(rest)}, nil
	case strings.Contains(pattern, "*"):
		return hostPattern{}, fmt.Errorf("invalid registry pattern %q (\"*\" is only allowed alone or as a leading \"*.\" label)", pattern)
	default:
		return hostPattern{exact: strings.ToLower(pattern)}, nil
	}
}

// repoPattern is a "/"-segmented glob matched against a repository path.
type repoPattern struct {
	segments []string
}

func compileRepoPattern(pattern string) repoPattern {
	return repoPattern{segments: strings.Split(pattern, "/")}
}

func (p repoPattern) match(repo string) bool {
	return matchSegments(p.segments, strings.Split(repo, "/"))
}

// matchSegments reports whether the "/"-split pattern matches the "/"-split
// name. A pattern segment of exactly "**" matches zero or more name segments;
// any other pattern segment matches exactly one name segment via
// matchOneSegment. This is the classic greedy wildcard match with a single
// backtrack point, lifted to path-segment granularity.
func matchSegments(pat, name []string) bool {
	pi, ni := 0, 0
	// Backtrack point for the most recent "**": (pattern index, name index).
	starPat, starName := -1, -1
	for ni < len(name) {
		if pi < len(pat) {
			if pat[pi] == "**" {
				// Tentatively let "**" match zero segments; remember where to
				// resume if a later segment forces it to consume more.
				starPat, starName = pi, ni
				pi++
				continue
			}
			if matchOneSegment(pat[pi], name[ni]) {
				pi++
				ni++
				continue
			}
		}
		if starPat != -1 {
			// Backtrack: let the last "**" swallow one more name segment.
			pi = starPat + 1
			starName++
			ni = starName
			continue
		}
		return false
	}
	// Any pattern tail must be "**" segments (each matching zero remaining).
	for pi < len(pat) && pat[pi] == "**" {
		pi++
	}
	return pi == len(pat)
}

// matchOneSegment matches a single path segment (no "/") against a pattern
// segment that may contain "*" (any run of characters within the segment) and
// "?" (exactly one character). It is the standard greedy wildcard match.
func matchOneSegment(pat, s string) bool {
	pr, sr := []rune(pat), []rune(s)
	pi, si := 0, 0
	star, ss := -1, -1
	for si < len(sr) {
		switch {
		case pi < len(pr) && (pr[pi] == '?' || pr[pi] == sr[si]):
			pi++
			si++
		case pi < len(pr) && pr[pi] == '*':
			star, ss = pi, si
			pi++
		case star != -1:
			pi = star + 1
			ss++
			si = ss
		default:
			return false
		}
	}
	for pi < len(pr) && pr[pi] == '*' {
		pi++
	}
	return pi == len(pr)
}

// parseOperations turns the config tokens into an opSet, rejecting an empty list
// or an unknown token.
func parseOperations(tokens []string) (opSet, error) {
	if len(tokens) == 0 {
		return 0, fmt.Errorf("operations must be a non-empty list")
	}
	var set opSet
	for _, t := range tokens {
		switch t {
		case "*":
			set |= opAll
		case "blob:read":
			set |= 1 << opBlobRead
		case "blob:write":
			set |= 1 << opBlobWrite
		case "manifest:read":
			set |= 1 << opManifestRead
		case "manifest:write":
			set |= 1 << opManifestWrite
		default:
			return 0, fmt.Errorf("unknown operation %q (want blob:read, blob:write, manifest:read, manifest:write, or *)", t)
		}
	}
	return set, nil
}

// compiledRule is a validated, ready-to-evaluate rule.
type compiledRule struct {
	host  hostPattern
	repo  repoPattern
	ops   opSet
	allow bool
	idx   int    // position in the file, for logging.
	desc  string // description, for logging.
}

// CompiledPolicy is an immutable, validated authorization policy. It is safe to
// share across goroutines and is swapped atomically on reload.
type CompiledPolicy struct {
	defaultAllow bool
	// registries is the deduplicated union of every rule's host pattern, used to
	// gate the anonymous /v2/ version check (which carries no repository).
	registries []hostPattern
	rules      []compiledRule
}

// compilePolicy validates a parsed policyConfig and compiles it. Any problem is
// a hard error so callers can fail closed (refuse to start / keep the previous
// policy on reload).
func compilePolicy(cfg policyConfig) (*CompiledPolicy, error) {
	if cfg.Version != 1 {
		return nil, fmt.Errorf("unsupported version %d (want 1)", cfg.Version)
	}
	var defaultAllow bool
	switch cfg.DefaultAction {
	case "", "deny":
		defaultAllow = false
	case "allow":
		defaultAllow = true
	default:
		return nil, fmt.Errorf("invalid defaultAction %q (want \"allow\" or \"deny\")", cfg.DefaultAction)
	}

	cp := &CompiledPolicy{defaultAllow: defaultAllow}
	seenRegistry := make(map[string]bool)
	for i, rc := range cfg.Rules {
		var allow bool
		switch rc.Action {
		case "allow":
			allow = true
		case "deny":
			allow = false
		default:
			return nil, fmt.Errorf("rule %d: invalid action %q (want \"allow\" or \"deny\")", i, rc.Action)
		}
		if rc.Registry == "" {
			return nil, fmt.Errorf("rule %d: registry is required", i)
		}
		host, err := compileHostPattern(rc.Registry)
		if err != nil {
			return nil, fmt.Errorf("rule %d: %w", i, err)
		}
		if rc.Repository == "" {
			return nil, fmt.Errorf("rule %d: repository is required (use \"**\" to match all)", i)
		}
		ops, err := parseOperations(rc.Operations)
		if err != nil {
			return nil, fmt.Errorf("rule %d: %w", i, err)
		}
		cp.rules = append(cp.rules, compiledRule{
			host:  host,
			repo:  compileRepoPattern(rc.Repository),
			ops:   ops,
			allow: allow,
			idx:   i,
			desc:  rc.Description,
		})
		if !seenRegistry[rc.Registry] {
			seenRegistry[rc.Registry] = true
			cp.registries = append(cp.registries, host)
		}
	}
	return cp, nil
}

// RegistryAllowed reports whether the gateway will speak to the given resolved
// host at all (port already stripped). It gates the anonymous /v2/ version
// check. A host is reachable if the default action is allow, or some rule names
// it: a request to a host no rule mentions can never be permitted, so the
// version handshake is refused too.
func (p *CompiledPolicy) RegistryAllowed(host string) bool {
	if p.defaultAllow {
		return true
	}
	for _, hp := range p.registries {
		if hp.match(host) {
			return true
		}
	}
	return false
}

// Decide reports whether the operation classified by req is permitted on
// (host, repo), returning the winning rule's index (-1 for the default) and its
// description for logging. host must already have its port stripped and repo
// must be the resolved repository path.
func (p *CompiledPolicy) Decide(host, repo string, req requirement) (allow bool, ruleIndex int, desc string) {
	switch req {
	case reqBlobRead:
		return p.decideOp(host, repo, opBlobRead)
	case reqBlobWrite:
		return p.decideOp(host, repo, opBlobWrite)
	case reqManifestRead:
		return p.decideOp(host, repo, opManifestRead)
	case reqManifestWrite:
		return p.decideOp(host, repo, opManifestWrite)
	case reqBlobReadOrWrite:
		// HEAD on a blob is part of both the pull and push flows; allow it if
		// either the read or the write of that kind is permitted.
		if a, i, d := p.decideOp(host, repo, opBlobRead); a {
			return a, i, d
		}
		return p.decideOp(host, repo, opBlobWrite)
	case reqManifestReadOrWrite:
		if a, i, d := p.decideOp(host, repo, opManifestRead); a {
			return a, i, d
		}
		return p.decideOp(host, repo, opManifestWrite)
	default:
		return false, -1, "unknown requirement"
	}
}

// decideOp walks the rules top-to-bottom for a single operation. A rule matches
// when it speaks to the operation and its host and repository patterns match;
// the first match's action decides. With no match, the default action applies.
func (p *CompiledPolicy) decideOp(host, repo string, op operation) (bool, int, string) {
	for _, r := range p.rules {
		if r.ops.has(op) && r.host.match(host) && r.repo.match(repo) {
			return r.allow, r.idx, r.desc
		}
	}
	return p.defaultAllow, -1, "default action"
}

// AllowAll returns a policy that permits every request. It backs the
// --dangerously-allow-all escape hatch and should never be used in production.
func AllowAll() *CompiledPolicy {
	return &CompiledPolicy{defaultAllow: true}
}

// LoadPolicyFile reads, parses, and compiles a policy file. JSON (by extension
// or a leading "{") is parsed with encoding/json; everything else is parsed as
// YAML with goccy/go-yaml (which also accepts JSON). Unknown fields are rejected
// so a typo in a security config is a loud error rather than a silent grant, and
// content after the first document is rejected so trailing rules cannot be
// silently dropped.
func LoadPolicyFile(path string) (*CompiledPolicy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading policy %s: %w", path, err)
	}
	var cfg policyConfig
	if looksLikeJSON(path, data) {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("parsing JSON policy %s: %w", path, err)
		}
		// Reject trailing content: Decode stops at the first value and would
		// otherwise silently ignore a second concatenated document.
		if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("parsing JSON policy %s: unexpected content after the policy object", path)
		}
	} else {
		dec := goyaml.NewDecoder(bytes.NewReader(data), goyaml.Strict())
		if err := dec.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("parsing YAML policy %s: %w", path, err)
		}
		// Reject a second YAML document (after a "---" separator) for the same
		// reason: its rules would otherwise be silently dropped.
		if err := dec.Decode(new(struct{})); !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("parsing YAML policy %s: expected a single document", path)
		}
	}
	cp, err := compilePolicy(cfg)
	if err != nil {
		return nil, fmt.Errorf("invalid policy %s: %w", path, err)
	}
	return cp, nil
}

func looksLikeJSON(path string, data []byte) bool {
	if strings.HasSuffix(strings.ToLower(path), ".json") {
		return true
	}
	return strings.HasPrefix(strings.TrimSpace(string(data)), "{")
}

// Summary returns a short human-readable description of the policy for logging.
func (p *CompiledPolicy) Summary() string {
	action := "deny"
	if p.defaultAllow {
		action = "allow"
	}
	return fmt.Sprintf("%d rules, defaultAction=%s", len(p.rules), action)
}
