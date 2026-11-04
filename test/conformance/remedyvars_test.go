package conformance

import (
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

const remedyVarsDir = "test/conformance/testdata/remedyvars"

// objectKey names a declared object by its package's path under the
// module and its name, so a variable of the same name in another package
// has another key, whichever copy of the type information read it.
func objectKey(obj types.Object) string {
	return strings.TrimPrefix(obj.Pkg().Path(), modulePrefix) + "." + obj.Name()
}

// moduleFunction is the body of a function or method of the module, with
// the type information of its package and its named results.
type moduleFunction struct {
	info    *types.Info
	body    *ast.BlockStmt
	results []types.Object
}

// funcKey names a function or method by its package's path under the module,
// the name of its receiver type for a method, and its name, so two methods of
// one name on different types have different keys.
func funcKey(fn *types.Func) string {
	fn = fn.Origin()
	if recv := fn.Type().(*types.Signature).Recv(); recv != nil {
		t := recv.Type()
		if ptr, ok := t.(*types.Pointer); ok {
			t = ptr.Elem()
		}
		if named, ok := types.Unalias(t).(*types.Named); ok {
			return objectKey(named.Obj()) + "." + fn.Name()
		}
	}
	return objectKey(fn)
}

// moduleFunctions returns the functions and methods of pkgs that have a
// body, by funcKey.
func moduleFunctions(pkgs []*packages.Package) map[string]moduleFunction {
	out := map[string]moduleFunction{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Syntax {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				fn, ok := pkg.TypesInfo.Defs[fd.Name].(*types.Func)
				if !ok {
					continue
				}
				mf := moduleFunction{info: pkg.TypesInfo, body: fd.Body}
				if fd.Type.Results != nil {
					for _, field := range fd.Type.Results.List {
						for _, name := range field.Names {
							if obj := pkg.TypesInfo.Defs[name]; obj != nil {
								mf.results = append(mf.results, obj)
							}
						}
					}
				}
				out[funcKey(fn)] = mf
			}
		}
	}
	return out
}

// localAssignments returns, for each local variable of the code under root,
// the expressions assigned to it by :=, = or += or given to it by a var
// declaration, a function literal included.
func localAssignments(info *types.Info, root ast.Node) map[types.Object][]ast.Expr {
	out := map[types.Object][]ast.Expr{}
	add := func(id *ast.Ident, value ast.Expr) {
		obj := info.Defs[id]
		if obj == nil {
			obj = info.Uses[id]
		}
		if v, ok := obj.(*types.Var); ok && !v.IsField() && v.Pkg() != nil && v.Parent() != v.Pkg().Scope() {
			out[obj] = append(out[obj], value)
		}
	}
	pair := func(names []*ast.Ident, values []ast.Expr) {
		for i, id := range names {
			switch {
			case len(values) == len(names):
				add(id, values[i])
			case len(values) == 1:
				add(id, values[0])
			}
		}
	}
	ast.Inspect(root, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if n.Tok != token.DEFINE && n.Tok != token.ASSIGN && n.Tok != token.ADD_ASSIGN {
				return true
			}
			var names []*ast.Ident
			for _, lhs := range n.Lhs {
				id, _ := lhs.(*ast.Ident)
				if id == nil {
					id = ast.NewIdent("_")
				}
				names = append(names, id)
			}
			pair(names, n.Rhs)
		case *ast.ValueSpec:
			pair(n.Names, n.Values)
		}
		return true
	})
	return out
}

