package conformance

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

const markerSourceDir = "test/conformance/testdata/markersource"

const (
	randV2Path     = "math/rand/v2"
	randV1Path     = "math/rand"
	cryptoRandPath = "crypto/rand"
)

// calledFunction returns the function or method a call names, or nil for a
// conversion, a builtin or a call of a function value.
func calledFunction(info *types.Info, call *ast.CallExpr) *types.Func {
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		fn, _ := info.Uses[fun].(*types.Func)
		return fn
	case *ast.SelectorExpr:
		fn, _ := info.Uses[fun.Sel].(*types.Func)
		return fn
	}
	return nil
}

// isPackageFunction reports whether fn is the function named name of the
// package at pkgPath, not a method.
func isPackageFunction(fn *types.Func, pkgPath, name string) bool {
	if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != pkgPath || fn.Name() != name {
		return false
	}
	return fn.Type().(*types.Signature).Recv() == nil
}

// markerSourceFindings reports where pkg (a package that declares the
// remedy marker the way internal/clierr does) does not take its marker from
// the one call of the top-level Uint64 of math/rand/v2:
//   - the variable RemedyMarker is initialized by markerFromBytes called on
//     the result of markerBytes;
//   - markerBytes calls the function Uint64 of math/rand/v2 once, and no
//     other function or method of math/rand/v2, math/rand or crypto/rand;
//   - the body of markerBytes is the array declaration, one
//     binary.LittleEndian.PutUint64(b[:], rand.Uint64()) whose second
//     argument is the call itself, and the return of the array;
//   - no file of the package imports math/rand or crypto/rand.
func markerSourceFindings(pkg *packages.Package) []string {
	var out []string
	report := func(pos token.Pos, format string, args ...any) {
		out = append(out, fmt.Sprintf("%s: %s", pkg.Fset.Position(pos), fmt.Sprintf(format, args...)))
	}
	info := pkg.TypesInfo
	var initializer ast.Expr
	var bytesFunc *ast.FuncDecl
	for _, f := range pkg.Syntax {
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == randV1Path || p == cryptoRandPath {
				report(imp.Pos(), "imports %s; the marker takes its bytes from %s only", p, randV2Path)
			}
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok && d.Tok == token.VAR {
						for i, name := range vs.Names {
							if name.Name == "RemedyMarker" && i < len(vs.Values) {
								initializer = vs.Values[i]
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.Name == "markerBytes" {
					bytesFunc = d
				}
			}
		}
	}

	calls := func(e ast.Expr, name string) *ast.CallExpr {
		call, ok := ast.Unparen(e).(*ast.CallExpr)
		if !ok {
			return nil
		}
		if fn := calledFunction(info, call); fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != pkg.PkgPath || fn.Name() != name {
			return nil
		}
		return call
	}
	if initializer == nil {
		out = append(out, pkg.PkgPath+": no variable RemedyMarker")
	} else if outer := calls(initializer, "markerFromBytes"); outer == nil || len(outer.Args) != 1 || calls(outer.Args[0], "markerBytes") == nil {
		report(initializer.Pos(), "RemedyMarker is not initialized by markerFromBytes(markerBytes())")
	}
	if bytesFunc == nil || bytesFunc.Body == nil {
		out = append(out, pkg.PkgPath+": no function markerBytes")
		return out
	}
	uint64Calls := 0
	ast.Inspect(bytesFunc.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn := calledFunction(info, call)
		if fn == nil || fn.Pkg() == nil {
			return true
		}
		switch fn.Pkg().Path() {
		case randV2Path:
			if isPackageFunction(fn, randV2Path, "Uint64") {
				uint64Calls++
			} else {
				report(call.Pos(), "markerBytes calls %s of %s; only the top-level Uint64 is allowed", fn.Name(), randV2Path)
			}
		case randV1Path, cryptoRandPath:
			report(call.Pos(), "markerBytes calls %s of %s", fn.Name(), fn.Pkg().Path())
		}
		return true
	})
	if uint64Calls != 1 {
		report(bytesFunc.Pos(), "markerBytes calls the top-level Uint64 of %s %d times; it must call it once", randV2Path, uint64Calls)
	}
	markerBytesBodyFindings(info, bytesFunc.Body, report)
	return out
}

