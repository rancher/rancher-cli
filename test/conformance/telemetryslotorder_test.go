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
// for the unobserved runs that seed a test; uploadEnv freezes the clock for
// the runs that upload. The frozen clock also keeps the read limit of the
// hourly pass (150 ms) from running out on a slow runner, which would leave
// an hour the pass meant to remove. The scan below fails when a test file
// outside internal/telemetry/ sets the observer anywhere else, when a helper
// stops calling the seams it needs, and when a function in a test file of
// internal/cli/ turns recording on without freezing the clock.
//
// The recording rule covers internal/cli/ only. The hourly pass runs inside
// telemetry.Record, whose one caller outside the telemetry package is
// internal/cli/record.go; a test in internal/telemetrysend or
// internal/commands/telemetrycmd cannot start the pass, and the telemetry
// package's own tests set their own clock.

const (
	observerSeamName    = "SetObserverForTest"
	slotSeamName        = "SetDistinctSlotsForTest"
	clockSeamName       = "SetFixedClockForTest"
	enabledSeamName     = "SetEnabledForTest"
	observerHelperName  = "observeFiles"
	recordingHelperName = "recordingEnv"
	uploadHelperName    = "uploadEnv"
	telemetryPackageDir = "internal/telemetry/"
	recordingScopeDir   = "internal/cli/"
)

// helperSeams maps each CLI test helper that turns the telemetry test seams on
// to the seams it must pass to a body-level Cleanup call. uploadEnv needs the
// clock only: it does not compare file operations, so event slots stay random.
var helperSeams = map[string][]string{
	observerHelperName:  {slotSeamName, clockSeamName},
	recordingHelperName: {slotSeamName, clockSeamName},
	uploadHelperName:    {clockSeamName},
}

// clockAllowlist maps "<file>:<func>" to the number of places that function
// turns recording on without freezing the clock, and why no hourly pass can
// run there. A different number fails the test.
var clockAllowlist = map[string]requestAllowance{}

// observerAllowlist maps "<file>:<func>" to the number of places that function
// sets the telemetry observer directly and why no operation list that holds
// an event write is compared there. A different number fails the test.
var observerAllowlist = map[string]requestAllowance{
	"internal/cli/refresh_signin_test.go:TestTheSignInRequestSharesOneDeadlineCountedFromItsCall": {2, "the observer only sleeps on a read; no list is compared"},
	"internal/cli/upload_test.go:TestPassRemovesAnOldHourWhenItsWriteOutlastsTheReadLimit":        {1, "the observer only sleeps on a write; no list is compared"},
	"internal/cli/upload_test.go:TestUploadStaysInTheDeadline":                                    {1, "the observer only sleeps on a read; no list is compared"},
	"internal/cli/uploadcommand_test.go:TestUploadCommandIsBounded":                               {1, "the observer only sleeps on a read; no list is compared"},
	"internal/telemetrysend/sendonce_test.go:TestUploadOncePerHour":                               {1, "Send writes no event, and the test checks the next attempt against the real clock"},
	"internal/telemetrysend/sendonce_test.go:TestWalkAfterTheDeletionRunsOnlyWithTimeLeft":        {1, "the observer only moves the test clock on a read; no list is compared"},
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

// bodyCleanupSeams returns the names of the calls that are arguments of a
// Cleanup call made as a statement of fd's body itself: the restore is kept,
// and the call runs whenever the function does.
func bodyCleanupSeams(fd *ast.FuncDecl) map[string]bool {
	seams := map[string]bool{}
	if fd.Body == nil {
		return seams
	}
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
	return seams
}

// bodyCallsHelper reports whether a statement of fd's body itself (an
// expression statement, an assignment or a variable declaration, not one
// inside a nested block or a function literal) calls recordingEnv,
// observeFiles or uploadEnv: the call runs whenever the function does.
func bodyCallsHelper(fd *ast.FuncDecl) bool {
	if fd.Body == nil {
		return false
	}
	isHelperCall := func(e ast.Expr) bool {
		call, ok := ast.Unparen(e).(*ast.CallExpr)
		if !ok {
			return false
		}
		_, ok = helperSeams[calledName(call.Fun)]
		return ok
	}
	for _, stmt := range fd.Body.List {
		switch x := stmt.(type) {
		case *ast.ExprStmt:
			if isHelperCall(x.X) {
				return true
			}
		case *ast.AssignStmt:
			if slices.ContainsFunc(x.Rhs, isHelperCall) {
				return true
			}
		case *ast.DeclStmt:
			if gd, ok := x.Decl.(*ast.GenDecl); ok {
				for _, spec := range gd.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok && slices.ContainsFunc(vs.Values, isHelperCall) {
						return true
					}
				}
			}
		}
	}
	return false
}

