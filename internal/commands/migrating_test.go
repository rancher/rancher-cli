package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

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

	legacy, err := os.ReadFile(filepath.Join(repoRoot, "internal", "commands", "testdata", "legacy-commands.txt"))
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
// from MIGRATING.md's "## Command disposition" table and returns the
// leading command-name portion of each: the run of plain word tokens after
// "rancher ", stopping at the first flag (-x), placeholder (<x>), or
// bracket ([x]) token. A trailing "a/b/c" shorthand (e.g. "inspect
// pod/deployment/project") expands into one candidate per alternative. A
// bare generic verb (get/patch/delete/create) immediately followed by an
// unresolved <kind>/<type> placeholder has nothing concrete to validate and
// is skipped.
func migratingReplacementCommands(t *testing.T, migrating string) []string {
	t.Helper()
	start := strings.Index(migrating, "## Command disposition")
	require.GreaterOrEqual(t, start, 0, "MIGRATING.md must have a Command disposition section")
	rest := migrating[start+len("## Command disposition"):]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		rest = rest[:end]
	}

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

// TestMigratingReplacementCommandsResolve makes the disposition table
// honest: every `rancher ...` replacement named in it must name a command
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

	manifestBytes, err := os.ReadFile(filepath.Join(repoRoot, "surface", "cli", "testdata", "schema.golden.json"))
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
