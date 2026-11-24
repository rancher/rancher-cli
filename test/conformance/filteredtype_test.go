package conformance

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// The scan of this file checks the places the compiler cannot see for a
// clierr.Filtered, the type output.FilterValue and runtime.FilterRemedies
// return. The compiler refuses to slice, index, pass to len, range over,
// compare with < or join with + a Filtered, or to convert one to a string;
// the five rules below check what is left, each from the expression and its
// parent alone, with no data flow. The rows of each rule name a function, a
// reason and the exact number of expressions they accept; a row that accepts
// more or fewer fails.
//
//  1. Sinks: every reference to Filtered.Marked is a call that is the Text or
//     WideText value of a runtime.Result literal, the right side of an
//     assignment to that field (the same index in a tuple), or an argument of
//     runtime.Prompter's Notice, or is in a function ftKindMarked lists (none
//     in production). A method value or a method expression of Marked is
//     refused.
//  2. Exits (the ways a Filtered's text becomes a string other than through
//     Marked): every reference to Filtered.WithoutMark is in a StyledText
//     function literal of a runtime.Result literal of a function ftKindStyled
//     lists, or in a function ftKindWithoutMark lists. Such a function, and a
//     function ftKindExitCaller lists, uses no clierr.Builder and builds no
//     runtime.Result. A call of a function ftKindWithoutMark lists is the
//     StyledText value of a Result literal, the right side of an assignment
//     to the StyledText of a Result, a call inside such a function, or a call
//     in a function ftKindExitCaller lists; a function value of either kind
//     is refused. A call of a function ftKindExitCaller lists is the Data
//     value of a Result literal. A listed function assigns only to its
//     locals. clierr.NewFiltered is referenced only by the two filters. The
//     field of the struct of Filtered or Builder, read through any type, and
//     a composite literal of that struct with a value, appear only in
//     filtered.go.
//  3. No conversion to an interface. No expression whose type has a Filtered
//     or a clierr.Builder at any depth is converted to an interface type, and
//     no generic function or type is instantiated with such a type argument.
//     A type has one when it is the type itself, a defined type over one, a
//     pointer to it, an element of a slice, array or channel, a key or value
//     of a map, a field of a struct (exported or not, embedded or not), the
//     result of a function type, or the result of an exported method of a
//     named type. A runtime.Result counts too, since fmt, encoding/json and
//     reflect read its Text whole: in the results of a function type and of
//     an exported method with no parameters as well, unless ftKindResult
//     lists the place. The results of a method with parameters never count a
//     Result, since every command type's Execute returns one. The positions
//     are a call argument (a tuple of a call, a variadic ...any, the built-in
//     append, go and defer calls included), a conversion, an assignment, a :=
//     that reuses a variable, a declaration (a tuple included), a return (of
//     a function literal too), a composite literal field, element, key or
//     value, a send, a map index, a comparison with an interface value, a
//     switch tag or case, and a range assignment. fmt reads an unexported
//     field by reflection, cannot call its Format method and applies a width
//     or a precision to the string inside. ftFormatRow accepts a Filtered
//     argument of clierr.Sprintf or Builder.Printf under %s or %v of a
//     constant format. ftFieldRows accept a value of a listed field of type
//     any, which only the functions ftKindFieldRead lists may read, and only
//     as the tag of a type switch with a clause for Filtered and one for its
//     slice, no clause of an interface type, and neither of those in a clause
//     with another type. A conversion of the struct to another struct type is
//     refused. An import of unsafe outside the packages ftUnsafeImports lists
//     is refused, and so is a non-test import of a package of ftTestOnly.
//  4. Reads of the sink fields: a read of the Text or WideText field of
//     runtime.Result (of any type whose underlying struct is runtime.Result's)
//     is the right side of an assignment to the same field of another Result,
//     the argument of clierr.Plain, an operand of == or != with "", or is in a
//     function ftKindRead lists. A row accepts any use of its read. A
//     compound assignment (r.Text += "x") appends and keeps every character,
//     and counts as a write. A conversion of a Result, or a pointer to one,
//     to a struct type with the same fields is refused: its Text is a field
//     this rule does not watch. A read of StyledText, which gives the text as
//     a string taken out of a Filtered, is a write, a whole copy, a
//     comparison with nil, a StyledText value, or in a function
//     ftKindStyledRead lists.
//  5. One Notice renderer: the only method named Notice of a type that
//     implements runtime.Prompter is ui.Prompter.Notice, and only the
//     functions ftKindRewrite lists call WithNoticeRewrite, with the
//     invocation's IO.ExpandText.
//
// The scan reads the non-test files of every GOOS the release builds, each for
// amd64. The bodies of internal/clierr/filtered.go are trusted: rules 1-3 skip
// that file, and TestNothingCutsAFilterResult checks its declarations against
// ftTrustedDecls, each with the unit test that names it, and its bodies
// against ftTrustedCalls (no slice, no index of a string, no []rune, no
// precision in a format). clierr.textAfter and clierr.Text (command.go),
// which TextOf passes the first part to, are the two functions over strings
// that receive a Filtered's text; the unit tests of TextOf and of Text cover
// them. Limits: a writer handed to ui.New that cuts what it is given; a Go
// function with no body, backed by an assembly file (the module has none and
// builds without cgo); a method with parameters that returns a
// runtime.Result, called by name from a template; a file every load leaves
// out by its build tags.

const (
	ftClierrPath  = modulePrefix + "internal/clierr"
	ftRuntimePath = modulePrefix + "internal/runtime"
)

// The row kinds of a scan: each maps the key of a function (or, for an
// instantiation, of the package that writes it and the generic function) to
// its reason and the exact number of expressions it accepts.
const (
	ftKindStyled      = "StyledText"
	ftKindWithoutMark = "WithoutMark"
	ftKindMarked      = "Marked"
	ftKindRead        = "read of Result.Text"
	ftKindResult      = "runtime.Result converted"
	ftKindExitCaller  = "caller of a WithoutMark function"
	ftKindStyledRead  = "read of Result.StyledText"
	ftKindFieldRead   = "read of a listed any field"
	ftKindRewrite     = "WithNoticeRewrite"
)

// ftFix is the fix every rule 3 finding names.
const ftFix = "pass .Marked() at a sink, use a method that returns a bool, or work on the plain value before the filter"

// ftNewFilteredCallers are the only functions that may reference
// clierr.NewFiltered.
var ftNewFilteredCallers = map[string]bool{
	"internal/output.FilterValue":     true,
	"internal/runtime.FilterRemedies": true,
}

// ftCountRow is an allowlist row: its reason and the exact number of
// expressions it accepts.
type ftCountRow struct {
	count  int
	reason string
}

// ftFormatRow accepts an argument of clierr.Sprintf or Builder.Printf whose
// type is clierr.Filtered itself and whose verb in the constant format is %s
// or %v.
var ftFormatRow = ftCountRow{121, "clierr.Sprintf and Builder.Printf write a Filtered argument whole for %s and %v, pad it by Width and ignore a precision; a struct that has one is refused, since Sprintf passes it on to fmt"}

// ftFieldRows accept a value of a struct field of type any that has a
// clierr.Filtered (the first count) or a slice of the struct's own type (the
// second), each named by package, type and field. The functions that may read the
// field are rows of ftKindFieldRead.
var ftFieldRows = map[string][2]ftCountRow{
	"internal/commands/configcmd.field.val": {
		{6, "renderFields writes a Filtered value whole into the text and projectFields puts WithoutMark into the json projection"},
		{6, "a []field value is a section whose keys are Filtered: renderFields and projectFields render it with the same cases"},
	},
}

// ftFinding is one finding of the scan: its position, its rule and why.
type ftFinding struct {
	pos    string
	rule   int
	reason string
}

// ftTestOnly are the packages that a non-test file may not import: they may
// import unsafe because only tests use them.
var ftTestOnly = []string{"internal/envtestlog"}

// ftConfig has the rows a scan checks against: the production rows, or
// the rows of a fixture.
type ftConfig struct {
	rows        map[string]map[string]ftCountRow
	newFiltered map[string]bool
	format      ftCountRow
	fields      map[string][2]ftCountRow
	unsafe      map[string]string
	notice      string
}

// ftProductionConfig is the scan of the module's production code.
var ftProductionConfig = ftConfig{
	rows:        ftProductionRows,
	newFiltered: ftNewFilteredCallers,
	format:      ftFormatRow,
	fields:      ftFieldRows,
	unsafe:      ftUnsafeImports,
	notice:      "internal/ui.Prompter",
}

// ftResult is what one scan found: the findings and the positions of the
// expressions each row accepted. A file in more than one load counts once.
type ftResult struct {
	findings []ftFinding
	format   map[string]bool
	fields   map[string][2]map[string]bool
	accepted map[string]map[string]map[string]bool
	unsafe   map[string]bool
}

// accept records the position of an accepted expression in set.
func (s *ftScanner) accept(set map[string]bool, at ast.Node) {
	set[s.pkg.Fset.Position(at.Pos()).String()] = true
}

// ftRel is a package path without the module prefix.
func ftRel(p string) string { return strings.TrimPrefix(p, modulePrefix) }

// ftIsNamed reports whether t is the named type pkgPath.name.
func ftIsNamed(t types.Type, pkgPath, name string) bool {
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == pkgPath && n.Obj().Name() == name
}

// ftIsFiltered reports whether t is clierr.Filtered itself.
func ftIsFiltered(t types.Type) bool {
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == ftClierrPath && n.Obj().Name() == "Filtered"
}

// ftHolds reports whether t has a clierr.Filtered or a clierr.Builder at
// any depth: the type itself, a defined type over one (it has none of their
// methods), a pointer, an element, a key or value, a field, the result of a
// function type, or the result of an exported method of a named type (which
// reflection and templates call by name). With result, a runtime.Result
// counts as well, in a function type's results and in the results of an
// exported method with no parameters too; the results of a method with
// parameters never count a Result, since every command type's Execute
// returns one.
func ftHolds(t types.Type, seen map[ftSeenKey]bool, result bool) bool {
	t = types.Unalias(t)
	if t == nil || seen[ftSeenKey{t, result}] {
		return false
	}
	seen[ftSeenKey{t, result}] = true
	switch t := t.(type) {
	case *types.Named:
		o := t.Obj()
		if o.Pkg() != nil && o.Pkg().Path() == ftClierrPath && (o.Name() == "Filtered" || o.Name() == "Builder") {
			return true
		}
		if result && ftIsResult(t) {
			return true
		}
		if ftHolds(t.Underlying(), seen, result) {
			return true
		}
		return ftHoldingMethod(t, seen, result) != ""
	case *types.Pointer:
		return ftHolds(t.Elem(), seen, result)
	case *types.Slice:
		return ftHolds(t.Elem(), seen, result)
	case *types.Array:
		return ftHolds(t.Elem(), seen, result)
	case *types.Chan:
		return ftHolds(t.Elem(), seen, result)
	case *types.Map:
		return ftHolds(t.Key(), seen, result) || ftHolds(t.Elem(), seen, result)
	case *types.Signature:
		for v := range t.Results().Variables() {
			if ftHolds(v.Type(), seen, result) {
				return true
			}
		}
	case *types.Struct:
		if ftIsFilteredStruct(t) {
			return true
		}
		if result && ftIsResult(t) {
			return true
		}
		for f := range t.Fields() {
			if ftHolds(f.Type(), seen, result) {
				return true
			}
		}
	}
	return false
}

