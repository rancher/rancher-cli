package conformance

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// Terms used in this file. The listed names are the keys of
// quotedFormRefused. The walk (quotedFormWalk) starts at an expression and
// goes to the values it is built from: a variable to the values assigned to
// it, a call to the values its function returns, a parameter to the arguments
// of the calls of its function, and so on (quotedFormReaches lists every
// case). The walk follows an expression when it goes from it to the values it
// is built from, and it follows a call into its function when it goes on to
// the values that function returns. A value or an argument reaches a listed
// name when the walk from it finds a use of that name. A leaf is an
// expression the walk finds no values for, and it reports nothing for it; the
// arguments of a call that is a leaf are still followed. A strconv quoting
// function is one of the functions in quotedFormStrconv, which quote a
// string. A sink is a call that prints a string in Go-quoted form: a call of
// a strconv quoting function, or a printf-family call with a %q or %#v verb
// or with a format that is not constant. A finding is what the scan finds for
// one quoted argument of a sink that reaches a listed name, or for one use of
// a strconv quoting function as a value (the scan counts such a use as a sink
// too); it names that argument, or that function. A load is the module's
// packages type-checked for one platform.

const quotedFormDir = "test/conformance/testdata/quotedform"

// quotedFormRefused lists the names that a value printed in Go-quoted form
// (by a strconv quoting function, or by a %q or %#v verb of a printf-family
// call) must not reach: ShellQuote and shellQuoteArgs, clierr.Rancher (the
// text that begins every remedy command), RemedyMarker (which opens every
// remedy), the functions that write a remedy command, and the command
// builder, which starts a command or writes text that holds one.
var quotedFormRefused = map[string]bool{
	"internal/clierr.ShellQuote":     true,
	"internal/clierr.shellQuoteArgs": true,
	"internal/clierr.Rancher":        true,

	"internal/clierr.NewCommand":            true,
	"internal/clierr.FileCommand":           true,
	"internal/clierr.FileCommandIn":         true,
	"internal/clierr.NewSequence":           true,
	"internal/clierr.NewTypedLine":          true,
	"internal/clierr.Revoke":                true,
	"internal/clierr.RevokeKubeconfig":      true,
	"internal/clierr.FileRevokeKubeconfig":  true,
	"internal/clierr.Text":                  true,
	"internal/clierr.FlagFragment":          true,
	"internal/clierr.FlagFragmentText":      true,
	"internal/clierr.FlagFragmentWord":      true,
	"internal/clierr.FlagFragmentAfterWord": true,
	"internal/clierr.DirectoryFree":         true,
	"internal/clierr.MarkRemedy":            true,
	"internal/clierr.RemedyMarker":          true,
	"internal/clierr.ShellWord":             true,
}

// quotedFormOtherGOOS lists the platforms whose files the scan of the module
// reads, besides the files of the platform the test runs on.
var quotedFormOtherGOOS = []string{"windows", "linux"}

// quotedFormAllowance is what an allowlisted function covers: the number of
// findings (distinct position and argument pairs), the source text of each
// quoted argument the entry names, and the reason a Go-quoted form there is
// correct.
type quotedFormAllowance struct {
	count     int
	arguments []string
	reason    string
}

// quotedFormAllowlist lists the functions where the scan reports a finding
// although the quoted text holds no command. Each key is a function key: the
// package's path under the module, the receiver type for a method, and the
// name. An entry names the quoted arguments it covers, and it covers any
// value built into them: a change to the function can add a command to a
// covered argument with no failure; check the function again whenever it
// changes. The walk does not tell the callers of a function apart: when one
// caller passes a command in, the walk treats the quoted value of every other
// caller as built from that command too.
var quotedFormAllowlist = map[string]quotedFormAllowance{
	"internal/cli.completionDebugf": {1, []string{"args"}, "a wrapper whose format is not constant. Every argument of every caller is checked; a %q of a command in a caller is reported at that caller's own call, and the one caller that passes clierr.Rancher (the text that begins every remedy command) prints it with %s"},
}

// quotedFormValue is an expression with the type information of its package.
type quotedFormValue struct {
	info *types.Info
	expr ast.Expr
}

// quotedFormCall is a static call with the type information of its package.
type quotedFormCall struct {
	info *types.Info
	call *ast.CallExpr
}

// quotedFormParam locates a parameter: the function it belongs to, its
// position and whether it is the variadic one.
type quotedFormParam struct {
	function string
	position int
	variadic bool
}

// quotedFormIndex holds what the walk looks up to follow an expression to the
// values it is built from, collected once for a set of packages: the values
// assigned to each variable, the parameters and calls of each function, and
// the values written to each buffer.
type quotedFormIndex struct {
	funcs   map[string]moduleFunction
	locals  map[types.Object][]quotedFormValue
	ranges  map[types.Object][]quotedFormValue
	params  map[types.Object]quotedFormParam
	callers map[string][]quotedFormCall
	pkgVars map[string][]quotedFormValue
	writes  map[types.Object][]quotedFormValue
	tuple   map[types.Object]map[ast.Expr]int
	// closures maps a local variable that holds a function literal to the key
	// under which the literal's parameters and the calls of the variable are
	// recorded.
	closures map[types.Object]string
}

func newQuotedFormIndex(pkgs []*packages.Package) *quotedFormIndex {
	x := &quotedFormIndex{
		funcs:    moduleFunctions(pkgs),
		locals:   map[types.Object][]quotedFormValue{},
		ranges:   map[types.Object][]quotedFormValue{},
		params:   map[types.Object]quotedFormParam{},
		callers:  map[string][]quotedFormCall{},
		pkgVars:  map[string][]quotedFormValue{},
		writes:   map[types.Object][]quotedFormValue{},
		tuple:    map[types.Object]map[ast.Expr]int{},
		closures: map[types.Object]string{},
	}
	for _, pkg := range pkgs {
		info := pkg.TypesInfo
		for _, f := range pkg.Syntax {
			for obj, values := range localAssignments(info, f) {
				for _, v := range values {
					x.locals[obj] = append(x.locals[obj], quotedFormValue{info, v})
					if lit, ok := ast.Unparen(v).(*ast.FuncLit); ok && obj.Pkg() != nil {
						key := "closure:" + obj.Pkg().Path() + "." + obj.Name() + "@" + strconv.Itoa(int(obj.Pos()))
						x.closures[obj] = key
						x.addParamList(info, key, lit.Type)
					}
				}
			}
			for _, decl := range f.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					x.addParams(info, d)
				case *ast.GenDecl:
					if d.Tok == token.VAR {
						x.addPackageVars(info, d)
					}
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.AssignStmt:
					x.addAssignment(info, n)
					if n.Tok == token.DEFINE || n.Tok == token.ASSIGN {
						x.addTuple(info, n.Lhs, n.Rhs)
					}
				case *ast.ValueSpec:
					names := make([]ast.Expr, len(n.Names))
					for i, name := range n.Names {
						names[i] = name
					}
					x.addTuple(info, names, n.Values)
				case *ast.RangeStmt:
					x.addRange(info, n)
				case *ast.CallExpr:
					x.addCall(info, n)
				}
				return true
			})
		}
	}
	return x
}

