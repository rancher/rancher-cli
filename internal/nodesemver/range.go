package nodesemver

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	spaceRE        = regexp.MustCompile(`\s+`)
	hyphenRangeRE  = regexp.MustCompile(`^\s*(` + xRangePlain + `)\s+-\s+(` + xRangePlain + `)\s*$`)
	comparatorTrim = regexp.MustCompile(`(\s*)` + comparatorOperand + `\s*(` + loosePlain + `|` + xRangePlain + `)`)
	tildeTrim      = regexp.MustCompile(`(\s*)(?:~>?)\s+`)
	caretTrim      = regexp.MustCompile(`(\s*)(?:\^)\s+`)
	tildeRE        = regexp.MustCompile(`^(?:~>?)` + xRangePlain + `$`)
	caretRE        = regexp.MustCompile(`^(?:\^)` + xRangePlain + `$`)
	xRangeRE       = regexp.MustCompile(`^` + comparatorOperand + `\s*` + xRangePlain + `$`)
	starRE         = regexp.MustCompile(`(<|>)?=?\s*\*`)
	gte0RE         = regexp.MustCompile(`^\s*>=\s*0\.0\.0\s*$`)
	comparatorRE   = regexp.MustCompile(`^` + comparatorOperand + `\s*(` + fullPlain + `)$|^$`)
)

// comparator is one operator and version of a range, such as ">=1.2.3". A nil
// version is the comparator that every version without a prerelease satisfies.
type comparator struct {
	op  string
	ver *version
}

// value is the comparator's canonical text, which is how identical comparators
// of one set are told apart.
func (c comparator) value() string {
	if c.ver == nil {
		return ""
	}
	return c.op + c.ver.String()
}

func (c comparator) isNullSet() bool { return c.value() == "<0.0.0-0" }

func (c comparator) test(v version) bool {
	if c.ver == nil {
		return true
	}
	cmp := compare(v, *c.ver)
	switch c.op {
	case "", "=":
		return cmp == 0
	case ">":
		return cmp > 0
	case ">=":
		return cmp >= 0
	case "<":
		return cmp < 0
	case "<=":
		return cmp <= 0
	}
	return false
}

// parseComparator reads one desugared comparator: an operator and a full
// version, or nothing.
func parseComparator(s string) (comparator, bool) {
	s = strings.Join(strings.Fields(s), " ")
	m := comparatorRE.FindStringSubmatch(s)
	if m == nil {
		return comparator{}, false
	}
	if m[2] == "" {
		return comparator{}, true
	}
	v, ok := parseVersion(m[2])
	if !ok {
		return comparator{}, false
	}
	op := m[1]
	if op == "=" {
		op = ""
	}
	return comparator{op: op, ver: &v}, true
}

// nodeRange is a parsed range: alternatives joined by "||", each a list of
// comparators that must all hold.
type nodeRange [][]comparator

// parseRange reads a range with the grammar of the Node.js package. It reports
// false for a range the package refuses.
func parseRange(raw string) (nodeRange, bool) {
	raw = spaceRE.ReplaceAllString(strings.TrimSpace(raw), " ")
	var set nodeRange
	for part := range strings.SplitSeq(raw, "||") {
		cs, ok := parseSet(strings.TrimSpace(part))
		if !ok {
			return nil, false
		}
		set = append(set, cs)
	}
	if len(set) > 1 {
		first := set[0]
		kept := set[:0:0]
		for _, cs := range set {
			if !cs[0].isNullSet() {
				kept = append(kept, cs)
			}
		}
		set = kept
		if len(set) == 0 {
			set = nodeRange{first}
		} else if len(set) > 1 {
			for _, cs := range set {
				if len(cs) == 1 && cs[0].ver == nil {
					set = nodeRange{cs}
					break
				}
			}
		}
	}
	return set, true
}

