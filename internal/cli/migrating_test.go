package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// migratingRepoRoot resolves the repository root from this test file's own
// path, so both tests below can read MIGRATING.md and its supporting
// testdata without depending on the working directory `go test` was
// invoked from.
func migratingRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller must resolve this file's path")
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// TestMigratingCoversLegacyInventory keeps MIGRATING.md's reconciliation
// honest on every clean clone: legacy-commands.txt is a tracked transcription
// of a gitignored legacy command inventory document, so this test never reads
// that document directly.
func TestMigratingCoversLegacyInventory(t *testing.T) {
	t.Parallel()

	repoRoot := migratingRepoRoot(t)

	migrating, err := os.ReadFile(filepath.Join(repoRoot, "MIGRATING.md"))
	require.NoError(t, err)

	legacy, err := os.ReadFile(filepath.Join(repoRoot, "internal", "cli", "testdata", "legacy-commands.txt"))
	require.NoError(t, err)

	for line := range strings.SplitSeq(strings.TrimSpace(string(legacy)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		require.Contains(t, string(migrating), line, "legacy command %q must appear verbatim in MIGRATING.md", line)
	}
}

var (
	migratingBacktickSpan = regexp.MustCompile("`([^`]*)`")
	migratingWordToken    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9/_-]*$`)
	migratingGenericVerbs = map[string]bool{"get": true, "patch": true, "delete": true, "create": true}
)

// migratingSchemaManifest is the minimal shape of schema.golden.json this
// test needs: the flattened command name list (bespoke commands AND every
// generated verb+kind leaf, e.g. "get pod") plus the kind table (canonical
// name, aliases, and per-verb availability) needed to resolve a plural
// alias spelling like "get clusters" against its singular canonical leaf
// "get cluster".
type migratingSchemaManifest struct {
	Commands []struct {
		Name string `json:"name"`
	} `json:"commands"`
	Kinds []struct {
		Name    string   `json:"name"`
		Aliases []string `json:"aliases"`
		Verbs   map[string]struct {
			State string `json:"state"`
		} `json:"verbs"`
	} `json:"kinds"`
}

// migratingReplacementCommands extracts every `rancher ...` backtick span
// from MIGRATING.md's "## Command disposition" table and from the "Now"
// column of its "## Changed in this CLI" table, and returns the leading
// command-name portion of each: the run of plain word tokens after
// "rancher ", stopping at the first flag (-x), placeholder (<x>), or
// bracket ([x]) token. A trailing "a/b/c" shorthand (e.g. "inspect
// pod/deployment/project") expands into one candidate per alternative. A
// bare generic verb (get/patch/delete/create) immediately followed by an
// unresolved <kind>/<type> placeholder has nothing concrete to validate and
// is skipped.
func migratingReplacementCommands(t *testing.T, migrating string) []string {
	t.Helper()
	rest := migratingSection(t, migrating, "## Command disposition") + "\n" + migratingNowColumn(t, migrating)

	var out []string
	for _, m := range migratingBacktickSpan.FindAllStringSubmatch(rest, -1) {
		remainder, ok := strings.CutPrefix(m[1], "rancher ")
		if !ok {
			continue
		}
		tokens := strings.Fields(remainder)
		var words []string
		for _, tok := range tokens {
			if !migratingWordToken.MatchString(tok) {
				break
			}
			words = append(words, tok)
		}
		if len(words) == 0 {
			continue
		}
		if len(words) == 1 && migratingGenericVerbs[words[0]] && len(tokens) > 1 && strings.HasPrefix(tokens[1], "<") {
			continue
		}
		last := words[len(words)-1]
		if strings.Contains(last, "/") {
			base := words[:len(words)-1]
			for alt := range strings.SplitSeq(last, "/") {
				out = append(out, strings.Join(append(append([]string{}, base...), alt), " "))
			}
			continue
		}
		out = append(out, strings.Join(words, " "))
	}
	return out
}

// migratingSection is the text under heading, up to the next "## "
// heading.
func migratingSection(t *testing.T, migrating, heading string) string {
	t.Helper()
	start := strings.Index(migrating, heading)
	require.GreaterOrEqual(t, start, 0, "MIGRATING.md must have a %q section", heading)
	rest := migrating[start+len(heading):]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// migratingNowColumn is the "Now" cell of every row of the "## Changed in
// this CLI" table: the spelling that replaces each changed one. Every table
// line must have the two columns, and the rows must name at least one
// replacement, so a reshaped table fails here instead of reading nothing.
func migratingNowColumn(t *testing.T, migrating string) string {
	t.Helper()
	var b strings.Builder
	rows := 0
	for line := range strings.SplitSeq(migratingSection(t, migrating, "## Changed in this CLI"), "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(line, "|")
		require.Len(t, cells, 4, "a \"Changed in this CLI\" row has a Before and a Now column: %q", line)
		if strings.TrimSpace(cells[1]) == "Before" || strings.HasPrefix(strings.TrimSpace(cells[1]), "---") {
			continue
		}
		rows++
		require.Contains(t, cells[2], "`rancher ", "a Now cell names the replacement command: %q", line)
		b.WriteString(cells[2])
		b.WriteString("\n")
	}
	require.Positive(t, rows, "the \"Changed in this CLI\" table has rows")
	return b.String()
}

