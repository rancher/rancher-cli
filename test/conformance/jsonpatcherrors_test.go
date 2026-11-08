package conformance

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

const (
	jsonPatchPath = "github.com/evanphx/json-patch/v5"
	jsonPatchDir  = "test/conformance/testdata/jsonpatcherrors"
)

// The rules of the scan, as they appear in a hit.
const (
	jsonPatchRuleErrorCall = "error-call"
	jsonPatchRuleArgument  = "error-argument"
	jsonPatchRuleUse       = "library-error-use"
	jsonPatchRuleReturn    = "library-call-returned"
)

var jsonPatchRuleText = map[string]string{
	jsonPatchRuleErrorCall: "the Error method of an error is called or taken as a value",
	jsonPatchRuleArgument:  "an error is passed to a formatter, an error constructor or panic",
	jsonPatchRuleUse:       "an error of the JSON patch library is used other than as a nil check, an argument of errors.Is or errors.AsType, or an argument of a mapper",
	jsonPatchRuleReturn:    "a call of the JSON patch library is returned to the caller",
}

// jsonPatchMappers are the functions that turn an error of the JSON patch
// library into the message the user sees, found in the scanned package.
var jsonPatchMappers = []string{"explainApply", "explainFirstFailure", "explainOperation", "explainMergeApply"}

// jsonPatchHit is one expression the scan flags, in the function that holds
// it.
type jsonPatchHit struct {
	function string
	rule     string
	pos      string
	from, to token.Pos
}

func (h jsonPatchHit) String() string { return h.function + " " + h.rule }

// jsonPatchAllowance is the number of hits an allowlisted function covers and
// why none shows the library's text to a user.
type jsonPatchAllowance struct {
	count int
	why   string
}

// jsonPatchHitAllowlist maps a function (as enclosingFunctionKey names it) to
// the hits it covers. A different number of hits, a hit with no row and a row
// with no hit fail the test.
var jsonPatchHitAllowlist = map[string]jsonPatchAllowance{
	"internal/engine/resources.patchCmd.decodePatch": {1, "formats the error of reading the user's own patch file; the JSON patch library is not involved"},
	"internal/engine/resources.patchDoc.apply":       {1, "returns the merge patch result to its caller, which passes the error to the mapper only"},
	"internal/engine/resources.patchDoc.runOps":      {1, "returns the library result to apply and to the search for the failing operation, which pass the error to the mapper only"},
}

// importsPath reports whether f imports the package at pkgPath.
func importsPath(f *ast.File, pkgPath string) bool {
	for _, imp := range f.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err == nil && p == pkgPath {
			return true
		}
	}
	return false
}

// funcOf returns the function or method a call expression names, looking
// through an explicit instantiation (errors.AsType[*T]).
func funcOf(info *types.Info, fun ast.Expr) *types.Func {
	fun = ast.Unparen(fun)
	switch x := fun.(type) {
	case *ast.IndexExpr:
		return funcOf(info, x.X)
	case *ast.IndexListExpr:
		return funcOf(info, x.X)
	case *ast.Ident:
		fn, _ := info.Uses[x].(*types.Func)
		return fn
	case *ast.SelectorExpr:
		fn, _ := info.Uses[x.Sel].(*types.Func)
		return fn
	}
	return nil
}

// implementsError reports whether t, or a pointer to it, is an error.
func implementsError(t types.Type) bool {
	if t == nil {
		return false
	}
	switch tt := t.(type) {
	case *types.Tuple:
		return false
	case *types.Basic:
		if tt.Kind() == types.UntypedNil {
			return false
		}
	}
	errType, _ := types.Universe.Lookup("error").Type().Underlying().(*types.Interface)
	return types.Implements(t, errType) || types.Implements(types.NewPointer(t), errType)
}

// isLibraryFunc reports whether fn is a function or method of the JSON patch
// library.
func isLibraryFunc(fn *types.Func) bool {
	return fn != nil && fn.Pkg() != nil && fn.Pkg().Path() == jsonPatchPath
}

