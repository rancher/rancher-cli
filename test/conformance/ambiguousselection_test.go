package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An AMBIGUOUS_SELECTION error tells the caller that several values matched
// and none was chosen; the values themselves are what the caller needs next.
// The scan below requires every use of clierr.CodeAmbiguousSelection outside
// internal/clierr to be one of:
//   - argument 0 of a clierr.New call whose builder chain (the calls
//     chained on its result) contains WithCandidates;
//   - an operand of == or !=, or an expression of a switch case, a
//     comparison of a code, not an error built from it;
//   - a use inside a declaration listed in ambiguousSelectionAllowlist.
//
// A use is keyed by the declaration that holds it, in the form declaredKeys
// records: "<path>:<name>" of a function (a method by its name alone), or of
// each name of the var or const spec that holds it. A use inside a spec with
// several names counts under each name, is reported once, and is exempt when
// any of its names is listed.
//
// The string literal "AMBIGUOUS_SELECTION", interpreted or raw, can only
// spell the code, so the scan refuses it outside internal/clierr whatever
// the allowlist says; spell the code with clierr.CodeAmbiguousSelection.
//
// Known limits: candidates attached in a later statement fail the scan
// (write the error as one chain); WithCandidates(nil) passes; a code held in
// a variable or a &clierr.Error literal is flagged, since the scan cannot
// follow the value; two methods with the same name in one file share one
// key, as in declaredKeys.

// ambiguousSelectionAllowlist names the declarations, as "<file>:<name>",
// that use the code without attaching candidates to the error built from it;
// each value says why.
var ambiguousSelectionAllowlist = map[string]string{
	"internal/engine/inspect/cluster.go:correlateProvisioningCluster": "the error is kept only as a degraded entry's code",
	"internal/token/noterminal.go:withNoTerminalRemedies":             "the candidates are passed to noTerminalError, limited to the auth configs that sign in without a terminal",
	"internal/commands/auth/login.go:promptForToken":                  "a terminal menu answer; the two choices are printed just above the prompt",
}

// ambiguousSelectionScan is the result of scanning one file.
type ambiguousSelectionScan struct {
	// uses counts every use of the code, by "<path>:<name>".
	uses map[string]int
	// flagged counts the uses that break the rule, by "<path>:<name>",
	// whether or not the declaration is allowlisted.
	flagged map[string]int
	// offenders describes each flagged use in a declaration not
	// allowlisted, and each string literal that spells the code.
	offenders []string
}

// scanAmbiguousSelection applies the rule to f, at path, with the allowlist
// given. A file inside internal/clierr defines the code and is skipped.
func scanAmbiguousSelection(fset *token.FileSet, path string, f *ast.File, allow map[string]string) ambiguousSelectionScan {
	res := ambiguousSelectionScan{uses: map[string]int{}, flagged: map[string]int{}}
	if strings.HasPrefix(path, "internal/clierr/") {
		return res
	}
	names := map[string]string{}
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		names[name] = p
	}
	isClierr := func(x ast.Expr) bool {
		id, ok := x.(*ast.Ident)
		return ok && names[id.Name] == clierrPath
	}
	isCode := func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "CodeAmbiguousSelection" && isClierr(sel.X)
	}
	// holdsCandidates reports whether the builder chain that starts at the
	// clierr.New call has a WithCandidates call. ancestors are the call's
	// ancestors, outermost first.
	holdsCandidates := func(ancestors []ast.Node, call *ast.CallExpr) bool {
		node := ast.Node(call)
		for i := len(ancestors) - 1; i >= 1; i -= 2 {
			sel, ok := ancestors[i].(*ast.SelectorExpr)
			outer, isCall := ancestors[i-1].(*ast.CallExpr)
			if !ok || !isCall || sel.X != node || outer.Fun != sel {
				return false
			}
			if sel.Sel.Name == "WithCandidates" {
				return true
			}
			node = outer
		}
		return false
	}

	// scanDecl scans one declaration: a function, or one type, var or const
	// spec. keys are the names it declares.
	scanDecl := func(keys []string, decl ast.Node) {
		var stack []ast.Node
		ast.Inspect(decl, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			if lit, isLit := n.(*ast.BasicLit); isLit && lit.Kind == token.STRING {
				if v, err := strconv.Unquote(lit.Value); err == nil && v == "AMBIGUOUS_SELECTION" {
					res.offenders = append(res.offenders, fset.Position(n.Pos()).String()+
						": the string literal "+lit.Value+" spells the code; spell the code with clierr.CodeAmbiguousSelection")
				}
				return true
			}
			if !isCode(n) {
				return true
			}
			for _, k := range keys {
				res.uses[k]++
			}
			ok := false
			if len(stack) >= 2 {
				switch p := stack[len(stack)-2].(type) {
				case *ast.BinaryExpr:
					ok = p.Op == token.EQL || p.Op == token.NEQ
				case *ast.CaseClause:
					ok = slices.Contains(p.List, ast.Expr(n.(*ast.SelectorExpr)))
				case *ast.CallExpr:
					sel, isSel := p.Fun.(*ast.SelectorExpr)
					if isSel && sel.Sel.Name == "New" && isClierr(sel.X) && len(p.Args) > 0 && p.Args[0] == n {
						ok = holdsCandidates(stack[:len(stack)-2], p)
					}
				}
			}
			if ok {
				return true
			}
			allowed := false
			for _, k := range keys {
				res.flagged[k]++
				if _, listed := allow[k]; listed {
					allowed = true
				}
			}
			if !allowed {
				res.offenders = append(res.offenders, fset.Position(n.Pos()).String()+
					": clierr.CodeAmbiguousSelection without candidates; chain WithCandidates on the clierr.New call, or compare the code with == or !=")
			}
			return true
		})
	}
	for _, d := range f.Decls {
		switch x := d.(type) {
		case *ast.FuncDecl:
			scanDecl([]string{path + ":" + x.Name.Name}, x)
		case *ast.GenDecl:
			for _, spec := range x.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					scanDecl([]string{path + ":" + sp.Name.Name}, sp)
				case *ast.ValueSpec:
					var keys []string
					for _, n := range sp.Names {
						keys = append(keys, path+":"+n.Name)
					}
					scanDecl(keys, sp)
				}
			}
		}
	}
	return res
}

