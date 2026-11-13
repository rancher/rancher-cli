package conformance

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"

	"golang.org/x/tools/go/packages"
)

// The exact-value scans read the production tree with full type information
// and check how printed commands are built: guard 3a (no change to a value
// before the command builder), guard 3b (whole-text filters) and the rules of
// guard 4 (every command goes through the builder). Each scan runs once per
// load, inside a sync.OnceValues, and the tests only read its results.

const (
	evClierr       = "internal/clierr"
	evKinds        = "internal/engine/kinds"
	evFixturesDir  = "test/conformance/testdata/exactvalue"
	evBuilderFile  = "internal/clierr/command.go"
	evTypedLineUse = "internal/cli/linerefusal.go"
)

// evFile is one Go file of a load with its parent links.
type evFile struct {
	pkg     *packages.Package
	info    *types.Info
	file    *ast.File
	path    string
	parents map[ast.Node]ast.Node
}

// evDecl is a function or method with a body.
type evDecl struct {
	f    *evFile
	decl *ast.FuncDecl
	fn   *types.Func
}

// evCallSite is one direct call of a function.
type evCallSite struct {
	f    *evFile
	call *ast.CallExpr
}

// evVarDecl is a package-level variable with its initializer.
type evVarDecl struct {
	f     *evFile
	value ast.Expr
}

// evProgram indexes one load: its files, the functions with a body, every
// direct call site of each function, the functions used as values, the
// interface method calls by method name, the package-level variables, and
// the variables built on clierr.Rancher.
type evProgram struct {
	files      []*evFile
	funcs      map[string]evDecl
	vars       map[string]evVarDecl
	calls      map[string][]evCallSite
	valueUses  map[string]int
	ifaceCalls map[string][]*types.Interface
	members    map[string]bool
	assigns    map[ast.Node]map[types.Object][]ast.Expr
}

// evFinding is one report of a scan. key is what an allowlist row matches
// besides the rule and the function: the callee for guard 3a.
type evFinding struct {
	rule, at, fn, key, msg string
}

func (f evFinding) String() string { return f.at + " (" + f.fn + "): " + f.msg }

// evFilterRef is one reference to a whole-text filter (guard 3b) with what
// the walk of its argument reached.
type evFilterRef struct {
	at, fn, filter string
	call           bool
	relayed        []string
}

// evResults is what the scans found in one or more loads. used names the
// allowlist rows the walk of guard 3a consulted.
type evResults struct {
	findings []evFinding
	refs     []evFilterRef
	used     map[string]bool
	files    map[string]bool
}

func (r *evResults) add(f evFinding) {
	if !slices.Contains(r.findings, f) {
		r.findings = append(r.findings, f)
	}
}

func (r *evResults) merge(o *evResults) {
	for _, f := range o.findings {
		r.add(f)
	}
	for _, ref := range o.refs {
		if !slices.ContainsFunc(r.refs, func(x evFilterRef) bool { return x.at == ref.at && x.filter == ref.filter }) {
			r.refs = append(r.refs, ref)
		}
	}
	for k := range o.used {
		r.used[k] = true
	}
	for k := range o.files {
		r.files[k] = true
	}
}

// byRule is the findings of one rule.
func (r *evResults) byRule(rule string) []evFinding {
	var out []evFinding
	for _, f := range r.findings {
		if f.rule == rule {
			out = append(out, f)
		}
	}
	return out
}

// evAllow is the allowlist rows a scan consults while it runs.
type evAllow struct {
	originCallees map[string]string
	originThrough map[string]string
	originParams  map[string]string
}

// evProductionFile reports whether a file of the module is production code:
// no test file, nothing under test/ or a testdata directory.
func evProductionFile(path string) bool {
	return strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") && !inTestdata(path) && !strings.HasPrefix(path, "test/")
}

