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
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// Every reader of a document from stdin drops every byte order mark at the
// start of stdin with engine.DropLeadingByteOrderMarks, and uses what it
// returns. The documents reach the commands through the FileSource port
// (internal/engine/client.go): its method Read takes a path, and "-" is
// stdin. The tests here find a reader by the signature of that method, a
// method Read(string) ([]byte, error). A reader that does not exist yet is
// found too:
//
//   - every use of a method named Read with that signature is in
//     stdinDocUseAllowlist with its exact count: a call, a method value and a
//     method expression;
//   - every declaration of a method with that signature, whatever its name, an
//     interface method included, is in stdinDocDeclAllowlist with a reason. A
//     second implementation of the port, or a new port named Load or
//     ReadFile, is found when it is declared;
//   - the function that an entry of stdinDocUseAllowlist names as its drop
//     function must call engine.DropLeadingByteOrderMarks (matched by the
//     function the name resolves to, not by its text) and use the result: the
//     result is the right-hand side of an assignment that does not discard it,
//     an argument of another call or an operand of a return.
//
// The behavior of each flag is asserted by the tables
// TestEveryStdinSecretDropsLeadingByteOrderMarks and
// TestEveryStdinDocumentDropsLeadingByteOrderMarks in internal/cli.
//
// Limits. A reader of the root input of the runtime (runtime.IO.In) that does
// not go through a method with this signature is not found; the scan of the
// other readers of stdin (TestEveryStdinReaderHasAnAllowlistEntry) covers the
// rest. A plain function with this signature (os.ReadFile) is not matched. A
// method in a file that builds for no platform but windows is found through
// the scan built for windows; a file that builds for neither the platform the
// test runs on nor windows is not found. The scan checks that the function an
// entry names as its drop function calls the helper and uses the result. It
// does not check that the reader is that function or calls it, that the use
// comes before the decode, or that it runs on every path: a reader that decodes
// without calling its drop function, a drop after the decode, or a drop in a
// branch that does not run passes, and the rows of the table catch it.

const (
	stdinDocFixtureDir = "test/conformance/testdata/stdindocuments"
	stdinDocEnginePath = modulePrefix + "internal/engine"
	stdinDocDropName   = "DropLeadingByteOrderMarks"
	stdinDocMethodName = "Read"

	stdinDocRuleUse      = "a use of a method Read(string) ([]byte, error) with no allowlist entry"
	stdinDocRuleCount    = "a use of a method Read(string) ([]byte, error) that the allowlist counts differently"
	stdinDocRuleDecl     = "a declaration of a method with the signature (string) ([]byte, error) and no allowlist entry"
	stdinDocRuleDrop     = "a function that reads a document and does not use the result of engine.DropLeadingByteOrderMarks"
	stdinDocRuleStale    = "an allowlist entry that matches nothing"
	stdinDocTableTest    = "internal/cli:TestEveryStdinDocumentDropsLeadingByteOrderMarks"
	stdinDocSecretTest   = "internal/cli:TestEveryStdinSecretDropsLeadingByteOrderMarks"
	stdinDocNotStdinText = "a file under the telemetry directory, not stdin"
)

// stdinDocUseEntry is one function of the module that uses a method named
// Read with the signature (string) ([]byte, error): how many times, why, the
// test that asserts the drop on its input, and its drop function.
type stdinDocUseEntry struct {
	count int
	why   string
	// row is "<package directory>:<test function>" of the test that asserts the
	// drop; empty only in a fixture table.
	row string
	// drops is the key of the drop function of the entry: a function of the
	// same package that calls engine.DropLeadingByteOrderMarks and uses the
	// result. It is the function of the entry or another function of its
	// package; the scan does not check that the reader calls it.
	drops string
}

// stdinDocUseAllowlist maps "<file>:<function>" (or "<file>:<Type>.<Method>")
// to the uses of a method named Read with the signature (string) ([]byte,
// error) in that function.
var stdinDocUseAllowlist = map[string]stdinDocUseEntry{
	"internal/engine/password.go:ReadPasswordStdin": {1, "reads the password a command takes on stdin", stdinDocSecretTest,
		"internal/engine/password.go:ReadPasswordStdin"},
	"internal/engine/password.go:ReadCurrentAndNewPasswordStdin": {1, "reads the two passwords a command takes on stdin", stdinDocSecretTest,
		"internal/engine/password.go:ReadCurrentAndNewPasswordStdin"},
	"internal/engine/catalog/values.go:ReadValues": {1, "reads every --values file, stdin included", stdinDocTableTest,
		"internal/engine/catalog/values.go:decodeValuesFile"},
	"internal/engine/resources/patch.go:patchCmd.decodePatch": {1, "reads --patch-file, stdin included", stdinDocTableTest,
		"internal/engine/resources/patch.go:patchCmd.decodePatch"},
	"internal/engine/resources/create.go:createCmd.readAndResolve": {1, "reads --filename, stdin included", stdinDocTableTest,
		"internal/engine/resources/create.go:createCmd.readAndResolve"},
	"internal/engine/provisioning/cloudcredwrite.go:createCloudCredentialCmd.readWords": {1, "reads every --from-file file of create cloud-credential, stdin included", stdinDocTableTest,
		"internal/engine/provisioning/cloudcredwrite.go:createCloudCredentialCmd.readWords"},
}

