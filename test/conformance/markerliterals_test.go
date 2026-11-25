package conformance

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

const markerLiteralsDir = "test/conformance/testdata/markerliterals"

// markerFirstRune and markerLastRune bound the five characters a remedy
// marker is made of: U+2060 to U+2064.
const (
	markerFirstRune = 0x2060
	markerLastRune  = 0x2064
)

// markerVariables are the package variables of clierr that hold marker
// characters, declared once and never assigned: the remedy marker, the remedy
// command that starts with it, and the removal mark.
var markerVariables = []string{"RemedyMarker", "Rancher", "RemovalMark"}

// removalMarkName is the variable of markerVariables that code outside
// clierr may name only where markerRules.markInitializer says.
const removalMarkName = "RemovalMark"

// markerRules lists what the check allows where it refuses the same thing
// everywhere else.
type markerRules struct {
	// literalFile reports whether a file may spell the marker characters.
	literalFile func(file string) bool
	// declaringFile reports whether a file may declare a variable of
	// markerVariables.
	declaringFile func(variable, file string) bool
	// markInitializer reports whether the package-level variable named
	// variable in the package at pkgPath may be initialized from the removal
	// mark. That initializer is the only place code outside the package that
	// declares the mark may name it.
	markInitializer func(pkgPath, variable string) bool
}

// isMarkerRune reports whether r is one of the five marker characters.
func isMarkerRune(r rune) bool { return r >= markerFirstRune && r <= markerLastRune }

// isMarkerVariable reports whether obj is a package-level variable of
// markerVariables of the package at markerPkg.
func isMarkerVariable(obj types.Object, markerPkg string) bool {
	v, ok := obj.(*types.Var)
	if !ok || v.Pkg() == nil || v.Pkg().Path() != markerPkg || v.Parent() != v.Pkg().Scope() {
		return false
	}
	return slices.Contains(markerVariables, v.Name())
}

// markerLiteralFindings reports, for the non-test files of pkg other than
// those rules.literalFile accepts:
//   - an expression whose constant value is a string that holds one of the
//     five marker characters: a literal, a concatenation of literals, a
//     byte escape or a named constant;
//   - an expression whose constant value is an integer from 0x2060 to
//     0x2064, of any type: a rune conversion, an argument of WriteRune,
//     AppendRune or EncodeRune, a rune in a composite literal, folded
//     arithmetic;
//   - a byte composite literal that holds the UTF-8 bytes of one of them.
//
// In every file, allowed or not, it reports an assignment, an increment, a
// range assignment or the address of a variable of markerVariables of
// markerPkg, and a declaration of one in a file rules.declaringFile refuses.
// For markerPkg itself it reports a variable not declared exactly once in the
// files that may declare it. For any other package it reports a reference to
// the removal mark except in the initializer of a package-level variable that
// rules.markInitializer accepts, and returns the number of references it
// accepted.
func markerLiteralFindings(pkg *packages.Package, markerPkg string, rules markerRules) ([]string, int) {
	info := pkg.TypesInfo
	seen := map[string]bool{}
	var out []string
	report := func(pos token.Pos, format string, args ...any) {
		p := pkg.Fset.Position(pos)
		key := fmt.Sprintf("%s:%d: %s", p.Filename, p.Line, fmt.Sprintf(format, args...))
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	declared := map[string]int{}
	accepted := 0
	for _, f := range pkg.Syntax {
		filename := pkg.Fset.Position(f.Pos()).Filename
		allowed := rules.literalFile(filename)
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			if id, ok := n.(*ast.Ident); ok {
				if obj := info.Defs[id]; isMarkerVariable(obj, markerPkg) {
					if rules.declaringFile(id.Name, filename) {
						declared[id.Name]++
					} else {
						report(id.Pos(), "declares %s outside the one file that may", id.Name)
					}
				}
				if isMarkerVariable(info.Uses[id], markerPkg) && assignedOrAddressed(stack) {
					report(id.Pos(), "assigns or takes the address of %s", id.Name)
				}
				if id.Name == removalMarkName && pkg.PkgPath != markerPkg && isMarkerVariable(info.Uses[id], markerPkg) {
					if initializesMarkCopy(stack, pkg.PkgPath, rules.markInitializer) {
						accepted++
					} else {
						report(id.Pos(), "names %s outside the package that declares it and outside the initializer of the one variable that may copy it", id.Name)
					}
				}
			}
			expr, ok := n.(ast.Expr)
			if !ok || allowed {
				return true
			}
			if tv, ok := info.Types[expr]; ok && tv.Value != nil {
				switch tv.Value.Kind() {
				case constant.String:
					if strings.ContainsFunc(constant.StringVal(tv.Value), isMarkerRune) {
						report(expr.Pos(), "a constant string holds a character of the remedy marker")
					}
				case constant.Int, constant.Float:
					if i := constant.ToInt(tv.Value); i.Kind() == constant.Int &&
						constant.Compare(i, token.GEQ, constant.MakeInt64(markerFirstRune)) &&
						constant.Compare(i, token.LEQ, constant.MakeInt64(markerLastRune)) {
						report(expr.Pos(), "an integer constant is a character of the remedy marker")
					}
				}
			}
			if lit, ok := n.(*ast.CompositeLit); ok && holdsMarkerBytes(info, lit) {
				report(lit.Pos(), "a byte literal holds the UTF-8 bytes of a character of the remedy marker")
			}
			return true
		})
	}
	if pkg.PkgPath == markerPkg {
		for _, name := range markerVariables {
			if declared[name] != 1 {
				out = append(out, fmt.Sprintf("%s: %s is declared %d times in the files that may declare it; it must be once", markerPkg, name, declared[name]))
			}
		}
	}
	return out, accepted
}

