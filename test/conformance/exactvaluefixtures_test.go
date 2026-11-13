package conformance

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// evFixtureDirs are the fixture packages of the exact-value scans. A line
// that a scan must report ends with a "// want" comment that names the
// rules ("// want 4.4 4.5"); every other line must not be reported. The
// packages whose name starts with "accepted" hold the accepted shapes and no
// such comment.
var evFixtureDirs = []string{
	"acceptedcommands", "acceptedfilters", "acceptedorigins", "acceptedwords",
	"filterbuilder", "filtermessage", "filterunlisted",
	"fragmentraw", "fragmentthenraw", "fragmentword",
	"hintcommand", "kindassign", "kindliteral",
	"leadalone", "leadconcat", "leadempty", "leadprose", "leadword",
	"originconcat", "originfield", "originfuncvalue", "originhelper", "originlocal", "originparam", "origintrim",
	"quoteaddassign", "quoteappend", "quotecommand", "quoteloop", "quoteparam", "quotereturn", "quotevar",
	"rawoperand", "recoverystring", "revokebuilder", "stringhelper", "textpart", "textthen", "twocommands", "typedline",
	"wordconversion", "worddecl", "wordgeneric", "wordjson", "wordjsonstruct", "wordreflect", "wordsscan", "wordunsafe",
}

const evFixturePkg = evFixturesDir + "/"

// evFixtureAt reads the fixture package and the line of a report.
var evFixtureAt = regexp.MustCompile(`^` + regexp.QuoteMeta(evFixturePkg) + `([^/]+)/[^:]+:(\d+)`)

// evFixtureAllow is the rows the guard 3a walk consults over the fixtures.
var evFixtureAllow = evAllow{
	originCallees: map[string]string{evFixturePkg + "acceptedorigins.canonical": "a fixture canonicalization"},
	originThrough: map[string]string{"cmp.Or": "returns one of its arguments unchanged"},
	originParams:  map[string]string{evFixturePkg + "acceptedorigins.listed": "a fixture function used as a value"},
}

var evFixtureOriginSites = map[evOriginSite]string{
	{evFixturePkg + "acceptedorigins.trimmed", "strings.TrimSpace"}: "a fixture site",
}

var evFixtureFilterRows = []evFilterRow{
	{evFixturePkg + "acceptedfilters.plain", "internal/output.Printable", false, "", "a fixture"},
	{evFixturePkg + "acceptedfilters.remedies", "internal/runtime.PrintableRemedies", true, "", "a fixture"},
	{evFixturePkg + "acceptedfilters.relayedWithReason", "internal/output.Printable", false, "a fixture reason", "a fixture"},
	{evFixturePkg + "filterbuilder.withCommand", "internal/output.Printable", false, "", "a fixture"},
	{evFixturePkg + "filtermessage.relayed", "internal/output.Printable", false, "", "a fixture"},
	{evFixturePkg + "originfield.revoke", "internal/output.Printable", false, "", "a fixture"},
	{evFixturePkg + "originlocal.build", "internal/output.Printable", false, "", "a fixture"},
	{evFixturePkg + "originparam.caller", "internal/output.Printable", false, "", "a fixture"},
}

var evFixtureCommandRows = []evCommandRow{
	{"4.5", evFixturePkg + "acceptedcommands.independent", "a fixture"},
}

// evFixtureReported is the lines the scans report in each fixture package,
// by rule, after the fixture allowlists.
func evFixtureReported(t *testing.T) map[string]map[int][]string {
	t.Helper()
	r, err := evFixtureResults()
	require.NoError(t, err)
	got := map[string]map[int][]string{}
	add := func(at, rule string) {
		m := evFixtureAt.FindStringSubmatch(at)
		if m == nil {
			return
		}
		dir := m[1]
		line, err := strconv.Atoi(m[2])
		require.NoError(t, err, at)
		if got[dir] == nil {
			got[dir] = map[int][]string{}
		}
		if !slices.Contains(got[dir][line], rule) {
			got[dir][line] = append(got[dir][line], rule)
		}
	}
	left, _ := evApplyOriginRows(r, evFixtureOriginSites, evFixtureAllow)
	for _, f := range left {
		add(f.at, "3a")
	}
	for _, p := range evCheckFilterRows(r.refs, evFixtureFilterRows) {
		add(p, "3b")
	}
	for _, rule := range []string{"4.1", "4.2", "4.3", "4.4", "4.5", "4.6", "4.7", "4.8", "4.9"} {
		left, _ := evApplyCommandRows(r.byRule(rule), rule, evFixtureCommandRows)
		for _, f := range left {
			add(f.at, rule)
		}
	}
	for _, m := range got {
		for _, rules := range m {
			slices.Sort(rules)
		}
	}
	return got
}

