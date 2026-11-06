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

// A probe's teardown steps delete or restore objects on the shared oracle, so
// they must still send their requests when the caller's context has ended.
// Every probe's teardown runs through runTeardown in
// internal/testrancher/fidelity/executor.go, which builds each step's context
// from context.WithoutCancel; a function that read Probe.Teardown anywhere else
// would run those steps on whatever context it was given. A cleanup a test
// registers with Cleanup runs after the test's own context is cancelled. Its
// function literal must not read that context.

const (
	fidelitySourceDir    = "internal/testrancher/fidelity/"
	fidelityExecutorFile = fidelitySourceDir + "executor.go"
	fidelityTeardownFunc = "runTeardown"
)

// contextDerivers are the context package functions that derive a context
// with a cancellation or a deadline.
var contextDerivers = []string{"WithCancel", "WithCancelCause", "WithDeadline", "WithDeadlineCause", "WithTimeout", "WithTimeoutCause"}

// contextCallName returns the name of the function expr calls when expr is a
// call of a function of a package named context, and "" otherwise.
func contextCallName(expr ast.Expr) string {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return ""
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "context" {
		return ""
	}
	return sel.Sel.Name
}

// declaresIdent reports whether lit declares an identifier called name: as a
// parameter, a := or range definition, or a var. It does not track scopes.
func declaresIdent(lit *ast.FuncLit, name string) bool {
	declared := false
	has := func(idents ...ast.Expr) {
		for _, e := range idents {
			if id, ok := e.(*ast.Ident); ok && id.Name == name {
				declared = true
			}
		}
	}
	ast.Inspect(lit, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncType:
			for _, field := range n.Params.List {
				for _, id := range field.Names {
					has(id)
				}
			}
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE {
				has(n.Lhs...)
			}
		case *ast.RangeStmt:
			if n.Tok == token.DEFINE {
				has(n.Key, n.Value)
			}
		case *ast.ValueSpec:
			for _, id := range n.Names {
				has(id)
			}
		}
		return true
	})
	return declared
}

// checkCleanupLiteral reports every call of a method named Context in lit and
// every use of the identifier ctx that lit does not declare itself. A selector
// field and the key of a composite literal are not uses.
func checkCleanupLiteral(lit *ast.FuncLit, report func(token.Pos, string)) {
	skip := map[*ast.Ident]bool{}
	ast.Inspect(lit, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			skip[n.Sel] = true
		case *ast.KeyValueExpr:
			if id, ok := n.Key.(*ast.Ident); ok {
				skip[id] = true
			}
		}
		return true
	})
	ownCtx := declaresIdent(lit, "ctx")
	ast.Inspect(lit, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Context" {
				report(n.Pos(), "a Cleanup function calls Context; the test's context is cancelled before a cleanup runs. Build the context from context.Background() with its own timeout")
			}
		case *ast.Ident:
			if n.Name == "ctx" && !skip[n] && !ownCtx {
				report(n.Pos(), "a Cleanup function uses a ctx declared outside it; the test's context is cancelled before a cleanup runs. Build the context from context.Background() with its own timeout")
			}
		}
		return true
	})
}