// addTuple records, for each variable that an expression with several values
// (a call, or the comma-ok form of an index, a type assertion or a receive) is
// assigned to, the position of the value it receives.
func (x *quotedFormIndex) addTuple(info *types.Info, lhs, rhs []ast.Expr) {
	if len(rhs) != 1 || len(lhs) < 2 {
		return
	}
	for i, l := range lhs {
		id, ok := l.(*ast.Ident)
		if !ok {
			continue
		}
		obj := info.Defs[id]
		if obj == nil {
			obj = info.Uses[id]
		}
		if obj == nil {
			continue
		}
		if x.tuple[obj] == nil {
			x.tuple[obj] = map[ast.Expr]int{}
		}
		x.tuple[obj][rhs[0]] = i
	}
}

// addParams records the parameters of a function declaration.
func (x *quotedFormIndex) addParams(info *types.Info, d *ast.FuncDecl) {
	if fn, ok := info.Defs[d.Name].(*types.Func); ok {
		x.addParamList(info, funcKey(fn), d.Type)
	}
}

// addParamList records the parameters of a function type under key: a function
// declaration's own key, or the key of a function literal held by a variable.
func (x *quotedFormIndex) addParamList(info *types.Info, key string, ft *ast.FuncType) {
	if ft.Params == nil {
		return
	}
	position := 0
	for _, field := range ft.Params.List {
		_, variadic := field.Type.(*ast.Ellipsis)
		if len(field.Names) == 0 {
			position++
			continue
		}
		for _, name := range field.Names {
			if obj := info.Defs[name]; obj != nil {
				x.params[obj] = quotedFormParam{key, position, variadic}
			}
			position++
		}
	}
}

// addPackageVars records the initializer of each package-level variable of a
// declaration.
func (x *quotedFormIndex) addPackageVars(info *types.Info, d *ast.GenDecl) {
	for _, spec := range d.Specs {
		vs := spec.(*ast.ValueSpec)
		for i, name := range vs.Names {
			obj := info.Defs[name]
			if obj == nil || name.Name == "_" {
				continue
			}
			switch {
			case len(vs.Values) == len(vs.Names):
				x.pkgVars[objectKey(obj)] = append(x.pkgVars[objectKey(obj)], quotedFormValue{info, vs.Values[i]})
			case len(vs.Values) == 1:
				x.pkgVars[objectKey(obj)] = append(x.pkgVars[objectKey(obj)], quotedFormValue{info, vs.Values[0]})
			}
		}
	}
}

// addAssignment records an assignment to a package-level variable of the
// module, and an assignment through an index to a local variable or a
// package-level variable (localAssignments skips both).
func (x *quotedFormIndex) addAssignment(info *types.Info, a *ast.AssignStmt) {
	if a.Tok != token.ASSIGN && a.Tok != token.ADD_ASSIGN {
		return
	}
	for i, lhs := range a.Lhs {
		var value ast.Expr
		switch {
		case len(a.Rhs) == len(a.Lhs):
			value = a.Rhs[i]
		case len(a.Rhs) == 1:
			value = a.Rhs[0]
		default:
			continue
		}
		target := ast.Unparen(lhs)
		indexed := false
		for {
			index, ok := target.(*ast.IndexExpr)
			if !ok {
				break
			}
			target = ast.Unparen(index.X)
			indexed = true
		}
		var id *ast.Ident
		switch t := target.(type) {
		case *ast.Ident:
			id = t
		case *ast.SelectorExpr:
			if info.Selections[t] == nil {
				id = t.Sel
			}
		}
		if id == nil {
			continue
		}
		v, ok := info.Uses[id].(*types.Var)
		if !ok || v.IsField() || v.Pkg() == nil {
			continue
		}
		switch {
		case v.Parent() == v.Pkg().Scope():
			if strings.HasPrefix(v.Pkg().Path(), modulePrefix) {
				x.pkgVars[objectKey(v)] = append(x.pkgVars[objectKey(v)], quotedFormValue{info, value})
			}
		case indexed:
			x.locals[v] = append(x.locals[v], quotedFormValue{info, value})
		}
	}
}

// addRange records the ranged expression for the key and the value variable
// of a range statement. A range over an integer, a function or a channel
// holds no expression to follow.
func (x *quotedFormIndex) addRange(info *types.Info, r *ast.RangeStmt) {
	t := info.TypeOf(r.X)
	if t == nil {
		return
	}
	switch u := t.Underlying().(type) {
	case *types.Signature, *types.Chan:
		return
	case *types.Basic:
		if u.Info()&types.IsString == 0 {
			return
		}
	}
	for _, e := range []ast.Expr{r.Key, r.Value} {
		id, ok := e.(*ast.Ident)
		if !ok {
			continue
		}
		obj := info.Defs[id]
		if obj == nil {
			obj = info.Uses[id]
		}
		if v, ok := obj.(*types.Var); ok {
			x.ranges[v] = append(x.ranges[v], quotedFormValue{info, r.X})
		}
	}
}

// addCall records a static call under its callee, and the arguments of a
// write into a strings.Builder or bytes.Buffer under that variable: its
// WriteString and Write calls, and the fmt.Fprintf and fmt.Fprint calls that
// take it.
func (x *quotedFormIndex) addCall(info *types.Info, c *ast.CallExpr) {
	if fn := quotedFormCallee(info, c); fn != nil {
		if key := quotedFormFuncKey(fn); key != "" {
			x.callers[key] = append(x.callers[key], quotedFormCall{info, c})
		}
	} else if id, ok := ast.Unparen(c.Fun).(*ast.Ident); ok {
		if key, ok := x.closures[info.Uses[id]]; ok {
			x.callers[key] = append(x.callers[key], quotedFormCall{info, c})
		}
	}
	sel, ok := ast.Unparen(c.Fun).(*ast.SelectorExpr)
	if !ok {
		return
	}
	if info.Selections[sel] != nil {
		if (sel.Sel.Name == "WriteString" || sel.Sel.Name == "Write") && len(c.Args) == 1 {
			if v := quotedFormBuilderVar(info, sel.X); v != nil {
				x.writes[v] = append(x.writes[v], quotedFormValue{info, c.Args[0]})
			}
		}
		return
	}
	fn, _ := info.Uses[sel.Sel].(*types.Func)
	if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != "fmt" || len(c.Args) < 2 {
		return
	}
	if fn.Name() != "Fprintf" && fn.Name() != "Fprint" {
		return
	}
	if v := quotedFormBuilderVar(info, c.Args[0]); v != nil {
		for _, a := range c.Args[1:] {
			x.writes[v] = append(x.writes[v], quotedFormValue{info, a})
		}
	}
}

