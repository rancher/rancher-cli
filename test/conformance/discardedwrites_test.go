package conformance

import (
	"fmt"
	"go/ast"
	"go/types"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

const discardedWritesDir = "test/conformance/testdata/discardedwrites"

// discardScopeExcluded are the packages and the directories whose files the
// scan does not read, by path under the module: the packages that only
// support tests, and every directory called testdata.
var discardScopeExcluded = []string{
	"internal/testrancher",
	"internal/commands/cmdtest",
	"internal/envtestlog",
	"internal/remedytest",
	"internal/runtime/completion/completiontest",
}

// discardedWrite is one place a call of the output path drops its error.
type discardedWrite struct {
	function string
	pos      string
	callee   string
}

// discardAllowance is the number of hits an allowlisted function has, and
// why dropping the error is correct there.
type discardAllowance struct {
	count int
	why   string
}

// discardedWriteAllowlist maps the function a dropped error is in, as
// enclosingFunctionKey writes it, to the hits it has and the reason each is
// correct: no other channel carries the failure, the write cannot fail, or
// the writer is not the command's standard output or standard error.
var discardedWriteAllowlist = map[string]discardAllowance{
	"cmd/rancher-credential-vault.main":         {1, "a start-up failure of a separate process that has no pipeline: nothing else carries it, and the exit code is 1 either way"},
	"cmd/rancher-credential-vault.run":          {6, "the usage text, the version and the unknown flag message of a separate process that has no pipeline: nothing else carries them; a failed write of --version or --help to stdout exits 0, accepted because the only thing the process prints is lost with a closed stdout and nothing is changed"},
	"cmd/rancher.main":                          {1, "the failure to build the command tree, before any pipeline exists: nothing else carries it, and the exit code is 1 either way"},
	"credhelper.serve":                          {1, "the usage or failure message of a credential helper, a separate process with no pipeline: nothing else carries it, and the exit code is 1 either way"},
	"internal/cli.OutdatedCompletionScript":     {1, "the one line about an out of date completion script, written to standard error before any pipeline exists: nothing else carries it"},
	"internal/cli.completeCurrentWord":          {3, "one completion candidate for the shell, which ends the request when the pipe closes and reads no exit code"},
	"internal/cli.completionDebugf":             {1, "one line of the opt-in completion trace: nothing else carries it and the completion result does not depend on it"},
	"internal/cli.printCompletionCandidate":     {1, "one completion candidate for the shell, which ends the request when the pipe closes and reads no exit code"},
	"internal/cli.universalShellCompleteFunc":   {2, "one completion candidate for the shell, which ends the request when the pipe closes and reads no exit code"},
	"internal/cli.wiredRegistry.root":           {1, "one completion candidate for the shell, which ends the request when the pipe closes and reads no exit code"},
	"internal/runtime.WriteError":               {1, "the one line written when the error already failed to write in its format: nothing else carries it, and the exit code is the error's own"},
	"internal/runtime.mirrorWarnings":           {1, "a warning line on standard error: the result and its exit code are decided before it, and nothing else carries a warning that failed to write"},
	"internal/telemetrysend.otlpEncoder.Encode": {1, "encodes into a bytes.Buffer the function holds, of a request that holds only strings, booleans, slices and structs of them, which cannot fail to encode"},
	"internal/token.startCallbackServer":        {2, "the page the local callback server answers to the browser, not standard output or standard error: the sign-in result does not depend on the browser receiving it"},
	"internal/ui.Prompter.Notice":               {1, "a notice on standard error: nothing else carries it and the run does not depend on it"},
	"internal/ui.Prompter.readLine":             {1, "the newline that ends an unterminated prompt after a cancelled read: the cancellation is the error returned"},
	"internal/ui.Prompter.writeErr":             {1, "the prompt label on standard error: the answer is read from standard input whether or not the label was written"},
}

// loadDiscardPackages type-checks the packages of pattern for the host and for
// each operating system the release builds (the first architecture listed for
// it), with cgo off. A file only another system builds is read with its
// types. It returns the packages of every load; a file several loads share
// appears in each, and the scan counts it once.
func loadDiscardPackages(pattern string) ([]*packages.Package, error) {
	platforms, err := releasePlatforms()
	if err != nil {
		return nil, err
	}
	envs := [][]string{append(os.Environ(), "CGO_ENABLED=0")}
	seen := map[string]bool{}
	for _, platform := range platforms {
		goos, goarch, _ := strings.Cut(platform, "/")
		if seen[goos] {
			continue
		}
		seen[goos] = true
		envs = append(envs, append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0"))
	}
	var out []*packages.Package
	for _, env := range envs {
		pkgs, err := packages.Load(&packages.Config{
			Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedFiles,
			Env:  env,
		}, pattern)
		if err != nil {
			return nil, err
		}
		for _, pkg := range pkgs {
			for _, e := range pkg.Errors {
				return nil, fmt.Errorf("type-check %s: %s", pkg.PkgPath, e)
			}
		}
		out = append(out, pkgs...)
	}
	return out, nil
}

// discardScope reports whether the scan reads the package at pkgPath.
func discardScope(pkgPath string) bool {
	rel := strings.TrimPrefix(pkgPath, modulePrefix)
	if slices.Contains(strings.Split(rel, "/"), "testdata") {
		return false
	}
	return !slices.ContainsFunc(discardScopeExcluded, func(dir string) bool {
		return rel == dir || strings.HasPrefix(rel, dir+"/")
	})
}

// ioWriterInterface is io.Writer, built here so a type from any package can be
// tested against it.
var ioWriterInterface = sync.OnceValue(func() *types.Interface {
	errType := types.Universe.Lookup("error").Type()
	bytes := types.NewSlice(types.Typ[types.Byte])
	sig := types.NewSignatureType(nil, nil, nil,
		types.NewTuple(types.NewVar(0, nil, "p", bytes)),
		types.NewTuple(types.NewVar(0, nil, "n", types.Typ[types.Int]), types.NewVar(0, nil, "err", errType)), false)
	return types.NewInterfaceType([]*types.Func{types.NewFunc(0, nil, "Write", sig)}, nil).Complete()
})

// hashInterface is hash.Hash: io.Writer with Sum, Reset, Size and BlockSize.
// A hash.Hash64 has every method of it.
var hashInterface = sync.OnceValue(func() *types.Interface {
	errType := types.Universe.Lookup("error").Type()
	bytes := types.NewSlice(types.Typ[types.Byte])
	sig := func(params, results []*types.Var) *types.Signature {
		return types.NewSignatureType(nil, nil, nil, types.NewTuple(params...), types.NewTuple(results...), false)
	}
	v := func(name string, t types.Type) *types.Var { return types.NewVar(0, nil, name, t) }
	return types.NewInterfaceType([]*types.Func{
		types.NewFunc(0, nil, "Write", sig([]*types.Var{v("p", bytes)}, []*types.Var{v("n", types.Typ[types.Int]), v("err", errType)})),
		types.NewFunc(0, nil, "Sum", sig([]*types.Var{v("b", bytes)}, []*types.Var{v("", bytes)})),
		types.NewFunc(0, nil, "Reset", sig(nil, nil)),
		types.NewFunc(0, nil, "Size", sig(nil, []*types.Var{v("", types.Typ[types.Int])})),
		types.NewFunc(0, nil, "BlockSize", sig(nil, []*types.Var{v("", types.Typ[types.Int])})),
	}, nil).Complete()
})

// implementsOrPointerDoes reports whether t, or a pointer to it, implements
// iface: a method with a pointer receiver counts for a value.
func implementsOrPointerDoes(t types.Type, iface *types.Interface) bool {
	t = types.Unalias(t)
	if types.Implements(t, iface) {
		return true
	}
	if _, isPointer := t.(*types.Pointer); isPointer {
		return false
	}
	return types.Implements(types.NewPointer(t), iface)
}

// cannotFail reports whether a write to a value of type t cannot fail: a
// strings.Builder, a bytes.Buffer or a hash.Hash, as a value or a pointer.
func cannotFail(t types.Type) bool {
	if t == nil {
		return false
	}
	d := derefType(t)
	return isNamedType(d, "strings", "Builder") || isNamedType(d, "bytes", "Buffer") ||
		implementsOrPointerDoes(t, hashInterface())
}

// urfaveHelpPrinters are the functions, and the variables that hold a
// function, of urfave/cli that print a help card. They return nil whether or
// not the card was written, because the card goes through HelpPrinter, which
// has no result; printStyledHelp in internal/cli keeps the write error for
// the run. They are listed so that a call whose result is dropped is
// reviewed.
var urfaveHelpPrinters = []string{
	"ShowRootCommandHelp", "ShowAppHelp", "ShowCommandHelp", "ShowSubcommandHelp",
	"DefaultShowRootCommandHelp", "DefaultShowCommandHelp", "DefaultShowSubcommandHelp",
}

// writerMethods are the methods that write to a writer, or flush what it
// holds, and report the failure.
var writerMethods = []string{"Write", "WriteString", "WriteByte", "WriteRune", "Flush", "Encode"}

// discardedCallee returns the name of the render or write call that call is
// and whose error the scan holds a source to use, or "" when call is neither.
// A render call is a function or method of a package in renderPkgs whose name
// starts with Render or render and whose last result is error. A write call
// is a function of fmt or io that writes to its first argument, or one of
// writerMethods on a receiver that implements io.Writer, or on a
// json.Encoder; a call on a value that cannot fail is not one.
func discardedCallee(info *types.Info, call *ast.CallExpr, renderPkgs []string) string {
	last := info.TypeOf(call)
	if tuple, ok := last.(*types.Tuple); ok {
		if tuple.Len() == 0 {
			return ""
		}
		last = tuple.At(tuple.Len() - 1).Type()
	}
	if last == nil || !types.Identical(last, types.Universe.Lookup("error").Type()) {
		return ""
	}
	var fn *types.Func
	var receiver ast.Expr
	var used types.Object
	switch f := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		used = info.Uses[f]
	case *ast.SelectorExpr:
		if sel := info.Selections[f]; sel != nil {
			used = sel.Obj()
			receiver = f.X
		} else {
			used = info.Uses[f.Sel]
		}
	}
	if v, ok := used.(*types.Var); ok && !v.IsField() && v.Pkg() != nil && v.Pkg().Path() == urfaveCLIPath && slices.Contains(urfaveHelpPrinters, v.Name()) {
		return v.Pkg().Name() + "." + v.Name()
	}
	fn, _ = used.(*types.Func)
	if fn == nil || fn.Pkg() == nil {
		return ""
	}
	name := fn.Name()
	if slices.Contains(renderPkgs, fn.Pkg().Path()) && (strings.HasPrefix(name, "Render") || strings.HasPrefix(name, "render")) {
		return fn.Pkg().Name() + "." + name
	}
	if receiver == nil {
		first := func() types.Type {
			if len(call.Args) == 0 {
				return nil
			}
			return info.TypeOf(call.Args[0])
		}
		switch fn.Pkg().Path() + "." + name {
		case "fmt.Fprint", "fmt.Fprintf", "fmt.Fprintln", "io.WriteString", "io.Copy", "io.CopyN", "io.CopyBuffer":
			if cannotFail(first()) {
				return ""
			}
			return fn.Pkg().Name() + "." + name
		case "fmt.Print", "fmt.Printf", "fmt.Println":
			return fn.Pkg().Name() + "." + name
		}
		if fn.Pkg().Path() == urfaveCLIPath && slices.Contains(urfaveHelpPrinters, name) {
			return fn.Pkg().Name() + "." + name
		}
		return ""
	}
	if !slices.Contains(writerMethods, name) {
		return ""
	}
	t := info.TypeOf(receiver)
	if t == nil || cannotFail(t) {
		return ""
	}
	if isNamedType(derefType(t), "encoding/json", "Encoder") || implementsOrPointerDoes(t, ioWriterInterface()) {
		return types.TypeString(derefType(t), func(p *types.Package) string { return p.Name() }) + "." + name
	}
	return ""
}

// discardedWrites returns the calls in the non-test files of pkgs that drop the
// error of a render or write call, in the shapes of an expression statement,
// an assignment whose last target is _, and a defer or go statement. A file
// that several packages share, as the loads of several systems do, is read
// once. files is the number of files read.
func discardedWrites(pkgs []*packages.Package, renderPkgs []string) (hits []discardedWrite, files int) {
	seenHits := map[string]bool{}
	seenFiles := map[string]bool{}
	for _, pkg := range pkgs {
		info := pkg.TypesInfo
		for _, f := range pkg.Syntax {
			filename := pkg.Fset.Position(f.Pos()).Filename
			if !seenFiles[filename] {
				seenFiles[filename] = true
				files++
			}
			for _, decl := range f.Decls {
				function := declarationFunction(pkg, decl)
				note := func(call *ast.CallExpr) {
					callee := discardedCallee(info, call, renderPkgs)
					if callee == "" {
						return
					}
					pos := pkg.Fset.Position(call.Pos()).String()
					if seenHits[pos] {
						return
					}
					seenHits[pos] = true
					hits = append(hits, discardedWrite{function: function, pos: pos, callee: callee})
				}
				ast.Inspect(decl, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.ExprStmt:
						if call, ok := ast.Unparen(n.X).(*ast.CallExpr); ok {
							note(call)
						}
					case *ast.AssignStmt:
						if len(n.Rhs) != 1 {
							return true
						}
						if id, ok := n.Lhs[len(n.Lhs)-1].(*ast.Ident); ok && id.Name == "_" {
							if call, ok := ast.Unparen(n.Rhs[0]).(*ast.CallExpr); ok {
								note(call)
							}
						}
					case *ast.DeferStmt:
						note(n.Call)
					case *ast.GoStmt:
						note(n.Call)
					}
					return true
				})
			}
		}
	}
	return hits, files
}

