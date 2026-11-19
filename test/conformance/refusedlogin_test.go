package conformance

import (
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

const (
	clierrRequestPkg   = modulePrefix + "internal/clierr"
	refusedLoginFixDir = "test/conformance/testdata/refusedlogin"
)

// requestLoginAllowed is a Request literal of production code that sets
// neither NoCredential: true nor a Login: the exact number of them in one
// function, with the reason a 401 to the request names no login command.
type requestLoginAllowed struct {
	count  int
	reason string
}

// requestLoginAllowlist lists them by file and function. A function with a
// different number of such literals fails, an entry that matches none fails.
var requestLoginAllowlist = map[string]requestLoginAllowed{
	"internal/token/owner.go requestOwner": {1, "its bearer is the token a sign-in just created, which the call checks the owner of; it is no stored login that the user replaces by name"},
}

// isClierrRequest reports whether t, a type as the checker recorded it, is
// clierr.Request or a pointer to it, whatever the spelling in the source (an
// elided literal type, an aliased import, a type alias).
func isClierrRequest(t types.Type) bool {
	if t == nil {
		return false
	}
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		t = types.Unalias(p.Elem())
	}
	return isNamedType(t, clierrRequestPkg, "Request")
}

// requestFinding is a place that builds a clierr.Request in production code
// with no login command and that sends a credential.
type requestFinding struct {
	position string
	file     string
	function string
	what     string
}

// setsKey returns the value of the key in a composite literal's elements.
func setsKey(lit *ast.CompositeLit, key string) (ast.Expr, bool) {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == key {
			return kv.Value, true
		}
	}
	return nil, false
}

// isZeroValueExpr reports whether e is the zero value written out: nil or an
// empty composite literal.
func isZeroValueExpr(e ast.Expr) bool {
	switch x := ast.Unparen(e).(type) {
	case *ast.Ident:
		return x.Name == "nil"
	case *ast.CompositeLit:
		return len(x.Elts) == 0
	}
	return false
}

// isClierrRequestValue reports whether t is clierr.Request itself, not a
// pointer to it.
func isClierrRequestValue(t types.Type) bool {
	return t != nil && isNamedType(types.Unalias(t), clierrRequestPkg, "Request")
}

// holdsRequestValues reports whether t is a slice, array or map whose element
// is a clierr.Request value: made empty, it holds zero Requests that are filled
// in later.
func holdsRequestValues(t types.Type) bool {
	if t == nil {
		return false
	}
	switch u := types.Unalias(t).(type) {
	case *types.Slice:
		return isClierrRequestValue(u.Elem())
	case *types.Array:
		return isClierrRequestValue(u.Elem())
	case *types.Map:
		return isClierrRequestValue(u.Elem())
	}
	return false
}

// declLabel names the function, or the type, a node of decl is in, for the
// allowlist.
func declLabel(decl ast.Decl) string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		return d.Name.Name
	case *ast.GenDecl:
		if len(d.Specs) == 1 {
			if ts, ok := d.Specs[0].(*ast.TypeSpec); ok {
				return "type " + ts.Name.Name
			}
		}
	}
	return "<package level>"
}

// requestFindings reports, in the files of pkg, every composite literal of
// type clierr.Request that sets neither NoCredential: true nor a Login other
// than the zero value (a parenthesized zero value included), and every other
// way to make a Request that no literal shows: new(clierr.Request), a declared
// clierr.Request variable with no value, make of a slice or map of Request
// values, an array of Request values, a named result of type clierr.Request
// and a struct field of type clierr.Request. Each is filled in after it is
// made, where no scan reads it. A pointer to a Request, as a variable, a field
// or an unnamed result, makes none and is accepted.
func requestFindings(pkg *packages.Package) []requestFinding {
	var out []requestFinding
	info := pkg.TypesInfo
	for _, f := range pkg.Syntax {
		for _, decl := range f.Decls {
			function := declLabel(decl)
			add := func(n ast.Node, what string) {
				pos := pkg.Fset.Position(n.Pos())
				out = append(out, requestFinding{pos.String(), pos.Filename, function, what})
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CompositeLit:
					if !isClierrRequest(info.TypeOf(x)) {
						return true
					}
					if v, ok := setsKey(x, "NoCredential"); ok {
						if tv, ok := info.Types[v]; ok && tv.Value != nil && tv.Value.String() == "true" {
							return true
						}
					}
					if v, ok := setsKey(x, "Login"); ok && !isZeroValueExpr(v) {
						return true
					}
					add(x, "a clierr.Request literal that sets neither NoCredential: true nor Login")
				case *ast.CallExpr:
					id, ok := ast.Unparen(x.Fun).(*ast.Ident)
					if !ok || len(x.Args) == 0 {
						return true
					}
					if _, builtin := info.Uses[id].(*types.Builtin); !builtin {
						return true
					}
					switch {
					case id.Name == "new" && isClierrRequest(info.TypeOf(x.Args[0])):
						add(x, "new(clierr.Request)")
					case id.Name == "make" && holdsRequestValues(info.TypeOf(x.Args[0])):
						add(x, "make of Request values")
					}
				case *ast.ValueSpec:
					if x.Type != nil && len(x.Values) == 0 && isClierrRequestValue(info.TypeOf(x.Type)) {
						add(x, "a declared clierr.Request with no value")
					}
				case *ast.ArrayType:
					if x.Len != nil && holdsRequestValues(info.TypeOf(x)) {
						add(x, "an array of clierr.Request values")
					}
				case *ast.FuncType:
					if x.Results == nil {
						return true
					}
					for _, field := range x.Results.List {
						if len(field.Names) > 0 && isClierrRequestValue(info.TypeOf(field.Type)) {
							add(field, "a named result of type clierr.Request")
						}
					}
				case *ast.StructType:
					for _, field := range x.Fields.List {
						if isClierrRequestValue(info.TypeOf(field.Type)) {
							add(field, "a struct field of type clierr.Request")
						}
					}
				}
				return true
			})
		}
	}
	return out
}

