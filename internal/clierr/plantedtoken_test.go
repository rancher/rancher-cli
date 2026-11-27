package clierr

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// plantedCase is one output of the admission message code that a planted
// token goes into.
type plantedCase struct {
	name string
	// fixedRun is a run of 4 characters of the fixed text of the output.
	fixedRun string
	// cut is true when the bound of the output falls inside the token.
	cut bool
	// output is the text the code shows for the token, with filter.
	output func(t *testing.T, token string, filter func(string) string) string
}

var plantedCases = []plantedCase{
	{"AdmissionRefusalOfBody, not cut", "forb", false, func(t *testing.T, v string, filter func(string) string) string {
		t.Helper()
		ce, ok := refusalOf(bodyOf(t, podRefusalHead+vapTextPrefix+"value "+v+" was refused"), filter)
		require.True(t, ok)
		return ce.Message
	}},
	{"AdmissionRefusalOfBody, cut inside the value", "forb", true, func(t *testing.T, v string, filter func(string) string) string {
		t.Helper()
		ce, ok := refusalOf(bodyOf(t, straddling(podRefusalHead+vapTextPrefix, v, maxAdmissionMessage)), filter)
		require.True(t, ok)
		return ce.Message
	}},
	{"FilteredServerMessage, not cut", "refu", false, func(t *testing.T, v string, filter func(string) string) string {
		t.Helper()
		return FilteredServerMessage(bodyOf(t, "refused "+v+" now"), filter)
	}},
	{"FilteredServerMessage, cut inside the value", "refu", true, func(t *testing.T, v string, filter func(string) string) string {
		t.Helper()
		return FilteredServerMessage(bodyOf(t, straddling("refused ", v, maxServerMessage)), filter)
	}},
}

// plantedOutputs returns a token that starts with the fixed run of the case,
// the output of the case for it, and the output of the case for the other
// token, each with the filter that hides its token when filterOn is true and
// with no filter when it is false.
func plantedOutputs(t *testing.T, tc plantedCase, filterOn bool) (planted, output, second string) {
	t.Helper()
	planted = tc.fixedRun + randomToken(t, 28)
	other := otherToken(t, planted)
	filter := func(v string) func(string) string {
		if filterOn {
			return hideToken(v)
		}
		return keepAsIs
	}
	return planted, tc.output(t, planted, filter(planted)), tc.output(t, other, filter(other))
}

// TestTokenRunLeftSkipsTheFixedTextOfTheCase asserts a token that starts with
// a run of the fixed text of the output, and that the filter hides, leaves no
// run. The output holds the run and a check that reads every run of the token
// in one output finds it; tokenRunLeft finds none, because the output of the
// case planted with another token holds the run too.
func TestTokenRunLeftSkipsTheFixedTextOfTheCase(t *testing.T) {
	t.Parallel()
	for _, tc := range plantedCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			token, got, second := plantedOutputs(t, tc, true)
			require.True(t, strings.HasPrefix(token, tc.fixedRun))
			require.Contains(t, got, tc.fixedRun, "the output holds a run of the token in its fixed text")
			assert.Contains(t, second, tc.fixedRun)
			run, found := tokenRunLeft(token, got, second)
			assert.False(t, found, "%q of the token is left in %q", run, got)
		})
	}
}

// TestTokenRunLeftFindsATokenTheFilterLeaves asserts tokenRunLeft finds a run
// of a token that starts with a run of the fixed text when the filter does not
// know the token: the whole token in an output that is not cut, and the start
// of the token in an output cut inside it.
func TestTokenRunLeftFindsATokenTheFilterLeaves(t *testing.T) {
	t.Parallel()
	for _, tc := range plantedCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			token, got, second := plantedOutputs(t, tc, false)
			run, found := tokenRunLeft(token, got, second)
			require.True(t, found, "the output shows the token: %s", got)
			assert.NotEqual(t, tc.fixedRun, run)
			assert.NotContains(t, second, run)
			if tc.cut {
				assert.Contains(t, token[:16], run)
			} else {
				assert.Contains(t, token, run)
			}
		})
	}
}

// callUnit is the names of the functions that a function of a test file, or one
// function literal inside it (a subtest), calls directly, not counting the
// literals nested in it.
type callUnit struct {
	function string
	called   map[string]bool
}