// ftIsInterface reports whether t is an interface type that is not a type
// parameter.
func ftIsInterface(t types.Type) bool {
	if t == nil {
		return false
	}
	if _, ok := types.Unalias(t).(*types.TypeParam); ok {
		return false
	}
	_, ok := t.Underlying().(*types.Interface)
	return ok
}

// ftResultField reports whether obj is the Text or WideText field of
// runtime.Result, and which.
func ftResultField(obj types.Object) (string, bool) {
	v, ok := obj.(*types.Var)
	if !ok || !v.IsField() || v.Pkg() == nil || v.Pkg().Path() != ftRuntimePath || (v.Name() != "Text" && v.Name() != "WideText") {
		return "", false
	}
	res, ok := v.Pkg().Scope().Lookup("Result").(*types.TypeName)
	if !ok {
		return "", false
	}
	st, ok := res.Type().Underlying().(*types.Struct)
	if !ok {
		return "", false
	}
	for f := range st.Fields() {
		if f == v {
			return v.Name(), true
		}
	}
	return "", false
}

// ftIsResult reports whether t's underlying struct is runtime.Result's.
func ftIsResult(t types.Type) bool {
	if t == nil {
		return false
	}
	st, ok := t.Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for f := range st.Fields() {
		if name, ok := ftResultField(f); ok && name == "Text" {
			return true
		}
	}
	return false
}

// ftIsNoticeOfPrompter reports whether fn is the Notice method of the
// runtime.Prompter interface.
func ftIsNoticeOfPrompter(fn *types.Func) bool {
	if fn == nil || fn.Name() != "Notice" || fn.Pkg() == nil || fn.Pkg().Path() != ftRuntimePath {
		return false
	}
	p, ok := fn.Pkg().Scope().Lookup("Prompter").(*types.TypeName)
	if !ok {
		return false
	}
	iface, ok := p.Type().Underlying().(*types.Interface)
	if !ok {
		return false
	}
	for m := range iface.Methods() {
		if m == fn {
			return true
		}
	}
	return false
}

// ftFilteredMethod reports whether sel selects the named method of
// clierr.Filtered, and the kind of selection.
func ftFilteredMethod(info *types.Info, sel *ast.SelectorExpr, name string) (types.SelectionKind, bool) {
	s, ok := info.Selections[sel]
	if !ok || s.Obj().Name() != name {
		return 0, false
	}
	fn, ok := s.Obj().(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != ftClierrPath {
		return 0, false
	}
	recv := fn.Signature().Recv()
	if recv == nil || !ftIsNamed(recv.Type(), ftClierrPath, "Filtered") {
		return 0, false
	}
	return s.Kind(), true
}

// ftCalledFunc is the function or method a call calls, or nil.
func ftCalledFunc(info *types.Info, call *ast.CallExpr) *types.Func {
	switch f := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		fn, _ := info.Uses[f].(*types.Func)
		return fn
	case *ast.SelectorExpr:
		if s, ok := info.Selections[f]; ok {
			fn, _ := s.Obj().(*types.Func)
			return fn
		}
		fn, _ := info.Uses[f.Sel].(*types.Func)
		return fn
	}
	return nil
}

// ftFuncKey is the key of a function declaration: package, then the
// receiver's type name when it has one, then the name.
func ftFuncKey(pkgPath string, fd *ast.FuncDecl) string {
	name := fd.Name.Name
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		t := fd.Recv.List[0].Type
		if st, ok := t.(*ast.StarExpr); ok {
			t = st.X
		}
		switch x := t.(type) {
		case *ast.IndexExpr:
			t = x.X
		case *ast.IndexListExpr:
			t = x.X
		}
		if id, ok := t.(*ast.Ident); ok {
			name = id.Name + "." + name
		}
	}
	return ftRel(pkgPath) + "." + name
}

// ftScanner scans one package of one load.
type ftScanner struct {
	cfg    ftConfig
	pkg    *packages.Package
	result *ftResult
	frames []ftFrame
	listed map[types.Object]string
}

func (s *ftScanner) add(at ast.Node, rule int, reason string) {
	s.result.findings = append(s.result.findings, ftFinding{s.pkg.Fset.Position(at.Pos()).String(), rule, reason})
}

func (s *ftScanner) typeOf(e ast.Expr) types.Type {
	if tv, ok := s.pkg.TypesInfo.Types[e]; ok {
		return tv.Type
	}
	return nil
}

// convert records a rule 3 finding when a value of type src is converted to
// the interface type dst. A runtime.Result counts too (fmt,
// encoding/json and reflect read its Text whole); a row of ftKindResult
// accepts it.
func (s *ftScanner) convert(at ast.Node, src, dst types.Type, where string) {
	if src == nil || dst == nil || !ftIsInterface(dst) || ftIsInterface(src) {
		return
	}
	if tup, ok := src.(*types.Tuple); ok {
		for v := range tup.Variables() {
			s.convert(at, v.Type(), dst, where)
		}
		return
	}
	if ftHolds(src, map[ftSeenKey]bool{}, false) {
		what := "is or has a clierr.Filtered or a clierr.Builder"
		if !ftHoldsDirect(src) {
			if n, ok := types.Unalias(src).(*types.Named); ok {
				if m := ftHoldingMethod(n, map[ftSeenKey]bool{}, false); m != "" {
					what = "has an exported method " + m + " whose results have a clierr.Filtered or a clierr.Builder, which a template or reflection calls by name,"
				}
			}
		}
		s.add(at, 3, fmt.Sprintf("%s: a value of type %s, which %s is converted to %s; %s", where, types.TypeString(src, ftQualifier), what, types.TypeString(dst, ftQualifier), ftFix))
		return
	}
	if ftHolds(src, map[ftSeenKey]bool{}, true) {
		if s.acceptRow(ftKindResult, s.fnKey(), at) {
			return
		}
		s.add(at, 3, fmt.Sprintf("%s: a value of type %s, which is or has a runtime.Result, is converted to %s: fmt, encoding/json and reflect read its Text whole; hand the Result to the pipeline, or read its Text through clierr.Plain", where, types.TypeString(src, ftQualifier), types.TypeString(dst, ftQualifier)))
	}
}

// ftQualifier writes a package by its name.
func ftQualifier(p *types.Package) string { return p.Name() }

// formatCall reports whether call is clierr.Sprintf or Builder.Printf.
func (s *ftScanner) formatCall(call *ast.CallExpr) bool {
	fn := ftCalledFunc(s.pkg.TypesInfo, call)
	if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != ftClierrPath {
		return false
	}
	if fn.Name() == "Sprintf" && fn.Signature().Recv() == nil {
		return true
	}
	recv := fn.Signature().Recv()
	return fn.Name() == "Printf" && recv != nil && ftIsNamed(recv.Type(), ftClierrPath, "Builder")
}

func (s *ftScanner) call(call *ast.CallExpr) {
	tv := s.pkg.TypesInfo.Types[call.Fun]
	if tv.IsType() {
		if len(call.Args) == 1 {
			src := s.typeOf(call.Args[0])
			s.convert(call, src, tv.Type, "a conversion")
			if src != nil && ftIsResult(ftDeref(src)) && !ftIsResult(ftDeref(tv.Type)) {
				if _, ok := ftDeref(tv.Type).Underlying().(*types.Struct); ok {
					s.add(call, 4, "a conversion of a runtime.Result to a struct type with the same fields: its Text is a field rule 4 does not watch; read the Text of the Result itself")
				}
			}
		}
		return
	}
	if tv.Type == nil {
		return
	}
	sig, ok := types.Unalias(tv.Type).Underlying().(*types.Signature)
	if !ok {
		return
	}
	params := sig.Params()
	paramAt := func(i int, spread bool) types.Type {
		switch {
		case sig.Variadic() && i >= params.Len()-1:
			last := params.At(params.Len() - 1).Type()
			if spread {
				return last
			}
			if sl, ok := last.Underlying().(*types.Slice); ok {
				return sl.Elem()
			}
		case i < params.Len():
			return params.At(i).Type()
		}
		return nil
	}
	if len(call.Args) == 1 {
		if tup, ok := s.typeOf(call.Args[0]).(*types.Tuple); ok {
			for i := range tup.Len() {
				s.convert(call.Args[0], tup.At(i).Type(), paramAt(i, false), "a call argument")
			}
			return
		}
	}
	format := s.formatCall(call)
	var verbs []rune
	verbsOK := false
	if format && len(call.Args) > 0 {
		if c := s.pkg.TypesInfo.Types[call.Args[0]]; c.Value != nil && c.Value.Kind() == constant.String {
			verbs, verbsOK = ftFormatVerbs(constant.StringVal(c.Value))
		}
	}
	for i, a := range call.Args {
		at := s.typeOf(a)
		if format && i >= 1 && !call.Ellipsis.IsValid() && ftIsFiltered(at) {
			switch {
			case !verbsOK:
				s.add(a, 3, "a clierr.Filtered argument of a format that is not a constant or uses an explicit argument index: write the format as a constant with %s or %v in order")
			case i-1 >= len(verbs) || (verbs[i-1] != 's' && verbs[i-1] != 'v'):
				s.add(a, 3, "a clierr.Filtered argument under a verb other than %s or %v: Sprintf writes the refusal %!<verb>(clierr.Filtered) there; use %s")
			default:
				s.accept(s.result.format, a)
			}
			continue
		}
		s.convert(a, at, paramAt(i, call.Ellipsis.IsValid()), "a call argument")
	}
}

func (s *ftScanner) composite(lit *ast.CompositeLit) {
	t := s.typeOf(lit)
	if t == nil {
		return
	}
	switch u := t.Underlying().(type) {
	case *types.Struct:
		for i, e := range lit.Elts {
			var fv *types.Var
			val := e
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				if key, ok := kv.Key.(*ast.Ident); ok {
					for f := range u.Fields() {
						if f.Name() == key.Name {
							fv = f
						}
					}
				}
				val = kv.Value
			} else if i < u.NumFields() {
				fv = u.Field(i)
			}
			if fv == nil {
				continue
			}
			src := s.typeOf(val)
			if n, ok := types.Unalias(t).(*types.Named); ok && n.Obj().Pkg() != nil {
				key := ftRel(n.Obj().Pkg().Path()) + "." + n.Obj().Name() + "." + fv.Name()
				if _, listed := s.cfg.fields[key]; listed {
					sets, ok := s.result.fields[key]
					if !ok {
						sets = [2]map[string]bool{{}, {}}
						s.result.fields[key] = sets
					}
					if ftIsFiltered(src) {
						s.accept(sets[0], val)
						continue
					}
					if sl, ok := types.Unalias(src).(*types.Slice); ok && types.Identical(sl.Elem(), t) {
						s.accept(sets[1], val)
						continue
					}
				}
			}
			s.convert(val, src, fv.Type(), "a composite literal field")
		}
	case *types.Slice, *types.Array:
		var elem types.Type
		if sl, ok := u.(*types.Slice); ok {
			elem = sl.Elem()
		} else {
			elem = u.(*types.Array).Elem()
		}
		for _, e := range lit.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				e = kv.Value
			}
			s.convert(e, s.typeOf(e), elem, "a composite literal element")
		}
	case *types.Map:
		for _, e := range lit.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				s.convert(kv.Key, s.typeOf(kv.Key), u.Key(), "a map literal key")
				s.convert(kv.Value, s.typeOf(kv.Value), u.Elem(), "a map literal value")
			}
		}
	}
}

