package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The refusal of an absent --cluster is written in one place, the builder in
// internal/engine/route.go, so every command gives it the same code and hint.
// The scan reads every string literal of the non-test Go files under internal/
// and cmd/ and reports one that contains a phrase below, outside that file
// and outside the allowlist.
//
// Limits: it reads string literals that contain "--cluster is required" or
// "--cluster or --all-clusters is required". A refusal of an absent --cluster
// worded differently (for example "--cluster must be set") is not found, and
// text near the phrase that is not a hit stays out (for example "pass exactly
// one of --cluster or --all-clusters"). The allowlist entry for
// internal/engine/steve/flag.go covers its four "--cluster is required with
// --project" literals. The runtime check over every command with a --cluster
// flag covers a command whose refusal reads differently, through its row.
var clusterRequiredPhrases = []string{
	"--cluster is required",
	"--cluster or --all-clusters is required",
}

const clusterRequiredBuilderFile = "internal/engine/route.go"

// clusterRequiredAllowlist maps a file to the reason it may write the phrase
// itself. Adding an entry is a decision for the maintainers: the test fails
// once the file holds no match.
var clusterRequiredAllowlist = map[string]string{
	"internal/engine/steve/flag.go": "the --project variants name a different refusal: the project decides the clusters offered",
}

// clusterRequiredScope reports whether the scan covers path.
func clusterRequiredScope(path string) bool {
	return strings.HasPrefix(path, "internal/") || strings.HasPrefix(path, "cmd/")
}

// scanClusterRequiredText returns the position of every string literal of f
// that contains one of the phrases.
func scanClusterRequiredText(fset *token.FileSet, f *ast.File) []string {
	var hits []string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		if slices.ContainsFunc(clusterRequiredPhrases, func(p string) bool { return strings.Contains(s, p) }) {
			hits = append(hits, fset.Position(lit.Pos()).String())
		}
		return true
	})
	return hits
}

// checkClusterRequiredText returns one message per file outside the builder
// file and the allowlist that holds a hit, and one per allowlist entry with
// no hit.
func checkClusterRequiredText(hits map[string][]string, allow map[string]string) []string {
	var out []string
	for _, file := range slices.Sorted(maps.Keys(hits)) {
		if file == clusterRequiredBuilderFile {
			continue
		}
		if _, ok := allow[file]; ok {
			continue
		}
		for _, pos := range hits[file] {
			out = append(out, pos+": the refusal of an absent --cluster is written by the builder in "+clusterRequiredBuilderFile+"; call it with a message from that file")
		}
	}
	for _, file := range slices.Sorted(maps.Keys(allow)) {
		if len(hits[file]) == 0 {
			out = append(out, "allowlist entry "+file+" matched nothing: remove it")
		}
	}
	return out
}

// TestClusterRequiredTextIsWrittenOnlyInTheSharedBuilder asserts no non-test
// Go file under internal/ and cmd/ writes the text of a refusal of an absent
// --cluster except the builder's file and the allowlist's entries.
func TestClusterRequiredTextIsWrittenOnlyInTheSharedBuilder(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	hits := map[string][]string{}
	scanned := 0
	for i, f := range tree.files {
		if !clusterRequiredScope(tree.paths[i]) {
			continue
		}
		scanned++
		if h := scanClusterRequiredText(tree.fset, f); len(h) > 0 {
			hits[tree.paths[i]] = h
		}
	}
	require.Greater(t, scanned, 100, "the scan reads the production tree")
	require.NotEmpty(t, hits[clusterRequiredBuilderFile], "the builder's file holds the messages")
	assert.Empty(t, checkClusterRequiredText(hits, clusterRequiredAllowlist))
}

const clusterRequiredFixturesDir = "test/conformance/testdata/clusterrequired"

// clusterRequiredFixtureFiles are the fixture files the tests below read.
var clusterRequiredFixtureFiles = []string{"planted.go", "clean.go"}

func scanClusterRequiredFixture(t *testing.T, name string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(clusterRequiredFixturesDir, name), nil, parser.AllErrors)
	require.NoError(t, err)
	return scanClusterRequiredText(fset, f)
}

// TestClusterRequiredScanFixtures asserts the scan finds each hand-written
// refusal of a planted file, and nothing in a file that only mentions the
// flag; and that a file outside the builder and the allowlist is reported,
// the allowlist silences its entry, and an entry with no hit is reported.
func TestClusterRequiredScanFixtures(t *testing.T) {
	t.Parallel()
	assert.Len(t, scanClusterRequiredFixture(t, "planted.go"), 2)
	assert.Empty(t, scanClusterRequiredFixture(t, "clean.go"))

	hits := map[string][]string{"a/planted.go": {"a/planted.go:1:1"}, clusterRequiredBuilderFile: {"x"}}
	assert.Len(t, checkClusterRequiredText(hits, nil), 1, "a file outside the builder's file is reported")
	assert.Empty(t, checkClusterRequiredText(hits, map[string]string{"a/planted.go": "a reason"}))
	got := checkClusterRequiredText(hits, map[string]string{"a/planted.go": "a reason", "b/gone.go": "a reason"})
	assert.Equal(t, []string{"allowlist entry b/gone.go matched nothing: remove it"}, got)
}

// TestClusterRequiredFixturesAreAllListed asserts the fixture directory holds
// exactly the files the scan's tests read.
func TestClusterRequiredFixturesAreAllListed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(clusterRequiredFixturesDir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, clusterRequiredFixtureFiles, names)
}