func evParents(file *ast.File) map[ast.Node]ast.Node {
	parents := map[ast.Node]ast.Node{}
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if len(stack) > 0 {
			parents[n] = stack[len(stack)-1]
		}
		stack = append(stack, n)
		return true
	})
	return parents
}

// newEVProgram indexes pkgs. keep selects the files by their slash path
// relative to the module root.
func newEVProgram(pkgs []*packages.Package, keep func(string) bool) (*evProgram, error) {
	root, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	members, _ := varsBuiltOnRancher(pkgs)
	p := &evProgram{
		funcs:      map[string]evDecl{},
		vars:       map[string]evVarDecl{},
		calls:      map[string][]evCallSite{},
		valueUses:  map[string]int{},
		ifaceCalls: map[string][]*types.Interface{},
		members:    members,
		assigns:    map[ast.Node]map[types.Object][]ast.Expr{},
	}
	for _, pkg := range pkgs {
		for _, file := range pkg.Syntax {
			path := catalogRelPath(root, pkg.Fset.Position(file.Pos()).Filename)
			if !keep(path) {
				continue
			}
			f := &evFile{pkg: pkg, info: pkg.TypesInfo, file: file, path: path, parents: evParents(file)}
			p.files = append(p.files, f)
			p.index(f)
		}
	}
	return p, nil
}

func (p *evProgram) index(f *evFile) {
	for _, d := range f.file.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if fn, ok := f.info.Defs[d.Name].(*types.Func); ok && d.Body != nil {
				p.funcs[funcKey(fn)] = evDecl{f, d, fn}
			}
		case *ast.GenDecl:
			for _, s := range d.Specs {
				vs, ok := s.(*ast.ValueSpec)
				if !ok || len(vs.Values) != len(vs.Names) {
					continue
				}
				for i, name := range vs.Names {
					if obj := f.info.Defs[name]; obj != nil {
						p.vars[objectKey(obj)] = evVarDecl{f, vs.Values[i]}
					}
				}
			}
		}
	}
	ast.Inspect(f.file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if key, _ := evCallee(f.info, x); key != "" {
				p.calls[key] = append(p.calls[key], evCallSite{f, x})
			}
			if sel, ok := ast.Unparen(x.Fun).(*ast.SelectorExpr); ok {
				if s, ok := f.info.Selections[sel]; ok && s.Kind() == types.MethodVal {
					if iface, ok := s.Recv().Underlying().(*types.Interface); ok {
						p.ifaceCalls[sel.Sel.Name] = append(p.ifaceCalls[sel.Sel.Name], iface)
					}
				}
			}
		case *ast.Ident:
			fn, ok := f.info.Uses[x].(*types.Func)
			if !ok || fn.Pkg() == nil || evCalledName(f, x) {
				return true
			}
			p.valueUses[funcKey(fn)]++
		}
		return true
	})
}

// evCalledName reports whether id names the function of a call.
func evCalledName(f *evFile, id *ast.Ident) bool {
	var cur ast.Node = id
	for {
		parent := f.parents[cur]
		switch x := parent.(type) {
		case *ast.SelectorExpr:
			if x.Sel != id {
				return false
			}
		case *ast.ParenExpr, *ast.IndexExpr, *ast.IndexListExpr:
		case *ast.CallExpr:
			return ast.Unparen(x.Fun) == cur || x.Fun == cur
		default:
			return false
		}
		cur = parent
	}
}

// evCallee is the function a call names statically, by funcKey, and its
// object; "" for a conversion, a builtin or a call of a function value.
func evCallee(info *types.Info, call *ast.CallExpr) (string, *types.Func) {
	fun := ast.Unparen(call.Fun)
	switch f := fun.(type) {
	case *ast.IndexExpr:
		fun = ast.Unparen(f.X)
	case *ast.IndexListExpr:
		fun = ast.Unparen(f.X)
	}
	var id *ast.Ident
	switch f := fun.(type) {
	case *ast.Ident:
		id = f
	case *ast.SelectorExpr:
		id = f.Sel
	}
	if id == nil {
		return "", nil
	}
	fn, ok := info.Uses[id].(*types.Func)
	if !ok || fn.Pkg() == nil {
		return "", nil
	}
	return funcKey(fn), fn
}