func (s *ftScanner) assign(lhs, rhs []ast.Expr) {
	if len(lhs) == len(rhs) {
		for i := range lhs {
			s.convert(rhs[i], s.typeOf(rhs[i]), s.typeOf(lhs[i]), "an assignment")
		}
		return
	}
	if len(rhs) == 1 {
		if tup, ok := s.typeOf(rhs[0]).(*types.Tuple); ok {
			for i := range lhs {
				if i < tup.Len() {
					s.convert(rhs[0], tup.At(i).Type(), s.typeOf(lhs[i]), "an assignment")
				}
			}
		}
	}
}

// ftFrame is one function on the walk's stack: its key and its results.
type ftFrame struct {
	key     string
	results *types.Tuple
	styled  bool
}

// file walks one file for the rules. The bodies of internal/clierr's
// filtered.go are trusted and have unit tests: rules 1-3 skip that file.
func (s *ftScanner) file(f *ast.File) {
	info := s.pkg.TypesInfo
	trusted := s.pkg.PkgPath == ftClierrPath && strings.HasSuffix(filepath.ToSlash(s.pkg.Fset.Position(f.Pos()).Filename), "/internal/clierr/filtered.go")
	for _, imp := range f.Imports {
		for _, p := range ftTestOnly {
			if imp.Path.Value == `"`+modulePrefix+p+`"` {
				s.add(imp, 3, fmt.Sprintf("a non-test file imports %s, which may import unsafe because only tests use it: import it only from a _test.go file", p))
			}
		}
		if imp.Path.Value != `"unsafe"` {
			continue
		}
		if _, ok := s.cfg.unsafe[ftRel(s.pkg.PkgPath)]; ok {
			s.result.unsafe[ftRel(s.pkg.PkgPath)] = true
			continue
		}
		s.add(imp, 3, "an import of unsafe: it reads the string inside a clierr.Filtered past every rule; work on the plain value before the filter")
	}
	var stack []ast.Node
	s.frames = nil
	parent := func(depth int) ast.Node {
		if len(stack) <= depth {
			return nil
		}
		return stack[len(stack)-1-depth]
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			switch top.(type) {
			case *ast.FuncDecl, *ast.FuncLit:
				s.frames = s.frames[:len(s.frames)-1]
			}
			return true
		}
		switch n := n.(type) {
		case *ast.FuncDecl:
			var res *types.Tuple
			if obj, ok := info.Defs[n.Name].(*types.Func); ok {
				res = obj.Signature().Results()
			}
			s.frames = append(s.frames, ftFrame{key: ftFuncKey(s.pkg.PkgPath, n), results: res})
		case *ast.FuncLit:
			var res *types.Tuple
			if sig, ok := s.typeOf(n).(*types.Signature); ok {
				res = sig.Results()
			}
			styled := false
			if kv, ok := parent(0).(*ast.KeyValueExpr); ok && kv.Value == n {
				if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "StyledText" {
					if lit, ok := parent(1).(*ast.CompositeLit); ok && ftIsResult(s.typeOf(lit)) {
						styled = true
					}
				}
			}
			outer := len(s.frames) > 0 && s.frames[len(s.frames)-1].styled
			s.frames = append(s.frames, ftFrame{key: s.fnKey(), results: res, styled: styled || outer})
		}
		if !trusted {
			s.rule3(n, s.frames)
			s.exitRows(n, parent)
			s.exitUses(n, parent)
			if c, ok := n.(*ast.CallExpr); ok {
				s.listedConversion(c)
			}
		}
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if !trusted {
				s.rule1(sel, parent)
				s.rule2(sel, s.frames)
				s.fields(sel, parent)
			}
			s.rule4(sel, parent)
			s.rewrite(sel, parent)
		}
		if id, ok := n.(*ast.Ident); ok && !trusted {
			if fn, ok := info.Uses[id].(*types.Func); ok && fn.Pkg() != nil && fn.Pkg().Path() == ftClierrPath && fn.Name() == "NewFiltered" && fn.Signature().Recv() == nil {
				if !s.cfg.newFiltered[s.fnKey()] {
					s.add(id, 2, "clierr.NewFiltered is referenced outside output.FilterValue and runtime.FilterRemedies; build CLI text with clierr.Plain")
				}
			}
		}
		stack = append(stack, n)
		return true
	})
}

func (s *ftScanner) rule1(sel *ast.SelectorExpr, parent func(int) ast.Node) {
	kind, ok := ftFilteredMethod(s.pkg.TypesInfo, sel, "Marked")
	if !ok {
		return
	}
	if kind != types.MethodVal {
		s.add(sel, 1, "a method expression of Filtered.Marked; call Marked at a sink: the Text or WideText of a runtime.Result, an argument of runtime.Prompter.Notice")
		return
	}
	call, ok := parent(0).(*ast.CallExpr)
	if !ok || call.Fun != sel {
		s.add(sel, 1, "a method value of Filtered.Marked; call Marked at a sink: the Text or WideText of a runtime.Result, an argument of runtime.Prompter.Notice")
		return
	}
	switch p := parent(1).(type) {
	case *ast.KeyValueExpr:
		if id, ok := p.Key.(*ast.Ident); ok && p.Value == call && (id.Name == "Text" || id.Name == "WideText") {
			if lit, ok := parent(2).(*ast.CompositeLit); ok && ftIsResult(s.typeOf(lit)) {
				return
			}
		}
	case *ast.AssignStmt:
		if len(p.Lhs) == len(p.Rhs) {
			for i, r := range p.Rhs {
				if r != call {
					continue
				}
				if lsel, ok := p.Lhs[i].(*ast.SelectorExpr); ok {
					if obj := s.pkg.TypesInfo.Uses[lsel.Sel]; obj != nil {
						if _, ok := ftResultField(obj); ok {
							return
						}
					}
				}
			}
		}
	case *ast.CallExpr:
		if slices.Contains(p.Args, ast.Expr(call)) && ftIsNoticeOfPrompter(ftCalledFunc(s.pkg.TypesInfo, p)) {
			return
		}
	}
	if s.acceptRow(ftKindMarked, s.fnKey(), sel) {
		return
	}
	s.add(sel, 1, "Filtered.Marked outside a sink; call Marked only as the Text or WideText of a runtime.Result or an argument of runtime.Prompter.Notice, and keep the text a clierr.Filtered until then")
}

func (s *ftScanner) rule2(sel *ast.SelectorExpr, frames []ftFrame) {
	if _, ok := ftFilteredMethod(s.pkg.TypesInfo, sel, "WithoutMark"); !ok {
		return
	}
	if len(frames) == 0 {
		s.add(sel, 2, "Filtered.WithoutMark outside a function; call it only in a function a row lists")
		return
	}
	fr := frames[len(frames)-1]
	if fr.styled && s.acceptRow(ftKindStyled, fr.key, sel) {
		return
	}
	if !fr.styled && s.acceptRow(ftKindWithoutMark, fr.key, sel) {
		return
	}
	s.add(sel, 2, fmt.Sprintf("Filtered.WithoutMark in %s, which no row lists; the text of a result or a notice goes to its sink through Marked", fr.key))
}

// rule4 checks a read of the Text or WideText of a runtime.Result. The left
// side of an assignment is a write; a compound assignment (r.Text += "x")
// keeps every character of the text and appends, and is accepted as one.
func (s *ftScanner) rule4(sel *ast.SelectorExpr, parent func(int) ast.Node) {
	obj := s.pkg.TypesInfo.Uses[sel.Sel]
	if obj == nil {
		return
	}
	field, ok := ftResultField(obj)
	if !ok {
		return
	}
	switch p := parent(0).(type) {
	case *ast.AssignStmt:
		for i, l := range p.Lhs {
			if l == ast.Expr(sel) {
				return
			}
			if len(p.Lhs) == len(p.Rhs) && p.Rhs[i] == ast.Expr(sel) {
				if lsel, ok := l.(*ast.SelectorExpr); ok {
					if lobj := s.pkg.TypesInfo.Uses[lsel.Sel]; lobj != nil {
						if name, ok := ftResultField(lobj); ok && name == field {
							return
						}
					}
				}
			}
		}
	case *ast.CallExpr:
		if fn := ftCalledFunc(s.pkg.TypesInfo, p); fn != nil && fn.Pkg() != nil && fn.Pkg().Path() == ftClierrPath && fn.Name() == "Plain" && len(p.Args) == 1 && p.Args[0] == ast.Expr(sel) {
			return
		}
	case *ast.BinaryExpr:
		if p.Op == token.EQL || p.Op == token.NEQ {
			other := p.Y
			if p.Y == ast.Expr(sel) {
				other = p.X
			}
			if lit, ok := ast.Unparen(other).(*ast.BasicLit); ok && lit.Kind == token.STRING && (lit.Value == `""` || lit.Value == "``") {
				return
			}
		}
	}
	if s.acceptRow(ftKindRead, s.fnKey(), sel) {
		return
	}
	s.add(sel, 4, fmt.Sprintf("a read of Result.%s that is not a whole copy into another Result, an argument of clierr.Plain or a comparison with \"\"; read the text through clierr.Plain and change it with the methods of clierr.Filtered", field))
}

