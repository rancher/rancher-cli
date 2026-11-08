package conformance

import (
	"go/ast"
	"go/token"
	"go/types"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

const constantFormatDir = "test/conformance/testdata/constantformat"

// fmtFormatArgument is the position of the format among the arguments of
// each printf-family function of fmt.
var fmtFormatArgument = map[string]int{"Sprintf": 0, "Errorf": 0, "Printf": 0, "Fprintf": 1, "Appendf": 1}

// formatArgument returns the position of the format among the arguments of
// the call and the callee's name, or -1 when the call is not of the printf
// family: a function of fmt named in fmtFormatArgument, or any called
// object (function, method, or variable that holds a function) whose
// signature has a parameter named format of type string followed by one of
// type ...any.
func formatArgument(info *types.Info, call *ast.CallExpr) (int, types.Object) {
	var obj types.Object
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		obj = info.Uses[fun]
	case *ast.SelectorExpr:
		obj = info.Uses[fun.Sel]
	}
	if fn, ok := obj.(*types.Func); ok && fn.Pkg() != nil && fn.Pkg().Path() == "fmt" {
		if i, ok := fmtFormatArgument[fn.Name()]; ok {
			return i, obj
		}
	}
	sig, ok := types.Unalias(info.TypeOf(call.Fun)).Underlying().(*types.Signature)
	if !ok || !sig.Variadic() || sig.Params().Len() < 2 {
		return -1, nil
	}
	params := sig.Params()
	n := params.Len()
	last, format := params.At(n-1), params.At(n-2)
	rest, ok := last.Type().(*types.Slice)
	if !ok || format.Name() != "format" {
		return -1, nil
	}
	if basic, ok := format.Type().(*types.Basic); !ok || basic.Kind() != types.String {
		return -1, nil
	}
	if iface, ok := rest.Elem().Underlying().(*types.Interface); !ok || !iface.Empty() {
		return -1, nil
	}
	if n-2 >= len(call.Args) {
		return -1, nil
	}
	return n - 2, obj
}

// formatFinding is one printf-family call whose format refers to an object
// of the remedy marker: where it is, and the function it is in as the
// package's path under the module and the function's name.
type formatFinding struct {
	function string
	position string
	object   string
}

// constantFormatFindings reports every printf-family call of pkg whose
// format argument refers to an object in targets, which a format must not:
// a format built from a variable is not a constant, and go vet stops
// reading it where other arguments follow. The forms it follows, in the
// format argument itself and in the code behind it (funcs holds the
// functions and methods of the module):
//   - a package-level variable in targets, of any type, and the roots (a
//     slice element, a map value, a struct field or a call of a function
//     value that is a target counts: the format refers to the variable);
//   - a local variable of the function (or of the package-level variable's
//     initializer) through the expressions assigned to it with :=, = or +=
//     or given to it by a var declaration, a function literal included;
//   - a function or method of the module, called in the format or held in a
//     variable, through the expressions its return statements return, and
//     through its named results at a return with no values;
//   - any of these however many steps apart.
//
// A format built through anything else, a function value passed in as a
// parameter or a call of an interface method, is not followed. It also
// returns the keys of the callees of the module that it found by their
// signature rather than as a function of fmt, each function once and each
// variable that holds a function once.
func constantFormatFindings(pkg *packages.Package, targets map[string]bool, funcs map[string]moduleFunction) (findings []formatFinding, byShape []string) {
	info := pkg.TypesInfo
	seenVars := map[types.Object]bool{}
	seenFuncs := map[string]bool{}
	for _, f := range pkg.Syntax {
		for _, decl := range f.Decls {
			var root ast.Node
			function := strings.TrimPrefix(pkg.PkgPath, modulePrefix) + ".<package level>"
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				root = d.Body
				function = strings.TrimPrefix(pkg.PkgPath, modulePrefix) + "." + d.Name.Name
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				root = d
			default:
				continue
			}
			locals := localAssignments(info, root)
			ast.Inspect(root, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				i, callee := formatArgument(info, call)
				if i < 0 || i >= len(call.Args) {
					return true
				}
				if callee != nil && callee.Pkg() != nil && strings.HasPrefix(callee.Pkg().Path(), modulePrefix) {
					if _, isFunc := callee.(*types.Func); isFunc && !seenFuncs[objectKey(callee)] {
						seenFuncs[objectKey(callee)] = true
						byShape = append(byShape, objectKey(callee))
					} else if !isFunc && !seenVars[callee] {
						seenVars[callee] = true
						byShape = append(byShape, objectKey(callee))
					}
				}
				ast.Inspect(call.Args[i], func(m ast.Node) bool {
					id, ok := m.(*ast.Ident)
					if !ok {
						return true
					}
					used := info.Uses[id]
					if used == nil || used.Pkg() == nil {
						return true
					}
					if slices.ContainsFunc(exprRefs(id, info, locals, funcs, map[any]bool{}), func(ref string) bool { return targets[ref] }) {
						findings = append(findings, formatFinding{function, pkg.Fset.Position(call.Pos()).String(), objectKey(used)})
					}
					return true
				})
				return true
			})
		}
	}
	return findings, byShape
}