// quotedFormBuilderVar returns the local variable that e names, or takes the
// address of, when its type is a strings.Builder or a bytes.Buffer (or a
// pointer to one).
func quotedFormBuilderVar(info *types.Info, e ast.Expr) *types.Var {
	e = ast.Unparen(e)
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = ast.Unparen(u.X)
	}
	id, ok := e.(*ast.Ident)
	if !ok {
		return nil
	}
	v, ok := info.Uses[id].(*types.Var)
	if !ok || v.IsField() || v.Pkg() == nil || v.Parent() == v.Pkg().Scope() {
		return nil
	}
	t := v.Type()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := types.Unalias(t).(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return nil
	}
	switch named.Obj().Pkg().Path() + "." + named.Obj().Name() {
	case "strings.Builder", "bytes.Buffer":
		return v
	}
	return nil
}

// quotedFormCarriesState reports whether the result at position pos (every
// result when pos is -1) of a call of type t is a struct or a pointer to one.
func quotedFormCarriesState(t types.Type, pos int) bool {
	if t == nil {
		return false
	}
	if tuple, ok := t.(*types.Tuple); ok {
		for i := range tuple.Len() {
			if (pos < 0 || pos == i) && quotedFormCarriesState(tuple.At(i).Type(), -1) {
				return true
			}
		}
		return false
	}
	if p, ok := t.Underlying().(*types.Pointer); ok {
		t = p.Elem()
	}
	_, isStruct := t.Underlying().(*types.Struct)
	return isStruct
}