func (s *ftScanner) rule3(n ast.Node, frames []ftFrame) {
	switch n := n.(type) {
	case *ast.CallExpr:
		s.call(n)
	case *ast.CompositeLit:
		s.composite(n)
	case *ast.AssignStmt:
		switch n.Tok {
		case token.ASSIGN:
			s.assign(n.Lhs, n.Rhs)
		case token.DEFINE:
			s.redeclare(n)
		}
	case *ast.ValueSpec:
		var tup *types.Tuple
		if len(n.Values) == 1 && len(n.Names) > 1 {
			tup, _ = s.typeOf(n.Values[0]).(*types.Tuple)
		}
		for i, id := range n.Names {
			obj := s.pkg.TypesInfo.Defs[id]
			switch {
			case obj == nil:
			case tup != nil && i < tup.Len():
				s.convert(n.Values[0], tup.At(i).Type(), obj.Type(), "a declaration")
			case i < len(n.Values):
				s.convert(n.Values[i], s.typeOf(n.Values[i]), obj.Type(), "a declaration")
			}
		}
	case *ast.ReturnStmt:
		if len(frames) == 0 || frames[len(frames)-1].results == nil {
			return
		}
		res := frames[len(frames)-1].results
		if len(n.Results) == res.Len() {
			for i, e := range n.Results {
				s.convert(e, s.typeOf(e), res.At(i).Type(), "a return")
			}
		} else if len(n.Results) == 1 {
			if tup, ok := s.typeOf(n.Results[0]).(*types.Tuple); ok {
				for i := range min(tup.Len(), res.Len()) {
					s.convert(n.Results[0], tup.At(i).Type(), res.At(i).Type(), "a return")
				}
			}
		}
	case *ast.SendStmt:
		if t := s.typeOf(n.Chan); t != nil {
			if ch, ok := t.Underlying().(*types.Chan); ok {
				s.convert(n.Value, s.typeOf(n.Value), ch.Elem(), "a send")
			}
		}
	case *ast.IndexExpr:
		if t := s.typeOf(n.X); t != nil {
			if m, ok := t.Underlying().(*types.Map); ok {
				s.convert(n.Index, s.typeOf(n.Index), m.Key(), "a map index")
			}
		}
	case *ast.BinaryExpr:
		if n.Op == token.EQL || n.Op == token.NEQ {
			s.convert(n.Y, s.typeOf(n.Y), s.typeOf(n.X), "a comparison")
			s.convert(n.X, s.typeOf(n.X), s.typeOf(n.Y), "a comparison")
		}
	case *ast.SwitchStmt:
		if n.Tag != nil {
			for _, st := range n.Body.List {
				for _, e := range st.(*ast.CaseClause).List {
					s.convert(e, s.typeOf(e), s.typeOf(n.Tag), "a switch case")
					s.convert(n.Tag, s.typeOf(n.Tag), s.typeOf(e), "a switch tag")
				}
			}
		}
	case *ast.RangeStmt:
		if n.Tok != token.ASSIGN {
			return
		}
		var kt, vt types.Type
		if t := s.typeOf(n.X); t != nil {
			switch u := t.Underlying().(type) {
			case *types.Map:
				kt, vt = u.Key(), u.Elem()
			case *types.Slice:
				vt = u.Elem()
			case *types.Array:
				vt = u.Elem()
			case *types.Chan:
				kt = u.Elem()
			}
		}
		if n.Key != nil {
			s.convert(n.Key, kt, s.typeOf(n.Key), "a range assignment")
		}
		if n.Value != nil {
			s.convert(n.Value, vt, s.typeOf(n.Value), "a range assignment")
		}
	}
}

// instances records a rule 3 finding for each instantiation with a type
// argument that has a clierr.Filtered or a clierr.Builder, and, outside
// internal/runtime, a runtime.Result, unless a row of ftKindResult names the
// package and the generic function.
func (s *ftScanner) instances() {
	for id, inst := range s.pkg.TypesInfo.Instances {
		if s.pkg.PkgPath == ftClierrPath && ftInFilteredGo(s.pkg.Fset.Position(id.Pos()).Filename) {
			continue
		}
		for t := range inst.TypeArgs.Types() {
			if ftHolds(t, map[ftSeenKey]bool{}, false) {
				s.add(id, 3, fmt.Sprintf("an instantiation of %s with %s, which is or has a clierr.Filtered or a clierr.Builder; %s", id.Name, types.TypeString(t, ftQualifier), ftFix))
				continue
			}
			if !ftHolds(t, map[ftSeenKey]bool{}, true) {
				continue
			}
			key := ftRel(s.pkg.PkgPath) + " uses " + id.Name
			if obj := s.pkg.TypesInfo.Uses[id]; obj != nil && obj.Pkg() != nil {
				key = ftRel(s.pkg.PkgPath) + " uses " + ftRel(obj.Pkg().Path()) + "." + obj.Name()
			}
			if !s.acceptRow(ftKindResult, key, id) {
				s.add(id, 3, fmt.Sprintf("an instantiation of %s with %s, which is or has a runtime.Result: a generic function can hand its Text to fmt, encoding/json or reflect; add a row with the reason it does not", id.Name, types.TypeString(t, ftQualifier)))
			}
		}
	}
}

// rule5 records a finding for each type of the package, other than the
// listed one, with a Notice method that implements runtime.Prompter.
func (s *ftScanner) rule5(prompter *types.Interface) {
	if prompter == nil {
		return
	}
	for id, obj := range s.pkg.TypesInfo.Defs {
		tn, ok := obj.(*types.TypeName)
		if !ok || tn.IsAlias() {
			continue
		}
		named, ok := tn.Type().(*types.Named)
		if !ok {
			continue
		}
		if _, isIface := named.Underlying().(*types.Interface); isIface {
			continue
		}
		if !types.Implements(named, prompter) && !types.Implements(types.NewPointer(named), prompter) {
			continue
		}
		key := ftRel(s.pkg.PkgPath) + "." + tn.Name()
		if key == s.cfg.notice {
			continue
		}
		s.add(id, 5, fmt.Sprintf("%s implements runtime.Prompter: only ui.Prompter.Notice may write a notice (it is the one renderer of notices)", key))
	}
}

// ftPrompter finds runtime.Prompter in a load: in one of its packages or
// among the packages they import.
func ftPrompter(pkgs []*packages.Package) *types.Interface {
	seen := map[*types.Package]bool{}
	var find func(p *types.Package) *types.Interface
	find = func(p *types.Package) *types.Interface {
		if p == nil || seen[p] {
			return nil
		}
		seen[p] = true
		if p.Path() == ftRuntimePath {
			if tn, ok := p.Scope().Lookup("Prompter").(*types.TypeName); ok {
				iface, _ := tn.Type().Underlying().(*types.Interface)
				return iface
			}
		}
		for _, imp := range p.Imports() {
			if iface := find(imp); iface != nil {
				return iface
			}
		}
		return nil
	}
	for _, pkg := range pkgs {
		if iface := find(pkg.Types); iface != nil {
			return iface
		}
	}
	return nil
}

// ftScan runs the rules over the packages of one or several loads and
// returns the findings, each once, with the positions each row accepted.
func ftScan(cfg ftConfig, loads ...[]*packages.Package) ftResult {
	r := ftResult{format: map[string]bool{}, fields: map[string][2]map[string]bool{}, accepted: map[string]map[string]map[string]bool{}, unsafe: map[string]bool{}}
	for _, pkgs := range loads {
		prompter := ftPrompter(pkgs)
		listed := ftListedFields(cfg, pkgs)
		for _, pkg := range pkgs {
			if !strings.HasPrefix(pkg.PkgPath, modulePrefix) {
				continue
			}
			sc := &ftScanner{cfg: cfg, pkg: pkg, result: &r, listed: listed}
			for _, f := range pkg.Syntax {
				sc.file(f)
			}
			sc.instances()
			sc.rule5(prompter)
		}
	}
	seen := map[string]bool{}
	var findings []ftFinding
	for _, f := range r.findings {
		if k := f.pos + "|" + f.reason; !seen[k] {
			seen[k] = true
			findings = append(findings, f)
		}
	}
	slices.SortFunc(findings, func(a, b ftFinding) int { return strings.Compare(a.pos, b.pos) })
	r.findings = findings
	return r
}

// ftCheckRows compares the counts a scan found with the rows of cfg: a row
// that accepts more or fewer expressions than it says fails, and so does a
// package that may import unsafe and does not.
func ftCheckRows(cfg ftConfig, r ftResult) []string {
	var problems []string
	if len(r.format) != cfg.format.count {
		problems = append(problems, fmt.Sprintf("the row for a clierr.Filtered argument of clierr.Sprintf or Builder.Printf accepts %d expressions; it says %d: count them again and write the number into the row", len(r.format), cfg.format.count))
	}
	for _, key := range slices.Sorted(maps.Keys(cfg.fields)) {
		rows, got := cfg.fields[key], r.fields[key]
		for i, what := range []string{"clierr.Filtered", "slice of its own type"} {
			if len(got[i]) != rows[i].count {
				problems = append(problems, fmt.Sprintf("the row %s for a %s accepts %d expressions; it says %d: count them again and write the number into the row", key, what, len(got[i]), rows[i].count))
			}
		}
	}
	for _, kind := range slices.Sorted(maps.Keys(cfg.rows)) {
		for _, key := range slices.Sorted(maps.Keys(cfg.rows[kind])) {
			if got, want := len(r.accepted[kind][key]), cfg.rows[kind][key].count; got != want {
				problems = append(problems, fmt.Sprintf("the %s row %s accepts %d expressions; it says %d: count them again and write the number into the row", kind, key, got, want))
			}
		}
	}
	for _, pkg := range slices.Sorted(maps.Keys(cfg.unsafe)) {
		if !r.unsafe[pkg] {
			problems = append(problems, fmt.Sprintf("the package %s may import unsafe and does not; remove its row", pkg))
		}
	}
	return problems
}

// ftProductionLoads are the module's non-test packages for every GOOS the
// release builds (scripts/release-build), each type-checked for amd64: a
// count then does not depend on the machine the test runs on.
func ftProductionLoads(t *testing.T) [][]*packages.Package {
	t.Helper()
	platforms, err := releasePlatforms()
	require.NoError(t, err)
	var loads [][]*packages.Package
	seen := map[string]bool{}
	for _, platform := range platforms {
		goos, _, _ := strings.Cut(platform, "/")
		if seen[goos] {
			continue
		}
		seen[goos] = true
		load, known := otherGOOSLoads[goos]
		require.True(t, known, "the release builds GOOS %s: add it to otherGOOSLoads", goos)
		pkgs, err := load()
		require.NoError(t, err)
		loads = append(loads, pkgs)
	}
	return loads
}

// TestNothingCutsAFilterResult asserts the five rules over the module's
// production code, the exact count of every row, and that the declarations
// of filtered.go, which the rules trust, are the ones ftTrustedDecls lists
// with their unit tests.
func TestNothingCutsAFilterResult(t *testing.T) {
	t.Parallel()
	loads := ftProductionLoads(t)
	r := ftScan(ftProductionConfig, loads...)
	for _, f := range r.findings {
		t.Errorf("%s: rule %d: %s", f.pos, f.rule, f.reason)
	}
	for _, p := range ftCheckRows(ftProductionConfig, r) {
		t.Error(p)
	}
	for _, p := range ftTrustedProblems(t, loads[0]) {
		t.Error(p)
	}
	for _, p := range ftTrustedBodyProblems(loads[0]) {
		t.Error(p)
	}
}

// ftFixtureDir is the directory of the fixtures of the scan.
const ftFixtureDir = "test/conformance/testdata/filteredtype"

// ftFixtures type-checks the fixture packages once.
var ftFixtures = sync.OnceValues(func() ([]*packages.Package, error) {
	return loadPackages("./"+ftFixtureDir+"/...", "")
})

