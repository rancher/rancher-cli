package conformance

import (
	"go/ast"
	"go/token"
	"go/types"
	"maps"
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
	expansionSitesDir = "test/conformance/testdata/expansionsites"
)

// expansionMethods are the methods of runtime.IO that write the remedy
// commands of a text for the invocation. ExpandLines is for the text
// output, which has many lines.
var expansionMethods = []string{"ExpandError", "ExpandText", "expandAll", "ExpandLines"}

// expansionSite is the enclosing function and the callee of one place that
// expands remedy commands.
type expansionSite struct{ function, callee string }

func (s expansionSite) String() string { return s.function + " " + s.callee }

// enclosingFunctionKey names the function a declaration declares: the package's path
// under the module, the receiver's type for a method, and the name.
func enclosingFunctionKey(pkg *packages.Package, fd *ast.FuncDecl) string {
	key := strings.TrimPrefix(pkg.PkgPath, modulePrefix) + "."
	if fd.Recv != nil && len(fd.Recv.List) == 1 {
		expr := fd.Recv.List[0].Type
		if star, ok := expr.(*ast.StarExpr); ok {
			expr = star.X
		}
		if id, ok := expr.(*ast.Ident); ok {
			key += id.Name + "."
		}
	}
	return key + fd.Name.Name
}

// isIOMethod reports whether fn is a method of runtime.IO.
func isIOMethod(fn *types.Func) bool {
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return false
	}
	recv := sig.Recv().Type()
	if ptr, ok := recv.(*types.Pointer); ok {
		recv = ptr.Elem()
	}
	named, ok := types.Unalias(recv).(*types.Named)
	return ok && named.Obj().Name() == "IO" && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == runtimePath
}

// isEmptyStringLiteral reports whether e is the literal "".
func isEmptyStringLiteral(e ast.Expr) bool {
	lit, ok := ast.Unparen(e).(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	s, err := strconv.Unquote(lit.Value)
	return err == nil && s == ""
}

// expansionDefinition reports whether the function named name of the file
// is one whose body defines how a text is expanded: the methods and
// helpers of internal/runtime/remedy.go and ExpandRemedies of
// internal/clierr/remedy.go.
func expansionDefinition(file, name string) bool {
	file = filepath.ToSlash(file)
	switch {
	case strings.HasSuffix(file, "/internal/runtime/remedy.go"):
		return slices.Contains([]string{"ExpandText", "ExpandLines", "ExpandError", "expandAll", "plainRemedies"}, name)
	case strings.HasSuffix(file, "/internal/clierr/remedy.go"):
		return name == "ExpandRemedies"
	}
	return false
}

// expansionSites returns, for the non-test files of pkgs, every reference
// (a call or a method value) to a method of runtime.IO in expansionMethods
// and every call of clierr.ExpandRemedies whose second argument is not the
// literal "", counted by site; and apart from them the calls of
// ExpandRemedies with the literal "", counted by the function that makes
// them. The bodies of the definitions are left out. Objects are matched as
// go/types resolves them, never by name alone.
func expansionSites(pkgs []*packages.Package) (sites map[expansionSite]int, empty map[string]int) {
	sites, empty = map[expansionSite]int{}, map[string]int{}
	for _, pkg := range pkgs {
		info := pkg.TypesInfo
		for _, f := range pkg.Syntax {
			file := pkg.Fset.Position(f.Pos()).Filename
			for _, decl := range f.Decls {
				function := strings.TrimPrefix(pkg.PkgPath, modulePrefix) + ".<package level>"
				if fd, ok := decl.(*ast.FuncDecl); ok {
					if expansionDefinition(file, fd.Name.Name) {
						continue
					}
					function = enclosingFunctionKey(pkg, fd)
				}
				calls := map[*ast.Ident]bool{}
				ast.Inspect(decl, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.CallExpr:
						fn := calledFunction(info, n)
						if !isPackageFunction(fn, clierrPath, "ExpandRemedies") {
							return true
						}
						switch fun := ast.Unparen(n.Fun).(type) {
						case *ast.Ident:
							calls[fun] = true
						case *ast.SelectorExpr:
							calls[fun.Sel] = true
						}
						if len(n.Args) == 2 && isEmptyStringLiteral(n.Args[1]) {
							empty[function]++
						} else {
							sites[expansionSite{function, "ExpandRemedies"}]++
						}
					case *ast.SelectorExpr:
						if fn, ok := info.Uses[n.Sel].(*types.Func); ok && slices.Contains(expansionMethods, fn.Name()) && isIOMethod(fn) {
							sites[expansionSite{function, fn.Name()}]++
						}
					}
					return true
				})
				// A reference to ExpandRemedies that is not a call is a site too.
				ast.Inspect(decl, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok && !calls[id] {
						if fn, ok := info.Uses[id].(*types.Func); ok && isPackageFunction(fn, clierrPath, "ExpandRemedies") {
							sites[expansionSite{function, "ExpandRemedies"}]++
						}
					}
					return true
				})
			}
		}
	}
	return sites, empty
}

// expansionSiteList is every place the production tree writes remedy
// commands for the invocation, one entry for each occurrence. The rows that
// drive them are in internal/runtime, with their own copy of these keys and
// counts.
var expansionSiteList = map[string]int{
	"internal/runtime.runRead ExpandText":           1,
	"internal/runtime.renderResult expandAll":       2,
	"internal/runtime.renderResult ExpandText":      2,
	"internal/runtime.renderResult ExpandLines":     2,
	"internal/runtime.renderEffect expandAll":       1,
	"internal/runtime.renderEffect ExpandLines":     1,
	"internal/runtime.renderErrDrained ExpandError": 1,
	"internal/runtime.renderErrDrained expandAll":   1,
	"internal/cli.renderDepsError ExpandError":      1,
}