// stdinDocDeclAllowlist maps "<file>:<Type>.<Method>" to why a declaration of
// a method with the signature (string) ([]byte, error) is no reader that needs
// a drop.
var stdinDocDeclAllowlist = map[string]string{
	"internal/engine/client.go:FileSource.Read":  "the port; its one implementation is internal/cli/filesource.go",
	"internal/cli/filesource.go:fileSource.Read": "the one stdin source; the readers drop the marks",
	"internal/telemetry/store.go:Store.readFile": stdinDocNotStdinText,
	"internal/telemetry/fsroot.go:root.ReadFile": stdinDocNotStdinText,
}

// stdinDocFinding is one place the scan found: its position as "file:line:col"
// and the text that names it.
type stdinDocFinding struct {
	key, text string
}

// stdinDocResult is what a scan of one or more loads found. The maps are keyed
// by position: a file that several loads read counts once.
type stdinDocResult struct {
	uses  map[string]stdinDocFinding
	decls map[string]stdinDocFinding
	// drops holds the key of each function that calls
	// engine.DropLeadingByteOrderMarks and uses the result.
	drops map[string]bool
}

func newStdinDocResult() stdinDocResult {
	return stdinDocResult{uses: map[string]stdinDocFinding{}, decls: map[string]stdinDocFinding{}, drops: map[string]bool{}}
}

func (r stdinDocResult) merge(other stdinDocResult) {
	maps.Copy(r.uses, other.uses)
	maps.Copy(r.decls, other.decls)
	maps.Copy(r.drops, other.drops)
}

// stdinDocReadSignature reports whether sig is a method that takes one string
// and returns a byte slice and an error: the signature of the method Read of
// the FileSource port.
func stdinDocReadSignature(sig *types.Signature) bool {
	if sig == nil || sig.Recv() == nil || sig.Variadic() || sig.Params().Len() != 1 || sig.Results().Len() != 2 {
		return false
	}
	return types.Identical(sig.Params().At(0).Type().Underlying(), types.Typ[types.String]) &&
		types.Identical(sig.Results().At(0).Type(), types.NewSlice(types.Typ[types.Byte])) &&
		types.Identical(sig.Results().At(1).Type(), types.Universe.Lookup("error").Type())
}

// stdinDocFunctionName names a top-level declaration the way the allowlists
// do: Name, or Type.Name for a method.
func stdinDocFunctionName(decl ast.Decl) string {
	fd, ok := decl.(*ast.FuncDecl)
	if !ok {
		return "(package level)"
	}
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	expr := fd.Recv.List[0].Type
	for {
		switch t := expr.(type) {
		case *ast.StarExpr:
			expr = t.X
			continue
		case *ast.ParenExpr:
			expr = t.X
			continue
		case *ast.IndexExpr:
			expr = t.X
			continue
		case *ast.Ident:
			return t.Name + "." + fd.Name.Name
		}
		return fd.Name.Name
	}
}

// stdinDocReceiverName names the type a method is declared on.
func stdinDocReceiverName(fn *types.Func) string {
	sig, _ := fn.Type().(*types.Signature)
	if sig == nil || sig.Recv() == nil {
		return "(none)"
	}
	t := sig.Recv().Type()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	if named, ok := types.Unalias(t).(*types.Named); ok {
		return named.Obj().Name()
	}
	return "(anonymous)"
}

// stdinDocIsDrop reports whether call calls engine.DropLeadingByteOrderMarks.
// The callee is the function the name resolves to: a function of that name in
// another package does not match.
func stdinDocIsDrop(info *types.Info, call *ast.CallExpr) bool {
	var fn *types.Func
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		fn, _ = info.Uses[fun].(*types.Func)
	case *ast.SelectorExpr:
		fn, _ = info.Uses[fun.Sel].(*types.Func)
	}
	return fn != nil && fn.Name() == stdinDocDropName && fn.Pkg() != nil && fn.Pkg().Path() == stdinDocEnginePath
}

