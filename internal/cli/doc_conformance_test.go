package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// COMMANDS.md states surface facts prose can't keep honest by itself: the
// generated-command counts paragraph and the Commands table. These tests
// derive the same facts from the live schema manifest (the single source of
// surface truth) and fail the build on drift. A failure here means COMMANDS.md
// went stale - update COMMANDS.md, not these tests.

const (
	countsAnchor = "surface-counts"
	tableAnchor  = "commands-table"
)

func docPath(t *testing.T, filename string) string {
	t.Helper()
	// Walk up from the working directory to the module root (go.mod). Works
	// under -trimpath (which strips runtime.Caller paths) and for a compiled
	// test binary run from anywhere inside the repository.
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, filename)
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent,
			"no go.mod found above the working directory; run the test from inside the repository")
		dir = parent
	}
}

func anchoredBlock(t *testing.T, filename, name string) string {
	t.Helper()
	raw, err := os.ReadFile(docPath(t, filename))
	require.NoError(t, err)
	open := "<!-- conformance:" + name + " -->"
	closing := "<!-- /conformance:" + name + " -->"
	start := strings.Index(string(raw), open)
	require.NotEqual(t, -1, start, "%s must contain the %s anchor", filename, open)
	rest := string(raw)[start+len(open):]
	end := strings.Index(rest, closing)
	require.NotEqual(t, -1, end, "%s must contain the %s anchor after %s", filename, closing, open)
	return rest[:end]
}

// surfaceCounts are the volatile integers COMMANDS.md's counts paragraph claims,
// derived here from the manifest and the kind table.
type surfaceCounts struct {
	kindTable      int // rows in kinds.All()
	get            int // generated "get <kind>" commands
	patch          int // generated "patch <kind>" commands
	del            int // generated "delete <kind>" commands (overrides included)
	deletableKinds int // kind rows with Deletable: true
	generated      int // get + patch + del
	total          int // every command in the manifest
}

// deriveSurface splits the live manifest's command names into the generated
// get/patch/delete-per-kind set and the fixed remainder, and computes the
// counts COMMANDS.md claims. A "delete <kind>" wired by an override on a
// non-deletable row (delete provisioning-cluster) is generated like any
// other - deriving from the manifest rather than kinds.Bindings() handles
// that without special-casing.
func deriveSurface(t *testing.T) (counts surfaceCounts, generated, fixed []string, manifest map[string]bool) {
	t.Helper()
	kindNames := map[string]bool{}
	for _, k := range kinds.All() {
		kindNames[k.Name] = true
		if k.Deletable {
			counts.deletableKinds++
		}
	}
	counts.kindTable = len(kinds.All())

	manifest = map[string]bool{}
	for _, name := range schemaCommandNames(t) {
		manifest[name] = true
		verb, kind, twoTokens := strings.Cut(name, " ")
		if twoTokens && kindNames[kind] {
			switch verb {
			case "get":
				counts.get++
				generated = append(generated, name)
				continue
			case "patch":
				counts.patch++
				generated = append(generated, name)
				continue
			case "delete":
				counts.del++
				generated = append(generated, name)
				continue
			}
		}
		fixed = append(fixed, name)
	}
	counts.generated = counts.get + counts.patch + counts.del
	counts.total = len(manifest)
	sort.Strings(generated)
	sort.Strings(fixed)
	return counts, generated, fixed, manifest
}

