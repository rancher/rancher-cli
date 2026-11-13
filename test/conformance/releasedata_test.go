package conformance

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The query that makes steve return a Helm release Secret decoded
// (includeHelmData) is how the CLI reads Secret data, and the one read of
// Secret data of any app command is the stored-values read of app upgrade. In
// non-test Go code a constant string that holds the query's name, however it
// is split into literals and constants of one package, belongs only to
// releaseDataFile, which holds that read, and to the files named in
// releaseDataAllowlist.
const (
	releaseDataFile  = "internal/engine/catalog/release.go"
	releaseDataQuery = "includeHelmData"
)

// releaseDataAllowlist names the files outside releaseDataFile that hold the
// query's name, each with its reason.
var releaseDataAllowlist = map[string]string{}

// releaseDataMatch is one constant string that holds the query's name.
type releaseDataMatch struct {
	path string
	line int
}

// scanReleaseDataQuery reports every constant string expression of files, type
// checked together into info, that holds releaseDataQuery. An expression is
// folded the way the compiler folds it: literals, named constants of the
// package and "+".
func scanReleaseDataQuery(fset *token.FileSet, files []*ast.File, info *types.Info) []releaseDataMatch {
	var out []releaseDataMatch
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			e, ok := n.(ast.Expr)
			if !ok {
				return true
			}
			tv, ok := info.Types[e]
			if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
				return true
			}
			if strings.Contains(constant.StringVal(tv.Value), releaseDataQuery) {
				pos := fset.Position(e.Pos())
				out = append(out, releaseDataMatch{path: filepath.ToSlash(pos.Filename), line: pos.Line})
			}
			return false
		})
	}
	return out
}

// releaseDataProblems turns matches per file into the reports: the query's
// name outside releaseDataFile and the allowlist, and an allowlist entry whose
// file does not hold it.
func releaseDataProblems(matches map[string][]releaseDataMatch, allow map[string]string) []string {
	var problems []string
	for _, path := range slices.Sorted(maps.Keys(matches)) {
		if path == releaseDataFile || allow[path] != "" {
			continue
		}
		for _, f := range matches[path] {
			problems = append(problems, path+":"+strconv.Itoa(f.line)+": holds "+releaseDataQuery+", the query that returns Secret data decoded; only "+releaseDataFile+" sends it")
		}
	}
	for _, path := range slices.Sorted(maps.Keys(allow)) {
		if len(matches[path]) == 0 {
			problems = append(problems, "allowlist entry "+path+" matched nothing: remove it")
		}
	}
	if len(matches[releaseDataFile]) == 0 {
		problems = append(problems, releaseDataFile+" does not hold "+releaseDataQuery+": the scan does not see the read")
	}
	return problems
}

// TestReleaseDataQueryIsSentInOnePlace asserts, over every non-test Go file,
// that only the stored-values read of the catalog package holds a constant
// string with the name of the query that makes steve return a release Secret
// decoded, however the string is split, and that the allowlist has no entry
// that matches nothing.
func TestReleaseDataQueryIsSentInOnePlace(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	byDir := map[string][]*ast.File{}
	for i, f := range tree.files {
		dir := filepath.Dir(tree.paths[i])
		byDir[dir] = append(byDir[dir], f)
	}
	matches := map[string][]releaseDataMatch{}
	for _, dir := range slices.Sorted(maps.Keys(byDir)) {
		for _, f := range scanReleaseDataQuery(tree.fset, byDir[dir], stubTypes(tree.fset, byDir[dir])) {
			matches[f.path] = append(matches[f.path], f)
		}
	}
	assert.Empty(t, releaseDataProblems(matches, releaseDataAllowlist))
}

// TestReleaseDataScan asserts the scan sees the query's name as a literal, as
// two literals joined by +, and as a named constant joined to a literal;
// that a mention in a comment or an identifier is not reported; that a file
// outside the one that may hold it is reported; that the allowlist silences
// its file; and that an allowlist entry with no match is reported.
func TestReleaseDataScan(t *testing.T) {
	t.Parallel()
	src := `package p

// includeHelmData in a comment is not a constant string.
const prefix = "include"

var includeHelmDataIdentifier = 1

var plain = "?includeHelmData=true"
var split = "?include" + "HelmData=true"
var named = prefix + "HelmData"
var clean = "?link=index"
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "a/b.go", src, parser.AllErrors)
	require.NoError(t, err)
	got := scanReleaseDataQuery(fset, []*ast.File{file}, stubTypes(fset, []*ast.File{file}))
	var lines []int
	for _, f := range got {
		lines = append(lines, f.line)
	}
	assert.Equal(t, []int{8, 9, 10}, lines, "the literal, the split literal and the named constant")

	matches := map[string][]releaseDataMatch{"a/b.go": got, releaseDataFile: got[:1]}
	assert.Len(t, releaseDataProblems(matches, nil), 3, "one report for each match outside the read's file")
	assert.Empty(t, releaseDataProblems(matches, map[string]string{"a/b.go": "a reason"}))
	assert.Equal(t, []string{"allowlist entry c/d.go matched nothing: remove it"},
		releaseDataProblems(matches, map[string]string{"a/b.go": "a reason", "c/d.go": "a reason"}))
	assert.Equal(t, []string{releaseDataFile + " does not hold " + releaseDataQuery + ": the scan does not see the read"},
		releaseDataProblems(map[string][]releaseDataMatch{}, nil))
}