// discardedWriteProblems compares hits with allow: a hit in a function with
// no row, a row whose count differs from the hits of its function, a row with
// no hit (stale) and a row with no reason.
func discardedWriteProblems(hits []discardedWrite, allow map[string]discardAllowance) []string {
	counts := map[string]int{}
	var positions = map[string][]string{}
	for _, h := range hits {
		counts[h.function]++
		positions[h.function] = append(positions[h.function], h.pos+" "+h.callee)
	}
	var problems []string
	for _, function := range slices.Sorted(maps.Keys(counts)) {
		row, ok := allow[function]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s drops the error of a render or write call (use it, or add a row with the reason it cannot matter):\n\t%s",
				function, strings.Join(positions[function], "\n\t")))
		case row.count != counts[function]:
			problems = append(problems, fmt.Sprintf("%s has %d dropped render or write errors and its row says %d:\n\t%s",
				function, counts[function], row.count, strings.Join(positions[function], "\n\t")))
		}
	}
	for _, function := range slices.Sorted(maps.Keys(allow)) {
		if counts[function] == 0 {
			problems = append(problems, fmt.Sprintf("the row for %s matches nothing: remove it", function))
		}
		if strings.TrimSpace(allow[function].why) == "" {
			problems = append(problems, fmt.Sprintf("the row for %s has no reason", function))
		}
	}
	return problems
}

