package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A fidelity test that runs a probe against a real Rancher calls
// runProbeOrSkip in test/e2e/fidelity/runner_test.go, which skips the test as
// not checked when the server's minor is newer than the probe's MaxMinor. The
// test below fails when another file in test/e2e/fidelity names RunProbe,
// RunProbeWith or RunProbeOrSkip, or imports the fidelity package under
// another name or with a dot, which would hide a call from the scan. No file
// is exempt.

const (
	fidelityRunnerDir      = "test/e2e/fidelity/"
	fidelityRunnerFile     = "test/e2e/fidelity/runner_test.go"
	fidelityPackagePath    = "github.com/rancher/rancher-cli/internal/testrancher/fidelity"
	fidelityRunnerFunction = "RunProbeOrSkip"
)

// checkFidelityRunnerUses parses every source in sources (path to content)
// and returns one problem for each of these: an identifier that starts with
// RunProbe in a file other than file; any such identifier but RunProbeOrSkip
// in file; an import of the fidelity package under a name; a file that never
// calls fidelity.RunProbeOrSkip.
func checkFidelityRunnerUses(sources map[string]string, file string) ([]string, error) {
	var problems []string
	fset := token.NewFileSet()
	called := false
	for path, src := range sources {
		f, err := parser.ParseFile(fset, path, src, parser.AllErrors)
		if err != nil {
			return nil, err
		}
		for _, imp := range f.Imports {
			if p, err := strconv.Unquote(imp.Path.Value); err == nil && p == fidelityPackagePath && imp.Name != nil {
				problems = append(problems, fset.Position(imp.Pos()).String()+": the fidelity package is imported as "+imp.Name.Name+"; import it without a name, and every call reads fidelity.<Name>")
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || !strings.HasPrefix(id.Name, "RunProbe") {
				return true
			}
			if path == file {
				if id.Name != fidelityRunnerFunction {
					problems = append(problems, fset.Position(id.Pos()).String()+": "+id.Name+" in "+file+"; only "+fidelityRunnerFunction+" may be named there")
				}
				return true
			}
			problems = append(problems, fset.Position(id.Pos()).String()+": "+id.Name+" outside "+file+"; run a probe through runProbeOrSkip in "+file+", which skips the test as not checked when the server's minor is newer than the probe's MaxMinor")
			return true
		})
		if path != file {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == fidelityRunnerFunction {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "fidelity" {
					called = true
				}
			}
			return true
		})
	}
	if _, ok := sources[file]; !ok {
		problems = append(problems, file+" is missing")
	} else if !called {
		problems = append(problems, file+" never calls fidelity."+fidelityRunnerFunction)
	}
	return problems, nil
}

// fidelityRunnerSources returns the content of every Go file under
// fidelityRunnerDir, from the list of repository files, as a map from each
// file's slash-separated path (from the module root) to its content.
func fidelityRunnerSources(t *testing.T) map[string]string {
	t.Helper()
	files, err := repositoryFiles()
	require.NoError(t, err)
	sources := map[string]string{}
	for _, path := range files {
		if !strings.HasPrefix(path, fidelityRunnerDir) || !strings.HasSuffix(path, ".go") {
			continue
		}
		src, err := os.ReadFile(filepath.FromSlash(path))
		require.NoError(t, err)
		sources[path] = string(src)
	}
	return sources
}

// TestFidelityProbesRunThroughRunProbeOrSkip asserts that no Go file in
// test/e2e/fidelity names RunProbe, RunProbeWith or RunProbeOrSkip except
// runner_test.go, which names only RunProbeOrSkip and calls it, and that no
// file there imports the fidelity package under another name. A test that ran
// a probe directly would record, verify or compare it on a Rancher newer than
// its MaxMinor, or fail a drift run on it.
func TestFidelityProbesRunThroughRunProbeOrSkip(t *testing.T) {
	t.Parallel()
	sources := fidelityRunnerSources(t)
	require.Contains(t, sources, fidelityRunnerFile)
	assert.Greater(t, len(sources), 1, "the scan reads the other test files too")
	problems, err := checkFidelityRunnerUses(sources, fidelityRunnerFile)
	require.NoError(t, err)
	for _, problem := range problems {
		t.Error(problem)
	}
}

// TestFidelityRunnerScanFlagsEveryOtherMention asserts the scan reports a
// call of RunProbe and of RunProbeWith, a call of RunProbeOrSkip, a call
// through an aliased import and one through a dot import in a file other than
// the runner file, a runner file that names RunProbe or never calls
// RunProbeOrSkip, and a missing runner file; and that it accepts the runner
// file with its call, and a mention in a comment or a string.
func TestFidelityRunnerScanFlagsEveryOtherMention(t *testing.T) {
	t.Parallel()
	const runner = "runner_test.go"
	const goodRunner = `package p

import "` + fidelityPackagePath + `"

func runProbeOrSkip() {
	fidelity.RunProbeOrSkip()
}
`
	const direct = `package p

import "` + fidelityPackagePath + `"

func TestDirect() {
	_, _ = fidelity.RunProbe()
	_, _ = fidelity.RunProbeWith()
}
`
	const otherCall = `package p

import "` + fidelityPackagePath + `"

func TestOther() {
	fidelity.RunProbeOrSkip()
}
`
	const aliased = `package p

import f "` + fidelityPackagePath + `"

func TestAliased() {
	_, _ = f.RunProbe()
}
`
	const dotted = `package p

import . "` + fidelityPackagePath + `"

func TestDotted() {
	_, _ = RunProbe()
}
`
	const commentOnly = `package p

// RunProbe is only named here.
func TestComment() {
	_ = "RunProbeWith"
}
`
	const runnerRunsDirect = `package p

import "` + fidelityPackagePath + `"

func runProbeOrSkip() {
	fidelity.RunProbeOrSkip()
	fidelity.RunProbe()
}
`
	const runnerNeverCalls = `package p

func runProbeOrSkip() {}
`

	check := func(files map[string]string) []string {
		t.Helper()
		problems, err := checkFidelityRunnerUses(files, runner)
		require.NoError(t, err)
		return problems
	}

	assert.Empty(t, check(map[string]string{runner: goodRunner, "comment_test.go": commentOnly}),
		"the runner file with its call passes, and a comment or a string is not a mention")

	problems := check(map[string]string{runner: goodRunner, "direct_test.go": direct})
	require.Len(t, problems, 2)
	assert.Contains(t, problems[0]+problems[1], "direct_test.go:6:")
	assert.Contains(t, problems[0]+problems[1], "direct_test.go:7:")

	problems = check(map[string]string{runner: goodRunner, "other_test.go": otherCall})
	require.Len(t, problems, 1, "a call of fidelity.RunProbeOrSkip in another file gives one problem")
	assert.Contains(t, problems[0], "other_test.go:6:")

	problems = check(map[string]string{runner: goodRunner, "aliased_test.go": aliased})
	assert.Len(t, problems, 2, "the aliased import and the call through it")

	problems = check(map[string]string{runner: goodRunner, "dotted_test.go": dotted})
	assert.Len(t, problems, 2, "the dot import and the call through it")

	problems = check(map[string]string{runner: runnerRunsDirect})
	require.Len(t, problems, 1)
	assert.Contains(t, problems[0], "RunProbe in "+runner+"; only RunProbeOrSkip may be named there")

	problems = check(map[string]string{runner: runnerNeverCalls})
	assert.Equal(t, []string{runner + " never calls fidelity.RunProbeOrSkip"}, problems)

	problems = check(map[string]string{"other_test.go": commentOnly})
	assert.Equal(t, []string{runner + " is missing"}, problems)
}