// stdinDocResultUsed reports whether the call at the top of stack has its
// result used: the right-hand side of an assignment or declaration that does
// not assign it to the blank identifier, an argument of another call, or an
// operand of a return. An expression statement, a blank assignment and any
// other place discard it.
func stdinDocResultUsed(stack []ast.Node) bool {
	child := stack[len(stack)-1]
	for i := len(stack) - 2; i >= 0; i-- {
		switch parent := stack[i].(type) {
		case *ast.ParenExpr:
			child = parent
			continue
		case *ast.AssignStmt:
			idx := slices.IndexFunc(parent.Rhs, func(e ast.Expr) bool { return e == child })
			if idx < 0 {
				return false
			}
			if len(parent.Lhs) == len(parent.Rhs) {
				if id, ok := parent.Lhs[idx].(*ast.Ident); ok && id.Name == "_" {
					return false
				}
			}
			return true
		case *ast.ValueSpec:
			idx := slices.IndexFunc(parent.Values, func(e ast.Expr) bool { return e == child })
			if idx < 0 {
				return false
			}
			return len(parent.Names) != len(parent.Values) || parent.Names[idx].Name != "_"
		case *ast.CallExpr:
			return slices.ContainsFunc(parent.Args, func(e ast.Expr) bool { return e == child })
		case *ast.ReturnStmt:
			return true
		}
		return false
	}
	return false
}

// stdinDocScan returns what the non-test files of pkg hold: every use of a
// method named Read with the signature (string) ([]byte, error), every
// declaration of a method with that signature, and every function that calls
// the drop and uses the result.
func stdinDocScan(pkg *packages.Package, wd string) stdinDocResult {
	info := pkg.TypesInfo
	res := newStdinDocResult()
	position := func(pos token.Pos) (string, string) {
		p := pkg.Fset.Position(pos)
		rel := byIDRel(pkg, pos, wd)
		return fmt.Sprintf("%s:%d:%d", rel, p.Line, p.Column), rel
	}
	for _, f := range pkg.Syntax {
		if strings.HasSuffix(pkg.Fset.Position(f.Pos()).Filename, "_test.go") {
			continue
		}
		_, rel := position(f.Pos())
		for _, decl := range f.Decls {
			key := rel + ":" + stdinDocFunctionName(decl)
			var stack []ast.Node
			ast.Inspect(decl, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				switch n := n.(type) {
				case *ast.Ident:
					if fn, ok := info.Uses[n].(*types.Func); ok && fn.Name() == stdinDocMethodName {
						if sig, _ := fn.Type().(*types.Signature); stdinDocReadSignature(sig) {
							at, _ := position(n.Pos())
							res.uses[at] = stdinDocFinding{key, at + ": a use of the method Read(string) ([]byte, error) in " + key}
						}
					}
					if fn, ok := info.Defs[n].(*types.Func); ok {
						if sig, _ := fn.Type().(*types.Signature); stdinDocReadSignature(sig) {
							at, _ := position(n.Pos())
							declKey := rel + ":" + stdinDocReceiverName(fn) + "." + fn.Name()
							res.decls[at] = stdinDocFinding{declKey, at + ": a declaration of the method " + fn.Name() + " with the signature (string) ([]byte, error) on " + stdinDocReceiverName(fn)}
						}
					}
				case *ast.CallExpr:
					if stdinDocIsDrop(info, n) && stdinDocResultUsed(stack) {
						res.drops[key] = true
					}
				}
				return true
			})
		}
	}
	return res
}

// stdinDocLoads scans the module for the platform the test runs on and for
// windows, once per process, and merges the results by position.
var stdinDocLoads = sync.OnceValues(func() (stdinDocResult, error) {
	pkgs, err := fieldManagerPackages()
	if err != nil {
		return stdinDocResult{}, err
	}
	wd, err := os.Getwd()
	if err != nil {
		return stdinDocResult{}, err
	}
	merged := newStdinDocResult()
	for _, pkg := range pkgs {
		merged.merge(stdinDocScan(pkg, wd))
	}
	windows, err := otherGOOSPackages("windows")
	if err != nil {
		return stdinDocResult{}, err
	}
	for _, pkg := range windows {
		merged.merge(stdinDocScan(pkg, wd))
	}
	return merged, nil
})