// formatTargets are the objects a format must not refer to: the two roots,
// the reviewed list of variables built on them that hold a remedy command, and
// every variable the scan of pkgs finds built on them, of any type (a string,
// a function value, a slice, a map, a struct).
func formatTargets(pkgs []*packages.Package) map[string]bool {
	targets := map[string]bool{"internal/clierr.Rancher": true, "internal/clierr.RemedyMarker": true}
	for _, key := range builtOnRancherReviewed {
		targets[key] = true
	}
	members, carriers := varsBuiltOnRancher(pkgs)
	for _, found := range []map[string]bool{members, carriers} {
		for key := range found {
			targets[key] = true
		}
	}
	return targets
}

// TestNoFormatIsBuiltFromTheRemedyMarker asserts no printf-family call of
// the production tree takes a format that refers to clierr.Rancher,
// clierr.RemedyMarker, a package-level variable built on them (directly, or
// through a function of the module whose returned expression refers to
// them), or a local variable whose value refers to one of those, in a
// function or in the initializer of a package-level variable: the marker
// joins the text as an argument, or by a + outside the format. The family is
// the functions of fmt and every callee whose signature ends in format string
// and ...any, a closure bound to a variable included; the four of them in the
// module outside fmt are listed, so a callee the check stops seeing fails.
func TestNoFormatIsBuiltFromTheRemedyMarker(t *testing.T) {
	t.Parallel()
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	var findings []formatFinding
	var byShape []string
	targets := formatTargets(pkgs)
	funcs := moduleFunctions(pkgs)
	for _, pkg := range pkgs {
		f, s := constantFormatFindings(pkg, targets, funcs)
		findings = append(findings, f...)
		byShape = append(byShape, s...)
	}
	for _, f := range findings {
		t.Errorf("%s: a format in %s refers to %s", f.position, f.function, f.object)
	}
	slices.Sort(byShape)
	assert.Equal(t, []string{
		"credhelper.fail",
		"internal/cli.completionDebugf",
		"internal/telemetry.w",
		"internal/telemetry.w",
	}, byShape)
}

// constantFormatFixtures type-checks the fixture packages once.
var constantFormatFixtures = sync.OnceValues(func() (map[string]*packages.Package, error) {
	pkgs, err := loadPackages("./"+constantFormatDir+"/...", "")
	if err != nil {
		return nil, err
	}
	out := map[string]*packages.Package{}
	for _, pkg := range pkgs {
		out[path.Base(pkg.PkgPath)] = pkg
	}
	return out, nil
})

// constantFormatCases lists every fixture package and how many calls the
// check reports in it.
var constantFormatCases = []struct {
	fixture  string
	findings int
}{
	{"constants", 0},
	{"plusvariable", 1},
	{"startsmarked", 1},
	{"remedymarker", 1},
	{"closure", 1},
	{"variableinit", 1},
	{"markervariable", 1},
	{"othervariable", 0},
	{"localvariable", 1},
	{"variablefromfunction", 1},
	{"directcall", 1},
	{"localclosure", 1},
	{"functionvalue", 1},
	{"method", 1},
	{"namedresult", 1},
	{"slicevariable", 1},
	{"functionvaluecall", 1},
	{"structfield", 1},
	{"mapvalue", 1},
}

// TestConstantFormatScanFixtures asserts the check on type-checked fixture
// packages: it reports a format joined with a variable built on the marker, a
// format that starts with clierr.Rancher, the same through a closure bound to
// a variable, one in the initializer of a package-level variable, one
// through a variable built on clierr.RemedyMarker, one through a local
// variable whose value is clierr.Rancher, one through a package-level
// variable initialized by a function that returns text built on it, one
// through a variable initialized by a function held in a variable, by a
// method or by a function with a named result and a bare return, one that
// calls such a function in the format itself, and one through a local
// closure that returns it, and one through a package-level slice, a function
// value called in the format, a struct field or a map value built on it; it
// accepts the rewritten forms (the marker as an
// argument, or joined outside the format), a local variable of plain text and
// a variable of the same name in another package.
func TestConstantFormatScanFixtures(t *testing.T) {
	t.Parallel()
	pkgs, err := constantFormatFixtures()
	require.NoError(t, err)
	for _, tc := range constantFormatCases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			pkg, ok := pkgs[tc.fixture]
			require.True(t, ok, "fixture package %s", tc.fixture)
			fixture := []*packages.Package{pkg}
			findings, _ := constantFormatFindings(pkg, formatTargets(fixture), moduleFunctions(fixture))
			assert.Len(t, findings, tc.findings, "%v", findings)
		})
	}
}

// TestConstantFormatFixturesAreAllListed asserts every fixture package on
// disk is in the table above.
func TestConstantFormatFixturesAreAllListed(t *testing.T) {
	t.Parallel()
	pkgs, err := constantFormatFixtures()
	require.NoError(t, err)
	var listed, loaded []string
	for _, tc := range constantFormatCases {
		listed = append(listed, tc.fixture)
	}
	for name := range pkgs {
		loaded = append(loaded, name)
	}
	assert.ElementsMatch(t, listed, loaded)
}
