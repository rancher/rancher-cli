package nodesemver

import "regexp"

// coerceRE finds the first run of up to three numbers joined by dots that is
// not inside a longer number, with the same limit of sixteen digits per number
// as the Node.js package.
var coerceRE = regexp.MustCompile(`(^|[^\d])(\d{1,16})(?:\.(\d{1,16}))?(?:\.(\d{1,16}))?(?:$|[^\d])`)

// Coerce returns the major.minor.patch version found in s, as the Node.js
// package's coerce does with its default options: the first run of up to three
// numbers, a missing minor or patch read as 0, and any prerelease and build
// metadata dropped ("v2.15.3-rc.1+k3s1" gives "2.15.3", "2.16" gives
// "2.16.0"). It reports false when s holds no number, or the numbers are not a
// version (a part with a leading zero, or above the largest exact integer).
func Coerce(s string) (string, bool) {
	m := coerceRE.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	minor, patch := m[3], m[4]
	if minor == "" {
		minor = "0"
	}
	if patch == "" {
		patch = "0"
	}
	candidate := m[2] + "." + minor + "." + patch
	v, ok := parseVersion(candidate)
	if !ok {
		return "", false
	}
	return v.String(), true
}
