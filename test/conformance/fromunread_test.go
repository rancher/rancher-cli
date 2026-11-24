package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fromUnreadSetters are the functions of non-test Go files outside
// internal/runtime/safety that may set DiffEntry.FromUnread, by file and
// enclosing function, each with the reason. An entry that sets it leaves
// `from` out of a preview's diff, so each setter needs a review.
var fromUnreadSetters = []struct {
	file, function, reason string
}{
	{"internal/engine/provisioning/cloudcredwrite.go", "updateDocument", "the patch preview does not read stored config values"},
}

// fromUnreadHit is one place a file sets FromUnread: a composite literal key,
// the left side of an assignment, the key or value of a range clause that
// assigns it, or its address.
type fromUnreadHit struct {
	function, at string
}

// findFromUnreadSetters parses the Go source of path (read from the file when
// src is nil) and returns every composite literal with a key named
// FromUnread, every assignment to a selector named FromUnread, every range
// clause that assigns one, and every address of one (`&x.FromUnread`, in any
// expression), each with the function declaration it is in ("" at the package
// level). A read of the field is not a setter.
func findFromUnreadSetters(path string, src any) ([]fromUnreadHit, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	var hits []fromUnreadHit
	for _, decl := range file.Decls {
		function := ""
		if fd, ok := decl.(*ast.FuncDecl); ok {
			function = funcDeclName(fd)
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.KeyValueExpr:
				if key, ok := n.Key.(*ast.Ident); ok && key.Name == "FromUnread" {
					hits = append(hits, fromUnreadHit{function, fset.Position(key.Pos()).String()})
				}
			case *ast.AssignStmt:
				for _, left := range n.Lhs {
					if sel, ok := left.(*ast.SelectorExpr); ok && sel.Sel.Name == "FromUnread" {
						hits = append(hits, fromUnreadHit{function, fset.Position(sel.Sel.Pos()).String()})
					}
				}
			case *ast.RangeStmt:
				for _, side := range []ast.Expr{n.Key, n.Value} {
					if sel, ok := side.(*ast.SelectorExpr); ok && sel.Sel.Name == "FromUnread" {
						hits = append(hits, fromUnreadHit{function, fset.Position(sel.Sel.Pos()).String()})
					}
				}
			case *ast.UnaryExpr:
				if sel, ok := n.X.(*ast.SelectorExpr); ok && n.Op == token.AND && sel.Sel.Name == "FromUnread" {
					hits = append(hits, fromUnreadHit{function, fset.Position(sel.Sel.Pos()).String()})
				}
			}
			return true
		})
	}
	return hits, nil
}

// TestOnlyTheListedFunctionsSetFromUnread asserts that a non-test Go file
// outside internal/runtime/safety and testdata sets DiffEntry.FromUnread only
// in a function fromUnreadSetters lists with its reason, and that every listed
// function still does.
func TestOnlyTheListedFunctionsSetFromUnread(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	var offenders []string
	seen := map[string]bool{}
	for _, path := range files {
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || inTestdata(path) || strings.HasPrefix(path, "internal/runtime/safety/") {
			continue
		}
		hits, perr := findFromUnreadSetters(path, nil)
		require.NoError(t, perr, path)
		for _, hit := range hits {
			key := path + " " + hit.function
			seen[key] = true
			listed := slices.ContainsFunc(fromUnreadSetters, func(l struct{ file, function, reason string }) bool {
				return l.file+" "+l.function == key
			})
			if !listed {
				offenders = append(offenders, fmt.Sprintf("%s: FromUnread set in %q", hit.at, hit.function))
			}
		}
	}
	assert.Empty(t, offenders, "only the functions fromUnreadSetters lists leave `from` out of a diff entry; a new one needs a review")
	for _, l := range fromUnreadSetters {
		assert.True(t, seen[l.file+" "+l.function], "%s %s is listed and no longer sets FromUnread; remove the entry", l.file, l.function)
	}
}

// TestTheFromUnreadScanFindsEverySetter asserts the rule of
// findFromUnreadSetters on small sources: a composite literal key, an
// assignment, a range clause that assigns the field and the address of the
// field are found, in a function or at the package level, and a read, the
// address of another field, a field declaration or another field name is not.
func TestTheFromUnreadScanFindsEverySetter(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, src string
		want      []string
	}{
		{"a literal key in a function", "package p\n\nfunc f() { _ = D{Path: \"x\", FromUnread: true} }\n", []string{"f"}},
		{"a literal key in a method", "package p\n\nfunc (c *T) g() { _ = []D{{FromUnread: true}} }\n", []string{"T.g"}},
		{"an assignment", "package p\n\nfunc f(d D) { d.FromUnread = true }\n", []string{"f"}},
		{"a literal key at the package level", "package p\n\nvar d = D{FromUnread: true}\n", []string{""}},
		{"the address in a definition", "package p\n\nfunc f(d D) { p := &d.FromUnread; *p = true }\n", []string{"f"}},
		{"the address as an argument", "package p\n\nfunc f(d D) { set(&d.FromUnread) }\n", []string{"f"}},
		{"a range clause value", "package p\n\nfunc f(d D, bs []bool) { for _, d.FromUnread = range bs {} }\n", []string{"f"}},
		{"a range clause key", "package p\n\nfunc f(d D, bs []bool) { for d.FromUnread = range bs {} }\n", []string{"f"}},
		{"the address of another field", "package p\n\nfunc f(d D) { set(&d.From) }\n", nil},
		{"a read", "package p\n\nfunc f(d D) bool { return d.FromUnread }\n", nil},
		{"a field declaration", "package p\n\ntype D struct{ FromUnread bool }\n", nil},
		{"another field", "package p\n\nfunc f() { _ = D{From: nil} }\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hits, err := findFromUnreadSetters("p.go", tc.src)
			require.NoError(t, err)
			var got []string
			for _, h := range hits {
				got = append(got, h.function)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
