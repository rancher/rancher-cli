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
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// A candidate list a command shows is the full list, or it is cut with
// (*clierr.Error).WithCandidatesCut, which records the size of the full list
// in the error. The scan below fails on each way production code outside
// internal/clierr could cut a list without that record:
//
//   - a slice expression on a []clierr.Candidate other than x[:0], also an
//     inline slice passed to WithCandidates;
//   - a comparison of len(x), for such an x, with anything but 0 (a counter
//     that stops collecting at a limit);
//   - an assignment to the Candidates or CandidatesTotal field of a
//     clierr.Error, or a composite literal that sets one;
//   - a use of WithCandidatesCut in internal/token, whose login failures
//     carry no candidate total.
//
// The scan resolves types: a PrimaryFailure literal that sets Candidates is
// another type and is not refused. It cannot see a cut made by a counter not
// tied to len, a cut of another element type before the conversion to
// []clierr.Candidate, or a slice of a type parameter constrained to
// []clierr.Candidate; the per-package tests of each cut site cover those.

const (
	cutTokenPackage   = modulePrefix + "internal/token"
	cutCandidatesDir  = "test/conformance/testdata/cutcandidates"
	cutCandidatesCall = "WithCandidatesCut"
)

const (
	cutSliceRule = "slice of a []clierr.Candidate other than x[:0] cuts a list unrecorded; attach the full list with WithCandidatesCut(list, limit)"
	cutLenRule   = "len of a []clierr.Candidate compared with something other than 0 limits a list unrecorded; collect the full list and attach it with WithCandidatesCut(list, limit)"
	cutFieldRule = "Candidates or CandidatesTotal of a clierr.Error set directly; attach candidates with WithCandidates or WithCandidatesCut"
	cutTokenRule = "WithCandidatesCut used in internal/token; a login failure's primary error carries no candidate total; attach the full list with WithCandidates"
)

// inTokenPackage reports whether pkgPath is internal/token or below it.
func inTokenPackage(pkgPath string) bool {
	return pkgPath == cutTokenPackage || strings.HasPrefix(pkgPath, cutTokenPackage+"/")
}

// isComparison reports whether op is a comparison operator.
func isComparison(op token.Token) bool {
	switch op {
	case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
		return true
	}
	return false
}

// isCandidateSlice reports whether t is a slice of clierr.Candidate.
func isCandidateSlice(t types.Type) bool {
	if t == nil {
		return false
	}
	s, ok := types.Unalias(t).Underlying().(*types.Slice)
	return ok && isNamedType(s.Elem(), clierrPath, "Candidate")
}

// isZeroConst reports whether e is the constant 0.
func isZeroConst(info *types.Info, e ast.Expr) bool {
	tv, ok := info.Types[e]
	return ok && tv.Value != nil && tv.Value.String() == "0"
}

