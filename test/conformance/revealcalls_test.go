package conformance

import (
	"fmt"
	"go/ast"
	"go/types"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// secret.Reveal is the only function that returns the text of a secret.Value.
// The reveal call site table below lists every function that calls it, with
// the reason and the types of what the function returns. The scan is a
// go/types walk of every production package with no data flow:
//
//   - every reference to secret.Reveal is the function of a call expression
//     whose nearest enclosing declaration is a function or method on the table,
//     and the call is not inside a function literal, which can be stored and
//     called later;
//   - each listed function's result types equal its row's;
//   - a row that no code uses, a row with no reason and a function declared
//     twice fail.
//
// The scan loads the packages for the platform the test runs on and for
// windows, as the scan of the readers of stdin does; a file that builds for
// neither is not seen. Test files are not scanned.

const (
	revealFixtureDir  = "test/conformance/testdata/revealcalls"
	revealPackagePath = modulePrefix + "internal/secret"
	revealName        = "Reveal"

	revealRuleUnlisted = "a call of secret.Reveal in a function with no row in revealCallSites"
	revealRuleNotCall  = "a reference to secret.Reveal that is not the function of a call"
	revealRuleLiteral  = "a call of secret.Reveal inside a function literal"
	revealRuleResults  = "a function on revealCallSites whose result types differ from its row"
	revealRuleStale    = "a row of revealCallSites that no code uses"
	revealRuleReason   = "a row of revealCallSites with no reason"
	revealRuleTwice    = "a function on revealCallSites that is declared twice"
)

// revealRow is one function that calls secret.Reveal.
type revealRow struct {
	// why is the reason the function reveals a text.
	why string
	// results are the types of what the function returns, joined with ", ".
	results string
	// body is the request body the text goes into, or "none".
	body string
}

// revealCallSites maps "<file>:<function>" (or "<file>:<Type>.<Method>") to the
// function that may call secret.Reveal.
var revealCallSites = map[string]revealRow{
	"internal/engine/client.go:CloudCredentialBody.RequestJSON": {
		"writes the request: a copy of the document with each Value revealed, then json.Marshal", "[]byte, error",
		"the Norman cloud credential create and update body"},
	"internal/engine/valuefilter.go:NewSecretFilter": {
		"builds the server-text filter from the values a request sent and the plain texts it sent along: each text's forms and NewTextFilter", "func(string) string", "none"},
	"internal/engine/provisioning/cloudcredwrite.go:cleanValue": {
		"takes a value and its source: a file or stdin value gets the encoding check, the byte order mark drop and cloudcredfields.Clean, a typed --set value gets cloudcredfields.Clean only", "secret.Value, cloudcredfields.Problem, bool", "none"},
	"internal/engine/provisioning/cloudcredwrite.go:decodesAsKubeconfig": {
		"reports whether the text decodes as a kubeconfig", "bool", "none"},
	"internal/engine/provisioning/cloudcredwrite.go:shownValue": {
		"a public field's value is shown in the preview diff as typed; a secret field returns engine.Redacted before any reveal, and its text is never returned or stored", "any", "none"},
	"internal/engine/provisioning/cloudcredwrite.go:fileWordPath": {
		"the path of a --from-file argument is not secret; refusals name it as typed; the function returns \"\" for the argument of any other flag", "string", "none"},
	"internal/engine/provisioning/cloudcredwrite.go:typedFieldName": {
		"a typed field name that equals a field of the driver without regard to case is quoted in a refusal, as typed; it reveals only after EqualFoldString matched", "string, bool", "none"},
}

// revealUse is one reference to secret.Reveal.
type revealUse struct {
	key, text string
	// shape is "call", "literal" or "notcall".
	shape string
}

// revealDecl is one declaration that has a reference.
type revealDecl struct {
	key, results string
}

type revealResult struct {
	uses  map[string]revealUse
	decls map[string]revealDecl
}

func newRevealResult() revealResult {
	return revealResult{uses: map[string]revealUse{}, decls: map[string]revealDecl{}}
}

func (r revealResult) merge(o revealResult) {
	maps.Copy(r.uses, o.uses)
	maps.Copy(r.decls, o.decls)
}

// revealIsReveal reports whether id names secret.Reveal.
func revealIsReveal(info *types.Info, id *ast.Ident) bool {
	fn, ok := info.Uses[id].(*types.Func)
	return ok && fn.Name() == revealName && fn.Pkg() != nil && fn.Pkg().Path() == revealPackagePath
}

// revealResultTypes lists the result types of the declared function fd, each
// qualified by its package name.
func revealResultTypes(info *types.Info, fd *ast.FuncDecl) string {
	fn, ok := info.Defs[fd.Name].(*types.Func)
	if !ok {
		return ""
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return ""
	}
	var out []string
	for v := range sig.Results().Variables() {
		out = append(out, types.TypeString(v.Type(), func(p *types.Package) string { return p.Name() }))
	}
	return strings.Join(out, ", ")
}

// revealShape classifies the reference at the top of stack: "literal" when a
// function literal encloses it, "notcall" when it is not the function of a call
// expression, "call" otherwise. A selector and parentheses around it are part
// of the function expression.
func revealShape(stack []ast.Node) string {
	if slices.ContainsFunc(stack[:len(stack)-1], func(n ast.Node) bool { _, ok := n.(*ast.FuncLit); return ok }) {
		return "literal"
	}
	i := len(stack) - 2
	expr := stack[len(stack)-1]
	if i >= 0 {
		if sel, ok := stack[i].(*ast.SelectorExpr); ok && sel.Sel == expr {
			expr = sel
			i--
		}
	}
	for i >= 0 {
		paren, ok := stack[i].(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = paren
		i--
	}
	if i >= 0 {
		if call, ok := stack[i].(*ast.CallExpr); ok && call.Fun == expr {
			return "call"
		}
	}
	return "notcall"
}

// revealScan returns every reference to secret.Reveal in the non-test files of
// pkg and the declarations that have one.
func revealScan(pkg *packages.Package, wd string) revealResult {
	info := pkg.TypesInfo
	res := newRevealResult()
	if pkg.PkgPath == revealPackagePath {
		// The type's own methods compare and cut its text; the exported API of
		// the package is covered by its own test.
		return res
	}
	for _, f := range pkg.Syntax {
		if strings.HasSuffix(pkg.Fset.Position(f.Pos()).Filename, "_test.go") {
			continue
		}
		rel := byIDRel(pkg, f.Pos(), wd)
		for _, decl := range f.Decls {
			key := rel + ":" + stdinDocFunctionName(decl)
			var stack []ast.Node
			ast.Inspect(decl, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				id, ok := n.(*ast.Ident)
				if !ok || !revealIsReveal(info, id) {
					return true
				}
				p := pkg.Fset.Position(id.Pos())
				at := fmt.Sprintf("%s:%d:%d", rel, p.Line, p.Column)
				res.uses[at] = revealUse{key: key, text: at + ": " + key, shape: revealShape(stack)}
				if fd, ok := decl.(*ast.FuncDecl); ok {
					dp := pkg.Fset.Position(fd.Pos())
					res.decls[fmt.Sprintf("%s:%d:%d", rel, dp.Line, dp.Column)] = revealDecl{key: key, results: revealResultTypes(info, fd)}
				}
				return true
			})
		}
	}
	return res
}

// revealLoads scans the module for the platform the test runs on and for
// windows, once per process, and merges the results by position.
var revealLoads = sync.OnceValues(func() (revealResult, error) {
	pkgs, err := fieldManagerPackages()
	if err != nil {
		return revealResult{}, err
	}
	wd, err := os.Getwd()
	if err != nil {
		return revealResult{}, err
	}
	merged := newRevealResult()
	for _, pkg := range pkgs {
		merged.merge(revealScan(pkg, wd))
	}
	windows, err := otherGOOSPackages("windows")
	if err != nil {
		return revealResult{}, err
	}
	for _, pkg := range windows {
		merged.merge(revealScan(pkg, wd))
	}
	return merged, nil
})

// revealCheck returns one problem per reference that breaks a rule and one per
// row that breaks one. Each problem starts with its rule.
func revealCheck(res revealResult, rows map[string]revealRow) []string {
	var problems []string
	used := map[string]bool{}
	for _, at := range slices.Sorted(maps.Keys(res.uses)) {
		u := res.uses[at]
		used[u.key] = true
		switch u.shape {
		case "notcall":
			problems = append(problems, revealRuleNotCall+": "+u.text+"; call it directly, and list the function in revealCallSites")
		case "literal":
			problems = append(problems, revealRuleLiteral+": "+u.text+"; a function literal can be stored and called later: call Reveal in the listed function itself")
		default:
			if _, ok := rows[u.key]; !ok {
				problems = append(problems, revealRuleUnlisted+": "+u.text+"; add a row with the reason, the result types and the request body, or do not reveal the text")
			}
		}
	}
	declared := map[string][]string{}
	for _, at := range slices.Sorted(maps.Keys(res.decls)) {
		d := res.decls[at]
		declared[d.key] = append(declared[d.key], at)
		row, ok := rows[d.key]
		if ok && d.results != row.results {
			problems = append(problems, fmt.Sprintf("%s: %s returns (%s), the row says (%s)", revealRuleResults, d.key, d.results, row.results))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(declared)) {
		if _, ok := rows[key]; ok && len(declared[key]) > 1 {
			problems = append(problems, fmt.Sprintf("%s: %s at %s", revealRuleTwice, key, strings.Join(declared[key], ", ")))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(rows)) {
		switch {
		case !used[key]:
			problems = append(problems, fmt.Sprintf("%s: %s matched nothing: remove it", revealRuleStale, key))
		case rows[key].why == "":
			problems = append(problems, fmt.Sprintf("%s: %s", revealRuleReason, key))
		}
	}
	return problems
}

// TestRevealIsCalledOnlyFromListedFunctions asserts every reference to
// secret.Reveal is a direct call in a function on revealCallSites, that each
// listed function returns the types of its row, and that no row is stale, empty
// of a reason or declared twice.
func TestRevealIsCalledOnlyFromListedFunctions(t *testing.T) {
	t.Parallel()
	res, err := revealLoads()
	require.NoError(t, err)
	for _, problem := range revealCheck(res, revealCallSites) {
		t.Error(problem)
	}
}

// TestRevealCallSiteRowsAreComplete asserts every row names its reason, its
// result types and its request body, in a file of a package that exists.
func TestRevealCallSiteRowsAreComplete(t *testing.T) {
	t.Parallel()
	for key, row := range revealCallSites {
		assert.NotEmpty(t, row.why, key)
		assert.NotEmpty(t, row.body, key)
		file, _, ok := strings.Cut(key, ":")
		require.True(t, ok, key)
		_, err := os.Stat(file)
		require.NoError(t, err, key)
		assert.True(t, strings.HasSuffix(file, ".go") && path.Dir(file) != ".", key)
	}
}

// revealFixtures type-checks the fixture packages once.
var revealFixtures = sync.OnceValues(func() (map[string]*packages.Package, error) {
	pkgs, err := loadPackages("./"+revealFixtureDir+"/...", "")
	if err != nil {
		return nil, err
	}
	out := map[string]*packages.Package{}
	for _, pkg := range pkgs {
		out[path.Base(pkg.PkgPath)] = pkg
	}
	return out, nil
})

// revealFixtureRows is the table of a fixture: one row for the function name in
// the file name.
func revealFixtureRows(fixture, file, function, results string) map[string]revealRow {
	return map[string]revealRow{
		revealFixtureDir + "/" + fixture + "/" + file + ":" + function: {"fixture", results, "none"},
	}
}

// revealFixtureCases lists every fixture package, the table it is checked
// against and the rules the check reports for it, in order.
var revealFixtureCases = []struct {
	fixture string
	rows    map[string]revealRow
	want    []string
}{
	{"listed", revealFixtureRows("listed", "listed.go", "Build", "string"), nil},
	{"aliased", revealFixtureRows("aliased", "aliased.go", "Build", "string"), nil},
	{"parenthesized", revealFixtureRows("parenthesized", "parenthesized.go", "Build", "string"), nil},
	{"unlisted", nil, []string{revealRuleUnlisted}},
	{"otherfile", revealFixtureRows("otherfile", "listed.go", "Build", "string"), []string{revealRuleUnlisted}},
	{"literal", revealFixtureRows("literal", "literal.go", "Build", "string"), []string{revealRuleLiteral}},
	{"funcvalue", revealFixtureRows("funcvalue", "funcvalue.go", "Build", ""), []string{revealRuleNotCall}},
	{"packagevalue", nil, []string{revealRuleNotCall}},
	{"results", revealFixtureRows("results", "results.go", "Build", "bool"), []string{revealRuleResults}},
	{"twice", revealFixtureRows("twice", "twice.go", "init", ""), []string{revealRuleTwice}},
	{"stale", revealFixtureRows("stale", "stale.go", "Gone", "string"), []string{revealRuleStale}},
	{"noreason", map[string]revealRow{revealFixtureDir + "/noreason/noreason.go:Build": {"", "string", "none"}}, []string{revealRuleReason}},
}

// TestRevealScanFixtures asserts each rule on type-checked fixture packages:
// every shape of a reference that the rules refuse is reported with its rule,
// and a direct call in a listed function, under an alias or in parentheses, is
// accepted.
func TestRevealScanFixtures(t *testing.T) {
	t.Parallel()
	pkgs, err := revealFixtures()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	for _, tc := range revealFixtureCases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			pkg, ok := pkgs[tc.fixture]
			require.True(t, ok, "fixture package %s", tc.fixture)
			var got []string
			for _, problem := range revealCheck(revealScan(pkg, wd), tc.rows) {
				rule, _, _ := strings.Cut(problem, ": ")
				got = append(got, rule)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestRevealScanFixturesAreAllListed asserts every fixture package on disk is
// in the table above.
func TestRevealScanFixturesAreAllListed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(revealFixtureDir)
	require.NoError(t, err)
	pkgs, err := revealFixtures()
	require.NoError(t, err)
	var onDisk, listed, loaded []string
	for _, e := range entries {
		if e.IsDir() {
			onDisk = append(onDisk, e.Name())
		}
	}
	for _, tc := range revealFixtureCases {
		listed = append(listed, tc.fixture)
	}
	for name := range pkgs {
		loaded = append(loaded, name)
	}
	assert.ElementsMatch(t, listed, onDisk)
	assert.ElementsMatch(t, listed, loaded)
}
