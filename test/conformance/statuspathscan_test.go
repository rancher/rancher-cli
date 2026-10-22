package conformance

import (
	"fmt"
	"go/ast"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// statusStateAllowed is every place that may use the path status.state of
// an object: the file, the enclosing function and why. A method is named
// "(T).name" or "(*T).name"; a function with no receiver, "name"; a
// package-level declaration, "".
var statusStateAllowed = []struct{ file, fn, why string }{
	{"internal/engine/resources/wait.go", "resolvedState", "reads status.state, then status.phase, of an object served by the Kubernetes proxy; wait does not read steve's metadata.state; a state= wait on a kind whose object has no status.state finds nothing"},
}

// statusStateReads returns one message per use in f of the path status.state
// outside the allowed functions. A use is a composite literal of any type
// holding the string literals "status", "state" next to each other (a
// []string, a [][]string with an inner literal, a column's Path); a call,
// whatever its name, with those two literals as adjacent arguments, unless
// its name starts with Set or Remove; and a string literal equal to
// "status.state" or ".status.state" or containing ".status.state" (a
// JSONPath). Each use is reported once, at its own position. used records
// the allowlist entries that matched.
func statusStateReads(fset *token.FileSet, filePath string, f *ast.File, used map[int]bool) []string {
	var out []string
	for _, d := range f.Decls {
		fn := ""
		if fd, ok := d.(*ast.FuncDecl); ok {
			fn = functionName(fd)
		}
		ast.Inspect(d, func(n ast.Node) bool {
			if !usesStatusState(n) {
				return true
			}
			for i, a := range statusStateAllowed {
				if a.file == filePath && a.fn == fn {
					used[i] = true
					return true
				}
			}
			out = append(out, fmt.Sprintf("%s: names the path status.state; steve serves an object's state in metadata.state; read it with steve.ObjectState", fset.Position(n.Pos())))
			return true
		})
	}
	return out
}

// functionName is "name" for a function and "(T).name" or "(*T).name" for a
// method.
func functionName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	recv := ast.Unparen(fd.Recv.List[0].Type)
	star := ""
	if s, ok := recv.(*ast.StarExpr); ok {
		star, recv = "*", ast.Unparen(s.X)
	}
	switch r := recv.(type) {
	case *ast.IndexExpr:
		recv = r.X
	case *ast.IndexListExpr:
		recv = r.X
	}
	return fmt.Sprintf("(%s%s).%s", star, receiverTypeName(recv), fd.Name.Name)
}

func receiverTypeName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return fmt.Sprintf("%T", e)
}

func usesStatusState(n ast.Node) bool {
	switch n := n.(type) {
	case *ast.CompositeLit:
		return hasStatusState(n.Elts)
	case *ast.CallExpr:
		name := ""
		switch fun := n.Fun.(type) {
		case *ast.SelectorExpr:
			name = fun.Sel.Name
		case *ast.Ident:
			name = fun.Name
		}
		return !strings.HasPrefix(name, "Set") && !strings.HasPrefix(name, "Remove") && hasStatusState(n.Args)
	case *ast.BasicLit:
		s := stringValue(n)
		return s == "status.state" || strings.Contains(s, ".status.state")
	}
	return false
}