// TestAmbiguousSelectionCarriesCandidates asserts every error built with
// AMBIGUOUS_SELECTION in the production tree carries the candidates, or is
// a listed function, and that every listed function still uses the code in
// a way the rule would flag.
func TestAmbiguousSelectionCarriesCandidates(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	var offenders []string
	uses := map[string]int{}
	flagged := map[string]int{}
	present := map[string]bool{}
	for i, f := range tree.files {
		res := scanAmbiguousSelection(tree.fset, tree.paths[i], f, ambiguousSelectionAllowlist)
		offenders = append(offenders, res.offenders...)
		maps.Copy(uses, res.uses)
		maps.Copy(flagged, res.flagged)
		maps.Copy(present, declaredKeys(tree.paths[i], f))
	}
	assert.Empty(t, offenders)

	for _, k := range slices.Sorted(maps.Keys(ambiguousSelectionAllowlist)) {
		if !assert.True(t, present[k], "allowlist entry %s names no function", k) {
			continue
		}
		assert.Positive(t, uses[k], "allowlist entry %s no longer uses the code", k)
		assert.Positive(t, flagged[k], "allowlist entry %s has no use the rule would flag", k)
	}
}

const ambiguousProbeHead = "package p\nimport (\n\t\"fmt\"\n\t\"github.com/rancher/rancher-cli/internal/clierr\"\n)\n" +
	"var _, _ = fmt.Sprint, clierr.New\n"

// probeAmbiguousSelection scans ambiguousProbeHead followed by src, as a
// file at path, with the allowlist given. A src that starts with "package "
// is scanned as a whole file.
func probeAmbiguousSelection(t *testing.T, path, src string, allow map[string]string) ambiguousSelectionScan {
	t.Helper()
	if !strings.HasPrefix(src, "package ") {
		src = ambiguousProbeHead + src
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.AllErrors)
	require.NoError(t, err)
	return scanAmbiguousSelection(fset, path, f, allow)
}

