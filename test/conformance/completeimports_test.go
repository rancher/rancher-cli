package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// The functions that answer a live completion site are declared in a closed
// set of restricted `complete` packages, one per owner of a live completion
// source. A source may talk to the server only through the capability the
// surface hands it, so these packages may import the seam, the kind table,
// structured errors and a fixed set of standard-library packages - and
// nothing that opens a socket, reads the environment or a file, runs a
// process, or reaches a client or a credential store, not even through a
// same-package helper. A source that needs no credential gets its server and
// CA from its command's CompletionTarget, which reads the loaded config, the
// environment and a CA file; the package itself reads none of them. The
// checks below hold every file of every such package to that list.

// completePackages is the closed set of restricted source packages.
var completePackages = map[string]bool{
	"internal/engine/kinds/complete":        true,
	"internal/engine/access/complete":       true,
	"internal/engine/projects/complete":     true,
	"internal/engine/rbac/complete":         true,
	"internal/engine/user/complete":         true,
	"internal/engine/provisioning/complete": true,
	"internal/engine/observe/complete":      true,
	"internal/engine/oidcclient/complete":   true,
	"internal/token/complete":               true,
}

// completeImportAllowlist is every import path a complete package's
// production files may name.
var completeImportAllowlist = map[string]bool{
	"context": true, "errors": true, "fmt": true, "maps": true, "slices": true, "sort": true, "strconv": true, "strings": true,
	modulePrefix + "internal/runtime/completion": true,
	modulePrefix + "internal/engine/kinds":       true,
	modulePrefix + "internal/clierr":             true,
}

// scanCompleteImports parses the production Go files of the package
// directory dir and returns one message per import outside the allowlist.
func scanCompleteImports(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	fset := token.NewFileSet()
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		require.NoError(t, err)
		for _, imp := range f.Imports {
			ip := strings.Trim(imp.Path.Value, `"`)
			if !completeImportAllowlist[ip] {
				out = append(out, filepath.ToSlash(p)+": imports "+ip)
			}
		}
	}
	slices.Sort(out)
	return out
}

// scanCompleteDeclarations type-checks the production files of the
// package directory dir and returns one message per declaration that
// could carry code from outside the package into a source: an exported
// function or method with a parameter whose type can carry behavior, an
// exported package variable of such a type (an owner could assign it),
// or a declared type whose underlying type can. A type carries behavior when
// it is, or contains through pointers, containers, struct fields, named
// types, aliases or type arguments, a function or an interface (the
// empty interface and error included): any such value can be a caller's
// code. A source's functions must be the package's own, so the surface's
// declaring-package check means what it says. The check is over types,
// not names, so context.Context, error, fmt.Stringer, an alias of the
// seam's Source and a struct field of context.CancelFunc are all seen.
func scanCompleteDeclarations(t *testing.T, dir string) []string {
	t.Helper()
	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedFiles}
	pkgs, err := packages.Load(cfg, "./"+dir)
	require.NoError(t, err)
	require.Len(t, pkgs, 1, "%s must be one package", dir)
	pkg := pkgs[0]
	for _, e := range pkg.Errors {
		require.Fail(t, "type-check "+dir, e.Error())
	}
	var out []string
	report := func(pos token.Pos, msg string) {
		out = append(out, filepath.ToSlash(pkg.Fset.Position(pos).String())+": "+msg)
	}
	scope := pkg.Types.Scope()
	for _, name := range scope.Names() {
		switch obj := scope.Lookup(name).(type) {
		case *types.TypeName:
			if carriesBehavior(obj.Type(), map[*types.Named]bool{}) {
				report(obj.Pos(), "type "+name+" can carry code: "+typeName(obj.Type().Underlying()))
			}
			if named, ok := obj.Type().(*types.Named); ok {
				for m := range named.Methods() {
					checkParams(m, name+"."+m.Name(), report)
				}
			}
		case *types.Func:
			checkParams(obj, name, report)
		case *types.Var:
			if obj.Exported() && carriesBehavior(obj.Type(), map[*types.Named]bool{}) {
				report(obj.Pos(), "exported var "+name+" is "+typeName(obj.Type())+", which can carry code")
			}
		}
	}
	slices.Sort(out)
	return out
}

// checkParams reports every parameter of an exported function or method
// whose type can carry behavior.
func checkParams(fn *types.Func, name string, report func(token.Pos, string)) {
	if !fn.Exported() {
		return
	}
	sig := fn.Type().(*types.Signature)
	for p := range sig.Params().Variables() {
		if carriesBehavior(p.Type(), map[*types.Named]bool{}) {
			report(p.Pos(), "exported "+name+" takes "+p.Name()+" "+typeName(p.Type())+", which can carry code")
		}
	}
}