// isLenOfCandidates reports whether e is a call of the builtin len on a
// []clierr.Candidate.
func isLenOfCandidates(info *types.Info, e ast.Expr) bool {
	call, ok := ast.Unparen(e).(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	id, ok := ast.Unparen(call.Fun).(*ast.Ident)
	if !ok {
		return false
	}
	if _, builtin := info.Uses[id].(*types.Builtin); !builtin || id.Name != "len" {
		return false
	}
	return isCandidateSlice(info.TypeOf(call.Args[0]))
}

// isCandidateField reports whether a selector names Candidates or
// CandidatesTotal as declared directly in clierr.Error (an embedded Error is
// followed to the struct that declares the field).
func isCandidateField(info *types.Info, sel *ast.SelectorExpr) bool {
	if sel.Sel.Name != "Candidates" && sel.Sel.Name != "CandidatesTotal" {
		return false
	}
	s, ok := info.Selections[sel]
	if !ok || s.Kind() != types.FieldVal {
		return false
	}
	owner := derefType(s.Recv())
	index := s.Index()
	for _, i := range index[:len(index)-1] {
		st, ok := owner.Underlying().(*types.Struct)
		if !ok {
			return false
		}
		owner = derefType(st.Field(i).Type())
	}
	return isNamedType(owner, clierrPath, "Error")
}

// scanCutCandidates returns the refusals in the non-test files of pkg, as
// "file:line: rule". inToken is true for the packages of internal/token.
func scanCutCandidates(pkg *packages.Package, wd string, inToken bool) []string {
	if pkg.PkgPath == clierrPath {
		return nil
	}
	info := pkg.TypesInfo
	var out []string
	for _, f := range pkg.Syntax {
		name := pkg.Fset.Position(f.Pos()).Filename
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		rel, err := filepath.Rel(wd, name)
		if err != nil {
			rel = name
		}
		rel = filepath.ToSlash(rel)
		report := func(pos token.Pos, rule string) {
			out = append(out, fmt.Sprintf("%s:%d: %s", rel, pkg.Fset.Position(pos).Line, rule))
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SliceExpr:
				if !isCandidateSlice(info.TypeOf(n.X)) {
					break
				}
				if n.Low == nil && n.Max == nil && n.High != nil && isZeroConst(info, n.High) {
					break
				}
				report(n.Pos(), cutSliceRule)
			case *ast.BinaryExpr:
				if !isComparison(n.Op) {
					break
				}
				for _, pair := range [][2]ast.Expr{{n.X, n.Y}, {n.Y, n.X}} {
					if isLenOfCandidates(info, pair[0]) && !isZeroConst(info, pair[1]) {
						report(n.Pos(), cutLenRule)
						break
					}
				}
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					if sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr); ok && isCandidateField(info, sel) {
						report(lhs.Pos(), cutFieldRule)
					}
				}
			case *ast.IncDecStmt:
				if sel, ok := ast.Unparen(n.X).(*ast.SelectorExpr); ok && isCandidateField(info, sel) {
					report(n.Pos(), cutFieldRule)
				}
			case *ast.CompositeLit:
				if !isNamedType(derefType(info.TypeOf(n)), clierrPath, "Error") {
					break
				}
				// A literal of another package cannot set the unexported
				// fields, so every element it sets is keyed.
				for _, elt := range n.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if id, ok := kv.Key.(*ast.Ident); ok && (id.Name == "Candidates" || id.Name == "CandidatesTotal") {
						report(kv.Pos(), cutFieldRule)
					}
				}
			case *ast.SelectorExpr:
				if !inToken || n.Sel.Name != cutCandidatesCall {
					break
				}
				fn, ok := info.Uses[n.Sel].(*types.Func)
				if ok && fn.Pkg() != nil && fn.Pkg().Path() == clierrPath && receiverName(fn) == "Error" {
					report(n.Pos(), cutTokenRule)
				}
			}
			return true
		})
	}
	return out
}

// TestNoCutCandidateListWithoutItsTotal asserts the scan finds nothing in the
// production tree.
func TestNoCutCandidateListWithoutItsTotal(t *testing.T) {
	t.Parallel()
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	require.NotEmpty(t, pkgs)
	wd, err := os.Getwd()
	require.NoError(t, err)
	var findings []string
	tokenPackages := 0
	for _, pkg := range pkgs {
		if inTokenPackage(pkg.PkgPath) {
			tokenPackages++
		}
		findings = append(findings, scanCutCandidates(pkg, wd, inTokenPackage(pkg.PkgPath))...)
	}
	require.Positive(t, tokenPackages, "no package of internal/token was loaded; the check of its use of WithCandidatesCut saw nothing")
	slices.Sort(findings)
	for _, f := range findings {
		t.Error(f)
	}
}

// cutCandidatesFixtures type-checks the fixture packages once.
var cutCandidatesFixtures = sync.OnceValues(func() (map[string]*packages.Package, error) {
	pkgs, err := loadPackages("./"+cutCandidatesDir+"/...", "")
	if err != nil {
		return nil, err
	}
	out := map[string]*packages.Package{}
	for _, pkg := range pkgs {
		out[path.Base(pkg.PkgPath)] = pkg
	}
	return out, nil
})

