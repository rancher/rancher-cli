package fidelity

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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

// serialScriptTests names the top-level tests that start a shell process, run
// serially on purpose, and the reason each stays serial.
var serialScriptTests = map[string]string{
	"TestEverySlotWaitsForTheFleetAgentAfterAnImport":             "its cases run as parallel subtests inside a serial parent",
	"TestKeepRefusalCommandsRunVerbatim":                          "its two cases run one after another as serial subtests",
	"TestKeepRunRebuildsAMovedK3sImageBeforeCheckingItsContainer": "its cases run as parallel subtests inside a serial parent",
	"TestKeepRunRecreatesOnAChangedPinOrNoState":                  "most of its cases run as parallel subtests inside a serial parent; two run one after another",
	"TestKeepRunReusesAHealthyPairWithoutRemoval":                 "its cases run as parallel subtests inside a serial parent",
	"TestKeepRunStoppedOracleExitsFive":                           "its cases run as parallel subtests inside a serial parent",
	"TestKeepRunStopsOnAFailedDownstreamCheck":                    "its cases run as parallel subtests inside a serial parent",
	"TestKeepRunStopsOnAFailedPairCheck":                          "its cases run as parallel subtests inside a serial parent",
	"TestKeepRunWaitsForAKeptDownstreamToTurnActive":              "its cases run as parallel subtests inside a serial parent",
	"TestNoKeepRunStillRemovesAndBoots":                           "it runs one case with no subtests",
	"TestHostMemoryFree":                                          "asserts how long a memory reading takes, and a busy machine would fail it",
	"TestRedactLog":                                               "asserts how long a filter takes on a long line, and a busy machine would fail it",
}

// serialScan is the result of scanning the test sources of the package.
type serialScan struct {
	// files are the names of the parsed files.
	files []string
	// starts are the file:line places that start a shell process.
	starts []string
	// reached are the top-level tests, fuzz tests and examples from which a
	// start is reachable.
	reached []string
	// serial are the reached functions that do not call t.Parallel in their own body.
	serial []string
	// offenders are the serial functions with no allowlist row.
	offenders []string
	// problems are the stale or empty allowlist rows.
	problems []string
}

// scope is the set of names a function declares itself at one point of its
// body, with the scope around it.
type scope struct {
	names  map[string]bool
	parent *scope
}

func newScope(parent *scope) *scope {
	return &scope{names: map[string]bool{}, parent: parent}
}

func (s *scope) declare(name string) {
	if name != "_" {
		s.names[name] = true
	}
}

func (s *scope) declared(name string) bool {
	for c := s; c != nil; c = c.parent {
		if c.names[name] {
			return true
		}
	}
	return false
}

// refWalker walks a function body or an initializer and reports every name
// reference: a bare identifier that is not declared at that point by the
// function itself, or the selector name of x.m. For an assignment to a name
// the function does not declare, it also calls onAssign with a function that
// reports the references on the right-hand side.
type refWalker struct {
	sc       *scope
	visit    func(name string, selector bool)
	onAssign func(name string, rhs func(visit func(name string, selector bool)))
}

func (w refWalker) child() refWalker {
	w.sc = newScope(w.sc)
	return w
}

func (w refWalker) expr(e ast.Expr) {
	if e != nil {
		ast.Walk(w, e)
	}
}

func (w refWalker) exprs(list []ast.Expr) {
	for _, e := range list {
		w.expr(e)
	}
}

func (w refWalker) stmt(s ast.Stmt) {
	if s != nil {
		ast.Walk(w, s)
	}
}

func (w refWalker) block(b *ast.BlockStmt) {
	if b != nil {
		ast.Walk(w, b)
	}
}

func (w refWalker) declareFields(list *ast.FieldList) {
	if list == nil {
		return
	}
	for _, f := range list.List {
		for _, id := range f.Names {
			w.sc.declare(id.Name)
		}
	}
}

func (w refWalker) declareIdent(e ast.Expr) {
	if id, ok := e.(*ast.Ident); ok {
		w.sc.declare(id.Name)
	} else {
		w.expr(e)
	}
}

// assigned reports an assignment of the values to the target when the target
// starts from a name the function does not declare.
func (w refWalker) assigned(target ast.Expr, values []ast.Expr) {
	if w.onAssign == nil {
		return
	}
	id := rootIdent(target)
	if id == nil || id.Name == "_" || w.sc.declared(id.Name) {
		return
	}
	w.onAssign(id.Name, func(visit func(name string, selector bool)) {
		sub := w
		sub.visit = visit
		sub.onAssign = nil
		sub.exprs(values)
	})
}

// rootIdent returns the identifier an assignment target starts from:
// hook, table["x"], cfg.run and *p start from hook, table, cfg and p.
func rootIdent(e ast.Expr) *ast.Ident {
	for {
		switch x := e.(type) {
		case *ast.Ident:
			return x
		case *ast.IndexExpr:
			e = x.X
		case *ast.SelectorExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		default:
			return nil
		}
	}
}

