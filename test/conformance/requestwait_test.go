package conformance

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// A handler that waits on its request's context and then returns makes
// net/http write an implicit 200, and a client whose cancellation lands in the
// same instant can read that answer instead of its own error. Every wait on
// a request's context therefore goes through the helpers of
// internal/testtransport, which abort the connection (or, for a hook of the
// test Rancher, return and let it abort its answer). This scan fails when any
// Go file in the repository, tests included, calls Done on a context that
// comes from an *http.Request outside that package.
//
// A context comes from a request when it is one of these (type information
// decides, never the name of a variable):
//   - the result of Context on an *http.Request: a call, a method expression
//     ((*http.Request).Context(r)), or Context on an interface variable that
//     was assigned or passed an *http.Request;
//   - a variable, struct field or parameter of a function of the same package
//     that is assigned or passed such a context;
//   - a context-typed result (every context position of a tuple) of a call
//     that has such a context or an *http.Request among its arguments:
//     context.With*, signal.NotifyContext, errgroup.WithContext and helpers.
//
// context.WithoutCancel never ends: neither it nor a context built on its
// result counts. context.AfterFunc on a request's context is refused too:
// its function runs when the client has gone, which is a wait. The scan looks
// for the Done call, not for the receive: a channel kept from it (done :=
// r.Context().Done()) is found.
//
// Type information comes from one packages.Load per set of custom build tags
// the repository's files name, with the test variants of every package. A
// file that no load saw (Windows-only files, files behind a platform tag the
// scan host does not match) goes through a syntax-only pass that decides by
// name: Context called on an identifier named r, req, request or in, the
// variables assigned from it, and context.With* (minus WithoutCancel),
// signal.NotifyContext and errgroup.WithContext applied to one.
//
// Holes the scan cannot close: a request's context handed to a function of
// another package that waits on it; a closure or function value that waits on
// its own parameter; a context a function returns after reaching the request
// some other way (a captured variable or a field); a method value taken from
// the request (f := r.Context) or from the context (d := ctx.Done) and called
// later; a context sent through a channel or stored in a slice or map; a wait
// that polls Err instead of calling Done; and, in the name-based pass, a
// helper that returns a derived context, a method expression, and Context
// through an interface.

const (
	requestWaitHelpers = "testtransport.HangAndAbort, testtransport.HangUntilReleased or testtransport.WaitUntilGone"
	packageLevelFunc   = "package level"
)

// requestWaitAllowlist maps "<file>:<function>" to the reason the function
// may wait on a request's context. An entry is a user decision. The function
// is the declared function or method that holds the call (Recv.Name for a
// method).
var requestWaitAllowlist = map[string]string{
	"internal/client/httpx/useragent_test.go:cancelAwareTransport.RoundTrip": "a client-side transport: it waits on the context of the request it was asked to send and returns an error, and a client has no response to abort",
	"internal/client/tlstrust/wrap_test.go:legacyCancelTransport.RoundTrip":  "a client-side transport: it waits on the context of the request it was asked to send and returns an error, and a client has no response to abort",
}

// The kinds of wait the scan finds.
const (
	waitKindDone      = "Done"
	waitKindAfterFunc = "AfterFunc"
)

// requestWait is one wait on a context that comes from a request: a call of
// Done on it, or a call of context.AfterFunc with it.
type requestWait struct {
	file      string
	line, col int
	fn        string
	kind      string
	// exempt is set for a call in the package that holds the helpers.
	exempt bool
}

func (w requestWait) position() string { return fmt.Sprintf("%s:%d:%d", w.file, w.line, w.col) }

func (w requestWait) allowKey() string { return w.file + ":" + w.fn }

// message is the finding for a wait outside the helpers.
func (w requestWait) message() string {
	what := "calls Done on the context of a request"
	if w.kind == waitKindAfterFunc {
		what = "calls context.AfterFunc on the context of a request, which runs its function once the client has gone"
	}
	return fmt.Sprintf("%s: %s %s; wait with %s", w.position(), w.fn, what, requestWaitHelpers)
}

// calleeFunc returns the declared function or method call invokes, or nil.
func calleeFunc(info *types.Info, call *ast.CallExpr) *types.Func {
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
	return fn
}