// checkFidelityTeardownContexts parses every source in sources (slash path to
// content) and returns one problem for each violation:
//
//   - in a non-test file of internal/testrancher/fidelity, a selector
//     .Teardown outside the one function runTeardown of executor.go; zero or
//     two functions named runTeardown; a runTeardown with no context.WithTimeout
//     call, or with a call of a context function that derives a cancellation
//     or deadline whose first argument is not a call of context.WithoutCancel;
//   - a selector .Teardown in a file of test/e2e/fidelity;
//   - in any file of both directories, a function literal passed to a Cleanup
//     call that calls a method named Context or reads an identifier ctx it does
//     not declare.
//
// The Cleanup check reads the literal's own body only. It does not see a helper
// the literal calls that reads the test's context, a context held in a variable
// not named ctx, a nested function that declares its own ctx while the literal
// uses an outer ctx elsewhere, or a function value passed to Cleanup. It does
// not check that runTeardown passes the context it derives to the step; the
// behavioral tests of the fidelity package cover that.
func checkFidelityTeardownContexts(sources map[string]string) ([]string, error) {
	var problems []string
	fset := token.NewFileSet()
	report := func(pos token.Pos, msg string) {
		problems = append(problems, fset.Position(pos).String()+": "+msg)
	}
	var runTeardowns []*ast.FuncDecl
	for _, path := range slices.Sorted(maps.Keys(sources)) {
		f, err := parser.ParseFile(fset, path, sources[path], parser.AllErrors)
		if err != nil {
			return nil, err
		}
		production := strings.HasPrefix(path, fidelitySourceDir) && !strings.HasSuffix(path, "_test.go")
		runner := strings.HasPrefix(path, fidelityRunnerDir)
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && production && fn.Recv == nil && fn.Name.Name == fidelityTeardownFunc {
				runTeardowns = append(runTeardowns, fn)
				if path != fidelityExecutorFile {
					report(fn.Pos(), fidelityTeardownFunc+" is declared outside "+fidelityExecutorFile+"; one function in that file runs every probe's teardown")
				}
				continue
			}
			if !production && !runner {
				continue
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Teardown" {
					report(sel.Pos(), "Teardown is read outside "+fidelityTeardownFunc+" in "+fidelityExecutorFile+"; run a probe's teardown steps only there; each step must keep running when the caller's context ends")
				}
				return true
			})
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "Cleanup" {
				return true
			}
			for _, arg := range call.Args {
				if lit, ok := arg.(*ast.FuncLit); ok {
					checkCleanupLiteral(lit, report)
				}
			}
			return true
		})
	}

	inExecutor := 0
	for _, fn := range runTeardowns {
		if fset.Position(fn.Pos()).Filename != fidelityExecutorFile {
			continue
		}
		inExecutor++
		timeouts := 0
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := contextCallName(call)
			if !slices.Contains(contextDerivers, name) {
				return true
			}
			if name == "WithTimeout" {
				timeouts++
			}
			if len(call.Args) == 0 || contextCallName(call.Args[0]) != "WithoutCancel" {
				report(call.Pos(), "context."+name+" in "+fidelityTeardownFunc+" takes a context that is not context.WithoutCancel(...); a step must not end when the caller's context does")
			}
			return true
		})
		if timeouts == 0 {
			report(fn.Pos(), fidelityTeardownFunc+" has no call of context.WithTimeout; every teardown step needs its own time limit")
		}
	}
	if inExecutor != 1 {
		problems = append(problems, fidelityExecutorFile+": want one function named "+fidelityTeardownFunc+", found "+strconv.Itoa(inExecutor))
	}
	return problems, nil
}

// fidelityTeardownSources returns the content of every Go file under the
// fidelity package and its e2e runners, from the list of repository files, as
// a map from each file's slash-separated path to its content.
func fidelityTeardownSources(t *testing.T) map[string]string {
	t.Helper()
	files, err := repositoryFiles()
	require.NoError(t, err)
	sources := map[string]string{}
	for _, path := range files {
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		if !strings.HasPrefix(path, fidelitySourceDir) && !strings.HasPrefix(path, fidelityRunnerDir) {
			continue
		}
		src, err := os.ReadFile(filepath.FromSlash(path))
		require.NoError(t, err)
		sources[path] = string(src)
	}
	return sources
}