// typeName renders t with package names rather than import paths, so a
// diagnostic reads as the source does.
func typeName(t types.Type) string {
	return types.TypeString(t, func(p *types.Package) string { return p.Name() })
}

// carriesBehavior reports whether a value of type t can hold code: a
// function, an interface, or anything reaching one.
func carriesBehavior(t types.Type, seen map[*types.Named]bool) bool {
	switch tt := types.Unalias(t).(type) {
	case *types.Signature, *types.Interface, *types.TypeParam:
		return true
	case *types.Named:
		if seen[tt] {
			return false
		}
		seen[tt] = true
		for arg := range tt.TypeArgs().Types() {
			if carriesBehavior(arg, seen) {
				return true
			}
		}
		return carriesBehavior(tt.Underlying(), seen)
	case *types.Pointer:
		return carriesBehavior(tt.Elem(), seen)
	case *types.Slice:
		return carriesBehavior(tt.Elem(), seen)
	case *types.Array:
		return carriesBehavior(tt.Elem(), seen)
	case *types.Chan:
		return carriesBehavior(tt.Elem(), seen)
	case *types.Map:
		return carriesBehavior(tt.Key(), seen) || carriesBehavior(tt.Elem(), seen)
	case *types.Struct:
		for f := range tt.Fields() {
			if carriesBehavior(f.Type(), seen) {
				return true
			}
		}
	}
	return false
}

// completePackagesOnDisk returns every package directory whose last
// element is "complete" under internal, as slash paths.
func completePackagesOnDisk(t *testing.T) []string {
	t.Helper()
	var out []string
	for pkg := range packagesOnDisk(t) {
		if path.Base(pkg) == "complete" && strings.HasPrefix(pkg, "internal/") {
			out = append(out, pkg)
		}
	}
	slices.Sort(out)
	return out
}

// TestCompletePackagesOnDiskSeeEveryOwner asserts the scans below look at
// every owner's complete package, the one of the sign-in commands under
// internal/token among them, not only those under internal/engine.
func TestCompletePackagesOnDiskSeeEveryOwner(t *testing.T) {
	t.Parallel()
	assert.Equal(t, slices.Sorted(maps.Keys(completePackages)), completePackagesOnDisk(t))
	assert.Contains(t, completePackagesOnDisk(t), "internal/token/complete")
}

// TestCompletePackagesAreTheClosedSet asserts every complete package in
// the tree is one of the named nine; a source package nobody reviewed
// into the set fails here.
func TestCompletePackagesAreTheClosedSet(t *testing.T) {
	t.Parallel()
	found := completePackagesOnDisk(t)
	require.NotEmpty(t, found, "no complete package on disk")
	for _, pkg := range found {
		assert.True(t, completePackages[pkg], "%s is a complete package outside the closed set", pkg)
	}
}

// TestSurfaceSourcePackagesEqualTheClosedSet asserts the set of packages
// BuildRoot accepts a source from (liveSourcePackages in
// internal/cli/livesource.go, as import paths) is exactly the closed set
// above, so the two lists cannot drift apart.
func TestSurfaceSourcePackagesEqualTheClosedSet(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "internal/cli/livesource.go", nil, 0)
	require.NoError(t, err)
	var surface []string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "liveSourcePackages" {
			return true
		}
		lit, ok := vs.Values[0].(*ast.CompositeLit)
		require.True(t, ok, "liveSourcePackages must be a composite literal")
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			require.True(t, ok)
			// Each key is modulePath + "/<path>": the path is the string
			// literal on the right of the concatenation.
			bin, ok := kv.Key.(*ast.BinaryExpr)
			require.True(t, ok, "key must be modulePath + a string literal")
			path, ok := bin.Y.(*ast.BasicLit)
			require.True(t, ok)
			surface = append(surface, strings.TrimPrefix(strings.Trim(path.Value, `"`), "/"))
		}
		return false
	})
	slices.Sort(surface)
	assert.Equal(t, slices.Sorted(maps.Keys(completePackages)), surface)
}

// TestCompletePackagesImportOnlyTheAllowlist asserts every production file
// of every complete package on disk imports nothing outside the allowlist.
// Test files are out of scope: they are not linked into the binary and
// need a test server and assertions.
func TestCompletePackagesImportOnlyTheAllowlist(t *testing.T) {
	t.Parallel()
	for _, pkg := range completePackagesOnDisk(t) {
		assert.Empty(t, scanCompleteImports(t, pkg), "%s imports outside the complete allowlist", pkg)
	}
}

// TestCompletePackagesDeclareNoBehaviorSlots asserts every complete
// package on disk has no exported function or method taking a value
// that can carry code, no exported variable that can hold one, and
// declares no type that can.
func TestCompletePackagesDeclareNoBehaviorSlots(t *testing.T) {
	t.Parallel()
	for _, pkg := range completePackagesOnDisk(t) {
		assert.Empty(t, scanCompleteDeclarations(t, pkg), "%s declares a slot outside code could fill", pkg)
	}
}