// initializesMarkCopy reports whether the identifier at the top of stack is
// in the initializer of a package-level variable with one name that the rules
// allow for the package at pkgPath. A variable declared inside a function, and
// a declaration with several names, are refused.
func initializesMarkCopy(stack []ast.Node, pkgPath string, accepts func(pkgPath, variable string) bool) bool {
	for i := len(stack) - 1; i >= 2; i-- {
		spec, ok := stack[i].(*ast.ValueSpec)
		if !ok {
			continue
		}
		decl, ok := stack[i-1].(*ast.GenDecl)
		if !ok || decl.Tok != token.VAR {
			return false
		}
		if _, ok := stack[i-2].(*ast.File); !ok {
			return false
		}
		return len(spec.Names) == 1 && accepts(pkgPath, spec.Names[0].Name)
	}
	return false
}

// assignedOrAddressed reports whether the identifier at the top of stack,
// or the selector it ends, is assigned to, incremented, assigned by a range
// clause or has its address taken.
func assignedOrAddressed(stack []ast.Node) bool {
	top := len(stack) - 1
	self := stack[top]
	i := top - 1
	if i >= 0 {
		if sel, ok := stack[i].(*ast.SelectorExpr); ok && sel.Sel == self {
			self, i = sel, i-1
		}
	}
	for i >= 0 {
		if paren, ok := stack[i].(*ast.ParenExpr); ok && paren.X == self {
			self, i = paren, i-1
			continue
		}
		break
	}
	if i < 0 {
		return false
	}
	switch p := stack[i].(type) {
	case *ast.AssignStmt:
		return slices.Contains(p.Lhs, self.(ast.Expr))
	case *ast.IncDecStmt:
		return p.X == self
	case *ast.UnaryExpr:
		return p.Op == token.AND && p.X == self
	case *ast.RangeStmt:
		return p.Tok == token.ASSIGN && (p.Key == self || p.Value == self)
	}
	return false
}

// holdsMarkerBytes reports whether lit is a composite literal of bytes that
// holds, in a row, 0xe2, 0x81 and one of 0xa0 to 0xa4.
func holdsMarkerBytes(info *types.Info, lit *ast.CompositeLit) bool {
	var elem types.Type
	switch t := info.TypeOf(lit).Underlying().(type) {
	case *types.Slice:
		elem = t.Elem()
	case *types.Array:
		elem = t.Elem()
	default:
		return false
	}
	if basic, ok := elem.Underlying().(*types.Basic); !ok || basic.Kind() != types.Uint8 {
		return false
	}
	var values []int64
	for _, e := range lit.Elts {
		v := int64(-1)
		if tv, ok := info.Types[e]; ok && tv.Value != nil && tv.Value.Kind() == constant.Int {
			if n, exact := constant.Int64Val(tv.Value); exact {
				v = n
			}
		}
		values = append(values, v)
	}
	for i := 0; i+2 < len(values); i++ {
		if values[i] == 0xe2 && values[i+1] == 0x81 && values[i+2] >= 0xa0 && values[i+2] <= 0xa4 {
			return true
		}
	}
	return false
}

// isClierrRemedyFile reports whether name is internal/clierr/remedy.go, the
// one file of the tree that may spell the marker's characters.
func isClierrRemedyFile(name string) bool {
	return strings.HasSuffix(filepath.ToSlash(name), "/internal/clierr/remedy.go")
}

