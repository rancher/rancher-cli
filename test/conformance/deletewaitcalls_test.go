package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deleteCallsWithoutWait are the functions under internal/engine that send
// a delete that is not a dry run without waiting for what it deletes, by
// file and enclosing function, each with the reason it does not wait.
var deleteCallsWithoutWait = []struct {
	file, function, reason string
}{
	{"internal/engine/rbac/revoke.go", "revokeCmd.Plan", "revoke is not a delete command; it deletes role bindings"},
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

// deleteSend is one call of a method named Delete with six arguments whose
// last argument (dryRun) is not the literal true.
type deleteSend struct {
	// function is the declaration the call is in, as funcDeclName names it.
	function string
	// at is the position of the call.
	at string
	// waits is true when the same innermost function holds a call of
	// waitDeleted that follows the delete, binds its error result to a named
	// variable and waits for the object the delete names.
	waits bool
}

// functionCalls are the calls of one innermost function: the function
// declaration or function literal that holds them directly, not one nested
// in it.
type functionCalls struct {
	deletes []*ast.CallExpr
	waits   []waitCall
}

// waitCall is a call of waitDeleted and whether its error result is bound to
// a variable that is not the blank identifier.
type waitCall struct {
	call  *ast.CallExpr
	bound bool
}

// findDeleteSends parses the Go source of path (read from the file when src
// is nil) and returns every send of a delete that is not a dry run, each
// with whether it waits. A send waits when a call of waitDeleted in the same
// innermost function (the same function declaration or function literal, not
// one nested in it or one that holds it):
//   - comes after the call of Delete in source order;
//   - has its error result bound to a variable that is not the blank
//     identifier (a bare call or a discarded result does not count);
//   - has a targets argument (the third) that is not the literal nil and
//     names the name argument of the Delete (the fourth) as namesObject
//     says: the `name:` field of a literal, the name argument of
//     newDeleteTarget, or the index of the waits map (a wait for another
//     object does not count).
//
// The rule reads the source and has these limits. Whether the wait is reached
// is the behavior rows' job (internal/cli deletewait_conformance_test.go): a
// wait in a branch that never runs, such as an if false, or on another client
// is not detected. The scan sees only a method named Delete with six
// arguments in a non-test file under internal/engine: a call of another
// arity, a call through a method value, a raw HTTP DELETE request and any
// delete outside internal/engine are not seen (no DeleteCollection exists in
// the repository). The behavior rows cover the delete commands the registry
// builds and one row for delete project with --delete-namespaces, added by
// hand: a new flag on another delete command that changes how it sends the
// delete gets no row until someone adds one.
func findDeleteSends(path string, src any) ([]deleteSend, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	var sends []deleteSend
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		var stack []ast.Node
		var functions []*functionCalls
		var popped []*functionCalls
		ast.Inspect(fd, func(n ast.Node) bool {
			if n == nil {
				top := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				switch top.(type) {
				case *ast.FuncDecl, *ast.FuncLit:
					popped = append(popped, functions[len(functions)-1])
					functions = functions[:len(functions)-1]
				}
				return true
			}
			stack = append(stack, n)
			switch n.(type) {
			case *ast.FuncDecl, *ast.FuncLit:
				functions = append(functions, &functionCalls{})
			}
			call, ok := n.(*ast.CallExpr)
			if !ok || len(functions) == 0 {
				return true
			}
			current := functions[len(functions)-1]
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				if fun.Sel.Name != "Delete" || len(call.Args) != 6 {
					return true
				}
				if last, ok := call.Args[5].(*ast.Ident); ok && last.Name == "true" {
					return true
				}
				current.deletes = append(current.deletes, call)
			case *ast.Ident:
				if fun.Name == "waitDeleted" {
					current.waits = append(current.waits, waitCall{call: call, bound: errorBound(stack, call)})
				}
			}
			return true
		})
		for _, fn := range popped {
			for _, del := range fn.deletes {
				sends = append(sends, deleteSend{function: funcDeclName(fd), at: fset.Position(del.Pos()).String(), waits: waitsAfter(del, fn.waits)})
			}
		}
	}
	return sends, nil
}

// errorBound reports whether the call, the last node of stack, is the only
// value of an assignment or a declaration whose second result is a name other
// than _.
func errorBound(stack []ast.Node, call *ast.CallExpr) bool {
	if len(stack) < 2 {
		return false
	}
	var lhs []ast.Expr
	switch parent := stack[len(stack)-2].(type) {
	case *ast.AssignStmt:
		if len(parent.Rhs) != 1 || parent.Rhs[0] != call {
			return false
		}
		lhs = parent.Lhs
	case *ast.ValueSpec:
		if len(parent.Values) != 1 || parent.Values[0] != call {
			return false
		}
		for _, name := range parent.Names {
			lhs = append(lhs, name)
		}
	default:
		return false
	}
	if len(lhs) != 2 {
		return false
	}
	id, ok := lhs[1].(*ast.Ident)
	return ok && id.Name != "_"
}

