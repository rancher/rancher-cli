package catalog

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

// valueCheckCall is a call, in a function of a test file, of a function that
// reads the runs of a value in a text.
type valueCheckCall struct {
	function string
	call     string
}

// valueCheckAllowlist is every function of the package's tests that calls
// valuePart or assertNoValuePart, which read the parts of a value in one
// output and so also find a part the output holds as fixed text. A value drawn
// at random is checked with assertNoValuePartLeft and a second output; a
// function listed with a redraw draws the value again until no part of it is
// in the fixed text.
var valueCheckAllowlist = map[valueCheckCall]string{
	{"otherValue", "valuePart"}:                                                                        "draws the other value until it shares no run with the planted value",
	{"assertNoValuePart", "valuePart"}:                                                                 "the check of a fixed value",
	{"TestRefusedActionKeepsTheTableAndAddsTheServerText", "valuePart"}:                                "redraws the value until no run is in the expected text",
	{"TestRefusedActionKeepsTheTableAndAddsTheServerText", "assertNoValuePart"}:                        "the value was redrawn until no run is in the expected text",
	{"TestRefusedActionHidesValuesInBodiesThatAreNotAnObject", "assertNoValuePart"}:                    "a fixed value",
	{"TestRefusedActionHidesValuesInStatusDetails", "valuePart"}:                                       "a fixed value",
	{"TestRefusedActionKeepsTheStatusFields", "assertNoValuePart"}:                                     "a fixed value",
	{"TestFilterHidesOverlappingValuesWhole", "valuePart"}:                                             "redraws the value until no run is in the expected text",
	{"TestRefusedActionRepeatedOverlappingValuesFilterTimeScalesWithTheBodySize", "assertNoValuePart"}: "a fixed value",
	{"TestValuePartLeftSkipsTheFixedTextOfTheCase", "valuePart"}:                                       "shows the error holds a run of the value in its fixed text",
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

// testFile is a parsed test file of the package directory.
type testFile struct {
	name string
	file *ast.File
}

// parsedTestFiles parses the test files of the directory.
func parsedTestFiles(t *testing.T) []testFile {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var files []testFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		files = append(files, testFile{name, file})
	}
	return files
}

// functionsCalling returns each call, in a function of the test files of the
// directory, of one of names, and the calls that are in a unit that calls none
// of drawers (when drawers is not empty).
func functionsCalling(t *testing.T, names, drawers []string) (calls []valueCheckCall, missing []string) {
	t.Helper()
	for _, f := range parsedTestFiles(t) {
		for _, decl := range f.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			for _, u := range callUnits(fn) {
				hasDrawer := false
				for _, d := range drawers {
					hasDrawer = hasDrawer || u.called[d]
				}
				for _, n := range names {
					if u.called[n] {
						calls = append(calls, valueCheckCall{u.function, n})
						if len(drawers) > 0 && !hasDrawer {
							missing = append(missing, f.name+": "+u.function+" calls "+n)
						}
					}
				}
			}
		}
	}
	return calls, missing
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

// TestARandomValueIsCheckedAgainstASecondOutput asserts the tests of the
// package call valuePart and assertNoValuePart only from the functions in
// valueCheckAllowlist, each with its reason, that every row names a call that
// exists, that a function, or a subtest of it, that calls
// assertNoValuePartLeft or valuePartLeft also calls otherValue (which draws
// the other value) or plantedCaseErrors (which runs the second case), and that
// no call of assertNoValuePartLeft passes one expression as both the error
// under test and the second error. A random value then never fails a check
// because a part of it is fixed text of the output.
func TestARandomValueIsCheckedAgainstASecondOutput(t *testing.T) {
	t.Parallel()
	calls, _ := functionsCalling(t, []string{"valuePart", "assertNoValuePart"}, nil)
	for _, c := range calls {
		assert.Contains(t, valueCheckAllowlist, c, "%s calls %s outside the allowlist: check a random value with assertNoValuePartLeft and the output of a second case planted with otherValue", c.function, c.call)
	}
	for row := range valueCheckAllowlist {
		assert.True(t, slices.Contains(calls, row), "the allowlist row %v matches no call", row)
	}
	_, missing := functionsCalling(t, []string{"assertNoValuePartLeft", "valuePartLeft"}, []string{"otherValue", "plantedCaseErrors"})
	missing = slices.DeleteFunc(missing, func(m string) bool { return strings.Contains(m, ": assertNoValuePartLeft calls ") })
	assert.Empty(t, missing, "a function that checks the runs of a value against a second output draws the other value")
	for _, f := range parsedTestFiles(t) {
		assert.Empty(t, callsWithSameArguments(f.file, "assertNoValuePartLeft", 2, 3), "%s passes the error under test as the second error", f.name)
	}
}

// TestAValueCheckRefusesTheErrorUnderTestAsItsSecondOutput asserts both
// refusals of a second output that is the output under test: sameError, which
// assertNoValuePartLeft requires to be false, finds the one error passed
// twice and not two errors of two cases; callsWithSameArguments finds a call
// that passes one expression twice and not a call that passes two.
func TestAValueCheckRefusesTheErrorUnderTestAsItsSecondOutput(t *testing.T) {
	t.Parallel()
	value := valueWithFixedRun()
	a := admissionActions[0]
	for _, tc := range admissionActions {
		if tc.values {
			a = tc
			break
		}
	}
	ce, second := plantedCaseErrors(t, a, podAnswers[0], notCut, value, true)
	assert.True(t, sameError(ce, ce))
	assert.False(t, sameError(ce, second))

	src := `package p
func f() {
	assertNoValuePartLeft(t, v, ce, ce, "twice")
	assertNoValuePartLeft(t, v, ce, second, "two")
}`
	file, err := parser.ParseFile(token.NewFileSet(), "p.go", src, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{`assertNoValuePartLeft(t, v, ce, ce, "twice")`}, callsWithSameArguments(file, "assertNoValuePartLeft", 2, 3))
}