// markerBytesBodyFindings reports where the body of markerBytes is not
// exactly three statements: the declaration of an array of 8 bytes, one
// binary.LittleEndian.PutUint64 of the whole array (b[:]) whose second
// argument is the call of the top-level Uint64 of math/rand/v2 itself, and
// the return of the array. A result that is shifted, multiplied, masked or
// copied into parts keeps fewer bits of entropy than the 64 the call gives.
func markerBytesBodyFindings(info *types.Info, body *ast.BlockStmt, report func(token.Pos, string, ...any)) {
	const want = "markerBytes must be the array declaration, one binary.LittleEndian.PutUint64(b[:], rand.Uint64()) and the return of the array"
	if len(body.List) != 3 {
		report(body.Pos(), "markerBytes holds %d statements; %s", len(body.List), want)
		return
	}
	var array types.Object
	if decl, ok := body.List[0].(*ast.DeclStmt); ok {
		if gd, ok := decl.Decl.(*ast.GenDecl); ok && gd.Tok == token.VAR && len(gd.Specs) == 1 {
			if vs := gd.Specs[0].(*ast.ValueSpec); len(vs.Names) == 1 && len(vs.Values) == 0 {
				array = info.Defs[vs.Names[0]]
			}
		}
	}
	if array == nil {
		report(body.List[0].Pos(), "the first statement of markerBytes is not the declaration of the array; %s", want)
		return
	}
	if arr, ok := array.Type().Underlying().(*types.Array); !ok || arr.Len() != 8 {
		report(body.List[0].Pos(), "the array of markerBytes is not 8 bytes long")
	}
	isArray := func(e ast.Expr) bool {
		id, ok := ast.Unparen(e).(*ast.Ident)
		return ok && info.Uses[id] == array
	}
	put, _ := body.List[1].(*ast.ExprStmt)
	var call *ast.CallExpr
	if put != nil {
		call, _ = put.X.(*ast.CallExpr)
	}
	if call == nil || !isLittleEndianPutUint64(info, call) || len(call.Args) != 2 {
		report(body.List[1].Pos(), "the second statement of markerBytes is not binary.LittleEndian.PutUint64 of the array; %s", want)
		return
	}
	if whole, ok := call.Args[0].(*ast.SliceExpr); !ok || !isArray(whole.X) || whole.Low != nil || whole.High != nil || whole.Max != nil {
		report(call.Args[0].Pos(), "the first argument of PutUint64 in markerBytes is not the whole array, b[:]")
	}
	if draw, ok := call.Args[1].(*ast.CallExpr); !ok || len(draw.Args) != 0 || !isPackageFunction(calledFunction(info, draw), randV2Path, "Uint64") {
		report(call.Args[1].Pos(), "the second argument of PutUint64 in markerBytes is not the call of Uint64 of %s itself; a value derived from it keeps fewer bits", randV2Path)
	}
	if ret, ok := body.List[2].(*ast.ReturnStmt); !ok || len(ret.Results) != 1 || !isArray(ret.Results[0]) {
		report(body.List[2].Pos(), "the last statement of markerBytes does not return the array; %s", want)
	}
}

// isLittleEndianPutUint64 reports whether call is
// binary.LittleEndian.PutUint64 of encoding/binary.
func isLittleEndianPutUint64(info *types.Info, call *ast.CallExpr) bool {
	fn := calledFunction(info, call)
	if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != "encoding/binary" || fn.Name() != "PutUint64" {
		return false
	}
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	order, ok := ast.Unparen(sel.X).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	v, ok := info.Uses[order.Sel].(*types.Var)
	return ok && v.Pkg() != nil && v.Pkg().Path() == "encoding/binary" && v.Name() == "LittleEndian"
}