// ftFixtureConfig is the scan of the fixtures: the rows name the fixture
// functions and types.
var ftFixtureConfig = ftConfig{
	rows: map[string]map[string]ftCountRow{
		ftKindStyled:      {"test/conformance/testdata/filteredtype/accepted.styled": {1, "a StyledText literal of a Result"}},
		ftKindWithoutMark: {"test/conformance/testdata/filteredtype/accepted.jsonProjection": {1, "a json projection"}, "test/conformance/testdata/filteredtype/accepted.restyle": {1, "a StyledText builder"}, "test/conformance/testdata/filteredtype/rejected.projectAndWrite": {1, "a projection that also writes"}},
		ftKindMarked:      {"test/conformance/testdata/filteredtype/accepted.lines": {1, "hands a line to a string form"}},
		ftKindRead:        {"test/conformance/testdata/filteredtype/accepted.render": {1, "prints a Text whole"}},
		ftKindResult:      {"test/conformance/testdata/filteredtype/accepted uses test/conformance/testdata/filteredtype/accepted.keep": {1, "stores a Result whole"}},
		ftKindFieldRead:   {"test/conformance/testdata/filteredtype/accepted.readVal": {1, "reads the any field in its listed function"}},
		ftKindRewrite:     {"test/conformance/testdata/filteredtype/accepted.withRewrite": {1, "the listed caller"}},
	},
	newFiltered: map[string]bool{"test/conformance/testdata/filteredtype/accepted.filter": true},
	format:      ftCountRow{3, "the accepted fixture's arguments"},
	fields: map[string][2]ftCountRow{
		"test/conformance/testdata/filteredtype/accepted.field.val": {{1, "a Filtered value"}, {1, "a []field value"}},
		"test/conformance/testdata/filteredtype/rejected.field.val": {{1, "a Filtered value"}, {0, "no []field value"}},
	},
	unsafe: map[string]string{},
	notice: "test/conformance/testdata/filteredtype/accepted.prompter",
}

// ftWants maps each fixture line that has a // want <rule> comment to its
// rule: the line must have a finding of that rule, and a line without such a
// comment must have none.
func ftWants(pkgs []*packages.Package) map[string]int {
	want := map[string]int{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Syntax {
			for _, cg := range f.Comments {
				for _, c := range cg.List {
					if rule, ok := strings.CutPrefix(c.Text, "// want "); ok {
						p := pkg.Fset.Position(c.Pos())
						var n int
						_, err := fmt.Sscanf(rule, "%d", &n)
						if err == nil {
							want[fmt.Sprintf("%s:%d", p.Filename, p.Line)] = n
						}
					}
				}
			}
		}
	}
	return want
}

// TestNothingCutsAFilterResultFixtures asserts every rule on its fixtures:
// each line of the rejected fixtures marked with the rule it breaks has a
// finding of that rule, and no other line of the rejected or the accepted
// fixtures has one; the rows of the fixture config accept the exact counts
// it gives.
func TestNothingCutsAFilterResultFixtures(t *testing.T) {
	t.Parallel()
	pkgs, err := ftFixtures()
	require.NoError(t, err)
	require.Len(t, pkgs, 2, "the accepted and the rejected fixture")
	r := ftScan(ftFixtureConfig, pkgs)
	want := ftWants(pkgs)
	got := map[string]int{}
	for _, f := range r.findings {
		p := f.pos[:strings.LastIndex(f.pos, ":")]
		if prior, ok := got[p]; ok && prior != f.rule {
			t.Errorf("%s has findings of rules %d and %d", p, prior, f.rule)
		}
		got[p] = f.rule
		if want[p] != f.rule {
			t.Errorf("%s: an unexpected finding of rule %d: %s", f.pos, f.rule, f.reason)
		}
	}
	for _, p := range slices.Sorted(maps.Keys(want)) {
		if got[p] != want[p] {
			t.Errorf("%s: no finding of rule %d", p, want[p])
		}
	}
	assert.Empty(t, ftCheckRows(ftFixtureConfig, r))
}

// TestNoticeRendererFindingNamesTheType asserts the finding texts of rule 5:
// for the fixture type that has every method of runtime.Prompter, and for
// the fixture's second caller of WithNoticeRewrite.
func TestNoticeRendererFindingNamesTheType(t *testing.T) {
	t.Parallel()
	pkgs, err := ftFixtures()
	require.NoError(t, err)
	var reasons []string
	for _, f := range ftScan(ftFixtureConfig, pkgs).findings {
		if f.rule == 5 {
			reasons = append(reasons, f.reason)
		}
	}
	assert.Equal(t, []string{
		"test/conformance/testdata/filteredtype/rejected.stderrPrompter implements runtime.Prompter: only ui.Prompter.Notice may write a notice (it is the one renderer of notices)",
		"a reference to ui.Prompter.WithNoticeRewrite other than its listed caller's WithNoticeRewrite(streams.ExpandText): the rewrite applies to every notice the one notice renderer writes",
	}, reasons)
}

// TestFilteredRowCountsAreChecked asserts that a row of rule 3 that accepts
// more or fewer expressions than it says fails, and so does a rule 2 row that
// accepts none.
func TestFilteredRowCountsAreChecked(t *testing.T) {
	t.Parallel()
	pkgs, err := ftFixtures()
	require.NoError(t, err)
	cfg := ftFixtureConfig
	cfg.format.count++
	cfg.rows = maps.Clone(cfg.rows)
	cfg.rows[ftKindWithoutMark] = maps.Clone(cfg.rows[ftKindWithoutMark])
	cfg.rows[ftKindWithoutMark]["test/conformance/testdata/filteredtype/accepted.gone"] = ftCountRow{1, "a function that does not exist"}
	problems := ftCheckRows(cfg, ftScan(cfg, pkgs))
	assert.Equal(t, []string{
		"the row for a clierr.Filtered argument of clierr.Sprintf or Builder.Printf accepts 3 expressions; it says 4: count them again and write the number into the row",
		"the WithoutMark row test/conformance/testdata/filteredtype/accepted.gone accepts 0 expressions; it says 1: count them again and write the number into the row",
	}, problems)
}

// TestFilteredFormatFunctionsKeepTheVetCheck asserts that go vet's printf
// check reports a wrong format in clierr.Sprintf and in Builder.Printf: the
// two pass their format and arguments on to fmt, which makes vet treat them
// as printf wrappers.
func TestFilteredFormatFunctionsKeepTheVetCheck(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(t.Context(), "go", "vet", "./"+path.Join("test", "conformance", "testdata", "filteredvet"))
	cmd.Env = testfs.EnvWithoutGit()
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "go vet reports the two wrong formats: %s", out)
	text := string(out)
	assert.Contains(t, text, "clierr.Sprintf format %d has arg name of wrong type", "the Sprintf call")
	assert.Contains(t, text, "Printf format %d has arg count of wrong type", "the Builder.Printf call")
	assert.Equal(t, 2, strings.Count(text, "of wrong type"), "one report per wrong format: %s", text)
}

// ftProductionRows are the rows of the production scan, by kind.
var ftProductionRows = map[string]map[string]ftCountRow{
	ftKindStyled: {
		"internal/commands/auth.authCheckCmd.Execute":       {1, "the styled form of auth status: printableText prints a StyledText only when the plain text has nothing to remove"},
		"internal/commands/configcmd.configInfoCmd.Execute": {1, "the styled form of config status: printableText prints a StyledText only when the plain text has nothing to remove"},
		"internal/engine/inspect.clusterCmd.Execute":        {1, "the styled form of inspect cluster: printableText prints a StyledText only when the plain text has nothing to remove"},
		"internal/engine/inspect.projectCmd.Execute":        {1, "the styled form of inspect project: printableText prints a StyledText only when the plain text has nothing to remove"},
		"internal/engine/inspect.userCmd.Execute":           {1, "the styled form of inspect user: printableText prints a StyledText only when the plain text has nothing to remove"},
		"internal/runtime.TextLinesOf":                      {1, "the styled form of every TextLines result: printableText prints a StyledText only when the plain text has nothing to remove"},
	},
	ftKindWithoutMark: {
		"internal/commands/configcmd.projectFields": {2, "the json projection of config status (a key and a value of Data, which json, yaml and templates print); it writes no text"},
		"internal/runtime.StyleEveryLineBoldOf":     {1, "the input of a StyledText builder: printableText prints a StyledText only when the plain text has nothing to remove"},
	},
	ftKindRead: {
		"internal/runtime.AccentLine":    {1, "reads the Text to restyle a line it matches whole; the Text itself is not changed"},
		"internal/runtime.printableText": {4, "the renderer of a result text: prints Text or WideText, or the StyledText when the plain text has nothing to remove"},
		"internal/runtime.renderResult":  {2, "the renderer of a result: decides which of Text and WideText prints, and passes it whole to printableText"},
		"internal/runtime.dryRunText":    {1, "the text of a TextLines result written whole into the dry-run preview"},
		"internal/runtime.errorText":     {1, "the text of a TextLines result written whole into the error text"},
	},
	ftKindResult: {
		"internal/token uses internal/config.CachedHit": {1, "the generic config.CachedHit, which internal/token calls for the exec credential cache with a runtime.Result, returns the Result its read function returns, whole; it prints nothing"},
	},
	ftKindExitCaller: {
		"internal/commands/configcmd.configData": {1, "the json projection of config status (Data): it calls projectFields and writes no text"},
	},
	ftKindFieldRead: {
		"internal/commands/configcmd.renderFields":  {1, "the text of config status: a case for each kind of value, a Filtered written whole"},
		"internal/commands/configcmd.projectFields": {1, "the json projection of config status: the same cases, a Filtered as WithoutMark"},
	},
	ftKindStyledRead: {
		"internal/runtime.printableText": {1, "the renderer of a result text: prints the StyledText only when the plain text has nothing to remove"},
		"internal/runtime.renderResult":  {1, "the renderer of a result: hands the StyledText to printableText"},
	},
	ftKindRewrite: {
		"internal/runtime.runRead": {1, "the pipeline's prompter, with the invocation's IO.ExpandText as the rewrite: it replaces this process's remedy markers and keeps every other character"},
	},
}

// ftUnsafeImports are the packages whose non-test files may import unsafe,
// each with its reason. The Filtered value is not one of the things they
// touch.
var ftUnsafeImports = map[string]string{
	"internal/testfs":     "a Windows test helper that calls the Win32 API for file ACLs; it is linked only into tests",
	"internal/envtestlog": "a Windows test helper that stops the envtest processes through the Win32 API; it is linked only into tests",
}

// acceptRow records at under the row key of kind when cfg has that row, and
// reports whether it did.
func (s *ftScanner) acceptRow(kind, key string, at ast.Node) bool {
	if _, ok := s.cfg.rows[kind][key]; !ok {
		return false
	}
	byKey := s.result.accepted[kind]
	if byKey == nil {
		byKey = map[string]map[string]bool{}
		s.result.accepted[kind] = byKey
	}
	if byKey[key] == nil {
		byKey[key] = map[string]bool{}
	}
	s.accept(byKey[key], at)
	return true
}

