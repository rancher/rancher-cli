package fidelity

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// timeBoundRow is an upper bound on time.Since in a test of the package, kept
// with its reason: the file, the function it sits in and the text of the bound.
type timeBoundRow struct{ file, function, bound, reason string }

// timeBoundRows are the bounds over 1 s on time.Since that the package keeps.
// Most sit in a wait in Go code with little real work, where the bound only
// fails a case that hangs or that waits for the wrong timer, and the timers it
// tells apart are far apart. Two are runs of a shell function: TestRedactLog
// bounds the speed of redact_log on a long line, and TestHostMemoryFree bounds
// a script function that a sleeping stub would hold for 30 s. TestRedactLog's bound checks how
// fast the shell runs, which also depends on how loaded the machine is.
var timeBoundRows = []timeBoundRow{
	{"createfixture_test.go", "TestCreateFixtureRetryEndsWithTheContext", "10*time.Second",
		"the context ends after 200 ms and the retry pause is an hour; 10 s tells the two apart"},
	{"oracledump_test.go", "TestRedactLog", "2*time.Second",
		"one shell run of redact_log on a line of 100000 characters; a loop over each character takes far longer"},
	{"executor_test.go", "TestWaitForStatusEndsWithTheStepDeadline", "3*time.Second",
		"the step deadline is 300 ms and the wait interval 5 s; 3 s tells the two apart"},
	{"oracledump_test.go", "TestHostMemoryFree", "c.maxTakes",
		"the test case allows 10 s for a memory_pressure stub that sleeps 30 s against a read limit of 1 s"},
	{"until_test.go", "TestUntilWaitDeadlineEndsTheWaitFirst", "10*time.Second",
		"the stored deadline is 250 ms and untilTimeout a minute; 10 s tells the two apart"},
	{"maxminor_test.go", "TestMaxMinorVersionRequestHasAStepTimeout", "5*time.Second",
		"the step timeout is 50 ms and the context 10 s; 5 s tells the two apart"},
}

// timeBoundFuncs are the testify functions that put an upper bound on their
// first value: Less(t, value, bound) and LessOrEqual(t, value, bound). Greater
// and Greaterf take the bound first: Greater(t, bound, value).
var timeBoundFuncs = map[string]bool{
	"Less": true, "Lessf": true, "LessOrEqual": true, "LessOrEqualf": true,
	"Greater": false, "Greaterf": false,
}

// timeBoundScan is what scanTimeBounds found: offenders are bounds over 1 s
// or bounds it cannot compute that have no row; problems are rows that match
// no bound.
type timeBoundScan struct {
	offenders []string
	problems  []string
}

// scanTimeBounds parses the test sources and reports every upper bound on
// time.Since over 1 s that has no row in rows. A bound is a call of assert or
// require Less, LessOrEqual or Greater (and their -f forms) whose bounded value
// is a time.Since call, or a variable the same function assigned from one
// (elapsed := time.Since(start)). The bound must be a duration the scan can
// compute: a number, a unit of package time, a constant of the package or of
// the function, or a product, sum or quotient of those. A bound it cannot
// compute is reported unless it has a row. GreaterOrEqual and the other
// minimums are not bounds. A row that matches no bound is a problem. Forms it
// does not see: a bound with the limit first in GreaterOrEqual (an upper bound
// written as assert.GreaterOrEqual(t, 60*time.Second, elapsed)), a comparison
// operator on the elapsed time (assert.True(t, time.Since(start) < d) or
// if time.Since(start) > d { t.Fatal(...) }), and a conversion of the elapsed
// time to a number (int(time.Since(start).Seconds()) compared with 60, which
// the scan reads as 60 ns). Such a form needs a review, not a row.
func scanTimeBounds(sources map[string]string, rows []timeBoundRow) (timeBoundScan, error) {
	var scan timeBoundScan
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		f, err := parser.ParseFile(fset, name, sources[name], 0)
		if err != nil {
			return scan, err
		}
		files[name] = f
	}
	consts := map[string]ast.Expr{}
	for _, f := range files {
		for _, decl := range f.Decls {
			collectConsts(decl, consts)
		}
	}
	used := make([]bool, len(rows))
	for _, name := range slices.Sorted(maps.Keys(files)) {
		for _, decl := range files[name].Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			local := maps.Clone(consts)
			since := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.DeclStmt:
					collectConsts(n.Decl, local)
				case *ast.AssignStmt:
					if len(n.Lhs) == len(n.Rhs) {
						for i, lhs := range n.Lhs {
							if id, ok := lhs.(*ast.Ident); ok && hasTimeSince(n.Rhs[i]) {
								since[id.Name] = true
							}
						}
					}
				}
				return true
			})
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || (pkg.Name != "assert" && pkg.Name != "require") {
					return true
				}
				valueFirst, ok := timeBoundFuncs[sel.Sel.Name]
				if !ok || len(call.Args) < 3 {
					return true
				}
				value, bound := call.Args[1], call.Args[2]
				if !valueFirst {
					value, bound = bound, value
				}
				if !hasTimeSince(value) && !isSinceVariable(value, since) {
					return true
				}
				text := exprText(fset, sources[name], bound)
				d, resolved := evalDuration(bound, local, 0)
				if resolved && d <= time.Second {
					return true
				}
				for i, row := range rows {
					if row.file == name && row.function == fn.Name.Name && row.bound == text {
						used[i] = true
						return true
					}
				}
				what := "cannot be computed"
				if resolved {
					what = d.String()
				}
				scan.offenders = append(scan.offenders, fmt.Sprintf("%s %s: %s.%s with the bound %s (%s)", fset.Position(call.Pos()), fn.Name.Name, pkg.Name, sel.Sel.Name, text, what))
				return true
			})
		}
	}
	for i, row := range rows {
		if row.reason == "" {
			scan.problems = append(scan.problems, fmt.Sprintf("the row for %s in %s %s needs a reason", row.bound, row.file, row.function))
		}
		if !used[i] {
			scan.problems = append(scan.problems, fmt.Sprintf("the row for %s in %s %s matches no bound; remove it", row.bound, row.file, row.function))
		}
	}
	return scan, nil
}

