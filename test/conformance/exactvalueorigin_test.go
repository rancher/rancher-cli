package conformance

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// evOriginStart is a builder entry point: the indexes of its value arguments
// (-1 for every argument) and the field of the argument that is the value.
type evOriginStart struct {
	args []int
	path string
}

// evOriginStarts are the builder entry points whose values guard 3a walks
// back from.
var evOriginStarts = map[string]evOriginStart{
	"internal/clierr.Command.Arg":           {[]int{-1}, ""},
	"internal/clierr.Command.FlagValue":     {[]int{1}, ""},
	"internal/clierr.FileCommandIn":         {[]int{0}, ""},
	"internal/clierr.Revoke":                {[]int{0}, "Name"},
	"internal/clierr.RevokeKubeconfig":      {[]int{0}, ""},
	"internal/clierr.FileRevokeKubeconfig":  {[]int{0}, ""},
	"internal/clierr.FlagFragment":          {[]int{1}, ""},
	"internal/clierr.FlagFragmentWord":      {[]int{1}, ""},
	"internal/clierr.FlagFragmentAfterWord": {[]int{2}, ""},
	"internal/clierr.ShellWord":             {[]int{0}, ""},
	"internal/clierr.TypedLine.Bare":        {[]int{0}, ""},
	"internal/clierr.TypedLine.Value":       {[]int{0}, ""},
	"internal/clierr.TypedLine.Joined":      {[]int{0, 1}, ""},
}

// evOriginWalk follows each value argument of the builder back to its
// sources.
type evOriginWalk struct {
	p     *evProgram
	r     *evResults
	allow evAllow
	seen  map[string]bool
}

// scanOrigins is guard 3a: from each value argument of a builder entry
// point, outside the builder's own file, back to its sources.
func (p *evProgram) scanOrigins(r *evResults, allow evAllow) {
	w := &evOriginWalk{p: p, r: r, allow: allow, seen: map[string]bool{}}
	for _, f := range p.files {
		if f.path == evBuilderFile {
			continue
		}
		ast.Inspect(f.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			key, _ := evCallee(f.info, call)
			start, ok := evOriginStarts[key]
			if !ok {
				return true
			}
			for _, i := range start.args {
				switch {
				case i < 0:
					for _, a := range call.Args {
						w.walk(f, a, start.path, 0, 0)
					}
				case i < len(call.Args):
					w.walk(f, call.Args[i], start.path, 0, 0)
				}
			}
			return true
		})
	}
}

func (w *evOriginWalk) report(f *evFile, n ast.Node, key, msg string) {
	f.finding(w.r, "3a", n, key, msg)
}

// evPathOf is a variable's name with the field path the walk follows.
func evPathOf(name, path string) string {
	if path == "" {
		return name
	}
	return name + "." + path
}

func evJoinPath(head, rest string) string {
	if rest == "" {
		return head
	}
	return head + "." + rest
}

// walk follows e, of which path names the part that becomes the value, back
// to its sources. result is the index of the value among the results of a
// call that returns several.
func (w *evOriginWalk) walk(f *evFile, e ast.Expr, path string, depth, result int) {
	if e == nil || depth > 40 {
		return
	}
	e = ast.Unparen(e)
	if tv, ok := f.info.Types[e]; ok && tv.Value != nil {
		return
	}
	switch x := e.(type) {
	case *ast.Ident:
		w.ident(f, x, path, depth)
	case *ast.SelectorExpr:
		if sel, ok := f.info.Selections[x]; ok {
			if sel.Kind() == types.FieldVal {
				w.walk(f, x.X, evJoinPath(x.Sel.Name, path), depth+1, 0)
			}
			return
		}
		w.ident(f, x.Sel, path, depth)
	case *ast.IndexExpr:
		w.walk(f, x.X, path, depth+1, 0)
	case *ast.SliceExpr:
		if evIsStringType(f.info.TypeOf(x.X)) {
			w.report(f, x, "slice", "a slice of a string changes the value before the command builder")
			return
		}
		w.walk(f, x.X, path, depth+1, 0)
	case *ast.StarExpr:
		w.walk(f, x.X, path, depth+1, 0)
	case *ast.UnaryExpr:
		if x.Op == token.AND {
			w.walk(f, x.X, path, depth+1, 0)
		}
	case *ast.TypeAssertExpr:
		w.walk(f, x.X, path, depth+1, 0)
	case *ast.CompositeLit:
		w.composite(f, x, path, depth)
	case *ast.BinaryExpr:
		if evIsStringType(f.info.TypeOf(x)) {
			w.report(f, x, "+", "a concatenation changes the value before the command builder")
		}
	case *ast.CallExpr:
		w.call(f, x, path, depth, result)
	}
}

