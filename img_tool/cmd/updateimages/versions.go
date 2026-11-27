package updateimages

import (
	"fmt"
	"strconv"
	"strings"
)

// versionTag is a tag that contains a version: a prefix (like "v" or
// "alpine"), dot-separated numbers, and a suffix (like "-slim-bookworm").
// Tags are only compared with tags of the same shape: the same prefix, the
// same suffix, and the same count of numbers. So "3.23" moves to "3.24" but
// not to "3.24.1" or "3.24-rc1", and "1.27-alpine" stays an alpine image.
type versionTag struct {
	prefix  string
	numbers []uint64
	suffix  string
}

func parseVersionTag(tag string) (versionTag, bool) {
	start := strings.IndexFunc(tag, func(r rune) bool { return r >= '0' && r <= '9' })
	if start < 0 {
		return versionTag{}, false
	}
	v := versionTag{prefix: tag[:start]}
	i := start
	for {
		j := i
		for j < len(tag) && tag[j] >= '0' && tag[j] <= '9' {
			j++
		}
		n, err := strconv.ParseUint(tag[i:j], 10, 64)
		if err != nil {
			return versionTag{}, false
		}
		v.numbers = append(v.numbers, n)
		i = j
		if i+1 < len(tag) && tag[i] == '.' && tag[i+1] >= '0' && tag[i+1] <= '9' {
			i++
			continue
		}
		break
	}
	v.suffix = tag[i:]
	return v, true
}

func (v versionTag) sameShape(other versionTag) bool {
	return v.prefix == other.prefix && v.suffix == other.suffix && len(v.numbers) == len(other.numbers)
}

func (v versionTag) compare(other versionTag) int {
	for i := range v.numbers {
		switch {
		case v.numbers[i] < other.numbers[i]:
			return -1
		case v.numbers[i] > other.numbers[i]:
			return 1
		}
	}
	return 0
}

// bump limits which part of a version may change in an update.
type bump int

const (
	// bumpNone never changes the tag.
	bumpNone bump = iota
	// bumpPatch keeps the first two numbers.
	bumpPatch
	// bumpMinor keeps the first number.
	bumpMinor
	// bumpMajor changes anything.
	bumpMajor
)

func parseBump(s string) (bump, error) {
	switch s {
	case "none":
		return bumpNone, nil
	case "patch":
		return bumpPatch, nil
	case "minor":
		return bumpMinor, nil
	case "major":
		return bumpMajor, nil
	}
	return 0, fmt.Errorf("invalid --max-bump %q: want major, minor, patch or none", s)
}

// fixedNumbers is how many leading numbers of a version must stay the same.
func (b bump) fixedNumbers(count int) int {
	switch b {
	case bumpMajor:
		return 0
	case bumpMinor:
		return min(1, count)
	case bumpPatch:
		return min(2, count)
	}
	return count
}

// newerVersionTag returns the highest tag of the same shape as current that
// is a higher version than current and changes no more than allowed. ok is
// false if current contains no version, or there is no such tag.
func newerVersionTag(current string, tags []string, allowed bump) (string, bool) {
	base, ok := parseVersionTag(current)
	if !ok || allowed == bumpNone {
		return "", false
	}
	fixed := allowed.fixedNumbers(len(base.numbers))
	best, bestTag := base, ""
	for _, tag := range tags {
		v, ok := parseVersionTag(tag)
		if !ok || !v.sameShape(base) {
			continue
		}
		keeps := true
		for i := 0; i < fixed; i++ {
			if v.numbers[i] != base.numbers[i] {
				keeps = false
				break
			}
		}
		if !keeps || v.compare(best) <= 0 {
			continue
		}
		best, bestTag = v, tag
	}
	return bestTag, bestTag != ""
}
