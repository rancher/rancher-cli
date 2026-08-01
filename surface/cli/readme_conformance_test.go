package cli

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// README.md states surface facts prose can't keep honest by itself: the
// generated-command counts paragraph and the Commands table. These tests
// derive the same facts from the live schema manifest (the single source of
// surface truth) and fail the build on drift. A failure here means README.md
// went stale — update README.md, not these tests.

const (
	countsAnchor = "surface-counts"
	tableAnchor  = "commands-table"
)

func readmePath(t *testing.T) string {
	t.Helper()
	// Walk up from the working directory to the module root (go.mod). Works
	// under -trimpath (which strips runtime.Caller paths) and for a compiled
	// test binary run from anywhere inside the repository.
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "README.md")
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent,
			"no go.mod found above the working directory; run the test from inside the repository")
		dir = parent
	}
}

func readmeAnchoredBlock(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(readmePath(t))
	require.NoError(t, err)
	open := "<!-- conformance:" + name + " -->"
	closing := "<!-- /conformance:" + name + " -->"
	start := strings.Index(string(raw), open)
	require.NotEqual(t, -1, start, "README.md must contain the %s anchor", open)
	rest := string(raw)[start+len(open):]
	end := strings.Index(rest, closing)
	require.NotEqual(t, -1, end, "README.md must contain the %s anchor after %s", closing, open)
	return rest[:end]
}

// surfaceCounts are the volatile integers README's counts paragraph claims,
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
// counts README claims. A "delete <kind>" wired by an override on a
// non-deletable row (delete provisioning-cluster) is generated like any
// other — deriving from the manifest rather than kinds.Bindings() handles
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

// TestReadmeGeneratedCommandMembership pins the exact generated command set
// to the kind table plus the explicit override list, not just its size: a
// per-kind override added or removed (a new refused verb, another
// delete-on-non-deletable wiring) reshapes the surface without moving any
// count, and README's "get and patch exist for every kind" prose would go
// stale silently. The override list mirrors surface/cli/root.go's
// kindOverrideTable and README's counts-paragraph prose — change them
// together.
func TestReadmeGeneratedCommandMembership(t *testing.T) {
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
		"generated commands expected from the kind table (plus overrides) but absent from the manifest — update README.md's verb prose and this test's override list together")
	assert.Empty(t, extra,
		"generated commands in the manifest with no kind-table or override justification — update README.md's verb prose and this test's override list together")
}

// TestReadmeSurfaceCountsMatchManifest pins README's counts paragraph (the
// conformance:surface-counts block) to the live manifest. The regex is tied
// to the paragraph's sentence shape on purpose: rewording the sentence is a
// deliberate act that must also revisit this test, while a silent count
// drift fails with the claimed-vs-real values.
func TestReadmeSurfaceCountsMatchManifest(t *testing.T) {
	counts, _, _, _ := deriveSurface(t)

	// Collapse the paragraph's line wrapping so the regex sees one line.
	flat := strings.Join(strings.Fields(readmeAnchoredBlock(t, countsAnchor)), " ")
	re := regexp.MustCompile("the (\\d+)-kind table: `get <kind>` \\((\\d+)\\), `patch <kind>` \\((\\d+)\\), " +
		"`delete <kind>` \\((\\d+) — (\\d+) deletable kinds.*\\) — (\\d+) generated commands, (\\d+) total")
	m := re.FindStringSubmatch(flat)
	require.NotNil(t, m,
		"README's surface-counts paragraph no longer matches its expected sentence shape; keep the counts in the established sentence (or rework this regex alongside a deliberate rewording): %q", flat)

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
			"README.md claims %d for %s; the live surface has %d — update README.md, not this test",
			claims[i], r.what, r.want)
	}
}

// TestReadmeCRTPatchRefusalClaim pins README's "patch clusterregistrationtoken
// is refused — read-only" prose to the override that implements it. Membership
// cannot see this: a refusing override replaces the generic patch under the
// same command name, so removing the refusal changes no command set. The pin
// is behavioral — the override must exist and its Plan must refuse.
func TestReadmeCRTPatchRefusalClaim(t *testing.T) {
	t.Parallel()
	o, ok := kindOverrides()["clusterregistrationtoken"]
	require.True(t, ok,
		"README claims patch clusterregistrationtoken is refused, but the kind has no override — update README.md and this test together")
	require.NotNil(t, o.patch,
		"README claims patch clusterregistrationtoken is refused, but the override supplies no patch replacement — update README.md and this test together")

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
	_, _, err := mc.Plan(context.Background(), nil)
	require.Error(t, err,
		"README claims patch clusterregistrationtoken is refused, but Plan accepted — update README.md and this test together")
	assert.Contains(t, err.Error(), "read-only")
}

// TestReadmeKindClaimsMatchTable pins the kind names README's prose singles
// out — deletable in the counts paragraph, not deletable in the verbs
// section — to the kind table. The counts alone can stay balanced while two
// kinds swap Deletable; this catches the swap the moment it contradicts a
// named claim. The claims map mirrors README's prose: change them together.
func TestReadmeKindClaimsMatchTable(t *testing.T) {
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
			"README's prose names kind %q, which is not in the kind table — update README.md and this claims map together", name)
		assert.Equal(t, wantDeletable, k.Deletable,
			"README's prose claims kind %q deletable=%v but the kind table says %v — update README.md and this claims map together",
			name, wantDeletable, k.Deletable)
	}
}

// readmeCommandRows extracts the command name from each row of README's
// Commands table: the first cell's leading backtick-quoted string, cut at
// the first token that starts an argument or flag (<, [, -, or (). Command
// names may be multi-word ("project add-namespace", "create resource").
func readmeCommandRows(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, line := range strings.Split(readmeAnchoredBlock(t, tableAnchor), "\n") {
		line = strings.TrimSpace(line)
		cell, isRow := strings.CutPrefix(line, "| `")
		if !isRow {
			continue // header, separator, or non-table line
		}
		quoted, _, closed := strings.Cut(cell, "`")
		require.True(t, closed, "unterminated backtick in Commands table row %q", line)
		var name []string
		for _, tok := range strings.Fields(quoted) {
			if strings.ContainsAny(tok[:1], "<[-(") {
				break
			}
			name = append(name, tok)
		}
		require.NotEmpty(t, name, "no command name found in Commands table row %q", line)
		names = append(names, strings.Join(name, " "))
	}
	require.NotEmpty(t, names, "the conformance:commands-table block contains no table rows")
	return names
}

// TestReadmeCommandsTableMatchesManifest pins README's Commands table to the
// live manifest, both ways: every fixed command has a row, and every row
// names a real command. Generated get/patch/delete-per-kind commands are
// exempt from needing rows — the counts paragraph covers them.
func TestReadmeCommandsTableMatchesManifest(t *testing.T) {
	_, _, fixed, manifest := deriveSurface(t)

	rows := map[string]bool{}
	for _, name := range readmeCommandRows(t) {
		rows[name] = true
	}

	var missing []string
	for _, name := range fixed {
		if !rows[name] {
			missing = append(missing, name)
		}
	}
	assert.Empty(t, missing,
		"commands in the schema manifest but missing from README's Commands table — update README.md, not this test")

	var unknown []string
	for name := range rows {
		if !manifest[name] {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	assert.Empty(t, unknown,
		"README Commands table rows that name no command in the schema manifest — update README.md, not this test")
}
