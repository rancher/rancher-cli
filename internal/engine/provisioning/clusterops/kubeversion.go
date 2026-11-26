package clusterops

import (
	"strconv"
	"strings"
)

// MaxVersionLen is the longest version string Rancher's semantic version
// package parses: 256 bytes.
const MaxVersionLen = 256

// looseVersion is a version read as Rancher reads it: the three numbers, and
// whether a pre-release part follows.
type looseVersion struct {
	major, minor, patch uint64
	prerelease          bool
}

// parseLooseVersion parses a version exactly as the semantic version package
// Rancher uses (github.com/Masterminds/semver/v3 in its default coercing mode)
// does: at most 256 bytes; an optional lowercase v; one to three dot-separated
// decimal parts, leading zeros allowed, each fitting 64 bits; then an optional
// - and pre-release part and an optional + and build part, each made of dot
// separated identifiers of ASCII letters, digits and hyphens. A pre-release
// identifier is not empty and, when it is all digits, has no leading zero; a
// build identifier is not empty. ok is false for anything else.
func parseLooseVersion(s string) (v looseVersion, ok bool) {
	if len(s) > MaxVersionLen {
		return v, false
	}
	rest := strings.TrimPrefix(s, "v")
	var parts [3]uint64
	count := 0
	for count < 3 {
		if count > 0 {
			if !strings.HasPrefix(rest, ".") {
				break
			}
			rest = rest[1:]
		}
		digits := leadingDigits(rest)
		if digits == "" {
			return v, false
		}
		n, err := strconv.ParseUint(digits, 10, 64)
		if err != nil {
			return v, false
		}
		parts[count] = n
		count++
		rest = rest[len(digits):]
	}
	v.major, v.minor, v.patch = parts[0], parts[1], parts[2]

	if after, found := strings.CutPrefix(rest, "-"); found {
		pre, build, hasBuild := strings.Cut(after, "+")
		if !validIdentifiers(pre, true) {
			return v, false
		}
		v.prerelease = true
		return v, !hasBuild || validIdentifiers(build, false)
	}
	if after, found := strings.CutPrefix(rest, "+"); found {
		return v, validIdentifiers(after, false)
	}
	return v, rest == ""
}

// leadingDigits returns the ASCII digits at the start of s.
func leadingDigits(s string) string {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	return s[:end]
}

// validIdentifiers reports whether s is one or more non-empty dot-separated
// identifiers of ASCII letters, digits and hyphens. With noLeadingZero an
// identifier of digits only has no leading zero (a pre-release).
func validIdentifiers(s string, noLeadingZero bool) bool {
	for id := range strings.SplitSeq(s, ".") {
		if id == "" {
			return false
		}
		digitsOnly := true
		for i := range len(id) {
			c := id[i]
			switch {
			case c >= '0' && c <= '9':
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-':
				digitsOnly = false
			default:
				return false
			}
		}
		if noLeadingZero && digitsOnly && len(id) > 1 && id[0] == '0' {
			return false
		}
	}
	return true
}

// KubernetesVersionParses reports whether Rancher's version parser accepts
// the string.
func KubernetesVersionParses(version string) bool {
	_, ok := parseLooseVersion(version)
	return ok
}

// BelowKubernetes125 reports whether a Kubernetes version is below v1.25.0 as
// Rancher compares it: the three numbers are lower than 1.25.0, or equal to it
// with a pre-release part. ok is false when Rancher's parser does not accept
// the version.
func BelowKubernetes125(version string) (below, ok bool) {
	v, ok := parseLooseVersion(version)
	if !ok {
		return false, false
	}
	switch {
	case v.major != 1:
		return v.major < 1, true
	case v.minor != 25:
		return v.minor < 25, true
	}
	return v.patch == 0 && v.prerelease, true
}
