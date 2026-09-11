package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// execLineRE matches a testscript line invoking the built binary: an
// optional leading "[cond]" guard, then an optional "!" (testscript
// negation) in its own group, before "exec rancher <args>" - testscript's
// own grammar strips a leading guard first and only then checks for "!", so
// a valid conditional-negative line is always written "[cond] ! exec ...",
// never the reverse. Group 1 is the guard, group 2 the negation marker (""
// when absent), group 3 everything after "rancher ".
var execLineRE = regexp.MustCompile(`^\s*(\[[^\]]*\]\s*)?(!)?\s*exec\s+rancher\s+(.+)$`)

// minParsedInvocations is a lower bound on how many exec rancher lines
// test/e2e/scripts/*.txtar carries. A coverage-parser regression that starts
// matching far too little must fail loudly here rather than look clean
// because both failure lists it drives happen to be empty. Set to ~90% of
// the corpus size when introduced (378 lines at the time); raise it when
// scenarios are added in bulk, lower it only when scenarios are
// deliberately removed.
const minParsedInvocations = 374

// manifestAttribution is the name+alias index every exec line's tokens are
// matched against: every manifest command name (fixed and generated), plus
// each command's aliases resolved via runtime.CommandAliasNames - the same
// resolution matchesAlias (internal/runtime/schema_cmd.go) uses - so a fixed
// command growing an alias stays covered without this gate ever changing.
type manifestAttribution struct {
	names   map[string]bool
	aliases map[string]string // "<verb> <alias>" -> real command name
	// boolFlags maps every global flag spelling (each side of a "-o/--output"
	// style name, split apart) to whether runtime.GlobalFlags declares it
	// Type "bool" - the schema's own arity source, never a hand-kept list
	// that can drift from it. A spelling absent from this map (an unknown or
	// per-command flag) defaults to taking a value, matching prior behavior.
	boolFlags map[string]bool
}

func buildManifestAttribution(t *testing.T) manifestAttribution {
	t.Helper()
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	a := manifestAttribution{names: map[string]bool{}, aliases: map[string]string{}, boolFlags: map[string]bool{}}
	for _, c := range m.Commands {
		a.names[c.Name] = true
		for _, full := range runtime.CommandAliasNames(c) {
			a.aliases[full] = c.Name
		}
	}
	for _, f := range runtime.GlobalFlags() {
		isBool := f.Type == "bool"
		for spelling := range strings.SplitSeq(f.Name, "/") {
			a.boolFlags[spelling] = isBool
		}
	}
	return a
}

// attribute resolves the tokens after "exec rancher" to one manifest command
// name: skip leading global flag tokens (and their value, unless the flag is
// the single-token "--x=y" form or a manifest-declared boolean flag, neither
// of which consumes a following token), then match the longest manifest
// command name (at most two words - no manifest command is longer) that is a
// prefix of what remains, resolving aliases the same way. Returns "", false
// when no prefix of any length matches.
func attribute(tokens []string, a manifestAttribution) (string, bool) {
	i := 0
	for i < len(tokens) && strings.HasPrefix(tokens[i], "-") {
		flag, _, hasEquals := strings.Cut(tokens[i], "=")
		switch {
		case hasEquals, a.boolFlags[flag]:
			i++
		default:
			i += 2
		}
	}
	rest := tokens[min(i, len(tokens)):]
	for k := min(2, len(rest)); k >= 1; k-- {
		cand := strings.Join(rest[:k], " ")
		if a.names[cand] {
			return cand, true
		}
		if real, ok := a.aliases[cand]; ok {
			return real, true
		}
	}
	return "", false
}

