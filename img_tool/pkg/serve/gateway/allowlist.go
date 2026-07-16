package gateway

import (
	"fmt"
	"regexp"
)

// CompileAllowlist turns exact-hostname and regex allow entries into a single
// list of anchored regular expressions suitable for [WithAllowedRegistries].
//
// Exact hostnames are quoted so metacharacters (notably '.') are literal; both
// kinds are anchored (^(?:...)$) for a full-string match so an entry like
// "gcr.io" never matches "evil-gcr.io.attacker.example".
func CompileAllowlist(exactHosts, regexes []string) ([]*regexp.Regexp, error) {
	allowed := make([]*regexp.Regexp, 0, len(exactHosts)+len(regexes))
	for _, host := range exactHosts {
		allowed = append(allowed, regexp.MustCompile("^(?:"+regexp.QuoteMeta(host)+")$"))
	}
	for _, pattern := range regexes {
		re, err := regexp.Compile("^(?:" + pattern + ")$")
		if err != nil {
			return nil, fmt.Errorf("invalid registry regex %q: %w", pattern, err)
		}
		allowed = append(allowed, re)
	}
	return allowed, nil
}