// TestRemedyMarkerComesFromTheRuntimeRandomSource asserts the marker of
// internal/clierr is initialized by markerFromBytes(markerBytes()), that
// markerBytes calls the top-level Uint64 of math/rand/v2 and nothing else of
// math/rand/v2, math/rand or crypto/rand, and that no non-test file of the
// package imports math/rand or crypto/rand: a generator built with rand.New
// could be seeded, and the system random source of crypto/rand ends the
// process when it fails.
func TestRemedyMarkerComesFromTheRuntimeRandomSource(t *testing.T) {
	t.Parallel()
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	var clierr *packages.Package
	for _, pkg := range pkgs {
		if pkg.PkgPath == clierrPath {
			clierr = pkg
		}
	}
	require.NotNil(t, clierr, "package internal/clierr was not loaded")
	assert.Empty(t, markerSourceFindings(clierr))
}

// markerSourceFixtures type-checks the fixture packages once.
var markerSourceFixtures = sync.OnceValues(func() (map[string]*packages.Package, error) {
	pkgs, err := loadPackages("./" + markerSourceDir + "/...")
	if err != nil {
		return nil, err
	}
	out := map[string]*packages.Package{}
	for _, pkg := range pkgs {
		out[path.Base(pkg.PkgPath)] = pkg
	}
	return out, nil
})

// markerSourceCases lists every fixture package and what the check says
// about it: the text a finding holds, or none for the package that does it
// right.
var markerSourceCases = []struct {
	fixture string
	want    string
}{
	{"good", ""},
	{"pcg", "markerBytes calls New of math/rand/v2"},
	{"fixed", "markerBytes calls the top-level Uint64 of math/rand/v2 0 times"},
	{"otherpackage", "markerBytes calls the top-level Uint64 of math/rand/v2 0 times"},
	{"cryptorand", "imports crypto/rand"},
	{"oldrand", "imports math/rand;"},
	{"initializer", "RemedyMarker is not initialized by markerFromBytes(markerBytes())"},
	{"spread", "the second argument of PutUint64 in markerBytes is not the call of Uint64"},
	{"repeated", "markerBytes holds 4 statements"},
}

// TestRemedyMarkerSourceCheckFixtures asserts the check on type-checked
// fixture packages: it accepts the source that calls the top-level Uint64
// of math/rand/v2, and reports a seeded generator built with rand.New, bytes
// that are fixed, a Uint64 of another package, crypto/rand, math/rand, an
// initializer that does not call markerBytes, a result of Uint64 spread over
// the array by a multiplier (32 bits of entropy) and a 16-bit value copied
// into each 2-byte part of it.
func TestRemedyMarkerSourceCheckFixtures(t *testing.T) {
	t.Parallel()
	pkgs, err := markerSourceFixtures()
	require.NoError(t, err)
	for _, tc := range markerSourceCases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			pkg, ok := pkgs[tc.fixture]
			require.True(t, ok, "fixture package %s", tc.fixture)
			findings := markerSourceFindings(pkg)
			if tc.want == "" {
				assert.Empty(t, findings)
				return
			}
			require.NotEmpty(t, findings)
			assert.Contains(t, strings.Join(findings, "\n"), tc.want)
		})
	}
}

// TestRemedyMarkerSourceFixturesAreAllListed asserts every fixture package
// on disk is in the table above, and the one that stands in for another
// package's Uint64 is the only one outside it.
func TestRemedyMarkerSourceFixturesAreAllListed(t *testing.T) {
	t.Parallel()
	pkgs, err := markerSourceFixtures()
	require.NoError(t, err)
	listed := map[string]bool{"fakerand": true}
	for _, tc := range markerSourceCases {
		listed[tc.fixture] = true
	}
	for name := range pkgs {
		assert.True(t, listed[name], "fixture %s has no row", name)
	}
	assert.Len(t, pkgs, len(listed))
}