// moduleRelative is path relative to the module root, slash-separated.
func moduleRelative(t *testing.T, path string) string {
	t.Helper()
	root, err := filepath.Abs(".")
	require.NoError(t, err)
	rel, err := filepath.Rel(root, path)
	require.NoError(t, err)
	return filepath.ToSlash(rel)
}

// buildSets are the module's packages for this machine, for windows and for
// linux.
func buildSets(t *testing.T) [][]*packages.Package {
	t.Helper()
	host, err := fieldManagerPackages()
	require.NoError(t, err)
	sets := [][]*packages.Package{host}
	for _, goos := range []string{"windows", "linux"} {
		other, err := otherGOOSPackages(goos)
		require.NoError(t, err)
		if other != nil {
			sets = append(sets, other)
		}
	}
	return sets
}

// TestEveryRequestThatSendsACredentialNamesALoginCommand asserts every
// clierr.Request that production code builds either sets NoCredential: true
// or a Login other than the zero value, so a 401 to a request that carried a
// stored login's token names the login command for that entry. The scan reads
// checked types, not spellings, of every package of the module that the
// build for this machine, for windows and for linux compiles (tests and
// testdata left out): it finds the literal however its type is written, and
// refuses the other ways to make a Request that requestFindings lists. The
// literals of the allowlist are the exceptions, each with its reason and the
// exact number a function may have; an entry that matches none fails.
func TestEveryRequestThatSendsACredentialNamesALoginCommand(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	counts := map[string]int{}
	scanned := 0
	for _, pkgs := range buildSets(t) {
		for _, pkg := range pkgs {
			scanned += len(pkg.Syntax)
			for _, fnd := range requestFindings(pkg) {
				if seen[fnd.position] {
					continue
				}
				seen[fnd.position] = true
				key := moduleRelative(t, fnd.file) + " " + fnd.function
				if _, ok := requestLoginAllowlist[key]; ok {
					counts[key]++
					continue
				}
				t.Errorf("%s: %s in %s", fnd.position, fnd.what, fnd.function)
			}
		}
	}
	require.Positive(t, scanned, "the scan sees the module's files")
	for _, key := range slices.Sorted(maps.Keys(requestLoginAllowlist)) {
		assert.Equal(t, requestLoginAllowlist[key].count, counts[key], "the allowlist entry %q lists this many Request literals with no login command", key)
	}
}

// refusedLoginFixtures loads the type-checked fixture packages by name.
func refusedLoginFixtures(t *testing.T) map[string]*packages.Package {
	t.Helper()
	pkgs, err := loadPackages("./"+refusedLoginFixDir+"/...", "")
	require.NoError(t, err)
	byName := map[string]*packages.Package{}
	for _, pkg := range pkgs {
		byName[filepath.Base(pkg.PkgPath)] = pkg
	}
	return byName
}

// TestRefusedLoginRequestScanFixtures asserts the Request scan on type-checked
// fixture packages: it accepts a literal with a login command or
// NoCredential: true and pointers to a Request, and reports a literal with
// neither, a login that is the zero value (parenthesized too),
// new(clierr.Request), a declared clierr.Request variable, an elided literal
// type in a slice and a map, a renamed import, a type alias, make of a slice
// of Request values, an array of them, a named result and a struct field.
func TestRefusedLoginRequestScanFixtures(t *testing.T) {
	t.Parallel()
	byName := refusedLoginFixtures(t)
	for name, want := range map[string]int{
		"compliant": 0, "missing": 1, "zerologin": 1, "parenlogin": 1, "newcall": 1, "vardecl": 1, "elided": 2, "aliased": 2,
		"makeslice": 1, "arraytype": 1, "namedresult": 1, "structfield": 2,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pkg, ok := byName[name]
			require.True(t, ok, "fixture package %s", name)
			found := requestFindings(pkg)
			assert.Len(t, found, want, "%v", found)
		})
	}
}