// ftIsFilteredStruct reports whether st is the underlying struct of
// clierr.Filtered or clierr.Builder: one unexported field declared in
// internal/clierr, s of type string or b of type strings.Builder.
func ftIsFilteredStruct(st *types.Struct) bool {
	if st.NumFields() != 1 {
		return false
	}
	f := st.Field(0)
	if f.Pkg() == nil || f.Pkg().Path() != ftClierrPath {
		return false
	}
	switch f.Name() {
	case "s":
		b, ok := f.Type().(*types.Basic)
		return ok && b.Kind() == types.String
	case "b":
		return ftIsNamed(f.Type(), "strings", "Builder")
	}
	return false
}

// fnKey is the key of the function the walk is in, or "".
func (s *ftScanner) fnKey() string {
	if len(s.frames) == 0 {
		return ""
	}
	return s.frames[len(s.frames)-1].key
}

// ftFormatVerbs returns the verb of each argument of a constant format, in
// order, and false when the format uses an explicit argument index.
func ftFormatVerbs(format string) ([]rune, bool) {
	var out []rune
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		i++
		for i < len(format) && strings.ContainsRune("+-# 0123456789.*", rune(format[i])) {
			if format[i] == '*' {
				out = append(out, '*')
			}
			i++
		}
		if i < len(format) && format[i] == '[' {
			return nil, false
		}
		if i < len(format) && format[i] != '%' {
			out = append(out, rune(format[i]))
		}
	}
	return out, true
}

// redeclare checks the left side of a := that reuses a variable already in
// scope: Go assigns to that variable, which can be an interface.
func (s *ftScanner) redeclare(n *ast.AssignStmt) {
	info := s.pkg.TypesInfo
	for i, l := range n.Lhs {
		id, ok := l.(*ast.Ident)
		if !ok || id.Name == "_" || info.Defs[id] != nil {
			continue
		}
		obj := info.Uses[id]
		if obj == nil {
			continue
		}
		var src types.Type
		if len(n.Lhs) == len(n.Rhs) {
			src = s.typeOf(n.Rhs[i])
		} else if len(n.Rhs) == 1 {
			if tup, ok := s.typeOf(n.Rhs[0]).(*types.Tuple); ok && i < tup.Len() {
				src = tup.At(i).Type()
			}
		}
		s.convert(n.Rhs[min(i, len(n.Rhs)-1)], src, obj.Type(), "an assignment")
	}
}

// exitRows refuses, in a function a WithoutMark or exit caller row lists,
// any expression of type clierr.Builder and a runtime.Result literal: such a
// function writes no text and builds no result. It checks each call of a
// WithoutMark row function: the StyledText value of a Result literal, a call
// inside such a function, or a call in a function an exit caller row lists;
// any other call is refused, since the string it gives back is taken out of a
// clierr.Filtered. It also checks a composite literal of the struct of
// clierr.Filtered or clierr.Builder with a value, which only filtered.go may
// write.
func (s *ftScanner) exitRows(n ast.Node, parent func(int) ast.Node) {
	key := s.fnKey()
	listed := false
	if len(s.frames) > 0 && !s.frames[len(s.frames)-1].styled {
		_, exit := s.cfg.rows[ftKindWithoutMark][key]
		_, caller := s.cfg.rows[ftKindExitCaller][key]
		listed = exit || caller
	}
	if e, ok := n.(ast.Expr); ok && listed {
		t := s.typeOf(e)
		if id, isIdent := e.(*ast.Ident); isIdent && t == nil {
			if v, ok := s.pkg.TypesInfo.Defs[id].(*types.Var); ok {
				t = v.Type()
			}
		}
		if t != nil && ftIsNamed(t, ftClierrPath, "Builder") {
			if _, isIdent := e.(*ast.Ident); isIdent || s.pkg.TypesInfo.Types[e].IsValue() {
				s.add(e, 2, fmt.Sprintf("%s reads WithoutMark or calls a function that does, and uses a clierr.Builder: such a function writes no text; move the text into another function", key))
			}
		}
	}
	switch n := n.(type) {
	case *ast.CompositeLit:
		t := s.typeOf(n)
		if listed && t != nil && ftIsResult(t) {
			s.add(n, 2, fmt.Sprintf("%s reads WithoutMark or calls a function that does, and builds a runtime.Result: such a function builds no result", key))
		}
		if t != nil && len(n.Elts) > 0 {
			if st, ok := t.Underlying().(*types.Struct); ok && ftIsFilteredStruct(st) {
				s.add(n, 2, "a composite literal of the struct of clierr.Filtered or clierr.Builder with a value outside filtered.go: build one with clierr.Plain or the two filters")
			}
		}
	case *ast.CallExpr:
		fn := ftCalledFunc(s.pkg.TypesInfo, n)
		if fn == nil || fn.Pkg() == nil {
			return
		}
		callee := ftRel(fn.Pkg().Path()) + "." + fn.Name()
		if recv := fn.Signature().Recv(); recv != nil {
			if nt, ok := types.Unalias(ftDeref(recv.Type())).(*types.Named); ok {
				callee = ftRel(fn.Pkg().Path()) + "." + nt.Obj().Name() + "." + fn.Name()
			}
		}
		if _, exit := s.cfg.rows[ftKindWithoutMark][callee]; !exit {
			return
		}
		if kv, ok := parent(0).(*ast.KeyValueExpr); ok && kv.Value == ast.Expr(n) {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "StyledText" {
				if lit, ok := parent(1).(*ast.CompositeLit); ok && ftIsResult(s.typeOf(lit)) {
					return
				}
			}
		}
		if as, ok := parent(0).(*ast.AssignStmt); ok && len(as.Lhs) == len(as.Rhs) {
			for i, r := range as.Rhs {
				if r != ast.Expr(n) {
					continue
				}
				if lsel, ok := as.Lhs[i].(*ast.SelectorExpr); ok {
					if v, ok := s.pkg.TypesInfo.Uses[lsel.Sel].(*types.Var); ok && v.IsField() && v.Name() == "StyledText" && v.Pkg() != nil && v.Pkg().Path() == ftRuntimePath {
						return
					}
				}
			}
		}
		if _, ok := s.cfg.rows[ftKindWithoutMark][key]; ok {
			return
		}
		if s.acceptRow(ftKindExitCaller, key, n) {
			return
		}
		s.add(n, 2, fmt.Sprintf("a call of %s, which reads WithoutMark, outside a StyledText value and the listed callers: its result is a string taken out of a clierr.Filtered", callee))
	}
}

// fields checks a selection of the field of clierr.Filtered or
// clierr.Builder (by the struct that declares it, whatever type the
// selection goes through), which only filtered.go may read, and a selection
// of a listed field of type any (ftFieldRows, by the field itself), which
// only the rows of ftKindFieldRead may read, and only as the tag of a type
// switch whose clauses each name one type: after the conversion to any, no
// other rule sees the value.
func (s *ftScanner) fields(sel *ast.SelectorExpr, parent func(int) ast.Node) {
	sl, ok := s.pkg.TypesInfo.Selections[sel]
	if !ok || sl.Kind() != types.FieldVal {
		return
	}
	if st := ftDeclaringStruct(sl); st != nil && ftIsFilteredStruct(st) {
		s.add(sel, 2, "a read of the field of clierr.Filtered or clierr.Builder outside filtered.go: use Marked at a sink or the type's methods")
		return
	}
	key, listed := s.listed[sl.Obj()]
	if !listed {
		return
	}
	if !ftTypeSwitchTag(sel, parent) || !s.switchClauses(sel, parent) {
		s.add(sel, 3, fmt.Sprintf("a read of %s that is not the tag of a type switch with a clause for clierr.Filtered and one for its slice, no clause of an interface type, and no clause that names one of those with another type: the field has a clierr.Filtered as an any, which no rule follows", key))
		return
	}
	if !s.acceptRow(ftKindFieldRead, s.fnKey(), sel) {
		s.add(sel, 3, fmt.Sprintf("a read of %s outside the functions that may read it: the field has a clierr.Filtered as an any, which no rule follows; read it in a listed function", key))
	}
}

// rewrite checks a reference to ui.Prompter.WithNoticeRewrite: the function
// it is given rewrites every notice ui.Prompter.Notice writes. Only the rows
// of ftKindRewrite may give one, and only the invocation's IO.ExpandText,
// which replaces this process's remedy markers and keeps every other
// character.
func (s *ftScanner) rewrite(sel *ast.SelectorExpr, parent func(int) ast.Node) {
	sl, ok := s.pkg.TypesInfo.Selections[sel]
	if !ok || sl.Obj().Name() != "WithNoticeRewrite" {
		return
	}
	fn, ok := sl.Obj().(*types.Func)
	if !ok || fn.Signature().Recv() == nil || !ftIsNamed(fn.Signature().Recv().Type(), modulePrefix+"internal/ui", "Prompter") {
		return
	}
	expand := false
	if call, ok := parent(0).(*ast.CallExpr); ok && call.Fun == ast.Expr(sel) && len(call.Args) == 1 {
		if arg, ok := ast.Unparen(call.Args[0]).(*ast.SelectorExpr); ok {
			if asl, ok := s.pkg.TypesInfo.Selections[arg]; ok && asl.Obj().Name() == "ExpandText" && ftIsNamed(asl.Recv(), ftRuntimePath, "IO") {
				expand = true
			}
		}
	}
	if !expand || !s.acceptRow(ftKindRewrite, s.fnKey(), sel) {
		s.add(sel, 5, "a reference to ui.Prompter.WithNoticeRewrite other than its listed caller's WithNoticeRewrite(streams.ExpandText): the rewrite applies to every notice the one notice renderer writes")
	}
}

// ftSeenKey is a type ftHolds visited, with the mode it visited it in: a type
// that has no Filtered when a Result does not count can have one when a
// Result counts.
type ftSeenKey struct {
	t      types.Type
	result bool
}

// ftHoldingMethod is the name of an exported method of t (its pointer method
// set) whose results have a Filtered or a Builder, or, with result and no
// parameters, a runtime.Result; "" for none.
func ftHoldingMethod(t *types.Named, seen map[ftSeenKey]bool, result bool) string {
	for sel := range types.NewMethodSet(types.NewPointer(t)).Methods() {
		if !sel.Obj().Exported() {
			continue
		}
		sig, ok := sel.Type().(*types.Signature)
		if !ok {
			continue
		}
		mode := result && sig.Params().Len() == 0
		for v := range sig.Results().Variables() {
			if ftHolds(v.Type(), seen, mode) {
				return sel.Obj().Name()
			}
		}
	}
	return ""
}

// ftHoldsDirect reports whether t has a Filtered or a Builder through its
// own structure, not only through the results of its methods.
func ftHoldsDirect(t types.Type) bool {
	if n, ok := types.Unalias(t).(*types.Named); ok {
		o := n.Obj()
		if o.Pkg() != nil && o.Pkg().Path() == ftClierrPath && (o.Name() == "Filtered" || o.Name() == "Builder") {
			return true
		}
		t = n.Underlying()
	}
	return ftHolds(t, map[ftSeenKey]bool{}, false)
}

// ftDeref is t without one pointer.
func ftDeref(t types.Type) types.Type {
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		return p.Elem()
	}
	return t
}