// waitsAfter reports whether one of the waits satisfies the rule of
// findDeleteSends for del.
func waitsAfter(del *ast.CallExpr, waits []waitCall) bool {
	name := types.ExprString(del.Args[3])
	for _, w := range waits {
		if w.call.Pos() < del.End() || !w.bound || len(w.call.Args) < 3 {
			continue
		}
		targets := w.call.Args[2]
		if id, ok := targets.(*ast.Ident); ok && id.Name == "nil" {
			continue
		}
		if namesObject(targets, name) {
			return true
		}
	}
	return false
}

// namesObject reports whether the targets expression names the object called
// text, a Delete's name argument, in a place that says which object is
// waited for: the value of a `name:` field of a composite literal, the third
// argument of a call of newDeleteTarget(k, ns, name, obj, cluster), or the
// index of the waits map (waits[name]). The same text in any other place, such
// as the value of an `ns:` field, another argument, a call of another function
// or the index of another map, does not count.
func namesObject(root ast.Node, text string) bool {
	found := false
	ast.Inspect(root, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CompositeLit:
			for _, elt := range n.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "name" && types.ExprString(kv.Value) == text {
						found = true
					}
					found = found || namesObject(kv.Value, text)
					continue
				}
				found = found || namesObject(elt, text)
			}
			return false
		case *ast.CallExpr:
			if fun, ok := n.Fun.(*ast.Ident); ok && fun.Name == "newDeleteTarget" && len(n.Args) > 2 && types.ExprString(n.Args[2]) == text {
				found = true
			}
		case *ast.IndexExpr:
			if isWaitsMap(n.X) && types.ExprString(n.Index) == text {
				found = true
			}
		}
		return !found
	})
	return found
}

// isWaitsMap reports whether x is the map of waits by namespace name: waits or
// a field named waits.
func isWaitsMap(x ast.Expr) bool {
	switch x := x.(type) {
	case *ast.Ident:
		return x.Name == "waits"
	case *ast.SelectorExpr:
		return x.Sel.Name == "waits"
	}
	return false
}

// TestEveryDeleteUnderTheEngineWaitsOrIsListed asserts that every call of a
// method named Delete with six arguments in a non-test file under
// internal/engine whose last argument (dryRun) is not the literal true is in
// a function that waits for what it deleted, by the rule of findDeleteSends,
// or is listed in deleteCallsWithoutWait with its reason; and that every
// listed function still holds such a call.
func TestEveryDeleteUnderTheEngineWaitsOrIsListed(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	var offenders []string
	seen := map[string]bool{}
	for _, path := range files {
		if !strings.HasPrefix(path, "internal/engine/") || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			continue
		}
		sends, perr := findDeleteSends(path, nil)
		require.NoError(t, perr, path)
		for _, send := range sends {
			seen[path+" "+send.function] = true
			if send.waits {
				continue
			}
			listed := false
			for _, l := range deleteCallsWithoutWait {
				if l.file == path && l.function == send.function {
					listed = true
				}
			}
			if !listed {
				offenders = append(offenders, fmt.Sprintf("%s: %s", send.at, send.function))
			}
		}
	}
	assert.Empty(t, offenders, "a delete that is not a dry run is followed in its function by a wait for what it deleted, or is listed with its reason")
	assert.True(t, seen["internal/engine/resources/delete.go PlanFromFetched"], "PlanFromFetched sends the delete")
	assert.True(t, seen["internal/engine/resources/deleteproject.go deleteProjectCmd.execDeleteNamespaces"], "execDeleteNamespaces sends the namespace deletes")
	assert.True(t, seen["internal/engine/resources/deleteproject.go deleteProjectCmd.deleteProject"], "deleteProject sends the project delete")
	for _, l := range deleteCallsWithoutWait {
		assert.True(t, seen[l.file+" "+l.function], "%s %s is listed and sends no delete", l.file, l.function)
	}
}