// quotedFormCommandType reports whether t is one of the types of the command
// builder, whose methods return text built from the command it started with:
// the walk follows the receiver of a method call on a value of one of them.
func quotedFormCommandType(t types.Type) bool {
	if t == nil {
		return false
	}
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := types.Unalias(t).(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	switch objectKey(named.Obj()) {
	case "internal/clierr.Command", "internal/clierr.Sequence", "internal/clierr.TypedLine":
		return true
	}
	return false
}

// quotedFormCallee returns the function or method a call names statically: a
// function, a qualified function of another package, or a method called on a
// value. A function held in a variable or a field, a method expression and a
// conversion have none.
func quotedFormCallee(info *types.Info, c *ast.CallExpr) *types.Func {
	fun := ast.Unparen(c.Fun)
	switch f := fun.(type) {
	case *ast.IndexExpr:
		fun = ast.Unparen(f.X)
	case *ast.IndexListExpr:
		fun = ast.Unparen(f.X)
	}
	switch f := fun.(type) {
	case *ast.Ident:
		fn, _ := info.Uses[f].(*types.Func)
		return fn
	case *ast.SelectorExpr:
		if sel := info.Selections[f]; sel != nil {
			if sel.Kind() != types.MethodVal {
				return nil
			}
			fn, _ := sel.Obj().(*types.Func)
			return fn
		}
		fn, _ := info.Uses[f.Sel].(*types.Func)
		return fn
	}
	return nil
}

// quotedFormFuncKey is funcKey, and "" for a method of a type that has no
// package (a method of error).
func quotedFormFuncKey(fn *types.Func) string {
	if fn.Pkg() == nil {
		return ""
	}
	if recv := fn.Type().(*types.Signature).Recv(); recv != nil {
		t := recv.Type()
		if ptr, ok := t.(*types.Pointer); ok {
			t = ptr.Elem()
		}
		if named, ok := types.Unalias(t).(*types.Named); ok && named.Obj().Pkg() == nil {
			return ""
		}
	}
	return funcKey(fn)
}

// quotedFormRefusedUse reports whether obj is one of the listed names: a
// function or method with its key, or a package-level variable.
func quotedFormRefusedUse(obj types.Object) bool {
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	switch o := obj.(type) {
	case *types.Func:
		return quotedFormRefused[quotedFormFuncKey(o)]
	case *types.Var:
		return !o.IsField() && o.Parent() == o.Pkg().Scope() && quotedFormRefused[objectKey(o)]
	}
	return false
}

// quotedFormWalk holds the state of one walk: the index it looks up, what it
// has already followed, and whether it has found a use of a listed name.
type quotedFormWalk struct {
	idx  *quotedFormIndex
	seen map[any]bool
	hit  bool
}

// quotedFormReaches reports whether e reaches a listed name: whether the walk
// from e finds a use of one. The walk looks at every identifier and call in e
// (the arguments of a call it does not follow into, such as one of a function
// outside the module, a builtin or a conversion, included) and continues
// from:
//   - a local variable: every expression assigned to it with :=, = or +=, or
//     through an index, or given by a var declaration;
//   - a call of a function or method of the module: every expression its
//     return statements return, a named result at a bare return included.
//     For a variable that receives one value of several (a, b := f()), the
//     walk continues only from the expression at its own position, and from
//     the named result at it. The walk follows the arguments of that call
//     only through the parameters they are bound to, except when the result
//     used is a struct or a pointer to one: then it follows the arguments
//     directly as well;
//   - a parameter of a function of the module: the arguments at every static
//     call of that function in the packages, and for a parameter of a function
//     literal held by a local variable, the arguments at the calls of that
//     variable;
//   - a package-level variable of the module: its initializer and every
//     assignment to it;
//   - a strings.Builder or bytes.Buffer local: the arguments written to it;
//   - a range variable: the ranged expression.
//
// The walk stops at a leaf: a struct field read, a map lookup, a function
// value (a variable that holds a function literal or a method value, and a
// literal), an interface method call, a type assertion, the binding of a type
// switch, a channel receive, or a call of a function outside the module. It
// reports nothing for a leaf. It still follows the arguments of a call that is
// a leaf.
func quotedFormReaches(idx *quotedFormIndex, info *types.Info, e ast.Expr) bool {
	w := &quotedFormWalk{idx: idx, seen: map[any]bool{}}
	w.expr(info, e)
	return w.hit
}

func (w *quotedFormWalk) expr(info *types.Info, e ast.Expr) {
	switch e := e.(type) {
	case *ast.ParenExpr:
		w.expr(info, e.X)
	case *ast.BinaryExpr:
		w.expr(info, e.X)
		w.expr(info, e.Y)
	case *ast.UnaryExpr:
		if e.Op != token.ARROW {
			w.expr(info, e.X)
		}
	case *ast.StarExpr:
		w.expr(info, e.X)
	case *ast.SliceExpr:
		w.expr(info, e.X)
	case *ast.IndexExpr:
		if t := info.TypeOf(e.X); t != nil {
			if _, isMap := t.Underlying().(*types.Map); isMap {
				return
			}
		}
		w.expr(info, e.X)
	case *ast.CompositeLit:
		for _, element := range e.Elts {
			w.expr(info, element)
		}
	case *ast.KeyValueExpr:
		w.expr(info, e.Key)
		w.expr(info, e.Value)
	case *ast.Ident:
		w.ident(info, e)
	case *ast.SelectorExpr:
		if info.Selections[e] == nil {
			w.ident(info, e.Sel)
		}
	case *ast.CallExpr:
		w.call(info, e)
	}
}

func (w *quotedFormWalk) ident(info *types.Info, id *ast.Ident) {
	obj := info.Uses[id]
	if quotedFormRefusedUse(obj) {
		w.hit = true
		return
	}
	if v, ok := obj.(*types.Var); ok && !v.IsField() {
		w.variable(v)
	}
}

func (w *quotedFormWalk) variable(v *types.Var) {
	if v.Pkg() == nil {
		return
	}
	if v.Parent() == v.Pkg().Scope() {
		if !strings.HasPrefix(v.Pkg().Path(), modulePrefix) {
			return
		}
		key := "var:" + objectKey(v)
		if w.seen[key] {
			return
		}
		w.seen[key] = true
		for _, value := range w.idx.pkgVars[objectKey(v)] {
			w.expr(value.info, value.expr)
		}
		return
	}
	if w.seen[v] {
		return
	}
	w.seen[v] = true
	for _, value := range w.idx.locals[v] {
		if pos, ok := w.idx.tuple[v][value.expr]; ok {
			w.valueAt(value.info, value.expr, pos)
			continue
		}
		w.expr(value.info, value.expr)
	}
	for _, value := range w.idx.ranges[v] {
		w.expr(value.info, value.expr)
	}
	for _, value := range w.idx.writes[v] {
		w.expr(value.info, value.expr)
	}
	if p, ok := w.idx.params[v]; ok {
		for _, c := range w.idx.callers[p.function] {
			args := c.call.Args
			switch {
			case p.variadic && p.position < len(args):
				for _, a := range args[p.position:] {
					w.expr(c.info, a)
				}
			case p.variadic:
			case p.position < len(args):
				w.expr(c.info, args[p.position])
			case len(args) == 1:
				w.expr(c.info, args[0])
			}
		}
	}
}

// valueAt follows the value at position pos of e, an expression that gives
// several values: the result of a call, or the value of the comma-ok form of
// an index, a type assertion or a receive (its second value is a bool).
func (w *quotedFormWalk) valueAt(info *types.Info, e ast.Expr, pos int) {
	if c, ok := ast.Unparen(e).(*ast.CallExpr); ok {
		w.callAt(info, c, pos)
		return
	}
	if pos == 0 {
		w.expr(info, e)
	}
}

func (w *quotedFormWalk) call(info *types.Info, c *ast.CallExpr) {
	w.callAt(info, c, -1)
}

// callAt follows a call; pos is the result the caller uses, or -1 for every
// result.
func (w *quotedFormWalk) callAt(info *types.Info, c *ast.CallExpr, pos int) {
	fun := ast.Unparen(c.Fun)
	switch f := fun.(type) {
	case *ast.IndexExpr:
		fun = ast.Unparen(f.X)
	case *ast.IndexListExpr:
		fun = ast.Unparen(f.X)
	}
	switch f := fun.(type) {
	case *ast.Ident:
		if quotedFormRefusedUse(info.Uses[f]) {
			w.hit = true
			return
		}
	case *ast.SelectorExpr:
		if info.Selections[f] == nil && quotedFormRefusedUse(info.Uses[f.Sel]) {
			w.hit = true
			return
		}
	}
	fn := quotedFormCallee(info, c)
	interfaceCall := false
	followed := false
	key := ""
	var mf moduleFunction
	if fn != nil {
		if recv := fn.Type().(*types.Signature).Recv(); recv != nil && types.IsInterface(recv.Type()) {
			interfaceCall = true
		}
		key = quotedFormFuncKey(fn)
		mf, followed = w.idx.funcs[key]
		followed = followed && !interfaceCall
	}
	// When the walk follows a call into its function, it follows an argument
	// of the call only through the parameter it is bound to. The exception is
	// a result that is a struct or a pointer to one: the function can store an
	// argument in it, and the walk follows the arguments of that call as well.
	// The walk follows the arguments of any other call directly: a call of a
	// function outside the module, a builtin, a conversion, or a call through
	// an interface or a function value.
	if !followed || quotedFormCarriesState(info.TypeOf(c), pos) {
		for _, a := range c.Args {
			w.expr(info, a)
		}
	}
	if fn == nil || interfaceCall {
		return
	}
	if sel, isSel := fun.(*ast.SelectorExpr); isSel && info.Selections[sel] != nil {
		if (!followed && quotedFormBuilderVar(info, sel.X) != nil) || quotedFormCommandType(info.TypeOf(sel.X)) {
			w.expr(info, sel.X)
		}
	}
	if !followed {
		return
	}
	seenKey := "func:" + key + "#" + strconv.Itoa(pos)
	if w.seen[seenKey] {
		return
	}
	w.seen[seenKey] = true
	results := fn.Type().(*types.Signature).Results().Len()
	ast.Inspect(mf.body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			switch {
			case len(n.Results) == 0:
				for i, res := range mf.results {
					if v, ok := res.(*types.Var); ok && (pos < 0 || pos == i) {
						w.variable(v)
					}
				}
			case pos >= 0 && len(n.Results) == results:
				w.expr(mf.info, n.Results[pos])
			case pos >= 0 && len(n.Results) == 1:
				w.valueAt(mf.info, n.Results[0], pos)
			default:
				for _, r := range n.Results {
					w.expr(mf.info, r)
				}
			}
		}
		return true
	})
}

// quotedFormVerbArguments returns the zero-based positions of the arguments the
// %q verbs and the %#v verbs (which print a string in Go syntax) of a constant
// format print. Flags, a width or precision of digits, a * (which takes an
// argument first), an argument index [n], and %% (which takes none) are read as
// fmt reads them.
func quotedFormVerbArguments(format string) []int {
	var out []int
	next := 0
	for i := 0; i < len(format); {
		if format[i] != '%' {
			i++
			continue
		}
		i++
		sharp := false
		for i < len(format) && strings.IndexByte("+-# 0", format[i]) >= 0 {
			sharp = sharp || format[i] == '#'
			i++
		}
		i = quotedFormArgumentIndex(format, i, &next)
		if i < len(format) && format[i] == '*' {
			next++
			i++
		} else {
			for i < len(format) && format[i] >= '0' && format[i] <= '9' {
				i++
			}
		}
		if i < len(format) && format[i] == '.' {
			i++
			i = quotedFormArgumentIndex(format, i, &next)
			if i < len(format) && format[i] == '*' {
				next++
				i++
			} else {
				for i < len(format) && format[i] >= '0' && format[i] <= '9' {
					i++
				}
			}
		}
		i = quotedFormArgumentIndex(format, i, &next)
		if i >= len(format) {
			break
		}
		verb, size := utf8.DecodeRuneInString(format[i:])
		i += size
		if verb == '%' {
			continue
		}
		if verb == 'q' || (verb == 'v' && sharp) {
			out = append(out, next)
		}
		next++
	}
	return out
}

