package cli

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// surfaceKind tells the three kinds of site apart: a flag of one command, a
// positional of one command and a global flag.
type surfaceKind int

const (
	surfaceOwnFlag surfaceKind = iota
	surfacePositional
	surfaceGlobalFlag
)

// surfaceSite is one flag or positional of the schema manifest, with the
// fields the conventions read. command is empty for a global flag; name has
// no leading dashes; letters are the short forms without the dash.
type surfaceSite struct {
	kind    surfaceKind
	command string
	name    string
	typ     string
	enum    []string
	desc    string
	letters []string
}

// key writes the site as a command line shows it: "cluster scale --nodes",
// "cluster scale <cluster>", "--config" for a global flag.
func (s surfaceSite) key() string {
	switch s.kind {
	case surfacePositional:
		return s.command + " <" + s.name + ">"
	case surfaceGlobalFlag:
		return "--" + s.name
	}
	return s.command + " --" + s.name
}

// letterKey writes a short form of the site: "get pod -n (--namespace)",
// "-c (--config)" for a global flag.
func (s surfaceSite) letterKey(letter string) string {
	if s.kind == surfaceGlobalFlag {
		return "-" + letter + " (--" + s.name + ")"
	}
	return s.command + " -" + letter + " (--" + s.name + ")"
}

func (s surfaceSite) isFlag() bool { return s.kind != surfacePositional }

// isValueFlag reports a flag that takes a value: every flag but a bool flag.
func (s surfaceSite) isValueFlag() bool { return s.isFlag() && s.typ != "bool" }

// surfaceSites walks every positional and flag of every command and every
// global flag. A global flag written "-o/--output" gives the name "output"
// and the letter "o".
func surfaceSites(m runtime.Manifest) []surfaceSite {
	var sites []surfaceSite
	for _, c := range m.Commands {
		for _, a := range c.Args {
			sites = append(sites, surfaceSite{
				kind: surfacePositional, command: c.Name, name: a.Name,
				typ: a.Type, enum: a.Enum, desc: a.Desc,
			})
		}
		for _, f := range c.Flags {
			sites = append(sites, surfaceSite{
				kind: surfaceOwnFlag, command: c.Name, name: f.Name,
				typ: f.Type, enum: f.Enum, desc: f.Desc, letters: f.Aliases,
			})
		}
	}
	for _, g := range m.GlobalFlags {
		site := surfaceSite{kind: surfaceGlobalFlag, typ: g.Type, enum: g.Enum, desc: g.Desc}
		for part := range strings.SplitSeq(g.Name, "/") {
			if long, ok := strings.CutPrefix(part, "--"); ok {
				site.name = long
			} else if short, ok := strings.CutPrefix(part, "-"); ok {
				site.letters = append(site.letters, short)
			}
		}
		sites = append(sites, site)
	}
	return sites
}

// surfaceManifest builds the schema manifest in memory, the data the schema
// golden records, and asserts the walk reads commands, flags, positionals and
// global flags: a test over an empty walk would pass for the wrong reason.
func surfaceManifest(t *testing.T) runtime.Manifest {
	t.Helper()
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	var flags, positionals, globals int
	for _, s := range surfaceSites(m) {
		switch s.kind {
		case surfaceOwnFlag:
			flags++
		case surfacePositional:
			positionals++
		case surfaceGlobalFlag:
			globals++
		}
	}
	require.NotEmpty(t, m.Commands, "the manifest has no command")
	require.Positive(t, flags, "the walk saw no flag")
	require.Positive(t, positionals, "the walk saw no argument")
	require.Positive(t, globals, "the walk saw no global flag")
	return m
}

// surfaceHit is one departure from a rule: the site, written as a command
// line shows it, and what is wrong with it.
type surfaceHit struct {
	site string
	why  string
}

// surfaceRow is one allowlist row: a site that departs from the rule today,
// with the reason it may.
type surfaceRow struct {
	site   string
	reason string
}

// surfaceAllowlist has the rows of one rule, the number of rows the rule
// declares (a row cannot be added or dropped unnoticed) and the name of its
// variable, which failure messages give.
type surfaceAllowlist struct {
	name  string
	count int
	rows  []surfaceRow
}