// isContextFunc reports whether call invokes the function of package context
// with the given name.
func isContextFunc(info *types.Info, call *ast.CallExpr, name string) bool {
	fn := calleeFunc(info, call)
	return fn != nil && fn.Pkg() != nil && fn.Pkg().Path() == "context" && fn.Name() == name
}

// isContextType reports whether t is context.Context.
func isContextType(t types.Type) bool {
	if t == nil {
		return false
	}
	named, ok := types.Unalias(t).(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "context" && named.Obj().Name() == "Context"
}

// hasContextResult reports whether t, the type of a call, is context.Context
// or a tuple with a context.Context element.
func hasContextResult(t types.Type) bool {
	if tuple, ok := t.(*types.Tuple); ok {
		for v := range tuple.Variables() {
			if isContextType(v.Type()) {
				return true
			}
		}
		return false
	}
	return isContextType(t)
}

// isRequestType reports whether t is net/http.Request or a pointer to it.
func isRequestType(t types.Type) bool {
	named, ok := derefType(t).(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "net/http" && named.Obj().Name() == "Request"
}

// isRequestContextCall reports whether call is Context called on an
// *http.Request (through a value that embeds one, or a defined type over it)
// or the method expression (*http.Request).Context(r).
func isRequestContextCall(info *types.Info, call *ast.CallExpr) bool {
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Context" {
		return false
	}
	s := info.Selections[sel]
	if s == nil {
		return false
	}
	wantArgs := 0
	if s.Kind() == types.MethodExpr {
		wantArgs = 1
	}
	if len(call.Args) != wantArgs {
		return false
	}
	fn, ok := s.Obj().(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != "net/http" {
		return false
	}
	sig, ok := fn.Type().(*types.Signature)
	return ok && sig.Recv() != nil && isRequestType(sig.Recv().Type())
}

// requestContexts holds what one type-checked package assigns or passes that
// comes from a request.
type requestContexts struct {
	pkg  *packages.Package
	info *types.Info
	// contexts are the variables, fields and parameters that hold a context
	// from a request.
	contexts map[types.Object]bool
	// requests are the variables, fields and parameters of an interface type
	// that were assigned or passed an *http.Request.
	requests map[types.Object]bool
}

func (c *requestContexts) object(e ast.Expr) types.Object {
	switch e := ast.Unparen(e).(type) {
	case *ast.Ident:
		return c.info.ObjectOf(e)
	case *ast.SelectorExpr:
		if s := c.info.Selections[e]; s != nil {
			return s.Obj()
		}
		return c.info.Uses[e.Sel]
	}
	return nil
}

// isRequestValue reports whether e is an *http.Request, or an interface value
// that holds one.
func (c *requestContexts) isRequestValue(e ast.Expr) bool {
	if isRequestType(c.info.TypeOf(e)) {
		return true
	}
	obj := c.object(e)
	return obj != nil && c.requests[obj]
}

// isContext reports whether e is a context from a request.
func (c *requestContexts) isContext(e ast.Expr) bool {
	e = ast.Unparen(e)
	call, ok := e.(*ast.CallExpr)
	if !ok {
		obj := c.object(e)
		return obj != nil && c.contexts[obj]
	}
	if isRequestContextCall(c.info, call) {
		return true
	}
	if isContextFunc(c.info, call, "WithoutCancel") {
		return false
	}
	if sel, isSel := ast.Unparen(call.Fun).(*ast.SelectorExpr); isSel && sel.Sel.Name == "Context" && len(call.Args) == 0 {
		if obj := c.object(sel.X); obj != nil && c.requests[obj] {
			return true
		}
	}
	if !hasContextResult(c.info.TypeOf(call)) {
		return false
	}
	for _, arg := range call.Args {
		if c.isContext(arg) || c.isRequestValue(arg) {
			return true
		}
	}
	return false
}

// mark records the variable or field that e names in set, and reports whether
// that is news.
func (c *requestContexts) mark(set map[types.Object]bool, e ast.Expr) bool {
	obj := c.object(e)
	if obj == nil || set[obj] {
		return false
	}
	set[obj] = true
	return true
}

// propagate marks, until nothing changes, every variable, field and
// parameter the package assigns or passes a context from a request, or an
// *http.Request through an interface type.
func (c *requestContexts) propagate() {
	for changed := true; changed; {
		changed = false
		for _, f := range c.pkg.Syntax {
			ast.Inspect(f, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.AssignStmt:
					changed = c.markAssigned(n.Lhs, n.Rhs) || changed
				case *ast.ValueSpec:
					names := make([]ast.Expr, len(n.Names))
					for i, id := range n.Names {
						names[i] = id
					}
					changed = c.markAssigned(names, n.Values) || changed
				case *ast.CompositeLit:
					changed = c.markFields(n) || changed
				case *ast.CallExpr:
					changed = c.markParams(n) || changed
				}
				return true
			})
		}
	}
}