// ftDeclaringStruct is the struct that declares the field a selection
// selects, found by following the selection's index path from its receiver.
func ftDeclaringStruct(sl *types.Selection) *types.Struct {
	t := sl.Recv()
	var st *types.Struct
	for _, i := range sl.Index() {
		u, ok := ftDeref(t).Underlying().(*types.Struct)
		if !ok {
			return nil
		}
		st = u
		t = u.Field(i).Type()
	}
	return st
}

// ftTypeSwitchTag reports whether sel is x in `switch v := x.(type)`.
func ftTypeSwitchTag(sel *ast.SelectorExpr, parent func(int) ast.Node) bool {
	ta, ok := parent(0).(*ast.TypeAssertExpr)
	if !ok || ta.X != ast.Expr(sel) || ta.Type != nil {
		return false
	}
	as, ok := parent(1).(*ast.AssignStmt)
	if !ok {
		return false
	}
	ts, ok := parent(2).(*ast.TypeSwitchStmt)
	if !ok || ts.Assign != ast.Stmt(as) {
		return false
	}
	return true
}

// ftTrustedDecls are the declarations of internal/clierr/filtered.go, whose
// bodies rules 1-3 do not check, each with the unit test of
// internal/clierr/filtered_test.go whose body names it. Rules 1-3 trust this
// file, and a declaration in it never cuts the text it is given at a
// length: the test fails until a declaration the file adds is listed here with
// its unit test, and the file's bodies are checked for the operations that cut
// (ftTrustedBodyProblems).
var ftTrustedDecls = map[string]string{
	"Filtered":              "TestFilteredKeepsItsText",
	"NewFiltered":           "TestFilteredKeepsItsText",
	"Plain":                 "TestFilteredKeepsItsText",
	"Filtered.Append":       "TestFilteredJoinsTextWhole",
	"Filtered.AppendString": "TestFilteredJoinsTextWhole",
	"JoinFiltered":          "TestFilteredJoinsTextWhole",
	"Filtered.IsEmpty":      "TestFilteredIsEmptyOnlyForNoText",
	"Filtered.Width":        "TestFilteredWidthCountsRunes",
	"Filtered.TrimSuffix":   "TestFilteredTrimsAsTheStringsFunctions",
	"Filtered.TrimRight":    "TestFilteredTrimsAsTheStringsFunctions",
	"Filtered.Marked":       "TestFilteredKeepsItsText",
	"Filtered.WithoutMark":  "TestFilteredKeepsItsText",
	"Filtered.Format":       "TestFilteredRefusesFmtAndEncoders",
	"errFilteredEncoding":   "TestFilteredRefusesFmtAndEncoders",
	"Filtered.MarshalJSON":  "TestFilteredRefusesFmtAndEncoders",
	"Filtered.MarshalText":  "TestFilteredRefusesFmtAndEncoders",
	"whole":                 "TestSprintfWritesFilteredArgumentsWhole",
	"whole.Format":          "TestSprintfWritesFilteredArgumentsWhole",
	"Sprintf":               "TestSprintfWritesFilteredArgumentsWhole",
	"Builder":               "TestBuilderWritesEveryPartInOrder",
	"Builder.WriteString":   "TestBuilderWritesEveryPartInOrder",
	"Builder.Write":         "TestBuilderWritesEveryPartInOrder",
	"Builder.Printf":        "TestBuilderWritesEveryPartInOrder",
	"Builder.Filtered":      "TestBuilderWritesEveryPartInOrder",
	"TextOf":                "TestTextOfWritesItsFirstPartWhole",
}

