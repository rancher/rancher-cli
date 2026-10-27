package conformance

import (
	"go/ast"
	"go/types"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// A poll wait repeats a Get on an engine.ClusterClient with a timer between
// the reads. An authentication or certificate failure does not pass on a
// retry, so every such loop asks engine.WaitShouldStop about each Get error
// and ends the wait when it says so; otherwise a wait on an expired token
// uses up its whole timeout. This scan fails when a loop of that shape in
// production code does not call engine.WaitShouldStop in its own body and
// has no allowlist entry.
//
// A loop is a for or range statement whose body, nested select statements
// and function literals included, calls Get on a value whose type is
// engine.ClusterClient (or implements it), and also calls time.After,
// time.Sleep, time.NewTimer, time.NewTicker or time.Tick, or receives from
// the C field of a time.Ticker or time.Timer. Type information decides, never
// a name. Test files are not production code and are skipped.
//
// Holes the scan cannot close: a Get behind a helper or a closure called from
// the loop (rotate-secret's read of the client Secret), a Get through an
// interface narrower than engine.ClusterClient that only declares Get, a
// poll through List or GetTable, a range statement whose timer is in the
// range expression (for range ticker.C, for range time.Tick(d): the scan
// inspects the body only), a file behind a build tag the scan host does not
// match, and a call of engine.WaitShouldStop whose result is ignored (the
// tests of each loop cover that).

const (
	waitDecisionFixtureDir = "test/conformance/testdata/waitdecision"
	enginePackagePath      = modulePrefix + "internal/engine"
)

// waitDecisionAllowlist maps "<file>:<function>" to the reason a poll loop
// does not call engine.WaitShouldStop. It is empty: a loop either asks or
// the wait it implements retries every error on purpose, which needs a
// reason here.
var waitDecisionAllowlist = map[string]string{}

type pollLoop struct {
	file    string
	line    int
	fn      string
	decides bool
}

func (l pollLoop) key() string { return l.file + ":" + l.fn }

// scanPollLoops returns the poll loops of the production files of pkg;
// decides is whether the loop's own body calls engine.WaitShouldStop.
func scanPollLoops(pkg *packages.Package, wd string) []pollLoop {
	info := pkg.TypesInfo
	clientType := clusterClientInterface(pkg)
	var out []pollLoop
	for _, f := range pkg.Syntax {
		filename := pkg.Fset.Position(f.Pos()).Filename
		if strings.HasSuffix(filename, "_test.go") {
			continue
		}
		for _, d := range f.Decls {
			ast.Inspect(d, func(n ast.Node) bool {
				var body *ast.BlockStmt
				switch loop := n.(type) {
				case *ast.ForStmt:
					body = loop.Body
				case *ast.RangeStmt:
					body = loop.Body
				default:
					return true
				}
				var gets, waits, decides bool
				ast.Inspect(body, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.CallExpr:
						if isClientGet(info, clientType, n) {
							gets = true
						}
						if callsPackageFunc(info, n, enginePackagePath, "WaitShouldStop") {
							decides = true
						}
						for _, name := range []string{"After", "Sleep", "NewTimer", "NewTicker", "Tick"} {
							if callsPackageFunc(info, n, "time", name) {
								waits = true
							}
						}
					case *ast.UnaryExpr:
						if isTimerReceive(info, n) {
							waits = true
						}
					}
					return true
				})
				if gets && waits {
					pos := pkg.Fset.Position(n.Pos())
					out = append(out, pollLoop{file: relativeFile(wd, pos.Filename), line: pos.Line, fn: declName(d), decides: decides})
				}
				return true
			})
		}
	}
	return out
}

// callsPackageFunc reports whether call calls the package-level function name of the
// package with import path pkgPath.
func callsPackageFunc(info *types.Info, call *ast.CallExpr, pkgPath, name string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	fn, ok := info.Uses[sel.Sel].(*types.Func)
	return ok && fn.Pkg() != nil && fn.Pkg().Path() == pkgPath
}

// isClientGet reports whether call is a Get on a value of type
// engine.ClusterClient, or of a type that implements it.
func isClientGet(info *types.Info, clientType *types.Interface, call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Get" {
		return false
	}
	t := info.TypeOf(sel.X)
	if t == nil {
		return false
	}
	if named, ok := types.Unalias(derefType(t)).(*types.Named); ok {
		obj := named.Obj()
		if obj.Pkg() != nil && obj.Pkg().Path() == enginePackagePath && obj.Name() == "ClusterClient" {
			return true
		}
	}
	return clientType != nil && types.Implements(t, clientType)
}

