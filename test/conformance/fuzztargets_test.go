package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fuzzFunctions returns every fuzz function of the repository's test files
// as "Name|./package": a top-level function whose name starts with Fuzz and
// that takes a *testing.F.
func fuzzFunctions(t *testing.T) map[string]bool {
	t.Helper()
	files, err := repositoryFiles()
	require.NoError(t, err)
	found := map[string]bool{}
	for _, file := range files {
		if !strings.HasSuffix(file, "_test.go") || inTestdata(file) {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		require.NoError(t, err, file)
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Fuzz") || !takesTestingF(fn) {
				continue
			}
			found[fn.Name.Name+"|"+packagePath(file)] = true
		}
	}
	return found
}

// takesTestingF reports whether fn's only parameter is *testing.F.
func takesTestingF(fn *ast.FuncDecl) bool {
	params := fn.Type.Params.List
	if len(params) != 1 {
		return false
	}
	star, ok := params[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "F" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "testing"
}

// packagePath is the go test package argument of a file's directory:
// "./dir", or "." for the module root.
func packagePath(file string) string {
	if dir := path.Dir(file); dir != "." {
		return "./" + dir
	}
	return "."
}

// fuzzTargets returns the "Name|./package" lines of scripts/fuzz's TARGETS
// array, ignoring comment lines and trailing comments.
func fuzzTargets(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile("scripts/fuzz")
	require.NoError(t, err)
	_, rest, ok := strings.Cut(string(data), "TARGETS=(")
	require.True(t, ok, "scripts/fuzz has no TARGETS array")
	body, _, ok := strings.Cut(rest, ")")
	require.True(t, ok, "scripts/fuzz's TARGETS array is not closed")
	targets := map[string]bool{}
	for line := range strings.SplitSeq(body, "\n") {
		line, _, _ = strings.Cut(line, "#")
		line = strings.Trim(strings.TrimSpace(line), `"`)
		if line != "" {
			targets[line] = true
		}
	}
	return targets
}

// TestFuzzFunctionsAndTargetsAgree asserts, in both directions, that every
// fuzz function in the repository is a target of scripts/fuzz, which the
// weekly fuzz workflow runs, and that every target names a fuzz function: a
// new fuzz function that is not listed never runs there.
func TestFuzzFunctionsAndTargetsAgree(t *testing.T) {
	t.Parallel()
	functions := fuzzFunctions(t)
	targets := fuzzTargets(t)
	require.NotEmpty(t, functions)
	for f := range functions {
		assert.True(t, targets[f], "fuzz function %q is not in the TARGETS array of scripts/fuzz - add it", f)
	}
	for target := range targets {
		assert.True(t, functions[target], "scripts/fuzz target %q names no fuzz function - remove it or check the spelling", target)
	}
}