// surfaceConventionsFile is the file that has the guards and their allowlists.
const surfaceConventionsFile = "internal/cli/surfaceconventions_test.go"

// surfaceCheck compares the departures a rule found with its allowlist and
// returns one failure for each problem: a departure with no row, a row with
// no departure, a row with no reason, two rows for one site, and a declared
// count that differs from the number of rows. rule is the label
// surfaceRuleLabel returns, and starts every failure.
func surfaceCheck(rule string, hits []surfaceHit, allow surfaceAllowlist) []string {
	var failures []string
	where := allow.name + " in " + surfaceConventionsFile
	listed := map[string]bool{}
	var rows []surfaceRow
	for _, r := range allow.rows {
		if listed[r.site] {
			failures = append(failures, fmt.Sprintf("%s: %s has two rows for %q; delete one and lower its count by one", rule, where, r.site))
			continue
		}
		listed[r.site] = true
		rows = append(rows, r)
		if strings.TrimSpace(r.reason) == "" {
			failures = append(failures, fmt.Sprintf("%s: the row for %q in %s has no reason; write one", rule, r.site, where))
		}
	}
	found := map[string]bool{}
	for _, h := range hits {
		if found[h.site] {
			continue
		}
		found[h.site] = true
		if !listed[h.site] {
			failures = append(failures, fmt.Sprintf("%s: %s: %s, or add a row with a reason to %s", rule, h.site, h.why, where))
		}
	}
	for _, r := range rows {
		if !found[r.site] {
			failures = append(failures, fmt.Sprintf("%s: %q has a row in %s, but the test finds nothing wrong with it (it follows the rule now, or it no longer exists); remove the row and lower the count by one", rule, r.site, where))
		}
	}
	if len(allow.rows) != allow.count {
		failures = append(failures, fmt.Sprintf("%s: the count of %s is %d and the number of rows is %d; set the count to %d (a higher count means a new flag, argument or command that does not follow the rule, and gets the same review as any surface change)", rule, where, allow.count, len(allow.rows), len(allow.rows)))
	}
	return failures
}

// surfaceReport fails the test once for each failure.
func surfaceReport(t *testing.T, failures []string) {
	t.Helper()
	for _, f := range failures {
		assert.Fail(t, f)
	}
}

var (
	surfaceDocHeading = regexp.MustCompile(`^## ([0-9]+)\. `)
	surfaceDocToken   = regexp.MustCompile("`([^`]*)`")
	surfaceDocLetter  = regexp.MustCompile("`-([A-Za-z])`")
)

// surfaceDocText returns the text of SURFACE.md.
func surfaceDocText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(docPath(t, "SURFACE.md"))
	require.NoError(t, err)
	return string(b)
}

// surfaceDocSection returns the lines of rule n of doc: those after the
// heading "## n. " up to the next "## " heading, each with its "\r" removed (a
// Windows checkout has CRLF line ends).
func surfaceDocSection(doc string, n int) []string {
	var lines []string
	in := false
	for line := range strings.SplitSeq(doc, "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case strings.HasPrefix(line, fmt.Sprintf("## %d. ", n)):
			in = true
		case strings.HasPrefix(line, "## "):
			in = false
		case in:
			lines = append(lines, line)
		}
	}
	return lines
}

// surfaceRuleNumber returns the number of the rule of doc whose "Checked by: "
// line has test in backticks, and false when no rule has it.
func surfaceRuleNumber(doc, test string) (int, bool) {
	number := 0
	for line := range strings.SplitSeq(doc, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if m := surfaceDocHeading.FindStringSubmatch(line); m != nil {
			number, _ = strconv.Atoi(m[1])
			continue
		}
		rest, ok := strings.CutPrefix(line, "Checked by: ")
		if !ok || number == 0 {
			continue
		}
		for _, m := range surfaceDocToken.FindAllStringSubmatch(rest, -1) {
			if m[1] == test {
				return number, true
			}
		}
	}
	return 0, false
}