// discardScanPackages type-checks the packages of the module the scan reads.
var discardScanPackages = sync.OnceValues(func() ([]*packages.Package, error) {
	pkgs, err := loadDiscardPackages("./...")
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(pkgs, func(pkg *packages.Package) bool { return !discardScope(pkg.PkgPath) }), nil
})

// TestNoRenderOrWriteErrorIsDiscarded asserts that no non-test code of the
// module drops the error of a render or write call of the output path: a
// function of internal/output or internal/runtime whose name starts with
// Render or render and returns an error, fmt.Print, Printf, Println, Fprint,
// Fprintf and Fprintln, io.WriteString, io.Copy, io.CopyN and io.CopyBuffer,
// the help card printers of urfave/cli, and the Write, WriteString,
// WriteByte, WriteRune, Flush and Encode methods of a writer. Every system
// the release builds is read; the result does not depend on the machine that
// runs the test. A write to a strings.Builder, a bytes.Buffer or a hash
// cannot fail and is not a hit. A hit needs a row of discardedWriteAllowlist
// with its count and the reason dropping the error is correct. The scan does
// not see a writer that is not in these lists, and it does not see a bare
// call of a function that returns an error and renders without a name that
// starts with Render or render (renderDepsError in internal/cli is one):
// those are for review.
func TestNoRenderOrWriteErrorIsDiscarded(t *testing.T) {
	t.Parallel()
	pkgs, err := discardScanPackages()
	require.NoError(t, err)
	hits, files := discardedWrites(pkgs, []string{outputPath, runtimePath})
	assert.Greater(t, files, 100, "the scan read too few files")
	assert.Empty(t, discardedWriteProblems(hits, discardedWriteAllowlist))
}

