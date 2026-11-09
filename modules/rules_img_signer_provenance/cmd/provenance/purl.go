package main

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// refToPURL converts an image reference (and optional platform) to a Package
// URL of type "docker", producing the same strings as BuildKit's
// util/purl.RefToPURL, for example:
//
//	alpine:3.17                -> pkg:docker/alpine@3.17
//	alpine@sha256:...          -> pkg:docker/alpine?digest=sha256:...
//	ghcr.io/foo/bar            -> pkg:docker/ghcr.io/foo/bar@latest
//	busybox + linux/amd64      -> pkg:docker/busybox@latest?platform=linux%2Famd64
func refToPURL(ref string, platform *v1.Platform) (string, error) {
	base, digest, _ := strings.Cut(ref, "@")
	parsed, err := name.ParseReference(base, name.WeakValidation)
	if err != nil {
		return "", fmt.Errorf("parsing image reference %q: %w", ref, err)
	}
	if digest != "" {
		if _, err := v1.NewHash(digest); err != nil {
			return "", fmt.Errorf("parsing digest of image reference %q: %w", ref, err)
		}
	}
	repo := parsed.Context()
	familiar := repo.RepositoryStr()
	if repo.RegistryStr() == name.DefaultRegistry {
		familiar = strings.TrimPrefix(familiar, "library/")
	} else {
		familiar = repo.RegistryStr() + "/" + familiar
	}
	version := ""
	if tag, ok := parsed.(name.Tag); ok {
		// A digest-only reference carries no version; BuildKit only keeps a
		// tag there when one was written explicitly.
		last := base[strings.LastIndex(base, "/")+1:]
		if digest == "" || strings.Contains(last, ":") {
			version = tag.TagStr()
		}
	}

	segments := strings.Split(familiar, "/")
	for i, s := range segments {
		segments[i] = purlEscape(s)
	}
	out := "pkg:docker/" + strings.Join(segments, "/")
	if version != "" {
		out += "@" + purlEscape(version)
	}
	var qualifiers []string // sorted by key: digest < platform
	if digest != "" {
		qualifiers = append(qualifiers, "digest="+purlEscape(digest))
	}
	if platform != nil && platform.OS != "" && platform.Architecture != "" {
		qualifiers = append(qualifiers, "platform="+purlEscape(formatPlatform(normalizePlatform(*platform))))
	}
	if len(qualifiers) > 0 {
		out += "?" + strings.Join(qualifiers, "&")
	}
	return out, nil
}

// normalizePlatform applies containerd's platform normalization, which BuildKit
// applies to the platforms it records: the default variant of an architecture is
// dropped (arm64/v8 is just arm64, amd64/v1 is amd64) and common aliases are
// spelled canonically. An image index may say linux/arm64/v8 while BuildKit
// records, and tooling matches on, linux/arm64.
func normalizePlatform(p v1.Platform) v1.Platform {
	p.OS = strings.ToLower(p.OS)
	if p.OS == "macos" {
		p.OS = "darwin"
	}
	arch, variant := strings.ToLower(p.Architecture), strings.ToLower(p.Variant)
	switch arch {
	case "i386":
		arch, variant = "386", ""
	case "x86_64", "x86-64", "amd64":
		arch = "amd64"
		if variant == "v1" {
			variant = ""
		}
	case "aarch64", "arm64":
		arch = "arm64"
		if variant == "8" || variant == "v8" {
			variant = ""
		}
	case "armhf":
		arch, variant = "arm", "v7"
	case "armel":
		arch, variant = "arm", "v6"
	case "arm":
		switch variant {
		case "", "7":
			variant = "v7"
		case "5", "6", "8":
			variant = "v" + variant
		}
	}
	p.Architecture, p.Variant = arch, variant
	return p
}

// formatPlatform renders os/arch[/variant].
func formatPlatform(p v1.Platform) string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// purlEscape percent-encodes everything but unreserved characters and ':'
// (BuildKit leaves ':' in registry ports and digests unescaped but encodes '/'
// inside qualifier values).
func purlEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~', c == ':':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// redactURL replaces any user:password in a URL with xxxxx:xxxxx, like
// BuildKit's urlutil.RedactCredentials. Strings that are not URLs with a
// scheme and userinfo are returned unchanged.
func redactURL(s string) string {
	if !strings.Contains(s, "://") {
		return s
	}
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	u.User = url.UserPassword("xxxxx", "xxxxx")
	return u.String()
}