// emptyPrefixAllowlist is every function that expands remedy commands with
// the empty prefix, each with the reason: the text goes to a file or a
// report, where the command is written plain.
var emptyPrefixAllowlist = map[string]string{
	"internal/token.Deletion.tlsMessage":        "the message is data in the revoke report, which no invocation expands; the command is written plain",
	"internal/engine/observe.errorStatus":       "the status is written into the support bundle manifest, which no invocation expands; the command is written plain",
	"internal/engine/observe.logOutcome.status": "the status is written into the support bundle manifest, which no invocation expands; the command is written plain",
}

// TestEveryPlaceThatExpandsRemediesHasARow asserts the places the production
// tree expands remedy commands for the invocation are exactly the listed
// ones, with their counts: a call or method value of IO.ExpandError,
// IO.ExpandText, IO.expandAll or IO.ExpandLines, or of clierr.ExpandRemedies
// with a prefix, outside the definitions. A new place fails until it is
// listed here and has rows in internal/runtime (TestRuntimeRowsMatchTheScans
// compares the two lists by key: it cannot tell whether the drive of a row
// runs the site its key names, so each drive must run that site); the list
// also fails for a place that is gone.
func TestEveryPlaceThatExpandsRemediesHasARow(t *testing.T) {
	t.Parallel()
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	sites, _ := expansionSites(pkgs)
	got := map[string]int{}
	for site, n := range sites {
		got[site.String()] = n
	}
	assert.Equal(t, expansionSiteList, got)
}

// TestRemedyExpansionWithNoPrefixIsListed asserts every function that calls
// clierr.ExpandRemedies with the empty prefix is in the allowlist, and that
// every allowlist entry still calls it.
func TestRemedyExpansionWithNoPrefixIsListed(t *testing.T) {
	t.Parallel()
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	_, empty := expansionSites(pkgs)
	for function := range empty {
		_, ok := emptyPrefixAllowlist[function]
		assert.True(t, ok, "%s expands remedy commands with no prefix and is not in the allowlist", function)
	}
	for function, why := range emptyPrefixAllowlist {
		assert.Contains(t, empty, function, "allowlist entry %s (%s) matches nothing; remove it", function, why)
	}
}

// expansionFixtures type-checks the fixture packages once.
var expansionFixtures = sync.OnceValues(func() (map[string]*packages.Package, error) {
	pkgs, err := loadPackages("./" + expansionSitesDir + "/...")
	if err != nil {
		return nil, err
	}
	out := map[string]*packages.Package{}
	for _, pkg := range pkgs {
		out[path.Base(pkg.PkgPath)] = pkg
	}
	return out, nil
})

// expansionFixtureCases lists every fixture package, the sites the scan
// finds in it and the functions it finds a call with the empty prefix in.
var expansionFixtureCases = []struct {
	fixture string
	sites   map[string]int
	empty   map[string]int
}{
	{"none", map[string]int{}, map[string]int{}},
	{"methods", map[string]int{
		expansionSitesDir + "/methods.call ExpandText":     1,
		expansionSitesDir + "/methods.value ExpandError":   1,
		expansionSitesDir + "/methods.embedded ExpandText": 1,
		expansionSitesDir + "/methods.lines ExpandLines":   1,
	}, map[string]int{}},
	{"othertype", map[string]int{}, map[string]int{}},
	{"prefixed", map[string]int{
		expansionSitesDir + "/prefixed.expand ExpandRemedies":          1,
		expansionSitesDir + "/prefixed.reference ExpandRemedies":       1,
		expansionSitesDir + "/prefixed.<package level> ExpandRemedies": 1,
	}, map[string]int{}},
	{"unprefixed", map[string]int{}, map[string]int{expansionSitesDir + "/unprefixed.plain": 2}},
}

// TestExpansionSiteScanFixtures asserts the scan on type-checked fixture
// packages: it finds the methods of runtime.IO by their resolved object,
// called, held as a value or reached through an embedded IO; finds a call of
// ExpandRemedies with a prefix, one that is only referenced and one at
// package level; counts a call with the empty prefix apart; and passes over a
// type of another package that has methods of the same names.
func TestExpansionSiteScanFixtures(t *testing.T) {
	t.Parallel()
	pkgs, err := expansionFixtures()
	require.NoError(t, err)
	for _, tc := range expansionFixtureCases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			pkg, ok := pkgs[tc.fixture]
			require.True(t, ok, "fixture package %s", tc.fixture)
			sites, empty := expansionSites([]*packages.Package{pkg})
			got := map[string]int{}
			for site, n := range sites {
				got[site.String()] = n
			}
			assert.Equal(t, tc.sites, got)
			assert.Equal(t, tc.empty, empty)
		})
	}
}

// TestExpansionSiteFixturesAreAllListed asserts every fixture package on
// disk is in the table above.
func TestExpansionSiteFixturesAreAllListed(t *testing.T) {
	t.Parallel()
	pkgs, err := expansionFixtures()
	require.NoError(t, err)
	var listed []string
	for _, tc := range expansionFixtureCases {
		listed = append(listed, tc.fixture)
	}
	assert.ElementsMatch(t, listed, slices.Collect(maps.Keys(pkgs)))
}