// isFormatterOrConstructor reports whether fn builds text or an error from
// its arguments: the fmt printers, errors.New and the constructors of
// internal/clierr that take an error or a message.
func isFormatterOrConstructor(fn *types.Func) bool {
	if fn == nil || fn.Pkg() == nil {
		return false
	}
	switch fn.Pkg().Path() {
	case "fmt":
		return slices.Contains([]string{"Errorf", "Sprintf", "Sprint", "Sprintln", "Fprint", "Fprintf", "Fprintln", "Append", "Appendf", "Appendln"}, fn.Name())
	case "errors":
		return fn.Name() == "New"
	case clierrPath:
		return slices.Contains([]string{"New", "Wrap", "WithCause", "WithMessage", "Retryable"}, fn.Name())
	}
	return false
}

// isNilIdent reports whether e is the predeclared nil.
func isNilIdent(info *types.Info, e ast.Expr) bool {
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok {
		return false
	}
	_, isNil := info.Uses[id].(*types.Nil)
	return isNil
}

// scanJSONPatchDecl returns the hits of the rules in one declaration, and the
// number of calls of the library it holds.
func scanJSONPatchDecl(pkg *packages.Package, root ast.Node, function string) (hits []jsonPatchHit, libraryCalls int) {
	info := pkg.TypesInfo
	hit := func(n ast.Node, rule string) {
		hits = append(hits, jsonPatchHit{function: function, rule: rule, pos: pkg.Fset.Position(n.Pos()).String(), from: n.Pos(), to: n.End()})
	}

	bound := map[types.Object]bool{}
	bind := func(lhs []ast.Expr, rhs ast.Expr) {
		call, ok := ast.Unparen(rhs).(*ast.CallExpr)
		if !ok || !isLibraryFunc(funcOf(info, call.Fun)) {
			return
		}
		var results []types.Type
		if tuple, ok := info.TypeOf(call).(*types.Tuple); ok {
			for v := range tuple.Variables() {
				results = append(results, v.Type())
			}
		} else {
			results = append(results, info.TypeOf(call))
		}
		for i, l := range lhs {
			id, ok := l.(*ast.Ident)
			if !ok || i >= len(results) || !implementsError(results[i]) {
				continue
			}
			if obj := info.ObjectOf(id); obj != nil {
				bound[obj] = true
			}
		}
	}
	ast.Inspect(root, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if len(n.Rhs) == 1 {
				bind(n.Lhs, n.Rhs[0])
			}
		case *ast.ValueSpec:
			if len(n.Values) == 1 {
				lhs := make([]ast.Expr, len(n.Names))
				for i, id := range n.Names {
					lhs[i] = id
				}
				bind(lhs, n.Values[0])
			}
		}
		return true
	})

	var uses []jsonPatchHit
	var stack []ast.Node
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		switch n := n.(type) {
		case *ast.CallExpr:
			fn := funcOf(info, n.Fun)
			if isLibraryFunc(fn) {
				libraryCalls++
			}
			id, isIdent := ast.Unparen(n.Fun).(*ast.Ident)
			isPanic := false
			if isIdent {
				_, isPanic = info.Uses[id].(*types.Builtin)
				isPanic = isPanic && id.Name == "panic"
			}
			if (isFormatterOrConstructor(fn) || isPanic) && slices.ContainsFunc(n.Args, func(a ast.Expr) bool { return implementsError(info.TypeOf(a)) }) {
				hit(n, jsonPatchRuleArgument)
			}
		case *ast.SelectorExpr:
			if fn, ok := info.Uses[n.Sel].(*types.Func); ok && fn.Name() == "Error" {
				if sig, ok := fn.Type().(*types.Signature); ok && sig.Params().Len() == 0 && sig.Results().Len() == 1 && implementsError(info.TypeOf(n.X)) {
					hit(n, jsonPatchRuleErrorCall)
					if call, ok := stack[len(stack)-2].(*ast.CallExpr); ok && ast.Unparen(call.Fun) == n {
						hits[len(hits)-1].to = call.End()
					}
				}
			}
		case *ast.ReturnStmt:
			for _, r := range n.Results {
				if call, ok := ast.Unparen(r).(*ast.CallExpr); ok && isLibraryFunc(funcOf(info, call.Fun)) {
					hit(n, jsonPatchRuleReturn)
					break
				}
			}
		case *ast.Ident:
			obj := info.Uses[n]
			if obj == nil || !bound[obj] || len(stack) < 2 {
				return true
			}
			if allowedLibraryErrorUse(info, pkg.Types, n, stack[len(stack)-2]) {
				return true
			}
			uses = append(uses, jsonPatchHit{function: function, rule: jsonPatchRuleUse, pos: pkg.Fset.Position(n.Pos()).String(), from: n.Pos(), to: n.End()})
		}
		return true
	})
	for _, u := range uses {
		inside := slices.ContainsFunc(hits, func(h jsonPatchHit) bool { return h.from <= u.from && u.to <= h.to })
		if !inside {
			hits = append(hits, u)
		}
	}
	return hits, libraryCalls
}