// TestFidelityTeardownContextsAreNotTheCallers asserts that a probe's teardown
// steps run only in runTeardown of executor.go, which builds each step's
// context from context.WithoutCancel with its own time limit, that no file of
// test/e2e/fidelity reads a probe's teardown, and that no function literal
// passed to Cleanup in the fidelity package or its e2e runners calls Context or
// reads an outer ctx. The Cleanup check reads the literal's own body only. It
// does not see a helper the literal calls that reads the test's context, a
// context held in a variable not named ctx, a nested function that declares its
// own ctx while the literal uses an outer ctx elsewhere, or a function value
// passed to Cleanup. It does not check that runTeardown passes the context it
// derives to the step; the behavioral tests of the fidelity package cover that.
// No file is exempt.
func TestFidelityTeardownContextsAreNotTheCallers(t *testing.T) {
	t.Parallel()
	sources := fidelityTeardownSources(t)
	require.Contains(t, sources, fidelityExecutorFile)
	var inPackage, inRunners int
	for path := range sources {
		if strings.HasPrefix(path, fidelitySourceDir) {
			inPackage++
		} else {
			inRunners++
		}
	}
	t.Logf("files read: %d under %s, %d under %s", inPackage, fidelitySourceDir, inRunners, fidelityRunnerDir)
	assert.Greater(t, inPackage, 1)
	assert.Greater(t, inRunners, 1)
	problems, err := checkFidelityTeardownContexts(sources)
	require.NoError(t, err)
	for _, problem := range problems {
		t.Error(problem)
	}
}