// TestAmbiguousSelectionScanFlagsEachShape asserts the scan flags each
// source shape that builds the error with no candidates in its chain.
func TestAmbiguousSelectionScanFlagsEachShape(t *testing.T) {
	t.Parallel()
	for name, src := range map[string]string{
		"bare New":                    "func f() error { return clierr.New(clierr.CodeAmbiguousSelection, \"m\") }",
		"New with only a hint":        "func f() error { return clierr.New(clierr.CodeAmbiguousSelection, \"m\").WithHint(\"h\") }",
		"candidates in a later line":  "func f(c []clierr.Candidate) error { e := clierr.New(clierr.CodeAmbiguousSelection, \"m\"); return e.WithCandidates(c) }",
		"code from a variable":        "func f() error { code := clierr.CodeAmbiguousSelection; return clierr.New(code, \"m\") }",
		"error literal":               "func f() error { return &clierr.Error{Code: clierr.CodeAmbiguousSelection} }",
		"code as a second argument":   "func f(c []clierr.Candidate) error { return clierr.New(\"x\", clierr.CodeAmbiguousSelection).WithCandidates(c) }",
		"code passed to another call": "func f() error { return g(clierr.CodeAmbiguousSelection) }\nfunc g(string) error { return nil }",
		"package-level value":         "var code = clierr.CodeAmbiguousSelection",
		"candidates on another error": "func f(c []clierr.Candidate) error { _ = clierr.New(\"X\", \"o\").WithCandidates(c); return clierr.New(clierr.CodeAmbiguousSelection, \"m\") }",
		"string literal":              "var s = \"AMBIGUOUS_SELECTION\"",
		"raw string literal":          "var s = `AMBIGUOUS_SELECTION`",
		"string literal in a call":    "func f() error { return clierr.New(\"AMBIGUOUS_SELECTION\", \"m\") }",
		"renamed import":              "package p\nimport ce \"github.com/rancher/rancher-cli/internal/clierr\"\nfunc f() error { return ce.New(ce.CodeAmbiguousSelection, \"m\") }",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := "internal/probe/p.go"
			res := probeAmbiguousSelection(t, path, src, nil)
			assert.NotEmpty(t, res.offenders)
		})
	}
}

// TestAmbiguousSelectionScanPassesEachShape asserts the scan accepts each
// source shape that carries the candidates, compares the code, or belongs to
// another package.
func TestAmbiguousSelectionScanPassesEachShape(t *testing.T) {
	t.Parallel()
	for name, src := range map[string]string{
		"head alone":                       "",
		"WithCandidates chain":             "func f(c []clierr.Candidate) error { return clierr.New(clierr.CodeAmbiguousSelection, \"m\").WithCandidates(c) }",
		"WithCandidates after a hint":      "func f(c []clierr.Candidate) error { return clierr.New(clierr.CodeAmbiguousSelection, \"m\").WithHint(\"h\").WithCandidates(c) }",
		"WithCandidates before a cause":    "func f(c []clierr.Candidate, e error) error { return clierr.New(clierr.CodeAmbiguousSelection, \"m\").WithCandidates(c).WithCause(e) }",
		"WithCandidates on a long chain":   "func f(c []clierr.Candidate) error {\n\treturn clierr.New(clierr.CodeAmbiguousSelection,\n\t\tfmt.Sprintf(\"%d\", 1)).\n\t\tWithCandidates(c)\n}",
		"equality comparison":              "func f(e *clierr.Error) bool { return e.Code == clierr.CodeAmbiguousSelection }",
		"inequality comparison":            "func f(e *clierr.Error) bool { return clierr.CodeAmbiguousSelection != e.Code }",
		"switch case":                      "func f(e *clierr.Error) bool { switch e.Code { case clierr.CodeAmbiguousSelection: return true }; return false }",
		"switch case among several":        "func f(e *clierr.Error) bool { switch e.Code { case clierr.CodeNotFound, clierr.CodeAmbiguousSelection: return true }; return false }",
		"another string literal":           "var s = \"NOT_FOUND\"",
		"another code":                     "func f() error { return clierr.New(clierr.CodeNotFound, \"m\") }",
		"function literal with candidates": "var g = func(c []clierr.Candidate) error { return clierr.New(clierr.CodeAmbiguousSelection, \"m\").WithCandidates(c) }",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res := probeAmbiguousSelection(t, "internal/probe/p.go", src, nil)
			assert.Empty(t, res.offenders)
			assert.Empty(t, res.flagged)
		})
	}

	t.Run("inside internal/clierr", func(t *testing.T) {
		t.Parallel()
		res := probeAmbiguousSelection(t, "internal/clierr/x.go", "func f() error { return clierr.New(clierr.CodeAmbiguousSelection, \"m\") }", nil)
		assert.Empty(t, res.offenders)
		assert.Empty(t, res.uses)
	})
}