// quotedFormArgumentIndex reads an argument index [n] at format[i] into next
// and returns the position after it.
func quotedFormArgumentIndex(format string, i int, next *int) int {
	if i >= len(format) || format[i] != '[' {
		return i
	}
	end := strings.IndexByte(format[i:], ']')
	if end < 0 {
		return i
	}
	if n, err := strconv.Atoi(format[i+1 : i+end]); err == nil && n >= 1 {
		*next = n - 1
	}
	return i + end + 1
}

// quotedFormStrconv holds the strconv quoting functions, which write a string
// as a Go-quoted string; the value is true for an Append form, which quotes
// its last argument (the first is the buffer it appends to). QuoteRune and its
// variants take a rune and are not sinks.
var quotedFormStrconv = map[string]bool{
	"Quote":                false,
	"QuoteToASCII":         false,
	"QuoteToGraphic":       false,
	"AppendQuote":          true,
	"AppendQuoteToASCII":   true,
	"AppendQuoteToGraphic": true,
}

// quotedFormIsStrconvQuote reports whether fn is one of quotedFormStrconv.
func quotedFormIsStrconvQuote(fn *types.Func) bool {
	if fn.Pkg() == nil || fn.Pkg().Path() != "strconv" {
		return false
	}
	_, ok := quotedFormStrconv[fn.Name()]
	return ok
}

// quotedFormSinkArguments returns the arguments a call prints in Go-quoted
// form: the string quoted by a strconv quoting function and, for a
// printf-family call, the arguments the %q and %#v verbs of a constant format
// print, or every argument after a format that is not constant. sink is false
// for any other call. A format is constant when the type information holds a
// constant value for it.
func quotedFormSinkArguments(info *types.Info, call *ast.CallExpr) (args []ast.Expr, sink, constantFormat bool) {
	if fn := quotedFormCallee(info, call); fn != nil && quotedFormIsStrconvQuote(fn) && len(call.Args) > 0 {
		if quotedFormStrconv[fn.Name()] {
			return call.Args[len(call.Args)-1:], true, true
		}
		return call.Args[:1], true, true
	}
	i, _ := formatArgument(info, call)
	if i < 0 || i >= len(call.Args) {
		return nil, false, false
	}
	rest := call.Args[i+1:]
	tv, ok := info.Types[call.Args[i]]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return rest, len(rest) > 0, false
	}
	for _, p := range quotedFormVerbArguments(constant.StringVal(tv.Value)) {
		switch {
		case call.Ellipsis.IsValid() && len(rest) > 0:
			args = append(args, rest[len(rest)-1])
		case p < len(rest):
			args = append(args, rest[p])
		}
	}
	return args, len(args) > 0, true
}

// quotedFormFinding is one finding: the function it is in, its position
// (module-relative file:line) and the source text of the quoted argument.
type quotedFormFinding struct {
	function string
	position string
	argument string
	// valueUse is set for a strconv quoting function used as a value instead
	// of called; argument is then its name.
	valueUse bool
}

// quotedFormResult is what the scan of one load found: a map from position
// and argument to each finding, and a map from the position of each sink to
// the function it is in.
type quotedFormResult struct {
	findings    map[string]quotedFormFinding
	sinks       map[string]string
	nonConstant map[string]string
}

func newQuotedFormResult() quotedFormResult {
	return quotedFormResult{findings: map[string]quotedFormFinding{}, sinks: map[string]string{}, nonConstant: map[string]string{}}
}

// quotedFormScan reads the non-test files of pkgs and reports a finding for
// each quoted argument of a sink that reaches a listed name. It also reports a
// finding for each use of a strconv quoting function as a value (passed,
// assigned or returned, not called), wherever it is: the scan cannot tell
// where such a function value is used later.
func quotedFormScan(pkgs []*packages.Package) quotedFormResult {
	idx := newQuotedFormIndex(pkgs)
	res := newQuotedFormResult()
	for _, pkg := range pkgs {
		info := pkg.TypesInfo
		rel := strings.TrimPrefix(pkg.PkgPath, modulePrefix)
		position := func(pos token.Pos) string {
			p := pkg.Fset.Position(pos)
			return rel + "/" + filepath.ToSlash(filepath.Base(p.Filename)) + ":" + strconv.Itoa(p.Line)
		}
		for _, f := range pkg.Syntax {
			for _, decl := range f.Decls {
				var root ast.Node
				function := rel + ".<package level>"
				switch d := decl.(type) {
				case *ast.FuncDecl:
					fn, ok := info.Defs[d.Name].(*types.Func)
					if d.Body == nil || !ok {
						continue
					}
					root, function = d.Body, funcKey(fn)
				case *ast.GenDecl:
					if d.Tok != token.VAR {
						continue
					}
					root = d
				default:
					continue
				}
				report := func(at string, args []ast.Expr) {
					for _, a := range args {
						if quotedFormReaches(idx, info, a) {
							text := types.ExprString(a)
							res.findings[at+" "+text] = quotedFormFinding{function, at, text, false}
						}
					}
				}
				callees := map[*ast.Ident]bool{}
				ast.Inspect(root, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					switch fun := ast.Unparen(call.Fun).(type) {
					case *ast.Ident:
						callees[fun] = true
					case *ast.SelectorExpr:
						callees[fun.Sel] = true
					}
					args, sink, constantFormat := quotedFormSinkArguments(info, call)
					if !sink {
						return true
					}
					at := position(call.Pos())
					res.sinks[at] = function
					if !constantFormat {
						res.nonConstant[at] = function
					}
					report(at, args)
					return true
				})
				ast.Inspect(root, func(n ast.Node) bool {
					id, ok := n.(*ast.Ident)
					if !ok || callees[id] {
						return true
					}
					if fn, ok := info.Uses[id].(*types.Func); ok && quotedFormIsStrconvQuote(fn) {
						at := position(id.Pos())
						name := fn.Pkg().Name() + "." + fn.Name()
						res.sinks[at] = function
						res.findings[at+" "+name] = quotedFormFinding{function, at, name, true}
					}
					return true
				})
			}
		}
	}
	return res
}

