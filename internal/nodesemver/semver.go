// Package nodesemver reads the semantic version ranges that Rancher UI
// extension charts carry in their annotations, with the rules of the Node.js
// semver package that the Rancher UI evaluates them with. Go's constraint
// libraries read some of these ranges differently (a prerelease version, an
// x-range, a hyphen range, "||"), so the module has its own reader, limited to
// what the extension checks call: Satisfies and Coerce, both with the default
// options of the Node.js package (no loose parsing, no includePrerelease).
package nodesemver

import (
	"regexp"
	"strconv"
	"strings"
)

// maxSafeInteger is the largest integer a JavaScript number holds exactly. A
// version part above it is not a version.
const maxSafeInteger = 9007199254740991

// maxVersionLength is the longest version string that is read.
const maxVersionLength = 256

const (
	numericID    = `0|[1-9]\d*`
	nonNumericID = `\d*[a-zA-Z-][a-zA-Z0-9-]*`
	mainVersion  = `(` + numericID + `)\.(` + numericID + `)\.(` + numericID + `)`
	preID        = `(?:` + numericID + `|` + nonNumericID + `)`
	prerelease   = `(?:-(` + preID + `(?:\.` + preID + `)*))`
	buildID      = `[a-zA-Z0-9-]+`
	build        = `(?:\+(` + buildID + `(?:\.` + buildID + `)*))`
	fullPlain    = `v?` + mainVersion + prerelease + `?` + build + `?`

	numericIDLoose    = `\d+`
	mainVersionLoose  = `(` + numericIDLoose + `)\.(` + numericIDLoose + `)\.(` + numericIDLoose + `)`
	preIDLoose        = `(?:` + numericIDLoose + `|` + nonNumericID + `)`
	prereleaseLoose   = `(?:-?(` + preIDLoose + `(?:\.` + preIDLoose + `)*))`
	loosePlain        = `[v=\s]*` + mainVersionLoose + prereleaseLoose + `?` + build + `?`
	xIdentifier       = `(` + numericID + `|x|X|\*)`
	xRangePlain       = `[v=\s]*` + xIdentifier + `(?:\.` + xIdentifier + `(?:\.` + xIdentifier + `(?:` + prerelease + `)?` + build + `?)?)?`
	comparatorOperand = `((?:<|>)?=?)`
)

var (
	fullRE = regexp.MustCompile(`^` + fullPlain + `$`)
	digits = regexp.MustCompile(`^[0-9]+$`)
)

// version is a parsed version: three numbers and the prerelease identifiers.
// Build metadata is read and dropped, as it never affects a comparison.
type version struct {
	major, minor, patch int64
	prerelease          []string
}

// parseVersion reads a version the way the Node.js package reads a version
// string with default options: surrounding whitespace and a leading "v" are
// accepted, a leading zero in a number is not.
func parseVersion(s string) (version, bool) {
	if len(s) > maxVersionLength {
		return version{}, false
	}
	m := fullRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return version{}, false
	}
	var v version
	for i, dst := range []*int64{&v.major, &v.minor, &v.patch} {
		n, err := strconv.ParseInt(m[i+1], 10, 64)
		if err != nil || n > maxSafeInteger {
			return version{}, false
		}
		*dst = n
	}
	if m[4] != "" {
		v.prerelease = strings.Split(m[4], ".")
	}
	return v, true
}

// String is the version as the Node.js package prints it: no "v", no build
// metadata.
func (v version) String() string {
	s := strconv.FormatInt(v.major, 10) + "." + strconv.FormatInt(v.minor, 10) + "." + strconv.FormatInt(v.patch, 10)
	if len(v.prerelease) > 0 {
		s += "-" + strings.Join(v.prerelease, ".")
	}
	return s
}

// compare orders two versions: the numbers first, then the prerelease
// identifiers. A version with a prerelease is lower than the same numbers with
// none.
func compare(a, b version) int {
	for _, p := range [][2]int64{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if p[0] != p[1] {
			if p[0] < p[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a.prerelease) == 0 && len(b.prerelease) == 0:
		return 0
	case len(a.prerelease) == 0:
		return 1
	case len(b.prerelease) == 0:
		return -1
	}
	for i := 0; ; i++ {
		switch {
		case i >= len(a.prerelease) && i >= len(b.prerelease):
			return 0
		case i >= len(b.prerelease):
			return 1
		case i >= len(a.prerelease):
			return -1
		case a.prerelease[i] == b.prerelease[i]:
			continue
		}
		return compareIdentifiers(a.prerelease[i], b.prerelease[i])
	}
}

// compareIdentifiers orders two prerelease identifiers: numbers by value and
// lower than any text, text by character code.
func compareIdentifiers(a, b string) int {
	an, bn := digits.MatchString(a), digits.MatchString(b)
	switch {
	case an && bn:
		if len(a) != len(b) {
			if len(a) < len(b) {
				return -1
			}
			return 1
		}
	case an:
		return -1
	case bn:
		return 1
	}
	return strings.Compare(a, b)
}