// TestGeneratedCommandMembership asserts the exact generated command set
// to the kind table plus the explicit override list, not just its size: a
// per-kind override added or removed (a new refused verb, another
// delete-on-non-deletable wiring) reshapes the surface without moving any
// count, and REFERENCE.md's verb prose would go stale silently. The
// override list mirrors internal/cli/root.go's kindOverrideTable and
// COMMANDS.md's counts-paragraph prose - change them together.
func TestGeneratedCommandMembership(t *testing.T) {
	overrides := []string{"delete provisioning-cluster"}

	expected := map[string]bool{}
	for _, k := range kinds.All() {
		expected["get "+k.Name] = true
		expected["patch "+k.Name] = true
		if k.Deletable {
			expected["delete "+k.Name] = true
		}
	}
	for _, name := range overrides {
		expected[name] = true
	}

	_, generated, _, _ := deriveSurface(t)
	actual := map[string]bool{}
	for _, name := range generated {
		actual[name] = true
	}

	var missing, extra []string
	for name := range expected {
		if !actual[name] {
			missing = append(missing, name)
		}
	}
	for name := range actual {
		if !expected[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	assert.Empty(t, missing,
		"generated commands expected from the kind table (plus overrides) but absent from the manifest - update REFERENCE.md's verb prose and this test's override list together")
	assert.Empty(t, extra,
		"generated commands in the manifest with no kind-table or override justification - update REFERENCE.md's verb prose and this test's override list together")
}

// TestSurfaceCountsMatchManifest asserts COMMANDS.md's counts paragraph (the
// conformance:surface-counts block) to the live manifest. The regex is tied
// to the paragraph's sentence shape on purpose: rewording the sentence is a
// deliberate act that must also revisit this test, while a silent count
// drift fails with the claimed-vs-real values.
func TestSurfaceCountsMatchManifest(t *testing.T) {
	counts, _, _, _ := deriveSurface(t)

	// Collapse the paragraph's line wrapping so the regex sees one line.
	flat := strings.Join(strings.Fields(anchoredBlock(t, "COMMANDS.md", countsAnchor)), " ")
	re := regexp.MustCompile("the (\\d+)-kind table: `get <kind>` \\((\\d+)\\), `patch <kind>` \\((\\d+)\\), " +
		"`delete <kind>` \\((\\d+) - (\\d+) deletable kinds.*\\) - (\\d+) generated commands, (\\d+) total")
	m := re.FindStringSubmatch(flat)
	require.NotNil(t, m,
		"COMMANDS.md's surface-counts paragraph no longer matches its expected sentence shape; keep the counts in the established sentence (or rework this regex alongside a deliberate rewording): %q", flat)

	claims := make([]int, 0, len(m)-1)
	for _, s := range m[1:] {
		n, err := strconv.Atoi(s)
		require.NoError(t, err)
		claims = append(claims, n)
	}
	real := []struct {
		what string
		want int
	}{
		{"kind-table size", counts.kindTable},
		{"generated get commands", counts.get},
		{"generated patch commands", counts.patch},
		{"generated delete commands", counts.del},
		{"deletable kinds", counts.deletableKinds},
		{"generated commands total", counts.generated},
		{"overall command total", counts.total},
	}
	for i, r := range real {
		assert.Equal(t, r.want, claims[i],
			"COMMANDS.md claims %d for %s; the live surface has %d - update COMMANDS.md, not this test",
			claims[i], r.what, r.want)
	}
}

// TestCRTPatchRefusalClaim asserts REFERENCE.md's verb prose
// "patch clusterregistrationtoken is refused - read-only" to the override
// that implements it. Membership cannot see this: a refusing override
// replaces the generic patch under the same command name, so removing the
// refusal changes no command set. The assertion is behavioral - the override
// must exist and its Plan must refuse.
func TestCRTPatchRefusalClaim(t *testing.T) {
	t.Parallel()
	o, ok := kindOverrides()["clusterregistrationtoken"]
	require.True(t, ok,
		"REFERENCE.md claims patch clusterregistrationtoken is refused, but the kind has no override - update REFERENCE.md and this test together")
	require.NotNil(t, o.patch,
		"REFERENCE.md claims patch clusterregistrationtoken is refused, but the override supplies no patch replacement - update REFERENCE.md and this test together")

	var crt kinds.Kind
	found := false
	for _, k := range kinds.All() {
		if k.Name == "clusterregistrationtoken" {
			crt, found = k, true
			break
		}
	}
	require.True(t, found)

	mc, ok := o.patch(crt, runDeps{}).(runtime.MutatingCommand)
	require.True(t, ok, "the clusterregistrationtoken patch override must build a mutating command")
	_, _, err := mc.Plan(t.Context(), nil)
	require.Error(t, err,
		"REFERENCE.md claims patch clusterregistrationtoken is refused, but Plan accepted - update REFERENCE.md and this test together")
	assert.ErrorContains(t, err, "read-only")
}

// TestKindClaimsMatchTable asserts the kind names REFERENCE.md's prose singles
// out - deletable in the counts paragraph, not deletable in the verbs
// section - to the kind table. The counts alone can stay balanced while two
// kinds swap Deletable; this catches the swap the moment it contradicts a
// named claim. The claims map mirrors COMMANDS.md's counts paragraph and
// REFERENCE.md's Kinds & verbs prose - change them together.
func TestKindClaimsMatchTable(t *testing.T) {
	t.Parallel()
	claims := map[string]bool{
		"projectroletemplatebinding": true,
		"clusterroletemplatebinding": true,
		"globalrolebinding":          true,
		"provisioning-cluster":       false,
		"bundle":                     false,
		"bundledeployment":           false,
		"machine":                    false,
		"machineset":                 false,
		"machinedeployment":          false,
		"clusterregistrationtoken":   false,
		"token":                      true,
		"kubeconfig":                 true,
		"cluster":                    false,
		"clustergroup":               false,
		"setting":                    false,
		"feature":                    false,
		"nodedriver":                 false,
		"user":                       true,
		"roletemplate":               false,
		"globalrole":                 false,
	}
	byName := map[string]kinds.Kind{}
	for _, k := range kinds.All() {
		byName[k.Name] = k
	}
	for name, wantDeletable := range claims {
		k, ok := byName[name]
		require.True(t, ok,
			"COMMANDS.md's counts paragraph and REFERENCE.md's Kinds & verbs prose name kind %q, which is not in the kind table - update them and this claims map together", name)
		assert.Equal(t, wantDeletable, k.Deletable,
			"COMMANDS.md's counts paragraph and REFERENCE.md's Kinds & verbs prose claim kind %q deletable=%v but the kind table says %v - update them and this claims map together",
			name, wantDeletable, k.Deletable)
	}
}

// primeMarker is COMMANDS.md's exact per-row label for a Prime-only
// command's Purpose cell - the same runtime.PrimeSentence every other
// rendered surface (command card, --help) uses, so the label cannot drift
// into its own wording.
const primeMarker = runtime.PrimeSentence

// primeCommandNames returns the set of manifest command names that require
// Rancher Prime, for TestCommandsTablePrimeLabels.
func primeCommandNames(t *testing.T) map[string]bool {
	t.Helper()
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	out := map[string]bool{}
	for _, c := range m.Commands {
		if c.Prime {
			out[c.Name] = true
		}
	}
	return out
}

// TestCommandsTablePrimeLabels asserts COMMANDS.md's per-row Prime label to the
// manifest, both ways: every FIXED Prime command's row carries the label, and
// no row carries the label without a matching Prime command. Scoped to fixed
// commands the same way TestCommandsTableMatchesManifest is: generated
// get/patch/delete-per-kind commands never get their own COMMANDS.md row (the
// counts paragraph and the kind's schema card cover them, e.g. `rancher
// schema oidcclient`), so a Capability-bearing kind row's generated verbs
// (the first being oidcclient, 2026-08-26) are not expected to appear here.
func TestCommandsTablePrimeLabels(t *testing.T) {
	t.Parallel()
	primeNames := primeCommandNames(t)
	_, _, fixed, _ := deriveSurface(t)
	fixedSet := map[string]bool{}
	for _, name := range fixed {
		fixedSet[name] = true
	}

	labeled := map[string]bool{}
	for _, row := range commandRows(t) {
		if strings.Contains(row.rest, primeMarker) {
			labeled[row.name] = true
		}
	}

	for name := range primeNames {
		if !fixedSet[name] {
			continue // generated per-kind command: no COMMANDS.md row expected
		}
		assert.True(t, labeled[name],
			"command %q requires Rancher Prime but its COMMANDS.md row carries no %q label", name, primeMarker)
	}
	for name := range labeled {
		assert.True(t, primeNames[name],
			"COMMANDS.md row %q carries the %q label but the manifest does not mark it Prime", name, primeMarker)
	}
}

// commandTableRow is one parsed row of COMMANDS.md's Commands table: the
// command name from the first cell, and the rest of the row after the
// closing backtick (the Purpose cell, carrying any Prime label).
type commandTableRow struct {
	name string
	rest string
}

// commandRows parses each row of COMMANDS.md's Commands table - the ONE
// parser every table-conformance test shares. The name is the first cell's
// leading backtick-quoted string, cut at the first token that starts an
// argument or flag (<, [, -, or (). Command names may be multi-word
// ("project add-namespace", "create resource").
func commandRows(t *testing.T) []commandTableRow {
	t.Helper()
	var rows []commandTableRow
	for line := range strings.SplitSeq(anchoredBlock(t, "COMMANDS.md", tableAnchor), "\n") {
		line = strings.TrimSpace(line)
		cell, isRow := strings.CutPrefix(line, "| `")
		if !isRow {
			continue // header, separator, or non-table line
		}
		quoted, rest, closed := strings.Cut(cell, "`")
		require.True(t, closed, "unterminated backtick in Commands table row %q", line)
		var name []string
		for tok := range strings.FieldsSeq(quoted) {
			if strings.ContainsAny(tok[:1], "<[-(") {
				break
			}
			name = append(name, tok)
		}
		require.NotEmpty(t, name, "no command name found in Commands table row %q", line)
		rows = append(rows, commandTableRow{name: strings.Join(name, " "), rest: rest})
	}
	require.NotEmpty(t, rows, "the conformance:commands-table block contains no table rows")
	return rows
}

// TestCommandsTableMatchesManifest asserts COMMANDS.md's Commands table to the
// live manifest, both ways: every fixed command has a row, and every row
// names a real command. Generated get/patch/delete-per-kind commands are
// exempt from needing rows - the counts paragraph covers them.
func TestCommandsTableMatchesManifest(t *testing.T) {
	_, _, fixed, manifest := deriveSurface(t)

	rows := map[string]bool{}
	for _, row := range commandRows(t) {
		rows[row.name] = true
	}

	var missing []string
	for _, name := range fixed {
		if !rows[name] {
			missing = append(missing, name)
		}
	}
	assert.Empty(t, missing,
		"commands in the schema manifest but missing from COMMANDS.md's Commands table - update COMMANDS.md, not this test")

	var unknown []string
	for name := range rows {
		if !manifest[name] {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	assert.Empty(t, unknown,
		"COMMANDS.md Commands table rows that name no command in the schema manifest - update COMMANDS.md, not this test")
}

// TestReferenceErrorCodesTableMatchesManifest asserts that REFERENCE.md's
// error-code table is the manifest's errorCodes rendered as rows: same
// codes, same order, same exit status, same meaning. A failure here means
// REFERENCE.md went stale - update REFERENCE.md, not this test.
func TestReferenceErrorCodesTableMatchesManifest(t *testing.T) {
	t.Parallel()
	block := anchoredBlock(t, "REFERENCE.md", "error-codes")
	var rows []clierr.CodeInfo
	for line := range strings.SplitSeq(block, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		require.Len(t, cells, 3, "row %q", line)
		exit, err := strconv.Atoi(strings.TrimSpace(cells[1]))
		require.NoError(t, err, "row %q", line)
		rows = append(rows, clierr.CodeInfo{
			Code: strings.Trim(strings.TrimSpace(cells[0]), "`"),
			Exit: exit,
			Desc: strings.TrimSpace(cells[2]),
		})
	}
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	assert.Equal(t, m.ErrorCodes, rows)
}

// contentsEntry is one bullet of REFERENCE.md's contents list.
type contentsEntry struct {
	label  string
	anchor string
}

var contentsBullet = regexp.MustCompile(`^- \[(.+)\]\(#([^)]+)\)$`)

// isFenceLine reports whether a line opens or closes a fenced code block and
// returns the fence marker.
func isFenceLine(line string) (string, bool) {
	trimmed := strings.TrimLeft(line, " ")
	for _, marker := range []string{"```", "~~~"} {
		if strings.HasPrefix(trimmed, marker) {
			return marker, true
		}
	}
	return "", false
}

// linesOutsideFences returns every line of doc that is not inside a fenced
// code block, fence lines excluded.
func linesOutsideFences(doc string) []string {
	var out []string
	open := ""
	for line := range strings.SplitSeq(doc, "\n") {
		if marker, ok := isFenceLine(line); ok {
			switch open {
			case "":
				open = marker
			case marker:
				open = ""
			}
			continue
		}
		if open == "" {
			out = append(out, line)
		}
	}
	return out
}

// headingAnchor renders a heading's text as the anchor GitHub gives it:
// lowercase, spaces to hyphens, every other character that is not a letter,
// digit, underscore or hyphen dropped.
func headingAnchor(text string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// contentsProblems compares the contents list at the top of a document with
// the document's `## ` headings outside fenced blocks: same entries, same
// order, each label equal to its heading's text and each anchor equal to the
// heading's anchor. It returns one message per difference.
func contentsProblems(doc string) []string {
	var headings []string
	var entries []contentsEntry
	for _, line := range linesOutsideFences(doc) {
		if text, ok := strings.CutPrefix(line, "## "); ok {
			headings = append(headings, text)
			continue
		}
		if len(headings) > 0 {
			continue
		}
		if m := contentsBullet.FindStringSubmatch(line); m != nil {
			entries = append(entries, contentsEntry{label: m[1], anchor: m[2]})
		}
	}
	var problems []string
	if len(entries) != len(headings) {
		problems = append(problems, fmt.Sprintf("the contents list has %d entries; the document has %d headings", len(entries), len(headings)))
	}
	for i, heading := range headings {
		if i >= len(entries) {
			problems = append(problems, fmt.Sprintf("heading %q has no contents entry", heading))
			continue
		}
		if entries[i].label != heading {
			problems = append(problems, fmt.Sprintf("contents entry %d is labeled %q; the heading is %q", i+1, entries[i].label, heading))
		}
		if want := headingAnchor(heading); entries[i].anchor != want {
			problems = append(problems, fmt.Sprintf("contents entry %q links #%s; the heading's anchor is #%s", entries[i].label, entries[i].anchor, want))
		}
	}
	for _, extra := range entries[min(len(headings), len(entries)):] {
		problems = append(problems, fmt.Sprintf("contents entry %q has no heading", extra.label))
	}
	return problems
}

// TestReferenceContentsListsEveryHeading asserts that REFERENCE.md's contents
// list names every `## ` heading outside fenced blocks, once and in order,
// with the heading's text as the label and the heading's anchor as the link.
// A failure here means REFERENCE.md went stale - update REFERENCE.md, not
// this test.
func TestReferenceContentsListsEveryHeading(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(docPath(t, "REFERENCE.md"))
	require.NoError(t, err)
	assert.Empty(t, contentsProblems(string(raw)))
}

const contentsFixture = `# Title

- [First & second](#first--second)
- [Third (--flag)](#third---flag)
- [Fourth](#fourth)

## First & second

` + "```" + `
## Not a heading
` + "```" + `

## Third (--flag)

## Fourth
`

// TestContentsProblemsFixture asserts that contentsProblems ignores a heading
// line inside a fenced block and reports each kind of difference between the
// contents list and the headings.
func TestContentsProblemsFixture(t *testing.T) {
	t.Parallel()
	require.Empty(t, contentsProblems(contentsFixture), "a heading line inside a fenced block is not counted")

	tests := []struct {
		name   string
		mutate func(string) string
	}{
		{"missing entry", func(s string) string { return strings.Replace(s, "- [Fourth](#fourth)\n", "", 1) }},
		{"extra heading", func(s string) string { return s + "\n## Fifth\n" }},
		{"extra entry", func(s string) string {
			return strings.Replace(s, "- [Fourth](#fourth)\n", "- [Fourth](#fourth)\n- [Fifth](#fifth)\n", 1)
		}},
		{"swapped entries", func(s string) string {
			return strings.Replace(s, "- [Third (--flag)](#third---flag)\n- [Fourth](#fourth)\n", "- [Fourth](#fourth)\n- [Third (--flag)](#third---flag)\n", 1)
		}},
		{"wrong anchor", func(s string) string { return strings.Replace(s, "#first--second", "#first-second", 1) }},
		{"wrong label", func(s string) string { return strings.Replace(s, "[Fourth]", "[Fourth part]", 1) }},
		{"fenced line counted by mistake is a heading the list lacks", func(s string) string {
			return strings.Replace(s, "```\n## Not a heading\n```", "## Not a heading", 1)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mutated := tc.mutate(contentsFixture)
			require.NotEqual(t, contentsFixture, mutated)
			assert.NotEmpty(t, contentsProblems(mutated))
		})
	}
}

// TestHeadingAnchor asserts the anchor rule on headings with ampersands,
// parentheses, backticks and an apostrophe.
func TestHeadingAnchor(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"Platforms & credential stores":              "platforms--credential-stores",
		"Project scope (--project)":                  "project-scope---project",
		"Revoking the CLI's tokens":                  "revoking-the-clis-tokens",
		"Auth configs (`--auth-provider`)":           "auth-configs---auth-provider",
		"Token reuse for kubectl (opt-in)":           "token-reuse-for-kubectl-opt-in",
		"When login fails after a token was created": "when-login-fails-after-a-token-was-created",
	}
	for heading, want := range tests {
		assert.Equal(t, want, headingAnchor(heading), heading)
	}
}

// splitTableCells splits one markdown table row into trimmed cells. A pipe
// preceded by a backslash belongs to its cell and is kept with the backslash.
func splitTableCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	if strings.HasSuffix(line, "|") && !strings.HasSuffix(line, `\|`) {
		line = strings.TrimSuffix(line, "|")
	}
	var cells []string
	var cell strings.Builder
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line) && line[i+1] == '|':
			cell.WriteString(`\|`)
			i++
		case line[i] == '|':
			cells = append(cells, strings.TrimSpace(cell.String()))
			cell.Reset()
		default:
			cell.WriteByte(line[i])
		}
	}
	return append(cells, strings.TrimSpace(cell.String()))
}