// discardFixtures reads the fixture package the way the module is read.
var discardFixtures = sync.OnceValues(func() ([]*packages.Package, error) {
	return loadDiscardPackages("./" + discardedWritesDir + "/...")
})

// TestDiscardedWriteScanFixtures asserts the scan finds exactly the planted
// drops of the fixture package: a render call and a write call dropped by a
// bare statement, by an assignment to _ and by a go or defer statement, a
// method of a type that implements io.Writer, an io.Writer value, a
// json.Encoder, fmt.Print, Printf and Println, io.CopyN and io.CopyBuffer and
// the help card printers of urfave/cli, a drop in a function literal and at
// package level, and a drop in a file only windows builds; and none of the
// writes to a strings.Builder, bytes.Buffer, hash.Hash and hash.Hash64, as
// values and pointers, the calls that use their result, and a render function
// that returns no error.
func TestDiscardedWriteScanFixtures(t *testing.T) {
	t.Parallel()
	pkgs, err := discardFixtures()
	require.NoError(t, err)
	var fixture []*packages.Package
	for _, pkg := range pkgs {
		if path.Base(pkg.PkgPath) == "discardedwrites" {
			fixture = append(fixture, pkg)
		}
	}
	require.NotEmpty(t, fixture)
	hits, _ := discardedWrites(fixture, []string{fixture[0].PkgPath})
	got := map[string]int{}
	for _, h := range hits {
		got[strings.TrimPrefix(h.function, discardedWritesDir+".")]++
	}
	assert.Equal(t, map[string]int{
		"blankAssignedRender": 1,
		"blankPairFprintln":   1,
		"blankErrorOnly":      1,
		"bareFprintln":        1,
		"bareFprintf":         1,
		"bareEncode":          1,
		"deferredFlush":       1,
		"bareRender":          1,
		"bareRenderMethod":    1,
		"bareExportedRender":  1,
		"goWrite":             1,
		"bareWriteString":     1,
		"bareCopy":            1,
		"barePrint":           1,
		"barePrintf":          1,
		"barePrintln":         1,
		"bareCopyN":           1,
		"bareCopyBuffer":      1,
		"blankRootHelp":       1,
		"bareSubcommandHelp":  1,
		"bareInterfaceWrite":  1,
		"bareWriterMethods":   2,
		"bareValueWriter":     1,
		"closureDrop":         1,
		"<package level>":     1,
		"windowsDrop":         1,
	}, got)
}