// markAssigned handles lhs = rhs, including a call whose results include a
// context (context.WithCancel, errgroup.WithContext): each context position
// of the tuple is marked.
func (c *requestContexts) markAssigned(lhs, rhs []ast.Expr) bool {
	changed := false
	switch {
	case len(lhs) == len(rhs):
		for i := range lhs {
			switch {
			case c.isContext(rhs[i]):
				changed = c.mark(c.contexts, lhs[i]) || changed
			case c.isRequestValue(rhs[i]) && types.IsInterface(c.info.TypeOf(lhs[i])):
				changed = c.mark(c.requests, lhs[i]) || changed
			}
		}
	case len(rhs) == 1 && len(lhs) > 1:
		tuple, ok := c.info.TypeOf(rhs[0]).(*types.Tuple)
		if !ok || !c.isContext(rhs[0]) {
			return false
		}
		for i := range min(tuple.Len(), len(lhs)) {
			if isContextType(tuple.At(i).Type()) {
				changed = c.mark(c.contexts, lhs[i]) || changed
			}
		}
	}
	return changed
}

// markFields handles a struct literal that sets a field to a context from a
// request, or an interface field to an *http.Request, keyed or positional.
func (c *requestContexts) markFields(lit *ast.CompositeLit) bool {
	t := c.info.TypeOf(lit)
	if t == nil {
		return false
	}
	st, ok := derefType(t).Underlying().(*types.Struct)
	if !ok {
		return false
	}
	changed := false
	for i, elt := range lit.Elts {
		var field *types.Var
		value := elt
		if kv, isKV := elt.(*ast.KeyValueExpr); isKV {
			id, isID := kv.Key.(*ast.Ident)
			if !isID {
				continue
			}
			field, _ = c.info.Uses[id].(*types.Var)
			value = kv.Value
		} else if i < st.NumFields() {
			field = st.Field(i)
		}
		switch {
		case field == nil:
		case c.isContext(value) && !c.contexts[field]:
			c.contexts[field] = true
			changed = true
		case c.isRequestValue(value) && types.IsInterface(field.Type()) && !c.requests[field]:
			c.requests[field] = true
			changed = true
		}
	}
	return changed
}

// markParams handles a call of a declared function or method of this
// package with a context from a request, or an *http.Request for an
// interface parameter, as an argument.
func (c *requestContexts) markParams(call *ast.CallExpr) bool {
	fn := calleeFunc(c.info, call)
	if fn == nil || fn.Pkg() != c.pkg.Types {
		return false
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return false
	}
	changed := false
	for i, arg := range call.Args {
		if i >= sig.Params().Len() {
			break
		}
		p := sig.Params().At(i)
		switch {
		case c.isContext(arg) && !c.contexts[p]:
			c.contexts[p] = true
			changed = true
		case c.isRequestValue(arg) && types.IsInterface(p.Type()) && !c.requests[p]:
			c.requests[p] = true
			changed = true
		}
	}
	return changed
}

// receiverBaseTypeName returns the type name of a method receiver expression.
func receiverBaseTypeName(e ast.Expr) string {
	for {
		switch t := e.(type) {
		case *ast.StarExpr:
			e = t.X
		case *ast.ParenExpr:
			e = t.X
		case *ast.IndexExpr:
			e = t.X
		case *ast.IndexListExpr:
			e = t.X
		case *ast.Ident:
			return t.Name
		default:
			return ""
		}
	}
}

// declName names a declaration the way an allowlist entry does.
func declName(d ast.Decl) string {
	fd, ok := d.(*ast.FuncDecl)
	if !ok {
		return packageLevelFunc
	}
	if fd.Recv != nil && len(fd.Recv.List) == 1 {
		return receiverBaseTypeName(fd.Recv.List[0].Type) + "." + fd.Name.Name
	}
	return fd.Name.Name
}

