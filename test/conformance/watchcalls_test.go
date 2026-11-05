package conformance

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// Every watch the CLI opens goes through the one method that bounds it,
// internal/client/provider's WatchObject, which sits in one file. A watch
// opened anywhere else has none of that method's bounds (the open timer, the
// stream deadline, timeoutSeconds, the resourceVersion of a list) and none of
// the loop's rules. This scan fails when a production file other than that
// file does any of these:
//   - names a method called Watch that a package of k8s.io/client-go declares
//     (dynamic.ResourceInterface, rest.Interface and *rest.Request, the typed
//     clients), whatever the receiver is called, in a call or as a method
//     value (w := dyn.Resource(gvr).Watch);
//   - sets the Watch field of a metav1.ListOptions, in a composite literal or
//     by assignment, to anything but the constant false;
//   - holds a string literal that contains watch=true.
//
// A watch adds no log path that a Get has not: production code imports
// neither k8s.io/client-go/tools/watch nor k8s.io/client-go/tools/cache,
// which log at error level, and calls no setter of klog's output or
// verbosity (InitFlags, SetOutput, SetOutputBySeverity, SetLogger,
// SetLoggerWithOptions, SetSlogLogger) and no flag.Set of one of klog's
// flags. These rules apply to every production file, the watch file included.
// Test files are not production code and are skipped.
//
// Type information decides, never a name. Holes the scan cannot close: a
// watch through a method that is not called Watch, a request built from
// pieces (a query assembled from separate literals, or a path and a query sent
// through a raw http.Client), a Watch method reached through an interface that
// client-go does not declare, a generated client of another module, and a file
// behind a build tag the scan host does not match.

const (
	watchFile        = "internal/client/provider/watch.go"
	watchFixtureDir  = "test/conformance/testdata/watchscan"
	clientGoPath     = "k8s.io/client-go"
	klogImportPath   = "k8s.io/klog/v2"
	ruleWatchCall    = "a Watch call on a client-go type"
	ruleWatchOption  = "Watch set on metav1.ListOptions"
	ruleWatchLiteral = "a string literal that holds watch=true"
	ruleWatchImport  = "an import of a package that logs at error level"
	ruleKlogSetter   = "a call that changes klog's output or verbosity"
)

var (
	bannedWatchImports = map[string]bool{
		"k8s.io/client-go/tools/watch": true,
		"k8s.io/client-go/tools/cache": true,
	}
	klogSetters = map[string]bool{
		"InitFlags": true, "SetOutput": true, "SetOutputBySeverity": true, "SetLogger": true,
		"SetLoggerWithOptions": true, "SetSlogLogger": true,
	}
	klogFlags = map[string]bool{
		"v": true, "vmodule": true, "logtostderr": true, "alsologtostderr": true, "stderrthreshold": true,
		"log_file": true, "log_dir": true, "skip_headers": true, "one_output": true,
	}
)

type watchFinding struct {
	file string
	line int
	rule string
}

func (f watchFinding) String() string {
	return f.file + ":" + strconv.Itoa(f.line) + ": " + f.rule
}

// isClientGoPackage reports whether path is k8s.io/client-go or a package of it.
func isClientGoPackage(path string) bool {
	return path == clientGoPath || strings.HasPrefix(path, clientGoPath+"/")
}

// isConstantFalse reports whether e is a constant expression with the value
// false.
func isConstantFalse(info *types.Info, e ast.Expr) bool {
	tv, ok := info.Types[e]
	return ok && tv.Value != nil && tv.Value.Kind() == constant.Bool && !constant.BoolVal(tv.Value)
}

// isListOptionsWatch reports whether id names the Watch field of
// metav1.ListOptions.
func isListOptionsWatch(info *types.Info, id *ast.Ident) bool {
	v, ok := info.Uses[id].(*types.Var)
	return ok && v.IsField() && v.Name() == "Watch" && v.Pkg() != nil && v.Pkg().Path() == metav1Path
}

// calleeIn returns the function or method call invokes when its package has
// path pkgPath.
func calleeIn(info *types.Info, call *ast.CallExpr, pkgPath string) *types.Func {
	var id *ast.Ident
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		id = fun
	case *ast.SelectorExpr:
		id = fun.Sel
	default:
		return nil
	}
	fn, _ := info.Uses[id].(*types.Func)
	if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != pkgPath {
		return nil
	}
	return fn
}