// isTimerReceive reports whether n is a receive from the C field of a
// time.Ticker or time.Timer.
func isTimerReceive(info *types.Info, n *ast.UnaryExpr) bool {
	if n.Op.String() != "<-" {
		return false
	}
	sel, ok := n.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "C" {
		return false
	}
	field, ok := info.Uses[sel.Sel].(*types.Var)
	if !ok || !field.IsField() || field.Pkg() == nil || field.Pkg().Path() != "time" {
		return false
	}
	named, ok := types.Unalias(derefType(info.TypeOf(sel.X))).(*types.Named)
	return ok && (named.Obj().Name() == "Ticker" || named.Obj().Name() == "Timer")
}

// clusterClientInterface finds engine.ClusterClient from the package itself
// or its direct imports; nil when the package uses neither.
func clusterClientInterface(pkg *packages.Package) *types.Interface {
	engine := pkg.Types
	if engine.Path() != enginePackagePath {
		engine = nil
		for _, imp := range pkg.Types.Imports() {
			if imp.Path() == enginePackagePath {
				engine = imp
			}
		}
	}
	if engine == nil {
		return nil
	}
	obj := engine.Scope().Lookup("ClusterClient")
	if obj == nil {
		return nil
	}
	iface, _ := obj.Type().Underlying().(*types.Interface)
	return iface
}

// loadPollLoops type-checks pattern (production files only) and scans every
// package.
func loadPollLoops(t *testing.T, pattern string) []pollLoop {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	pkgs, err := loadRequestWaitPackages(pattern, nil)
	require.NoError(t, err)
	var loops []pollLoop
	for _, pkg := range pkgs {
		if strings.HasSuffix(pkg.ID, ".test") || strings.HasSuffix(pkg.ID, "_test") {
			continue
		}
		loops = append(loops, scanPollLoops(pkg, wd)...)
	}
	slices.SortFunc(loops, func(a, b pollLoop) int { return strings.Compare(a.key(), b.key()) })
	return slices.CompactFunc(loops, func(a, b pollLoop) bool { return a.key() == b.key() && a.line == b.line })
}

// TestEveryPollWaitAsksWhetherToStop asserts every poll loop in production
// code calls engine.WaitShouldStop in its own body or has an allowlist
// entry, and that the scan sees the two loops the repository has today (a
// scan that stopped seeing them would pass vacuously).
func TestEveryPollWaitAsksWhetherToStop(t *testing.T) {
	t.Parallel()
	loops := loadPollLoops(t, "./...")
	var found []string
	for _, l := range loops {
		found = append(found, l.key())
		if _, allowed := waitDecisionAllowlist[l.key()]; !l.decides && !allowed {
			t.Errorf("%s:%d: the poll loop in %s does not call engine.WaitShouldStop on a Get error", l.file, l.line, l.fn)
		}
	}
	assert.Equal(t, []string{
		"internal/engine/oidcclient/wait.go:waitForOIDCClient",
		"internal/engine/resources/wait.go:waitFor",
	}, found)
	for key := range waitDecisionAllowlist {
		assert.Contains(t, found, key, "allowlist entry %q matches no poll loop", key)
	}
}

func TestPollWaitAllowlistRules(t *testing.T) {
	t.Parallel()
	for key, reason := range waitDecisionAllowlist {
		file, fn, ok := strings.Cut(key, ":")
		assert.True(t, ok && strings.HasSuffix(file, ".go") && fn != "", "entry %q is not <file>:<function>", key)
		assert.NotEmpty(t, reason, "entry %q has no reason", key)
		_, err := os.Stat(file)
		assert.NoError(t, err, "entry %q names a file that does not exist", key)
	}
}

// TestPollWaitScanFixtures asserts the scan on type-checked fixtures: a loop
// with a time.After in a select, a loop with a ticker made before it and a
// range loop are found and fail without the call and pass with it, a call
// outside the loop's body does not count, and a Get on another type or a loop
// without a timer is not a poll loop.
func TestPollWaitScanFixtures(t *testing.T) {
	t.Parallel()
	loops := loadPollLoops(t, "./"+waitDecisionFixtureDir+"/...")
	byFixture := map[string][]pollLoop{}
	for _, l := range loops {
		fixture, _, _ := strings.Cut(strings.TrimPrefix(l.file, waitDecisionFixtureDir+"/"), "/")
		byFixture[fixture] = append(byFixture[fixture], l)
	}
	for fixture, decides := range map[string]bool{
		"afterloop":    false,
		"afterloopok":  true,
		"tickerloop":   false,
		"tickerloopok": true,
		"rangeloop":    false,
		"rangeloopok":  true,
		"calloutside":  false,
	} {
		require.Len(t, byFixture[fixture], 1, "fixture %s", fixture)
		assert.Equal(t, decides, byFixture[fixture][0].decides, "fixture %s", fixture)
	}
	assert.Empty(t, byFixture["negative"], "a Get on another type, or a loop with no timer, is not a poll loop")
}