// evBuiltin is the name of the builtin a call names, or "".
func evBuiltin(info *types.Info, call *ast.CallExpr) string {
	id, ok := ast.Unparen(call.Fun).(*ast.Ident)
	if !ok {
		return ""
	}
	if b, ok := info.Uses[id].(*types.Builtin); ok {
		return b.Name()
	}
	return ""
}

func evIsModule(fn *types.Func) bool {
	return fn != nil && fn.Pkg() != nil && strings.HasPrefix(fn.Pkg().Path(), modulePrefix)
}

func (f *evFile) at(n ast.Node) string {
	return fmt.Sprintf("%s:%d", f.path, f.pkg.Fset.Position(n.Pos()).Line)
}

func (f *evFile) inPackage(rel string) bool {
	return f.pkg.PkgPath == modulePrefix+rel
}

// enclosing is the declaration that holds n and its key: a function or
// method by funcKey, a package-level variable by objectKey, or the package.
func (f *evFile) enclosing(n ast.Node) (ast.Node, string) {
	for c := n; c != nil; c = f.parents[c] {
		switch d := c.(type) {
		case *ast.FuncDecl:
			if fn, ok := f.info.Defs[d.Name].(*types.Func); ok {
				return d, funcKey(fn)
			}
			return d, d.Name.Name
		case *ast.ValueSpec:
			if len(d.Names) > 0 {
				if obj := f.info.Defs[d.Names[0]]; obj != nil && obj.Pkg() != nil && obj.Parent() == obj.Pkg().Scope() {
					return d, objectKey(obj)
				}
			}
		}
	}
	return f.file, strings.TrimPrefix(f.pkg.PkgPath, modulePrefix)
}

func (f *evFile) finding(r *evResults, rule string, n ast.Node, key, msg string) {
	_, fn := f.enclosing(n)
	r.add(evFinding{rule: rule, at: f.at(n), fn: fn, key: key, msg: msg})
}

// localsOf is the expressions assigned to each local variable of root.
func (p *evProgram) localsOf(f *evFile, root ast.Node) map[types.Object][]ast.Expr {
	if m, ok := p.assigns[root]; ok {
		return m
	}
	m := localAssignments(f.info, root)
	ast.Inspect(root, func(n ast.Node) bool {
		rs, ok := n.(*ast.RangeStmt)
		if !ok || rs.Tok != token.DEFINE {
			return true
		}
		for _, k := range []ast.Expr{rs.Key, rs.Value} {
			if id, ok := k.(*ast.Ident); ok {
				if obj := f.info.Defs[id]; obj != nil {
					m[obj] = append(m[obj], &ast.IndexExpr{X: rs.X, Lbrack: rs.X.Pos(), Index: ast.NewIdent("_"), Rbrack: rs.X.End()})
				}
			}
		}
		return true
	})
	p.assigns[root] = m
	return m
}

func evIsStringType(t types.Type) bool {
	if t == nil {
		return false
	}
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsString != 0
}

func evIsStringSlice(t types.Type) bool {
	if t == nil {
		return false
	}
	s, ok := t.Underlying().(*types.Slice)
	return ok && evIsStringType(s.Elem())
}

// evStringish is a string type or a container of one.
func evStringish(t types.Type) bool {
	if t == nil {
		return false
	}
	switch u := t.Underlying().(type) {
	case *types.Basic:
		return u.Info()&types.IsString != 0
	case *types.Slice:
		return evStringish(u.Elem())
	case *types.Array:
		return evStringish(u.Elem())
	case *types.Map:
		return evStringish(u.Elem())
	case *types.Pointer:
		return evStringish(u.Elem())
	}
	return false
}