// TestFidelityTeardownScanFlagsEveryViolation asserts the scan reports a second
// reader of Teardown in another file and in another function of executor.go, a
// runTeardown that passes ctx to WithTimeout, one with no WithTimeout, one with
// a second WithTimeout that takes ctx, a missing runTeardown, two of them, a
// Teardown selector in an e2e runner file, and a Cleanup literal that calls
// Context or reads an outer ctx; and that it accepts a test file that reads
// Teardown, a Cleanup literal that builds context.Background() or declares its
// own ctx, a selector field or composite literal key named ctx, and a mention
// in a comment or a string.
func TestFidelityTeardownScanFlagsEveryViolation(t *testing.T) {
	t.Parallel()
	const goodExecutor = `package fidelity

import (
	"context"
	"time"
)

type probe struct{ Teardown []func(context.Context) }

func runTeardown(ctx context.Context, p probe, timeout time.Duration) {
	for _, s := range p.Teardown {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		s(sctx)
		cancel()
	}
}
`
	const passesCtx = `package fidelity

import (
	"context"
	"time"
)

type probe struct{ Teardown []func(context.Context) }

func runTeardown(ctx context.Context, p probe, timeout time.Duration) {
	for _, s := range p.Teardown {
		sctx, cancel := context.WithTimeout(ctx, timeout)
		s(sctx)
		cancel()
	}
}
`
	const noTimeout = `package fidelity

import "context"

type probe struct{ Teardown []func(context.Context) }

func runTeardown(ctx context.Context, p probe) {
	for _, s := range p.Teardown {
		s(context.WithoutCancel(ctx))
	}
}
`
	const twoTimeouts = `package fidelity

import (
	"context"
	"time"
)

type probe struct{ Teardown []func(context.Context) }

func runTeardown(ctx context.Context, p probe, timeout time.Duration) {
	for _, s := range p.Teardown {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		s(sctx)
		cancel()
		sctx, cancel = context.WithTimeout(ctx, timeout)
		s(sctx)
		cancel()
	}
}
`
	const noRunTeardown = `package fidelity

func other() {}
`
	const secondRunTeardown = `package fidelity

import (
	"context"
	"time"
)

func runTeardown(ctx context.Context, timeout time.Duration) {
	_, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	cancel()
}
`
	const otherReader = `package fidelity

func other(p probe) {
	for range p.Teardown {
	}
}
`
	const executorOtherReader = goodExecutor + `
func another(p probe) int { return len(p.Teardown) }
`
	const testReader = `package fidelity

import "testing"

func TestReads(t *testing.T) {
	var p probe
	_ = p.Teardown
}
`
	const runnerReader = `package fidelity

func runIt(p probe) {
	_ = p.Teardown
}
`
	const cleanupContext = `package fidelity

import "testing"

func TestContext(t *testing.T) {
	t.Cleanup(func() {
		_ = t.Context()
	})
}
`
	const cleanupOuterCtx = `package fidelity

import (
	"context"
	"testing"
)

func TestOuter(t *testing.T) {
	ctx := t.Context()
	t.Cleanup(func() {
		use(ctx)
	})
}

func use(context.Context) {}
`
	const cleanupAccepted = `package fidelity

import (
	"context"
	"testing"
)

type holder struct{ ctx context.Context }

func TestAccepted(t *testing.T) {
	// t.Cleanup(func() { _ = t.Context() }) is only named here.
	_ = "t.Context()"
	h := holder{}
	t.Cleanup(func() {
		ctx := context.Background()
		use(ctx)
	})
	t.Cleanup(func() {
		_ = h.ctx
		_ = holder{ctx: nil}
	})
	t.Cleanup(func() {
		func(ctx context.Context) { use(ctx) }(context.Background())
	})
	t.Cleanup(func() {
		var ctx = context.Background()
		use(ctx)
	})
}

func use(context.Context) {}
`
	pkg := func(name string) string { return fidelitySourceDir + name }
	runner := func(name string) string { return fidelityRunnerDir + name }
	check := func(files map[string]string) []string {
		t.Helper()
		problems, err := checkFidelityTeardownContexts(files)
		require.NoError(t, err)
		return problems
	}
	base := func(extra map[string]string) map[string]string {
		files := map[string]string{fidelityExecutorFile: goodExecutor}
		maps.Copy(files, extra)
		return files
	}

	assert.Empty(t, check(base(map[string]string{
		pkg("reads_test.go"):      testReader,
		pkg("cleanup_test.go"):    cleanupAccepted,
		runner("cleanup_test.go"): cleanupAccepted,
	})), "a test file may read Teardown, and the accepted Cleanup literals pass")

	problems := check(base(map[string]string{pkg("other.go"): otherReader}))
	require.Len(t, problems, 1, "a second reader in another file")
	assert.Contains(t, problems[0], "other.go:4:")

	problems = check(map[string]string{fidelityExecutorFile: executorOtherReader})
	require.Len(t, problems, 1, "a second reader in another function of executor.go")
	assert.Contains(t, problems[0], "executor.go:")

	problems = check(map[string]string{fidelityExecutorFile: passesCtx})
	require.Len(t, problems, 1, "runTeardown passes ctx to WithTimeout")
	assert.Contains(t, problems[0], "context.WithTimeout in runTeardown takes a context that is not context.WithoutCancel")

	problems = check(map[string]string{fidelityExecutorFile: noTimeout})
	require.Len(t, problems, 1, "runTeardown with no WithTimeout")
	assert.Contains(t, problems[0], "has no call of context.WithTimeout")

	problems = check(map[string]string{fidelityExecutorFile: twoTimeouts})
	require.Len(t, problems, 1, "two WithTimeout calls, one taking ctx")
	assert.Contains(t, problems[0], "takes a context that is not context.WithoutCancel")

	problems = check(map[string]string{fidelityExecutorFile: noRunTeardown})
	assert.Equal(t, []string{fidelityExecutorFile + ": want one function named runTeardown, found 0"}, problems)

	problems = check(base(map[string]string{pkg("second.go"): secondRunTeardown}))
	require.Len(t, problems, 1, "a second runTeardown")
	assert.Contains(t, problems[0], "runTeardown is declared outside "+fidelityExecutorFile)

	problems = check(base(map[string]string{runner("runner_test.go"): runnerReader}))
	require.Len(t, problems, 1, "Teardown read in an e2e runner file")
	assert.Contains(t, problems[0], "runner_test.go:4:")

	problems = check(base(map[string]string{pkg("context_test.go"): cleanupContext}))
	require.Len(t, problems, 1, "a Cleanup literal calls t.Context()")
	assert.Contains(t, problems[0], "context_test.go:7:")
	assert.Contains(t, problems[0], "calls Context")

	problems = check(base(map[string]string{runner("outer_test.go"): cleanupOuterCtx}))
	require.Len(t, problems, 1, "a Cleanup literal reads an outer ctx")
	assert.Contains(t, problems[0], "outer_test.go:11:")
	assert.Contains(t, problems[0], "uses a ctx declared outside it")
}
