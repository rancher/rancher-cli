package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An upload test that sets a deadline of its own and then needs work to finish
// before that deadline fails on a slow runner for a reason no change under test
// causes. The helper input in internal/telemetrysend gives every attempt a
// deadline no runner reaches, and a test that needs the time to run out moves
// the clock of the deadline checks (stopClock, SetClockForTest) instead of
// waiting for it. The scan below fails when a test file of the upload code sets
// a deadline anywhere else, unless the function is on the allowlist with the
// reason a real deadline is needed there.
//
// The scan reads syntax only. It does not see a deadline that a table field
// feeds into an assignment with no helper call, or a context with a timeout
// made in a file of internal/cli or internal/commands/telemetrycmd that
// mentions none of the upload names in uploadNames.

const (
	uploadInputHelper     = "internal/telemetrysend/sendhelpers_test.go:input"
	uploadBudgetHelper    = "internal/cli/refresh_helpers_test.go:shrinkBudget"
	uploadFarDeadlineName = "farDeadline"
	uploadDeadlineField   = "Deadline"
	retiredShortHelper    = "shortInput"
)

// uploadDeadlineDirs are the directories whose test files the scan covers.
var uploadDeadlineDirs = []string{"internal/telemetrysend/", "internal/cli/", "internal/commands/telemetrycmd/"}

// uploadDeadlineVars are the package variables a test assigns to change a
// deadline of the upload code.
var uploadDeadlineVars = []string{"uploadCommandBound", "factsBudget"}

// uploadDeadlineAllowlist maps "<file>:<func>" to the number of places that
// function sets a deadline and why a real deadline is needed there. A different
// number fails the test. A call of a function that sets a deadline counts in
// the caller.
var uploadDeadlineAllowlist = map[string]requestAllowance{
	"internal/telemetrysend/sendfiles_test.go:TestSendRefusesWhatItMust":      {1, "100 ms under a 1 s floor: Send returns at its first check on every runner"},
	"internal/telemetrysend/sendonce_test.go:TestUploadOncePerHour":           {1, "500 ms under a 1 s floor: Send returns at its first check on every runner"},
	"internal/telemetrysend/sendnow_test.go:TestUploadCommandIsBounded":       {2, "600 ms with the time up on every path and 100 ms under a 1 s floor: the result is the same on every runner"},
	"internal/cli/upload_test.go:TestUploadStaysInTheDeadline":                {2, "the real timer ends a hung request and the test asserts the elapsed time, so it needs a real wait; the request must start inside the hangBound limit and the elapsed time must stay under hangBound plus 10 s, on every platform (the Windows jobs run it); the other row leaves 50 ms, under the floor on any runner"},
	"internal/cli/upload_test.go:TestDebugLinesShowOnlyCountsOriginAndAnswer": {1, "the real timer ends a hung request and the test asserts an upper bound on the elapsed time, so it needs a real wait; the request must start inside the hangBound limit and the elapsed time must stay under hangBound plus 10 s, on every platform (the Windows jobs run it)"},
	"internal/cli/upload_test.go:releaseRequestRows":                          {3, "a 10 s deadline in a function literal and its two calls, in rows that assert no request is made"},
	"internal/cli/uploadcommand_test.go:TestUploadCommandIsBounded":           {4, "the shrink function and its three calls: in the row that never answers, the real timer ends a hung request and the test asserts the elapsed time, so it needs a real wait; the request must start inside the hangBound limit and the elapsed time must stay under hangBound plus 10 s, on every platform (the Windows jobs run it); the other two rows leave 50 ms under the floor or sleep past the limit before the sender starts, with the same result on every runner"},
	"internal/cli/uploadcommand_test.go:TestUploadCommandTakesNoConfigLock":   {1, "a 200 ms context that bounds a probe of the config lock inside the sender seam; no request or upload deadline depends on it"},
}

// hasAllowance reports whether key is on the allowlist.
func hasAllowance(allow map[string]requestAllowance, key string) bool {
	_, ok := allow[key]
	return ok
}