// TestDiscardedWriteAllowlistRules asserts the rules on the hits: a function
// with no row is reported, a count that differs fails, a row that matches
// nothing is stale, and a row with no reason fails.
func TestDiscardedWriteAllowlistRules(t *testing.T) {
	t.Parallel()
	hit := discardedWrite{function: "a.f", pos: "a.go:1:1", callee: "fmt.Fprintln"}
	allow := map[string]discardAllowance{"a.f": {1, "why"}}
	assert.Empty(t, discardedWriteProblems([]discardedWrite{hit}, allow), "a row with its count passes")
	assert.Len(t, discardedWriteProblems([]discardedWrite{hit}, nil), 1, "a function with no row is reported")
	assert.Len(t, discardedWriteProblems([]discardedWrite{hit, hit}, allow), 1, "a count that differs fails")
	assert.Equal(t, []string{"the row for b.g matches nothing: remove it"},
		discardedWriteProblems([]discardedWrite{hit}, map[string]discardAllowance{"a.f": {1, "why"}, "b.g": {1, "why"}}), "a row with no hit is stale")
	assert.Equal(t, []string{"the row for a.f has no reason"},
		discardedWriteProblems([]discardedWrite{hit}, map[string]discardAllowance{"a.f": {1, " "}}), "a row with no reason fails")
}