func (w refWalker) Visit(n ast.Node) ast.Visitor {
	switch n := n.(type) {
	case nil:
		return nil
	case *ast.Ident:
		if !w.sc.declared(n.Name) {
			w.visit(n.Name, false)
		}
		return nil
	case *ast.SelectorExpr:
		w.visit(n.Sel.Name, true)
		w.expr(n.X)
		return nil
	case *ast.Field:
		w.expr(n.Type)
		return nil
	case *ast.CompositeLit:
		w.expr(n.Type)
		for _, elt := range n.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				w.expr(kv.Key)
				w.expr(kv.Value)
				continue
			}
			w.expr(elt)
		}
		return nil
	case *ast.FuncLit:
		c := w.child()
		c.declareFields(n.Type.Params)
		c.declareFields(n.Type.Results)
		c.expr(n.Type)
		c.block(n.Body)
		return nil
	case *ast.BlockStmt:
		c := w.child()
		for _, s := range n.List {
			c.stmt(s)
		}
		return nil
	case *ast.IfStmt:
		c := w.child()
		c.stmt(n.Init)
		c.expr(n.Cond)
		c.block(n.Body)
		c.stmt(n.Else)
		return nil
	case *ast.ForStmt:
		c := w.child()
		c.stmt(n.Init)
		c.expr(n.Cond)
		c.stmt(n.Post)
		c.block(n.Body)
		return nil
	case *ast.RangeStmt:
		w.expr(n.X)
		c := w.child()
		if n.Tok == token.DEFINE {
			c.declareIdent(n.Key)
			c.declareIdent(n.Value)
		} else {
			c.expr(n.Key)
			c.expr(n.Value)
			for _, target := range []ast.Expr{n.Key, n.Value} {
				if target != nil {
					c.assigned(target, []ast.Expr{n.X})
				}
			}
		}
		c.block(n.Body)
		return nil
	case *ast.SwitchStmt:
		c := w.child()
		c.stmt(n.Init)
		c.expr(n.Tag)
		c.block(n.Body)
		return nil
	case *ast.TypeSwitchStmt:
		c := w.child()
		c.stmt(n.Init)
		switch a := n.Assign.(type) {
		case *ast.AssignStmt:
			c.exprs(a.Rhs)
			for _, left := range a.Lhs {
				c.declareIdent(left)
			}
		default:
			c.stmt(n.Assign)
		}
		c.block(n.Body)
		return nil
	case *ast.CaseClause:
		c := w.child()
		c.exprs(n.List)
		for _, s := range n.Body {
			c.stmt(s)
		}
		return nil
	case *ast.CommClause:
		c := w.child()
		c.stmt(n.Comm)
		for _, s := range n.Body {
			c.stmt(s)
		}
		return nil
	case *ast.AssignStmt:
		if n.Tok == token.DEFINE {
			w.exprs(n.Rhs)
			for _, left := range n.Lhs {
				w.declareIdent(left)
			}
			return nil
		}
		w.exprs(n.Lhs)
		w.exprs(n.Rhs)
		for _, left := range n.Lhs {
			w.assigned(left, n.Rhs)
		}
		return nil
	case *ast.DeclStmt:
		decl, ok := n.Decl.(*ast.GenDecl)
		if !ok {
			return nil
		}
		for _, spec := range decl.Specs {
			switch s := spec.(type) {
			case *ast.ValueSpec:
				w.expr(s.Type)
				w.exprs(s.Values)
				for _, id := range s.Names {
					w.sc.declare(id.Name)
				}
			case *ast.TypeSpec:
				w.sc.declare(s.Name.Name)
				w.expr(s.Type)
			}
		}
		return nil
	}
	return w
}

// funcWalker starts a walk of a function: its receiver, parameters and results
// are declared before the body.
func funcWalker(d *ast.FuncDecl, visit func(name string, selector bool), onAssign func(string, func(func(string, bool)))) refWalker {
	w := refWalker{sc: newScope(nil), visit: visit, onAssign: onAssign}
	w.declareFields(d.Recv)
	w.declareFields(d.Type.Params)
	w.declareFields(d.Type.Results)
	return w
}

// isEntryName reports whether a package-level function name is a test, a fuzz
// test or an example. Benchmarks are not: the race pass runs none.
func isEntryName(name string) bool {
	return strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "Fuzz") || strings.HasPrefix(name, "Example")
}