func evIsClierrType(t types.Type, name string) bool {
	return t != nil && isNamedType(t, modulePrefix+evClierr, name)
}

// evIsWordType reports clierr.CommandWord and clierr.Lead.
func evIsWordType(t types.Type) bool {
	return evIsClierrType(t, "CommandWord") || evIsClierrType(t, "Lead")
}

func evIsKind(t types.Type) bool { return t != nil && isNamedType(t, modulePrefix+evKinds, "Kind") }

// evHolds reports whether t is a type target matches, or holds one in a
// field (of a nested struct too), an element, a map key or value, or behind
// a pointer. For a struct of internal/clierr only its exported fields count.
func evHolds(t types.Type, target func(types.Type) bool, seen map[types.Type]bool) bool {
	if t == nil || seen[t] {
		return false
	}
	if target(t) {
		return true
	}
	seen[t] = true
	clierrStruct := false
	if n, ok := types.Unalias(t).(*types.Named); ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == modulePrefix+evClierr {
		clierrStruct = true
	}
	switch u := t.Underlying().(type) {
	case *types.Struct:
		for fld := range u.Fields() {
			if clierrStruct && !fld.Exported() {
				continue
			}
			if evHolds(fld.Type(), target, seen) {
				return true
			}
		}
	case *types.Slice:
		return evHolds(u.Elem(), target, seen)
	case *types.Array:
		return evHolds(u.Elem(), target, seen)
	case *types.Pointer:
		return evHolds(u.Elem(), target, seen)
	case *types.Map:
		return evHolds(u.Key(), target, seen) || evHolds(u.Elem(), target, seen)
	case *types.Chan:
		return evHolds(u.Elem(), target, seen)
	}
	return false
}

func evHoldsWord(t types.Type) bool { return evHolds(t, evIsWordType, map[types.Type]bool{}) }
func evHoldsKind(t types.Type) bool { return evHolds(t, evIsKind, map[types.Type]bool{}) }

// evFieldOwner is the named struct type that declares the field a selection
// reads, through embedded fields.
func evFieldOwner(sel *types.Selection) *types.Named {
	t := sel.Recv()
	idx := sel.Index()
	for _, i := range idx[:len(idx)-1] {
		if p, ok := t.Underlying().(*types.Pointer); ok {
			t = p.Elem()
		}
		st, ok := t.Underlying().(*types.Struct)
		if !ok {
			return nil
		}
		t = st.Field(i).Type()
	}
	if p, ok := t.Underlying().(*types.Pointer); ok {
		t = p.Elem()
	}
	n, _ := types.Unalias(t).(*types.Named)
	return n
}

func evNamedIs(n *types.Named, pkgRel, name string) bool {
	return n != nil && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == modulePrefix+pkgRel && n.Obj().Name() == name
}