// isDoneCall reports whether n is a call of a method named Done with no
// arguments, and returns the receiver expression.
func isDoneCall(n ast.Node) (*ast.CallExpr, ast.Expr, bool) {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return nil, nil, false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Done" {
		return nil, nil, false
	}
	return call, sel.X, true
}

// relativeFile returns name as a slash path relative to wd.
func relativeFile(wd, name string) string {
	rel, err := filepath.Rel(wd, name)
	if err != nil {
		rel = name
	}
	return filepath.ToSlash(rel)
}

// scanRequestWaitsTyped returns the waits on a context from a request in one
// type-checked package. exemptPath is the import path of the package that
// holds the helpers.
func scanRequestWaitsTyped(pkg *packages.Package, wd, exemptPath string) []requestWait {
	c := &requestContexts{pkg: pkg, info: pkg.TypesInfo, contexts: map[types.Object]bool{}, requests: map[types.Object]bool{}}
	c.propagate()
	var out []requestWait
	for _, f := range pkg.Syntax {
		name := relativeFile(wd, pkg.Fset.Position(f.Pos()).Filename)
		for _, d := range f.Decls {
			fn := declName(d)
			add := func(call *ast.CallExpr, kind string) {
				pos := pkg.Fset.Position(call.Pos())
				out = append(out, requestWait{file: name, line: pos.Line, col: pos.Column, fn: fn, kind: kind, exempt: pkg.PkgPath == exemptPath})
			}
			ast.Inspect(d, func(n ast.Node) bool {
				if call, recv, ok := isDoneCall(n); ok && c.isContext(recv) {
					add(call, waitKindDone)
				}
				if call, ok := n.(*ast.CallExpr); ok && isContextFunc(c.info, call, "AfterFunc") && len(call.Args) > 0 && c.isContext(call.Args[0]) {
					add(call, waitKindAfterFunc)
				}
				return true
			})
		}
	}
	return out
}

// requestNames are the variable names the syntax-only pass takes for an
// *http.Request.
var requestNames = []string{"r", "req", "request", "in"}

// qualifiedCall returns the package identifier and the function name of a
// call written as pkg.Name(...).
func qualifiedCall(call *ast.CallExpr) (pkg, name string) {
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return "", ""
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", ""
	}
	return id.Name, sel.Sel.Name
}

// scanRequestWaitsByName is the syntax-only pass for a file no type-checked
// load saw. It decides by name, which is why it is the fallback. A file in
// exemptDir is the helpers' own.
func scanRequestWaitsByName(fset *token.FileSet, file *ast.File, name, exemptDir string) []requestWait {
	tainted := map[string]bool{}
	label := func(e ast.Expr) string {
		switch e := ast.Unparen(e).(type) {
		case *ast.Ident:
			return e.Name
		case *ast.SelectorExpr:
			return e.Sel.Name
		}
		return ""
	}
	var isContext func(e ast.Expr) bool
	isContext = func(e ast.Expr) bool {
		e = ast.Unparen(e)
		call, ok := e.(*ast.CallExpr)
		if !ok {
			l := label(e)
			return l != "" && tainted[l]
		}
		if sel, isSel := ast.Unparen(call.Fun).(*ast.SelectorExpr); isSel && sel.Sel.Name == "Context" && len(call.Args) == 0 {
			return slices.Contains(requestNames, label(sel.X))
		}
		pkg, fn := qualifiedCall(call)
		derives := pkg == "context" && strings.HasPrefix(fn, "With") ||
			pkg == "signal" && fn == "NotifyContext" ||
			pkg == "errgroup" && fn == "WithContext"
		return derives && fn != "WithoutCancel" && len(call.Args) > 0 && isContext(call.Args[0])
	}
	// resultIndex is the position of the context among a call's results.
	resultIndex := func(e ast.Expr) int {
		if call, ok := ast.Unparen(e).(*ast.CallExpr); ok {
			if pkg, fn := qualifiedCall(call); pkg == "errgroup" && fn == "WithContext" {
				return 1
			}
		}
		return 0
	}
	mark := func(e ast.Expr) bool {
		l := label(e)
		if l == "" || l == "_" || tainted[l] {
			return false
		}
		tainted[l] = true
		return true
	}
	for changed := true; changed; {
		changed = false
		ast.Inspect(file, func(n ast.Node) bool {
			var lhs, rhs []ast.Expr
			switch n := n.(type) {
			case *ast.AssignStmt:
				lhs, rhs = n.Lhs, n.Rhs
			case *ast.ValueSpec:
				for _, id := range n.Names {
					lhs = append(lhs, id)
				}
				rhs = n.Values
			case *ast.KeyValueExpr:
				lhs, rhs = []ast.Expr{n.Key}, []ast.Expr{n.Value}
			}
			switch {
			case len(lhs) == len(rhs):
				for i := range lhs {
					if isContext(rhs[i]) {
						changed = mark(lhs[i]) || changed
					}
				}
			case len(rhs) == 1 && len(lhs) > 1 && isContext(rhs[0]):
				if i := resultIndex(rhs[0]); i < len(lhs) {
					changed = mark(lhs[i]) || changed
				}
			}
			return true
		})
	}
	var out []requestWait
	for _, d := range file.Decls {
		fn := declName(d)
		add := func(call *ast.CallExpr, kind string) {
			pos := fset.Position(call.Pos())
			out = append(out, requestWait{file: name, line: pos.Line, col: pos.Column, fn: fn, kind: kind, exempt: filepath.ToSlash(filepath.Dir(name)) == exemptDir})
		}
		ast.Inspect(d, func(n ast.Node) bool {
			if call, recv, ok := isDoneCall(n); ok && isContext(recv) {
				add(call, waitKindDone)
			}
			if call, ok := n.(*ast.CallExpr); ok {
				if pkg, fn := qualifiedCall(call); pkg == "context" && fn == "AfterFunc" && len(call.Args) > 0 && isContext(call.Args[0]) {
					add(call, waitKindAfterFunc)
				}
			}
			return true
		})
	}
	return out
}

