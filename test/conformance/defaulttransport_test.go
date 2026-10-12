package conformance

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// httptest.Server.Close closes the idle connections of
// http.DefaultTransport, so a request that goes through the default
// transport fails with "http: CloseIdleConnections called" whenever another
// test closes its server at the wrong moment. Every test binary that can
// start an httptest server therefore installs
// testtransport.GuardDefaultTransport in TestMain, before m.Run: a request
// that still reaches the default transport then fails on every run.
//
// A directory is in scope when one of its test files imports
// net/http/httptest, internal/testrancher,
// internal/runtime/completion/completiontest (the two non-test packages
// that start servers for other packages' tests), or a package whose
// non-test files import one of those, at any depth. The package that
// defines GuardDefaultTransport is excluded by rule, not by an allowlist
// entry: its own tests swap the global and must run serially. A new
// allowlist entry is a user decision.
//
// A non-test file in a package outside that set may not import
// net/http/httptest: a new helper that starts servers for other packages'
// tests must be listed in serverHelperPackages so the directories that
// import it are in scope.

const (
	testTransportPath = modulePrefix + "internal/testtransport"
	guardFunc         = "GuardDefaultTransport"
)

const httptestPath = "net/http/httptest"

// serverHelperPackages are the packages that start servers for other
// packages' tests. A package that imports one of them in a non-test file
// starts servers too, and so on up the import chain: serverPackages
// computes the whole set.
var serverHelperPackages = map[string]bool{
	modulePrefix + "internal/testrancher":                       true,
	modulePrefix + "internal/runtime/completion/completiontest": true,
}

// unguardedAllowlist maps a directory in scope to the reason it has no
// guard yet.
var unguardedAllowlist = map[string]string{
	"internal/token": "its tests still reach the default transport, in files another change is rewriting; the guard is added after that change merges",
}

// ciPlatforms are the platforms CI compiles every package for.
var ciPlatforms = []struct{ goos, goarch string }{
	{"linux", "amd64"}, {"linux", "arm64"}, {"linux", "s390x"},
	{"darwin", "amd64"}, {"darwin", "arm64"}, {"windows", "amd64"},
}

// tagCombinations returns every combination of the repository's own build
// tags that the files of one directory name, the empty one included: a test
// run can set any of them.
func tagCombinations(files []goFile) [][]string {
	set := map[string]bool{}
	for _, f := range files {
		tags, err := customTagsIn(strings.NewReader(f.src))
		if err != nil {
			continue
		}
		for _, tag := range tags {
			set[tag] = true
		}
	}
	tags := slices.Sorted(maps.Keys(set))
	out := make([][]string, 0, 1<<len(tags))
	for mask := range 1 << len(tags) {
		var combo []string
		for i, tag := range tags {
			if mask&(1<<i) != 0 {
				combo = append(combo, tag)
			}
		}
		out = append(out, combo)
	}
	return out
}

// goFile is one parsed file of a directory.
type goFile struct {
	path string
	src  string
	ast  *ast.File
}

func (f goFile) isTest() bool { return strings.HasSuffix(f.path, "_test.go") }

// importName returns the name the file gives the package at importPath:
// the declared name, "." for a dot import, the last path element
// otherwise, and "" when the file does not import it or imports it only as
// _.
func importName(file *ast.File, importPath string) string {
	for _, spec := range file.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil || p != importPath {
			continue
		}
		switch {
		case spec.Name == nil:
			return path.Base(p)
		case spec.Name.Name == "_":
			return ""
		default:
			return spec.Name.Name
		}
	}
	return ""
}

// importsAny reports whether the file imports net/http/httptest (when
// httptest is true) or a package of the set.
func importsAny(file *ast.File, set map[string]bool, httptest bool) bool {
	for _, spec := range file.Imports {
		if p, err := strconv.Unquote(spec.Path.Value); err == nil && (set[p] || httptest && p == httptestPath) {
			return true
		}
	}
	return false
}

// serverPackages returns the import paths of the packages that put a
// directory in scope: the listed helpers and every package whose non-test
// files import one of them, repeated until nothing is added. The package
// that defines the guard stays out.
func serverPackages(dirs map[string][]goFile) map[string]bool {
	set := maps.Clone(serverHelperPackages)
	for changed := true; changed; {
		changed = false
		for dir, files := range dirs {
			if set[modulePrefix+dir] || definesGuard(files) {
				continue
			}
			if slices.ContainsFunc(files, func(f goFile) bool { return !f.isTest() && importsAny(f.ast, set, false) }) {
				set[modulePrefix+dir] = true
				changed = true
			}
		}
	}
	return set
}