// callUnits returns the unit of decl and the unit of every function literal in
// it, with the name of decl.
func callUnits(decl *ast.FuncDecl) []callUnit {
	var units []callUnit
	var visit func(body *ast.BlockStmt)
	visit = func(body *ast.BlockStmt) {
		u := callUnit{function: decl.Name.Name, called: map[string]bool{}}
		ast.Inspect(body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncLit:
				visit(n.Body)
				return false
			case *ast.CallExpr:
				if id, ok := n.Fun.(*ast.Ident); ok {
					u.called[id.Name] = true
				}
			}
			return true
		})
		units = append(units, u)
	}
	visit(decl.Body)
	return units
}

// plantedTokenCall is a call, in a function of a test file, of a function
// that reads the runs of a token.
type plantedTokenCall struct {
	function string
	call     string
}

// tokenRunAllowlist is every function of the package's tests that calls
// tokenRunLeft, which reads the runs of a token in one text. A planted token is
// checked with assertNoTokenPart and the output of a second case planted with
// otherToken.
var tokenRunAllowlist = map[plantedTokenCall]string{
	{"assertNoTokenPart", "tokenRunLeft"}:                          "the check of a planted token, given the output of the second case",
	{"otherToken", "tokenRunLeft"}:                                 "draws the other token until it shares no run with the planted token",
	{"TestTokenRunLeftSkipsTheFixedTextOfTheCase", "tokenRunLeft"}: "shows the output holds a run of the token in its fixed text",
	{"TestTokenRunLeftFindsATokenTheFilterLeaves", "tokenRunLeft"}: "shows a token the filter leaves is found",
}

// callsWithSameArguments returns the position of each call of the function
// name in file whose arguments i and j are the same expression, which passes
// one output as both the output under test and the second output.
func callsWithSameArguments(file *ast.File, name string, i, j int) []string {
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) <= max(i, j) {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == name && types.ExprString(call.Args[i]) == types.ExprString(call.Args[j]) {
			found = append(found, types.ExprString(call))
		}
		return true
	})
	return found
}

// TestAPlantedTokenIsCheckedAgainstASecondOutput asserts the tests of the
// package call tokenRunLeft only from the functions in tokenRunAllowlist, that
// every row names a call that exists, that a function that calls plantedToken
// or assertNoTokenPart, or a subtest of it, also calls otherToken (which draws
// the token of the second case), and that no call of assertNoTokenPart or
// tokenRunLeft passes one expression as both the text under test and the second
// text. A planted token then never fails a check because a run of it is fixed
// text of the output.
func TestAPlantedTokenIsCheckedAgainstASecondOutput(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var calls []plantedTokenCall
	var missing []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		assert.Empty(t, callsWithSameArguments(file, "assertNoTokenPart", 2, 3), "%s passes the text under test as the second text", name)
		assert.Empty(t, callsWithSameArguments(file, "tokenRunLeft", 1, 2), "%s passes the text under test as the second text", name)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			for _, u := range callUnits(fn) {
				if u.called["tokenRunLeft"] {
					calls = append(calls, plantedTokenCall{u.function, "tokenRunLeft"})
				}
				for _, planted := range []string{"plantedToken", "assertNoTokenPart"} {
					if u.called[planted] && !u.called["otherToken"] {
						missing = append(missing, name+": "+u.function+" calls "+planted)
					}
				}
			}
		}
	}
	for _, c := range calls {
		assert.Contains(t, tokenRunAllowlist, c, "%s calls %s outside the allowlist: check a planted token with assertNoTokenPart and the output of a second case planted with otherToken", c.function, c.call)
	}
	for row := range tokenRunAllowlist {
		assert.True(t, slices.Contains(calls, row), "the allowlist row %v matches no call", row)
	}
	assert.Empty(t, missing, "a function that plants a token or checks its runs draws the other token")
}

// TestATokenCheckRefusesTheOutputUnderTestAsItsSecondOutput asserts the guard
// of the call arguments finds a call that passes one expression twice and not a
// call that passes two.
func TestATokenCheckRefusesTheOutputUnderTestAsItsSecondOutput(t *testing.T) {
	t.Parallel()
	src := `package p
func f() {
	assertNoTokenPart(t, token, got, got, "twice")
	assertNoTokenPart(t, token, got, second, "two")
	tokenRunLeft(token, got, got)
	tokenRunLeft(token, got, second)
}`
	file, err := parser.ParseFile(token.NewFileSet(), "p.go", src, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{`assertNoTokenPart(t, token, got, got, "twice")`}, callsWithSameArguments(file, "assertNoTokenPart", 2, 3))
	assert.Equal(t, []string{`tokenRunLeft(token, got, got)`}, callsWithSameArguments(file, "tokenRunLeft", 1, 2))
}