// scanTelemetrySeams returns, for f:
//   - every mention of the observer setter outside the function observeFiles,
//     keyed by file and enclosing declaration (a function literal belongs to
//     its enclosing declaration);
//   - for each declaration of a helper in helperSeams, keyed by its position
//     and name, the seams whose call is an argument of a Cleanup call made as a
//     statement of the function body itself;
//   - when the file is in internal/cli/, every mention of the setter that turns
//     recording on, keyed the same way, in a declaration that neither passes
//     the clock seam to a body-level Cleanup nor calls one of the helpers as
//     a statement of the function body itself (a call in a subtest or a
//     function literal does not count: the code before it runs without the
//     clock). The helpers themselves are left to the rule above.
//
// Matching is by identifier name, which covers a qualified, aliased and dot
// imported use alike.
func scanTelemetrySeams(fset *token.FileSet, filePath string, f *ast.File) (observer []requestMatch, helpers map[string]map[string]bool, recording []requestMatch) {
	helpers = map[string]map[string]bool{}
	inScope := strings.HasPrefix(filePath, recordingScopeDir)
	for _, decl := range f.Decls {
		fn := "(package level)"
		fd, isFunc := decl.(*ast.FuncDecl)
		if isFunc {
			fn = fd.Name.Name
		}
		_, isHelper := helperSeams[fn]
		isHelper = isHelper && isFunc
		var seams map[string]bool
		callsHelper := false
		if isFunc {
			seams = bodyCleanupSeams(fd)
			callsHelper = bodyCallsHelper(fd)
		}
		if isHelper && fd.Body != nil {
			helpers[fset.Position(fd.Pos()).String()+": "+fn] = seams
		}
		var enabled []requestMatch
		ast.Inspect(decl, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if x.Name == observerSeamName && fn != observerHelperName {
					observer = append(observer, requestMatch{
						key:  filePath + ":" + fn,
						text: fset.Position(x.Pos()).String() + ": the telemetry observer is set outside observeFiles; use observeFiles, which also turns on in-order event slots and the frozen clock, or add the function to the allowlist with the reason no operation list is compared",
					})
				}
				if x.Name == enabledSeamName {
					enabled = append(enabled, requestMatch{
						key:  filePath + ":" + fn,
						text: fset.Position(x.Pos()).String() + ": recording is turned on without the frozen telemetry clock; pass telemetry.SetFixedClockForTest() to a Cleanup call in the function body, call recordingEnv, observeFiles or uploadEnv, or add the function to the clock allowlist with the reason no hourly pass can run",
					})
				}
			}
			return true
		})
		if inScope && !isHelper && !callsHelper && !seams[clockSeamName] {
			recording = append(recording, enabled...)
		}
	}
	return observer, helpers, recording
}