// composite follows the field path into a struct literal, or every element
// of a slice, array or map literal.
func (w *evOriginWalk) composite(f *evFile, x *ast.CompositeLit, path string, depth int) {
	t := f.info.TypeOf(x)
	if t == nil {
		return
	}
	u := t.Underlying()
	if p, ok := u.(*types.Pointer); ok {
		u = p.Elem().Underlying()
	}
	switch u := u.(type) {
	case *types.Struct:
		if path == "" {
			return
		}
		head, rest, _ := strings.Cut(path, ".")
		for i, el := range x.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if id, ok := kv.Key.(*ast.Ident); ok && id.Name == head {
					w.walk(f, kv.Value, rest, depth+1, 0)
				}
				continue
			}
			if i < u.NumFields() && u.Field(i).Name() == head {
				w.walk(f, el, rest, depth+1, 0)
			}
		}
	case *types.Slice, *types.Array, *types.Map:
		for _, el := range x.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				el = kv.Value
			}
			w.walk(f, el, path, depth+1, 0)
		}
	}
}

// call accepts a conversion, append, a call that returns no string, a call
// that takes no string that is not constant (a source) and an allowlisted
// call, and reports any other call that returns a string.
func (w *evOriginWalk) call(f *evFile, x *ast.CallExpr, path string, depth, result int) {
	info := f.info
	if tv, ok := info.Types[x.Fun]; ok && tv.IsType() {
		for _, a := range x.Args {
			w.walk(f, a, path, depth+1, 0)
		}
		return
	}
	switch b := evBuiltin(info, x); b {
	case "":
	case "make", "new", "len", "cap":
		return
	case "append":
		for _, a := range x.Args {
			w.walk(f, a, path, depth+1, 0)
		}
		return
	default:
		if evStringish(info.TypeOf(x)) {
			w.report(f, x, b, "the builtin "+b+" changes the value before the command builder")
		}
		return
	}
	key, _ := evCallee(info, x)
	if _, ok := w.allow.originCallees[key]; ok && key != "" {
		w.r.used["callee:"+key] = true
		return
	}
	if _, ok := w.allow.originThrough[key]; ok && key != "" {
		w.r.used["through:"+key] = true
		for _, a := range x.Args {
			w.walk(f, a, path, depth+1, 0)
		}
		return
	}
	rt := info.TypeOf(x)
	if tup, ok := rt.(*types.Tuple); ok {
		if result >= tup.Len() {
			return
		}
		rt = tup.At(result).Type()
	}
	if !evStringish(rt) {
		return
	}
	transform := false
	for _, a := range x.Args {
		tv, ok := info.Types[a]
		if !ok || tv.Value != nil {
			continue
		}
		if tup, ok := tv.Type.(*types.Tuple); ok {
			for v := range tup.Variables() {
				transform = transform || evStringish(v.Type())
			}
			continue
		}
		transform = transform || evStringish(tv.Type)
	}
	if sel, ok := ast.Unparen(x.Fun).(*ast.SelectorExpr); ok {
		if s, ok := info.Selections[sel]; ok && s.Kind() == types.MethodVal && evStringish(info.TypeOf(sel.X)) {
			transform = true
		}
	}
	if !transform {
		return
	}
	name := key
	if name == "" {
		name = types.ExprString(x.Fun)
	}
	w.report(f, x, name, "the call "+name+" returns a string made from another string before the command builder")
}

// ident follows a package-level variable to its initializer, a parameter to
// every direct call site, and a local variable to every value assigned to
// it.
func (w *evOriginWalk) ident(f *evFile, id *ast.Ident, path string, depth int) {
	v, ok := f.info.Uses[id].(*types.Var)
	if !ok || v.Pkg() == nil || v.IsField() {
		return
	}
	if v.Parent() == v.Pkg().Scope() {
		key := "var:" + objectKey(v) + "|" + path
		if w.seen[key] {
			return
		}
		w.seen[key] = true
		if d, ok := w.p.vars[objectKey(v)]; ok {
			w.walk(d.f, d.value, path, depth+1, 0)
		}
		return
	}
	key := fmt.Sprintf("%s:%d|%s", f.path, v.Pos(), path)
	if w.seen[key] {
		return
	}
	w.seen[key] = true
	root, fnKey := f.enclosing(id)
	if fd, ok := root.(*ast.FuncDecl); ok {
		w.param(f, fd, fnKey, v, path, depth)
	}
	w.assignments(f, root, v, path, depth)
}