var cutFindingPosition = regexp.MustCompile(`^[^:]+:[0-9]+: `)

// cutCandidatesCases lists every fixture package and the refusals the scan
// reports for it.
var cutCandidatesCases = []struct {
	fixture string
	want    []string
}{
	{"slicehigh", []string{cutSliceRule}},
	{"slicelow", []string{cutSliceRule}},
	{"slicemin", []string{cutSliceRule}},
	{"slicenonzero", []string{cutSliceRule}},
	{"slicenamed", []string{cutSliceRule}},
	{"slicealias", []string{cutSliceRule}},
	{"sliceinline", []string{cutSliceRule}},
	{"lenless", []string{cutLenRule}},
	{"lengreater", []string{cutLenRule}},
	{"lenequalsone", []string{cutLenRule}},
	{"lenreversed", []string{cutLenRule}},
	{"lenappend", []string{cutLenRule}},
	{"setcandidates", []string{cutFieldRule}},
	{"settotal", []string{cutFieldRule}},
	{"incrementtotal", []string{cutFieldRule}},
	{"literalcandidates", []string{cutFieldRule}},
	{"literaltotal", []string{cutFieldRule}},
	{"setembedded", []string{cutFieldRule}},
	{"tokencut", []string{cutTokenRule, cutTokenRule}},
	{"zeroslice", nil},
	{"zerolen", nil},
	{"zerolenvariants", nil},
	{"otherslice", nil},
	{"otherlen", nil},
	{"primaryliteral", nil},
	{"otherfield", nil},
	{"readfields", nil},
	{"cutoutsidetoken", nil},
	{"fullbuilders", nil},
}

