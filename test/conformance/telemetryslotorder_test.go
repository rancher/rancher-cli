package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A test that compares the file operations of an event write sees an extra
// open when two random event slots meet, and an extra directory creation when
// the run crosses an hour. The telemetry package has two test seams for this:
// one makes event slots come in order, the other freezes the clock. A test
// outside the package watches file operations only through the CLI test
// helper observeFiles, which turns both seams on; recordingEnv turns them on
// for the unobserved runs that seed a test. The scan below fails when a
// test file outside internal/telemetry/ sets the observer anywhere else, and
// when either helper stops calling both seams.

const (
	observerSeamName    = "SetObserverForTest"
	slotSeamName        = "SetDistinctSlotsForTest"
	clockSeamName       = "SetFixedClockForTest"
	observerHelperName  = "observeFiles"
	recordingHelperName = "recordingEnv"
	telemetryPackageDir = "internal/telemetry/"
)

// observerAllowlist maps "<file>:<func>" to the number of places that function
// sets the telemetry observer directly and why no operation list that holds
// an event write is compared there. A different number fails the test.
var observerAllowlist = map[string]requestAllowance{
	"internal/cli/refresh_signin_test.go:TestTheSignInRequestSharesOneDeadlineCountedFromItsCall": {2, "the observer only sleeps on a read; no list is compared"},
	"internal/cli/upload_test.go:TestUploadStaysInTheDeadline":                                    {1, "the observer only sleeps on a read; no list is compared"},
	"internal/cli/uploadcommand_test.go:TestUploadCommandIsBounded":                               {1, "the observer only sleeps on a read; no list is compared"},
	"internal/telemetrysend/sendonce_test.go:TestUploadOncePerHour":                               {1, "Send writes no event, and the test checks the next attempt against the real clock"},
	"internal/telemetrysend/sendonce_test.go:TestWalkAfterTheDeletionRunsOnlyWithTimeLeft":        {1, "the observer only sleeps on a read; no list is compared"},
}

// calledName returns the name a call's function expression ends in: f for
// f(...) and for x.f(...), or the empty string.
func calledName(e ast.Expr) string {
	switch x := ast.Unparen(e).(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
}

// scanTelemetrySeams returns, for f, every mention of the observer setter
// outside the function observeFiles, keyed by file and enclosing declaration
// (a function literal belongs to its enclosing declaration), and for each
// declaration of observeFiles and recordingEnv, keyed by its position and
// name, the seams whose call is an argument of a Cleanup call made as a
// statement of the function body itself: the restore is kept, and the call
// runs whenever the helper does.
// Matching is by identifier name, which covers a qualified, aliased and dot
// imported use alike.
func scanTelemetrySeams(fset *token.FileSet, filePath string, f *ast.File) ([]requestMatch, map[string]map[string]bool) {
	var matches []requestMatch
	helpers := map[string]map[string]bool{}
	for _, decl := range f.Decls {
		fn := "(package level)"
		fd, isFunc := decl.(*ast.FuncDecl)
		if isFunc {
			fn = fd.Name.Name
		}
		if isFunc && fd.Body != nil && (fn == observerHelperName || fn == recordingHelperName) {
			seams := map[string]bool{}
			for _, stmt := range fd.Body.List {
				es, ok := stmt.(*ast.ExprStmt)
				if !ok {
					continue
				}
				if cl, ok := es.X.(*ast.CallExpr); ok && calledName(cl.Fun) == "Cleanup" {
					for _, arg := range cl.Args {
						if call, ok := arg.(*ast.CallExpr); ok {
							seams[calledName(call.Fun)] = true
						}
					}
				}
			}
			helpers[fset.Position(fd.Pos()).String()+": "+fn] = seams
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if x.Name == observerSeamName && fn != observerHelperName {
					matches = append(matches, requestMatch{
						key:  filePath + ":" + fn,
						text: fset.Position(x.Pos()).String() + ": the telemetry observer is set outside observeFiles; use observeFiles, which also turns on in-order event slots and the frozen clock, or add the function to the allowlist with the reason no operation list is compared",
					})
				}
			}
			return true
		})
	}
	return matches, helpers
}

