package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// A wait loop reads an object again and again with a timer between the
// reads: it repeats a Get on an engine.ClusterClient, or lists and watches
// the object through an engine.ObjectWatcher and receives its events. An
// authentication or certificate failure does not pass on a retry, so every
// such loop asks engine.WaitShouldStop about each error of those requests and
// ends the wait when it says so; otherwise a wait on an expired token uses up
// its whole timeout. This scan fails when a loop of that shape in production
// code does not call engine.WaitShouldStop in its own body and has no
// allowlist entry.
//
// A loop is a for or range statement whose body, nested select statements
// and function literals included, does one of these:
//   - calls Get on a value whose type is engine.ClusterClient (or implements
//     it);
//   - calls ListObject or WatchObject on a value whose type is
//     engine.ObjectWatcher (or implements it);
//   - receives from a channel of engine.ObjectEvent;
//
// and also calls time.After, time.Sleep, time.NewTimer, time.NewTicker or
// time.Tick, or receives from a channel of time.Time (the C field of a
// time.Ticker or time.Timer, or a variable that holds it); a range statement
// whose range expression is such a channel or a call of time.Tick waits by
// that. A call of engine.WaitShouldStop is a call of the function of that
// package, by its qualified name or, inside package engine, by its bare name.
// Type information decides, never a name. Test files are not production code
// and are skipped.
//
// The shared loop of package engine keeps its calls, its event receive, its
// timer and its call of engine.WaitShouldStop in the body of its one for
// statement, with no helper or callback for them: the scan inspects only a
// loop's body, and a flat body is a constraint it can check.
//
// Holes the scan cannot close: a request behind a helper or a closure called
// from the loop (rotate-secret's read of the client Secret), a Get through an
// interface narrower than engine.ClusterClient that only declares Get, a
// poll through ClusterClient.List or GetTable, a range over an event channel
// (for ev := range events: the scan reads the range expression only for a
// timer, not for a read), a file behind a build tag the scan host does not
// match, and a call of
// engine.WaitShouldStop whose result is ignored (the tests of each loop cover
// that; each error source of the shared loop has its own test).

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
	clientType := engineInterface(pkg, "ClusterClient")
	watcherType := engineInterface(pkg, "ObjectWatcher")
	var out []pollLoop
	for _, f := range pkg.Syntax {
		filename := pkg.Fset.Position(f.Pos()).Filename
		if strings.HasSuffix(filename, "_test.go") {
			continue
		}
		for _, d := range f.Decls {
			ast.Inspect(d, func(n ast.Node) bool {
				var body *ast.BlockStmt
				var rangeTimer bool
				switch loop := n.(type) {
				case *ast.ForStmt:
					body = loop.Body
				case *ast.RangeStmt:
					body = loop.Body
					rangeTimer = rangesOverTimer(info, loop.X)
				default:
					return true
				}
				gets, decides := false, false
				waits := rangeTimer
				ast.Inspect(body, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.CallExpr:
						if isClientGet(info, clientType, n) || isWatcherCall(info, watcherType, n) {
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
						if isEventReceive(info, n) {
							gets = true
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
// package with import path pkgPath: by its qualified name (engine.WaitShouldStop), or,
// inside that package, by its bare name. A bare name counts only when the identifier
// resolves to a function declared in pkgPath, so a local wrapper or a function of
// another package with the same name does not.
func callsPackageFunc(info *types.Info, call *ast.CallExpr, pkgPath, name string) bool {
	var id *ast.Ident
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		id = fun.Sel
	case *ast.Ident:
		id = fun
	default:
		return false
	}
	if id.Name != name {
		return false
	}
	fn, ok := info.Uses[id].(*types.Func)
	return ok && fn.Pkg() != nil && fn.Pkg().Path() == pkgPath && fn.Type().(*types.Signature).Recv() == nil
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

// isWatcherCall reports whether call is ListObject or WatchObject on a value
// of type engine.ObjectWatcher, or of a type that implements it.
func isWatcherCall(info *types.Info, watcherType *types.Interface, call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "ListObject" && sel.Sel.Name != "WatchObject") {
		return false
	}
	t := info.TypeOf(sel.X)
	if t == nil {
		return false
	}
	if named, ok := types.Unalias(derefType(t)).(*types.Named); ok {
		obj := named.Obj()
		if obj.Pkg() != nil && obj.Pkg().Path() == enginePackagePath && obj.Name() == "ObjectWatcher" {
			return true
		}
	}
	return watcherType != nil && types.Implements(t, watcherType)
}

// isEventReceive reports whether n is a receive from a channel of
// engine.ObjectEvent.
func isEventReceive(info *types.Info, n *ast.UnaryExpr) bool {
	if n.Op.String() != "<-" {
		return false
	}
	ch, ok := info.TypeOf(n.X).Underlying().(*types.Chan)
	if !ok {
		return false
	}
	named, ok := types.Unalias(ch.Elem()).(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == enginePackagePath && named.Obj().Name() == "ObjectEvent"
}

// isTimeChan reports whether t is a channel of time.Time.
func isTimeChan(t types.Type) bool {
	if t == nil {
		return false
	}
	ch, ok := t.Underlying().(*types.Chan)
	if !ok {
		return false
	}
	named, ok := types.Unalias(ch.Elem()).(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "time" && named.Obj().Name() == "Time"
}

// isTimerReceive reports whether n is a receive from a channel of time.Time:
// the C field of a time.Ticker or time.Timer, a variable or parameter that
// holds it, or the result of a function that returns one.
func isTimerReceive(info *types.Info, n *ast.UnaryExpr) bool {
	return n.Op.String() == "<-" && isTimeChan(info.TypeOf(n.X))
}

// rangesOverTimer reports whether the range expression x of a loop is a
// channel of time.Time (for range ticker.C, for range tick) or a call of
// time.Tick: the timer is then in the range expression, not in the body.
func rangesOverTimer(info *types.Info, x ast.Expr) bool {
	if call, ok := ast.Unparen(x).(*ast.CallExpr); ok && callsPackageFunc(info, call, "time", "Tick") {
		return true
	}
	return isTimeChan(info.TypeOf(x))
}

// engineInterface finds the interface of package engine with the given name
// from the package itself or its direct imports; nil when the package uses
// neither.
func engineInterface(pkg *packages.Package, name string) *types.Interface {
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
	obj := engine.Scope().Lookup(name)
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

// TestEveryPollWaitAsksWhetherToStop asserts every wait loop in production
// code calls engine.WaitShouldStop in its own body or has an allowlist
// entry, and that the scan sees exactly the two loops the repository has
// today: the OIDC client wait and the shared loop of package engine, one
// entry each (a scan that stopped seeing them would pass vacuously, and a
// loop split into two for statements would add an entry).
func TestEveryPollWaitAsksWhetherToStop(t *testing.T) {
	t.Parallel()
	loops := loadPollLoops(t, "./...")
	var found []string
	for _, l := range loops {
		found = append(found, l.key())
		if _, allowed := waitDecisionAllowlist[l.key()]; !l.decides && !allowed {
			t.Errorf("%s:%d: the wait loop in %s does not call engine.WaitShouldStop on a request error", l.file, l.line, l.fn)
		}
	}
	assert.Equal(t, []string{
		"internal/engine/oidcclient/wait.go:waitForOIDCClient",
		"internal/engine/waitloop.go:WaitObject",
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
// range loop are found and fail without the call and pass with it, a loop
// that calls ListObject and WatchObject on an engine.ObjectWatcher and one
// that only receives from a channel of engine.ObjectEvent are found the same
// way, a loop whose only timer is a receive from a variable of type
// <-chan time.Time, a range over such a channel and a range over time.Tick(d)
// are found, a call outside the loop's body does not count, and a Get on another
// type or a loop without a timer is not a poll loop.
func TestPollWaitScanFixtures(t *testing.T) {
	t.Parallel()
	loops := loadPollLoops(t, "./"+waitDecisionFixtureDir+"/...")
	byFixture := map[string][]pollLoop{}
	for _, l := range loops {
		fixture, _, _ := strings.Cut(strings.TrimPrefix(l.file, waitDecisionFixtureDir+"/"), "/")
		byFixture[fixture] = append(byFixture[fixture], l)
	}
	for fixture, decides := range map[string]bool{
		"afterloop":         false,
		"afterloopok":       true,
		"tickerloop":        false,
		"tickerloopok":      true,
		"rangeloop":         false,
		"rangeloopok":       true,
		"calloutside":       false,
		"watchloop":         false,
		"watchloopok":       true,
		"receiveloop":       false,
		"receiveloopok":     true,
		"timerchanloop":     false,
		"timerchanloopok":   true,
		"rangetickerloop":   false,
		"rangetickerloopok": true,
		"rangetickloop":     false,
		"rangetickloopok":   true,
	} {
		require.Len(t, byFixture[fixture], 1, "fixture %s", fixture)
		assert.Equal(t, decides, byFixture[fixture][0].decides, "fixture %s", fixture)
	}
	assert.Empty(t, byFixture["negative"], "a Get on another type, or a loop with no timer, is not a poll loop")
}

// TestCallsPackageFuncAcceptsABareNameInsideThePackage asserts, on type-checked
// source of a package whose path is the engine's, that a call of
// WaitShouldStop by its bare name counts, and that a call of a local wrapper,
// of a method with that name, and a call asked about another package path do
// not.
func TestCallsPackageFuncAcceptsABareNameInsideThePackage(t *testing.T) {
	t.Parallel()
	const src = `package engine

func WaitShouldStop(err error) bool { return err != nil }

func wrapper(err error) bool { return WaitShouldStop(err) }

type checker struct{}

func (checker) WaitShouldStop(err error) bool { return false }

func direct(err error) bool { return WaitShouldStop(err) }

func throughWrapper(err error) bool { return wrapper(err) }

func throughMethod(c checker, err error) bool { return c.WaitShouldStop(err) }
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "engine.go", src, 0)
	require.NoError(t, err)
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
	_, err = (&types.Config{}).Check(enginePackagePath, fset, []*ast.File{file}, info)
	require.NoError(t, err)

	callIn := func(fn string) *ast.CallExpr {
		for _, d := range file.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == fn {
				var call *ast.CallExpr
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					if c, ok := n.(*ast.CallExpr); ok && call == nil {
						call = c
					}
					return true
				})
				return call
			}
		}
		require.Failf(t, "no function", "%s", fn)
		return nil
	}
	assert.True(t, callsPackageFunc(info, callIn("direct"), enginePackagePath, "WaitShouldStop"))
	assert.False(t, callsPackageFunc(info, callIn("throughWrapper"), enginePackagePath, "WaitShouldStop"), "a local wrapper is not the function")
	assert.False(t, callsPackageFunc(info, callIn("throughMethod"), enginePackagePath, "WaitShouldStop"), "a method of the same name is not the function")
	assert.False(t, callsPackageFunc(info, callIn("direct"), modulePrefix+"internal/other", "WaitShouldStop"), "the package path decides")
}