// cleanupLiterals returns the function literals that are an argument of a
// Cleanup call: an assignment there restores a value.
func cleanupLiterals(root ast.Node) map[*ast.FuncLit]bool {
	out := map[*ast.FuncLit]bool{}
	ast.Inspect(root, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && calledName(call.Fun) == "Cleanup" {
			for _, arg := range call.Args {
				if lit, ok := arg.(*ast.FuncLit); ok {
					out[lit] = true
				}
			}
		}
		return true
	})
	return out
}

// uploadNames are the identifiers that show a file runs an upload: the sender
// entry points, the seams of the cli tests and their helpers.
var uploadNames = []string{"Send", "SendNow", "sendNow", "sendStoredEvents", "withEndpoint", "uploadEnv", "uploadArgv"}

// contextCalls are the context functions that end a request at a real time the
// clock of the attempt does not know.
var contextCalls = []string{"WithTimeout", "WithDeadline", "WithTimeoutCause", "WithDeadlineCause"}

// scanScope says what a file's scan matches beyond the deadline fields.
type scanScope struct {
	// contexts is true when a context with a timeout or deadline counts: in every
	// test file of internal/telemetrysend, and elsewhere in a file that mentions
	// an upload name.
	contexts bool
	// sendPackage is true for a file of package telemetrysend, where Input names
	// the sender's input without a qualifier.
	sendPackage bool
}

// scopeFor returns the scope of a file.
func scopeFor(path string, f *ast.File) scanScope {
	inSend := strings.HasPrefix(path, "internal/telemetrysend/")
	return scanScope{contexts: inSend || mentionsUpload(f), sendPackage: inSend}
}

// isSenderInput reports whether t names telemetrysend.Input: qualified, or
// unqualified in package telemetrysend.
func (sc scanScope) isSenderInput(t ast.Expr) bool {
	switch x := t.(type) {
	case *ast.Ident:
		return sc.sendPackage && x.Name == "Input"
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		return ok && id.Name == "telemetrysend" && x.Sel.Name == "Input"
	}
	return false
}

// mentionsUpload reports whether root uses one of the upload identifiers.
func mentionsUpload(root ast.Node) bool {
	found := false
	ast.Inspect(root, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && slices.Contains(uploadNames, id.Name) {
			found = true
		}
		return !found
	})
	return found
}

// firstElt returns the first element of a composite literal, or nil.
func firstElt(lit *ast.CompositeLit) ast.Expr {
	if len(lit.Elts) == 0 {
		return nil
	}
	return lit.Elts[0]
}

// directDeadlines returns the places in root that set an upload deadline:
//   - a composite literal key Deadline;
//   - a literal of telemetrysend.Input with positional fields;
//   - an assignment to a selector Deadline;
//   - an assignment to one of the deadline variables, in a tuple too;
//   - a call of a function in contextCalls, when the scope counts contexts.
//
// An assignment in a function literal passed to Cleanup restores a value and
// is left out.
func directDeadlines(root ast.Node, sc scanScope) []token.Pos {
	restores := cleanupLiterals(root)
	var out []token.Pos
	var walk func(n ast.Node, inRestore bool)
	walk = func(n ast.Node, inRestore bool) {
		ast.Inspect(n, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncLit:
				if restores[x] && !inRestore {
					walk(x.Body, true)
					return false
				}
			case *ast.KeyValueExpr:
				if id, ok := x.Key.(*ast.Ident); ok && id.Name == uploadDeadlineField {
					out = append(out, x.Pos())
				}
			case *ast.CompositeLit:
				if _, keyed := firstElt(x).(*ast.KeyValueExpr); sc.isSenderInput(x.Type) && len(x.Elts) > 0 && !keyed {
					out = append(out, x.Pos())
				}
			case *ast.CallExpr:
				if sc.contexts && slices.Contains(contextCalls, calledName(x.Fun)) {
					out = append(out, x.Pos())
				}
			case *ast.AssignStmt:
				if inRestore {
					return true
				}
				for _, lhs := range x.Lhs {
					switch l := ast.Unparen(lhs).(type) {
					case *ast.SelectorExpr:
						if l.Sel.Name == uploadDeadlineField {
							out = append(out, x.Pos())
						}
					case *ast.Ident:
						if slices.Contains(uploadDeadlineVars, l.Name) {
							out = append(out, x.Pos())
						}
					}
				}
			}
			return true
		})
	}
	walk(root, false)
	return out
}