// checkTelemetrySeams parses every _test.go source in sources (path to
// content) outside the telemetry package's directory and returns the problems
// found against allow: an observer setter outside observeFiles and the
// allowlist, an allowlist entry that no longer matches, and a helper that is
// missing or does not pass both seams to Cleanup.
func checkTelemetrySeams(sources map[string]string, allow map[string]requestAllowance) ([]string, error) {
	fset := token.NewFileSet()
	var matches []requestMatch
	helpers := map[string]map[string]bool{}
	for _, path := range slices.Sorted(maps.Keys(sources)) {
		if strings.HasPrefix(path, telemetryPackageDir) || !strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, sources[path], parser.AllErrors)
		if err != nil {
			return nil, err
		}
		m, h := scanTelemetrySeams(fset, path, f)
		matches = append(matches, m...)
		maps.Copy(helpers, h)
	}
	problems := checkRequestMatches(matches, allow)
	for _, want := range []string{observerHelperName, recordingHelperName} {
		found := false
		for _, key := range slices.Sorted(maps.Keys(helpers)) {
			if !strings.HasSuffix(key, ": "+want) {
				continue
			}
			found = true
			for _, seam := range []string{slotSeamName, clockSeamName} {
				if !helpers[key][seam] {
					problems = append(problems, fmt.Sprintf("%s does not pass telemetry.%s() to Cleanup", key, seam))
				}
			}
		}
		if !found {
			problems = append(problems, fmt.Sprintf("the function declaration %s is missing: the scan hard-codes the helper that turns the telemetry test seams on", want))
		}
	}
	return problems, nil
}

// TestTelemetryObserverIsSetOnlyThroughTheHelper asserts every test file
// outside internal/telemetry/ sets the telemetry file operation observer
// through observeFiles or from the allowlist, and that observeFiles and
// recordingEnv each pass both seams' restore functions to Cleanup.
func TestTelemetryObserverIsSetOnlyThroughTheHelper(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	sources := map[string]string{}
	for _, path := range files {
		if !strings.HasSuffix(path, "_test.go") || strings.HasPrefix(path, telemetryPackageDir) {
			continue
		}
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		sources[path] = string(src)
	}
	problems, err := checkTelemetrySeams(sources, observerAllowlist)
	require.NoError(t, err)
	for _, problem := range problems {
		t.Error(problem)
	}
	for key, a := range observerAllowlist {
		assert.NotEmpty(t, a.why, key)
	}
}