// parseSet reads the comparators of one alternative.
func parseSet(s string) ([]comparator, bool) {
	s = hyphenRangeRE.ReplaceAllStringFunc(s, hyphenReplace)
	s = comparatorTrim.ReplaceAllString(s, "${1}${2}${3}")
	s = tildeTrim.ReplaceAllString(s, "${1}~")
	s = caretTrim.ReplaceAllString(s, "${1}^")
	s = strings.Join(strings.Fields(s), " ")
	var parts []string
	for c := range strings.SplitSeq(s, " ") {
		parts = append(parts, replaceStars(replaceXRanges(replaceTildes(replaceCarets(c)))))
	}
	var parsed []comparator
	for _, text := range spaceRE.Split(strings.Join(parts, " "), -1) {
		if gte0RE.MatchString(strings.TrimSpace(text)) {
			text = ""
		}
		c, ok := parseComparator(text)
		if !ok {
			return nil, false
		}
		parsed = append(parsed, c)
	}
	var comps []comparator
	var order []string
	byValue := map[string]comparator{}
	for _, c := range parsed {
		if c.isNullSet() {
			return []comparator{c}, true
		}
		v := c.value()
		if _, seen := byValue[v]; !seen {
			order = append(order, v)
		}
		byValue[v] = c
	}
	if len(byValue) > 1 {
		delete(byValue, "")
	}
	for _, v := range order {
		if c, ok := byValue[v]; ok {
			comps = append(comps, c)
		}
	}
	return comps, true
}

// isX reports whether a version part is a wildcard or missing.
func isX(s string) bool { return s == "" || s == "x" || s == "X" || s == "*" }

// plusOne is the number after the version part s.
func plusOne(s string) string {
	n, _ := strconv.ParseInt(s, 10, 64)
	return strconv.FormatInt(n+1, 10)
}

// hyphenReplace turns "1.2.3 - 2.3.4" into ">=1.2.3 <=2.3.4". A missing part on
// the left is zero and a missing part on the right allows every value of it.
func hyphenReplace(s string) string {
	m := hyphenRangeRE.FindStringSubmatch(s)
	from, fM, fm, fp := m[1], m[2], m[3], m[4]
	to, tM, tm, tp, tpr := m[7], m[8], m[9], m[10], m[11]
	switch {
	case isX(fM):
		from = ""
	case isX(fm):
		from = ">=" + fM + ".0.0"
	case isX(fp):
		from = ">=" + fM + "." + fm + ".0"
	default:
		from = ">=" + from
	}
	switch {
	case isX(tM):
		to = ""
	case isX(tm):
		to = "<" + plusOne(tM) + ".0.0-0"
	case isX(tp):
		to = "<" + tM + "." + plusOne(tm) + ".0-0"
	case tpr != "":
		to = "<=" + tM + "." + tm + "." + tp + "-" + tpr
	default:
		to = "<=" + to
	}
	return strings.TrimSpace(from + " " + to)
}

func replaceTildes(s string) string {
	var out []string
	for c := range strings.FieldsSeq(s) {
		out = append(out, replaceTilde(c))
	}
	return strings.Join(out, " ")
}

// replaceTilde turns "~1.2.3" into ">=1.2.3 <1.3.0-0": patch-level changes.
func replaceTilde(s string) string {
	m := tildeRE.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	M, mi, p, pr := m[1], m[2], m[3], m[4]
	switch {
	case isX(M):
		return ""
	case isX(mi):
		return ">=" + M + ".0.0 <" + plusOne(M) + ".0.0-0"
	case isX(p):
		return ">=" + M + "." + mi + ".0 <" + M + "." + plusOne(mi) + ".0-0"
	case pr != "":
		return ">=" + M + "." + mi + "." + p + "-" + pr + " <" + M + "." + plusOne(mi) + ".0-0"
	}
	return ">=" + M + "." + mi + "." + p + " <" + M + "." + plusOne(mi) + ".0-0"
}

func replaceCarets(s string) string {
	var out []string
	for c := range strings.FieldsSeq(s) {
		out = append(out, replaceCaret(c))
	}
	return strings.Join(out, " ")
}