// evParamIndex is the index of v among the parameters of ft, and whether it
// is the variadic one; -1 when v is none of them.
func evParamIndex(info *types.Info, ft *ast.FuncType, v *types.Var) (int, bool) {
	i := 0
	for _, fld := range ft.Params.List {
		if len(fld.Names) == 0 {
			i++
			continue
		}
		for _, name := range fld.Names {
			if info.Defs[name] == v {
				_, variadic := fld.Type.(*ast.Ellipsis)
				return i, variadic
			}
			i++
		}
	}
	return -1, false
}

// param follows a receiver or a parameter of fd, or of a function literal
// inside it, to the call sites. It reports false when v is neither.
func (w *evOriginWalk) param(f *evFile, fd *ast.FuncDecl, fnKey string, v *types.Var, path string, depth int) bool {
	idx, variadic := evParamIndex(f.info, fd.Type, v)
	if fd.Recv != nil && idx < 0 {
		for _, fld := range fd.Recv.List {
			for _, name := range fld.Names {
				if f.info.Defs[name] == v {
					idx = -2
				}
			}
		}
	}
	if idx == -1 {
		inLit := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if lit, ok := n.(*ast.FuncLit); ok {
				if i, _ := evParamIndex(f.info, lit.Type, v); i >= 0 {
					inLit = true
				}
			}
			return !inLit
		})
		if !inLit {
			return false
		}
		if _, ok := w.allow.originParams[fnKey]; ok {
			w.r.used["param:"+fnKey] = true
			return true
		}
		w.report(f, fd.Name, "param", "the parameter "+evPathOf(v.Name(), path)+" of a function literal in "+fd.Name.Name+" reaches the command builder; the walk cannot follow a function value")
		return true
	}
	fn, _ := f.info.Defs[fd.Name].(*types.Func)
	if w.cannotFollow(fn, fnKey) {
		if _, ok := w.allow.originParams[fnKey]; ok {
			w.r.used["param:"+fnKey] = true
			return true
		}
		w.report(f, fd.Name, "param", "the parameter "+evPathOf(v.Name(), path)+" of "+fd.Name.Name+" reaches the command builder, and "+fd.Name.Name+" is called through a function value or an interface method, which the walk cannot follow")
		return true
	}
	for _, site := range w.p.calls[fnKey] {
		args := site.call.Args
		switch {
		case idx == -2:
			if sel, ok := ast.Unparen(site.call.Fun).(*ast.SelectorExpr); ok {
				w.walk(site.f, sel.X, path, depth+1, 0)
			}
		case variadic && site.call.Ellipsis == token.NoPos:
			for j := idx; j < len(args); j++ {
				w.walk(site.f, args[j], path, depth+1, 0)
			}
		case len(args) == 1 && idx > 0:
			w.walk(site.f, args[0], path, depth+1, idx)
		case idx < len(args):
			w.walk(site.f, args[idx], path, depth+1, 0)
		}
	}
	return true
}

// cannotFollow reports whether fn is used as a value, or is a method an
// interface method call can reach.
func (w *evOriginWalk) cannotFollow(fn *types.Func, key string) bool {
	if w.p.valueUses[key] > 0 {
		return true
	}
	if fn == nil {
		return false
	}
	recv := fn.Type().(*types.Signature).Recv()
	if recv == nil {
		return false
	}
	t := recv.Type()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	return slices.ContainsFunc(w.p.ifaceCalls[fn.Name()], func(iface *types.Interface) bool { return evImplements(t, iface) })
}

// evLhsTarget is the local variable an assignment target writes and the
// field path it writes; an index drops the path.
func evLhsTarget(info *types.Info, l ast.Expr) (*types.Var, string) {
	var parts []string
	for {
		switch x := ast.Unparen(l).(type) {
		case *ast.Ident:
			v, _ := info.ObjectOf(x).(*types.Var)
			slices.Reverse(parts)
			return v, strings.Join(parts, ".")
		case *ast.SelectorExpr:
			parts = append(parts, x.Sel.Name)
			l = x.X
		case *ast.StarExpr:
			l = x.X
		case *ast.IndexExpr:
			parts = nil
			l = x.X
		default:
			return nil, ""
		}
	}
}