// collectConsts adds the constants a declaration names, with their value
// expressions, to consts.
func collectConsts(decl ast.Decl, consts map[string]ast.Expr) {
	gen, ok := decl.(*ast.GenDecl)
	if !ok || gen.Tok != token.CONST {
		return
	}
	for _, spec := range gen.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, name := range vs.Names {
			if i < len(vs.Values) {
				consts[name.Name] = vs.Values[i]
			}
		}
	}
}

// hasTimeSince reports whether the expression holds a call of time.Since.
func hasTimeSince(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "time" && sel.Sel.Name == "Since" {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// isSinceVariable reports whether the expression is a variable the function
// assigned from a time.Since call.
func isSinceVariable(e ast.Expr, since map[string]bool) bool {
	id, ok := e.(*ast.Ident)
	return ok && since[id.Name]
}

// timeUnits are the duration constants of package time.
var timeUnits = map[string]time.Duration{
	"Nanosecond": time.Nanosecond, "Microsecond": time.Microsecond, "Millisecond": time.Millisecond,
	"Second": time.Second, "Minute": time.Minute, "Hour": time.Hour,
}

// evalDuration computes a constant duration expression: numbers, the units of
// package time, constants in consts, time.Duration conversions, parentheses
// and the operators *, /, + and -. It reports false for anything else.
func evalDuration(e ast.Expr, consts map[string]ast.Expr, depth int) (time.Duration, bool) {
	if depth > 20 {
		return 0, false
	}
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.INT {
			return 0, false
		}
		n, err := strconv.ParseInt(e.Value, 0, 64)
		return time.Duration(n), err == nil
	case *ast.ParenExpr:
		return evalDuration(e.X, consts, depth+1)
	case *ast.Ident:
		value, ok := consts[e.Name]
		if !ok {
			return 0, false
		}
		return evalDuration(value, consts, depth+1)
	case *ast.SelectorExpr:
		if pkg, ok := e.X.(*ast.Ident); ok && pkg.Name == "time" {
			d, ok := timeUnits[e.Sel.Name]
			return d, ok
		}
		return 0, false
	case *ast.CallExpr:
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok && len(e.Args) == 1 {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "time" && sel.Sel.Name == "Duration" {
				return evalDuration(e.Args[0], consts, depth+1)
			}
		}
		return 0, false
	case *ast.BinaryExpr:
		x, okX := evalDuration(e.X, consts, depth+1)
		y, okY := evalDuration(e.Y, consts, depth+1)
		if !okX || !okY {
			return 0, false
		}
		switch e.Op {
		case token.MUL:
			return x * y, true
		case token.QUO:
			if y == 0 {
				return 0, false
			}
			return x / y, true
		case token.ADD:
			return x + y, true
		case token.SUB:
			return x - y, true
		}
	}
	return 0, false
}

// exprText is the source text of an expression.
func exprText(fset *token.FileSet, source string, e ast.Expr) string {
	return source[fset.Position(e.Pos()).Offset:fset.Position(e.End()).Offset]
}

// TestNoTestBoundsItsRunTimeOverOneSecond asserts that no test of the package
// puts an upper bound over 1 s on time.Since (see scanTimeBounds), except the
// rows of timeBoundRows. Such a bound measures how loaded the machine is: a
// wait that ignores its limit is ended by a count instead, as the sleep stub
// does for the script. A row that matches no bound fails the test. This keeps
// the list from going stale.
func TestNoTestBoundsItsRunTimeOverOneSecond(t *testing.T) {
	t.Parallel()
	scan, err := scanTimeBounds(packageTestSources(t), timeBoundRows)
	require.NoError(t, err)
	assert.Empty(t, scan.problems)
	assert.Empty(t, scan.offenders, "these tests bound their run time with time.Since: end the wait by a count, or add a row with the reason")
}

// TestTimeBoundScanMatcher asserts the rule of scanTimeBounds on small
// sources.
func TestTimeBoundScanMatcher(t *testing.T) {
	t.Parallel()
	const header = "package p\n\nimport (\n\t\"testing\"\n\t\"time\"\n)\n\n"
	cases := []struct {
		name      string
		body      string
		rows      []timeBoundRow
		offenders int
		problems  int
	}{
		{"a literal bound over 1 s", "start := time.Now()\nassert.Less(t, time.Since(start), 60*time.Second)", nil, 1, 0},
		{"a bound on a variable assigned from time.Since", "start := time.Now()\nelapsed := time.Since(start)\nassert.Less(t, elapsed, 60*time.Second)", nil, 1, 0},
		{"a variable assigned with =", "var elapsed time.Duration\nelapsed = time.Since(time.Now())\nrequire.LessOrEqualf(t, elapsed, 2*time.Minute, \"x\")", nil, 1, 0},
		{"require and the -f form", "require.Lessf(t, time.Since(time.Now()), 2*time.Second, \"x\")", nil, 1, 0},
		{"Greater with the bound first", "assert.Greater(t, 5*time.Second, time.Since(time.Now()))", nil, 1, 0},
		{"Greater with the elapsed time first is a minimum", "assert.Greater(t, time.Since(time.Now()), 5*time.Second)", nil, 0, 0},
		{"a package constant", "assert.Less(t, time.Since(time.Now()), limit)", nil, 1, 0},
		{"a function constant", "const wait = 90 * time.Second\nassert.Less(t, time.Since(time.Now()), wait)", nil, 1, 0},
		{"a bound that cannot be computed", "d := time.Second\nassert.Less(t, time.Since(time.Now()), d)", nil, 1, 0},
		{"a bound of 1 s", "assert.Less(t, time.Since(time.Now()), time.Second)", nil, 0, 0},
		{"a bound in milliseconds", "assert.Less(t, time.Since(time.Now()), 600*time.Millisecond)", nil, 0, 0},
		{"a conversion", "assert.Less(t, time.Since(time.Now()), time.Duration(3)*time.Second)", nil, 1, 0},
		{"GreaterOrEqual is a minimum", "assert.GreaterOrEqual(t, time.Since(time.Now()), 5*time.Second)", nil, 0, 0},
		{"a value that is not elapsed time", "n := 70\nassert.Less(t, n, 60)", nil, 0, 0},
		{"a row that matches a bound", "assert.Less(t, time.Since(time.Now()), 60*time.Second)", []timeBoundRow{{"a_test.go", "TestA", "60*time.Second", "why"}}, 0, 0},
		{"a row with no reason", "assert.Less(t, time.Since(time.Now()), 60*time.Second)", []timeBoundRow{{"a_test.go", "TestA", "60*time.Second", ""}}, 0, 1},
		{"a row that matches no bound", "", []timeBoundRow{{"a_test.go", "TestA", "60*time.Second", "why"}}, 0, 1},
		{"a row for another function", "assert.Less(t, time.Since(time.Now()), 60*time.Second)", []timeBoundRow{{"a_test.go", "TestB", "60*time.Second", "why"}}, 1, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			source := header + "const limit = time.Minute\n\nfunc TestA(t *testing.T) {\n" + c.body + "\n}\n"
			scan, err := scanTimeBounds(map[string]string{"a_test.go": source}, c.rows)
			require.NoError(t, err)
			assert.Len(t, scan.offenders, c.offenders, "offenders: %v", scan.offenders)
			assert.Len(t, scan.problems, c.problems, "problems: %v", scan.problems)
		})
	}
}