// buildContexts returns one build context per CI platform and combination
// of custom tags, reading the files of one directory from their parsed
// source.
func buildContexts(files []goFile) []*build.Context {
	sources := map[string]string{}
	for _, f := range files {
		sources[f.path] = f.src
	}
	var out []*build.Context
	for _, platform := range ciPlatforms {
		for _, tags := range tagCombinations(files) {
			ctxt := build.Default
			ctxt.GOOS, ctxt.GOARCH = platform.goos, platform.goarch
			ctxt.CgoEnabled = false
			ctxt.BuildTags = tags
			ctxt.JoinPath = path.Join
			ctxt.OpenFile = func(name string) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(sources[name])), nil
			}
			out = append(out, &ctxt)
		}
	}
	return out
}

// builds reports whether the Go tool compiles the file in the context, by
// its name and its //go:build line. A file the tool cannot read counts as
// onError, so that a file that starts servers counts as built and a
// TestMain file as not built: a mistake is reported, not hidden.
func builds(ctxt *build.Context, f goFile, onError bool) bool {
	match, err := ctxt.MatchFile(path.Dir(f.path), path.Base(f.path))
	if err != nil {
		return onError
	}
	return match
}

// isGuardCall reports whether e is a call of GuardDefaultTransport through
// the import name the file gives the guard package.
func isGuardCall(e ast.Expr, name string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		id, isID := fun.X.(*ast.Ident)
		return isID && id.Name == name && fun.Sel.Name == guardFunc
	case *ast.Ident:
		return name == "." && fun.Name == guardFunc
	}
	return false
}

// isMethodCall reports whether e is the call recv.method().
func isMethodCall(e ast.Expr, recv, method string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != method {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == recv
}

// runsTests reports whether stmt is a top-level statement whose own call
// expression, or the call it assigns, or an argument of that call, is
// m.Run(): `m.Run()`, `code := m.Run()`, `os.Exit(m.Run())`,
// `code := wrap(m.Run(), ...)`. A call inside an if, a loop or a function
// literal may never run.
func runsTests(stmt ast.Stmt, m string) bool {
	var exprs []ast.Expr
	switch s := stmt.(type) {
	case *ast.ExprStmt:
		exprs = []ast.Expr{s.X}
	case *ast.AssignStmt:
		exprs = s.Rhs
	}
	for _, e := range exprs {
		if isMethodCall(e, m, "Run") {
			return true
		}
		if call, ok := e.(*ast.CallExpr); ok && slices.ContainsFunc(call.Args, func(a ast.Expr) bool { return isMethodCall(a, m, "Run") }) {
			return true
		}
	}
	return false
}

// callsFunc reports whether e is a call of the variable name.
func callsFunc(e ast.Expr, name string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == name
}

// reads reports whether n mentions the identifier name other than as the
// target of an assignment.
func reads(n ast.Node, name string) bool {
	if n == nil {
		return false
	}
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for _, e := range x.Rhs {
				found = found || reads(e, name)
			}
			for _, e := range x.Lhs {
				if _, isID := e.(*ast.Ident); !isID {
					found = found || reads(e, name)
				}
			}
			return false
		case *ast.Ident:
			found = found || x.Name == name
		}
		return !found
	})
	return found
}

// assignedName returns the variable a single assignment statement stores
// the result of a one-value right-hand side in, and that right-hand side;
// the blank identifier is not a variable.
func assignedName(stmt ast.Stmt) (string, ast.Expr) {
	assign, ok := stmt.(*ast.AssignStmt)
	if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		return "", nil
	}
	id, ok := assign.Lhs[0].(*ast.Ident)
	if !ok || id.Name == "_" {
		return "", nil
	}
	return id.Name, assign.Rhs[0]
}