// quotedFormCheck compares the findings with the allowlist and returns one
// message for each difference: a function with no entry, a count that differs
// from the entry's, a quoted argument the entry does not name, an argument the
// entry names that no finding quotes, and an entry that matches nothing.
func quotedFormCheck(findings map[string]quotedFormFinding, allow map[string]quotedFormAllowance) []string {
	byFunction := map[string][]quotedFormFinding{}
	for _, f := range findings {
		byFunction[f.function] = append(byFunction[f.function], f)
	}
	var problems []string
	for _, function := range slices.Sorted(maps.Keys(byFunction)) {
		found := byFunction[function]
		slices.SortFunc(found, func(a, b quotedFormFinding) int {
			return strings.Compare(a.position+" "+a.argument, b.position+" "+b.argument)
		})
		entry, ok := allow[function]
		if !ok {
			for _, f := range found {
				if f.valueUse {
					problems = append(problems, fmt.Sprintf("%s: %s uses %s as a value without calling it (passed, assigned or returned); call it, because the scan can check only a call", f.position, function, f.argument))
					continue
				}
				problems = append(problems, fmt.Sprintf("%s: %s prints %s in Go-quoted form, and its value comes from a name listed in quotedFormRefused (clierr.ShellQuote, clierr.Rancher or the command builder, for example); print it with %%s, because a shell reads a Go-quoted string differently from the text clierr.ShellQuote writes; if the quoted text holds no command, add an entry to quotedFormAllowlist", f.position, function, f.argument))
			}
			continue
		}
		if entry.count != len(found) {
			var where []string
			for _, f := range found {
				where = append(where, f.position+" "+f.argument)
			}
			problems = append(problems, fmt.Sprintf("%s: the allowlist covers %d finding(s), the scan found %d: %s; if a quoted text holds a command, print it with %%s; otherwise set the count of the entry to the number the scan found", function, entry.count, len(found), strings.Join(where, ", ")))
		}
		named := map[string]bool{}
		for _, f := range found {
			named[f.argument] = true
			if !slices.Contains(entry.arguments, f.argument) {
				problems = append(problems, fmt.Sprintf("%s: %s quotes %s, which the allowlist entry does not name; print it with %%s, or add it to the entry if the quoted text holds no command", f.position, function, f.argument))
			}
		}
		for _, argument := range entry.arguments {
			if !named[argument] {
				problems = append(problems, fmt.Sprintf("allowlist entry %s names the argument %s, which no finding quotes: remove it", function, argument))
			}
		}
	}
	for _, function := range slices.Sorted(maps.Keys(allow)) {
		if len(byFunction[function]) == 0 {
			problems = append(problems, fmt.Sprintf("allowlist entry %s matched nothing: remove it", function))
		}
	}
	return problems
}

// quotedFormLoads scans the module for the platform the test runs on and for
// each platform in quotedFormOtherGOOS other than that one, once per process.
// It returns a map from "host" (the platform the test runs on) or the GOOS of
// another platform to the result of that load.
var quotedFormLoads = sync.OnceValues(func() (map[string]quotedFormResult, error) {
	pkgs, err := fieldManagerPackages()
	if err != nil {
		return nil, err
	}
	out := map[string]quotedFormResult{"host": quotedFormScan(pkgs)}
	for _, goos := range quotedFormOtherGOOS {
		other, err := otherGOOSPackages(goos)
		if err != nil {
			return nil, err
		}
		if other != nil {
			out[goos] = quotedFormScan(other)
		}
	}
	return out, nil
})

// quotedFormMerged merges the results of the loads by position: a file that
// several loads read counts once.
func quotedFormMerged(t *testing.T) quotedFormResult {
	t.Helper()
	loads, err := quotedFormLoads()
	require.NoError(t, err)
	merged := newQuotedFormResult()
	for _, res := range loads {
		maps.Copy(merged.findings, res.findings)
		maps.Copy(merged.sinks, res.sinks)
		maps.Copy(merged.nonConstant, res.nonConstant)
	}
	return merged
}

// TestQuotedFormScan asserts that no Go-quoted form in the non-test files of
// the module (a strconv quoting function, or a %q or %#v verb of a
// printf-family call) prints a value that reaches clierr.ShellQuote,
// clierr.shellQuoteArgs or clierr.Rancher, and that no strconv quoting
// function is used as a value, wherever it is. A command is not written
// inside a Go-quoted string: a POSIX shell reads a dollar sign, a backquote
// and a backslash in it as it would in double quotes. The scan reads the files
// of every package of the module for the platform the test runs on and for
// windows and linux, and merges the findings of the loads by position. A
// function with a finding although its quoted text holds no command is in
// quotedFormAllowlist with the number of findings it covers and the quoted
// arguments it names. The test fails for a different count, for an argument
// the entry does not name and for an entry that matches nothing.
//
// Limits: the scan does not check these cases.
//   - A struct field that holds a command and is printed with %q (a struct
//     field read is a leaf; none is printed today).
//   - A value written into a parameter: through a pointer (fill(&s, n) with
//     *dst = clierr.ShellQuote(n), then s printed in Go-quoted form), into a
//     slice or a map, appended through a pointer to a slice, or sent on a
//     channel. The exception is a struct, or a pointer to one, that a call of
//     a function of the module returns: the walk follows the arguments of that
//     call, which covers a function that stores its argument in the struct it
//     returns.
//   - A command built from a string literal such as "rancher get " + name: the
//     scan checks only the listed names.
//   - The walk does not follow into: a call of a function value other than a
//     strconv quoting function (which is reported when it is used as a value),
//     a call through an interface, a function literal called in place, a
//     function literal held by a package-level variable or passed as an
//     argument, a strings.Builder or bytes.Buffer passed to another function,
//     and a call of a function outside the module.
func TestQuotedFormScan(t *testing.T) {
	t.Parallel()
	merged := quotedFormMerged(t)
	for _, problem := range quotedFormCheck(merged.findings, quotedFormAllowlist) {
		t.Error(problem)
	}
	t.Logf("%d sinks, %d with a format that is not constant, %d findings", len(merged.sinks), len(merged.nonConstant), len(merged.findings))
}

// TestQuotedFormRefusedNames asserts the listed names are exactly the names
// in the list below: a name cannot be added or removed without editing both.
func TestQuotedFormRefusedNames(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{
		"internal/clierr.DirectoryFree",
		"internal/clierr.FileCommand",
		"internal/clierr.FileCommandIn",
		"internal/clierr.FileRevokeKubeconfig",
		"internal/clierr.FlagFragment",
		"internal/clierr.FlagFragmentAfterWord",
		"internal/clierr.FlagFragmentText",
		"internal/clierr.FlagFragmentWord",
		"internal/clierr.MarkRemedy",
		"internal/clierr.NewCommand",
		"internal/clierr.NewSequence",
		"internal/clierr.NewTypedLine",
		"internal/clierr.Rancher",
		"internal/clierr.RemedyMarker",
		"internal/clierr.Revoke",
		"internal/clierr.RevokeKubeconfig",
		"internal/clierr.ShellQuote",
		"internal/clierr.ShellWord",
		"internal/clierr.Text",
		"internal/clierr.shellQuoteArgs",
	}, slices.Sorted(maps.Keys(quotedFormRefused)), "the keys of quotedFormRefused and this list differ: edit both together")
}