// fixtureRoot holds the fixture packages the two scans are proven on.
const fixtureRoot = "test/conformance/testdata/completeimports"

// messageOf strips the file position from a scan message: the path, and
// the line and column the declaration scan adds after it.
func messageOf(t *testing.T, line string) string {
	t.Helper()
	_, rest, ok := strings.Cut(line, ".go:")
	require.True(t, ok, line)
	return strings.TrimSpace(strings.TrimLeft(rest, "0123456789:"))
}

// TestCompleteDeclarationScanRejectsFixtures asserts the declaration scan
// accepts the clean fixture and rejects a constructor taking a Source, a
// function parameter, an interface parameter, each declared behavior
// type, and behavior hidden behind a name: context.Context, error,
// fmt.Stringer, an alias of Source, a struct with a context.CancelFunc
// field, a slice of functions, a map of interfaces, a pointer to a
// struct that carries one, and an exported variable of a function type.
func TestCompleteDeclarationScanRejectsFixtures(t *testing.T) {
	t.Parallel()
	assert.Empty(t, scanCompleteDeclarations(t, fixtureRoot+"/accepted"))
	tests := []struct {
		fixture string
		want    []string
	}{
		{fixture: "wrapper", want: []string{
			"exported Wrap takes src completion.Source, which can carry code",
		}},
		{fixture: "funcparam", want: []string{
			"exported Lister.Using takes l interface{List() []string}, which can carry code",
			"exported WithNames takes names func() []string, which can carry code",
		}},
		{fixture: "functype", want: []string{
			"type Namer can carry code: func() []string",
			"type Options can carry code: struct{Filter func(string) bool}",
			"type Reader can carry code: interface{Read() []string}",
		}},
		{fixture: "namedbehavior", want: []string{
			"exported WithAlias takes src complete.Src, which can carry code",
			"exported WithContext takes ctx context.Context, which can carry code",
			"exported WithError takes err error, which can carry code",
			"exported WithPointer takes c *complete.Cancel, which can carry code",
			"exported WithStringer takes s fmt.Stringer, which can carry code",
			"exported var ExternalHook is context.CancelFunc, which can carry code",
			"type Cancel can carry code: struct{Stop context.CancelFunc}",
			"type Filters can carry code: []func(string) bool",
			"type Lookup can carry code: map[string]fmt.Stringer",
			"type Src can carry code: struct{Claim func(req completion.Request) (completion.Requirements, bool); Complete func(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result}",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			t.Parallel()
			got := scanCompleteDeclarations(t, fixtureRoot+"/"+tt.fixture)
			var msgs []string
			for _, m := range got {
				msgs = append(msgs, messageOf(t, m))
			}
			slices.Sort(msgs)
			assert.Equal(t, tt.want, msgs, "%v", got)
		})
	}
}

// TestCompleteImportScanRejectsFixtures asserts the scan accepts a package
// that stays inside the allowlist and rejects each way out: a same-package
// helper that imports a client, a raw socket, an environment read, a file
// read, a process execution, the kube client, the engine's client ports,
// and the credential store.
func TestCompleteImportScanRejectsFixtures(t *testing.T) {
	t.Parallel()
	assert.Empty(t, scanCompleteImports(t, fixtureRoot+"/accepted"))
	tests := []struct {
		fixture string
		imports []string
	}{
		{fixture: "helperclient", imports: []string{modulePrefix + "internal/client/httpx"}},
		{fixture: "socket", imports: []string{"net"}},
		{fixture: "env", imports: []string{"os"}},
		{fixture: "file", imports: []string{"io/fs", "os"}},
		{fixture: "exec", imports: []string{"os/exec"}},
		{fixture: "kube", imports: []string{modulePrefix + "internal/client/kube"}},
		{fixture: "engineclient", imports: []string{modulePrefix + "internal/engine"}},
		{fixture: "credential", imports: []string{modulePrefix + "internal/config"}},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			t.Parallel()
			got := scanCompleteImports(t, fixtureRoot+"/"+tt.fixture)
			var imports []string
			for _, m := range got {
				imports = append(imports, strings.TrimPrefix(messageOf(t, m), "imports "))
			}
			slices.Sort(imports)
			assert.Equal(t, tt.imports, imports, "%v", got)
		})
	}
	// The helper case is rejected through its helper file, not the source file.
	got := scanCompleteImports(t, fixtureRoot+"/helperclient")
	require.Len(t, got, 1)
	assert.True(t, strings.HasPrefix(got[0], fixtureRoot+"/helperclient/helper.go:"), got[0])
}