// evFixtureWanted is the lines each fixture marks with "// want".
func evFixtureWanted(t *testing.T, dir string) map[int][]string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(evFixturesDir, dir, "fixture.go"))
	require.NoError(t, err)
	want := map[int][]string{}
	for i, line := range strings.Split(string(src), "\n") {
		_, rules, ok := strings.Cut(line, "// want ")
		if !ok {
			continue
		}
		fields := strings.Fields(rules)
		slices.Sort(fields)
		want[i+1] = fields
	}
	return want
}

// TestExactValueScanFixtures asserts the exact-value scans on the fixture
// packages: each refused shape is reported on its marked line under its
// rule, and nothing else is reported, the accepted shapes included.
func TestExactValueScanFixtures(t *testing.T) {
	t.Parallel()
	got := evFixtureReported(t)
	for _, dir := range evFixtureDirs {
		t.Run(dir, func(t *testing.T) {
			want := evFixtureWanted(t, dir)
			if strings.HasPrefix(dir, "accepted") {
				assert.Empty(t, want, "an accepted fixture marks no line")
			} else {
				assert.NotEmpty(t, want, "a refused fixture marks the line it is reported on")
			}
			g := got[dir]
			if g == nil {
				g = map[int][]string{}
			}
			assert.Equal(t, want, g)
		})
	}
}

// TestExactValueFixturesAreAllListed asserts every fixture directory is
// listed and every listed one exists.
func TestExactValueFixturesAreAllListed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(evFixturesDir)
	require.NoError(t, err)
	var dirs []string
	for _, e := range entries {
		dirs = append(dirs, e.Name())
	}
	assert.ElementsMatch(t, evFixtureDirs, dirs)
}

// TestExactValueAllowlistRowsMustMatch asserts the allowlist rules of the
// scans on the fixtures: a row of each kind that matches nothing is
// reported, a filter row that says a command may pass a filter that changes
// it is refused outside the table, wide and name writers, and a relayed-text
// reason the walk no longer needs is reported.
func TestExactValueAllowlistRowsMustMatch(t *testing.T) {
	t.Parallel()
	r, err := evFixtureResults()
	require.NoError(t, err)

	sites := map[evOriginSite]string{{"a.gone", "strings.TrimSpace"}: "x"}
	maps.Copy(sites, evFixtureOriginSites)
	allow := evAllow{originCallees: map[string]string{"a.gone": "x"}, originThrough: map[string]string{}, originParams: map[string]string{}}
	maps.Copy(allow.originCallees, evFixtureAllow.originCallees)
	maps.Copy(allow.originThrough, evFixtureAllow.originThrough)
	maps.Copy(allow.originParams, evFixtureAllow.originParams)
	_, unused := evApplyOriginRows(r, sites, allow)
	assert.Equal(t, []string{"callee row a.gone", "site row a.gone / strings.TrimSpace"}, unused)

	rows := append(slices.Clone(evFixtureFilterRows), evFilterRow{"a.gone", "internal/output.Printable", false, "", "x"})
	assert.Contains(t, evCheckFilterRows(r.refs, rows), "the row a.gone / internal/output.Printable matched no reference; remove it")
	rows = slices.Clone(evFixtureFilterRows)
	rows[0].command = true
	assert.Contains(t, evCheckFilterRows(r.refs, rows), fmt.Sprintf("the row %s / internal/output.Printable says the text may hold a command; only PrintableLines, PrintableRemedies, the catalog values filter and the table, wide and name writers may filter one", rows[0].fn))
	rows = slices.Clone(evFixtureFilterRows)
	rows[0].relays = "no longer needed"
	assert.Contains(t, evCheckFilterRows(r.refs, rows), fmt.Sprintf("the row %s / internal/output.Printable gives a reason for relayed text the walk no longer reaches; remove the reason", rows[0].fn))

	_, unusedRows := evApplyCommandRows(r.byRule("4.5"), "4.5", append(slices.Clone(evFixtureCommandRows), evCommandRow{"4.5", "a.gone", "x"}))
	assert.Equal(t, []string{"a.gone"}, unusedRows)
}