// TestQuotedFormRefusedNamesExist asserts every listed name is a function or a
// variable declared in internal/clierr: a name misspelled in both lists would
// match no code.
func TestQuotedFormRefusedNamesExist(t *testing.T) {
	t.Parallel()
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	var clierrPackage *packages.Package
	for _, pkg := range pkgs {
		if pkg.PkgPath == modulePrefix+"internal/clierr" {
			clierrPackage = pkg
		}
	}
	require.NotNil(t, clierrPackage)
	for key := range quotedFormRefused {
		name, ok := strings.CutPrefix(key, "internal/clierr.")
		require.True(t, ok, "%s does not start with internal/clierr.; write the name as internal/clierr.<name> in quotedFormRefused", key)
		switch clierrPackage.Types.Scope().Lookup(name).(type) {
		case *types.Func, *types.Var:
		default:
			t.Errorf("%s is not a function or variable declared in internal/clierr; fix the spelling in quotedFormRefused", key)
		}
	}
}

// TestQuotedFormPlatformLoads asserts the scan reads the module for the
// platform the test runs on and for each of windows and linux, except that the
// platform the test runs on gets one load only (otherGOOSPackages returns
// nothing for it). A scan of fewer loads would miss the files that the build
// constraints of the platform the test runs on leave out. At most one of
// windows and linux is the platform the test runs on, which leaves at least
// one other load.
func TestQuotedFormPlatformLoads(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"windows", "linux"}, quotedFormOtherGOOS)
	loads, err := quotedFormLoads()
	require.NoError(t, err)
	want := []string{"host"}
	for _, goos := range []string{"windows", "linux"} {
		other, err := otherGOOSPackages(goos)
		require.NoError(t, err)
		if other != nil {
			want = append(want, goos)
		}
	}
	assert.ElementsMatch(t, want, slices.Collect(maps.Keys(loads)))
	assert.Contains(t, slices.Collect(maps.Keys(loads)), "host")
	assert.GreaterOrEqual(t, len(loads), 2, "the platform the test runs on and at least one other platform")
}

// TestQuotedFormKnownSites asserts the scan finds the sinks the module is
// known to hold, by function: a scan that finds none fails.
func TestQuotedFormKnownSites(t *testing.T) {
	t.Parallel()
	merged := quotedFormMerged(t)
	counts := map[string]int{}
	for _, function := range merged.sinks {
		counts[function]++
	}
	for function, want := range map[string]int{
		"internal/clierr.IDText":                          1,
		"internal/runtime.OnceOnlyLine":                   1,
		"internal/token.successTextID":                    1,
		"internal/engine/extres.kubeconfigHeader":         2,
		"internal/commands/auth.decorateDelegatedFailure": 1,
	} {
		assert.Equal(t, want, counts[function], "the sinks found in %s", function)
	}
}

// quotedFormFixtures type-checks the fixture packages once, by package name.
var quotedFormFixtures = sync.OnceValues(func() (map[string]*packages.Package, error) {
	pkgs, err := loadPackages("./"+quotedFormDir+"/...", "")
	if err != nil {
		return nil, err
	}
	out := map[string]*packages.Package{}
	for _, pkg := range pkgs {
		out[path.Base(pkg.PkgPath)] = pkg
	}
	return out, nil
})

// quotedFormPlatformFixtures type-checks the platform fixture once for each of
// windows and linux.
var quotedFormPlatformFixtures = map[string]func() ([]*packages.Package, error){
	"windows": sync.OnceValues(func() ([]*packages.Package, error) {
		return loadPackages("./"+quotedFormDir+"/platform", "windows")
	}),
	"linux": sync.OnceValues(func() ([]*packages.Package, error) {
		return loadPackages("./"+quotedFormDir+"/platform", "linux")
	}),
}

// quotedFormHostPlatform returns the functions of the platform fixture in the
// files that a build on the platform the test runs on includes: the windows
// file and the linux file hold one finding each, and a build on any other
// platform includes neither.
func quotedFormHostPlatform(pkg *packages.Package) []string {
	var out []string
	for _, file := range pkg.GoFiles {
		switch filepath.Base(file) {
		case "platform_windows.go":
			out = append(out, "quotedOnWindows")
		case "platform_linux.go":
			out = append(out, "quotedOnLinux")
		}
	}
	return out
}

// quotedFormCases lists every fixture package and the functions the scan
// reports a finding in.
var quotedFormCases = []struct {
	fixture string
	refused []string
}{
	{"direct", []string{"quoted"}},
	{"ranchermarker", []string{"quoted"}},
	{"localvar", []string{"quoted"}},
	{"funcreturn", []string{"quotedFunction", "quotedMethod", "quotedNamedResult"}},
	{"parameter", []string{"closure", "show"}},
	{"collected", []string{"bufferFprintf", "bufferWrite", "builderWriteString", "sliceAppend", "sliceIndex", "sliceLiteral"}},
	{"calls", []string{"nested", "trimmed"}},
	{"cycle", []string{"recursive", "selfReference"}},
	{"verbforms", []string{"indexed", "percent", "plus", "sharp", "star"}},
	{"variableformat", []string{"show"}},
	{"packagevar", []string{"assigned", "initializer"}},
	{"platform", nil},
	{"rangevar", []string{"literal", "local"}},
	{"tuple", []string{"quotedCommandFirst", "quotedNamedCommand"}},
	{"sinkforms", []string{"<package level>", "appendASCII", "appendGraphic", "appendQuote", "asValue", "assignedValue", "indexedSharpV", "passed", "passedToHelper", "plusSharpV", "quoter", "sharpV", "toASCII", "toGraphic"}},
	{"twovalues", []string{"two"}},
	{"moduleargs", []string{"quotedReturning"}},
	{"builder", []string{"chained", "directoryFree", "fileCommand", "fileCommandIn", "fileRevokeKubeconfig", "fragment", "fragmentAfterWord", "fragmentText", "fragmentWord", "localCommand", "markRemedy", "newCommand", "remedyMarker", "revoke", "revokeKubeconfig", "sequence", "shellWord", "text", "typedLine"}},
	{"setters", []string{"quotedPointerSetter", "quotedSetter"}},
	{"accepted", nil},
}

// quotedFormRefusedFunctions returns the names, within the package, of the
// functions a scan of pkgs reports a finding in.
func quotedFormRefusedFunctions(pkgs []*packages.Package) []string {
	var names []string
	seen := map[string]bool{}
	for _, f := range quotedFormScan(pkgs).findings {
		if !seen[f.function] {
			seen[f.function] = true
			names = append(names, f.function[strings.LastIndex(f.function, ".")+1:])
		}
	}
	slices.Sort(names)
	return names
}