// assignments follows every value root assigns to v, or to the part of v
// that path names.
func (w *evOriginWalk) assignments(f *evFile, root ast.Node, v *types.Var, path string, depth int) {
	info := f.info
	follow := func(rhs ast.Expr, lpath string, result int) {
		switch {
		case lpath == "":
			w.walk(f, rhs, path, depth+1, result)
		case path == lpath:
			w.walk(f, rhs, "", depth+1, result)
		case strings.HasPrefix(path, lpath+"."):
			w.walk(f, rhs, path[len(lpath)+1:], depth+1, result)
		}
	}
	ast.Inspect(root, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, l := range s.Lhs {
				target, lpath := evLhsTarget(info, l)
				if target != v {
					continue
				}
				if s.Tok == token.ADD_ASSIGN {
					w.report(f, s, "+=", "a += changes the value before the command builder")
					continue
				}
				if len(s.Rhs) == len(s.Lhs) {
					follow(s.Rhs[i], lpath, 0)
				} else if len(s.Rhs) == 1 {
					follow(s.Rhs[0], lpath, i)
				}
			}
		case *ast.ValueSpec:
			for i, name := range s.Names {
				if info.Defs[name] != v {
					continue
				}
				if len(s.Values) == len(s.Names) {
					follow(s.Values[i], "", 0)
				} else if len(s.Values) == 1 {
					follow(s.Values[0], "", i)
				}
			}
		case *ast.RangeStmt:
			for _, k := range []ast.Expr{s.Key, s.Value} {
				if id, ok := k.(*ast.Ident); ok && info.ObjectOf(id) == v {
					w.walk(f, s.X, path, depth+1, 0)
				}
			}
		}
		return true
	})
}

// evOriginSite is an allowlist row of guard 3a for one call in one
// function: the enclosing function and the callee (or the operation).
type evOriginSite struct{ fn, callee string }

// evApplyOriginRows splits the guard 3a findings into those no row covers,
// and lists the rows that matched nothing.
func evApplyOriginRows(r *evResults, sites map[evOriginSite]string, allow evAllow) (left []evFinding, unused []string) {
	used := map[evOriginSite]bool{}
	for _, f := range r.byRule("3a") {
		s := evOriginSite{f.fn, f.key}
		if _, ok := sites[s]; ok {
			used[s] = true
			continue
		}
		left = append(left, f)
	}
	for _, s := range slices.SortedFunc(maps.Keys(sites), func(a, b evOriginSite) int { return strings.Compare(a.fn+a.callee, b.fn+b.callee) }) {
		if !used[s] {
			unused = append(unused, "site row "+s.fn+" / "+s.callee)
		}
	}
	for kind, rows := range map[string]map[string]string{"callee": allow.originCallees, "through": allow.originThrough, "param": allow.originParams} {
		for _, k := range slices.Sorted(maps.Keys(rows)) {
			if !r.used[kind+":"+k] {
				unused = append(unused, kind+" row "+k)
			}
		}
	}
	slices.Sort(unused)
	return left, unused
}

// TestNoValueChangesBeforeTheCommandBuilder asserts guard 3a over the
// production tree: each value argument of Arg, FlagValue, FileCommandIn's
// dir, Revoke (the Name of its TokenRef), RevokeKubeconfig,
// FileRevokeKubeconfig, FlagFragment, FlagFragmentWord,
// FlagFragmentAfterWord, ShellWord and TypedLine's Bare, Value and Joined
// reaches the builder from its source with no change on the way. The walk
// goes back through locals, parameters (to every direct call site, with the
// field path kept), field reads, composite-literal fields, index, range,
// append, type assertions, conversions, package-level variables, calls that
// return no string, and calls that take no string other than constants (a
// source such as created.GetName() or in.String("cluster")). Any other call
// that returns a string from a string, a concatenation, a += and a slice of
// a string fail, unless the callee is an allowlisted source or
// canonicalization (evOriginCallees), a call that returns one of its
// arguments unchanged (evOriginThrough), or the site has a row
// (evOriginSites). A parameter of a function used as a value, of a method
// an interface method call can reach, or of a function literal needs a row
// in evOriginParams. Every row matches something.
//
// Limits: a value written through a pointer by another function (fill(&v)),
// a field set by another function on a struct the value is read from, and a
// method reached through an interface whose signature prints differently in
// two loads are not seen. A field read on the result of a call that returns
// a struct is a source.
func TestNoValueChangesBeforeTheCommandBuilder(t *testing.T) {
	t.Parallel()
	r, err := evProductionResults()
	require.NoError(t, err)
	left, unused := evApplyOriginRows(r, evOriginSites, evProductionAllow)
	for _, f := range left {
		t.Error(f.String())
	}
	for _, u := range unused {
		t.Errorf("guard 3a allowlist: the %s matched nothing; remove it", u)
	}
}
