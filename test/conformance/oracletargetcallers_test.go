package conformance

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A test builds a fidelity oracle target with fidelity.NewOracleTargetForTest,
// which removes the users the target provisions on the shared oracle when the
// test ends. The scan below fails on every other mention of
// NewOracleTargetFromEnv: a call, a selector, a function value or a
// declaration, in any file including test files, outside the allowlist.

const oracleConstructorName = "NewOracleTargetFromEnv"

// oracleConstructorAllowlist maps "<file>:<func>" to the number of mentions
// of the constructor in that function and why the function needs the plain
// constructor. A different number of mentions fails the test.
var oracleConstructorAllowlist = map[string]requestAllowance{
	"internal/testrancher/fidelity/oracletarget.go:NewOracleTargetFromEnv":          {1, "the declaration"},
	"internal/testrancher/fidelity/oracletarget.go:NewOracleTargetForTest":          {1, "the helper that adds the cleanup"},
	"internal/testrancher/fidelity/oracletarget_test.go:TestNewOracleTargetFromEnv": {5, "unit tests of the constructor; they send nothing to a real server"},
	"test/e2e/fidelity/testmain_test.go:finishDrift":                                {1, "reads /rancherversion without a bearer and provisions no user"},
}

// scanOracleConstructor returns one match for every identifier named
// NewOracleTargetFromEnv in f, keyed by file and enclosing function.
func scanOracleConstructor(fset *token.FileSet, filePath string, f *ast.File) []requestMatch {
	var out []requestMatch
	for _, decl := range f.Decls {
		fn := "(package level)"
		if fd, ok := decl.(*ast.FuncDecl); ok {
			fn = fd.Name.Name
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == oracleConstructorName {
				out = append(out, requestMatch{
					key:  filePath + ":" + fn,
					text: fset.Position(id.Pos()).String() + ": " + oracleConstructorName + " outside the allowlist; build an oracle target in a test with fidelity.NewOracleTargetForTest, which cleans up its users",
				})
			}
			return true
		})
	}
	return out
}

// checkOracleConstructorUses parses every source in sources (path to
// content) and returns the problems checkRequestMatches finds against allow.
func checkOracleConstructorUses(sources map[string]string, allow map[string]requestAllowance) ([]string, error) {
	fset := token.NewFileSet()
	var matches []requestMatch
	for path, src := range sources {
		f, err := parser.ParseFile(fset, path, src, parser.AllErrors)
		if err != nil {
			return nil, err
		}
		matches = append(matches, scanOracleConstructor(fset, path, f)...)
	}
	return checkRequestMatches(matches, allow), nil
}

// TestOracleConstructorUsesAreAllowlisted asserts no file uses the plain
// oracle constructor outside the allowlist: a test builds its target with
// fidelity.NewOracleTargetForTest, which registers the cleanup. Another
// package cannot build a usable OracleTarget any other way, because its
// fields are unexported.
func TestOracleConstructorUsesAreAllowlisted(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	sources := map[string]string{}
	for _, path := range files {
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		if bytes.Contains(src, []byte(oracleConstructorName)) {
			sources[path] = string(src)
		}
	}
	problems, err := checkOracleConstructorUses(sources, oracleConstructorAllowlist)
	require.NoError(t, err)
	for _, problem := range problems {
		t.Error(problem)
	}
	for key, a := range oracleConstructorAllowlist {
		assert.NotEmpty(t, a.why, key)
	}
}

// TestOracleConstructorScanFlagsEveryOtherMention asserts the scan reports a
// call, a function value, a package-level use, a call through an aliased
// import and a call through a dot import outside the allowlist,
// accepts an allowlisted function with its exact count, and reports a wrong
// count and an entry that matches nothing.
func TestOracleConstructorScanFlagsEveryOtherMention(t *testing.T) {
	t.Parallel()
	const call = `package p

import "example.com/fidelity"

func TestUses(t *testing.T) {
	tgt, err := fidelity.NewOracleTargetFromEnv()
	_, _ = tgt, err
}
`
	const value = `package p

import "example.com/fidelity"

func TestValue(t *testing.T) {
	build := fidelity.NewOracleTargetFromEnv
	_ = build
}

var global = fidelity.NewOracleTargetFromEnv
`
	const aliased = `package p

import fid "example.com/fidelity"

func TestAliased(t *testing.T) {
	_, _ = fid.NewOracleTargetFromEnv()
}
`
	const dotted = `package p

import . "example.com/fidelity"

func TestDotted(t *testing.T) {
	_, _ = NewOracleTargetFromEnv()
}
`
	const commentOnly = `package p

// NewOracleTargetFromEnv is only named here.
func TestComment(t *testing.T) {
	_ = "NewOracleTargetFromEnv"
}
`
	allow := map[string]requestAllowance{
		"ok_test.go:TestOK": {2, "reason"},
	}
	const allowed = `package p

func TestOK() {
	NewOracleTargetFromEnv()
	_ = NewOracleTargetFromEnv
}
`

	problems, err := checkOracleConstructorUses(map[string]string{"call_test.go": call}, nil)
	require.NoError(t, err)
	require.Len(t, problems, 1)
	assert.Contains(t, problems[0], "call_test.go:6:")
	assert.Contains(t, problems[0], "fidelity.NewOracleTargetForTest")

	problems, err = checkOracleConstructorUses(map[string]string{"value_test.go": value}, nil)
	require.NoError(t, err)
	assert.Len(t, problems, 2, "the function value and the package-level use are both reported")

	problems, err = checkOracleConstructorUses(map[string]string{"aliased_test.go": aliased, "dotted_test.go": dotted}, nil)
	require.NoError(t, err)
	assert.Len(t, problems, 2, "a call through an aliased import and one through a dot import are both reported")

	problems, err = checkOracleConstructorUses(map[string]string{"comment_test.go": commentOnly}, nil)
	require.NoError(t, err)
	assert.Empty(t, problems, "a comment and a string literal are not mentions")

	problems, err = checkOracleConstructorUses(map[string]string{"ok_test.go": allowed}, allow)
	require.NoError(t, err)
	assert.Empty(t, problems, "an allowlisted function with its exact count passes")

	allow["ok_test.go:TestOK"] = requestAllowance{1, "reason"}
	problems, err = checkOracleConstructorUses(map[string]string{"ok_test.go": allowed}, allow)
	require.NoError(t, err)
	require.Len(t, problems, 1)
	assert.Contains(t, problems[0], "the allowlist covers 1 match(es)")

	problems, err = checkOracleConstructorUses(map[string]string{"ok_test.go": allowed}, map[string]requestAllowance{
		"gone_test.go:TestGone": {1, "reason"},
	})
	require.NoError(t, err)
	assert.Contains(t, problems, "allowlist entry gone_test.go:TestGone matched nothing: remove it")
}