// localHelpers returns the names of the local variables of decl bound to a
// function literal that sets a deadline.
func localHelpers(decl ast.Decl, sc scanScope) map[string]bool {
	out := map[string]bool{}
	bind := func(name ast.Expr, value ast.Expr) {
		id, ok := name.(*ast.Ident)
		lit, isLit := value.(*ast.FuncLit)
		if ok && isLit && len(directDeadlines(lit, sc)) > 0 {
			out[id.Name] = true
		}
	}
	ast.Inspect(decl, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if len(x.Lhs) == len(x.Rhs) {
				for i := range x.Lhs {
					bind(x.Lhs[i], x.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			if len(x.Names) == len(x.Values) {
				for i := range x.Names {
					bind(x.Names[i], x.Values[i])
				}
			}
		}
		return true
	})
	return out
}

// declKey returns the key of a declaration: its name, or "(package level)".
func declKey(decl ast.Decl) string {
	if fd, ok := decl.(*ast.FuncDecl); ok {
		return fd.Name.Name
	}
	return "(package level)"
}

// isFarDeadline reports whether e reads time.Now().Add(farDeadline).
func isFarDeadline(e ast.Expr) bool {
	add, ok := ast.Unparen(e).(*ast.CallExpr)
	if !ok || calledName(add.Fun) != "Add" || len(add.Args) != 1 {
		return false
	}
	sel, ok := ast.Unparen(add.Fun).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	now, ok := ast.Unparen(sel.X).(*ast.CallExpr)
	if !ok || calledName(now.Fun) != "Now" {
		return false
	}
	id, ok := ast.Unparen(add.Args[0]).(*ast.Ident)
	return ok && id.Name == uploadFarDeadlineName
}

// isMinutes reports whether e is an integer literal of at least two times
// time.Minute or time.Hour, as in 5 * time.Minute.
func isMinutes(e ast.Expr) bool {
	bin, ok := ast.Unparen(e).(*ast.BinaryExpr)
	if !ok || bin.Op != token.MUL {
		return false
	}
	lit, ok := ast.Unparen(bin.X).(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return false
	}
	n, err := strconv.Atoi(lit.Value)
	unit := calledName(bin.Y)
	return err == nil && n >= 2 && (unit == "Minute" || unit == "Hour")
}

// checkInputHelper returns the problems with the sanctioned deadline helper
// of internal/telemetrysend: it must exist, give its Deadline the value
// time.Now().Add(farDeadline), and farDeadline must be minutes.
func checkInputHelper(fset *token.FileSet, files map[string]*ast.File) []string {
	path, name, _ := strings.Cut(uploadInputHelper, ":")
	f := files[path]
	if f == nil {
		return []string{"the file " + path + " is missing: the scan hard-codes the sanctioned deadline helper input"}
	}
	var problems []string
	found, valueOK, constOK := false, false, false
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name.Name != name || d.Body == nil {
				continue
			}
			found = true
			ast.Inspect(d, func(n ast.Node) bool {
				if kv, ok := n.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok && id.Name == uploadDeadlineField && isFarDeadline(kv.Value) {
						valueOK = true
					}
				}
				return true
			})
			if n := len(directDeadlines(d, scopeFor(path, f))); n != 1 {
				problems = append(problems, fset.Position(d.Pos()).String()+": "+name+" must hold exactly one place that sets a deadline, found "+strconv.Itoa(n))
			}
			if !valueOK {
				problems = append(problems, fset.Position(d.Pos()).String()+": "+name+" must give Deadline the value time.Now().Add("+uploadFarDeadlineName+")")
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					for i, n := range vs.Names {
						if n.Name == uploadFarDeadlineName && i < len(vs.Values) && isMinutes(vs.Values[i]) {
							constOK = true
						}
					}
				}
			}
		}
	}
	if !found {
		problems = append(problems, "the function declaration "+uploadInputHelper+" is missing: the scan hard-codes the sanctioned deadline helper")
	}
	if !constOK {
		problems = append(problems, path+": "+uploadFarDeadlineName+" must be declared as a whole number of minutes or hours, at least 2")
	}
	return problems
}