// evImplements reports whether t or *t has every method of iface with the
// same signature, compared as text: the loads hold more than one copy of a
// package's types, so types.Implements cannot be used across packages.
func evImplements(t types.Type, iface *types.Interface) bool {
	if iface.NumMethods() == 0 {
		return false
	}
	ms := types.NewMethodSet(types.NewPointer(t))
	for m := range iface.Methods() {
		found := false
		for method := range ms.Methods() {
			o := method.Obj()
			if o.Name() == m.Name() && types.TypeString(o.Type(), nil) == types.TypeString(m.Type(), nil) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Keys of the builder and of the functions the scans name.
var (
	evBuilderMethods = map[string]bool{
		"internal/clierr.Command.String":  true,
		"internal/clierr.Command.Field":   true,
		"internal/clierr.Sequence.String": true,
		"internal/clierr.Sequence.Field":  true,
		"internal/clierr.Sequence.Lines":  true,
		"internal/clierr.TypedLine.Field": true,
	}
	evFragmentFuncs = map[string]bool{
		"internal/clierr.FlagFragment":          true,
		"internal/clierr.FlagFragmentWord":      true,
		"internal/clierr.FlagFragmentAfterWord": true,
		"internal/clierr.FlagFragmentText":      true,
	}
	evStringMethods = map[string]bool{
		"internal/clierr.Command.String":  true,
		"internal/clierr.Sequence.String": true,
	}
)

const (
	evText        = "internal/clierr.Text"
	evShellQuote  = "internal/clierr.ShellQuote"
	evMarkRemedy  = "internal/clierr.MarkRemedy"
	evDirFree     = "internal/clierr.DirectoryFree"
	evRancherVar  = "internal/clierr.Rancher"
	evMarkerVar   = "internal/clierr.RemedyMarker"
	evKindWord    = "internal/engine/kinds.Kind.Word"
	evKindAlias   = "internal/engine/kinds.Kind.AliasWord"
	evNewCommand  = "internal/clierr.NewCommand"
	evFileCommand = "internal/clierr.FileCommand"
	evFileCmdIn   = "internal/clierr.FileCommandIn"
)

// evShellQuoteArgs are the spellings of the positional quoting helper, before
// and after it becomes unexported.
var evShellQuoteArgs = map[string]bool{"internal/clierr.ShellQuoteArgs": true, "internal/clierr.shellQuoteArgs": true}

// evKind is what an operand of a text is.
type evKind int

const (
	evOther    evKind = iota // a value the text does not write
	evConst                  // constant text, a CommandWord or a local of constants only
	evBuilder                // a Command, Sequence or TypedLine method result
	evFragment               // a fragment function result
	evRancher                // clierr.Rancher, the marker, a variable built on them, MarkRemedy
	evTextCall               // a clierr.Text result
	evQuoted                 // clierr.ShellQuote output
)

func (k evKind) start() bool { return k == evBuilder || k == evFragment || k == evRancher }

// evItem is one piece of a text: constant text or an operand.
type evItem struct {
	lit   string
	isLit bool
	expr  ast.Expr
	kind  evKind
	open  bool
}

var (
	evVerb       = regexp.MustCompile(`%(\[\d+\])?[-+# 0]*(\*|\d+)?(\.(\*|\d+)?)?[a-zA-Z%]`)
	evPlainStart = regexp.MustCompile("(^|[\\s:;(`'])rancher( |$)")
)

func evHasClauseEnd(s string) bool {
	return strings.Contains(s, "; ") || strings.Contains(s, ". ") || strings.Contains(s, "\n")
}

// evAfterClauseEnd is the part of s after its last clause end, and whether
// it has one.
func evAfterClauseEnd(s string) (string, bool) {
	i := max(strings.LastIndex(s, "; ")+2, strings.LastIndex(s, ". ")+2, strings.LastIndex(s, "\n")+1)
	if i <= 1 && !strings.HasPrefix(s, "\n") {
		return s, false
	}
	return s[i:], true
}

// items is the pieces of a text expression in the order they print: the
// operands of a + chain, a format of fmt.Sprintf or fmt.Errorf with its
// arguments, the elements of a []string literal joined by strings.Join, and
// the operand of a conversion to a string type.
func (p *evProgram) items(f *evFile, e ast.Expr, depth int) []evItem {
	e = ast.Unparen(e)
	if s, ok := constString(f.info, e); ok {
		return []evItem{{lit: s, isLit: true}}
	}
	spaced := func(parts []ast.Expr) []evItem {
		var out []evItem
		for i, el := range parts {
			if i > 0 {
				out = append(out, evItem{lit: " ", isLit: true})
			}
			out = append(out, p.items(f, el, depth)...)
		}
		return out
	}
	switch x := e.(type) {
	case *ast.BinaryExpr:
		if x.Op == token.ADD && evIsStringType(f.info.TypeOf(x)) {
			return append(p.items(f, x.X, depth), p.items(f, x.Y, depth)...)
		}
	case *ast.CompositeLit:
		if evIsStringSlice(f.info.TypeOf(x)) {
			return spaced(x.Elts)
		}
	case *ast.CallExpr:
		if evBuiltin(f.info, x) == "append" && evIsStringSlice(f.info.TypeOf(x)) && x.Ellipsis == token.NoPos {
			return spaced(x.Args)
		}
		if tv, ok := f.info.Types[x.Fun]; ok && tv.IsType() && len(x.Args) == 1 && evIsStringType(f.info.TypeOf(x)) && !evIsWordType(f.info.TypeOf(x)) {
			return p.items(f, x.Args[0], depth)
		}
		switch key, _ := evCallee(f.info, x); key {
		case "fmt.Sprintf", "fmt.Errorf":
			if len(x.Args) > 0 {
				if format, ok := constString(f.info, x.Args[0]); ok {
					return p.formatItems(f, format, x.Args[1:], depth)
				}
			}
		case "fmt.Sprint":
			var out []evItem
			for _, a := range x.Args {
				out = append(out, p.items(f, a, depth)...)
			}
			return out
		case "strings.Join":
			if len(x.Args) == 2 {
				lit, ok := ast.Unparen(x.Args[0]).(*ast.CompositeLit)
				sep, sok := constString(f.info, x.Args[1])
				if ok && sok {
					var out []evItem
					for i, el := range lit.Elts {
						if i > 0 {
							out = append(out, evItem{lit: sep, isLit: true})
						}
						out = append(out, p.items(f, el, depth)...)
					}
					return out
				}
			}
		}
	}
	k, open := p.kindOf(f, e, depth)
	return []evItem{{expr: e, kind: k, open: open}}
}

func (p *evProgram) formatItems(f *evFile, format string, args []ast.Expr, depth int) []evItem {
	var out []evItem
	last, next := 0, 0
	for _, loc := range evVerb.FindAllStringSubmatchIndex(format, -1) {
		out = append(out, evItem{lit: format[last:loc[0]], isLit: true})
		last = loc[1]
		verb := format[loc[0]:loc[1]]
		switch {
		case strings.HasSuffix(verb, "%"):
			out = append(out, evItem{lit: "%", isLit: true})
		case loc[2] >= 0 || strings.Contains(verb, "*") || next >= len(args):
			out = append(out, evItem{kind: evOther})
		default:
			out = append(out, p.items(f, args[next], depth)...)
			next++
		}
	}
	return append(out, evItem{lit: format[last:], isLit: true})
}

// evOpen reports whether a text ends inside the clause of a command. A
// fragment is a whole flag with its value, so it opens no clause.
func evOpen(items []evItem) bool {
	open := false
	for _, it := range items {
		switch {
		case it.isLit:
			if evHasClauseEnd(it.lit) {
				open = false
			}
		case it.kind == evBuilder || it.kind == evRancher:
			open = true
		}
	}
	return open
}

// textCallHoldsCommand reports whether a clierr.Text call has a Command or a
// Sequence part.
func textCallHoldsCommand(f *evFile, call *ast.CallExpr) bool {
	return slices.ContainsFunc(call.Args, func(a ast.Expr) bool {
		t := f.info.TypeOf(a)
		return evIsClierrType(t, "Command") || evIsClierrType(t, "Sequence")
	})
}

// kindOf classifies one operand of a text. A local variable of the
// function counts through the expressions assigned to it.
func (p *evProgram) kindOf(f *evFile, e ast.Expr, depth int) (evKind, bool) {
	e = ast.Unparen(e)
	t := f.info.TypeOf(e)
	if evIsWordType(t) {
		return evConst, false
	}
	if _, ok := constString(f.info, e); ok {
		return evConst, false
	}
	switch x := e.(type) {
	case *ast.CallExpr:
		key, fn := evCallee(f.info, x)
		switch {
		case evBuilderMethods[key]:
			return evBuilder, true
		case evFragmentFuncs[key]:
			return evFragment, true
		case key == evText:
			return evTextCall, false
		case key == evMarkRemedy:
			return evRancher, true
		case key == evShellQuote:
			return evQuoted, false
		}
		if evIsModule(fn) && depth < 3 && p.returnsText(key, depth+1) {
			return evTextCall, false
		}
	case *ast.IndexExpr:
		if call, ok := ast.Unparen(x.X).(*ast.CallExpr); ok {
			if key, _ := evCallee(f.info, call); evBuilderMethods[key] {
				return evBuilder, true
			}
		}
	case *ast.Ident, *ast.SelectorExpr:
		id, _ := x.(*ast.Ident)
		if sel, ok := x.(*ast.SelectorExpr); ok {
			id = sel.Sel
		}
		obj := f.info.Uses[id]
		v, ok := obj.(*types.Var)
		if !ok || v.Pkg() == nil {
			break
		}
		if !v.IsField() {
			key := objectKey(v)
			if key == evRancherVar || key == evMarkerVar || p.members[key] {
				return evRancher, true
			}
		}
		if _, isIdent := x.(*ast.Ident); !isIdent || v.IsField() || v.Parent() == v.Pkg().Scope() || depth >= 4 {
			break
		}
		return p.localKind(f, x.(*ast.Ident), nil, depth)
	}
	return evOther, false
}

// localKind classifies a local variable by the values assigned to it, other
// than skip: a command start when any value holds one (open when any value
// ends inside a command's clause), constant when every value is, a
// clierr.Text result when every value is one.
func (p *evProgram) localKind(f *evFile, id *ast.Ident, skip ast.Expr, depth int) (evKind, bool) {
	v, ok := f.info.Uses[id].(*types.Var)
	if !ok || v.Pkg() == nil || v.IsField() || v.Parent() == v.Pkg().Scope() || depth >= 4 {
		return evOther, false
	}
	root, _ := f.enclosing(id)
	var values []ast.Expr
	for _, val := range p.localsOf(f, root)[v] {
		if val != skip {
			values = append(values, val)
		}
	}
	if len(values) == 0 {
		return evOther, false
	}
	kind, open, consts, texts := evOther, false, 0, 0
	for _, val := range values {
		its := p.items(f, val, depth+1)
		if len(its) == 1 && !its[0].isLit && its[0].kind == evTextCall {
			texts++
			continue
		}
		if !slices.ContainsFunc(its, func(it evItem) bool { return !it.isLit && it.kind != evConst }) {
			consts++
		}
		for _, it := range its {
			switch {
			case it.isLit:
			case it.kind == evFragment && kind != evBuilder && kind != evRancher:
				kind = evFragment
			case it.kind == evBuilder || it.kind == evRancher:
				kind = it.kind
			case it.kind == evQuoted && kind == evOther:
				kind = evQuoted
			}
		}
		open = open || evOpen(its)
	}
	switch {
	case kind != evOther:
		return kind, open
	case consts == len(values):
		return evConst, false
	case texts == len(values):
		return evTextCall, false
	}
	return evOther, false
}

// returnsText reports whether every string a module function returns is a
// clierr.Text result.
func (p *evProgram) returnsText(key string, depth int) bool {
	d, ok := p.funcs[key]
	if !ok {
		return false
	}
	sig := d.fn.Type().(*types.Signature)
	found, all := false, true
	ast.Inspect(d.decl.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || len(ret.Results) != sig.Results().Len() {
			return true
		}
		for i, r := range ret.Results {
			if !evIsStringType(sig.Results().At(i).Type()) || evIsWordType(sig.Results().At(i).Type()) {
				continue
			}
			found = true
			if k, _ := p.kindOf(d.f, r, depth); k != evTextCall {
				all = false
			}
		}
		return true
	})
	return found && all
}

// textRoots calls visit for every text expression of the files: a + chain of
// strings that is not part of another, a call of fmt.Sprintf, fmt.Errorf,
// fmt.Sprint or strings.Join, and a += on a string variable, with its pieces.
// For a +=, the first piece is the variable with what was assigned to it
// before.
func (p *evProgram) textRoots(visit func(f *evFile, root ast.Node, items []evItem)) {
	for _, f := range p.files {
		ast.Inspect(f.file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BinaryExpr:
				if x.Op != token.ADD || !evIsStringType(f.info.TypeOf(x)) {
					return true
				}
				parent := f.parents[n]
				for {
					pe, ok := parent.(*ast.ParenExpr)
					if !ok {
						break
					}
					parent = f.parents[pe]
				}
				if pb, ok := parent.(*ast.BinaryExpr); ok && pb.Op == token.ADD && evIsStringType(f.info.TypeOf(pb)) {
					return true
				}
				visit(f, x, p.items(f, x, 0))
			case *ast.CallExpr:
				switch key, _ := evCallee(f.info, x); key {
				case "fmt.Sprintf", "fmt.Errorf", "fmt.Sprint", "strings.Join":
					visit(f, x, p.items(f, x, 0))
				}
			case *ast.AssignStmt:
				if x.Tok != token.ADD_ASSIGN || len(x.Lhs) != 1 || !evIsStringType(f.info.TypeOf(x.Lhs[0])) {
					return true
				}
				first := evItem{expr: x.Lhs[0], kind: evOther}
				if id, ok := x.Lhs[0].(*ast.Ident); ok {
					first.kind, first.open = p.localKind(f, id, x.Rhs[0], 0)
				}
				visit(f, x, append([]evItem{first}, p.items(f, x.Rhs[0], 0)...))
			}
			return true
		})
	}
}

