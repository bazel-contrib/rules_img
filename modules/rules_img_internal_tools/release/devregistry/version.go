package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// version is a Bazel module version: a SemVer core, optionally followed by a
// prerelease part.
type version struct {
	major, minor, patch int
	prerelease          string
	raw                 string
}

var versionPattern = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?$`)

func parseVersion(raw string) (version, error) {
	match := versionPattern.FindStringSubmatch(raw)
	if match == nil {
		return version{}, fmt.Errorf("%q is not a version of the form major.minor.patch[-prerelease]", raw)
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	patch, _ := strconv.Atoi(match[3])
	return version{major: major, minor: minor, patch: patch, prerelease: match[4], raw: raw}, nil
}

// core returns the version without its prerelease part.
func (v version) core() (int, int, int) {
	return v.major, v.minor, v.patch
}

// compareCore orders two versions by their SemVer core alone.
func compareCore(a, b version) int {
	aMajor, aMinor, aPatch := a.core()
	bMajor, bMinor, bPatch := b.core()
	switch {
	case aMajor != bMajor:
		return cmpInt(aMajor, bMajor)
	case aMinor != bMinor:
		return cmpInt(aMinor, bMinor)
	default:
		return cmpInt(aPatch, bPatch)
	}
}

// compareVersions orders two versions the way Bazel's module resolution does,
// closely enough to sort a list of published prereleases: by core first, and a
// prerelease before the release it precedes.
func compareVersions(a, b version) int {
	if core := compareCore(a, b); core != 0 {
		return core
	}
	switch {
	case a.prerelease == b.prerelease:
		return 0
	case a.prerelease == "":
		return 1
	case b.prerelease == "":
		return -1
	default:
		return strings.Compare(a.prerelease, b.prerelease)
	}
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// validatePrereleaseVersion checks that a version is publishable as a
// prerelease of the version the module declares.
//
// Bazel resolves a module to the *highest* version anything in the graph asks
// for, and a prerelease sorts below the release it belongs to. So a prerelease
// of the version that is already released would lose against it: any other
// module depending on the released version would silently win, and the
// prerelease a user asked for would not be built. Publishing under the next
// version up is what makes the channel usable, so it is enforced here.
func validatePrereleaseVersion(publish, declared string) error {
	published, err := parseVersion(publish)
	if err != nil {
		return err
	}
	if published.prerelease == "" {
		return fmt.Errorf("%q has no prerelease part; a pre-release version looks like 0.4.0-20260811-fa8b7de", publish)
	}
	current, err := parseVersion(declared)
	if err != nil {
		return fmt.Errorf("version declared by the module: %w", err)
	}
	if compareCore(published, current) <= 0 {
		major, minor, patch := current.core()
		return fmt.Errorf(
			"%q is not above the released version %q, so Bazel would resolve %q instead of it. Publish a prerelease of the next version up, e.g. %d.%d.%d-%s",
			publish, declared, declared, major, minor, patch+1, published.prerelease,
		)
	}
	return nil
}