// firstTableAfterHeading returns the rows of the first table under the named
// `## ` heading (outside fenced blocks): the run of lines starting with a
// pipe, ending at the first line that does not. The header and separator rows
// are included.
func firstTableAfterHeading(doc, heading string) []string {
	var rows []string
	inSection, inTable := false, false
	for _, line := range linesOutsideFences(doc) {
		switch {
		case !inSection:
			inSection = line == "## "+heading
		case strings.HasPrefix(line, "|"):
			inTable = true
			rows = append(rows, line)
		case inTable:
			return rows
		}
	}
	return rows
}

// envTableRow is one data row of the environment variable table, with the
// code spans removed: Flag is empty for a row whose cell is a bare hyphen.
type envTableRow struct {
	Name string
	Flag string
}

// codeSpanText returns the text of a cell that is exactly one code span.
func codeSpanText(cell string) (string, bool) {
	if len(cell) > 2 && strings.HasPrefix(cell, "`") && strings.HasSuffix(cell, "`") && strings.Count(cell, "`") == 2 {
		return cell[1 : len(cell)-1], true
	}
	return "", false
}

// parseEnvTable reads the first table under the Environment variables
// heading: columns Var, Flag, Effect.
func parseEnvTable(doc string) ([]envTableRow, error) {
	table := firstTableAfterHeading(doc, "Environment variables")
	if len(table) < 2 {
		return nil, errors.New("no table under the Environment variables heading")
	}
	if header := splitTableCells(table[0]); !slices.Equal(header, []string{"Var", "Flag", "Effect"}) {
		return nil, fmt.Errorf("the table's columns are %q; want Var, Flag, Effect", header)
	}
	var rows []envTableRow
	for _, line := range table[2:] {
		cells := splitTableCells(line)
		if len(cells) != 3 {
			return nil, fmt.Errorf("row %q has %d cells; want 3", line, len(cells))
		}
		name, ok := codeSpanText(cells[0])
		if !ok {
			return nil, fmt.Errorf("row %q: the Var cell is not one code span", line)
		}
		row := envTableRow{Name: name}
		if cells[1] != "-" {
			if row.Flag, ok = codeSpanText(cells[1]); !ok {
				return nil, fmt.Errorf("row %q: the Flag cell is neither one code span nor a bare hyphen", line)
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// envTableProblems compares the environment variable table with the declared
// variables: the same names (each once) and, per variable, the declared flag.
func envTableProblems(doc string, vars []runtime.EnvVar) []string {
	rows, err := parseEnvTable(doc)
	if err != nil {
		return []string{err.Error()}
	}
	declared := map[string]string{}
	for _, v := range vars {
		declared[v.Name] = v.Flag
	}
	var problems []string
	seen := map[string]bool{}
	for _, row := range rows {
		flag, ok := declared[row.Name]
		switch {
		case seen[row.Name]:
			problems = append(problems, fmt.Sprintf("%s has more than one row", row.Name))
		case !ok:
			problems = append(problems, fmt.Sprintf("%s has a row but is not declared by EnvVars()", row.Name))
		case row.Flag != flag:
			problems = append(problems, fmt.Sprintf("%s has Flag %q; EnvVars() declares %q", row.Name, row.Flag, flag))
		}
		seen[row.Name] = true
	}
	for _, v := range vars {
		if !seen[v.Name] {
			problems = append(problems, fmt.Sprintf("%s is declared by EnvVars() and has no row", v.Name))
		}
	}
	return problems
}

// TestReferenceEnvTableMatchesEnvVars asserts that the environment variable
// table in REFERENCE.md has one row for each variable runtime.EnvVars()
// declares, and that each row's Flag cell is the declared flag. A failure
// here means REFERENCE.md went stale - update REFERENCE.md, not this test.
func TestReferenceEnvTableMatchesEnvVars(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(docPath(t, "REFERENCE.md"))
	require.NoError(t, err)
	assert.Empty(t, envTableProblems(string(raw), runtime.EnvVars()))
}

const envTableFixture = `## Environment variables

Intro text.

| Var | Flag | Effect |
|---|---|---|
| ` + "`A_VAR`" + ` | ` + "`-c/--config`" + ` | one ` + "`x`" + ` \| two \| three |
| ` + "`B_VAR`" + ` | - | plain |

### Which CA

| Var | Flag | Effect |
|---|---|---|
| ` + "`C_VAR`" + ` | ` + "`--other`" + ` | second table |

` + "```" + `
| ` + "`D_VAR`" + ` | - | in a fence |
` + "```" + `
`

var envFixtureVars = []runtime.EnvVar{
	{Name: "A_VAR", Flag: "-c/--config"},
	{Name: "B_VAR"},
}

// TestEnvTableProblemsFixture asserts that envTableProblems reads an escaped
// pipe, a backticked Flag cell, a bare hyphen and a second table correctly,
// and reports each kind of difference from the declared variables.
func TestEnvTableProblemsFixture(t *testing.T) {
	t.Parallel()
	require.Empty(t, envTableProblems(envTableFixture, envFixtureVars),
		"an escaped pipe, a backticked Flag cell, a bare hyphen and a second table are all read correctly")

	tests := []struct {
		name   string
		mutate func(string) string
		vars   []runtime.EnvVar
	}{
		{"missing row", func(s string) string { return strings.Replace(s, "| `B_VAR` | - | plain |\n", "", 1) }, envFixtureVars},
		{"undeclared row", func(s string) string {
			return strings.Replace(s, "| `B_VAR` | - | plain |\n", "| `B_VAR` | - | plain |\n| `E_VAR` | - | extra |\n", 1)
		}, envFixtureVars},
		{"duplicate row", func(s string) string {
			return strings.Replace(s, "| `B_VAR` | - | plain |\n", "| `B_VAR` | - | plain |\n| `B_VAR` | - | again |\n", 1)
		}, envFixtureVars},
		{"wrong flag", func(s string) string { return strings.Replace(s, "`-c/--config`", "`--config`", 1) }, envFixtureVars},
		{"flag on a variable that declares none", func(s string) string { return strings.Replace(s, "| `B_VAR` | - |", "| `B_VAR` | `--b` |", 1) }, envFixtureVars},
		{"bare hyphen for a declared flag", func(s string) string { return strings.Replace(s, "`-c/--config`", "-", 1) }, envFixtureVars},
		{"variable declared without a row", func(s string) string { return s }, append([]runtime.EnvVar{{Name: "F_VAR"}}, envFixtureVars...)},
		{"unescaped pipe in a cell", func(s string) string { return strings.Replace(s, `\| two`, `| two`, 1) }, envFixtureVars},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mutated := tc.mutate(envTableFixture)
			if tc.name != "variable declared without a row" {
				require.NotEqual(t, envTableFixture, mutated)
			}
			assert.NotEmpty(t, envTableProblems(mutated, tc.vars))
		})
	}
}

// TestSplitTableCells asserts that a pipe preceded by a backslash stays in its
// cell.
func TestSplitTableCells(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"`A`", "-", `x \| y`}, splitTableCells("| `A` | - | x \\| y |"))
	assert.Equal(t, []string{"a", "b"}, splitTableCells("|a|b|"))
}