// replaceCaret turns "^1.2.3" into ">=1.2.3 <2.0.0-0": changes that keep the
// leftmost non-zero part.
func replaceCaret(s string) string {
	m := caretRE.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	M, mi, p, pr := m[1], m[2], m[3], m[4]
	switch {
	case isX(M):
		return ""
	case isX(mi):
		return ">=" + M + ".0.0 <" + plusOne(M) + ".0.0-0"
	case isX(p):
		if M == "0" {
			return ">=" + M + "." + mi + ".0 <" + M + "." + plusOne(mi) + ".0-0"
		}
		return ">=" + M + "." + mi + ".0 <" + plusOne(M) + ".0.0-0"
	}
	low := ">=" + M + "." + mi + "." + p
	if pr != "" {
		low += "-" + pr
	}
	switch {
	case M != "0":
		return low + " <" + plusOne(M) + ".0.0-0"
	case mi == "0":
		return low + " <" + M + "." + mi + "." + plusOne(p) + "-0"
	}
	return low + " <" + M + "." + plusOne(mi) + ".0-0"
}

func replaceXRanges(s string) string {
	var out []string
	for c := range strings.FieldsSeq(s) {
		out = append(out, replaceXRange(c))
	}
	return strings.Join(out, " ")
}

// replaceXRange turns a comparator with a wildcard or missing part, such as
// "1.x", ">1.2" or "<=1", into plain comparators. A comparator with all three
// parts is returned as it is.
func replaceXRange(s string) string {
	s = strings.TrimSpace(s)
	m := xRangeRE.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	gtlt, M, mi, p := m[1], m[2], m[3], m[4]
	xM := isX(M)
	xm := xM || isX(mi)
	xp := xm || isX(p)
	if gtlt == "=" && xp {
		gtlt = ""
	}
	switch {
	case xM:
		if gtlt == ">" || gtlt == "<" {
			return "<0.0.0-0"
		}
		return "*"
	case gtlt != "" && xp:
		if xm {
			mi = "0"
		}
		p = "0"
		pr := ""
		switch gtlt {
		case ">":
			gtlt = ">="
			if xm {
				M = plusOne(M)
				mi = "0"
			} else {
				mi = plusOne(mi)
			}
		case "<=":
			gtlt = "<"
			if xm {
				M = plusOne(M)
			} else {
				mi = plusOne(mi)
			}
		}
		if gtlt == "<" {
			pr = "-0"
		}
		return gtlt + M + "." + mi + "." + p + pr
	case xm:
		return ">=" + M + ".0.0 <" + plusOne(M) + ".0.0-0"
	case xp:
		return ">=" + M + "." + mi + ".0 <" + M + "." + plusOne(mi) + ".0-0"
	}
	return s
}

// replaceStars drops a lone "*", with or without an operator, which allows
// every version.
func replaceStars(s string) string {
	s = strings.TrimSpace(s)
	if loc := starRE.FindStringIndex(s); loc != nil {
		return s[:loc[0]] + s[loc[1]:]
	}
	return s
}

// test reports whether v satisfies the range. A version with a prerelease
// satisfies an alternative only when one of its comparators has the same
// major, minor and patch and a prerelease of its own.
func (r nodeRange) test(v version) bool {
	for _, cs := range r {
		if setTest(cs, v) {
			return true
		}
	}
	return false
}

func setTest(cs []comparator, v version) bool {
	for _, c := range cs {
		if !c.test(v) {
			return false
		}
	}
	if len(v.prerelease) == 0 {
		return true
	}
	for _, c := range cs {
		if c.ver == nil || len(c.ver.prerelease) == 0 {
			continue
		}
		if c.ver.major == v.major && c.ver.minor == v.minor && c.ver.patch == v.patch {
			return true
		}
	}
	return false
}

// Satisfies reports whether ver satisfies the range, as the Node.js
// package's satisfies does with its default options. A version or a range the
// package refuses is false, and so is an empty version.
func Satisfies(ver, rng string) bool {
	r, ok := parseRange(rng)
	if !ok {
		return false
	}
	v, ok := parseVersion(ver)
	if !ok {
		return false
	}
	return r.test(v)
}