// checkUploadDeadlines parses every _test.go source in sources (path to
// content) below the upload code's directories and returns the problems found
// against allow: a place that sets a deadline outside the allowlist, an
// allowlist entry whose count differs or that matches nothing, and a sanctioned
// helper that is missing or changed.
func checkUploadDeadlines(sources map[string]string, allow map[string]requestAllowance) ([]string, error) {
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, path := range slices.Sorted(maps.Keys(sources)) {
		if !strings.HasSuffix(path, "_test.go") || !slices.ContainsFunc(uploadDeadlineDirs, func(dir string) bool { return strings.HasPrefix(path, dir) }) {
			continue
		}
		f, err := parser.ParseFile(fset, path, sources[path], parser.AllErrors)
		if err != nil {
			return nil, err
		}
		files[path] = f
	}
	sanctioned := map[string]bool{uploadInputHelper: true, uploadBudgetHelper: true}
	// The top-level functions that set a deadline, outside the sanctioned ones
	// and the allowlist: a call of an allowlisted function is covered by its
	// reason.
	helpers := map[string]bool{retiredShortHelper: true}
	for _, path := range slices.Sorted(maps.Keys(files)) {
		for _, decl := range files[path].Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && !sanctioned[path+":"+fd.Name.Name] && !hasAllowance(allow, path+":"+fd.Name.Name) && len(directDeadlines(decl, scopeFor(path, files[path]))) > 0 {
				helpers[fd.Name.Name] = true
			}
		}
	}
	var matches []requestMatch
	var problems []string
	for _, path := range slices.Sorted(maps.Keys(files)) {
		for _, decl := range files[path].Decls {
			key := path + ":" + declKey(decl)
			if sanctioned[key] {
				continue
			}
			for _, pos := range directDeadlines(decl, scopeFor(path, files[path])) {
				matches = append(matches, requestMatch{key, fset.Position(pos).String() + ": a test sets an upload deadline; use input(), whose deadline no runner reaches, and move the clock with stopClock, or add the function to the allowlist with the reason a real deadline is needed"})
			}
			local := localHelpers(decl, scopeFor(path, files[path]))
			callsBudget, callsEndpoint := false, false
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch name := calledName(call.Fun); {
				case helpers[name] || local[name]:
					matches = append(matches, requestMatch{key, fset.Position(call.Pos()).String() + ": a call of " + name + ", which sets an upload deadline; use input() and the test clock, or add the function to the allowlist with the reason a real deadline is needed"})
				case name == "shrinkBudget":
					callsBudget = true
				case name == "withEndpoint":
					callsEndpoint = true
				}
				return true
			})
			if callsBudget && callsEndpoint {
				problems = append(problems, key+": shrinkBudget shortens the facts refresh budget and must not run in a test that sets an upload endpoint")
			}
		}
	}
	problems = append(problems, checkRequestMatches(matches, allow)...)
	problems = append(problems, checkInputHelper(fset, files)...)
	if budgetPath, _, _ := strings.Cut(uploadBudgetHelper, ":"); files[budgetPath] == nil {
		problems = append(problems, "the file "+budgetPath+" is missing: the scan hard-codes the sanctioned deadline helper shrinkBudget")
	}
	return problems, nil
}

// TestUploadTestsSetNoDeadlineOfTheirOwn asserts the test files of
// internal/telemetrysend, internal/cli and internal/commands/telemetrycmd set an
// upload deadline only through the sanctioned helpers or from the allowlist, and
// that input gives its deadline the value time.Now().Add(farDeadline).
func TestUploadTestsSetNoDeadlineOfTheirOwn(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	sources := map[string]string{}
	for _, path := range files {
		if !strings.HasSuffix(path, "_test.go") || !slices.ContainsFunc(uploadDeadlineDirs, func(dir string) bool { return strings.HasPrefix(path, dir) }) {
			continue
		}
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		sources[path] = string(src)
	}
	problems, err := checkUploadDeadlines(sources, uploadDeadlineAllowlist)
	require.NoError(t, err)
	for _, problem := range problems {
		t.Error(problem)
	}
	for key, a := range uploadDeadlineAllowlist {
		assert.NotEmpty(t, a.why, key)
	}
}