// allowedLibraryErrorUse reports whether the identifier id, a variable that
// holds an error of the library, is used in an allowed way inside parent: a
// comparison with nil, an argument of errors.Is or errors.AsType, an argument
// of a mapper of the scanned package, or the target of an assignment.
func allowedLibraryErrorUse(info *types.Info, scanned *types.Package, id *ast.Ident, parent ast.Node) bool {
	switch p := parent.(type) {
	case *ast.BinaryExpr:
		if p.Op != token.EQL && p.Op != token.NEQ {
			return false
		}
		other := p.X
		if ast.Unparen(p.X) == id {
			other = p.Y
		}
		return isNilIdent(info, other)
	case *ast.CallExpr:
		fn := funcOf(info, p.Fun)
		if fn == nil || fn.Pkg() == nil {
			return false
		}
		if fn.Pkg().Path() == "errors" && (fn.Name() == "Is" || fn.Name() == "AsType") {
			return len(p.Args) > 0 && ast.Unparen(p.Args[0]) == id
		}
		return fn.Pkg() == scanned && slices.Contains(jsonPatchMappers, fn.Name())
	case *ast.AssignStmt:
		return slices.ContainsFunc(p.Lhs, func(l ast.Expr) bool { return ast.Unparen(l) == id })
	}
	return false
}

// scanJSONPatchErrors returns the hits of the rules in the non-test files of
// pkg that import the JSON patch library, the number of those files and the
// number of calls of the library in them.
func scanJSONPatchErrors(pkg *packages.Package, wd string) (hits []jsonPatchHit, files, calls int) {
	for _, f := range pkg.Syntax {
		name := pkg.Fset.Position(f.Pos()).Filename
		if strings.HasSuffix(name, "_test.go") || !importsPath(f, jsonPatchPath) {
			continue
		}
		files++
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				h, c := scanJSONPatchDecl(pkg, d, enclosingFunctionKey(pkg, d))
				hits, calls = append(hits, h...), calls+c
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || len(vs.Names) == 0 {
						continue
					}
					h, c := scanJSONPatchDecl(pkg, vs, strings.TrimPrefix(pkg.PkgPath, modulePrefix)+"."+vs.Names[0].Name)
					hits, calls = append(hits, h...), calls+c
				}
			}
		}
	}
	for i := range hits {
		if rel, err := filepath.Rel(wd, hits[i].pos); err == nil {
			hits[i].pos = filepath.ToSlash(rel)
		}
	}
	return hits, files, calls
}