// evLoad is the production tree type-checked for this machine, and for
// GOOS=windows and GOOS=linux when that is not this machine, each indexed.
var evProductionPrograms = sync.OnceValues(func() ([]*evProgram, error) {
	sets, err := catalogScanPackages()
	if err != nil {
		return nil, err
	}
	var out []*evProgram
	for _, pkgs := range sets {
		p, err := newEVProgram(pkgs, evProductionFile)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
})

// evProductionResults is every exact-value scan over the production tree,
// with the production allowlists the walks consult.
var evProductionResults = sync.OnceValues(func() (*evResults, error) {
	programs, err := evProductionPrograms()
	if err != nil {
		return nil, err
	}
	out := &evResults{used: map[string]bool{}, files: map[string]bool{}}
	for _, p := range programs {
		out.merge(p.scan(evProductionAllow))
	}
	return out, nil
})

// evFixtureResults is every exact-value scan over the fixture packages, with
// the fixture allowlists.
var evFixtureResults = sync.OnceValues(func() (*evResults, error) {
	pkgs, err := loadPackages("./"+evFixturesDir+"/...", "")
	if err != nil {
		return nil, err
	}
	p, err := newEVProgram(pkgs, func(path string) bool { return strings.HasPrefix(path, evFixturesDir+"/") })
	if err != nil {
		return nil, err
	}
	return p.scan(evFixtureAllow), nil
})

// scan runs every exact-value scan over the program.
func (p *evProgram) scan(allow evAllow) *evResults {
	r := &evResults{used: map[string]bool{}, files: map[string]bool{}}
	for _, f := range p.files {
		r.files[f.path] = true
	}
	p.scanOrigins(r, allow)
	p.scanFilters(r)
	p.scanWordOrigins(r)
	p.scanKinds(r)
	p.scanTextCalls(r)
	p.scanShellQuote(r)
	p.scanTexts(r)
	p.scanHelpers(r)
	p.scanRevokes(r)
	p.scanTypedLine(r)
	p.scanCommandFields(r)
	return r
}