// TestUploadDeadlineScanFlagsEveryCase asserts the scan reports:
//   - a Deadline key in a qualified and an unqualified Input literal, and a
//     positional literal of telemetrysend.Input (not another type named Input);
//   - an assignment to a Deadline field, a tuple assignment to a deadline
//     variable and a deadline set in a function literal;
//   - a context with a timeout or deadline (all four context functions) in any
//     test file of internal/telemetrysend, and elsewhere only in a file that
//     mentions an upload name, with a helper that returns one and its caller;
//   - a call of a helper or of a local function literal that sets a deadline,
//     and a call of shortInput.
//
// It leaves out an assignment that restores a value in Cleanup, and the callers
// of an allowlisted helper. It accepts an allowlisted function with its exact
// count and reports a wrong count and an entry that matches nothing. It accepts
// the sanctioned helpers and reports a changed value of input, a second
// deadline in input, a farDeadline that is not minutes, a missing helper and
// shrinkBudget in a test that sets an upload endpoint. Files outside the three
// directories are ignored, and CRLF source reads the same.
func TestUploadDeadlineScanFlagsEveryCase(t *testing.T) {
	t.Parallel()
	const helpers = `package telemetrysend

import "time"

const farDeadline = 5 * time.Minute

func input(dir, origin string) Input {
	return Input{Deadline: time.Now().Add(farDeadline)}
}
`
	const budget = `package cli

func shrinkBudget(t *testing.T, d time.Duration) {
	old := factsBudget
	factsBudget = d
	t.Cleanup(func() { factsBudget = old })
}
`
	base := func(extra map[string]string) map[string]string {
		out := map[string]string{
			"internal/telemetrysend/sendhelpers_test.go": helpers,
			"internal/cli/refresh_helpers_test.go":       budget,
		}
		maps.Copy(out, extra)
		return out
	}
	check := func(sources map[string]string, allow map[string]requestAllowance) []string {
		t.Helper()
		problems, err := checkUploadDeadlines(sources, allow)
		require.NoError(t, err)
		return problems
	}

	assert.Empty(t, check(base(nil), nil), "the sanctioned helpers pass")

	const qualified = `package telemetrysend

func TestQualified(t *testing.T) {
	_ = telemetrysend.Input{Deadline: time.Now().Add(time.Second)}
	_ = Input{Deadline: time.Now().Add(time.Second)}
}
`
	problems := check(base(map[string]string{"internal/telemetrysend/q_test.go": qualified}), nil)
	require.Len(t, problems, 2, "a qualified and an unqualified literal")
	assert.Contains(t, problems[0], "internal/telemetrysend/q_test.go:4:")
	assert.Contains(t, problems[1], "internal/telemetrysend/q_test.go:5:")

	const assigned = `package cli

func TestAssigned(t *testing.T) {
	in.Deadline = time.Now().Add(time.Second)
	uploadCommandBound, uploadFloor = time.Second, time.Second
	t.Run("x", func(t *testing.T) { short.Deadline = time.Now() })
	t.Cleanup(func() { factsBudget = old })
	t.Cleanup(func() { in.Deadline = old })
}
`
	problems = check(base(map[string]string{"internal/cli/a_test.go": assigned}), nil)
	require.Len(t, problems, 3, "a field, a tuple and a function literal; the Cleanup restores are left out")

	const caller = `package cli

func makeInput() Input { return Input{Deadline: time.Now()} }

func TestCaller(t *testing.T) {
	makeInput()
	shortInput(dir, url, time.Second)
	shrink := func() { factsBudget = time.Second }
	shrink()
	shrink()
}
`
	problems = check(base(map[string]string{"internal/cli/c_test.go": caller}), nil)
	assert.Len(t, problems, 1+1+1+1+2, "the helper body, the call of the helper, shortInput, the literal and its two calls")

	problems = check(base(map[string]string{"internal/cli/c_test.go": caller}), map[string]requestAllowance{"internal/cli/c_test.go:makeInput": {1, "reason"}})
	assert.Len(t, problems, 4, "a call of an allowlisted helper is covered by its reason; the others still count")

	const contexts = `package telemetrysend

func TestContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	Send(ctx, in)
	ctx, cancel = context.WithDeadline(t.Context(), time.Now())
	SendNow(ctx, in)
	ctx, cancel = context.WithTimeoutCause(t.Context(), time.Second, nil)
	ctx, cancel = context.WithDeadlineCause(t.Context(), time.Now(), nil)
}
`
	problems = check(base(map[string]string{"internal/telemetrysend/ctx_test.go": contexts}), nil)
	require.Len(t, problems, 4, "all four context functions in a test file of the sender")
	for i, line := range []string{"4", "6", "8", "9"} {
		assert.Contains(t, problems[i], "internal/telemetrysend/ctx_test.go:"+line+":")
	}

	const bare = `package telemetrysend

func boundedContext(t *testing.T) (context.Context, context.CancelFunc) {
	return context.WithTimeout(t.Context(), time.Second)
}
`
	const bareCaller = `package telemetrysend

func TestBounded(t *testing.T) {
	ctx, cancel := boundedContext(t)
	defer cancel()
	Send(ctx, input(dir, url))
}
`
	problems = check(base(map[string]string{
		"internal/telemetrysend/bare_test.go":   bare,
		"internal/telemetrysend/caller_test.go": bareCaller,
	}), nil)
	require.Len(t, problems, 2, "a helper that returns a short context, and the test that passes it to Send")
	assert.Contains(t, problems[0], "internal/telemetrysend/bare_test.go:4:")
	assert.Contains(t, problems[1], "internal/telemetrysend/caller_test.go:4:")
	assert.Contains(t, problems[1], "a call of boundedContext")

	const cliContext = `package cli

func TestCompletes(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	complete(ctx)
}
`
	const cliHelper = `package cli

func shortContext(t *testing.T) context.Context {
	ctx, _ := context.WithTimeout(t.Context(), time.Second)
	return ctx
}

func TestUploads(t *testing.T) {
	withEndpoint(t, "http://x")
}
`
	assert.Empty(t, check(base(map[string]string{"internal/cli/complete_test.go": cliContext}), nil), "a file with no upload name is not scanned for contexts")
	problems = check(base(map[string]string{"internal/cli/uploads_test.go": cliHelper}), nil)
	require.Len(t, problems, 1, "a file with an upload name is scanned in every declaration")
	assert.Contains(t, problems[0], "internal/cli/uploads_test.go:4:")

	const positional = `package cli

func TestPositional(t *testing.T) {
	_ = telemetrysend.Input{dir, "dev", origin, enc, nil, time.Now(), time.Second, nil, nil}
	_ = cmdtest.Input{dir, "dev"}
	_ = Input{dir}
	_ = Input{}
}
`
	problems = check(base(map[string]string{"internal/cli/p_test.go": positional}), nil)
	require.Len(t, problems, 1, "a positional literal of telemetrysend.Input; other types and unqualified Input in cli pass")
	assert.Contains(t, problems[0], "internal/cli/p_test.go:4:")

	const sendPositional = `package telemetrysend

func TestSendPositional(t *testing.T) {
	_ = Input{dir, "dev", origin}
	_ = telemetrysend.Input{dir}
	_ = Input{}
}
`
	problems = check(base(map[string]string{"internal/telemetrysend/p_test.go": sendPositional}), nil)
	require.Len(t, problems, 2, "an unqualified and a qualified positional literal in the sender's package; the empty literal passes")
	assert.Contains(t, problems[0], "internal/telemetrysend/p_test.go:4:")
	assert.Contains(t, problems[1], "internal/telemetrysend/p_test.go:5:")

	allow := map[string]requestAllowance{"internal/cli/a_test.go:TestAssigned": {3, "reason"}}
	assert.Empty(t, check(base(map[string]string{"internal/cli/a_test.go": assigned}), allow), "an allowlisted function with its exact count passes")

	allow["internal/cli/a_test.go:TestAssigned"] = requestAllowance{4, "reason"}
	problems = check(base(map[string]string{"internal/cli/a_test.go": assigned}), allow)
	require.Len(t, problems, 1)
	assert.Contains(t, problems[0], "the allowlist covers 4 match(es)")

	problems = check(base(nil), map[string]requestAllowance{"internal/cli/gone_test.go:TestGone": {1, "reason"}})
	assert.Equal(t, []string{"allowlist entry internal/cli/gone_test.go:TestGone matched nothing: remove it"}, problems)

	assert.Empty(t, check(base(map[string]string{
		"internal/telemetry/own_test.go":  qualified,
		"test/conformance/other_test.go":  qualified,
		"internal/clifake/other_test.go":  qualified,
		"internal/cli/not_a_test_file.go": qualified,
	}), nil), "files outside the three directories and non-test files are not scanned")
	assert.Len(t, check(base(map[string]string{"internal/commands/telemetrycmd/q_test.go": qualified}), nil), 2, "the command package is scanned")

	crlf := strings.ReplaceAll(helpers, "\n", "\r\n")
	assert.Empty(t, check(base(map[string]string{"internal/telemetrysend/sendhelpers_test.go": crlf}), nil), "CRLF source reads the same")

	changed := strings.Replace(helpers, "time.Now().Add(farDeadline)", "time.Now().Add(20 * time.Second)", 1)
	require.NotEqual(t, helpers, changed)
	problems = check(base(map[string]string{"internal/telemetrysend/sendhelpers_test.go": changed}), nil)
	require.Len(t, problems, 1)
	assert.Contains(t, problems[0], "input must give Deadline the value time.Now().Add(farDeadline)")

	second := strings.Replace(helpers, "return Input{Deadline: time.Now().Add(farDeadline)}", "in := Input{Deadline: time.Now().Add(farDeadline)}\n\tin.Deadline = time.Now().Add(time.Second)\n\treturn in", 1)
	require.NotEqual(t, helpers, second)
	problems = check(base(map[string]string{"internal/telemetrysend/sendhelpers_test.go": second}), nil)
	require.Len(t, problems, 1, "a second deadline assignment in input")
	assert.Contains(t, problems[0], "input must hold exactly one place that sets a deadline, found 2")

	for name, replacement := range map[string]string{
		"seconds": "const farDeadline = 20 * time.Second",
		"one":     "const farDeadline = 1 * time.Minute",
		"plain":   "const farDeadline = time.Minute",
	} {
		changed = strings.Replace(helpers, "const farDeadline = 5 * time.Minute", replacement, 1)
		problems = check(base(map[string]string{"internal/telemetrysend/sendhelpers_test.go": changed}), nil)
		require.Len(t, problems, 1, name)
		assert.Contains(t, problems[0], "farDeadline must be declared as a whole number of minutes or hours, at least 2", name)
	}

	problems = check(map[string]string{"internal/cli/refresh_helpers_test.go": budget}, nil)
	assert.Equal(t, []string{"the file internal/telemetrysend/sendhelpers_test.go is missing: the scan hard-codes the sanctioned deadline helper input"}, problems, "a missing file")

	renamed := strings.Replace(helpers, "func input(", "func inputs(", 1)
	problems = check(base(map[string]string{"internal/telemetrysend/sendhelpers_test.go": renamed}), nil)
	assert.Contains(t, problems, "the function declaration "+uploadInputHelper+" is missing: the scan hard-codes the sanctioned deadline helper")

	problems = check(map[string]string{"internal/telemetrysend/sendhelpers_test.go": helpers}, nil)
	assert.Equal(t, []string{"the file internal/cli/refresh_helpers_test.go is missing: the scan hard-codes the sanctioned deadline helper shrinkBudget"}, problems)

	const refresh = `package cli

func TestRefreshWithEndpoint(t *testing.T) {
	shrinkBudget(t, time.Second)
	withEndpoint(t, "http://x")
}

func TestRefreshOnly(t *testing.T) {
	shrinkBudget(t, time.Second)
}
`
	problems = check(base(map[string]string{"internal/cli/refresh_test.go": refresh}), nil)
	assert.Equal(t, []string{"internal/cli/refresh_test.go:TestRefreshWithEndpoint: shrinkBudget shortens the facts refresh budget and must not run in a test that sets an upload endpoint"}, problems)
}