// requestWaitScan is what a scan saw.
type requestWaitScan struct {
	// waits are the Done calls on a context from a request, by position.
	waits map[string]requestWait
	// seen are the absolute paths of the files a type-checked load saw.
	seen map[string]bool
	// testFiles counts the _test.go files each tag set's load saw.
	testFiles map[string]int
	// tagSets are the sets the scan loaded.
	tagSets [][]string
}

func tagSetKey(tags []string) string { return strings.Join(tags, ",") }

// requestWaitTagSets returns the sets of custom build tags the given files
// name: the empty set, then one set for each distinct set a single file
// names. A load with exactly a file's tags compiles that file in.
func requestWaitTagSets(files []string) ([][]string, error) {
	seen := map[string]bool{"": true}
	sets := [][]string{nil}
	for _, path := range files {
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		tags, err := customTagsIn(f)
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		slices.Sort(tags)
		tags = slices.Compact(tags)
		if key := tagSetKey(tags); !seen[key] {
			seen[key] = true
			sets = append(sets, tags)
		}
	}
	return sets, nil
}

// loadRequestWaitPackages type-checks the packages of pattern with their test
// variants, compiled with tags.
func loadRequestWaitPackages(pattern string, tags []string) ([]*packages.Package, error) {
	cfg := &packages.Config{
		Mode:  packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedFiles,
		Tests: true,
	}
	if len(tags) > 0 {
		cfg.BuildFlags = []string{"-tags=" + strings.Join(tags, ",")}
	}
	pkgs, err := packages.Load(cfg, pattern)
	if err != nil {
		return nil, err
	}
	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			return nil, fmt.Errorf("type-check %s (tags %q): %s", pkg.ID, tags, e)
		}
	}
	return pkgs, nil
}

// scanRequestWaits loads pattern once per tag set and collects the waits of
// every package variant, each position once.
func scanRequestWaits(pattern string, tagSets [][]string, exemptPath, wd string) (*requestWaitScan, error) {
	scan := &requestWaitScan{waits: map[string]requestWait{}, seen: map[string]bool{}, testFiles: map[string]int{}, tagSets: tagSets}
	for _, tags := range tagSets {
		pkgs, err := loadRequestWaitPackages(pattern, tags)
		if err != nil {
			return nil, err
		}
		tests := map[string]bool{}
		for _, pkg := range pkgs {
			if strings.HasSuffix(pkg.ID, ".test") {
				continue
			}
			for _, f := range pkg.Syntax {
				name := pkg.Fset.Position(f.Pos()).Filename
				scan.seen[name] = true
				if strings.HasSuffix(name, "_test.go") {
					tests[name] = true
				}
			}
			for _, w := range scanRequestWaitsTyped(pkg, wd, exemptPath) {
				scan.waits[w.position()] = w
			}
		}
		scan.testFiles[tagSetKey(tags)] = len(tests)
	}
	return scan, nil
}