// exprRefs returns the keys of the objects the code under node refers to.
// A local variable counts through the expressions assigned to it (locals),
// a function literal assigned to it included. A function or method of the
// module (funcs) that node names, called or not, counts through the
// expressions its return statements return, and through its named results
// at a return with none, however many steps apart; a method is followed
// where the call names it statically. seen stops a loop.
func exprRefs(node ast.Node, info *types.Info, locals map[types.Object][]ast.Expr, funcs map[string]moduleFunction, seen map[any]bool) []string {
	var refs []string
	ast.Inspect(node, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		used := info.Uses[id]
		if used == nil || used.Pkg() == nil {
			return true
		}
		if values, isLocal := locals[used]; isLocal {
			if !seen[used] {
				seen[used] = true
				for _, v := range values {
					refs = append(refs, exprRefs(v, info, locals, funcs, seen)...)
				}
			}
			return true
		}
		key := objectKey(used)
		fnObj, isFunc := used.(*types.Func)
		if isFunc {
			key = funcKey(fnObj)
		}
		refs = append(refs, key)
		if fn, ok := funcs[key]; ok && isFunc && !seen[key] {
			seen[key] = true
			fnLocals := localAssignments(fn.info, fn.body)
			ast.Inspect(fn.body, func(m ast.Node) bool {
				ret, ok := m.(*ast.ReturnStmt)
				if !ok {
					return true
				}
				for _, r := range ret.Results {
					refs = append(refs, exprRefs(r, fn.info, fnLocals, funcs, seen)...)
				}
				if len(ret.Results) == 0 {
					for _, res := range fn.results {
						if !seen[res] {
							seen[res] = true
							for _, v := range fnLocals[res] {
								refs = append(refs, exprRefs(v, fn.info, fnLocals, funcs, seen)...)
							}
						}
					}
				}
				return true
			})
		}
		return true
	})
	return refs
}

// isStringVar reports whether obj is a variable of a string type.
func isStringVar(obj types.Object) bool {
	basic, ok := obj.Type().Underlying().(*types.Basic)
	return ok && basic.Info()&types.IsString != 0
}

// varsBuiltOnRancher returns the key of every package-level variable of
// pkgs whose initializer refers to clierr.Rancher or clierr.RemedyMarker, to
// another variable that does, or to a function or method of the module whose
// returned expression refers to one of them, however many steps apart. The
// variables of a string type are the members: the text itself, which the hint
// flattener has to read. The others (a function value, a slice, a map, a
// struct) are the carriers: they hold or lead to such text, so a format that
// refers to one is built from it, but a hint cannot be one. A function is
// followed wherever its name appears, called or held in a variable; a local
// variable of the initializer or of a function counts through the expressions
// assigned to it, a function literal included; a named result counts at a
// return with no values. The two are the roots and are neither.
func varsBuiltOnRancher(pkgs []*packages.Package) (members, carriers map[string]bool) {
	roots := []string{"internal/clierr.Rancher", "internal/clierr.RemedyMarker"}
	funcs := moduleFunctions(pkgs)
	type declared struct {
		key  string
		text bool
		refs []string
	}
	var decls []declared
	for _, pkg := range pkgs {
		for _, f := range pkg.Syntax {
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok || gd.Tok != token.VAR {
					continue
				}
				for _, spec := range gd.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, name := range vs.Names {
						obj := pkg.TypesInfo.Defs[name]
						if obj == nil {
							continue
						}
						var init ast.Expr
						switch {
						case len(vs.Values) == len(vs.Names):
							init = vs.Values[i]
						case len(vs.Values) == 1:
							init = vs.Values[0]
						default:
							continue
						}
						locals := localAssignments(pkg.TypesInfo, init)
						decls = append(decls, declared{objectKey(obj), isStringVar(obj), exprRefs(init, pkg.TypesInfo, locals, funcs, map[any]bool{})})
					}
				}
			}
		}
	}
	reached := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, d := range decls {
			if reached[d.key] || slices.Contains(roots, d.key) {
				continue
			}
			if slices.ContainsFunc(d.refs, func(r string) bool { return slices.Contains(roots, r) || reached[r] }) {
				reached[d.key] = true
				changed = true
			}
		}
	}
	members, carriers = map[string]bool{}, map[string]bool{}
	for _, d := range decls {
		switch {
		case reached[d.key] && d.text:
			members[d.key] = true
		case reached[d.key]:
			carriers[d.key] = true
		}
	}
	return members, carriers
}

// builtOnRancherCarriers is every variable of the production tree, of a type
// other than a string, that holds or leads to text built on clierr.Rancher,
// each with the reason it is not a hint. A new one fails until it is reviewed
// and listed; an entry that matches nothing fails.
var builtOnRancherCarriers = map[string]string{
	"internal/cli.openDirectory":      "a function value (resolveDeps behind a package variable, so a test can replace it); it is called to open a directory and holds no text",
	"internal/cli.serverRows":         "a map of functions that read each command's own server from its input; the functions it names lead to a remedy command in an error, and the map holds no text",
	"internal/cli.notFoundHints":      "a table of the details and hints of an unknown command; the marked text in it is a detail rendered before the config directory is resolved, which is not a hint",
	"internal/engine/kinds.table":     "a slice of resource kinds, a composite literal the flattener does not read; the marked text in it is the refusal of create from a manifest, which is not a hint",
	"internal/engine/kinds.kindsByGR": "an index built by a function call over the kinds table; it holds no command text of its own",
	"internal/runtime.markedRancher":  "its value is a byte conversion call that the flattener does not read, and it is not a hint",
}