func stringValue(e ast.Expr) string {
	lit, ok := ast.Unparen(e).(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	s, _ := strconv.Unquote(lit.Value)
	return s
}

// hasStatusState reports whether exprs hold the string literals "status",
// "state" next to each other.
func hasStatusState(exprs []ast.Expr) bool {
	for i := 1; i < len(exprs); i++ {
		if stringValue(exprs[i-1]) == "status" && stringValue(exprs[i]) == "state" {
			return true
		}
	}
	return false
}

// TestNoProductionCodeReadsStatusState asserts no production Go file uses
// the path status.state of an object outside the allowlist. Rancher's
// management resources carry no status.state; steve computes a state for
// every object it serves and serves it in metadata.state. The scan refuses
// the literals "status", "state" next to each other in a composite literal
// or as call arguments, and a string literal naming the dotted path. A call
// named Set* or Remove* with the literals as separate arguments is a write
// and is accepted; a write through a slice literal is refused like any
// other use of the path.
//
// What it cannot see: a path built in a variable (a slice assembled with
// append, keys held in variables or constants, a helper called with them),
// map indexing such as obj.Object["status"].(map[string]any)["state"], typed
// decoding of a status struct, prose that has status.state without the
// leading dot, a concatenation or Sprintf of the parts, a struct tag, and
// text in embedded or non-Go files. A snippet row records each Go spelling
// of these as accepted, so a later widening shows in the table; embedded and
// non-Go files cannot be a row. A literal that must hold
// ".status.state" gets an allowlist entry by file and function.
func TestNoProductionCodeReadsStatusState(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	used := map[int]bool{}
	var findings []string
	for i, f := range tree.files {
		findings = append(findings, statusStateReads(tree.fset, tree.paths[i], f, used)...)
	}
	slices.Sort(findings)
	for _, finding := range findings {
		t.Error(finding)
	}
	for i, a := range statusStateAllowed {
		if !used[i] {
			t.Errorf("allowlist entry %s %s matched nothing: remove it", a.file, a.fn)
		}
	}
}

// TestStatusStateScanFixtures asserts the scan on source snippets, each with
// the number of findings it must give: every spelling of the two path
// elements it refuses (call arguments of any callee, a []string, a [][]string
// with an inner literal, a column's Path, a write through a slice, a dotted
// string, a JSONPath), the allowed function and its lookalikes (a method of
// the same name, the name in another file or another function of the
// file), the accepted paths and writes, and the spellings it cannot see. A
// looser rule fails its row.
func TestStatusStateScanFixtures(t *testing.T) {
	t.Parallel()
	const (
		x    = "internal/engine/x/x.go"
		wait = "internal/engine/resources/wait.go"
		pkgP = "package p; "
		inFn = pkgP + "func f() { "
		end  = " }"
	)
	for _, tc := range []struct {
		name, filePath, src string
		want                int
	}{
		{"NestedString", x, inFn + `_, _, _ = unstructured.NestedString(o, "status", "state")` + end, 1},
		{"NestedFieldNoCopy", x, inFn + `_, _, _ = unstructured.NestedFieldNoCopy(o, "status", "state")` + end, 1},
		{"NestedMap with a prefix", x, inFn + `_, _, _ = unstructured.NestedMap(o, "a", "status", "state")` + end, 1},
		{"a bare reader", x, inFn + `_, _, _ = NestedString(o, "status", "state")` + end, 1},
		{"a helper of another name", x, inFn + `_ = str(o, "status", "state")` + end, 1},
		{"a slice literal given to a reader", x, inFn + `_, _, _ = unstructured.NestedString(o, []string{"status", "state"}...)` + end, 1},
		{"a kinds column", x, pkgP + `var c = Column{Header: "STATE", Path: []string{"status", "state"}}`, 1},
		{"an alternate path with an elided literal", x, pkgP + `var c = Column{AltPaths: [][]string{{"status", "state"}}}`, 1},
		{"a package variable", x, pkgP + `var s, _, _ = unstructured.NestedString(o, "status", "state")`, 1},
		{"inside a closure", x, inFn + `g := func() { _, _, _ = unstructured.NestedString(o, "status", "state") }; g()` + end, 1},
		{"a write through a slice", x, inFn + `_ = unstructured.SetNestedField(o, v, []string{"status", "state"}...)` + end, 1},
		{"a dotted string", x, inFn + `_ = strings.Split("status.state", ".")` + end, 1},
		{"a dotted string given to a reader", x, inFn + `_, _, _ = unstructured.NestedString(o, "status.state")` + end, 1},
		{"a leading dot", x, inFn + `_ = ".status.state"` + end, 1},
		{"a JSONPath", x, inFn + "_ = \"{.status.state}\"" + end, 1},
		{"a custom column", x, inFn + `_ = "STATE:.status.state"` + end, 1},
		{"the allowed function", wait, pkgP + `func resolvedState() { _, _, _ = unstructured.NestedString(o, "status", "state") }`, 0},
		{"the allowed file, another function", wait, pkgP + `func other() { _, _, _ = unstructured.NestedString(o, "status", "state") }`, 1},
		{"the allowed function name in another file", x, pkgP + `func resolvedState() { _, _, _ = unstructured.NestedString(o, "status", "state") }`, 1},
		{"a method of the allowed name", wait, pkgP + `func (w *w) resolvedState() { _, _, _ = unstructured.NestedString(o, "status", "state") }`, 1},
		{"a value method of the allowed name", wait, pkgP + `func (w w) resolvedState() { _, _, _ = unstructured.NestedString(o, "status", "state") }`, 1},
		{"a write", x, inFn + `_ = unstructured.SetNestedField(o, v, "status", "state")` + end, 0},
		{"a removal", x, inFn + `unstructured.RemoveNestedField(o, "status", "state")` + end, 0},
		{"metadata.state", x, inFn + `_, _, _ = unstructured.NestedString(o, "metadata", "state", "name")` + end, 0},
		{"status.conditions", x, inFn + `_, _, _ = unstructured.NestedSlice(o, "status", "conditions")` + end, 0},
		{"the other order", x, inFn + `_, _, _ = unstructured.NestedString(o, "state", "status")` + end, 0},
		{"the literals apart", x, inFn + `_, _, _ = unstructured.NestedString(o, "status", "x", "state")` + end, 0},
		{"a map literal with the keys", x, inFn + `_ = map[string]any{"status": 1, "state": 2}` + end, 0},
		{"a slice built in a variable", x, inFn + `p := []string{"status"}; p = append(p, "state"); _, _, _ = unstructured.NestedString(o, p...)` + end, 0},
		{"keys in variables", x, inFn + `a, b := "status", "state"; _ = str(o, a, b)` + end, 0},
		{"a constant key", x, pkgP + `const k = "state"; func f() { _, _, _ = unstructured.NestedString(o, "status", k) }`, 0},
		{"map indexing", x, inFn + `_ = o.Object["status"].(map[string]any)["state"]` + end, 0},
		{"typed decoding", x, inFn + `_ = c.Status.State` + end, 0},
		{"prose with the path", x, inFn + `_ = "matches status.state exactly"` + end, 0},
		{"a concatenation", x, inFn + `_ = "status." + "state"` + end, 0},
		{"a Sprintf", x, inFn + `_ = fmt.Sprintf("status.%s", "state")` + end, 0},
		{"a struct tag", x, pkgP + "type s struct { State string `json:\"status.state\"` }", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fset, f := parseSnippet(t, tc.filePath, tc.src)
			got := statusStateReads(fset, tc.filePath, f, map[int]bool{})
			assert.Len(t, got, tc.want, strings.Join(got, "\n"))
		})
	}
}