// checksRestore reports whether stmt calls the function restore returns and
// uses its error: either an if statement whose init assigns the call to a
// variable that its condition or body reads, or an assignment of the call
// to a variable that a later statement of rest reads.
func checksRestore(stmt ast.Stmt, rest []ast.Stmt, restore string) bool {
	if ifStmt, ok := stmt.(*ast.IfStmt); ok && ifStmt.Init != nil {
		if name, rhs := assignedName(ifStmt.Init); name != "" && callsFunc(rhs, restore) {
			return reads(ifStmt.Cond, name) || reads(ifStmt.Body, name) || reads(ifStmt.Else, name)
		}
		return false
	}
	if name, rhs := assignedName(stmt); name != "" && callsFunc(rhs, restore) {
		return slices.ContainsFunc(rest, func(later ast.Stmt) bool { return reads(later, name) })
	}
	return false
}

// guardsTestMain reports whether the file declares a TestMain that, among
// its top-level statements, assigns the result of GuardDefaultTransport to a
// variable, then runs m.Run, then calls that variable and uses its error. A
// guard whose result is dropped, inside an if or a function literal, or
// restored before the run or without a look at the error, does not count.
func guardsTestMain(f goFile) bool {
	name := importName(f.ast, testTransportPath)
	if name == "" {
		return false
	}
	for _, decl := range f.ast.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != "TestMain" || fn.Body == nil {
			continue
		}
		params := fn.Type.Params.List
		if len(params) != 1 || len(params[0].Names) != 1 {
			continue
		}
		m := params[0].Names[0].Name
		stmts := fn.Body.List
		for i, stmt := range stmts {
			restore, rhs := assignedName(stmt)
			if restore == "" || !isGuardCall(rhs, name) {
				continue
			}
			for j := i + 1; j < len(stmts); j++ {
				if !runsTests(stmts[j], m) {
					continue
				}
				for k := j + 1; k < len(stmts); k++ {
					if checksRestore(stmts[k], stmts[k+1:], restore) {
						return true
					}
				}
				break
			}
		}
	}
	return false
}

// definesGuard reports whether a non-test file of the directory declares
// GuardDefaultTransport.
func definesGuard(files []goFile) bool {
	for _, f := range files {
		if f.isTest() {
			continue
		}
		for _, decl := range f.ast.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == guardFunc {
				return true
			}
		}
	}
	return false
}

// unguardedProblem returns why the directory's tests are not guarded, or ""
// when they are or the directory is out of scope.
func unguardedProblem(files []goFile, servers map[string]bool) string {
	var triggers []goFile
	for _, f := range files {
		if f.isTest() && importsAny(f.ast, servers, true) {
			triggers = append(triggers, f)
		}
	}
	if len(triggers) == 0 || definesGuard(files) {
		return ""
	}
	var guarded []goFile
	for _, f := range files {
		if f.isTest() && guardsTestMain(f) {
			guarded = append(guarded, f)
		}
	}
	if len(guarded) == 0 {
		return "its tests start httptest servers but no TestMain calls testtransport." + guardFunc + " before m.Run and checks the error of the function it returns after m.Run"
	}
	for _, ctxt := range buildContexts(files) {
		starts := slices.ContainsFunc(triggers, func(f goFile) bool { return builds(ctxt, f, true) })
		guards := slices.ContainsFunc(guarded, func(f goFile) bool { return builds(ctxt, f, false) })
		if starts && !guards {
			return "its guarding TestMain is behind a build constraint that does not cover every test file that starts a server (" + ctxt.GOOS + "/" + ctxt.GOARCH + " with tags " + strings.Join(ctxt.BuildTags, ",") + " builds a server file but not the TestMain), so that run has no guard"
		}
	}
	return ""
}

// scanDefaultTransportGuards returns one message per directory whose tests
// start servers without a guard, and one per stale allowlist entry.
func scanDefaultTransportGuards(dirs map[string][]goFile, allowlist map[string]string) []string {
	var out []string
	servers := serverPackages(dirs)
	for _, dir := range slices.Sorted(maps.Keys(dirs)) {
		out = append(out, unlistedHelpers(dir, dirs[dir], servers)...)
		problem := unguardedProblem(dirs[dir], servers)
		_, allowed := allowlist[dir]
		switch {
		case problem != "" && !allowed:
			out = append(out, dir+": "+problem)
		case problem == "" && allowed:
			out = append(out, dir+": allowlist entry is stale: the directory is guarded or no longer starts servers; delete the entry")
		}
	}
	for _, dir := range slices.Sorted(maps.Keys(allowlist)) {
		if _, ok := dirs[dir]; !ok {
			out = append(out, dir+": allowlist entry is stale: no Go files in the directory; delete the entry")
		}
	}
	return out
}

