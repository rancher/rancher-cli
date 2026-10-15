package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mainEntryFile is the binary's entry point.
const mainEntryFile = "cmd/rancher/main.go"

// urfaveImport is the import path of the command-line library.
const urfaveImport = "github.com/urfave/cli/v3"

// TestTheEntryPointRunsTheCommandThroughCliRun asserts that the command tree
// cli.BuildRoot builds is run through cli.Run, which refuses a command line
// before urfave parses it, and not through the tree's own Run: no non-test
// Go file outside internal/cli imports urfave, so none can name the type
// of the tree; the entry point calls cli.Run and holds no other use of a
// method named Run, a call or a method value; and no other non-test file
// outside internal/cli calls Run on a variable that holds a tree
// cli.BuildRoot returned.
func TestTheEntryPointRunsTheCommandThroughCliRun(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	fset := token.NewFileSet()
	var offenders []string
	callsCliRun := false
	for _, path := range files {
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.HasPrefix(path, "internal/cli/") || inTestdata(path) {
			continue
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, perr, path)
		for _, imp := range file.Imports {
			if p, uerr := strconv.Unquote(imp.Path.Value); uerr == nil && p == urfaveImport {
				offenders = append(offenders, fset.Position(imp.Pos()).String()+": imports urfave")
			}
		}
		built := map[string]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.AssignStmt:
				if len(v.Rhs) == 1 && isBuildRootCall(v.Rhs[0]) {
					if id, ok := v.Lhs[0].(*ast.Ident); ok {
						built[id.Name] = true
					}
				}
			case *ast.ValueSpec:
				if len(v.Values) == 1 && isBuildRootCall(v.Values[0]) {
					built[v.Names[0].Name] = true
				}
			}
			return true
		})
		cliRuns := map[ast.Expr]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && isSelector(call.Fun, "cli", "Run") {
				cliRuns[call.Fun] = true
				callsCliRun = callsCliRun || path == mainEntryFile
			}
			return true
		})
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Run" || cliRuns[sel] {
				return true
			}
			// In the entry point any other use of a method named Run, a call
			// or a method value, runs the tree another way.
			id, named := sel.X.(*ast.Ident)
			if path == mainEntryFile || (named && built[id.Name]) {
				offenders = append(offenders, fset.Position(sel.Pos()).String()+": a use of Run other than a call of cli.Run")
			}
			return true
		})
	}
	assert.Empty(t, offenders, "a command tree from cli.BuildRoot runs through cli.Run")
	assert.True(t, callsCliRun, "%s calls cli.Run", mainEntryFile)
}

// isBuildRootCall reports whether expr is a call of cli.BuildRoot.
func isBuildRootCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	return ok && isSelector(call.Fun, "cli", "BuildRoot")
}

// isSelector reports whether expr is the selector pkg.name.
func isSelector(expr ast.Expr, pkg, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}