// TestTelemetrySeamScanFlagsEveryCase asserts the scan reports a direct
// observer mention in a function that is not on the allowlist (also in a
// function literal, through an aliased or dot import, and in a package-level
// variable), accepts an allowlisted function with its exact count, reports a
// wrong count and an entry that matches nothing, and reports a missing
// helper, a helper that lacks either seam, a seam call whose restore is
// dropped and a seam that is only referenced. Files of the telemetry package
// itself are left out and CRLF source reads the same.
func TestTelemetrySeamScanFlagsEveryCase(t *testing.T) {
	t.Parallel()
	const helpers = `package cli

import "github.com/rancher/rancher-cli/internal/telemetry"

func recordingEnv(tb testing.TB) {
	tb.Cleanup(telemetry.SetDistinctSlotsForTest())
	tb.Cleanup(telemetry.SetFixedClockForTest())
}

func observeFiles(tb testing.TB) {
	tb.Cleanup(telemetry.SetDistinctSlotsForTest())
	tb.Cleanup(telemetry.SetFixedClockForTest())
	tb.Cleanup(telemetry.SetObserverForTest(nil))
	func() { telemetry.SetObserverForTest(nil) }()
}
`
	check := func(sources map[string]string, allow map[string]requestAllowance) []string {
		t.Helper()
		problems, err := checkTelemetrySeams(sources, allow)
		require.NoError(t, err)
		return problems
	}
	withHelpers := func(name, src string) map[string]string {
		return map[string]string{"internal/cli/helpers_test.go": helpers, name: src}
	}
	helpersWith := func(old, replacement string) map[string]string {
		changed := strings.Replace(helpers, old, replacement, 1)
		require.NotEqual(t, helpers, changed)
		return map[string]string{"internal/cli/helpers_test.go": changed}
	}

	assert.Empty(t, check(map[string]string{"internal/cli/helpers_test.go": helpers}, nil),
		"both helpers with both seams, and the observer set inside observeFiles, pass")

	const direct = `package cli

import "github.com/rancher/rancher-cli/internal/telemetry"

func TestDirect(t *testing.T) {
	telemetry.SetObserverForTest(nil)
}
`
	problems := check(withHelpers("internal/cli/direct_test.go", direct), nil)
	require.Len(t, problems, 1)
	assert.Contains(t, problems[0], "internal/cli/direct_test.go:6:")
	assert.Contains(t, problems[0], "observeFiles")

	const aliased = `package cli

import tel "github.com/rancher/rancher-cli/internal/telemetry"

func TestAliased(t *testing.T) {
	tel.SetObserverForTest(nil)
}
`
	const literal = `package cli

import "github.com/rancher/rancher-cli/internal/telemetry"

func TestLiteral(t *testing.T) {
	t.Run("x", func(t *testing.T) { telemetry.SetObserverForTest(nil) })
}

var global = telemetry.SetObserverForTest
`
	assert.Len(t, check(withHelpers("internal/cli/aliased_test.go", aliased), nil), 1, "an aliased import")
	assert.Len(t, check(withHelpers("internal/cli/literal_test.go", literal), nil), 2, "a function literal and a package-level value")

	assert.Empty(t, check(withHelpers("internal/telemetry/own_test.go", direct), nil), "the telemetry package's own files are not scanned")
	assert.Len(t, check(withHelpers("internal/telemetrysend/own_test.go", direct), nil), 1, "a sibling directory with the same prefix is scanned")

	allow := map[string]requestAllowance{"internal/cli/direct_test.go:TestDirect": {1, "reason"}}
	assert.Empty(t, check(withHelpers("internal/cli/direct_test.go", direct), allow), "an allowlisted function with its exact count passes")

	allow["internal/cli/direct_test.go:TestDirect"] = requestAllowance{2, "reason"}
	problems = check(withHelpers("internal/cli/direct_test.go", direct), allow)
	require.Len(t, problems, 1)
	assert.Contains(t, problems[0], "the allowlist covers 2 match(es)")

	problems = check(map[string]string{"internal/cli/helpers_test.go": helpers}, map[string]requestAllowance{"internal/cli/gone_test.go:TestGone": {1, "reason"}})
	assert.Equal(t, []string{"allowlist entry internal/cli/gone_test.go:TestGone matched nothing: remove it"}, problems)

	crlf := strings.ReplaceAll(helpers, "\n", "\r\n")
	assert.Empty(t, check(map[string]string{"internal/cli/helpers_test.go": crlf}, nil), "CRLF source reads the same")

	problems = check(helpersWith("func observeFiles(tb testing.TB) {\n\ttb.Cleanup(telemetry.SetDistinctSlotsForTest())\n", "func observeFiles(tb testing.TB) {\n"), nil)
	assertOneHelperProblem(t, problems, observerHelperName, slotSeamName, "")

	problems = check(helpersWith("\ttb.Cleanup(telemetry.SetFixedClockForTest())\n}\n\nfunc observeFiles", "}\n\nfunc observeFiles"), nil)
	assertOneHelperProblem(t, problems, recordingHelperName, clockSeamName, "")

	problems = check(helpersWith("\ttb.Cleanup(telemetry.SetFixedClockForTest())\n}\n\nfunc observeFiles", "\ttelemetry.SetFixedClockForTest()\n}\n\nfunc observeFiles"), nil)
	assertOneHelperProblem(t, problems, recordingHelperName, clockSeamName, "a call whose restore is dropped")

	problems = check(helpersWith("\ttb.Cleanup(telemetry.SetFixedClockForTest())\n}\n\nfunc observeFiles", "\t_ = telemetry.SetFixedClockForTest\n}\n\nfunc observeFiles"), nil)
	assertOneHelperProblem(t, problems, recordingHelperName, clockSeamName, "a reference without a call")

	problems = check(helpersWith("\ttb.Cleanup(telemetry.SetFixedClockForTest())\n}\n\nfunc observeFiles", "\t_ = func() { tb.Cleanup(telemetry.SetFixedClockForTest()) }\n}\n\nfunc observeFiles"), nil)
	assertOneHelperProblem(t, problems, recordingHelperName, clockSeamName, "a Cleanup call inside a function literal that may never run")

	problems = check(helpersWith("func recordingEnv(", "func recordingEnvironment("), nil)
	assert.Equal(t, []string{"the function declaration recordingEnv is missing: the scan hard-codes the helper that turns the telemetry test seams on"}, problems)

	assert.Len(t, check(map[string]string{}, nil), 2, "no helper at all reports both")
}

// assertOneHelperProblem asserts problems holds exactly one problem: the
// helper declaration, named by file, line and column, does not pass seam to
// Cleanup.
func assertOneHelperProblem(t *testing.T, problems []string, helper, seam, msg string) {
	t.Helper()
	require.Len(t, problems, 1, msg)
	assert.Regexp(t, `^internal/cli/helpers_test\.go:\d+:\d+: `+helper+` does not pass telemetry\.`+seam+`\(\) to Cleanup$`, problems[0], msg)
}