// TestMigratingReplacementCommandsResolve makes the disposition table and
// the changed-spelling table honest: every `rancher ...` replacement named
// in them must name a command
// that actually exists - either verbatim (a bespoke command, or a
// generated verb+kind leaf under its canonical singular name) or as a
// generic verb applied to a kind's plural alias (e.g. "get clusters"
// resolves to the "cluster" kind's available "get" verb). A future rename
// of any named command or kind alias breaks this test.
func TestMigratingReplacementCommandsResolve(t *testing.T) {
	t.Parallel()

	repoRoot := migratingRepoRoot(t)

	migrating, err := os.ReadFile(filepath.Join(repoRoot, "MIGRATING.md"))
	require.NoError(t, err)

	manifestBytes, err := os.ReadFile(filepath.Join(repoRoot, "internal", "cli", "testdata", "schema.golden.json"))
	require.NoError(t, err)
	var manifest migratingSchemaManifest
	require.NoError(t, json.Unmarshal(manifestBytes, &manifest))

	bespoke := make(map[string]bool, len(manifest.Commands))
	for _, c := range manifest.Commands {
		bespoke[c.Name] = true
	}
	type kindEntry struct{ verbs map[string]bool }
	byAlias := make(map[string]kindEntry, len(manifest.Kinds)*2)
	for _, k := range manifest.Kinds {
		entry := kindEntry{verbs: make(map[string]bool, len(k.Verbs))}
		for verb, v := range k.Verbs {
			entry.verbs[verb] = v.State == "available"
		}
		byAlias[k.Name] = entry
		for _, a := range k.Aliases {
			byAlias[a] = entry
		}
	}

	candidates := migratingReplacementCommands(t, string(migrating))
	require.NotEmpty(t, candidates, "the extractor must find at least the known replacement commands")

	var bad []string
	for _, cand := range candidates {
		if bespoke[cand] {
			continue
		}
		if parts := strings.Fields(cand); len(parts) == 2 && migratingGenericVerbs[parts[0]] {
			if k, ok := byAlias[parts[1]]; ok && k.verbs[parts[0]] {
				continue
			}
		}
		bad = append(bad, cand)
	}
	sort.Strings(bad)
	require.Empty(t, bad, "MIGRATING.md names replacement command(s) that do not resolve against the schema manifest: %v", bad)
}

// TestMigratingReplacementsNameRequiredFlags asserts every `rancher ...`
// replacement in the disposition table and in the "Now" column of the
// "Changed in this CLI" table spells out its command's required flags, so a
// reader who copies one is not refused for a missing flag. A generated
// verb named through a kind alias (get pods) is checked as its command (get
// pod); a span that names no command fails TestMigratingReplacementCommandsResolve.
func TestMigratingReplacementsNameRequiredFlags(t *testing.T) {
	t.Parallel()
	repoRoot := migratingRepoRoot(t)
	migrating, err := os.ReadFile(filepath.Join(repoRoot, "MIGRATING.md"))
	require.NoError(t, err)
	manifestBytes, err := os.ReadFile(filepath.Join(repoRoot, "internal", "cli", "testdata", "schema.golden.json"))
	require.NoError(t, err)
	var manifest struct {
		Commands []struct {
			Name  string `json:"name"`
			Flags []struct {
				Name     string   `json:"name"`
				Aliases  []string `json:"aliases"`
				Required bool     `json:"required"`
			} `json:"flags"`
		} `json:"commands"`
		Kinds []struct {
			Name    string   `json:"name"`
			Aliases []string `json:"aliases"`
		} `json:"kinds"`
	}
	require.NoError(t, json.Unmarshal(manifestBytes, &manifest))
	// required maps every command to its required flags, each as every
	// spelling that sets it.
	required := map[string][][]string{}
	for _, c := range manifest.Commands {
		required[c.Name] = nil
		for _, f := range c.Flags {
			if !f.Required {
				continue
			}
			spellings := []string{"--" + f.Name}
			for _, a := range f.Aliases {
				spellings = append(spellings, "-"+a)
			}
			required[c.Name] = append(required[c.Name], spellings)
		}
	}
	kindOf := map[string]string{}
	for _, k := range manifest.Kinds {
		kindOf[k.Name] = k.Name
		for _, a := range k.Aliases {
			kindOf[a] = k.Name
		}
	}
	command := func(words []string) (string, bool) {
		for n := min(len(words), 2); n > 0; n-- {
			if _, ok := required[strings.Join(words[:n], " ")]; ok {
				return strings.Join(words[:n], " "), true
			}
		}
		if len(words) >= 2 && migratingGenericVerbs[words[0]] {
			if k, ok := kindOf[words[1]]; ok {
				name := words[0] + " " + k
				_, ok := required[name]
				return name, ok
			}
		}
		return "", false
	}
	text := migratingSection(t, string(migrating), "## Command disposition") + "\n" + migratingNowColumn(t, string(migrating))
	checked := 0
	for _, m := range migratingBacktickSpan.FindAllStringSubmatch(text, -1) {
		rest, ok := strings.CutPrefix(m[1], "rancher ")
		if !ok {
			continue
		}
		words := strings.Fields(rest)
		// A help invocation runs no request, so it needs none of the
		// command's required flags.
		if slices.Contains(words, "--help") || slices.Contains(words, "-h") {
			continue
		}
		name, ok := command(words)
		if !ok {
			continue
		}
		checked++
		for _, spellings := range required[name] {
			assert.True(t, slices.ContainsFunc(spellings, func(s string) bool { return slices.Contains(words, s) }),
				"%q names %s without its required %s", m[1], name, spellings[0])
		}
	}
	assert.Positive(t, checked)
}