// authStatusFindings reports, in the files of the type-checked packages, every
// command built with the words "auth status" (clierr's NewCommand, FileCommand
// or FileCommandIn, found through the checked objects so a renamed import is
// read too, the words read through constants and concatenation) and every
// hint text that holds the command rancher auth status, in a function body, in
// a package-level declaration and in a Hint field of a table. seen counts the
// command constructors and hint sites it read, so a scan that reads nothing
// fails its test.
func authStatusFindings(t *testing.T, pkgs []*packages.Package) (findings []string, seen int) {
	t.Helper()
	var files []*ast.File
	var paths []string
	infoOf := map[*ast.File]*types.Info{}
	fsetOf := map[*ast.File]*token.FileSet{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Syntax {
			files = append(files, f)
			paths = append(paths, moduleRelative(t, pkg.Fset.Position(f.Pos()).Filename))
			infoOf[f], fsetOf[f] = pkg.TypesInfo, pkg.Fset
		}
	}
	funcsByName := map[string][]*ast.FuncDecl{}
	for _, f := range files {
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok {
				funcsByName[fd.Name.Name] = append(funcsByName[fd.Name.Name], fd)
			}
		}
	}
	fl := &flattener{funcsByName: funcsByName, consts: packageConsts(files, paths), funcScope: map[*ast.FuncDecl]fileScope{}}
	for i, f := range files {
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok {
				fl.funcScope[fd] = scopeOf(f, paths[i])
			}
		}
	}
	starts := map[string]int{"NewCommand": 0, "FileCommand": 0, "FileCommandIn": 1}
	for i, f := range files {
		fl.scope = scopeOf(f, paths[i])
		info, fset := infoOf[f], fsetOf[f]
		for _, decl := range f.Decls {
			var assigns map[string][]ast.Expr
			if fd, ok := decl.(*ast.FuncDecl); ok {
				if fd.Body == nil {
					continue
				}
				assigns = collectLocalAssigns(fd.Body)
			}
			flag := func(expr ast.Expr, pos token.Pos, command bool) {
				seen++
				for _, text := range fl.flatten(expr, assigns, 0) {
					text = strings.TrimSpace(text)
					if (command && text == "auth status") || (!command && strings.Contains(text, "rancher auth status")) {
						findings = append(findings, fset.Position(pos).String())
						return
					}
				}
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					var id *ast.Ident
					switch fn := ast.Unparen(x.Fun).(type) {
					case *ast.Ident:
						id = fn
					case *ast.SelectorExpr:
						id = fn.Sel
					}
					if id == nil {
						return true
					}
					obj, ok := info.Uses[id].(*types.Func)
					if !ok || obj.Pkg() == nil || obj.Pkg().Path() != clierrRequestPkg {
						return true
					}
					if arg, ok := starts[obj.Name()]; ok && arg < len(x.Args) {
						flag(x.Args[arg], x.Pos(), true)
					}
					if (obj.Name() == "WithHint" || obj.Name() == "WithHintCommand") && len(x.Args) > 0 {
						flag(x.Args[0], x.Pos(), false)
					}
				case *ast.KeyValueExpr:
					if key, ok := x.Key.(*ast.Ident); ok && key.Name == "Hint" {
						flag(x.Value, x.Pos(), false)
					}
				}
				return true
			})
		}
	}
	return findings, seen
}

// TestNoCommandNamesAuthStatusAsAHint asserts no production Go file builds a
// command with the words "auth status" or gives a hint that holds the command
// rancher auth status: a refused credential names the login command, and auth
// status sends the same credential. The words are read through constants and
// concatenation, as the hint grammar reads a hint, in every package the build
// for this machine, for windows and for linux compiles.
func TestNoCommandNamesAuthStatusAsAHint(t *testing.T) {
	t.Parallel()
	seen := 0
	reported := map[string]bool{}
	for _, pkgs := range buildSets(t) {
		found, n := authStatusFindings(t, pkgs)
		seen += n
		for _, pos := range found {
			if !reported[pos] {
				reported[pos] = true
				t.Errorf("%s: a command or a hint that names auth status", pos)
			}
		}
	}
	require.Positive(t, seen, "the scan sees command constructors and hints")
}

// TestAuthStatusScanFixtures asserts the scan on type-checked fixture
// packages: it reports the words written as a literal, through a constant, in
// a hint built from clierr.Rancher, in a package-level declaration, in a Hint
// field of a table and through a renamed import, and accepts other commands.
func TestAuthStatusScanFixtures(t *testing.T) {
	t.Parallel()
	byName := refusedLoginFixtures(t)
	for name, want := range map[string]int{
		"authstatusliteral": 1, "authstatusconstant": 1, "authstatusconcat": 1, "authstatusclean": 0,
		"authstatuspackagelevel": 1, "authstatustable": 1, "authstatusrenamed": 1,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pkg, ok := byName[name]
			require.True(t, ok, "fixture package %s", name)
			found, seen := authStatusFindings(t, []*packages.Package{pkg})
			assert.Len(t, found, want, "%v", found)
			assert.Positive(t, seen, "the scan reads the fixture's constructors")
		})
	}
}