// TestAmbiguousSelectionAllowlistIsExact asserts an allowlisted function is
// exempt, and that the counts the stale-entry check reads are exact: a use
// that carries candidates counts as a use and is not flagged.
func TestAmbiguousSelectionAllowlistIsExact(t *testing.T) {
	t.Parallel()
	const path = "internal/probe/p.go"
	const bare = "func f() error { return clierr.New(clierr.CodeAmbiguousSelection, \"m\") }"
	allow := map[string]string{path + ":f": "a reason"}

	res := probeAmbiguousSelection(t, path, bare, allow)
	assert.Empty(t, res.offenders)
	assert.Equal(t, 1, res.uses[path+":f"])
	assert.Equal(t, 1, res.flagged[path+":f"])

	const carried = "func f(c []clierr.Candidate) error { return clierr.New(clierr.CodeAmbiguousSelection, \"m\").WithCandidates(c) }"
	res = probeAmbiguousSelection(t, path, carried, allow)
	assert.Equal(t, 1, res.uses[path+":f"])
	assert.Zero(t, res.flagged[path+":f"])

	res = probeAmbiguousSelection(t, path, "func f() error { return nil }", allow)
	assert.Zero(t, res.uses[path+":f"])

	res = probeAmbiguousSelection(t, path, bare+"\nfunc g() error { return clierr.New(clierr.CodeAmbiguousSelection, \"m\") }", allow)
	require.Len(t, res.offenders, 1)
	assert.Contains(t, res.offenders[0], "clierr.CodeAmbiguousSelection")
}

// TestAmbiguousSelectionAllowlistKeysFollowDeclaredKeys asserts a use is
// keyed by the declaration that holds it, as declaredKeys records it: the
// spec of a var block, and every name of a multi-name spec.
func TestAmbiguousSelectionAllowlistKeysFollowDeclaredKeys(t *testing.T) {
	t.Parallel()
	const path = "internal/probe/p.go"
	const block = "var (\n\tfirst = 1\n\tsecond = func() error { return clierr.New(clierr.CodeAmbiguousSelection, \"m\") }\n)"

	res := probeAmbiguousSelection(t, path, block, nil)
	require.Len(t, res.offenders, 1)
	assert.Equal(t, 1, res.uses[path+":second"])
	assert.Zero(t, res.uses[path+":first"])

	res = probeAmbiguousSelection(t, path, block, map[string]string{path + ":second": "a reason"})
	assert.Empty(t, res.offenders)
	assert.Equal(t, 1, res.flagged[path+":second"])

	res = probeAmbiguousSelection(t, path, block, map[string]string{path + ":first": "a reason"})
	assert.Len(t, res.offenders, 1, "an entry for another spec of the block does not exempt the use")

	const multi = "var a, b = clierr.CodeAmbiguousSelection, 1"
	res = probeAmbiguousSelection(t, path, multi, nil)
	assert.Len(t, res.offenders, 1, "one use under two names is reported once")
	assert.Equal(t, 1, res.uses[path+":a"])
	assert.Equal(t, 1, res.uses[path+":b"])
	res = probeAmbiguousSelection(t, path, multi, map[string]string{path + ":b": "a reason"})
	assert.Empty(t, res.offenders, "any listed name exempts the use")
	assert.Equal(t, 1, res.flagged[path+":a"])
	assert.Equal(t, 1, res.flagged[path+":b"])

	const typ = "type T [len(clierr.CodeAmbiguousSelection)]int"
	res = probeAmbiguousSelection(t, path, typ, nil)
	assert.Len(t, res.offenders, 1, "a use inside a type spec is keyed by the type's name")
	assert.Equal(t, 1, res.uses[path+":T"])
}

// TestAmbiguousSelectionLiteralIgnoresTheAllowlist asserts the string
// literal that spells the code is always an offender, and is counted in
// neither the uses nor the flagged uses the stale-entry check reads.
func TestAmbiguousSelectionLiteralIgnoresTheAllowlist(t *testing.T) {
	t.Parallel()
	const path = "internal/probe/p.go"
	allow := map[string]string{path + ":f": "a reason"}
	for name, src := range map[string]string{
		"interpreted": "func f() string { return \"AMBIGUOUS_SELECTION\" }",
		"raw":         "func f() string { return `AMBIGUOUS_SELECTION` }",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res := probeAmbiguousSelection(t, path, src, allow)
			require.Len(t, res.offenders, 1)
			assert.Contains(t, res.offenders[0], "spell the code with clierr.CodeAmbiguousSelection")
			assert.Empty(t, res.uses)
			assert.Empty(t, res.flagged)
		})
	}

	res := probeAmbiguousSelection(t, "internal/clierr/x.go", "func f() string { return \"AMBIGUOUS_SELECTION\" }", nil)
	assert.Empty(t, res.offenders, "internal/clierr defines the code")
}