// TestCutCandidatesScanFixtures asserts each rule on type-checked fixture
// packages: every refused form is reported with its rule, and each form that
// looks alike but cuts nothing is accepted. The fixture named tokencut stands
// for a package of internal/token.
func TestCutCandidatesScanFixtures(t *testing.T) {
	t.Parallel()
	pkgs, err := cutCandidatesFixtures()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	for _, tc := range cutCandidatesCases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			pkg, ok := pkgs[tc.fixture]
			require.True(t, ok, "fixture package %s", tc.fixture)
			var got []string
			for _, f := range scanCutCandidates(pkg, wd, tc.fixture == "tokencut") {
				got = append(got, cutFindingPosition.ReplaceAllString(f, ""))
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestCutCandidatesScanFixturesAreAllListed asserts every fixture package on
// disk is in the table above, so a fixture added without its row fails.
func TestCutCandidatesScanFixturesAreAllListed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(cutCandidatesDir)
	require.NoError(t, err)
	pkgs, err := cutCandidatesFixtures()
	require.NoError(t, err)
	for _, e := range entries {
		if e.IsDir() {
			assert.Contains(t, pkgs, e.Name())
		}
	}
	var listed []string
	for _, tc := range cutCandidatesCases {
		listed = append(listed, tc.fixture)
	}
	assert.ElementsMatch(t, listed, slices.Collect(maps.Keys(pkgs)))
}

// TestInTokenPackage asserts internal/token and the packages below it count
// as internal/token, and a package that only shares the prefix does not.
func TestInTokenPackage(t *testing.T) {
	t.Parallel()
	assert.True(t, inTokenPackage(modulePrefix+"internal/token"))
	assert.True(t, inTokenPackage(modulePrefix+"internal/token/complete"))
	assert.False(t, inTokenPackage(modulePrefix+"internal/tokenx"))
	assert.False(t, inTokenPackage(modulePrefix+"internal/engine/rbac"))
}

// A NOT_FOUND error whose candidates are a list the server returns is cut to
// clierr.CandidateLimit entries, with the size of the full list. The scan below
// refuses these uses in the production code outside internal/clierr:
//
//   - a call of (*clierr.Error).WithCandidates, the call that attaches a list
//     whole, on an error built by clierr.New with the constant
//     clierr.CodeNotFound in the same chain of method calls;
//   - a call of WithCandidates on an error whose code the scan cannot tell: the
//     receiver is a variable or the result of a function, or the code passed to
//     clierr.New is not a constant;
//   - any use of the Candidates field of a clierr.Error, which takes the list
//     without its total, whether it is passed to WithCandidates, to a function
//     or kept in a variable, and a clierr.Error literal that sets Candidates;
//     (*clierr.Error).WithCandidatesOf passes both;
//   - a call of WithCandidatesCut whose limit is not the constant
//     clierr.CandidateLimit.
//
// A call of WithCandidates on an error built with another constant code, such
// as AMBIGUOUS_SELECTION, whose candidates the caller must choose from in full,
// is not refused. A function that must do one of the refused things is a row
// of uncutCandidateAllowlist with the number of findings it covers and the
// reason; the test fails on a different number, and on a row that matches no
// finding; a row cannot outlive the code it covers.
//
// The scan reads the bodies of functions and methods of the packages built for
// this machine, for windows and for linux. It cannot see a call in the
// initializer of a package-level variable, a method value (f := e.WithCandidates;
// f(c)), or a file built for none of those systems. It does not check what a
// list has or in what order; the tests of each producer do.

const (
	uncutCandidatesDir = "test/conformance/testdata/uncutcandidates"
	uncutCandidateCall = "WithCandidates"
	uncutCutCall       = "WithCandidatesCut"
)

const (
	uncutNotFoundRule = "NOT_FOUND error attaches a full candidate list with WithCandidates; attach it with WithCandidatesMatchesFirst(list, typed), or with WithCandidatesCut(list, clierr.CandidateLimit) when the list is the entries that contain the typed value, or all of them when none does"
	uncutUnknownRule  = "WithCandidates on an error whose code the scan cannot see; build the error and attach the list in one chain from clierr.New with a constant code, or add the function to uncutCandidateAllowlist with the reason"
	uncutReadRule     = "the Candidates of a clierr.Error are used here without that error's total; pass the error itself to WithCandidatesOf, or add the function to uncutCandidateAllowlist with the reason"
	uncutLimitRule    = "WithCandidatesCut with a limit other than clierr.CandidateLimit; every cut list has the one limit"
)

// uncutAllowance is the number of findings an allowlisted function covers and
// why the function may do what the scan refuses.
type uncutAllowance struct {
	count int
	why   string
}

// uncutCandidateAllowlist maps "file:function" of a production function to the
// number of refused uses the function may keep, and why. A different number of
// findings fails the test.
var uncutCandidateAllowlist = map[string]uncutAllowance{
	"internal/token/token.go:selectAuthProvider":            {2, "the candidates are the enabled auth configs of one server, a short fixed set; a login failure carries no candidate total"},
	"internal/token/noterminal.go:unsupportedConfigRemedy":  {2, "a login failure passes on the candidates of the primary failure, which carry no total"},
	"internal/token/noterminal.go:noTerminalError":          {1, "a login failure passes on the candidates it is given, which carry no total"},
	"internal/token/failure.go:compose":                     {2, "a login failure passes on the candidates of the primary failure, which carry no total"},
	"internal/token/failure.go:primaryOf":                   {1, "a login failure passes on the candidates of the primary failure, which carry no total"},
	"internal/runtime/pipeline.go:errorText":                {4, "the one renderer of an error prints the candidates and, from the total, how many it left out"},
	"internal/engine/rbac/revoke.go:Plan":                   {1, "the candidates are the roles one subject has at one scope, a short set"},
	"internal/engine/resources/lookup.go:lookupHandle":      {2, "the candidates are every object whose display name or lookup field equals the typed text, because the caller must pick one of them"},
	"internal/commands/register/register.go:ServerNotFound": {1, "the candidates are the servers of the local config, a short list"},
}

// uncutFinding is one refused use.
type uncutFinding struct {
	file, function, rule string
	line, column         int
}

// codeOfChain returns the constant code of the clierr.New call a chain of
// (*clierr.Error) method calls starts from, and whether the scan could tell.
func codeOfChain(info *types.Info, x ast.Expr) (string, bool) {
	for {
		call, ok := ast.Unparen(x).(*ast.CallExpr)
		if !ok {
			return "", false
		}
		fn := methodOf(info, call)
		if fn == nil {
			return "", false
		}
		if isPackageFunction(fn, clierrPath, "New") {
			if len(call.Args) == 0 {
				return "", false
			}
			return constString(info, call.Args[0])
		}
		sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
		if !ok || receiverName(fn) != "Error" || fn.Pkg() == nil || fn.Pkg().Path() != clierrPath {
			return "", false
		}
		x = sel.X
	}
}

// isCandidateLimit reports whether e names the constant clierr.CandidateLimit.
func isCandidateLimit(info *types.Info, e ast.Expr) bool {
	sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	c, ok := info.Uses[sel.Sel].(*types.Const)
	return ok && c.Pkg() != nil && c.Pkg().Path() == clierrPath && c.Name() == "CandidateLimit"
}

// scanUncutCandidates returns what the rules above refuse in the non-test files
// of pkg.
func scanUncutCandidates(pkg *packages.Package, wd string) []uncutFinding {
	if pkg.PkgPath == clierrPath {
		return nil
	}
	info := pkg.TypesInfo
	var out []uncutFinding
	for _, f := range pkg.Syntax {
		name := pkg.Fset.Position(f.Pos()).Filename
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		rel, err := filepath.Rel(wd, name)
		if err != nil {
			rel = name
		}
		rel = filepath.ToSlash(rel)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			report := func(pos token.Pos, rule string) {
				at := pkg.Fset.Position(pos)
				out = append(out, uncutFinding{rel, fd.Name.Name, rule, at.Line, at.Column})
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.SelectorExpr:
					if n.Sel.Name == "Candidates" && isCandidateField(info, n) {
						report(n.Pos(), uncutReadRule)
					}
				case *ast.CompositeLit:
					if !isNamedType(derefType(info.TypeOf(n)), clierrPath, "Error") {
						return true
					}
					for _, elt := range n.Elts {
						if kv, ok := elt.(*ast.KeyValueExpr); ok {
							if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Candidates" {
								report(kv.Pos(), uncutReadRule)
							}
						}
					}
				case *ast.CallExpr:
					sel, ok := ast.Unparen(n.Fun).(*ast.SelectorExpr)
					if !ok {
						return true
					}
					fn, ok := info.Uses[sel.Sel].(*types.Func)
					if !ok || fn.Pkg() == nil || fn.Pkg().Path() != clierrPath || receiverName(fn) != "Error" {
						return true
					}
					switch sel.Sel.Name {
					case uncutCandidateCall:
						code, known := codeOfChain(info, sel.X)
						switch {
						case !known:
							report(n.Pos(), uncutUnknownRule)
						case code == clierr.CodeNotFound:
							report(n.Pos(), uncutNotFoundRule)
						}
					case uncutCutCall:
						if len(n.Args) != 2 || !isCandidateLimit(info, n.Args[1]) {
							report(n.Pos(), uncutLimitRule)
						}
					}
				}
				return true
			})
		}
	}
	return out
}