// stdinDocCheck returns one problem per finding of res that no allowlist
// entry covers, one per use entry whose count differs from the uses it covers,
// one per use entry whose drop function does not drop, and one per entry
// that matches nothing. Each problem starts with its rule.
func stdinDocCheck(res stdinDocResult, uses map[string]stdinDocUseEntry, decls map[string]string) []string {
	var problems []string
	byKey := map[string][]string{}
	for _, at := range slices.Sorted(maps.Keys(res.uses)) {
		f := res.uses[at]
		byKey[f.key] = append(byKey[f.key], f.text)
	}
	for _, key := range slices.Sorted(maps.Keys(byKey)) {
		entry, ok := uses[key]
		switch {
		case !ok:
			for _, text := range byKey[key] {
				problems = append(problems, stdinDocRuleUse+": "+text+"; the function must drop every byte order mark at the start of stdin with engine.DropLeadingByteOrderMarks and use the result, then get an entry in stdinDocUseAllowlist that names the function that drops and the test that asserts it")
			}
		case len(byKey[key]) != entry.count:
			problems = append(problems, fmt.Sprintf("%s: %s: the allowlist covers %d use(s) (%s) and the scan found %d: %s; correct the count, or give every other use its own entry", stdinDocRuleCount, key, entry.count, entry.why, len(byKey[key]), strings.Join(byKey[key], "; ")))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(uses)) {
		entry := uses[key]
		if len(byKey[key]) == 0 {
			problems = append(problems, fmt.Sprintf("%s: %s matched nothing: remove it", stdinDocRuleStale, key))
			continue
		}
		if !res.drops[entry.drops] {
			problems = append(problems, fmt.Sprintf("%s: %s names %q as the function that drops, which does not call engine.DropLeadingByteOrderMarks and use the result (assign it, pass it to a call or return it)", stdinDocRuleDrop, key, entry.drops))
		}
	}
	byDecl := map[string][]string{}
	for _, at := range slices.Sorted(maps.Keys(res.decls)) {
		f := res.decls[at]
		byDecl[f.key] = append(byDecl[f.key], f.text)
	}
	for _, key := range slices.Sorted(maps.Keys(byDecl)) {
		if _, ok := decls[key]; !ok {
			for _, text := range byDecl[key] {
				problems = append(problems, stdinDocRuleDecl+": "+text+"; a reader of documents declared this way must drop every byte order mark at the start of stdin; add the declaration to stdinDocDeclAllowlist with a reason, and its uses to stdinDocUseAllowlist")
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(decls)) {
		if len(byDecl[key]) == 0 {
			problems = append(problems, fmt.Sprintf("%s: %s matched nothing: remove it", stdinDocRuleStale, key))
		}
	}
	return problems
}

// TestEveryDocumentReaderDropsLeadingByteOrderMarks asserts that every use of
// a method Read(string) ([]byte, error), and every declaration of a method
// with that signature, is in the allowlists, and that the drop function of
// each entry calls engine.DropLeadingByteOrderMarks and uses the result.
func TestEveryDocumentReaderDropsLeadingByteOrderMarks(t *testing.T) {
	t.Parallel()
	res, err := stdinDocLoads()
	require.NoError(t, err)
	for _, problem := range stdinDocCheck(res, stdinDocUseAllowlist, stdinDocDeclAllowlist) {
		t.Error(problem)
	}
}

// TestStdinDocumentAllowlistEntriesAreComplete asserts every entry gives a
// reason, a positive count, the drop function and a test function that
// exists, and that the drop function of an entry is in the same package
// directory as the entry.
func TestStdinDocumentAllowlistEntriesAreComplete(t *testing.T) {
	t.Parallel()
	for key, entry := range stdinDocUseAllowlist {
		assert.NotEmpty(t, entry.why, key)
		assert.Positive(t, entry.count, key)
		assert.NotEmpty(t, entry.drops, key)
		dir, name, ok := strings.Cut(entry.row, ":")
		require.True(t, ok, "%s: %q must be <package directory>:<test function>", key, entry.row)
		assert.True(t, byIDTestFunctionExists(t, dir, name), "%s: test function %s is not declared in %s", key, name, dir)
		file, _, _ := strings.Cut(key, ":")
		dropFile, _, _ := strings.Cut(entry.drops, ":")
		assert.Equal(t, path.Dir(file), path.Dir(dropFile), "%s: the function that drops is in another package", key)
	}
	for key, why := range stdinDocDeclAllowlist {
		assert.NotEmpty(t, why, key)
	}
}

// stdinDocFixtures type-checks the fixture packages once.
var stdinDocFixtures = sync.OnceValues(func() (map[string]*packages.Package, error) {
	pkgs, err := loadPackages("./"+stdinDocFixtureDir+"/...", "")
	if err != nil {
		return nil, err
	}
	out := map[string]*packages.Package{}
	for _, pkg := range pkgs {
		out[path.Base(pkg.PkgPath)] = pkg
	}
	return out, nil
})

// stdinDocFixtureKey is the key a fixture's function has in an allowlist.
func stdinDocFixtureKey(fixture, function string) string {
	return stdinDocFixtureDir + "/" + fixture + "/" + fixture + ".go:" + function
}

// stdinDocFixtureEntry is an allowlist entry for a fixture function that
// reads, and drops in the function named drops (the same function when it is
// empty).
func stdinDocFixtureEntry(fixture, function, drops string) map[string]stdinDocUseEntry {
	if drops == "" {
		drops = function
	}
	return map[string]stdinDocUseEntry{
		stdinDocFixtureKey(fixture, function): {1, "fixture", "", stdinDocFixtureKey(fixture, drops)},
	}
}

// stdinDocFixtureCases lists every fixture package, the allowlists it is
// checked against and the rules the check reports for it.
var stdinDocFixtureCases = []struct {
	fixture string
	uses    map[string]stdinDocUseEntry
	decls   map[string]string
	want    []string
}{
	{"unlisted", nil, nil, []string{stdinDocRuleUse}},
	{"nodrop", stdinDocFixtureEntry("nodrop", "Load", ""), nil, []string{stdinDocRuleDrop}},
	{"dropelsewhere", stdinDocFixtureEntry("dropelsewhere", "Load", "decode"), nil, nil},
	{"trimprefix", stdinDocFixtureEntry("trimprefix", "Load", ""), nil, []string{stdinDocRuleDrop}},
	{"discarded", stdinDocFixtureEntry("discarded", "Load", ""), nil, []string{stdinDocRuleDrop}},
	{"barestatement", stdinDocFixtureEntry("barestatement", "Load", ""), nil, []string{stdinDocRuleDrop}},
	{"returned", stdinDocFixtureEntry("returned", "Load", ""), nil, nil},
	{"assigned", stdinDocFixtureEntry("assigned", "Load", ""), nil, nil},
	{"otherpackagedrop", stdinDocFixtureEntry("otherpackagedrop", "Load", ""), nil, []string{stdinDocRuleDrop}},
	{"secondimpl", nil, nil, []string{stdinDocRuleDecl}},
	{"loadport", nil, nil, []string{stdinDocRuleDecl}},
	{"wrapper", nil, nil, []string{stdinDocRuleUse}},
	{"methodvalue", nil, nil, []string{stdinDocRuleUse, stdinDocRuleUse}},
	{"methodexpr", nil, nil, []string{stdinDocRuleUse}},
	{"otherread", nil, nil, nil},
	{"plainfunction", nil, nil, nil},
	{"countdiffers", stdinDocFixtureEntry("countdiffers", "Load", ""), nil, []string{stdinDocRuleCount}},
	{"stale", stdinDocFixtureEntry("stale", "Gone", ""), nil, []string{stdinDocRuleStale}},
}

// TestStdinDocumentScanFixtures asserts each rule on type-checked fixture
// packages: every form that reads a document and does not use the result of
// the drop is reported with its rule; every form that looks alike but is no
// reader of stdin, or that uses the result of the drop, is accepted.
func TestStdinDocumentScanFixtures(t *testing.T) {
	t.Parallel()
	pkgs, err := stdinDocFixtures()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	for _, tc := range stdinDocFixtureCases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			pkg, ok := pkgs[tc.fixture]
			require.True(t, ok, "fixture package %s", tc.fixture)
			var got []string
			for _, problem := range stdinDocCheck(stdinDocScan(pkg, wd), tc.uses, tc.decls) {
				rule, _, _ := strings.Cut(problem, ": ")
				got = append(got, rule)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestStdinDocumentScanFixturesAreAllListed asserts every fixture package on
// disk is in the table above. A fixture added without its row fails.
func TestStdinDocumentScanFixturesAreAllListed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(filepath.FromSlash(stdinDocFixtureDir))
	require.NoError(t, err)
	pkgs, err := stdinDocFixtures()
	require.NoError(t, err)
	var onDisk, listed, loaded []string
	for _, e := range entries {
		if e.IsDir() {
			onDisk = append(onDisk, e.Name())
		}
	}
	for _, tc := range stdinDocFixtureCases {
		listed = append(listed, tc.fixture)
	}
	for name := range pkgs {
		loaded = append(loaded, name)
	}
	assert.ElementsMatch(t, listed, onDisk)
	assert.ElementsMatch(t, listed, loaded)
}