// isClierrRemovalMarkFile reports whether name is
// internal/clierr/removalmark.go, the one file that may declare RemovalMark.
func isClierrRemovalMarkFile(name string) bool {
	return strings.HasSuffix(filepath.ToSlash(name), "/internal/clierr/removalmark.go")
}

// productionMarkerRules are the rules for the module: only remedy.go spells
// the marker characters, RemedyMarker and Rancher are declared in remedy.go
// and RemovalMark in removalmark.go, and the one variable that may copy
// RemovalMark is removalMark of internal/runtime, which the writer of a
// result's output uses to remove the mark.
var productionMarkerRules = markerRules{
	literalFile: isClierrRemedyFile,
	declaringFile: func(variable, file string) bool {
		if variable == removalMarkName {
			return isClierrRemovalMarkFile(file)
		}
		return isClierrRemedyFile(file)
	},
	markInitializer: func(pkgPath, variable string) bool {
		return pkgPath == runtimePath && variable == "removalMark"
	},
}

// TestNoProductionFileSpellsTheMarkerCharacters asserts that no non-test
// file of the module outside internal/clierr/remedy.go holds a constant
// string with one of U+2060 to U+2064, an integer constant from 0x2060 to
// 0x2064 or a byte literal of their UTF-8 form, that RemedyMarker, Rancher
// and RemovalMark are each declared once, in the file that may, and never
// assigned or addressed, and that no file outside internal/clierr names
// RemovalMark except the initializer of removalMark in internal/runtime: a
// hint with a fixed marker would be expanded for any text that had the same
// characters, and a builder that writes the removal mark adds the filter note
// where no filter removed anything. The check reads the non-test files that
// the build for this machine's operating system compiles, with no build tags,
// as the other marker rules do.
func TestNoProductionFileSpellsTheMarkerCharacters(t *testing.T) {
	t.Parallel()
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	var findings []string
	scanned, allowedFiles, markFiles, markCopies := 0, 0, 0, 0
	for _, pkg := range pkgs {
		for _, f := range pkg.Syntax {
			scanned++
			name := pkg.Fset.Position(f.Pos()).Filename
			if isClierrRemedyFile(name) {
				allowedFiles++
			}
			if isClierrRemovalMarkFile(name) {
				markFiles++
			}
		}
		found, copies := markerLiteralFindings(pkg, clierrPath, productionMarkerRules)
		findings = append(findings, found...)
		markCopies += copies
	}
	require.Positive(t, scanned)
	require.Equal(t, 1, allowedFiles, "internal/clierr/remedy.go was not scanned")
	require.Equal(t, 1, markFiles, "internal/clierr/removalmark.go was not scanned")
	require.Equal(t, 1, markCopies, "the one reference to RemovalMark outside internal/clierr is the initializer of removalMark in internal/runtime: a different count means that reference moved or a rule accepts too much")
	slices.Sort(findings)
	for _, f := range findings {
		t.Error(f)
	}
}

// markerLiteralsFixtures type-checks the fixture packages once.
var markerLiteralsFixtures = sync.OnceValues(func() (map[string]*packages.Package, error) {
	pkgs, err := loadPackages("./"+markerLiteralsDir+"/...", "")
	if err != nil {
		return nil, err
	}
	out := map[string]*packages.Package{}
	for _, pkg := range pkgs {
		out[path.Base(pkg.PkgPath)] = pkg
	}
	return out, nil
})

// markerLiteralsAllowed is the one file of a fixture package that may spell
// the marker's characters and declare its variables.
func markerLiteralsAllowed(name string) bool { return filepath.Base(name) == "allowed.go" }

// markerLiteralsRules are the rules for a fixture package: the one allowed
// file, and removalMarkCopy as the one variable that may copy RemovalMark.
var markerLiteralsRules = markerRules{
	literalFile:     markerLiteralsAllowed,
	declaringFile:   func(_, file string) bool { return markerLiteralsAllowed(file) },
	markInitializer: func(_, variable string) bool { return variable == "removalMarkCopy" },
}