// TestNotFoundCandidatesAreCut asserts the scan finds nothing in the production
// code except what the allowlist covers, with the number of findings each row
// names, and that every row is used.
func TestNotFoundCandidatesAreCut(t *testing.T) {
	t.Parallel()
	wd, err := os.Getwd()
	require.NoError(t, err)
	byFunction := map[string][]uncutFinding{}
	seen := map[string]bool{}
	scanned := 0
	for _, pkgs := range buildSets(t) {
		for _, pkg := range pkgs {
			scanned += len(pkg.Syntax)
			for _, f := range scanUncutCandidates(pkg, wd) {
				at := fmt.Sprintf("%s:%d:%d:%s", f.file, f.line, f.column, f.rule)
				if seen[at] {
					continue
				}
				seen[at] = true
				key := f.file + ":" + f.function
				byFunction[key] = append(byFunction[key], f)
			}
		}
	}
	require.NotZero(t, scanned)
	var findings []string
	for key, fs := range byFunction {
		allow, ok := uncutCandidateAllowlist[key]
		if ok && allow.count == len(fs) {
			continue
		}
		for _, f := range fs {
			msg := fmt.Sprintf("%s:%d: %s", f.file, f.line, f.rule)
			if ok {
				msg += fmt.Sprintf(" (the allowlist row covers %d, the scan found %d)", allow.count, len(fs))
			}
			findings = append(findings, msg)
		}
	}
	slices.Sort(findings)
	for _, f := range findings {
		t.Error(f)
	}
	for key, allow := range uncutCandidateAllowlist {
		assert.NotEmpty(t, allow.why, key)
		assert.Positive(t, allow.count, key)
		_, used := byFunction[key]
		assert.True(t, used, "the allowlist row %s matches no finding of the scan", key)
	}
}