// TestQuotedFormScanFixtures asserts the scan on type-checked fixture packages.
//   - It reports a finding when the quoted value is built from a listed name
//     through: a direct call, clierr.Rancher, a local variable, the return of
//     a function, a method or a named result, a parameter, a strings.Builder
//     or bytes.Buffer, an index assignment, an append, a literal, a call
//     inside the argument, a cycle, a parameter of a function literal held by
//     a variable, a package-level variable, a variable of a range, the command
//     result of a call with several results, and a function that stores a
//     command in the struct it returns. It also reports a finding for each
//     form of verb (%q, %+q, %#q, %#v, %+#v, %#[2]v), each strconv quoting
//     function and a format that is not constant.
//   - It reports a finding in the files of another platform when that
//     platform is loaded.
//   - It reports a finding for a strconv quoting function used as a value in
//     a package-level variable, a returned function, an argument and a local
//     variable.
//   - It follows the arguments of a function of the module only through the
//     result the function returns (the moduleargs fixture).
//   - It reports no finding for: a struct field, a map lookup, a function
//     value, an interface call, a function literal called in place, the
//     stated limits (a value written through a pointer parameter, into a
//     slice or a map, appended through a pointer to a slice or sent on a
//     channel, a function literal held by a package-level variable or passed
//     as an argument), the buffer an Append form appends to, a rune quoted by
//     QuoteRune, the plain result of a call whose other result holds a
//     command, a type assertion, a type switch binding, a constant format
//     that maps %q to another argument, a plain parameter, a command written
//     with %s and a module function that ignores its argument.
func TestQuotedFormScanFixtures(t *testing.T) {
	t.Parallel()
	pkgs, err := quotedFormFixtures()
	require.NoError(t, err)
	for _, tc := range quotedFormCases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			pkg, ok := pkgs[tc.fixture]
			require.True(t, ok, "fixture package %s", tc.fixture)
			want := tc.refused
			if tc.fixture == "platform" {
				want = quotedFormHostPlatform(pkg)
			}
			assert.Equal(t, want, quotedFormRefusedFunctions([]*packages.Package{pkg}))
		})
	}
}

// TestQuotedFormScanPlatformFixtures asserts the files of another platform are
// read when that platform is loaded: the windows file and the linux file of
// the platform fixture each hold one finding, found in that file.
func TestQuotedFormScanPlatformFixtures(t *testing.T) {
	t.Parallel()
	for goos, file := range map[string]string{"windows": "platform_windows.go", "linux": "platform_linux.go"} {
		t.Run(goos, func(t *testing.T) {
			t.Parallel()
			pkgs, err := quotedFormPlatformFixtures[goos]()
			require.NoError(t, err)
			findings := quotedFormScan(pkgs).findings
			require.Len(t, findings, 1, "%v", findings)
			for _, f := range findings {
				assert.Contains(t, f.position, "/"+file+":")
			}
		})
	}
}

// TestQuotedFormFindingArguments asserts a finding names the quoted argument
// that reaches a listed name: the call with three quoted arguments has one
// sink and two findings, one for each argument that reaches a listed name, and
// none for the plain one.
func TestQuotedFormFindingArguments(t *testing.T) {
	t.Parallel()
	pkgs, err := quotedFormFixtures()
	require.NoError(t, err)
	pkg, ok := pkgs["twovalues"]
	require.True(t, ok)
	var arguments []string
	for _, f := range quotedFormScan([]*packages.Package{pkg}).findings {
		arguments = append(arguments, f.argument)
	}
	assert.ElementsMatch(t, []string{"clierr.ShellQuote(name)", "clierr.Rancher"}, arguments)
}

// TestQuotedFormFixturesAreAllListed asserts every fixture package on disk is
// in the table above.
func TestQuotedFormFixturesAreAllListed(t *testing.T) {
	t.Parallel()
	pkgs, err := quotedFormFixtures()
	require.NoError(t, err)
	var listed, loaded []string
	for _, tc := range quotedFormCases {
		listed = append(listed, tc.fixture)
	}
	for name := range pkgs {
		loaded = append(loaded, name)
	}
	assert.ElementsMatch(t, listed, loaded)
}

// TestQuotedFormAllowlistRules asserts the allowlist check on the findings of
// the rangevar fixture, which has findings in two functions. An entry covers
// its function, its count and the quoted arguments it names. The check fails
// for: a different count, an empty allowlist when a function has a finding, an
// entry for a function with no finding (the entry matched nothing), a quoted
// argument the entry does not name, an argument the entry names that no
// finding quotes, and a second quoted argument added to a call of an
// allowlisted function.
func TestQuotedFormAllowlistRules(t *testing.T) {
	t.Parallel()
	pkgs, err := quotedFormFixtures()
	require.NoError(t, err)
	pkg, ok := pkgs["rangevar"]
	require.True(t, ok)
	findings := quotedFormScan([]*packages.Package{pkg}).findings
	require.Len(t, findings, 2, "%v", findings)
	base := quotedFormDir + "/rangevar."
	var literalAt string
	for _, f := range findings {
		if f.function == base+"literal" {
			literalAt = f.position
		}
	}
	require.NotEmpty(t, literalAt)
	entry := func(count int, arguments ...string) quotedFormAllowance {
		return quotedFormAllowance{count, arguments, "reason"}
	}
	both := func(literal, local quotedFormAllowance) map[string]quotedFormAllowance {
		return map[string]quotedFormAllowance{base + "literal": literal, base + "local": local}
	}
	secondValue := quotedFormFinding{base + "literal", literalAt, "clierr.Rancher", false}
	cases := []struct {
		name  string
		extra []quotedFormFinding
		allow map[string]quotedFormAllowance
		want  []string
	}{
		{"count 1 and the argument named for each function passes", nil, both(entry(1, "c"), entry(1, "c")), nil},
		{"count 0 fails", nil, both(entry(0, "c"), entry(1, "c")), []string{base + "literal: the allowlist covers 0 finding(s), the scan found 1"}},
		{"count 2 fails", nil, both(entry(2, "c"), entry(1, "c")), []string{base + "literal: the allowlist covers 2 finding(s), the scan found 1"}},
		{"an empty allowlist with a function that has a finding fails", nil, nil, []string{base + "literal", base + "local"}},
		{"an entry that matched nothing fails", nil, map[string]quotedFormAllowance{base + "literal": entry(1, "c"), base + "local": entry(1, "c"), base + "plain": entry(1, "c")}, []string{"allowlist entry " + base + "plain matched nothing"}},
		{"an argument the entry does not name fails", nil, both(entry(1, "d"), entry(1, "c")), []string{"quotes c, which the allowlist entry does not name", "names the argument d, which no finding quotes"}},
		{"a second quoted argument on a call of an allowlisted function fails", []quotedFormFinding{secondValue}, both(entry(1, "c"), entry(1, "c")), []string{"covers 1 finding(s), the scan found 2", "quotes clierr.Rancher, which the allowlist entry does not name"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			all := maps.Clone(findings)
			for _, f := range tc.extra {
				all[f.position+" "+f.argument] = f
			}
			problems := quotedFormCheck(all, tc.allow)
			require.Len(t, problems, len(tc.want), "%v", problems)
			for i, want := range tc.want {
				assert.Contains(t, problems[i], want)
			}
		})
	}
}