// scanSerialScriptTests asserts nothing itself. It parses the sources (file
// name to Go source) and reports which top-level tests, fuzz tests and
// examples start a shell process, directly or through other functions, and
// which of them run serially. A benchmark is not examined: the race pass runs
// none.
//
// A start is any of these, with each package name taken from the import of its
// file (a dot import of os/exec, os or syscall is an error):
//   - a call to exec.Command or exec.CommandContext whose command argument is
//     not a string literal, or is a literal whose last path element is not
//     git (see startExemptCommand); a shell, env, sudo, python3 and the script
//     run by its path are all starts;
//   - a reference to exec.Command or exec.CommandContext that is not the
//     callee of a call, such as a function value held in a variable;
//   - the type exec.Cmd used as a value: a composite literal, new(exec.Cmd), a
//     var, a field or a parameter of that type (a pointer type is not a
//     start);
//   - a reference to os.StartProcess, syscall.Exec, syscall.ForkExec or
//     syscall.StartProcess.
//
// A package-level type declaration is a node too: the type exec.Cmd used as a
// value in it (an alias, an embedded field, a field) is a start of the type,
// and a function that names the type reaches it.
//
// The scan does not follow three forms, which are accepted limits: a git
// command whose Path or Args are changed after it is created, a write to a
// package var through a pointer to it held in a local, and a function stored
// in a registry value (a method call that keeps it) and called later.
//
// Matching rule:
//  1. Every source is parsed; a source that does not parse is an error.
//  2. A node is a package-level function, a package method or a package var.
//     All methods with one name are one node. The identifiers in a var's
//     initializer are the var's edges, and so are the identifiers on the
//     right-hand side of an assignment to the var (or to an element or field
//     of it, or by a range with =) anywhere in the package, init functions
//     and function literals in initializers included.
//  3. Every reference by name is an edge: a call, a function or method value
//     that is not called, an identifier inside a function literal, and an
//     identifier in a package-level initializer. A bare identifier resolves
//     to package-level functions and vars. A selector x.m resolves to every
//     package method named m, an accepted over-approximation. A composite
//     literal key that names a package function or var is an edge, so a
//     struct field key with the same name is one too (an accepted
//     over-approximation); struct field names in a type are not references.
//  4. A bare identifier is skipped only when the enclosing function has
//     declared the name before that point in the same or an enclosing scope
//     (a parameter, :=, var, range variable or closure parameter). A use
//     before the declaration, or in another scope, is an edge. A local name
//     never hides the selector name of x.m.
//  5. A top-level Test, Fuzz or Example function reaches a start when a path
//     of edges leads from its body to a node that holds one.
//
// A reached function runs in parallel when t.Parallel() is a statement of its
// own body. A call inside a subtest does not count.
func scanSerialScriptTests(sources map[string]string, allowlist map[string]string) (serialScan, error) {
	type node struct {
		starts []string
		edges  map[string]bool
	}
	type funcInfo struct {
		decl *ast.FuncDecl
		imp  fileImports
	}
	type varInit struct {
		expr ast.Expr
		imp  fileImports
	}
	var scan serialScan
	fset := token.NewFileSet()
	funcs := map[string][]funcInfo{}
	methods := map[string][]funcInfo{}
	varNames := map[string]bool{}
	varInits := map[string][]varInit{}
	typeNames := map[string]bool{}
	typeInits := map[string][]varInit{}

	for _, name := range slices.Sorted(maps.Keys(sources)) {
		file, err := parser.ParseFile(fset, name, sources[name], 0)
		if err != nil {
			return scan, fmt.Errorf("parsing %s: %w", name, err)
		}
		scan.files = append(scan.files, name)
		imp, err := importNames(file)
		if err != nil {
			return scan, fmt.Errorf("%s: %w", name, err)
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				if d.Recv != nil {
					methods[d.Name.Name] = append(methods[d.Name.Name], funcInfo{d, imp})
				} else {
					funcs[d.Name.Name] = append(funcs[d.Name.Name], funcInfo{d, imp})
				}
			case *ast.GenDecl:
				if d.Tok == token.TYPE {
					for _, spec := range d.Specs {
						ts := spec.(*ast.TypeSpec)
						typeNames[ts.Name.Name] = true
						typeInits[ts.Name.Name] = append(typeInits[ts.Name.Name], varInit{ts.Type, imp})
					}
				}
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					value := spec.(*ast.ValueSpec)
					for _, id := range value.Names {
						if id.Name != "_" {
							varNames[id.Name] = true
						}
						if value.Type != nil {
							varInits[id.Name] = append(varInits[id.Name], varInit{value.Type, imp})
						}
						for _, v := range value.Values {
							varInits[id.Name] = append(varInits[id.Name], varInit{v, imp})
						}
					}
				}
			}
		}
	}

	nodes := map[string]*node{}
	nodeFor := func(id string) *node {
		n := nodes[id]
		if n == nil {
			n = &node{edges: map[string]bool{}}
			nodes[id] = n
		}
		return n
	}
	edgeTo := func(from string, name string, selector bool) {
		n := nodeFor(from)
		if selector {
			if len(methods[name]) > 0 {
				n.edges["method:"+name] = true
			}
			return
		}
		if len(funcs[name]) > 0 {
			n.edges["func:"+name] = true
		}
		if varNames[name] {
			n.edges["var:"+name] = true
		}
		if typeNames[name] {
			n.edges["type:"+name] = true
		}
	}
	onAssign := func(name string, rhs func(visit func(name string, selector bool))) {
		if !varNames[name] {
			return
		}
		nodeFor("var:" + name)
		rhs(func(ref string, selector bool) { edgeTo("var:"+name, ref, selector) })
	}
	for name, infos := range funcs {
		for _, info := range infos {
			id := "func:" + name
			nodeFor(id).starts = append(nodeFor(id).starts, shellStarts(fset, info.decl, info.imp)...)
			funcWalker(info.decl, func(ref string, selector bool) { edgeTo(id, ref, selector) }, onAssign).block(info.decl.Body)
		}
	}
	for name, infos := range methods {
		for _, info := range infos {
			id := "method:" + name
			nodeFor(id).starts = append(nodeFor(id).starts, shellStarts(fset, info.decl, info.imp)...)
			funcWalker(info.decl, func(ref string, selector bool) { edgeTo(id, ref, selector) }, onAssign).block(info.decl.Body)
		}
	}
	for name, inits := range typeInits {
		for _, init := range inits {
			id := "type:" + name
			nodeFor(id).starts = append(nodeFor(id).starts, shellStarts(fset, init.expr, init.imp)...)
			refWalker{sc: newScope(nil), visit: func(ref string, selector bool) { edgeTo(id, ref, selector) }}.expr(init.expr)
		}
	}
	for name, inits := range varInits {
		for _, init := range inits {
			id := "var:" + name
			nodeFor(id).starts = append(nodeFor(id).starts, shellStarts(fset, init.expr, init.imp)...)
			refWalker{sc: newScope(nil), visit: func(ref string, selector bool) { edgeTo(id, ref, selector) }, onAssign: onAssign}.expr(init.expr)
		}
	}

	for _, n := range nodes {
		scan.starts = append(scan.starts, n.starts...)
	}
	slices.Sort(scan.starts)
	scan.starts = slices.Compact(scan.starts)

	reaches := func(from string) bool {
		seen := map[string]bool{}
		queue := []string{from}
		for len(queue) > 0 {
			id := queue[0]
			queue = queue[1:]
			if seen[id] {
				continue
			}
			seen[id] = true
			n := nodes[id]
			if n == nil {
				continue
			}
			if len(n.starts) > 0 {
				return true
			}
			for next := range n.edges {
				queue = append(queue, next)
			}
		}
		return false
	}

	parallel := map[string]bool{}
	for name, infos := range funcs {
		if !isEntryName(name) {
			continue
		}
		for _, info := range infos {
			isParallel := callsParallel(info.decl)
			parallel[name] = isParallel
			if !reaches("func:" + name) {
				continue
			}
			scan.reached = append(scan.reached, name)
			if !isParallel {
				scan.serial = append(scan.serial, name)
			}
		}
	}
	slices.Sort(scan.reached)
	slices.Sort(scan.serial)

	for _, name := range scan.serial {
		if _, ok := allowlist[name]; !ok {
			scan.offenders = append(scan.offenders, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(allowlist)) {
		switch {
		case strings.TrimSpace(allowlist[name]) == "":
			scan.problems = append(scan.problems, fmt.Sprintf("row %s has an empty reason", name))
		case len(funcs[name]) == 0 || !isEntryName(name):
			scan.problems = append(scan.problems, fmt.Sprintf("row %s names no test", name))
		case parallel[name]:
			scan.problems = append(scan.problems, fmt.Sprintf("row %s names a test that runs in parallel", name))
		case !slices.Contains(scan.reached, name):
			scan.problems = append(scan.problems, fmt.Sprintf("row %s names a test that starts no shell process", name))
		}
	}
	return scan, nil
}

// fileImports holds the names a file imports os/exec, os and syscall under;
// an empty name means the file does not import the package.
type fileImports struct {
	exec, os, syscall string
}

// importNames reads the import names of a file. A dot import of one of the
// three packages is an error: the scan could not find the starts.
func importNames(file *ast.File) (fileImports, error) {
	var names fileImports
	for _, imp := range file.Imports {
		var target *string
		switch imp.Path.Value {
		case `"os/exec"`:
			target = &names.exec
		case `"os"`:
			target = &names.os
		case `"syscall"`:
			target = &names.syscall
		default:
			continue
		}
		*target = path.Base(strings.Trim(imp.Path.Value, `"`))
		if imp.Name != nil {
			*target = imp.Name.Name
		}
		if *target == "." {
			return names, fmt.Errorf("imports %s with a dot; the scan cannot find its starts", imp.Path.Value)
		}
	}
	return names, nil
}

// isPackageSelector reports whether the expression is pkg.name for one of the
// names, with pkg the name the package is imported under (empty: not imported).
func isPackageSelector(e ast.Expr, pkg string, names ...string) (*ast.SelectorExpr, bool) {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || pkg == "" {
		return nil, false
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok || x.Name != pkg || !slices.Contains(names, sel.Sel.Name) {
		return nil, false
	}
	return sel, true
}

// shellStarts lists the file:line of every start under root.
func shellStarts(fset *token.FileSet, root ast.Node, imp fileImports) []string {
	var starts []string
	callees := map[*ast.SelectorExpr]bool{}
	pointers := map[*ast.SelectorExpr]bool{}
	add := func(pos token.Pos) {
		p := fset.Position(pos)
		starts = append(starts, fmt.Sprintf("%s:%d", path.Base(p.Filename), p.Line))
	}
	ast.Inspect(root, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			sel, ok := isPackageSelector(n.Fun, imp.exec, "Command", "CommandContext")
			if !ok {
				return true
			}
			callees[sel] = true
			index := 0
			if sel.Sel.Name == "CommandContext" {
				index = 1
			}
			if index >= len(n.Args) || commandStartsAProcess(n.Args[index]) {
				add(n.Pos())
			}
		case *ast.StarExpr:
			if sel, ok := isPackageSelector(n.X, imp.exec, "Cmd"); ok {
				pointers[sel] = true
			}
		case *ast.SelectorExpr:
			if _, ok := isPackageSelector(n, imp.exec, "Command", "CommandContext"); ok && !callees[n] {
				add(n.Pos())
			}
			if _, ok := isPackageSelector(n, imp.exec, "Cmd"); ok && !pointers[n] {
				add(n.Pos())
			}
			if _, ok := isPackageSelector(n, imp.os, "StartProcess"); ok {
				add(n.Pos())
			}
			if _, ok := isPackageSelector(n, imp.syscall, "Exec", "ForkExec", "StartProcess"); ok {
				add(n.Pos())
			}
		}
		return true
	})
	return starts
}