// uncutCandidatesFixtures type-checks the fixture packages once.
var uncutCandidatesFixtures = sync.OnceValues(func() (map[string]*packages.Package, error) {
	pkgs, err := loadPackages("./"+uncutCandidatesDir+"/...", "")
	if err != nil {
		return nil, err
	}
	out := map[string]*packages.Package{}
	for _, pkg := range pkgs {
		out[path.Base(pkg.PkgPath)] = pkg
	}
	return out, nil
})

// uncutCandidatesCases lists every fixture package and the rules the scan
// reports for it.
var uncutCandidatesCases = []struct {
	fixture string
	want    []string
}{
	{"notfoundfull", []string{uncutNotFoundRule}},
	{"notfoundchain", []string{uncutNotFoundRule}},
	{"notfoundalias", []string{uncutNotFoundRule}},
	{"unknownvariable", []string{uncutUnknownRule}},
	{"unknownfunction", []string{uncutUnknownRule}},
	{"unknowncode", []string{uncutUnknownRule}},
	{"twostatements", []string{uncutUnknownRule}},
	{"copiedlist", []string{uncutReadRule}},
	{"notfoundcopiedlist", []string{uncutNotFoundRule, uncutReadRule}},
	{"readparameter", []string{uncutReadRule}},
	{"readliteral", []string{uncutReadRule}},
	{"readvariable", []string{uncutReadRule}},
	{"notfoundcutother", []string{uncutLimitRule}},
	{"cutlocalconstant", []string{uncutLimitRule}},
	{"unknowncutother", []string{uncutLimitRule}},
	{"notfoundcut", nil},
	{"notfoundmatchesfirst", nil},
	{"copyof", nil},
	{"ambiguousfull", nil},
	{"othercodefull", nil},
	{"copyfromother", nil},
	{"readtotal", nil},
}

// TestUncutCandidatesScanFixtures asserts each rule on type-checked fixture
// packages: every refused form is reported with its rule, and none of the
// look-alike forms that are accepted is.
func TestUncutCandidatesScanFixtures(t *testing.T) {
	t.Parallel()
	pkgs, err := uncutCandidatesFixtures()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	for _, tc := range uncutCandidatesCases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			pkg, ok := pkgs[tc.fixture]
			require.True(t, ok, "fixture package %s", tc.fixture)
			var got []string
			for _, f := range scanUncutCandidates(pkg, wd) {
				got = append(got, f.rule)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestUncutCandidatesScanFixturesAreAllListed asserts every fixture package on
// disk is in the table above; a fixture added without its row fails.
func TestUncutCandidatesScanFixturesAreAllListed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(uncutCandidatesDir)
	require.NoError(t, err)
	pkgs, err := uncutCandidatesFixtures()
	require.NoError(t, err)
	for _, e := range entries {
		if e.IsDir() {
			assert.Contains(t, pkgs, e.Name())
		}
	}
	var listed []string
	for _, tc := range uncutCandidatesCases {
		listed = append(listed, tc.fixture)
	}
	assert.ElementsMatch(t, listed, slices.Collect(maps.Keys(pkgs)))
}