// checkJSONPatchHits returns one message per hit with no allowlist row, one
// per row whose count differs from its hits, and one per row with no hit.
func checkJSONPatchHits(hits []jsonPatchHit, allow map[string]jsonPatchAllowance) []string {
	byFunction := map[string][]jsonPatchHit{}
	for _, h := range hits {
		byFunction[h.function] = append(byFunction[h.function], h)
	}
	describe := func(hs []jsonPatchHit) string {
		var parts []string
		for _, h := range hs {
			parts = append(parts, h.pos+": "+jsonPatchRuleText[h.rule])
		}
		return strings.Join(parts, "; ")
	}
	var out []string
	for _, function := range slices.Sorted(maps.Keys(byFunction)) {
		a, ok := allow[function]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("%s has no allowlist row: %s", function, describe(byFunction[function])))
		case len(byFunction[function]) != a.count:
			out = append(out, fmt.Sprintf("%s: the allowlist covers %d hit(s) (%s), found %d: %s", function, a.count, a.why, len(byFunction[function]), describe(byFunction[function])))
		}
	}
	for _, function := range slices.Sorted(maps.Keys(allow)) {
		if len(byFunction[function]) == 0 {
			out = append(out, "allowlist row "+function+" matches nothing: remove it")
		}
	}
	return out
}

// TestJSONPatchErrorsAreNeverShownAsText asserts that no error of the JSON
// patch library turns into text in the files that import it: a typed scan of
// the non-test files of the module flags the Error method of any error, called
// or taken as a method value or a method expression, an error passed to
// fmt.Errorf, fmt.Sprintf, fmt.Sprint, fmt.Sprintln, fmt.Fprint*,
// fmt.Append*, errors.New, clierr.New, clierr.Wrap, WithCause or panic, a variable
// bound to the error result of a library call and used other than as a nil
// check, an argument of errors.Is or errors.AsType or an argument of a mapper
// (explainApply, explainFirstFailure, explainOperation, explainMergeApply of
// the scanned package), and a return of a library call. Each hit is one
// expression, counted once when two rules flag it. Every hit needs a row in
// jsonPatchHitAllowlist for its function with the count and a reason.
//
// Limits: the scan loads the files of the host platform only; a function
// literal in a package-level var has no function declaration to key a row by,
// so the row is keyed by the variable; an error passed as any or fmt.Stringer,
// or stored in a slice, is not an error-typed argument; the scan follows no
// flow past the variable the error is bound to (an error stored in a struct
// field or a closure is not followed; an alias such as e := err is a use of
// err and is flagged); a library error that reaches a function as a
// parameter or from a wrapper such as the guarded call is covered by the
// first two rules only; a method value of the library that is called later
// is not a call site; a function variable that holds fmt.Sprintf or another
// formatter (sp := fmt.Sprintf) is not a formatter call; it sees only files
// that import the library, and a mapper in a file that does not import it
// is not checked; and it cannot tell a mapper that formats text from one
// that does not (the tests of the resources package cover that).
func TestJSONPatchErrorsAreNeverShownAsText(t *testing.T) {
	t.Parallel()
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	var hits []jsonPatchHit
	files, calls := 0, 0
	for _, pkg := range pkgs {
		h, f, c := scanJSONPatchErrors(pkg, wd)
		hits, files, calls = append(hits, h...), files+f, calls+c
	}
	assert.GreaterOrEqual(t, files, 2, "the scan read too few files that import the library")
	assert.Greater(t, calls, 4, "the scan found too few calls of the library")
	for _, problem := range checkJSONPatchHits(hits, jsonPatchHitAllowlist) {
		t.Error(problem)
	}
	for function, a := range jsonPatchHitAllowlist {
		assert.NotEmpty(t, a.why, function)
	}
}