// unlistedHelpers returns one message per non-test file of the directory that
// imports net/http/httptest although neither its package is one of servers
// nor it is the guard package.
func unlistedHelpers(dir string, files []goFile, servers map[string]bool) []string {
	if servers[modulePrefix+dir] || definesGuard(files) {
		return nil
	}
	var out []string
	for _, f := range files {
		if !f.isTest() && importsAny(f.ast, nil, true) {
			out = append(out, f.path+": a non-test file imports net/http/httptest, so the directories that import its package start servers; list the package in serverHelperPackages")
		}
	}
	return out
}

// parseDirs parses every Go file outside testdata, grouped by directory,
// from the files git sees. Test files decide which directories are in scope;
// non-test files are read for the guard's own definition and for helpers
// that import net/http/httptest.
func parseDirs(t *testing.T) map[string][]goFile {
	t.Helper()
	all, err := repositoryFiles()
	require.NoError(t, err)
	fset := token.NewFileSet()
	dirs := map[string][]goFile{}
	for _, p := range all {
		if !strings.HasSuffix(p, ".go") || inTestdata(p) {
			continue
		}
		src, err := os.ReadFile(p)
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, p, src, parser.ParseComments|parser.SkipObjectResolution)
		require.NoError(t, err)
		dirs[path.Dir(p)] = append(dirs[path.Dir(p)], goFile{path: p, src: string(src), ast: file})
	}
	return dirs
}

// TestEveryTestBinaryThatStartsServersGuardsTheDefaultTransport asserts
// that every directory whose tests can start an httptest server installs
// the guard in TestMain.
func TestEveryTestBinaryThatStartsServersGuardsTheDefaultTransport(t *testing.T) {
	t.Parallel()
	dirs := parseDirs(t)
	require.NotEmpty(t, dirs)
	for _, problem := range scanDefaultTransportGuards(dirs, unguardedAllowlist) {
		t.Error(problem)
	}
}

func fixtureDirs(t *testing.T, files map[string]string) map[string][]goFile {
	t.Helper()
	fset := token.NewFileSet()
	dirs := map[string][]goFile{}
	for p, src := range files {
		file, err := parser.ParseFile(fset, p, src, parser.ParseComments|parser.AllErrors)
		require.NoError(t, err)
		dirs[path.Dir(p)] = append(dirs[path.Dir(p)], goFile{path: p, src: src, ast: file})
	}
	return dirs
}

const (
	serverTestSrc = `package p; import "net/http/httptest"; var _ = httptest.NewServer`
	mainImports   = `import ("os"; "testing"; "github.com/rancher/rancher-cli/internal/testtransport")`
	guardedMain   = `package p
import ("os"; "testing"; "github.com/rancher/rancher-cli/internal/testtransport")
func TestMain(m *testing.M) {
	restore := testtransport.GuardDefaultTransport()
	code := m.Run()
	if err := restore(); err != nil { code = 1 }
	os.Exit(code)
}`
)