// scanWatchUses returns the findings of the production files of pkg. The
// file exempt, a path relative to the working directory, is not scanned for
// the watch rules; "" exempts none.
func scanWatchUses(pkg *packages.Package, wd, exempt string) []watchFinding {
	info := pkg.TypesInfo
	var out []watchFinding
	for _, f := range pkg.Syntax {
		filename := pkg.Fset.Position(f.Pos()).Filename
		if strings.HasSuffix(filename, "_test.go") {
			continue
		}
		rel := relativeFile(wd, filename)
		add := func(pos token.Pos, rule string) {
			out = append(out, watchFinding{file: rel, line: pkg.Fset.Position(pos).Line, rule: rule})
		}
		for _, imp := range f.Imports {
			if path, err := strconv.Unquote(imp.Path.Value); err == nil && bannedWatchImports[path] {
				add(imp.Pos(), ruleWatchImport)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if fn := calleeIn(info, n, klogImportPath); fn != nil && klogSetters[fn.Name()] {
					add(n.Pos(), ruleKlogSetter)
				}
				if fn := calleeIn(info, n, "flag"); fn != nil && fn.Name() == "Set" && len(n.Args) > 0 {
					if tv, ok := info.Types[n.Args[0]]; ok && tv.Value != nil && tv.Value.Kind() == constant.String &&
						klogFlags[constant.StringVal(tv.Value)] {
						add(n.Pos(), ruleKlogSetter)
					}
				}
			case *ast.SelectorExpr:
				if rel == exempt || n.Sel.Name != "Watch" {
					return true
				}
				if s := info.Selections[n]; s != nil {
					if fn, ok := s.Obj().(*types.Func); ok && fn.Pkg() != nil && isClientGoPackage(fn.Pkg().Path()) {
						add(n.Pos(), ruleWatchCall)
					}
				}
			case *ast.KeyValueExpr:
				if rel == exempt {
					return true
				}
				if id, ok := n.Key.(*ast.Ident); ok && isListOptionsWatch(info, id) && !isConstantFalse(info, n.Value) {
					add(n.Pos(), ruleWatchOption)
				}
			case *ast.AssignStmt:
				if rel == exempt || len(n.Lhs) != 1 || len(n.Rhs) != 1 {
					return true
				}
				if sel, ok := n.Lhs[0].(*ast.SelectorExpr); ok && isListOptionsWatch(info, sel.Sel) && !isConstantFalse(info, n.Rhs[0]) {
					add(n.Pos(), ruleWatchOption)
				}
			case *ast.BasicLit:
				if rel == exempt || n.Kind != token.STRING {
					return true
				}
				if s, err := strconv.Unquote(n.Value); err == nil && strings.Contains(s, "watch=true") {
					add(n.Pos(), ruleWatchLiteral)
				}
			}
			return true
		})
	}
	return out
}

// loadWatchFindings type-checks pattern (production files only count) and
// scans every package.
func loadWatchFindings(t *testing.T, pattern, exempt string) []watchFinding {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	pkgs, err := loadRequestWaitPackages(pattern, nil)
	require.NoError(t, err)
	var out []watchFinding
	for _, pkg := range pkgs {
		if strings.HasSuffix(pkg.ID, ".test") || strings.HasSuffix(pkg.ID, "_test") {
			continue
		}
		out = append(out, scanWatchUses(pkg, wd, exempt)...)
	}
	slices.SortFunc(out, func(a, b watchFinding) int {
		if c := strings.Compare(a.file, b.file); c != 0 {
			return c
		}
		return a.line - b.line
	})
	return slices.Compact(out)
}

// TestEveryWatchGoesThroughTheProviderWatchFile asserts no production file
// outside the provider's watch file opens a watch, and no production file
// imports a package that logs at error level or changes klog's output or
// verbosity. It also asserts the scan sees the Watch call in the watch
// file, so a scan that stopped seeing watches would not pass vacuously.
func TestEveryWatchGoesThroughTheProviderWatchFile(t *testing.T) {
	t.Parallel()
	for _, f := range loadWatchFindings(t, "./...", watchFile) {
		t.Errorf("%s", f)
	}
	var inWatchFile []watchFinding
	for _, f := range loadWatchFindings(t, "./internal/client/provider/", "") {
		if f.file == watchFile {
			inWatchFile = append(inWatchFile, f)
		}
	}
	require.Len(t, inWatchFile, 1, "the scan finds the one Watch call of the watch file")
	assert.Equal(t, ruleWatchCall, inWatchFile[0].rule)
}

// TestWatchScanFixtures asserts the scan on type-checked fixtures: each way
// of opening a watch, each banned import and each klog setter is found once
// with its rule, and code that only looks alike is not.
func TestWatchScanFixtures(t *testing.T) {
	t.Parallel()
	got := map[string][]string{}
	for _, f := range loadWatchFindings(t, "./"+watchFixtureDir+"/...", "") {
		fixture, _, _ := strings.Cut(strings.TrimPrefix(f.file, watchFixtureDir+"/"), "/")
		got[fixture] = append(got[fixture], f.rule)
	}
	for fixture, want := range map[string][]string{
		"calldynamic":  {ruleWatchCall},
		"methodvalue":  {ruleWatchCall},
		"callrest":     {ruleWatchCall},
		"calltyped":    {ruleWatchCall},
		"optionsmade":  {ruleWatchOption},
		"optionsset":   {ruleWatchOption},
		"literal":      {ruleWatchLiteral},
		"importwatch":  {ruleWatchImport},
		"importcache":  {ruleWatchImport},
		"klogsetters":  {ruleKlogSetter, ruleKlogSetter, ruleKlogSetter, ruleKlogSetter},
		"lookalike":    nil,
		"exemptedfile": {ruleWatchImport, ruleWatchCall},
	} {
		assert.Equal(t, want, got[fixture], "fixture %s", fixture)
	}
}

// TestWatchScanExemptionCoversOnlyTheWatchRules asserts the exempt file is
// not scanned for watch calls, options and literals, and still is for the
// banned imports.
func TestWatchScanExemptionCoversOnlyTheWatchRules(t *testing.T) {
	t.Parallel()
	exempt := watchFixtureDir + "/exemptedfile/f.go"
	var rules []string
	for _, f := range loadWatchFindings(t, "./"+watchFixtureDir+"/exemptedfile/", exempt) {
		rules = append(rules, f.rule)
	}
	assert.Equal(t, []string{ruleWatchImport}, rules)
}
