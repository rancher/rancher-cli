package conformance

import (
	"fmt"
	"go/ast"
	"go/constant"
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

// A secret the CLI reads from a prompt or from the token variable gets a row
// here, with what it is and whether the text stays a string. The scan is a
// go/types walk of every production package:
//
//   - every reference to the method PromptSecret or ReadLine of a Prompter of
//     internal/runtime or internal/ui (a call, a method value, a method
//     expression) is counted for the function it is in;
//   - every call of os.Getenv or os.LookupEnv whose argument has the constant
//     value "RANCHER_CLI_TOKEN" (the constant config.EnvToken and a literal
//     both match) is counted the same way;
//   - each function's count equals its row's, each row has a reason and says
//     whether the text stays a string, and a row that matches nothing is
//     stale.
//
// Limit: a variable name built at run time or passed through a variable is not
// matched; a new secret variable is found by name through the environment
// variable rows of internal/cli/secretinput_test.go. Test files are not
// scanned. The scan loads the packages for the platform the test runs on and
// for windows.

const (
	promptFixtureDir   = "test/conformance/testdata/promptreads"
	promptRuntimePath  = modulePrefix + "internal/runtime"
	promptUIPath       = modulePrefix + "internal/ui"
	promptTokenVarName = "RANCHER_CLI_TOKEN"

	promptRuleUnlisted = "a read of a prompt or of the token variable in a function with no row in classifiedReads"
	promptRuleCount    = "a function whose reads of a prompt or of the token variable the row counts differently"
	promptRuleStale    = "a row of classifiedReads that matches nothing"
	promptRuleStays    = "a row of classifiedReads that does not say whether the text stays a string, or has no reason"
)

// classifiedRead is one function that reads a secret from a prompt or the
// token variable: how many reads, what they read, and whether the text stays a
// string.
type classifiedRead struct {
	count int
	why   string
	stays string
}

// classifiedReads maps "<file>:<function>" (or "<file>:<Type>.<Method>") to the
// reads in that function.
var classifiedReads = map[string]classifiedRead{
	"internal/token/token.go:basicAuth": {1, "asks for the password of a basic sign-in",
		"stays a string: the password goes into the sign-in request"},
	"internal/commands/auth/login.go:resolveLoginToken": {1, "reads the token that --token-stdin takes",
		"stays a string: the token goes into the login credential the command stores"},
	"internal/commands/auth/login.go:promptForToken": {1, "asks for the token pasted at the prompt",
		"stays a string: the token goes into the login credential the command stores"},
	"internal/config/resolve.go:Config.Effective": {1, "reads the token variable into the connection settings",
		"stays a string: the token is the bearer credential of the connection"},
	"internal/cli/completion_token.go:resolveCompletionToken": {1, "reads the token variable for live completion",
		"stays a string: the token is the bearer credential of the completion requests"},
	"internal/cli/engine.go:clientFactory.StoredLoginName":                   {1, "checks that the token variable is set", classifiedPresence},
	"internal/commands/auth/authcheck.go:authCheckCmd.Execute":               {2, "checks that the token variable is set, in two places", classifiedPresence},
	"internal/commands/configcmd/set_completion.go:completionCapabilityNote": {1, "checks that the token variable is set", classifiedPresence},
	"internal/engine/kubeconfig/command.go:kubeconfigCmd.Execute":            {1, "checks that the token variable is set", classifiedPresence},
}

// classifiedPresence is the stays field of a read that only checks that the
// variable is not empty.
const classifiedPresence = "a presence check: the text is compared with the empty string and kept nowhere"

// promptFinding is one read the scan found.
type promptFinding struct{ key, text string }

// promptIsPrompterMethod reports whether fn is the method PromptSecret or
// ReadLine of a type named Prompter of internal/runtime or internal/ui.
func promptIsPrompterMethod(fn *types.Func) bool {
	if fn.Name() != "PromptSecret" && fn.Name() != "ReadLine" {
		return false
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return false
	}
	t := sig.Recv().Type()
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := types.Unalias(t).(*types.Named)
	if !ok || named.Obj().Name() != "Prompter" || named.Obj().Pkg() == nil {
		return false
	}
	pkgPath := named.Obj().Pkg().Path()
	return pkgPath == promptRuntimePath || pkgPath == promptUIPath
}

// promptScan returns every read of a prompt and of the token variable in the
// non-test files of pkg, keyed by position.
func promptScan(pkg *packages.Package, wd string) map[string]promptFinding {
	info := pkg.TypesInfo
	out := map[string]promptFinding{}
	for _, f := range pkg.Syntax {
		if strings.HasSuffix(pkg.Fset.Position(f.Pos()).Filename, "_test.go") {
			continue
		}
		rel := byIDRel(pkg, f.Pos(), wd)
		for _, decl := range f.Decls {
			key := rel + ":" + stdinDocFunctionName(decl)
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.Ident:
					if fn, ok := info.Uses[n].(*types.Func); ok && promptIsPrompterMethod(fn) {
						p := pkg.Fset.Position(n.Pos())
						at := fmt.Sprintf("%s:%d:%d", rel, p.Line, p.Column)
						out[at] = promptFinding{key, at + ": a reference to " + fn.Name() + " in " + key}
					}
				case *ast.CallExpr:
					fn := methodOf(info, n)
					if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != "os" || (fn.Name() != "Getenv" && fn.Name() != "LookupEnv") || len(n.Args) == 0 {
						return true
					}
					tv, ok := info.Types[n.Args[0]]
					if !ok || tv.Value == nil || tv.Value.Kind() != constant.String || constant.StringVal(tv.Value) != promptTokenVarName {
						return true
					}
					p := pkg.Fset.Position(n.Pos())
					at := fmt.Sprintf("%s:%d:%d", rel, p.Line, p.Column)
					out[at] = promptFinding{key, at + ": a read of " + promptTokenVarName + " in " + key}
				}
				return true
			})
		}
	}
	return out
}