// ftTrustedProblems compares the declarations of filtered.go in pkgs with
// ftTrustedDecls, and checks that filtered_test.go declares each test the
// table names.
func ftTrustedProblems(t *testing.T, pkgs []*packages.Package) []string {
	t.Helper()
	var problems []string
	found := map[string]bool{}
	for _, pkg := range pkgs {
		if pkg.PkgPath != ftClierrPath {
			continue
		}
		for _, f := range pkg.Syntax {
			if !strings.HasSuffix(filepath.ToSlash(pkg.Fset.Position(f.Pos()).Filename), "/internal/clierr/filtered.go") {
				continue
			}
			for _, d := range f.Decls {
				switch d := d.(type) {
				case *ast.FuncDecl:
					found[strings.TrimPrefix(ftFuncKey(pkg.PkgPath, d), "internal/clierr.")] = true
				case *ast.GenDecl:
					for _, sp := range d.Specs {
						switch sp := sp.(type) {
						case *ast.TypeSpec:
							found[sp.Name.Name] = true
						case *ast.ValueSpec:
							for _, id := range sp.Names {
								found[id.Name] = true
							}
						}
					}
				}
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(found)) {
		if _, ok := ftTrustedDecls[name]; !ok {
			problems = append(problems, fmt.Sprintf("internal/clierr/filtered.go declares %s, which ftTrustedDecls does not list: rules 1-3 trust this file, and a declaration in it never cuts the text it is given at a length; add its unit test and its name to ftTrustedDecls", name))
		}
	}
	src, err := os.ReadFile("internal/clierr/filtered_test.go")
	require.NoError(t, err)
	for _, name := range slices.Sorted(maps.Keys(ftTrustedDecls)) {
		if !found[name] {
			problems = append(problems, fmt.Sprintf("ftTrustedDecls lists %s, which internal/clierr/filtered.go does not declare; remove it", name))
		}
		test := ftTrustedDecls[name]
		i := strings.Index(string(src), "func "+test+"(")
		if i < 0 {
			problems = append(problems, fmt.Sprintf("ftTrustedDecls names %s for %s, which internal/clierr/filtered_test.go does not declare", test, name))
			continue
		}
		body := string(src[i:])
		if j := strings.Index(body[1:], "\nfunc "); j >= 0 {
			body = body[:j+1]
		}
		short := name[strings.LastIndex(name, ".")+1:]
		if !strings.Contains(body[len("func "+test):], short) {
			problems = append(problems, fmt.Sprintf("ftTrustedDecls names %s for %s, whose body does not name %s: name the unit test that exercises it", test, name, short))
		}
	}
	return problems
}

// ftListedFields maps the field object of each listed field of type any
// (ftFieldRows) in a load to its key: a selection of it through an embedding
// struct or a defined type is the same object.
func ftListedFields(cfg ftConfig, pkgs []*packages.Package) map[types.Object]string {
	out := map[types.Object]string{}
	for key := range cfg.fields {
		parts := strings.Split(key, ".")
		if len(parts) < 3 {
			continue
		}
		pkgPath := modulePrefix + strings.Join(parts[:len(parts)-2], ".")
		typeName, fieldName := parts[len(parts)-2], parts[len(parts)-1]
		for _, pkg := range pkgs {
			if pkg.PkgPath != pkgPath || pkg.Types == nil {
				continue
			}
			tn, ok := pkg.Types.Scope().Lookup(typeName).(*types.TypeName)
			if !ok {
				continue
			}
			if st, ok := tn.Type().Underlying().(*types.Struct); ok {
				for f := range st.Fields() {
					if f.Name() == fieldName {
						out[f] = key
					}
				}
			}
		}
	}
	return out
}

// ftInFilteredGo reports whether a file is internal/clierr/filtered.go.
func ftInFilteredGo(filename string) bool {
	return strings.HasSuffix(filepath.ToSlash(filename), "/internal/clierr/filtered.go")
}

// ftObjKey is the row key of a function object: package, then the
// receiver's type name when it has one, then the name.
func ftObjKey(fn *types.Func) string {
	key := ftRel(fn.Pkg().Path()) + "." + fn.Name()
	if recv := fn.Signature().Recv(); recv != nil {
		if nt, ok := types.Unalias(ftDeref(recv.Type())).(*types.Named); ok {
			key = ftRel(fn.Pkg().Path()) + "." + nt.Obj().Name() + "." + fn.Name()
		}
	}
	return key
}

// exitUses checks the other ways a WithoutMark string leaves its function: a
// reference to a WithoutMark or exit caller row function that is not the
// function of a call (a function value; the value of a blank declaration is
// accepted), a call of an exit caller row function that is not the Data value
// of a runtime.Result literal or in a listed function, an assignment in a
// listed function to anything but a local or an index of a local, and a read of
// Result.StyledText, which gives the text as a string taken out of a Filtered,
// that is not a write, a whole copy into another Result's StyledText, a
// comparison with nil, a StyledText value or in a function ftKindStyledRead
// lists.
func (s *ftScanner) exitUses(n ast.Node, parent func(int) ast.Node) {
	info := s.pkg.TypesInfo
	key := s.fnKey()
	_, inExit := s.cfg.rows[ftKindWithoutMark][key]
	_, inCaller := s.cfg.rows[ftKindExitCaller][key]
	listed := (inExit || inCaller) && len(s.frames) > 0 && !s.frames[len(s.frames)-1].styled
	switch n := n.(type) {
	case *ast.Ident:
		fn, ok := info.Uses[n].(*types.Func)
		if !ok || fn.Pkg() == nil {
			return
		}
		k := ftObjKey(fn)
		_, a := s.cfg.rows[ftKindWithoutMark][k]
		_, b := s.cfg.rows[ftKindExitCaller][k]
		if !a && !b {
			return
		}
		var fun ast.Node = n
		depth := 0
		if sel, ok := parent(0).(*ast.SelectorExpr); ok && sel.Sel == n {
			fun, depth = sel, 1
		}
		if call, ok := parent(depth).(*ast.CallExpr); ok && ast.Node(call.Fun) == fun {
			return
		}
		if vs, ok := parent(depth).(*ast.ValueSpec); ok {
			for i, v := range vs.Values {
				if ast.Node(v) == fun && i < len(vs.Names) && vs.Names[i].Name == "_" {
					return
				}
			}
		}
		s.add(n, 2, fmt.Sprintf("a reference to %s, which returns a string taken out of a clierr.Filtered, that is not a call: call it where its row allows", k))
	case *ast.CallExpr:
		fn := ftCalledFunc(info, n)
		if fn == nil || fn.Pkg() == nil {
			return
		}
		k := ftObjKey(fn)
		if _, ok := s.cfg.rows[ftKindExitCaller][k]; !ok || listed {
			return
		}
		if kv, ok := parent(0).(*ast.KeyValueExpr); ok && kv.Value == ast.Expr(n) {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Data" {
				if lit, ok := parent(1).(*ast.CompositeLit); ok && ftIsResult(s.typeOf(lit)) {
					return
				}
			}
		}
		s.add(n, 2, fmt.Sprintf("a call of %s that is not the Data value of a runtime.Result literal: its result has strings taken out of a clierr.Filtered", k))
	case *ast.AssignStmt:
		if !listed {
			return
		}
		for _, l := range n.Lhs {
			if id, ok := l.(*ast.Ident); ok && id.Name == "_" {
				continue
			}
			id, ok := ast.Unparen(l).(*ast.Ident)
			if !ok {
				if ix, isIndex := ast.Unparen(l).(*ast.IndexExpr); isIndex {
					id, ok = ast.Unparen(ix.X).(*ast.Ident)
				}
			}
			if !ok || id == nil {
				s.add(l, 2, fmt.Sprintf("%s reads WithoutMark or calls a function that does, and assigns to something other than a local", key))
				continue
			}
			if v, isVar := info.Uses[id].(*types.Var); isVar && v.Parent() == s.pkg.Types.Scope() {
				s.add(l, 2, fmt.Sprintf("%s reads WithoutMark or calls a function that does, and assigns to a package-level variable", key))
			}
		}
	case *ast.SelectorExpr:
		v, ok := info.Uses[n.Sel].(*types.Var)
		if !ok || !v.IsField() || v.Name() != "StyledText" || v.Pkg() == nil || v.Pkg().Path() != ftRuntimePath {
			return
		}
		switch p := parent(0).(type) {
		case *ast.AssignStmt:
			for i, l := range p.Lhs {
				if l == ast.Expr(n) {
					return
				}
				if len(p.Lhs) == len(p.Rhs) && p.Rhs[i] == ast.Expr(n) {
					if lsel, ok := l.(*ast.SelectorExpr); ok && lsel.Sel.Name == "StyledText" {
						return
					}
				}
			}
		case *ast.BinaryExpr:
			other := p.X
			if p.X == ast.Expr(n) {
				other = p.Y
			}
			if id, ok := other.(*ast.Ident); ok && id.Name == "nil" {
				return
			}
		case *ast.KeyValueExpr:
			if id, ok := p.Key.(*ast.Ident); ok && id.Name == "StyledText" && p.Value == ast.Expr(n) {
				return
			}
		}
		if s.acceptRow(ftKindStyledRead, key, n) {
			return
		}
		s.add(n, 4, "a read of Result.StyledText that is not a write, a whole copy into another Result, a comparison with nil or a StyledText value: it gives a string taken out of a clierr.Filtered")
	}
}

// listedConversion refuses a conversion of a struct that has a listed field
// of type any (ftFieldRows) to another struct type: the new struct's field
// is another object, which the field rules do not follow.
func (s *ftScanner) listedConversion(call *ast.CallExpr) {
	if len(call.Args) != 1 {
		return
	}
	tv := s.pkg.TypesInfo.Types[call.Fun]
	if !tv.IsType() {
		return
	}
	src := s.typeOf(call.Args[0])
	if src == nil {
		return
	}
	sst, ok := ftDeref(src).Underlying().(*types.Struct)
	if !ok {
		return
	}
	dst, ok := ftDeref(tv.Type).Underlying().(*types.Struct)
	if !ok || dst == sst {
		return
	}
	for f := range sst.Fields() {
		if key, listed := s.listed[f]; listed {
			s.add(call, 3, fmt.Sprintf("a conversion of a struct with the field %s to another struct type: the field rules do not follow the converted field; read the field in a listed function, not through a converted struct", key))
			return
		}
	}
}

// switchClauses reports whether the type switch on the listed field sel has
// a clause for clierr.Filtered and one for the listed slice, no clause of an
// interface type, and, in a clause with several types, none of those.
func (s *ftScanner) switchClauses(sel *ast.SelectorExpr, parent func(int) ast.Node) bool {
	sl, ok := s.pkg.TypesInfo.Selections[sel]
	if !ok {
		return false
	}
	ts := parent(2).(*ast.TypeSwitchStmt)
	recv, _ := types.Unalias(ftDeref(sl.Recv())).(*types.Named)
	var hasF, hasS bool
	for _, c := range ts.Body.List {
		list := c.(*ast.CaseClause).List
		for _, e := range list {
			t := s.pkg.TypesInfo.TypeOf(e)
			if t == nil {
				continue
			}
			if b, ok := t.(*types.Basic); ok && b.Kind() == types.UntypedNil {
				continue
			}
			isF := ftIsFiltered(t)
			slice, isSlice := types.Unalias(t).(*types.Slice)
			isS := isSlice && recv != nil && types.Identical(slice.Elem(), recv)
			if ftIsInterface(t) || (len(list) > 1 && (isF || isS)) {
				return false
			}
			hasF = hasF || isF
			hasS = hasS || isS
		}
	}
	return hasF && hasS
}

// ftTrustedCalls are the functions the bodies of filtered.go may call,
// beside the file's own declarations, the builtins and conversions to a type
// other than []rune: none of them cuts a string at a length. A format of
// fmt.Sprintf or fmt.Fprintf there is a constant with no precision, or the
// format parameter of the declaration passed on with its own arguments: a
// precision cuts a string argument.
var ftTrustedCalls = map[string]bool{
	"strings.Builder.WriteString":    true,
	"strings.Builder.String":         true,
	"strings.Join":                   true,
	"strings.TrimSuffix":             true,
	"strings.TrimRight":              true,
	"strings.Repeat":                 true,
	"unicode/utf8.RuneCountInString": true,
	"fmt.Sprintf":                    true,
	"fmt.Fprintf":                    true,
	"errors.New":                     true,
	"fmt.State.Width":                true,
	"fmt.State.Flag":                 true,
	"fmt.State.Write":                true,
	"internal/clierr.textAfter":      true,
}

// ftTrustedBodyProblems checks the bodies of filtered.go: no slice
// expression, no index of a string, no conversion to []rune, calls only of
// ftTrustedCalls, the file's own declarations and the builtins, and no
// precision in a format (ftFormatProblem).
func ftTrustedBodyProblems(pkgs []*packages.Package) []string {
	var problems []string
	for _, pkg := range pkgs {
		if pkg.PkgPath != ftClierrPath {
			continue
		}
		info := pkg.TypesInfo
		for _, f := range pkg.Syntax {
			if !ftInFilteredGo(pkg.Fset.Position(f.Pos()).Filename) {
				continue
			}
			for _, decl := range f.Decls {
				fd, _ := decl.(*ast.FuncDecl)
				ast.Inspect(decl, func(n ast.Node) bool {
					at := func() string { return pkg.Fset.Position(n.Pos()).String() }
					switch n := n.(type) {
					case *ast.SliceExpr:
						problems = append(problems, at()+": a slice expression in filtered.go: a declaration there never cuts at a length")
					case *ast.IndexExpr:
						if t := info.TypeOf(n.X); t != nil {
							if b, ok := t.Underlying().(*types.Basic); ok && b.Info()&types.IsString != 0 {
								problems = append(problems, at()+": an index of a string in filtered.go: a declaration there never cuts at a length")
							}
						}
					case *ast.CallExpr:
						tv := info.Types[n.Fun]
						if tv.IsType() {
							if sl, ok := tv.Type.Underlying().(*types.Slice); ok {
								if b, ok := sl.Elem().(*types.Basic); ok && b.Kind() == types.Int32 {
									problems = append(problems, at()+": a conversion to []rune in filtered.go: a declaration there never cuts at a length")
								}
							}
							return true
						}
						if tv.IsBuiltin() {
							return true
						}
						fn := ftCalledFunc(info, n)
						if fn == nil || fn.Pkg() == nil {
							problems = append(problems, at()+": a call of a function value in filtered.go: call a function ftTrustedCalls lists")
							return true
						}
						if fn.Pkg().Path() == ftClierrPath && ftInFilteredGo(pkg.Fset.Position(fn.Pos()).Filename) {
							return true
						}
						key := fn.Pkg().Path() + "." + fn.Name()
						if strings.HasPrefix(key, modulePrefix) {
							key = ftRel(key)
						}
						if recv := fn.Signature().Recv(); recv != nil {
							if nt, ok := types.Unalias(ftDeref(recv.Type())).(*types.Named); ok {
								key = nt.Obj().Pkg().Path() + "." + nt.Obj().Name() + "." + fn.Name()
							}
						}
						if !ftTrustedCalls[key] {
							problems = append(problems, fmt.Sprintf("%s: filtered.go calls %s, which ftTrustedCalls does not list: a declaration there never cuts at a length; work with the listed functions", at(), key))
						}
						if p := ftFormatProblem(info, fd, n, key); p != "" {
							problems = append(problems, at()+": "+p)
						}
					}
					return true
				})
			}
		}
	}
	return problems
}

// ftFormatProblem checks the format of a call of fmt.Sprintf or fmt.Fprintf
// in a declaration fd of filtered.go: it is a constant with no precision, or
// fd's format parameter passed on with fd's variadic parameter. It returns
// "" for any other call.
func ftFormatProblem(info *types.Info, fd *ast.FuncDecl, call *ast.CallExpr, key string) string {
	i := 0
	switch key {
	case "fmt.Sprintf":
	case "fmt.Fprintf":
		i = 1
	default:
		return ""
	}
	if len(call.Args) <= i {
		return ""
	}
	arg := call.Args[i]
	if tv := info.Types[arg]; tv.Value != nil && tv.Value.Kind() == constant.String {
		if format := constant.StringVal(tv.Value); ftHasPrecision(format) {
			return fmt.Sprintf("the format %q of %s has a precision, which cuts a string argument: a declaration in filtered.go never cuts at a length; drop the precision", format, key)
		}
		return ""
	}
	if fd != nil && call.Ellipsis.IsValid() && len(call.Args) == i+2 {
		var params []types.Object
		for _, fl := range fd.Type.Params.List {
			for _, name := range fl.Names {
				params = append(params, info.Defs[name])
			}
		}
		id, ok1 := arg.(*ast.Ident)
		rest, ok2 := call.Args[i+1].(*ast.Ident)
		variadic := false
		if pl := fd.Type.Params.List; len(pl) > 0 {
			_, variadic = pl[len(pl)-1].Type.(*ast.Ellipsis)
		}
		if ok1 && ok2 && variadic && len(params) >= 2 {
			if obj := info.Uses[id]; obj != nil && slices.Contains(params[:len(params)-1], obj) && info.Uses[rest] == params[len(params)-1] {
				return ""
			}
		}
	}
	return fmt.Sprintf("the format of %s is neither a constant nor the format parameter of its declaration passed on with its own arguments: a precision in it would cut a string argument; use a constant format", key)
}

// TestFilteredFormatPrecisionIsFound asserts that the body check of
// filtered.go finds a precision in every place a format can have one, and
// none in a format without one.
func TestFilteredFormatPrecisionIsFound(t *testing.T) {
	t.Parallel()
	for format, want := range map[string]bool{
		"%.300s":                 true,
		"%-24.3s":                true,
		"%*.*s":                  true,
		"%[1].2v":                true,
		"a %s and %.1s":          true,
		"%s":                     false,
		"%-24s":                  false,
		"%%.3s":                  false,
		"%%!%c(clierr.Filtered)": false,
		"100%% of %d.5":          false,
	} {
		assert.Equal(t, want, ftHasPrecision(format), format)
	}
}

// ftHasPrecision reports whether a verb of format has a precision (%.3s,
// %.*s, %[1].3s).
func ftHasPrecision(format string) bool {
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		j := i + 1
		for j < len(format) && strings.IndexByte("+-# 0123456789*[].", format[j]) >= 0 {
			if format[j] == '.' {
				return true
			}
			j++
		}
		i = j
	}
	return false
}