// TestTheDeleteRuleFindsAWaitOnlyWhenItWaitsForTheDeletedObject asserts the
// rule of findDeleteSends on small sources: a delete is followed by a wait
// that comes after it, binds its error result and waits for the object the
// delete names, in the same function; a wait for another object, a wait with
// no targets, a wait in another function, a wait before the delete, a wait
// whose result is discarded and a bare call do not count.
func TestTheDeleteRuleFindsAWaitOnlyWhenItWaitsForTheDeletedObject(t *testing.T) {
	t.Parallel()
	const head = "package p\n\nfunc f(ctx context.Context, c client, ns, name, other string) error {\n"
	for _, tc := range []struct {
		name string
		body string
		want []bool
	}{
		{"a wait for the object", head + `
	if err := c.Delete(ctx, gvr, ns, name, "1", false); err != nil {
		return err
	}
	end, err := waitDeleted(ctx, c, []deleteTarget{newDeleteTarget(k, ns, name, obj)}, deadline, "5m")
	_ = end
	return err
}`, []bool{true}},
		{"a wait for the object bound with a declaration", head + `
	if err := c.Delete(ctx, gvr, ns, name, "1", false); err != nil {
		return err
	}
	var end, err = waitDeleted(ctx, c, []deleteTarget{{name: name}}, deadline, "5m")
	_ = end
	return err
}`, []bool{true}},
		{"a wait for another object", head + `
	_ = c.Delete(ctx, gvr, ns, name, "1", false)
	end, err := waitDeleted(ctx, c, []deleteTarget{newDeleteTarget(k, ns, other, obj)}, deadline, "5m")
	_ = end
	return err
}`, []bool{false}},
		{"the name in another position of the helper", head + `
	_ = c.Delete(ctx, gvr, ns, name, "1", false)
	end, err := waitDeleted(ctx, c, []deleteTarget{newDeleteTarget(k, name, other, obj)}, deadline, "5m")
	_ = end
	return err
}`, []bool{false}},
		{"the name in another field", head + `
	_ = c.Delete(ctx, gvr, ns, name, "1", false)
	end, err := waitDeleted(ctx, c, []deleteTarget{{ns: name, name: other}}, deadline, "5m")
	_ = end
	return err
}`, []bool{false}},
		{"a wait found by index", head + `
	_ = c.Delete(ctx, gvr, ns, name, "1", false)
	end, err := waitDeleted(ctx, c, []deleteTarget{waits[name]}, deadline, "5m")
	_ = end
	return err
}`, []bool{true}},
		{"a wait found by the name field", head + `
	_ = c.Delete(ctx, gvr, ns, name, "1", false)
	end, err := waitDeleted(ctx, c, []deleteTarget{{ns: ns, name: name}}, deadline, "5m")
	_ = end
	return err
}`, []bool{true}},
		{"the name as the third argument of another function", head + `
	_ = c.Delete(ctx, gvr, ns, name, "1", false)
	end, err := waitDeleted(ctx, c, []deleteTarget{pick(k, ns, name, obj)}, deadline, "5m")
	_ = end
	return err
}`, []bool{false}},
		{"the name as the index of another map", head + `
	_ = c.Delete(ctx, gvr, ns, name, "1", false)
	end, err := waitDeleted(ctx, c, []deleteTarget{others[name]}, deadline, "5m")
	_ = end
	return err
}`, []bool{false}},
		{"a wait with no targets", head + `
	_ = c.Delete(ctx, gvr, ns, name, "1", false)
	end, err := waitDeleted(ctx, c, nil, deadline, "5m")
	_ = end
	return err
}`, []bool{false}},
		{"the delete in a closure and the wait in the declaration", head + `
	run := func() error {
		return c.Delete(ctx, gvr, ns, name, "1", false)
	}
	_ = run
	end, err := waitDeleted(ctx, c, []deleteTarget{newDeleteTarget(k, ns, name, obj)}, deadline, "5m")
	_ = end
	return err
}`, []bool{false}},
		{"the delete in the declaration and the wait in a closure", head + `
	_ = c.Delete(ctx, gvr, ns, name, "1", false)
	return func() error {
		end, err := waitDeleted(ctx, c, []deleteTarget{newDeleteTarget(k, ns, name, obj)}, deadline, "5m")
		_ = end
		return err
	}()
}`, []bool{false}},
		{"the wait before the delete", head + `
	end, err := waitDeleted(ctx, c, []deleteTarget{newDeleteTarget(k, ns, name, obj)}, deadline, "5m")
	_ = end
	_ = c.Delete(ctx, gvr, ns, name, "1", false)
	return err
}`, []bool{false}},
		{"the result discarded", head + `
	_ = c.Delete(ctx, gvr, ns, name, "1", false)
	_, _ = waitDeleted(ctx, c, []deleteTarget{newDeleteTarget(k, ns, name, obj)}, deadline, "5m")
	return nil
}`, []bool{false}},
		{"a bare call", head + `
	_ = c.Delete(ctx, gvr, ns, name, "1", false)
	waitDeleted(ctx, c, []deleteTarget{newDeleteTarget(k, ns, name, obj)}, deadline, "5m")
	return nil
}`, []bool{false}},
		{"a dry run and a call with other arguments", head + `
	_ = c.Delete(ctx, gvr, ns, name, "1", true)
	_ = c.Delete(ctx, name)
	_ = c.Remove(ctx, gvr, ns, name, "1", false)
	return nil
}`, nil},
		{"two deletes and one wait", head + `
	_ = c.Delete(ctx, gvr, ns, name, "1", false)
	_ = c.Delete(ctx, gvr, ns, other, "1", false)
	end, err := waitDeleted(ctx, c, []deleteTarget{newDeleteTarget(k, ns, name, obj)}, deadline, "5m")
	_ = end
	return err
}`, []bool{true, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sends, err := findDeleteSends("snippet.go", tc.body)
			require.NoError(t, err)
			var got []bool
			for _, s := range sends {
				got = append(got, s.waits)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