// checkTelemetrySeams parses every _test.go source in sources (path to
// content) outside the telemetry package's directory and returns the problems
// found against the two allowlists: an observer setter outside observeFiles
// and allow, a function in internal/cli/ that turns recording on without the
// frozen clock and outside clockAllow, an allowlist entry that no longer
// matches, and a helper that is missing or does not pass its seams to
// Cleanup.
func checkTelemetrySeams(sources map[string]string, allow, clockAllow map[string]requestAllowance) ([]string, error) {
	fset := token.NewFileSet()
	var matches, recordingMatches []requestMatch
	helpers := map[string]map[string]bool{}
	for _, path := range slices.Sorted(maps.Keys(sources)) {
		if strings.HasPrefix(path, telemetryPackageDir) || !strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, sources[path], parser.AllErrors)
		if err != nil {
			return nil, err
		}
		m, h, r := scanTelemetrySeams(fset, path, f)
		matches = append(matches, m...)
		recordingMatches = append(recordingMatches, r...)
		maps.Copy(helpers, h)
	}
	problems := checkRequestMatches(matches, allow)
	problems = append(problems, checkRequestMatches(recordingMatches, clockAllow)...)
	for _, want := range slices.Sorted(maps.Keys(helperSeams)) {
		found := false
		for _, key := range slices.Sorted(maps.Keys(helpers)) {
			if !strings.HasSuffix(key, ": "+want) {
				continue
			}
			found = true
			for _, seam := range helperSeams[want] {
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
// through observeFiles or from the allowlist, that observeFiles and
// recordingEnv each pass both seams' restore functions to Cleanup and
// uploadEnv passes the clock's, and that every function in a test file of
// internal/cli/ that turns recording on freezes the clock, calls one of the
// helpers or is on the clock allowlist.
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
	problems, err := checkTelemetrySeams(sources, observerAllowlist, clockAllowlist)
	require.NoError(t, err)
	for _, problem := range problems {
		t.Error(problem)
	}
	for _, allow := range []map[string]requestAllowance{observerAllowlist, clockAllowlist} {
		for key, a := range allow {
			assert.NotEmpty(t, a.why, key)
		}
	}
}

// TestTelemetrySeamScanFlagsEveryCase asserts the scan reports a direct
// observer mention in a function that is not on the allowlist (also in a
// function literal, through an aliased or dot import, and in a package-level
// variable), accepts an allowlisted function with its exact count, reports a
// wrong count and an entry that matches nothing, and reports a missing
// helper, a helper that lacks a seam it needs (uploadEnv needs the clock
// only), a seam call whose restore is dropped and a seam that is only
// referenced. It also asserts that a function in internal/cli/ that turns
// recording on is reported unless it passes the clock to a body-level Cleanup
// (not one in a function literal, not a bare call or a reference), calls
// recordingEnv, observeFiles or uploadEnv, or is on the clock allowlist with
// its exact count; that a wrong count and an entry matching nothing are
// reported; and that a file outside internal/cli/ is not scanned for it. Files
// of the telemetry package itself are left out and CRLF source reads the same.
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

func uploadEnv(t *testing.T) string {
	t.Cleanup(telemetry.SetEnabledForTest())
	t.Cleanup(telemetry.SetFixedClockForTest())
	return ""
}
`
	check := func(sources map[string]string, allow map[string]requestAllowance) []string {
		t.Helper()
		problems, err := checkTelemetrySeams(sources, allow, nil)
		require.NoError(t, err)
		return problems
	}
	checkClock := func(sources map[string]string, clockAllow map[string]requestAllowance) []string {
		t.Helper()
		problems, err := checkTelemetrySeams(sources, nil, clockAllow)
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
	assert.Equal(t, "internal/cli/direct_test.go:TestDirect: the allowlist covers 2 match(es) (reason), found 1: "+
		"internal/cli/direct_test.go:6:12: the telemetry observer is set outside observeFiles; use observeFiles, which also turns on in-order event slots and the frozen clock, or add the function to the allowlist with the reason no operation list is compared",
		problems[0])

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

	problems = check(helpersWith("\tt.Cleanup(telemetry.SetFixedClockForTest())\n\treturn", "\treturn"), nil)
	assertOneHelperProblem(t, problems, uploadHelperName, clockSeamName, "uploadEnv without the clock")

	problems = check(helpersWith("\tt.Cleanup(telemetry.SetFixedClockForTest())\n\treturn", "\ttelemetry.SetFixedClockForTest()\n\treturn"), nil)
	assertOneHelperProblem(t, problems, uploadHelperName, clockSeamName, "uploadEnv with a call whose restore is dropped")

	problems = check(helpersWith("\tt.Cleanup(telemetry.SetFixedClockForTest())\n\treturn", "\t_ = telemetry.SetFixedClockForTest\n\treturn"), nil)
	assertOneHelperProblem(t, problems, uploadHelperName, clockSeamName, "uploadEnv with a reference without a call")

	problems = check(helpersWith("\tt.Cleanup(telemetry.SetFixedClockForTest())\n\treturn", "\t_ = func() { t.Cleanup(telemetry.SetFixedClockForTest()) }\n\treturn"), nil)
	assertOneHelperProblem(t, problems, uploadHelperName, clockSeamName, "uploadEnv with a Cleanup call inside a function literal")

	problems = check(helpersWith("func uploadEnv(", "func uploadEnvironment("), nil)
	assert.Equal(t, []string{"the function declaration uploadEnv is missing: the scan hard-codes the helper that turns the telemetry test seams on"}, problems)

	assert.Len(t, check(map[string]string{}, nil), 3, "no helper at all reports all three")

	const enabledNoClock = `package cli

import "github.com/rancher/rancher-cli/internal/telemetry"

func TestEnabledNoClock(t *testing.T) {
	t.Cleanup(telemetry.SetEnabledForTest())
}
`
	const noClockText = ": recording is turned on without the frozen telemetry clock; pass telemetry.SetFixedClockForTest() to a Cleanup call in the function body, call recordingEnv, observeFiles or uploadEnv, or add the function to the clock allowlist with the reason no hourly pass can run"
	const noClockMessage = `^internal/cli/enabled_test\.go:6:\d+: recording is turned on without the frozen telemetry clock; pass telemetry\.SetFixedClockForTest\(\) to a Cleanup call in the function body, call recordingEnv, observeFiles or uploadEnv, or add the function to the clock allowlist with the reason no hourly pass can run$`
	problems = checkClock(withHelpers("internal/cli/enabled_test.go", enabledNoClock), nil)
	require.Len(t, problems, 1)
	assert.Regexp(t, noClockMessage, problems[0], "a function that turns recording on without the clock")

	assert.Empty(t, checkClock(withHelpers("internal/cli/enabled_test.go", strings.Replace(enabledNoClock, "\tt.Cleanup(telemetry.SetEnabledForTest())\n", "\tt.Cleanup(telemetry.SetEnabledForTest())\n\tt.Cleanup(telemetry.SetFixedClockForTest())\n", 1)), nil),
		"the clock in a body-level Cleanup")
	assert.Empty(t, checkClock(withHelpers("internal/cli/enabled_test.go", strings.Replace(enabledNoClock, "\tt.Cleanup(telemetry.SetEnabledForTest())\n", "\tt.Cleanup(telemetry.SetFixedClockForTest())\n\tt.Cleanup(telemetry.SetEnabledForTest())\n", 1)), nil),
		"the clock before the setter")
	for _, helper := range []string{recordingHelperName, observerHelperName, uploadHelperName} {
		for _, form := range []string{"\t%s(t)\n", "\tdir := %s(t)\n", "\t_ = %s(t)\n", "\tvar dir = %s(t)\n"} {
			assert.Empty(t, checkClock(withHelpers("internal/cli/enabled_test.go", strings.Replace(enabledNoClock, "\tt.Cleanup(telemetry.SetEnabledForTest())\n", "\tt.Cleanup(telemetry.SetEnabledForTest())\n"+fmt.Sprintf(form, helper), 1)), nil),
				"a body-level statement that calls "+helper+": "+form)
		}
	}

	for name, body := range map[string]string{
		"a bare call":                     "\ttelemetry.SetFixedClockForTest()\n",
		"a reference":                     "\t_ = telemetry.SetFixedClockForTest\n",
		"a function literal":              "\t_ = func() { t.Cleanup(telemetry.SetFixedClockForTest()) }\n",
		"a helper call in a subtest":      "\tt.Run(\"x\", func(t *testing.T) { uploadEnv(t) })\n",
		"a helper call in a closure":      "\t_ = func() { observeFiles(t) }\n",
		"a helper call in a nested block": "\tif t != nil {\n\t\trecordingEnv(t)\n\t}\n",
		"a helper reference":              "\t_ = uploadEnv\n",
	} {
		src := strings.Replace(enabledNoClock, "\tt.Cleanup(telemetry.SetEnabledForTest())\n", "\tt.Cleanup(telemetry.SetEnabledForTest())\n"+body, 1)
		problems = checkClock(withHelpers("internal/cli/enabled_test.go", src), nil)
		require.Len(t, problems, 1, name)
		assert.Regexp(t, noClockMessage, problems[0], name)
	}

	const enabledTwice = `package cli

import tel "github.com/rancher/rancher-cli/internal/telemetry"

func TestEnabledTwice(t *testing.T) {
	t.Cleanup(tel.SetEnabledForTest())
	t.Run("x", func(t *testing.T) { t.Cleanup(tel.SetEnabledForTest()) })
}

var global = tel.SetEnabledForTest
`
	problems = checkClock(withHelpers("internal/cli/twice_test.go", enabledTwice), nil)
	require.Len(t, problems, 3, "an aliased import, a function literal and a package-level value")
	assert.Equal(t, []string{
		"internal/cli/twice_test.go:10:18" + noClockText,
		"internal/cli/twice_test.go:6:16" + noClockText,
		"internal/cli/twice_test.go:7:48" + noClockText,
	}, problems, "a package-level value, then the function's two mentions in source order")

	assert.Empty(t, checkClock(withHelpers("internal/telemetrysend/enabled_test.go", enabledNoClock), nil), "a file outside internal/cli/ is not scanned for the frozen clock")
	assert.Empty(t, checkClock(withHelpers("internal/telemetry/enabled_test.go", enabledNoClock), nil), "the telemetry package's own files are not scanned")
	assert.Empty(t, checkClock(map[string]string{"internal/cli/helpers_test.go": helpers}, nil), "the helpers themselves are left to the helper rule")

	clockAllow := map[string]requestAllowance{"internal/cli/enabled_test.go:TestEnabledNoClock": {1, "reason"}}
	assert.Empty(t, checkClock(withHelpers("internal/cli/enabled_test.go", enabledNoClock), clockAllow), "an allowlisted function with its exact count passes")

	clockAllow["internal/cli/enabled_test.go:TestEnabledNoClock"] = requestAllowance{2, "reason"}
	problems = checkClock(withHelpers("internal/cli/enabled_test.go", enabledNoClock), clockAllow)
	require.Len(t, problems, 1)
	assert.Equal(t, "internal/cli/enabled_test.go:TestEnabledNoClock: the allowlist covers 2 match(es) (reason), found 1: internal/cli/enabled_test.go:6:22"+noClockText, problems[0])

	problems = checkClock(map[string]string{"internal/cli/helpers_test.go": helpers}, map[string]requestAllowance{"internal/cli/gone_test.go:TestGone": {1, "reason"}})
	assert.Equal(t, []string{"allowlist entry internal/cli/gone_test.go:TestGone matched nothing: remove it"}, problems)

	clockAllow["internal/cli/enabled_test.go:TestEnabledNoClock"] = requestAllowance{1, "reason"}
	assert.Empty(t, checkClock(withHelpers("internal/cli/enabled_test.go", strings.ReplaceAll(enabledNoClock, "\n", "\r\n")), clockAllow), "CRLF source reads the same")
	problems = checkClock(withHelpers("internal/cli/enabled_test.go", strings.ReplaceAll(enabledNoClock, "\n", "\r\n")), nil)
	require.Len(t, problems, 1)
	assert.Regexp(t, noClockMessage, problems[0], "CRLF source reports the same line")
}

// assertOneHelperProblem asserts problems holds exactly one problem: the
// helper declaration, named by file, line and column, does not pass seam to
// Cleanup.
func assertOneHelperProblem(t *testing.T, problems []string, helper, seam, msg string) {
	t.Helper()
	require.Len(t, problems, 1, msg)
	assert.Regexp(t, `^internal/cli/helpers_test\.go:\d+:\d+: `+helper+` does not pass telemetry\.`+seam+`\(\) to Cleanup$`, problems[0], msg)
}
