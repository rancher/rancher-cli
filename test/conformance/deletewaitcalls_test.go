package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deleteWaitFile is the file whose PlanFromFetched sends the delete every
// delete command uses and waits after it.
const deleteWaitFile = "internal/engine/resources/delete.go"

// deleteCallsWithoutWait are the functions under internal/engine that send
// a delete that is not a dry run outside PlanFromFetched, by file and
// enclosing function, each with the reason it does not wait.
var deleteCallsWithoutWait = []struct {
	file, function, reason string
}{
	{"internal/engine/rbac/revoke.go", "revokeCmd.Plan", "revoke is not a delete command; it deletes role bindings"},
	{"internal/engine/resources/deleteproject.go", "deleteProjectCmd.execDeleteNamespaces", "--delete-namespaces does not wait"},
	{"internal/engine/resources/deleteproject.go", "deleteProjectCmd.deleteProject", "--delete-namespaces does not wait"},
}

// funcDeclName is the name of a function declaration: "Type.Method" for a
// method, the name alone for a function.
func funcDeclName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	typ := fd.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

// TestEveryDeleteUnderTheEngineWaitsOrIsListed asserts that every call of a
// method named Delete with six arguments in a non-test file under
// internal/engine whose last argument (dryRun) is not the literal true is
// inside PlanFromFetched, which waits after the delete, or is listed in
// deleteCallsWithoutWait with its reason; and that every listed function
// still holds such a call.
func TestEveryDeleteUnderTheEngineWaitsOrIsListed(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	fset := token.NewFileSet()
	var offenders []string
	seen := map[string]bool{}
	for _, path := range files {
		if !strings.HasPrefix(path, "internal/engine/") || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, perr, path)
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			name := funcDeclName(fd)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 6 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Delete" {
					return true
				}
				if last, ok := call.Args[5].(*ast.Ident); ok && last.Name == "true" {
					return true
				}
				key := path + " " + name
				seen[key] = true
				if path == deleteWaitFile && name == "PlanFromFetched" {
					return true
				}
				for _, l := range deleteCallsWithoutWait {
					if l.file == path && l.function == name {
						return true
					}
				}
				offenders = append(offenders, fmt.Sprintf("%s: %s", fset.Position(call.Pos()), name))
				return true
			})
		}
	}
	assert.Empty(t, offenders, "a delete that is not a dry run is sent from PlanFromFetched, which waits after it, or is listed with its reason")
	assert.True(t, seen[deleteWaitFile+" PlanFromFetched"], "PlanFromFetched sends the delete")
	for _, l := range deleteCallsWithoutWait {
		assert.True(t, seen[l.file+" "+l.function], "%s %s is listed and sends no delete", l.file, l.function)
	}
}