// checkRequestWaits returns one message per wait that is neither the
// helpers' own nor allowlisted, and one per stale allowlist entry.
func checkRequestWaits(waits []requestWait, allow map[string]string) []string {
	var out []string
	used := map[string]bool{}
	for _, w := range waits {
		if w.exempt {
			continue
		}
		if _, ok := allow[w.allowKey()]; ok {
			used[w.allowKey()] = true
			continue
		}
		out = append(out, w.message())
	}
	for key := range allow {
		if !used[key] {
			out = append(out, fmt.Sprintf("allowlist entry %s matched nothing: remove it", key))
		}
	}
	slices.Sort(out)
	return out
}

// requestWaitVacuity returns a message for each way the scan could have
// passed without looking: a tag set whose load saw no test file, or no call
// of Done in the package that holds the helpers.
func requestWaitVacuity(scan *requestWaitScan) []string {
	var out []string
	for _, tags := range scan.tagSets {
		if scan.testFiles[tagSetKey(tags)] == 0 {
			out = append(out, fmt.Sprintf("the load for tags %q saw no _test.go file; tests must be loaded", tags))
		}
	}
	exempt := 0
	for _, w := range scan.waits {
		if w.exempt {
			exempt++
		}
	}
	if exempt == 0 {
		out = append(out, "the load saw no call of Done on a request's context in the package that holds the helpers")
	}
	return out
}

// sourceFilesNoLoadSaw returns the Go files of files, outside testdata, that
// no type-checked load saw.
func sourceFilesNoLoadSaw(t *testing.T, files []string, seen map[string]bool) []string {
	t.Helper()
	var out []string
	for _, path := range files {
		if !strings.HasSuffix(path, ".go") || inTestdata(path) {
			continue
		}
		abs, err := filepath.Abs(path)
		require.NoError(t, err)
		if !seen[abs] {
			out = append(out, path)
		}
	}
	return out
}

// ownGoFiles returns the files outside testdata directories: the ones go
// build can see.
func ownGoFiles(files []string) []string {
	var out []string
	for _, f := range files {
		if !inTestdata(f) {
			out = append(out, f)
		}
	}
	return out
}