// markerLiteralsCases lists every fixture package and the text each finding
// of the check holds, in any order; none for a package it accepts.
var markerLiteralsCases = []struct {
	fixture string
	// own is true for a package that declares the two marker variables
	// itself, which the check then reads as the package of the marker.
	own  bool
	want []string
}{
	{"clean", true, nil},
	{"literal", false, []string{"a constant string holds a character"}},
	{"escapes", false, []string{"a constant string holds a character"}},
	{"namedconst", false, []string{"a constant string holds a character", "a constant string holds a character"}},
	{"runeconversion", false, []string{"an integer constant is a character", "a constant string holds a character"}},
	{"foldedinteger", false, []string{"an integer constant is a character"}},
	{"appendrune", false, []string{"an integer constant is a character"}},
	{"writerune", false, []string{"an integer constant is a character"}},
	{"bytes", false, []string{"a byte literal holds the UTF-8 bytes"}},
	{"assigned", false, []string{"assigns or takes the address of Rancher"}},
	{"addressed", false, []string{"assigns or takes the address of RemedyMarker"}},
	{"declaredoutside", true, []string{"declares Rancher outside the one file that may", "Rancher is declared 0 times"}},
	{"assignedinside", true, []string{"assigns or takes the address of Rancher"}},
	{"markbuilder", false, []string{"names RemovalMark outside the package that declares it"}},
	{"markassigned", false, []string{"assigns or takes the address of RemovalMark", "names RemovalMark outside the package that declares it"}},
	{"markaddressed", false, []string{"assigns or takes the address of RemovalMark", "names RemovalMark outside the package that declares it"}},
	{"markalias", false, []string{"names RemovalMark outside the package that declares it"}},
	{"markdot", false, []string{"names RemovalMark outside the package that declares it"}},
	{"markcopy", false, nil},
	{"markcopyrefused", false, []string{"names RemovalMark outside the package that declares it", "names RemovalMark outside the package that declares it", "names RemovalMark outside the package that declares it"}},
	{"markdeclaredoutside", true, []string{"declares RemovalMark outside the one file that may", "RemovalMark is declared 0 times"}},
}

// TestMarkerLiteralScanFixtures asserts the check on type-checked fixture
// packages: it accepts a package that only reads the marker, with a local
// variable of the same name and integers beside the range, and reports each
// refused form once.
func TestMarkerLiteralScanFixtures(t *testing.T) {
	t.Parallel()
	pkgs, err := markerLiteralsFixtures()
	require.NoError(t, err)
	for _, tc := range markerLiteralsCases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			pkg, ok := pkgs[tc.fixture]
			require.True(t, ok, "fixture package %s", tc.fixture)
			markerPkg := clierrPath
			if tc.own {
				markerPkg = pkg.PkgPath
			}
			findings, _ := markerLiteralFindings(pkg, markerPkg, markerLiteralsRules)
			require.Len(t, findings, len(tc.want), "%q", findings)
			for _, want := range tc.want {
				assert.True(t, slices.ContainsFunc(findings, func(f string) bool { return strings.Contains(f, want) }), "%q is not in %q", want, findings)
			}
		})
	}
}

// TestMarkerDeclaringFilesAndCopies asserts the production rules: RemovalMark
// is declared in removalmark.go and the other two variables in remedy.go,
// each in no other file, and only removalMark of internal/runtime may copy
// RemovalMark.
func TestMarkerDeclaringFilesAndCopies(t *testing.T) {
	t.Parallel()
	rules := productionMarkerRules
	assert.True(t, rules.declaringFile("RemovalMark", "/r/internal/clierr/removalmark.go"))
	assert.False(t, rules.declaringFile("RemovalMark", "/r/internal/clierr/remedy.go"))
	assert.True(t, rules.declaringFile("Rancher", "/r/internal/clierr/remedy.go"))
	assert.True(t, rules.declaringFile("RemedyMarker", "/r/internal/clierr/remedy.go"))
	assert.False(t, rules.declaringFile("Rancher", "/r/internal/clierr/removalmark.go"))
	assert.True(t, rules.markInitializer(runtimePath, "removalMark"))
	assert.False(t, rules.markInitializer(runtimePath, "other"))
	assert.False(t, rules.markInitializer(modulePrefix+"internal/engine/access", "removalMark"))
}

// TestMarkerLiteralFixturesAreAllListed asserts every fixture package on
// disk is in the table above.
func TestMarkerLiteralFixturesAreAllListed(t *testing.T) {
	t.Parallel()
	pkgs, err := markerLiteralsFixtures()
	require.NoError(t, err)
	var listed, loaded []string
	for _, tc := range markerLiteralsCases {
		listed = append(listed, tc.fixture)
	}
	for name := range pkgs {
		loaded = append(loaded, name)
	}
	assert.ElementsMatch(t, listed, loaded)
}