// startExemptCommand is the one command a literal may name without being a
// start: the tests run git to build a fixture repository, and git starts no
// shell and no script of this repository. Any other literal (a shell, env, a
// launcher such as sudo, an interpreter, the script itself) is a start.
const startExemptCommand = "git"

// commandStartsAProcess reports whether the command argument of an exec call
// is a start: anything that is not a string literal, or a literal whose last
// path element (a backslash separates elements too) is not startExemptCommand,
// with an .exe suffix ignored.
func commandStartsAProcess(arg ast.Expr) bool {
	lit, ok := arg.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return true
	}
	command, err := strconv.Unquote(lit.Value)
	if err != nil {
		return true
	}
	return strings.TrimSuffix(path.Base(strings.ReplaceAll(command, `\`, "/")), ".exe") != startExemptCommand
}

// callsParallel reports whether t.Parallel() is a statement of the function's
// own body.
func callsParallel(d *ast.FuncDecl) bool {
	if d.Type.Params == nil || len(d.Type.Params.List) == 0 || len(d.Type.Params.List[0].Names) == 0 {
		return false
	}
	param := d.Type.Params.List[0].Names[0].Name
	for _, stmt := range d.Body.List {
		expr, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := expr.X.(*ast.CallExpr)
		if !ok {
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Parallel" {
			continue
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == param {
			return true
		}
	}
	return false
}

// packageTestSources reads every *_test.go file of the package directory.
func packageTestSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	sources := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		raw, err := os.ReadFile(e.Name())
		require.NoError(t, err)
		sources[e.Name()] = string(raw)
	}
	return sources
}

// TestEveryTestThatStartsAShellRunsInParallel asserts that every top-level
// test, fuzz test and example from which a shell process start is reachable
// calls t.Parallel() in its own body, or has a row in serialScriptTests with
// the reason it stays serial. The matching rule is in the comment of
// scanSerialScriptTests. A new test that runs the script: call t.Parallel() as
// its first statement, or add a row with the reason. A row for a test that is
// parallel, does not exist or starts no shell process fails the test, so the
// allowlist cannot go stale.
func TestEveryTestThatStartsAShellRunsInParallel(t *testing.T) {
	t.Parallel()
	scan, err := scanSerialScriptTests(packageTestSources(t), serialScriptTests)
	require.NoError(t, err)
	assert.Empty(t, scan.problems)
	assert.Empty(t, scan.offenders, "these tests start a shell process and run serially: call t.Parallel() or add a row with the reason")
}

// TestParallelGuardScanSeesTheSource asserts the scan reads every test file of
// the package and finds the shell starts and the tests that reach them.
func TestParallelGuardScanSeesTheSource(t *testing.T) {
	t.Parallel()
	sources := packageTestSources(t)
	scan, err := scanSerialScriptTests(sources, serialScriptTests)
	require.NoError(t, err)

	assert.Equal(t, slices.Sorted(maps.Keys(sources)), scan.files, "every test file of the package must be parsed")

	t.Logf("%d shell starts: %s", len(scan.starts), strings.Join(scan.starts, " "))
	t.Logf("%d tests reach a start, %d of them serial", len(scan.reached), len(scan.serial))
	t.Logf("reached: %s", strings.Join(scan.reached, " "))
	assert.NotEmpty(t, scan.starts)

	for _, canary := range []string{
		"TestHelmCheckBoot",
		"TestWriteStateRecordsTheBoot",
		"TestOracleMinorLoginRetries",
		"TestKeptDownstreamWait",
		"TestDriftDownstreamScriptRefusesTheDriftVariables",
		"TestKeepRefusalCommandsRunVerbatim",
	} {
		assert.Contains(t, scan.reached, canary, "the scan must reach this test")
	}
}

// TestSerialScriptMatcher asserts the matching rule on small sources.
func TestSerialScriptMatcher(t *testing.T) {
	t.Parallel()
	const header = "package p\nimport (\"os/exec\"; \"testing\")\n"
	const start = "func start() { _ = exec.Command(\"bash\", \"-c\", \"true\") }\n"
	type tc struct {
		name      string
		sources   map[string]string
		allowlist map[string]string
		offenders []string
		problems  []string
		reached   []string
		failure   string
	}
	cases := []tc{
		{
			name: "a parallel test that starts a shell",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { t.Parallel(); start() }\n"},
			reached: []string{"TestA"},
		},
		{
			name: "a serial test that reaches a start through one helper",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { start() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a serial test that reaches a start through two helpers",
			sources: map[string]string{"a_test.go": header + start +
				"func middle() { start() }\nfunc TestA(t *testing.T) { middle() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a serial test that reaches no start",
			sources: map[string]string{"a_test.go": header + start +
				"func other() {}\nfunc TestA(t *testing.T) { other() }\n"},
		},
		{
			name: "a start in another file",
			sources: map[string]string{
				"a_test.go": header + "func TestA(t *testing.T) { start() }\n",
				"b_test.go": header + start,
			},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a local variable that has the name of a helper",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { start := 1; _ = start }\n"},
		},
		{
			name: "a closure parameter that has the name of a helper",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { f := func(start int) int { return start }; _ = f(1) }\n"},
		},
		{
			name: "a function parameter that has the name of a helper",
			sources: map[string]string{"a_test.go": header + start +
				"func use(start int) int { return start }\nfunc TestA(t *testing.T) { _ = use(1) }\n"},
		},
		{
			name: "a named result that has the name of a helper",
			sources: map[string]string{"a_test.go": header + start +
				"func use() (start int) { return start }\nfunc TestA(t *testing.T) { _ = use() }\n"},
		},
		{
			name: "a receiver that has the name of a helper",
			sources: map[string]string{"a_test.go": header + start +
				"type h struct{}\nfunc (start h) use() int { _ = start; return 1 }\nfunc TestA(t *testing.T) { _ = h{}.use() }\n"},
		},
		{
			name: "a closure result that has the name of a helper",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { f := func() (start int) { return start }; _ = f() }\n"},
		},
		{
			name: "a type switch variable that has the name of a helper",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { switch start := any(1).(type) { default: _ = start } }\n"},
		},
		{
			name: "a select clause variable that has the name of a helper",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { ch := make(chan int); select { case start := <-ch: _ = start; default: } }\n"},
		},
		{
			name: "a local var that has the name of a helper",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { var start = 1; _ = start }\n"},
		},
		{
			name: "a local type that has the name of a helper",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { type start int; _ = start(1) }\n"},
		},
		{
			name: "the right-hand side of := is read before the name is declared",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { start := start; _ = start }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "the initializer of a local var is read before the name is declared",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { var start = start; _ = start }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a bare block does not leak its local",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { { start := 1; _ = start }; start() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "an if does not leak its local",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { if start := 1; start > 0 { _ = start }; start() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a for does not leak its local",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { for start := 0; start < 1; start++ { _ = start }; start() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a range does not leak its variable",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { for _, start := range []int{1} { _ = start }; start() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a switch does not leak its local",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { switch start := 1; start { case 1: }; start() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a type switch does not leak its variable",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { switch start := any(1).(type) { default: _ = start }; start() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a case clause does not leak its local to the next clause",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { switch { case true: start := 1; _ = start; case false: start() } }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a select clause does not leak its variable to the next clause",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { ch := make(chan int); select { case start := <-ch: _ = start; default: start() } }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a closure does not leak its local",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { f := func() { start := 1; _ = start }; f(); start() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a closure parameter does not leak",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { f := func(start int) { _ = start }; f(1); start() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a closure result does not leak",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { f := func() (start int) { return 1 }; _ = f(); start() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "the else branch is read",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { if t == nil { } else { start() } }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "the right-hand side of a type switch is read",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { switch x := any(start).(type) { default: _ = x } }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a field of a package var assigned",
			sources: map[string]string{"a_test.go": header + start +
				"var cfg struct{ run func() }\nfunc init() { cfg.run = start }\nfunc TestA(t *testing.T) { cfg.run() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a package var assigned through a pointer",
			sources: map[string]string{"a_test.go": header + start +
				"var hookp = new(func())\nfunc init() { *hookp = start }\nfunc TestA(t *testing.T) { (*hookp)() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a package var assigned in parentheses",
			sources: map[string]string{"a_test.go": header + start +
				"var hook func()\nfunc init() { (hook) = start }\nfunc TestA(t *testing.T) { hook() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a package var assigned by a range",
			sources: map[string]string{"a_test.go": header + start +
				"var hook func()\nfunc init() { for _, hook = range []func(){start} { } }\nfunc TestA(t *testing.T) { hook() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a package var assigned inside a closure of an initializer",
			sources: map[string]string{"a_test.go": header + start +
				"var hook func()\nvar _ = func() int { hook = start; return 0 }()\nfunc TestA(t *testing.T) { hook() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a composite literal key that names a package var",
			sources: map[string]string{"a_test.go": header +
				"var name = func() string { _ = exec.Command(\"bash\"); return \"\" }()\n" +
				"func TestA(t *testing.T) { _ = map[string]int{name: 1} }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a Windows path of git is not a start",
			sources: map[string]string{"a_test.go": header +
				"func TestA(t *testing.T) { _ = exec.Command(\"C:\\\\Git\\\\git\\\\git.exe\", \"init\") }\n"},
		},
		{
			name: "a Windows path of a shell is a start",
			sources: map[string]string{"a_test.go": header +
				"func TestA(t *testing.T) { _ = exec.Command(\"C:\\\\Git\\\\bin\\\\bash.exe\") }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "zsh is a start",
			sources: map[string]string{"a_test.go": header +
				"func TestA(t *testing.T) { _ = exec.Command(\"zsh\", \"-c\", \"true\") }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "python3 is a start",
			sources: map[string]string{"a_test.go": header +
				"func TestA(t *testing.T) { _ = exec.Command(\"python3\", \"x.py\") }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a launcher is a start",
			sources: map[string]string{"a_test.go": header +
				"func TestA(t *testing.T) { _ = exec.Command(\"sudo\", \"git\") }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "the script run by its path is a start",
			sources: map[string]string{"a_test.go": header +
				"func TestA(t *testing.T) { _ = exec.Command(\"./scripts/e2e-fidelity\") }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "new of exec.Cmd is a start",
			sources: map[string]string{"a_test.go": header +
				"func TestA(t *testing.T) { _ = new(exec.Cmd) }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a var of type exec.Cmd is a start",
			sources: map[string]string{"a_test.go": header +
				"func TestA(t *testing.T) { var c exec.Cmd; c.Path = \"x\" }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a field of type exec.Cmd is a start",
			sources: map[string]string{"a_test.go": header +
				"func TestA(t *testing.T) { type local struct{ cmd exec.Cmd }; _ = local{} }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a package var of type exec.Cmd is a start",
			sources: map[string]string{"a_test.go": header +
				"var shared exec.Cmd\nfunc TestA(t *testing.T) { _ = shared }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "os.StartProcess is a start",
			sources: map[string]string{"a_test.go": "package p\nimport (\"os\"; \"testing\")\n" +
				"func TestA(t *testing.T) { _, _ = os.StartProcess(\"x\", nil, nil) }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "syscall.Exec is a start",
			sources: map[string]string{"a_test.go": "package p\nimport (\"syscall\"; \"testing\")\n" +
				"func TestA(t *testing.T) { _ = syscall.Exec(\"x\", nil, nil) }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "syscall.ForkExec under another name is a start",
			sources: map[string]string{"a_test.go": "package p\nimport (sc \"syscall\"; \"testing\")\n" +
				"func TestA(t *testing.T) { _, _ = sc.ForkExec(\"x\", nil, nil) }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "os without StartProcess is not a start",
			sources: map[string]string{"a_test.go": "package p\nimport (\"os\"; \"testing\")\n" +
				"func TestA(t *testing.T) { _, _ = os.ReadFile(\"x\") }\n"},
		},
		{
			name: "a range variable that has the name of a helper",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { for _, start := range []int{1} { _ = start } }\n"},
		},
		{
			name: "a local declared inside a closure only",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { f := func() { start := 1; _ = start }; f() }\n"},
		},
		{
			name: "a local declared in another scope",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { if t != nil { start := 1; _ = start }; start() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a use before the local declaration",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { start(); start := 1; _ = start }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a use after the local declaration in a nested block",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { start := 1; { _ = start } }\n"},
		},
		{
			name: "a method value that is not called",
			sources: map[string]string{"a_test.go": header +
				"type h struct{}\nfunc (h) run() { _ = exec.Command(\"bash\") }\n" +
				"func TestA(t *testing.T) { f := h{}.run; _ = f }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a selector name that a local variable shares",
			sources: map[string]string{"a_test.go": header +
				"type h struct{}\nfunc (h) run() { _ = exec.Command(\"bash\") }\n" +
				"func TestA(t *testing.T) { run := 1; _ = run; h{}.run() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a helper reached through a package-level initializer",
			sources: map[string]string{"a_test.go": header + start +
				"var table = []func(){start}\nfunc TestA(t *testing.T) { table[0]() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a helper reached through a function literal in an initializer",
			sources: map[string]string{"a_test.go": header + start +
				"var table = map[string]func(){\"x\": func() { start() }}\nfunc TestA(t *testing.T) { table[\"x\"]() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a package var assigned in init",
			sources: map[string]string{"a_test.go": header + start +
				"var hook func()\nfunc init() { hook = start }\nfunc TestA(t *testing.T) { hook() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a package var assigned in a helper",
			sources: map[string]string{"a_test.go": header + start +
				"var hook func()\nfunc setup() { hook = start }\nfunc TestA(t *testing.T) { hook() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "an element of a package var assigned",
			sources: map[string]string{"a_test.go": header + start +
				"var table = map[string]func(){}\nfunc init() { table[\"x\"] = start }\nfunc TestA(t *testing.T) { table[\"x\"]() }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a local that has the name of a package var and is assigned",
			sources: map[string]string{"a_test.go": header + start +
				"var hook func()\nfunc init() { hook := func() {}; hook = start; _ = hook }\nfunc TestA(t *testing.T) { hook() }\n"},
		},
		{
			name: "a struct field key that has the name of a helper is an edge",
			sources: map[string]string{"a_test.go": header + start +
				"type row struct{ start int }\nfunc TestA(t *testing.T) { _ = row{start: 1} }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "t.Parallel only inside a subtest",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { t.Run(\"x\", func(t *testing.T) { t.Parallel(); start() }) }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a fuzz test that starts a shell",
			sources: map[string]string{"a_test.go": header + start +
				"func FuzzA(f *testing.F) { start() }\n"},
			offenders: []string{"FuzzA"},
			reached:   []string{"FuzzA"},
		},
		{
			name: "an example that starts a shell",
			sources: map[string]string{"a_test.go": header + start +
				"func ExampleA() { start() }\n"},
			offenders: []string{"ExampleA"},
			reached:   []string{"ExampleA"},
		},
		{
			name: "a fuzz test with a row",
			sources: map[string]string{"a_test.go": header + start +
				"func FuzzA(f *testing.F) { start() }\n"},
			allowlist: map[string]string{"FuzzA": "it has no parallel form"},
			reached:   []string{"FuzzA"},
		},
		{
			name: "a benchmark is not examined",
			sources: map[string]string{"a_test.go": header + start +
				"func BenchmarkA(b *testing.B) { start() }\n"},
		},
		{
			name: "an allowlisted serial test",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { start() }\n"},
			allowlist: map[string]string{"TestA": "shares a file"},
			reached:   []string{"TestA"},
		},
		{
			name: "a stale row that names no test",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { t.Parallel(); start() }\n"},
			allowlist: map[string]string{"TestGone": "shares a file"},
			problems:  []string{"row TestGone names no test"},
			reached:   []string{"TestA"},
		},
		{
			name: "a stale row that names a parallel test",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { t.Parallel(); start() }\n"},
			allowlist: map[string]string{"TestA": "shares a file"},
			problems:  []string{"row TestA names a test that runs in parallel"},
			reached:   []string{"TestA"},
		},
		{
			name: "a stale row that names a test that starts nothing",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) {}\n"},
			allowlist: map[string]string{"TestA": "shares a file"},
			problems:  []string{"row TestA names a test that starts no shell process"},
		},
		{
			name: "a row with an empty reason",
			sources: map[string]string{"a_test.go": header + start +
				"func TestA(t *testing.T) { start() }\n"},
			allowlist: map[string]string{"TestA": " "},
			problems:  []string{"row TestA has an empty reason"},
			reached:   []string{"TestA"},
		},
		{
			name: "a command that is not a string literal",
			sources: map[string]string{"a_test.go": header +
				"func TestA(t *testing.T) { name := \"x\"; _ = exec.Command(name) }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "git is not a start",
			sources: map[string]string{"a_test.go": header +
				"func TestA(t *testing.T) { _ = exec.CommandContext(t.Context(), \"git\", \"init\") }\n"},
		},
		{
			name: "the path of git is not a start",
			sources: map[string]string{"a_test.go": header +
				"func TestA(t *testing.T) { _ = exec.Command(\"/usr/bin/git\", \"init\") }\n"},
		},
		{
			name: "sh is a start of CommandContext",
			sources: map[string]string{"a_test.go": header +
				"func TestA(t *testing.T) { _ = exec.CommandContext(t.Context(), \"sh\", \"-c\", \"true\") }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "the os-exec package imported under another name",
			sources: map[string]string{"a_test.go": "package p\nimport (osexec \"os/exec\"; \"testing\")\n" +
				"func TestA(t *testing.T) { _ = osexec.Command(\"bash\") }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a name that is not the os-exec import is not a start",
			sources: map[string]string{"a_test.go": "package p\nimport (osexec \"os/exec\"; \"testing\")\n" +
				"func TestA(t *testing.T) { _ = exec.Command(\"bash\"); _ = osexec.LookPath }\n"},
		},
		{
			name: "the os-exec package imported with a dot",
			sources: map[string]string{"a_test.go": "package p\nimport (. \"os/exec\"; \"testing\")\n" +
				"func TestA(t *testing.T) { _ = Command(\"bash\") }\n"},
			failure: "with a dot; the scan cannot find its starts",
		},
		{
			name: "a command given as a path",
			sources: map[string]string{"a_test.go": header +
				"func TestA(t *testing.T) { _ = exec.Command(\"/bin/bash\", \"-c\", \"true\") }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "env as the launcher",
			sources: map[string]string{"a_test.go": header +
				"func TestA(t *testing.T) { _ = exec.Command(\"env\", \"x\") }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "exec.Command held in a variable",
			sources: map[string]string{"a_test.go": header +
				"func TestA(t *testing.T) { f := exec.Command; _ = f(\"git\") }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "exec.CommandContext passed as a value",
			sources: map[string]string{"a_test.go": header +
				"func use(f any) {}\nfunc TestA(t *testing.T) { use(exec.CommandContext) }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "an exec.Cmd literal",
			sources: map[string]string{"a_test.go": header +
				"func TestA(t *testing.T) { _ = &exec.Cmd{Path: \"/bin/bash\"} }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a pointer to exec.Cmd in a signature is not a start",
			sources: map[string]string{"a_test.go": header +
				"func take(c *exec.Cmd) {}\nfunc TestA(t *testing.T) { take(nil) }\n"},
		},
		{
			name: "a package var named with the blank identifier is no assignment target",
			sources: map[string]string{"a_test.go": header + start +
				"var _ = 1\nfunc other() { _ = start }\nfunc TestA(t *testing.T) { _ = 1 }\n"},
		},
		{
			name: "an assignment to the blank identifier is no edge from the blank var",
			sources: map[string]string{"a_test.go": header + start +
				"var _ = 1\nfunc other() { _ = start }\nfunc TestA(t *testing.T) { _, _ = 1, 2 }\n"},
		},
		{
			name: "a type alias of exec.Cmd is a start",
			sources: map[string]string{"a_test.go": header +
				"type cmd = exec.Cmd\nfunc TestA(t *testing.T) { c := cmd{Path: \"/bin/bash\"}; _ = c }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "an embedded exec.Cmd in a package type is a start",
			sources: map[string]string{"a_test.go": header +
				"type runner struct{ exec.Cmd }\nfunc TestA(t *testing.T) { _ = runner{} }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "a package type that holds no exec.Cmd is no start",
			sources: map[string]string{"a_test.go": header +
				"type runner struct{ cmd *exec.Cmd }\nfunc TestA(t *testing.T) { _ = runner{} }\n"},
		},
		{
			name: "syscall.StartProcess is a start",
			sources: map[string]string{"a_test.go": "package p\nimport (\"syscall\"; \"testing\")\n" +
				"func TestA(t *testing.T) { _, _, _ = syscall.StartProcess(\"x\", nil, nil) }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "os imported under another name",
			sources: map[string]string{"a_test.go": "package p\nimport (o \"os\"; \"testing\")\n" +
				"func TestA(t *testing.T) { _, _ = o.StartProcess(\"x\", nil, nil) }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "each file resolves its own import names",
			sources: map[string]string{
				"a_test.go": "package p\nimport (x \"os/exec\"; \"testing\")\nfunc TestA(t *testing.T) { _ = x.Command(\"bash\") }\n",
				"b_test.go": "package p\nimport (\"os/exec\"; \"testing\")\nfunc TestB(t *testing.T) { _ = exec.Command(\"bash\") }\n" +
					"func TestC(t *testing.T) { _ = x.Command(\"bash\") }\n",
			},
			offenders: []string{"TestA", "TestB"},
			reached:   []string{"TestA", "TestB"},
		},
		{
			name: "exec.Cmd as a by-value parameter of a function",
			sources: map[string]string{"a_test.go": header +
				"func take(c exec.Cmd) {}\nfunc TestA(t *testing.T) { take(nil) }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name: "exec.Cmd as a by-value parameter of a method",
			sources: map[string]string{"a_test.go": header +
				"type h struct{}\nfunc (h) take(c exec.Cmd) {}\nfunc TestA(t *testing.T) { h{}.take(nil) }\n"},
			offenders: []string{"TestA"},
			reached:   []string{"TestA"},
		},
		{
			name:    "a file that does not parse",
			sources: map[string]string{"a_test.go": "package p\nfunc (\n"},
			failure: "parsing a_test.go",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			scan, err := scanSerialScriptTests(c.sources, c.allowlist)
			if c.failure != "" {
				require.ErrorContains(t, err, c.failure)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.offenders, scan.offenders)
			assert.Equal(t, c.problems, scan.problems)
			assert.Equal(t, c.reached, scan.reached)
		})
	}
}