// requestWaitRepository scans the module once.
var requestWaitRepository = sync.OnceValues(func() (*requestWaitScan, error) {
	files, err := repositoryFiles()
	if err != nil {
		return nil, err
	}
	tagSets, err := requestWaitTagSets(ownGoFiles(files))
	if err != nil {
		return nil, err
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return scanRequestWaits("./...", tagSets, testTransportPath, wd)
})

// TestNoCodeWaitsOnARequestContextOutsideTheHelpers asserts that no Go file
// of the repository calls Done on a context that comes from an *http.Request
// except through internal/testtransport, and that the scan looked: it loaded
// test files under every tag set and saw the helpers' own call.
func TestNoCodeWaitsOnARequestContextOutsideTheHelpers(t *testing.T) {
	t.Parallel()
	scan, err := requestWaitRepository()
	require.NoError(t, err)
	files, err := repositoryFiles()
	require.NoError(t, err)
	for _, msg := range requestWaitVacuity(scan) {
		t.Error(msg)
	}
	loaded := map[string]bool{}
	for _, set := range scan.tagSets {
		for _, tag := range set {
			loaded[tag] = true
		}
	}
	for _, path := range ownGoFiles(files) {
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		for _, tag := range customTags(t, path) {
			assert.True(t, loaded[tag], "build tag %q (used in %s) is in no tag set the scan loaded", tag, path)
		}
	}
	waits := slices.Collect(maps.Values(scan.waits))
	for _, path := range sourceFilesNoLoadSaw(t, files, scan.seen) {
		fset, f := parseSnippet(t, path, readFile(t, path))
		waits = append(waits, scanRequestWaitsByName(fset, f, filepath.ToSlash(path), "internal/testtransport")...)
	}
	for _, msg := range checkRequestWaits(waits, requestWaitAllowlist) {
		t.Error(msg)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

// TestRequestWaitAllowlistRules asserts that every allowlist entry names a
// file and a function and carries a reason.
func TestRequestWaitAllowlistRules(t *testing.T) {
	t.Parallel()
	for key, reason := range requestWaitAllowlist {
		file, fn, ok := strings.Cut(key, ":")
		assert.True(t, ok && strings.HasSuffix(file, ".go") && fn != "", "entry %q is not <file>:<function>", key)
		assert.NotEmpty(t, reason, "entry %q has no reason", key)
		_, err := os.Stat(file)
		assert.NoError(t, err, "entry %q names a file that does not exist", key)
	}
}

const requestWaitFixtureDir = "test/conformance/testdata/requestwait"

// TestRequestWaitScanFixtures asserts the type-resolved scan on type-checked
// fixture packages, loaded the way the repository is: each spelling of a
// wait on a request's context is found, the helpers' package is exempt and
// a package whose path only starts with its path is not, a file behind a
// build tag is loaded, and a value that only looks like a request is not
// found. A rule that stops seeing its form fails its fixture.
func TestRequestWaitScanFixtures(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	var fixtureFiles []string
	for _, f := range files {
		if strings.HasPrefix(f, requestWaitFixtureDir+"/typed/") {
			fixtureFiles = append(fixtureFiles, f)
		}
	}
	tagSets, err := requestWaitTagSets(fixtureFiles)
	require.NoError(t, err)
	require.Len(t, tagSets, 2, "the fixtures name one build tag")
	wd, err := os.Getwd()
	require.NoError(t, err)
	scan, err := scanRequestWaits("./"+requestWaitFixtureDir+"/typed/...", tagSets, modulePrefix+requestWaitFixtureDir+"/typed/testtransport", wd)
	require.NoError(t, err)
	require.Empty(t, requestWaitVacuity(scan))

	found := map[string]int{}
	exempt := map[string]int{}
	for _, w := range scan.waits {
		fixture, _, _ := strings.Cut(strings.TrimPrefix(w.file, requestWaitFixtureDir+"/typed/"), "/")
		if w.exempt {
			exempt[fixture]++
		} else {
			found[fixture]++
		}
	}
	for fixture, want := range map[string]int{
		"direct":         1,
		"stored":         1,
		"donechan":       1,
		"derived":        1,
		"selectcase":     1,
		"field":          2,
		"helperparam":    1,
		"embedded":       1,
		"names":          3,
		"negative":       0,
		"clean":          0,
		"testtransport":  0,
		"testtransportx": 1,
		"tagged":         1,
		"notifycontext":  1,
		"errgroup":       1,
		"derivehelper":   1,
		"reqhelper":      1,
		"afterfunc":      1,
		"methodexpr":     1,
		"ifacecontext":   3,
		"detached":       0,
	} {
		assert.Equal(t, want, found[fixture], "fixture %s", fixture)
	}
	assert.Equal(t, map[string]int{"testtransport": 1}, exempt, "only the helpers' fixture package is exempt")
	for _, w := range scan.waits {
		if strings.Contains(w.file, "/afterfunc/") {
			assert.Equal(t, waitKindAfterFunc, w.kind)
			assert.Contains(t, w.message(), "context.AfterFunc")
		}
	}
}

// TestRequestWaitScanByNameFixtures asserts the syntax-only pass on source
// snippets: the names it takes for a request, a variable and a derived
// context, the exempt directory, and waits it must leave alone.
func TestRequestWaitScanByNameFixtures(t *testing.T) {
	t.Parallel()
	const head = "package p\nimport (\"context\"; \"net/http\")\nvar _ = context.Background\nvar _ http.Handler\n"
	for _, tc := range []struct {
		name   string
		file   string
		body   string
		finds  int
		exempt int
	}{
		{"direct", "internal/x/a_test.go", "func f(r *http.Request) { <-r.Context().Done() }", 1, 0},
		{"in", "internal/x/a_test.go", "func f(in *http.Request) { <-in.Context().Done() }", 1, 0},
		{"stored", "internal/x/a_test.go", "func f(req *http.Request) { ctx := req.Context(); <-ctx.Done() }", 1, 0},
		{"done channel", "internal/x/a_test.go", "func f(request *http.Request) { done := request.Context().Done(); <-done }", 1, 0},
		{"derived", "internal/x/a_test.go", "func f(r *http.Request) { c, cancel := context.WithCancel(r.Context()); defer cancel(); <-c.Done() }", 1, 0},
		{"signal.NotifyContext", "internal/x/a_test.go", "func f(r *http.Request) { c, stop := signal.NotifyContext(r.Context()); defer stop(); <-c.Done() }", 1, 0},
		{"errgroup.WithContext takes the second result", "internal/x/a_test.go", "func f(r *http.Request) { g, c := errgroup.WithContext(r.Context()); _ = g; <-c.Done() }", 1, 0},
		{"errgroup.WithContext first result is no context", "internal/x/a_test.go", "func f(r *http.Request) { g, c := errgroup.WithContext(r.Context()); _ = c; <-g.Done() }", 0, 0},
		{"context.WithoutCancel", "internal/x/a_test.go", "func f(r *http.Request) { <-context.WithoutCancel(r.Context()).Done() }", 0, 0},
		{"a context built on WithoutCancel", "internal/x/a_test.go", "func f(r *http.Request) { c, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 1); defer cancel(); <-c.Done() }", 0, 0},
		{"context.AfterFunc", "internal/x/a_test.go", "func f(r *http.Request) { context.AfterFunc(r.Context(), func() {}) }", 1, 0},
		{"the helpers' directory", "internal/testtransport/hang.go", "func f(r *http.Request) { <-r.Context().Done() }", 0, 1},
		{"a subdirectory of the helpers' directory", "internal/testtransport/x/hang.go", "func f(r *http.Request) { <-r.Context().Done() }", 1, 0},
		{"a sibling of the helpers' directory", "internal/testtransportx/hang.go", "func f(r *http.Request) { <-r.Context().Done() }", 1, 0},
		{"a context that is no request's", "internal/x/a_test.go", "func f(ctx context.Context) { <-ctx.Done() }", 0, 0},
		{"a package-level function", "internal/x/a_test.go", "var g = func(r *http.Request) { <-r.Context().Done() }", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fset, f := parseSnippet(t, tc.file, head+tc.body)
			var finds, exempt int
			for _, w := range scanRequestWaitsByName(fset, f, tc.file, "internal/testtransport") {
				if w.exempt {
					exempt++
				} else {
					finds++
				}
			}
			assert.Equal(t, tc.finds, finds)
			assert.Equal(t, tc.exempt, exempt)
		})
	}
}

// TestRequestWaitAllowlistAndVacuityRules asserts the checks around the scan:
// an allowlisted function is accepted, an entry for a function that no
// longer waits is stale, a wait in a function outside the list is refused
// with the helpers named, and a scan that loaded no test file, or never saw
// the helpers' own call, fails.
func TestRequestWaitAllowlistAndVacuityRules(t *testing.T) {
	t.Parallel()
	wait := requestWait{file: "a_test.go", line: 3, col: 5, fn: "f"}
	assert.Empty(t, checkRequestWaits([]requestWait{wait}, map[string]string{"a_test.go:f": "why"}))
	assert.Equal(t, []string{"allowlist entry a_test.go:gone matched nothing: remove it"},
		checkRequestWaits([]requestWait{wait}, map[string]string{"a_test.go:f": "why", "a_test.go:gone": "why"}))
	got := checkRequestWaits([]requestWait{wait}, nil)
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "a_test.go:3:5: f calls Done on the context of a request; wait with testtransport.HangAndAbort")
	wait.exempt = true
	assert.Empty(t, checkRequestWaits([]requestWait{wait}, nil), "the helpers' own call is no finding")

	scan := &requestWaitScan{
		waits:     map[string]requestWait{"a": wait},
		testFiles: map[string]int{"": 2, "e2e": 1},
		tagSets:   [][]string{nil, {"e2e"}},
	}
	assert.Empty(t, requestWaitVacuity(scan))
	scan.testFiles["e2e"] = 0
	require.Len(t, requestWaitVacuity(scan), 1, "a tag set whose load saw no test file")
	scan.testFiles["e2e"] = 1
	scan.waits["a"] = requestWait{file: "a_test.go", line: 3, col: 5, fn: "f"}
	require.Len(t, requestWaitVacuity(scan), 1, "no call of Done in the helpers' package")
}