// TestAttributeFlagArity asserts the three ways attribute() decides how many
// tokens a leading flag consumes: a manifest-declared boolean flag consumes
// only itself, an unrecognized (or per-command) flag defaults to consuming a
// following value, and a self-contained "--x=y" flag consumes only itself
// regardless of type.
func TestAttributeFlagArity(t *testing.T) {
	t.Parallel()
	a := buildManifestAttribution(t)
	cases := []struct {
		name   string
		tokens []string
		want   string
	}{
		{"boolean global flag takes no value", []string{"--debug", "create", "token"}, "create token"},
		{"unrecognized flag defaults to taking a value", []string{"--cluster", "c", "get", "pods"}, "get pod"},
		{"--x=y form is self-contained", []string{"--output=json", "get", "pods"}, "get pod"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := attribute(tc.tokens, a)
			require.True(t, ok, "tokens %v did not attribute", tc.tokens)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestExecLineRE is execLineRE's grammar specification: which lines match,
// which don't, and - for a match - the negation and command groups it
// extracts. Includes testscript's real guard-then-negation order and the
// rejected reverse order testscript itself never accepts.
func TestExecLineRE(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		line        string
		wantMatch   bool
		wantNegated bool
		wantCommand string
	}{
		{"bare exec", "exec rancher get pods", true, false, "get pods"},
		{"negated exec", "! exec rancher get pods", true, true, "get pods"},
		{"guard then negation", "[windows] ! exec rancher create kubeconfig --apply", true, true, "create kubeconfig --apply"},
		{"comment line, not a directive", "# exec rancher get pods", false, false, ""},
		{"exec mentioned inside an assertion", "stdout 'exec rancher'", false, false, ""},
		{"negation before guard is invalid testscript order", "! [windows] exec rancher get pods", false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := execLineRE.FindStringSubmatch(tc.line)
			if !tc.wantMatch {
				assert.Nil(t, m, "line %q unexpectedly matched execLineRE", tc.line)
				return
			}
			require.NotNil(t, m, "line %q did not match execLineRE", tc.line)
			assert.Equal(t, tc.wantNegated, m[2] != "", "negation group for %q", tc.line)
			assert.Equal(t, tc.wantCommand, m[3], "command group for %q", tc.line)
		})
	}
}

// TestEveryFixedCommandHasAnE2EScenario asserts that every fixed command in
// the schema manifest is invoked by at least one `exec rancher` line
// somewhere under test/e2e/scripts/*.txtar, and that the parser used to find
// those invocations can attribute every such line to a real command - a
// parser gap can otherwise hide a missing scenario just as effectively as
// the scenario itself being absent.
func TestEveryFixedCommandHasAnE2EScenario(t *testing.T) {
	t.Parallel()
	_, _, fixed, _ := deriveSurface(t)
	a := buildManifestAttribution(t)

	dir := docPath(t, filepath.Join("test", "e2e", "scripts"))
	files, err := filepath.Glob(filepath.Join(dir, "*.txtar"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "no txtar scenarios found under %s", dir)

	covered := map[string]bool{}
	var unattributed []string
	total := 0
	for _, file := range files {
		raw, err := os.ReadFile(file)
		require.NoError(t, err)
		for line := range strings.SplitSeq(string(raw), "\n") {
			m := execLineRE.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			total++
			negated := m[2] != ""
			tokens := strings.Fields(m[3])
			if name, ok := attribute(tokens, a); ok {
				covered[name] = true
				continue
			}
			// A negated line (proving an unknown command is rejected) or a
			// bare "<group> --generate-shell-completion" line (listing a
			// group's own subcommands, naming none of them) is unattributable
			// by design, not by parser gap - never counted as coverage,
			// never flagged as a parser failure. Accepted limitation: a
			// negated line naming a real, typo'd command (`! exec rancher
			// craete token`) is excluded the same way and so passes this
			// gate - testscript itself is what proves the typo's failure.
			if negated || slices.Contains(tokens, "--generate-shell-completion") {
				continue
			}
			unattributed = append(unattributed, filepath.Base(file)+": "+strings.TrimSpace(line))
		}
	}

	assert.GreaterOrEqual(t, total, minParsedInvocations,
		"parsed only %d exec rancher invocations across test/e2e/scripts/*.txtar, want >= %d - the line regex may be matching too little",
		total, minParsedInvocations)

	slices.Sort(unattributed)
	assert.Empty(t, unattributed,
		"exec rancher lines the coverage parser could not attribute to any manifest command (fix the parser, or the line): %v", unattributed)

	var missing []string
	for _, name := range fixed {
		if !covered[name] {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	assert.Empty(t, missing,
		"fixed commands with no `exec rancher <command>` line in any test/e2e/scripts/*.txtar scenario: %v", missing)
}