// TestDefaultTransportScanFixtures asserts the scan's rules from source
// snippets, so the whole-tree test above and this table cannot drift.
func TestDefaultTransportScanFixtures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		files     map[string]string
		allowlist map[string]string
		want      string
	}{
		{"httptest and no TestMain", map[string]string{"internal/x/a_test.go": serverTestSrc}, nil, "internal/x: its tests start httptest servers"},
		{"testrancher import", map[string]string{"internal/x/a_test.go": `package p; import _ "github.com/rancher/rancher-cli/internal/testrancher"`}, nil, "internal/x: its tests start"},
		{"completiontest import", map[string]string{"internal/x/a_test.go": `package p; import "github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"; var _ = completiontest.New`}, nil, "internal/x: its tests start"},
		{"no server", map[string]string{"internal/x/a_test.go": `package p; import "testing"; func TestA(t *testing.T) {}`}, nil, ""},
		{"guarded", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": guardedMain}, nil, ""},
		{"TestMain without the guard", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": `package p; import ("os"; "testing"); func TestMain(m *testing.M) { os.Exit(m.Run()) }`}, nil, "internal/x: its tests start"},
		{"guard after m.Run", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": `package p
import ("os"; "testing"; "github.com/rancher/rancher-cli/internal/testtransport")
func TestMain(m *testing.M) {
	code := m.Run()
	restore := testtransport.GuardDefaultTransport()
	if err := restore(); err != nil { code = 1 }
	os.Exit(code)
}`}, nil, "internal/x: its tests start"},
		{"guard without m.Run", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": `package p
import ("os"; "testing"; "github.com/rancher/rancher-cli/internal/testtransport")
func TestMain(m *testing.M) {
	restore := testtransport.GuardDefaultTransport()
	if err := restore(); err != nil { os.Exit(1) }
}`}, nil, "internal/x: its tests start"},
		{"guard in another function", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": `package p
import ("os"; "testing"; "github.com/rancher/rancher-cli/internal/testtransport")
func setup() { testtransport.GuardDefaultTransport() }
func TestMain(m *testing.M) { os.Exit(m.Run()) }`}, nil, "internal/x: its tests start"},
		{"guard result discarded", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": `package p
import ("os"; "testing"; "github.com/rancher/rancher-cli/internal/testtransport")
func TestMain(m *testing.M) {
	testtransport.GuardDefaultTransport()
	code := m.Run()
	os.Exit(code)
}`}, nil, "internal/x: its tests start"},
		{"guard result assigned to blank", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": `package p
import ("os"; "testing"; "github.com/rancher/rancher-cli/internal/testtransport")
func TestMain(m *testing.M) {
	_ = testtransport.GuardDefaultTransport()
	code := m.Run()
	os.Exit(code)
}`}, nil, "internal/x: its tests start"},
		{"restore called before m.Run", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": `package p
import ("os"; "testing"; "github.com/rancher/rancher-cli/internal/testtransport")
func TestMain(m *testing.M) {
	restore := testtransport.GuardDefaultTransport()
	if err := restore(); err != nil { os.Exit(1) }
	os.Exit(m.Run())
}`}, nil, "internal/x: its tests start"},
		{"restore result dropped", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": `package p
import ("os"; "testing"; "github.com/rancher/rancher-cli/internal/testtransport")
func TestMain(m *testing.M) {
	restore := testtransport.GuardDefaultTransport()
	code := m.Run()
	restore()
	os.Exit(code)
}`}, nil, "internal/x: its tests start"},
		{"restore error assigned to blank", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": `package p
import ("os"; "testing"; "github.com/rancher/rancher-cli/internal/testtransport")
func TestMain(m *testing.M) {
	restore := testtransport.GuardDefaultTransport()
	code := m.Run()
	_ = restore()
	os.Exit(code)
}`}, nil, "internal/x: its tests start"},
		{"restore error never read", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": `package p
import ("os"; "testing"; "github.com/rancher/rancher-cli/internal/testtransport")
func TestMain(m *testing.M) {
	restore := testtransport.GuardDefaultTransport()
	code := m.Run()
	err := restore()
	err = nil
	os.Exit(code)
}`}, nil, "internal/x: its tests start"},
		{"restore error in an if that never reads it", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": `package p
import ("os"; "testing"; "github.com/rancher/rancher-cli/internal/testtransport")
func TestMain(m *testing.M) {
	restore := testtransport.GuardDefaultTransport()
	code := m.Run()
	if err := restore(); code > 0 { code = 2 }
	os.Exit(code)
}`}, nil, "internal/x: its tests start"},
		{"restore error read after the call", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": `package p
import ("os"; "testing"; "github.com/rancher/rancher-cli/internal/testtransport")
func TestMain(m *testing.M) {
	restore := testtransport.GuardDefaultTransport()
	code := m.Run()
	err := restore()
	if err != nil { code = 1 }
	os.Exit(code)
}`}, nil, ""},
		{"guard call inside an if", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": `package p
import ("os"; "testing"; "github.com/rancher/rancher-cli/internal/testtransport")
func TestMain(m *testing.M) {
	var restore func() error
	if os.Getenv("X") == "" {
		restore = testtransport.GuardDefaultTransport()
	}
	code := m.Run()
	if err := restore(); err != nil { code = 1 }
	os.Exit(code)
}`}, nil, "internal/x: its tests start"},
		{"guard call inside a func literal", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": `package p
import ("os"; "testing"; "github.com/rancher/rancher-cli/internal/testtransport")
func TestMain(m *testing.M) {
	restore := func() func() error { return testtransport.GuardDefaultTransport() }()
	code := m.Run()
	if err := restore(); err != nil { code = 1 }
	os.Exit(code)
}`}, nil, "internal/x: its tests start"},
		{"aliased import", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": `package p
import ("os"; "testing"; tt "github.com/rancher/rancher-cli/internal/testtransport")
func TestMain(m *testing.M) {
	restore := tt.GuardDefaultTransport()
	code := m.Run()
	if err := restore(); err != nil { code = 1 }
	os.Exit(code)
}`}, nil, ""},
		{"dot import", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": `package p
import ("os"; "testing"; . "github.com/rancher/rancher-cli/internal/testtransport")
func TestMain(m *testing.M) {
	restore := GuardDefaultTransport()
	code := m.Run()
	if err := restore(); err != nil { code = 1 }
	os.Exit(code)
}`}, nil, ""},
		{"other package with the guard's name", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": `package p
import ("os"; "testing"; testtransport "example.com/other")
func TestMain(m *testing.M) {
	restore := testtransport.GuardDefaultTransport()
	code := m.Run()
	if err := restore(); err != nil { code = 1 }
	os.Exit(code)
}`}, nil, "internal/x: its tests start"},
		{"tagged TestMain, untagged server file", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": "//go:build e2e\n\n" + guardedMain}, nil, "build constraint"},
		{"tagged TestMain, same tag on the server file", map[string]string{"internal/x/a_test.go": "//go:build e2e\n\n" + serverTestSrc, "internal/x/testmain_test.go": "//go:build e2e\n\n" + guardedMain}, nil, ""},
		{"tagged TestMain, narrower server file", map[string]string{"internal/x/a_test.go": "//go:build e2e && linux\n\n" + serverTestSrc, "internal/x/testmain_test.go": "//go:build e2e\n\n" + guardedMain}, nil, ""},
		{"tagged TestMain, wider server file", map[string]string{"internal/x/a_test.go": "//go:build e2e || linux\n\n" + serverTestSrc, "internal/x/testmain_test.go": "//go:build e2e\n\n" + guardedMain}, nil, "build constraint"},
		{"negated tag on both", map[string]string{"internal/x/a_test.go": "//go:build !windows\n\n" + serverTestSrc, "internal/x/testmain_test.go": "//go:build !windows\n\n" + guardedMain}, nil, ""},
		{"TestMain in a windows-named file", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_windows_test.go": guardedMain}, nil, "build constraint"},
		{"untagged TestMain covers a tagged server file", map[string]string{"internal/x/a_test.go": "//go:build e2e\n\n" + serverTestSrc, "internal/x/testmain_test.go": guardedMain}, nil, ""},
		{"a Linux-named server file and a TestMain for every platform but Windows", map[string]string{"internal/x/a_linux_test.go": serverTestSrc, "internal/x/testmain_test.go": "//go:build !windows\n\n" + guardedMain}, nil, ""},
		{"a Windows-named server file and a TestMain for every platform but Windows", map[string]string{"internal/x/a_windows_test.go": serverTestSrc, "internal/x/testmain_test.go": "//go:build !windows\n\n" + guardedMain}, nil, "build constraint"},
		{"a server file for e2e and a TestMain for builds without it", map[string]string{"internal/x/a_test.go": "//go:build e2e\n\n" + serverTestSrc, "internal/x/testmain_test.go": "//go:build !e2e\n\n" + guardedMain}, nil, "build constraint"},
		{"a non-test file outside the listed packages imports httptest", map[string]string{"internal/y/helper.go": serverTestSrc}, nil, "internal/y/helper.go: a non-test file imports net/http/httptest"},
		{"a listed package may import httptest", map[string]string{"internal/testrancher/helper.go": serverTestSrc}, nil, ""},
		{"the package that defines the guard may import httptest", map[string]string{"internal/x/x.go": `package p; import "net/http/httptest"; var _ = httptest.NewServer; func GuardDefaultTransport() func() error { return nil }`}, nil, ""},
		{"a server file for e2e_fidelity and a TestMain for e2e", map[string]string{"internal/x/a_test.go": "//go:build e2e_fidelity\n\n" + serverTestSrc, "internal/x/testmain_test.go": "//go:build e2e\n\n" + guardedMain}, nil, "build constraint"},
		{"a two-level wrapper of a server helper puts a directory in scope", map[string]string{
			"internal/a/a.go":      `package a; import _ "github.com/rancher/rancher-cli/internal/testrancher"`,
			"internal/b/b.go":      `package b; import _ "github.com/rancher/rancher-cli/internal/a"`,
			"internal/x/a_test.go": `package p; import _ "github.com/rancher/rancher-cli/internal/b"`,
		}, nil, "internal/x: its tests start"},
		{"a wrapper imported only by a test file does not put a directory in scope", map[string]string{
			"internal/a/a_test.go":        `package a; import _ "github.com/rancher/rancher-cli/internal/testrancher"`,
			"internal/a/testmain_test.go": guardedMain,
			"internal/b/b.go":             `package b; import _ "github.com/rancher/rancher-cli/internal/a"`,
			"internal/x/a_test.go":        `package p; import _ "github.com/rancher/rancher-cli/internal/b"`,
		}, nil, ""},
		{"a wrapper may import httptest once it imports a helper", map[string]string{
			"internal/a/a.go": `package a; import _ "github.com/rancher/rancher-cli/internal/testrancher"; import "net/http/httptest"; var _ = httptest.NewServer`,
		}, nil, ""},
		{"m.Run inside an if", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": `package p
import ("os"; "testing"; "github.com/rancher/rancher-cli/internal/testtransport")
func TestMain(m *testing.M) {
	restore := testtransport.GuardDefaultTransport()
	code := 0
	if os.Getenv("X") == "" {
		code = m.Run()
	}
	if err := restore(); err != nil { code = 1 }
	os.Exit(code)
}`}, nil, "internal/x: its tests start"},
		{"m.Run inside a func literal", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": `package p
import ("os"; "testing"; "github.com/rancher/rancher-cli/internal/testtransport")
func TestMain(m *testing.M) {
	restore := testtransport.GuardDefaultTransport()
	code := func() int { return m.Run() }()
	if err := restore(); err != nil { code = 1 }
	os.Exit(code)
}`}, nil, "internal/x: its tests start"},
		{"m.Run as the argument of a wrapper", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": `package p
import ("os"; "testing"; "github.com/rancher/rancher-cli/internal/testtransport")
func wrap(code int) int { return code }
func TestMain(m *testing.M) {
	restore := testtransport.GuardDefaultTransport()
	code := wrap(m.Run())
	if err := restore(); err != nil { code = 1 }
	os.Exit(code)
}`}, nil, ""},
		{"the package that defines the guard", map[string]string{"internal/x/x.go": `package p; func GuardDefaultTransport() func() error { return nil }`, "internal/x/a_test.go": serverTestSrc}, nil, ""},
		{"a method of that name does not define it", map[string]string{"internal/x/x.go": `package p; type T struct{}; func (T) GuardDefaultTransport() {}`, "internal/x/a_test.go": serverTestSrc}, nil, "internal/x: its tests start"},
		{"allowlisted and unguarded", map[string]string{"internal/x/a_test.go": serverTestSrc}, map[string]string{"internal/x": "reason"}, ""},
		{"allowlisted and guarded is stale", map[string]string{"internal/x/a_test.go": serverTestSrc, "internal/x/testmain_test.go": guardedMain}, map[string]string{"internal/x": "reason"}, "allowlist entry is stale"},
		{"allowlisted and out of scope is stale", map[string]string{"internal/x/a_test.go": `package p`}, map[string]string{"internal/x": "reason"}, "allowlist entry is stale"},
		{"allowlisted and missing is stale", map[string]string{"internal/y/a_test.go": `package p`}, map[string]string{"internal/x": "reason"}, "internal/x: allowlist entry is stale: no Go files"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := scanDefaultTransportGuards(fixtureDirs(t, tc.files), tc.allowlist)
			if tc.want == "" {
				assert.Empty(t, got)
				return
			}
			require.NotEmpty(t, got)
			assert.Contains(t, strings.Join(got, "\n"), tc.want)
		})
	}
}

// TestDefaultTransportAllowlistEntriesNameAReason asserts that every
// allowlist entry carries a reason.
func TestDefaultTransportAllowlistEntriesNameAReason(t *testing.T) {
	t.Parallel()
	for dir, reason := range unguardedAllowlist {
		assert.NotEmpty(t, strings.TrimSpace(reason), "allowlist entry %s has no reason", dir)
	}
}