// TestJSONPatchHitAllowlistRules asserts the allowlist rules on hits: a row
// covers its function and its count, a function with no row is reported, a
// different count fails and a row that matches nothing is stale.
func TestJSONPatchHitAllowlistRules(t *testing.T) {
	t.Parallel()
	one := jsonPatchHit{function: "p.f", rule: jsonPatchRuleErrorCall, pos: "a.go:1:1"}
	allow := map[string]jsonPatchAllowance{"p.f": {1, "why"}}
	assert.Empty(t, checkJSONPatchHits([]jsonPatchHit{one}, allow))
	assert.Len(t, checkJSONPatchHits([]jsonPatchHit{one, {function: "p.g", rule: jsonPatchRuleReturn, pos: "b.go:2:1"}}, allow), 1, "a function with no row is reported")
	assert.Len(t, checkJSONPatchHits([]jsonPatchHit{one, one}, allow), 1, "a second hit fails the count")
	assert.Equal(t, []string{"allowlist row p.gone matches nothing: remove it"},
		checkJSONPatchHits([]jsonPatchHit{one}, map[string]jsonPatchAllowance{"p.f": {1, "why"}, "p.gone": {1, "why"}}))
}

// jsonPatchFixtures type-checks the fixture packages once.
var jsonPatchFixtures = sync.OnceValues(func() (map[string]*packages.Package, error) {
	pkgs, err := loadPackages("./"+jsonPatchDir+"/...", "")
	if err != nil {
		return nil, err
	}
	out := map[string]*packages.Package{}
	for _, pkg := range pkgs {
		out[path.Base(pkg.PkgPath)] = pkg
	}
	return out, nil
})

// TestJSONPatchErrorScanFixtures asserts the scan on type-checked fixture
// packages: one planted function for each flagged form, a package that uses a
// library error only in the allowed ways, and a package that formats an error
// and does not import the library, which is not scanned.
func TestJSONPatchErrorScanFixtures(t *testing.T) {
	t.Parallel()
	pkgs, err := jsonPatchFixtures()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	prefix := jsonPatchDir + "/"
	for _, tc := range []struct {
		fixture string
		files   int
		hits    []string
	}{
		{"planted", 1, []string{
			prefix + "planted.alias " + jsonPatchRuleUse,
			prefix + "planted.appended " + jsonPatchRuleArgument,
			prefix + "planted.asText " + jsonPatchRuleErrorCall,
			prefix + "planted.callError " + jsonPatchRuleErrorCall,
			prefix + "planted.methodExpression " + jsonPatchRuleErrorCall,
			prefix + "planted.methodValue " + jsonPatchRuleErrorCall,
			prefix + "planted.packageLevel " + jsonPatchRuleErrorCall,
			prefix + "planted.panicked " + jsonPatchRuleArgument,
			prefix + "planted.printed " + jsonPatchRuleArgument,
			prefix + "planted.returned " + jsonPatchRuleReturn,
			prefix + "planted.returnedMerge " + jsonPatchRuleReturn,
			prefix + "planted.viaCause " + jsonPatchRuleArgument,
			prefix + "planted.wrapped " + jsonPatchRuleArgument,
		}},
		{"allowed", 1, nil},
		{"nolibrary", 0, nil},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			pkg, ok := pkgs[tc.fixture]
			require.True(t, ok, "fixture package %s", tc.fixture)
			hits, files, _ := scanJSONPatchErrors(pkg, wd)
			assert.Equal(t, tc.files, files)
			var got []string
			for _, h := range hits {
				got = append(got, h.String())
			}
			slices.Sort(got)
			assert.Equal(t, tc.hits, got)
		})
	}
}

// TestJSONPatchErrorFixturesAreAllListed asserts every fixture package on
// disk is in the table above.
func TestJSONPatchErrorFixturesAreAllListed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(filepath.FromSlash(jsonPatchDir))
	require.NoError(t, err)
	var onDisk []string
	for _, e := range entries {
		if e.IsDir() {
			onDisk = append(onDisk, e.Name())
		}
	}
	pkgs, err := jsonPatchFixtures()
	require.NoError(t, err)
	assert.ElementsMatch(t, onDisk, slices.Collect(maps.Keys(pkgs)))
	assert.ElementsMatch(t, []string{"planted", "allowed", "nolibrary"}, onDisk)
}