// builtOnRancherReviewed is every variable built on clierr.Rancher that
// holds a remedy command, which the hint flattener has to read.
var builtOnRancherReviewed = []string{
	"internal/clierr.AuthStatusRemedy",
	"internal/config.DeleteServerCommand",
	"internal/config.LoginCommand",
	"internal/config.StatusCommand",
	"internal/config.TokenReuseOffCommand",
	"internal/config.unfinishedChangeNote",
	"internal/config.unreadableChangeNote",
	"internal/engine.ClusterRequiredHint",
	"internal/engine/access.anonymousListMessage",
	"internal/engine/access.localCannotImpersonateNote",
}

// TestEveryVariableBuiltOnTheMarkerIsReadByTheHintFlattener asserts the
// string variables built on clierr.Rancher in the production tree, by the
// forms varsBuiltOnRancher follows, are exactly the reviewed list, and that
// the hint flattener reads each of them: a hint built on a variable it cannot
// read would pass the hint grammar unchecked. The variables of another type
// built on it are exactly the reviewed carriers.
func TestEveryVariableBuiltOnTheMarkerIsReadByTheHintFlattener(t *testing.T) {
	t.Parallel()
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	members, carriers := varsBuiltOnRancher(pkgs)
	require.Equal(t, builtOnRancherReviewed, slices.Sorted(maps.Keys(members)))
	assert.Equal(t, slices.Sorted(maps.Keys(builtOnRancherCarriers)), slices.Sorted(maps.Keys(carriers)),
		"the variables of a type other than string built on the marker are the reviewed carriers")

	tree := parseSourceTree()
	require.NoError(t, tree.err)
	consts := packageConsts(tree.files, tree.paths)
	for _, key := range builtOnRancherReviewed {
		dot := strings.LastIndex(key, ".")
		_, read := consts[modulePrefix+key[:dot]][key[dot+1:]]
		assert.True(t, read, "the hint flattener does not read %s", key)
	}
}

// remedyVarsFixtures type-checks the fixture packages once.
var remedyVarsFixtures = sync.OnceValues(func() ([]*packages.Package, error) {
	return loadPackages("./" + remedyVarsDir + "/...")
})

// TestVarsBuiltOnTheMarkerScanFixtures asserts the scan on type-checked
// fixture packages: a variable built on clierr.Rancher or on
// clierr.RemedyMarker, one built on that variable, one built on a variable of
// another package, one wrapped in more text, one initialized by a function
// that returns text built on them (directly, through a local variable, or
// through another function, held in a variable, a method, or a named result
// and a bare return) are members; a variable of another type built on them (a
// function value, a slice, a map, a struct) is a carrier and not a member; a
// variable of plain text, one initialized by a function that returns plain
// text, and a package's own variable named Rancher with one built on it, are
// neither.
func TestVarsBuiltOnTheMarkerScanFixtures(t *testing.T) {
	t.Parallel()
	pkgs, err := remedyVarsFixtures()
	require.NoError(t, err)
	require.Len(t, pkgs, 3)
	base := remedyVarsDir
	members, carriers := varsBuiltOnRancher(pkgs)
	assert.Equal(t, []string{
		base + "/builton.ByName",
		base + "/builton.Parts",
		base + "/builton.Set",
		base + "/builton.textFn",
	}, slices.Sorted(maps.Keys(carriers)))
	assert.Equal(t, []string{
		base + "/builton.Chained",
		base + "/builton.Direct",
		base + "/builton.FromFunction",
		base + "/builton.FromFunctionValue",
		base + "/builton.FromLocalInFunction",
		base + "/builton.FromMarker",
		base + "/builton.FromMethod",
		base + "/builton.FromNamedResult",
		base + "/builton.FromNestedFunction",
		base + "/builton.Wrapped",
		base + "/consumer.FromOther",
	}, slices.Sorted(maps.Keys(members)))
}