// promptLoads scans the module for the platform the test runs on and for
// windows, once per process, and merges the findings by position.
var promptLoads = sync.OnceValues(func() (map[string]promptFinding, error) {
	pkgs, err := fieldManagerPackages()
	if err != nil {
		return nil, err
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	merged := map[string]promptFinding{}
	for _, pkg := range pkgs {
		maps.Copy(merged, promptScan(pkg, wd))
	}
	windows, err := otherGOOSPackages("windows")
	if err != nil {
		return nil, err
	}
	for _, pkg := range windows {
		maps.Copy(merged, promptScan(pkg, wd))
	}
	return merged, nil
})

// promptCheck returns one problem per function whose reads have no row or a
// different count, one per row that matches nothing and one per row without a
// reason or a stays field. Each problem starts with its rule.
func promptCheck(found map[string]promptFinding, rows map[string]classifiedRead) []string {
	var problems []string
	byKey := map[string][]string{}
	for _, at := range slices.Sorted(maps.Keys(found)) {
		f := found[at]
		byKey[f.key] = append(byKey[f.key], f.text)
	}
	for _, key := range slices.Sorted(maps.Keys(byKey)) {
		row, ok := rows[key]
		switch {
		case !ok:
			for _, text := range byKey[key] {
				problems = append(problems, promptRuleUnlisted+": "+text+"; add a row that says what the text is and whether it stays a string")
			}
		case len(byKey[key]) != row.count:
			problems = append(problems, fmt.Sprintf("%s: %s: the row counts %d (%s) and the scan found %d: %s", promptRuleCount, key, row.count, row.why, len(byKey[key]), strings.Join(byKey[key], "; ")))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(rows)) {
		row := rows[key]
		switch {
		case len(byKey[key]) == 0:
			problems = append(problems, fmt.Sprintf("%s: %s matched nothing: remove it", promptRuleStale, key))
		case row.why == "" || row.stays == "":
			problems = append(problems, fmt.Sprintf("%s: %s", promptRuleStays, key))
		}
	}
	return problems
}

// TestPromptAndTokenVariableReadsAreClassified asserts every read of a prompt
// and of the token variable is in classifiedReads with its exact count, with
// what it reads and whether the text stays a string.
func TestPromptAndTokenVariableReadsAreClassified(t *testing.T) {
	t.Parallel()
	found, err := promptLoads()
	require.NoError(t, err)
	for _, problem := range promptCheck(found, classifiedReads) {
		t.Error(problem)
	}
}

// TestClassifiedReadRowsAreComplete asserts every row names its reason, how
// whether the text stays a string and a count, in a file that exists.
func TestClassifiedReadRowsAreComplete(t *testing.T) {
	t.Parallel()
	for key, row := range classifiedReads {
		assert.NotEmpty(t, row.why, key)
		assert.NotEmpty(t, row.stays, key)
		assert.Positive(t, row.count, key)
		file, _, ok := strings.Cut(key, ":")
		require.True(t, ok, key)
		_, err := os.Stat(file)
		require.NoError(t, err, key)
	}
}

// promptFixtures type-checks the fixture packages once.
var promptFixtures = sync.OnceValues(func() (map[string]*packages.Package, error) {
	pkgs, err := loadPackages("./"+promptFixtureDir+"/...", "")
	if err != nil {
		return nil, err
	}
	out := map[string]*packages.Package{}
	for _, pkg := range pkgs {
		out[path.Base(pkg.PkgPath)] = pkg
	}
	return out, nil
})

// promptFixtureRows is the table of a fixture: one row for the function of the
// file named like the fixture.
func promptFixtureRows(fixture, function string, count int) map[string]classifiedRead {
	return map[string]classifiedRead{
		promptFixtureDir + "/" + fixture + "/" + fixture + ".go:" + function: {count, "fixture", "as a string"},
	}
}

// promptFixtureCases lists every fixture package, the table it is checked
// against and the rules the check reports for it, in order.
var promptFixtureCases = []struct {
	fixture string
	rows    map[string]classifiedRead
	want    []string
}{
	{"listed", promptFixtureRows("listed", "Read", 1), nil},
	{"unlisted", nil, []string{promptRuleUnlisted}},
	{"methodvalue", nil, []string{promptRuleUnlisted}},
	{"methodexpr", nil, []string{promptRuleUnlisted}},
	{"readline", nil, []string{promptRuleUnlisted}},
	{"uiprompter", nil, []string{promptRuleUnlisted}},
	{"tokenconstant", nil, []string{promptRuleUnlisted}},
	{"tokenliteral", nil, []string{promptRuleUnlisted}},
	{"tokenlookup", nil, []string{promptRuleUnlisted}},
	{"othervariable", nil, nil},
	{"countdiffers", promptFixtureRows("countdiffers", "Read", 1), []string{promptRuleCount}},
	{"stale", promptFixtureRows("stale", "Gone", 1), []string{promptRuleStale}},
	{"noheld", map[string]classifiedRead{promptFixtureDir + "/noheld/noheld.go:Read": {1, "fixture", ""}}, []string{promptRuleStays}},
}

// TestPromptReadScanFixtures asserts each rule on type-checked fixture
// packages: every form of a read that has no row is reported, a read of
// another variable is not, and a row with its exact count passes.
func TestPromptReadScanFixtures(t *testing.T) {
	t.Parallel()
	pkgs, err := promptFixtures()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	for _, tc := range promptFixtureCases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			pkg, ok := pkgs[tc.fixture]
			require.True(t, ok, "fixture package %s", tc.fixture)
			var got []string
			for _, problem := range promptCheck(promptScan(pkg, wd), tc.rows) {
				rule, _, _ := strings.Cut(problem, ": ")
				got = append(got, rule)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestPromptReadScanFixturesAreAllListed asserts every fixture package on disk
// is in the table above.
func TestPromptReadScanFixturesAreAllListed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(promptFixtureDir)
	require.NoError(t, err)
	pkgs, err := promptFixtures()
	require.NoError(t, err)
	var onDisk, listed, loaded []string
	for _, e := range entries {
		if e.IsDir() {
			onDisk = append(onDisk, e.Name())
		}
	}
	for _, tc := range promptFixtureCases {
		listed = append(listed, tc.fixture)
	}
	for name := range pkgs {
		loaded = append(loaded, name)
	}
	assert.ElementsMatch(t, listed, onDisk)
	assert.ElementsMatch(t, listed, loaded)
}