// surfaceRuleLabel returns the label that starts every failure of the test t:
// the rule of doc whose "Checked by:" line names t, and the test name. It
// fails t when no rule names it.
func surfaceRuleLabel(t *testing.T, doc string) string {
	t.Helper()
	n, ok := surfaceRuleNumber(doc, t.Name())
	if !ok {
		require.Fail(t, "no rule of SURFACE.md names "+t.Name()+" on a \"Checked by:\" line")
	}
	return fmt.Sprintf("SURFACE.md rule %d (%s)", n, t.Name())
}

// surfaceDocRow is one row of the verb table in SURFACE.md: the verbs and the
// verb prefixes of its first cell, and whether the row is for a local group
// only.
type surfaceDocRow struct {
	verbs     []string
	prefixes  []string
	localOnly bool
}

// surfaceDocVerbRows returns the rows of the verb table in rule 12 of doc: the
// first run of lines that start with "|", without its header and separator.
// In the first cell of a row a backticked word that ends in "<x>" is a prefix
// without "<x>", one that ends in "-" is a prefix, and any other is a verb;
// the words "in a local group only" make the row local-only.
func surfaceDocVerbRows(doc string) []surfaceDocRow {
	var lines []string
	started := false
	for _, line := range surfaceDocSection(doc, 12) {
		if strings.HasPrefix(line, "|") {
			started = true
			lines = append(lines, line)
		} else if started {
			break
		}
	}
	if len(lines) < 2 {
		return nil
	}
	var rows []surfaceDocRow
	for _, line := range lines[2:] {
		cells := strings.Split(line, "|")
		if len(cells) < 2 {
			continue
		}
		row := surfaceDocRow{localOnly: strings.Contains(cells[1], "in a local group only")}
		for _, m := range surfaceDocToken.FindAllStringSubmatch(cells[1], -1) {
			switch word := m[1]; {
			case strings.HasSuffix(word, "<x>"):
				row.prefixes = append(row.prefixes, strings.TrimSuffix(word, "<x>"))
			case strings.HasSuffix(word, "-"):
				row.prefixes = append(row.prefixes, word)
			default:
				row.verbs = append(row.verbs, word)
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// surfaceVerbTableProblems returns a problem when the rows of the verb table
// in SURFACE.md differ from the rows of table, in order: the rows of table and
// one row with no verb for the top-level nouns. A table of a different length
// gives one problem; otherwise each row that differs gives one.
func surfaceVerbTableProblems(rows []surfaceDocRow, table surfaceVerbTable) []string {
	var want []surfaceDocRow
	for _, r := range table.rows {
		want = append(want, surfaceDocRow{verbs: r.verbs, prefixes: r.prefixes, localOnly: r.localOnly})
	}
	want = append(want, surfaceDocRow{})
	if len(rows) != len(want) {
		return []string{fmt.Sprintf("the verb table in SURFACE.md has %d rows and surfaceVerbs gives %d (its rows and one for the top-level nouns); change both together", len(rows), len(want))}
	}
	var problems []string
	for i, got := range rows {
		w := want[i]
		if !slices.Equal(got.verbs, w.verbs) || !slices.Equal(got.prefixes, w.prefixes) || got.localOnly != w.localOnly {
			problems = append(problems, fmt.Sprintf("row %d of the verb table in SURFACE.md has verbs %q, prefixes %q and in a local group only %t, but row %d of surfaceVerbs has verbs %q, prefixes %q and in a local group only %t; make them the same",
				i+1, got.verbs, got.prefixes, got.localOnly, i+1, w.verbs, w.prefixes, w.localOnly))
		}
	}
	return problems
}

// surfaceDocShortLetters returns the short flag letters written as `-X` in the
// first paragraph of rule 10 of doc, in order.
func surfaceDocShortLetters(doc string) []string {
	var paragraph []string
	for _, line := range surfaceDocSection(doc, 10) {
		if strings.TrimSpace(line) == "" {
			if len(paragraph) == 0 {
				continue
			}
			break
		}
		paragraph = append(paragraph, line)
	}
	var letters []string
	for _, m := range surfaceDocLetter.FindAllStringSubmatch(strings.Join(paragraph, " "), -1) {
		letters = append(letters, m[1])
	}
	return letters
}

// surfaceShortListProblems returns a problem when the letters rule 10 of
// SURFACE.md lists differ, in order, from the letters of shorts.
func surfaceShortListProblems(letters []string, shorts []surfaceShort) []string {
	var want []string
	for _, s := range shorts {
		want = append(want, s.letter)
	}
	if slices.Equal(letters, want) {
		return nil
	}
	return []string{fmt.Sprintf("the short flags in SURFACE.md rule 10 are %q, but surfaceShorts has %q; make them the same", letters, want)}
}

func plantedFlag(name, typ, desc string, enum ...string) runtime.ArgSpec {
	return runtime.ArgSpec{Name: name, Type: typ, Desc: desc, Enum: enum}
}

func plantedCommand(name string, ct runtime.CommandType, flags []runtime.ArgSpec, args ...runtime.ArgSpec) runtime.SchemaCommand {
	return runtime.SchemaCommand{Name: name, CommandType: ct, Flags: flags, Args: args}
}

func plantedManifest(globals []runtime.GlobalFlag, commands ...runtime.SchemaCommand) runtime.Manifest {
	return runtime.Manifest{GlobalFlags: globals, Commands: commands}
}

func hitSites(hits []surfaceHit) []string {
	var sites []string
	for _, h := range hits {
		sites = append(sites, h.site)
	}
	return sites
}

// TestSurfaceGuardsReportPlantedDepartures asserts that each matcher of the
// seven surface tests reports exactly one planted departure in a small
// hand-built manifest and none in a clean one, and that the allowlist comparison
// fails on a row with no departure, a row with no reason, two rows for one
// site and a wrong declared count. It also asserts that the comparison of
// SURFACE.md's verb table and short-flag letters with the test's copies
// reports a row or letter added or removed on one side only, and that the
// concept table refuses a chain of names.
func TestSurfaceGuardsReportPlantedDepartures(t *testing.T) {
	t.Parallel()

	t.Run("flag names", func(t *testing.T) {
		pool := []surfaceConcept{{"pool", "pool"}}
		clean := plantedManifest(nil,
			plantedCommand("cluster scale", runtime.Mutating, []runtime.ArgSpec{plantedFlag("pool", "string", "")}),
		)
		hits, problems := surfaceConceptHits(clean, pool)
		assert.Empty(t, hits)
		assert.Empty(t, problems)

		second := plantedManifest(nil,
			plantedCommand("cluster scale", runtime.Mutating, []runtime.ArgSpec{plantedFlag("pool", "string", "")}),
			plantedCommand("create cluster", runtime.Mutating, []runtime.ArgSpec{plantedFlag("pool-name", "string", "")}),
		)
		hits, problems = surfaceConceptHits(second, []surfaceConcept{{"pool", "pool"}, {"pool-name", "pool"}})
		assert.Equal(t, []string{"create cluster --pool-name"}, hitSites(hits))
		assert.Empty(t, problems)

		missing := plantedManifest(nil,
			plantedCommand("cluster scale", runtime.Mutating, []runtime.ArgSpec{plantedFlag("pool", "string", ""), plantedFlag("replicas", "string", "")}),
		)
		hits, problems = surfaceConceptHits(missing, pool)
		assert.Empty(t, hits)
		require.Len(t, problems, 1)
		assert.Contains(t, problems[0], "--replicas")

		unused := plantedManifest(nil,
			plantedCommand("cluster scale", runtime.Mutating, []runtime.ArgSpec{plantedFlag("pool", "string", "")}),
		)
		hits, problems = surfaceConceptHits(unused, []surfaceConcept{{"node-count", "quantity"}, {"pool", "pool"}})
		assert.Empty(t, hits)
		require.Len(t, problems, 1)
		assert.Contains(t, problems[0], "--node-count")

		unsorted := plantedManifest(nil,
			plantedCommand("cluster scale", runtime.Mutating, []runtime.ArgSpec{plantedFlag("pool", "string", ""), plantedFlag("quantity", "string", "")}),
		)
		_, problems = surfaceConceptHits(unsorted, []surfaceConcept{{"quantity", "quantity"}, {"pool", "pool"}})
		require.Len(t, problems, 1)
		assert.Contains(t, problems[0], "out of order")

		_, problems = surfaceConceptHits(unsorted, []surfaceConcept{{"pool", "pool"}, {"pool", "pool"}, {"quantity", "quantity"}})
		require.Len(t, problems, 1)
		assert.Contains(t, problems[0], "two rows")

		chain := plantedManifest(nil,
			plantedCommand("cluster scale", runtime.Mutating, []runtime.ArgSpec{
				plantedFlag("a", "string", ""), plantedFlag("b", "string", ""), plantedFlag("c", "string", ""),
			}),
		)
		hits, problems = surfaceConceptHits(chain, []surfaceConcept{{"a", "b"}, {"b", "c"}, {"c", "c"}})
		assert.Equal(t, []string{"cluster scale --a", "cluster scale --b"}, hitSites(hits))
		require.Len(t, problems, 1)
		assert.Contains(t, problems[0], "whose own row gives --c")
	})

	t.Run("list flags", func(t *testing.T) {
		clean := plantedManifest(nil,
			plantedCommand("create secret", runtime.Mutating, []runtime.ArgSpec{
				plantedFlag("from-literal", "stringSlice", "a key=value pair, repeatable"),
				plantedFlag("verbose", "bool", "print the command and its output, comma-separated"),
				plantedFlag("name", "string", "the command name"),
			}),
		)
		assert.Empty(t, surfaceListHits(clean))

		for _, desc := range []string{"comma-separated tags", "Comma-Separated tags", "tags separated by commas", "pairs as key=value,key=value"} {
			planted := plantedManifest(nil,
				plantedCommand("create machine-config", runtime.Mutating, []runtime.ArgSpec{plantedFlag("tags", "string", desc)}),
			)
			assert.Equal(t, []string{"create machine-config --tags"}, hitSites(surfaceListHits(planted)), desc)
		}
	})

	t.Run("switches", func(t *testing.T) {
		clean := plantedManifest(nil,
			plantedCommand("create user", runtime.Mutating, []runtime.ArgSpec{
				plantedFlag("enabled", "bool", "turn the user on"),
				plantedFlag("mode", "string", "the mode", "fast", "slow"),
			}),
		)
		assert.Empty(t, surfaceSwitchHits(clean))

		for _, flag := range []runtime.ArgSpec{
			plantedFlag("enabled", "string", "the user state", "true", "false"),
			plantedFlag("enabled", "string", "the user state", "false", "true"),
			plantedFlag("enabled", "string", "true|false"),
			plantedFlag("enabled", "string", "false|true"),
		} {
			planted := plantedManifest(nil, plantedCommand("create user", runtime.Mutating, []runtime.ArgSpec{flag}))
			assert.Equal(t, []string{"create user --enabled"}, hitSites(surfaceSwitchHits(planted)))
		}
	})

	t.Run("value lists", func(t *testing.T) {
		clean := plantedManifest(nil,
			plantedCommand("support-bundle", runtime.ReadOnly, []runtime.ArgSpec{
				plantedFlag("targets", "string", "the targets (server, agent, webhook)", "server", "agent", "webhook"),
				plantedFlag("shape", "string", "one of `a|b|c` and the default"),
				plantedFlag("note", "string", "one of two (a, b) or the other"),
				plantedFlag("force", "bool", "a|b"),
			}),
		)
		assert.Empty(t, surfaceEnumHits(clean))

		for _, desc := range []string{"the sizes a|b", "the sizes (small, medium, large)", "the targets (server, fleet-agent, kube-api-auth)"} {
			planted := plantedManifest(nil,
				plantedCommand("support-bundle", runtime.ReadOnly, []runtime.ArgSpec{plantedFlag("targets", "string", desc)}),
			)
			assert.Equal(t, []string{"support-bundle --targets"}, hitSites(surfaceEnumHits(planted)), desc)
		}
	})

	t.Run("short flags", func(t *testing.T) {
		pairs := []surfaceShort{{"n", "namespace"}, {"o", "output"}}
		clean := plantedManifest([]runtime.GlobalFlag{{Name: "-o/--output", Type: "enum"}},
			plantedCommand("get pod", runtime.ReadOnly, []runtime.ArgSpec{{Name: "namespace", Type: "string", Aliases: []string{"n"}}}),
		)
		assert.Empty(t, surfaceShortHits(clean, pairs))

		planted := plantedManifest([]runtime.GlobalFlag{{Name: "-c/--config", Type: "string"}},
			plantedCommand("delete pod", runtime.Mutating, []runtime.ArgSpec{{Name: "yes", Type: "bool", Aliases: []string{"y"}}}),
		)
		assert.Equal(t, []string{"delete pod -y (--yes)", "-c (--config)"}, hitSites(surfaceShortHits(planted, pairs)))
	})

	t.Run("positionals", func(t *testing.T) {
		clean := plantedManifest(nil,
			plantedCommand("get pod", runtime.ReadOnly, []runtime.ArgSpec{plantedFlag("namespace", "string", "")}, runtime.ArgSpec{Name: "name", Type: "string"}),
		)
		assert.Empty(t, surfaceObjectHits(clean))

		flagName := plantedManifest(nil,
			plantedCommand("config use-server", runtime.Local, []runtime.ArgSpec{plantedFlag("name", "string", "")}),
		)
		assert.Equal(t, []string{"config use-server --name"}, hitSites(surfaceObjectHits(flagName)))

		for _, name := range []string{"cluster", "namespace", "project", "server"} {
			planted := plantedManifest(nil,
				plantedCommand("delete pod", runtime.Mutating, nil, runtime.ArgSpec{Name: name, Type: "string"}),
			)
			assert.Equal(t, []string{"delete pod <" + name + ">"}, hitSites(surfaceObjectHits(planted)))
		}
	})

	t.Run("verbs", func(t *testing.T) {
		table := surfaceVerbs
		clean := plantedManifest(nil,
			plantedCommand("get pod", runtime.ReadOnly, nil),
			plantedCommand("create cluster", runtime.Mutating, nil),
			plantedCommand("config list-servers", runtime.Local, nil),
			plantedCommand("config use-server", runtime.Local, nil),
			plantedCommand("cluster set-autoscaling", runtime.Mutating, nil),
			plantedCommand("cluster scale", runtime.Mutating, nil),
			plantedCommand("node drain", runtime.Mutating, nil),
			plantedCommand("grant", runtime.Mutating, nil),
			plantedCommand("images", runtime.ReadOnly, nil),
			plantedCommand("kubeconfig", runtime.Local, nil),
		)
		assert.Empty(t, surfaceVerbHits(clean, table))

		for name, ct := range map[string]runtime.CommandType{
			"extension list":      runtime.ReadOnly,
			"get-token":           runtime.Local,
			"cluster rename":      runtime.Mutating,
			"cluster use-cluster": runtime.Mutating,
			"charts":              runtime.Mutating,
		} {
			planted := plantedManifest(nil, plantedCommand(name, ct, nil))
			assert.Equal(t, []string{name}, hitSites(surfaceVerbHits(planted, table)), name)
		}

		localVerbInMixedGroup := plantedManifest(nil,
			plantedCommand("telemetry on", runtime.Local, nil),
			plantedCommand("telemetry upload", runtime.Mutating, nil),
			plantedCommand("telemetry delete-events", runtime.Local, nil),
			plantedCommand("telemetry status", runtime.Local, nil),
		)
		assert.Equal(t, []string{"telemetry delete-events"}, hitSites(surfaceVerbHits(localVerbInMixedGroup, table)))
	})

	t.Run("allowlist", func(t *testing.T) {
		hit := surfaceHit{site: "create user --enabled", why: "a string switch"}
		row := surfaceRow{site: "create user --enabled", reason: "to become a bool flag"}
		assert.Empty(t, surfaceCheck("rule 8", []surfaceHit{hit}, surfaceAllowlist{count: 1, rows: []surfaceRow{row}}))

		failures := surfaceCheck("rule 8", nil, surfaceAllowlist{count: 1, rows: []surfaceRow{row}})
		require.Len(t, failures, 1)
		assert.Contains(t, failures[0], "finds nothing wrong")
		assert.Contains(t, failures[0], "create user --enabled")

		failures = surfaceCheck("rule 8", []surfaceHit{hit}, surfaceAllowlist{count: 1, rows: []surfaceRow{{site: row.site}}})
		require.Len(t, failures, 1)
		assert.Contains(t, failures[0], "has no reason")

		failures = surfaceCheck("rule 8", []surfaceHit{hit}, surfaceAllowlist{count: 2, rows: []surfaceRow{row, row}})
		require.Len(t, failures, 1)
		assert.Contains(t, failures[0], "two rows")

		failures = surfaceCheck("rule 8", []surfaceHit{hit}, surfaceAllowlist{count: 2, rows: []surfaceRow{row}})
		require.Len(t, failures, 1)
		assert.Contains(t, failures[0], "is 2 and the number of rows is 1")

		failures = surfaceCheck("rule 8", []surfaceHit{hit}, surfaceAllowlist{count: 0})
		require.Len(t, failures, 1)
		assert.Contains(t, failures[0], "create user --enabled")
		assert.Contains(t, failures[0], "rule 8")
	})

	t.Run("document tables", func(t *testing.T) {
		doc := strings.Join([]string{
			"## 10. Short",
			"",
			"A short flag: `-n` namespace,",
			"`-o` output.",
			"",
			"Kept on purpose: `-h`.",
			"",
			"## 12. Verbs",
			"",
			"| Verb | Meaning |",
			"|---|---|",
			"| `get` | read |",
			"| `add-<x>` | add |",
			"| a Rancher action word | act |",
			"| local-state verbs, in a local group only: `list-`, `edit` | local |",
			"| a top-level noun | print |",
			"",
			"## 3. X",
			"",
			"Checked by: `TestX` (f_test.go)",
			"",
		}, "\n")
		table := surfaceVerbTable{rows: []surfaceVerbRow{
			{verbs: []string{"get"}},
			{prefixes: []string{"add-"}},
			{commands: []string{"node drain"}},
			{verbs: []string{"edit"}, prefixes: []string{"list-"}, localOnly: true},
		}}
		shorts := []surfaceShort{{"n", "namespace"}, {"o", "output"}}
		verbProblems := func(doc string) []string {
			return surfaceVerbTableProblems(surfaceDocVerbRows(doc), table)
		}
		letterProblems := func(doc string) []string {
			return surfaceShortListProblems(surfaceDocShortLetters(doc), shorts)
		}
		change := func(old, new string) string {
			require.Contains(t, doc, old)
			return strings.Replace(doc, old, new, 1)
		}

		assert.Empty(t, verbProblems(doc))
		assert.Empty(t, letterProblems(doc))
		assert.Empty(t, verbProblems(strings.ReplaceAll(doc, "\n", "\r\n")))
		assert.Empty(t, letterProblems(strings.ReplaceAll(doc, "\n", "\r\n")))

		problems := verbProblems(change("| a top-level noun | print |\n", "| a top-level noun | print |\n| `status` | report |\n"))
		require.Len(t, problems, 1)
		assert.Contains(t, problems[0], "has 6 rows and surfaceVerbs gives 5")

		problems = verbProblems(change("`list-`, `edit`", "`list-`"))
		require.Len(t, problems, 1)
		assert.Contains(t, problems[0], "row 4")

		problems = letterProblems(change("`-o` output.", "`-w` watch, `-o` output."))
		require.Len(t, problems, 1)
		assert.Contains(t, problems[0], "rule 10")

		problems = letterProblems(change("`-o` output.", "output."))
		require.Len(t, problems, 1)
		assert.Contains(t, problems[0], "rule 10")

		assert.NotEmpty(t, verbProblems(""))
		assert.NotEmpty(t, letterProblems(""))

		n, ok := surfaceRuleNumber(doc, "TestX")
		assert.True(t, ok)
		assert.Equal(t, 3, n)
		_, ok = surfaceRuleNumber(doc, "TestY")
		assert.False(t, ok)
	})
}
