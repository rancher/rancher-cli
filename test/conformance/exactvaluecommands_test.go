package conformance

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// evCommandRow is an allowlist row of guard 4: the rule, the site (an
// enclosing function by its key, or a file by its path) and the reason.
type evCommandRow struct{ rule, site, reason string }

// evApplyCommandRows splits the findings of a rule into those no row
// covers, and lists the rows of that rule that matched nothing.
func evApplyCommandRows(findings []evFinding, rule string, rows []evCommandRow) (left []evFinding, unused []string) {
	used := make([]bool, len(rows))
	for _, f := range findings {
		file, _, _ := strings.Cut(f.at, ":")
		i := slices.IndexFunc(rows, func(row evCommandRow) bool {
			return row.rule == rule && (row.site == f.fn || row.site == file)
		})
		if i < 0 {
			left = append(left, f)
			continue
		}
		used[i] = true
	}
	for i, row := range rows {
		if row.rule == rule && !used[i] {
			unused = append(unused, row.site)
		}
	}
	return left, unused
}

func evReportRule(t *testing.T, rule string) {
	t.Helper()
	r, err := evProductionResults()
	require.NoError(t, err)
	left, unused := evApplyCommandRows(r.byRule(rule), rule, evCommandRows)
	for _, f := range left {
		t.Error(f.String())
	}
	for _, u := range unused {
		t.Errorf("guard 4 allowlist: the rule %s row for %s matched nothing; remove it", rule, u)
	}
}

func (p *evProgram) exempt(f *evFile) bool { return f.path == evBuilderFile }

// scanWordOrigins is rule 4.1 for clierr.CommandWord and clierr.Lead,
// outside internal/clierr.
func (p *evProgram) scanWordOrigins(r *evResults) {
	for _, f := range p.files {
		if f.inPackage(evClierr) {
			continue
		}
		info := f.info
		ast.Inspect(f.file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.TypeSpec:
				if evIsWordType(info.TypeOf(x.Type)) {
					f.finding(r, "4.1", x, "", "the type "+x.Name.Name+" is declared from "+types.ExprString(x.Type)+"; a conversion to it would make command text from any string")
				}
			case *ast.TypeSwitchStmt:
				for _, s := range x.Body.List {
					for _, e := range s.(*ast.CaseClause).List {
						if evIsWordType(info.TypeOf(e)) {
							f.finding(r, "4.1", e, "", "a type switch case of "+types.ExprString(e)+" makes command text from a runtime value")
						}
					}
				}
			case *ast.UnaryExpr:
				if x.Op == token.AND && evHoldsWord(info.TypeOf(x.X)) {
					f.finding(r, "4.1", x, "", "the address of "+types.ExprString(x.X)+", which holds a CommandWord or Lead, lets another function write it")
				}
			case *ast.SelectorExpr:
				p.implicitAddress(r, f, x, evHoldsWord, "a CommandWord or Lead")
			case *ast.CallExpr:
				p.wordCall(r, f, x)
			case *ast.Ident:
				if inst, ok := info.Instances[x]; ok {
					for t := range inst.TypeArgs.Types() {
						if evHoldsWord(t) {
							f.finding(r, "4.1", x, "", "the instantiation of "+x.Name+" with "+t.String()+" can convert a runtime value to command text")
						}
					}
				}
			}
			if e, ok := n.(ast.Expr); ok {
				p.wordOrigin(r, f, e)
			}
			return true
		})
	}
}

// implicitAddress reports a method with a pointer receiver called on a
// value of a type that holds what holds checks.
func (p *evProgram) implicitAddress(r *evResults, f *evFile, x *ast.SelectorExpr, holds func(types.Type) bool, what string) {
	sel, ok := f.info.Selections[x]
	if !ok || sel.Kind() != types.MethodVal {
		return
	}
	recv := sel.Obj().Type().(*types.Signature).Recv()
	if recv == nil {
		return
	}
	if _, ptr := recv.Type().(*types.Pointer); !ptr {
		return
	}
	xt := f.info.TypeOf(x.X)
	if xt == nil {
		return
	}
	if _, ptr := xt.Underlying().(*types.Pointer); ptr || !holds(xt) {
		return
	}
	f.finding(r, "4.1", x, "", "the method "+x.Sel.Name+" takes the address of "+types.ExprString(x.X)+", which holds "+what)
}

// wordCall reports new of a type that holds a CommandWord or Lead, an unsafe
// conversion to or from a pointer to one, and a reflect call given one.
func (p *evProgram) wordCall(r *evResults, f *evFile, x *ast.CallExpr) {
	info := f.info
	if evBuiltin(info, x) == "new" && len(x.Args) == 1 && evHoldsWord(info.TypeOf(x.Args[0])) {
		f.finding(r, "4.1", x, "", "new of a type that holds a CommandWord or Lead lets another function write it")
		return
	}
	if tv, ok := info.Types[x.Fun]; ok && tv.IsType() && len(x.Args) == 1 {
		to, from := tv.Type, info.TypeOf(x.Args[0])
		if evIsUnsafePointer(to) && evHoldsWord(from) || evIsUnsafePointer(from) && evHoldsWord(to) {
			f.finding(r, "4.1", x, "", "an unsafe conversion makes command text from another value")
		}
		return
	}
	if _, fn := evCallee(info, x); fn != nil && fn.Pkg().Path() == "reflect" {
		for _, a := range x.Args {
			if evHoldsWord(info.TypeOf(a)) {
				f.finding(r, "4.1", a, "", "reflection on a value that holds a CommandWord or Lead")
			}
		}
	}
}

func evIsUnsafePointer(t types.Type) bool {
	b, ok := t.(*types.Basic)
	return ok && b.Kind() == types.UnsafePointer
}

// wordOrigin checks one expression of type CommandWord or Lead: a constant,
// a +, a call of Word or AliasWord of a kinds.Kind, a variable, parameter or
// field, an index of a container and a call of a module function are
// allowed (the values they hold or return are expressions of the type
// themselves, each checked here); anything else fails.
func (p *evProgram) wordOrigin(r *evResults, f *evFile, e ast.Expr) {
	info := f.info
	tv, ok := info.Types[e]
	if !ok || tv.IsType() || tv.Value != nil || !evIsWordType(tv.Type) {
		return
	}
	if _, fn := f.enclosing(e); fn == evKindWord || fn == evKindAlias {
		return
	}
	what := ""
	switch x := e.(type) {
	case *ast.ParenExpr, *ast.Ident, *ast.SelectorExpr, *ast.BinaryExpr, *ast.IndexExpr, *ast.IndexListExpr:
		return
	case *ast.CallExpr:
		if ftv, ok := info.Types[x.Fun]; ok && ftv.IsType() {
			what = "a conversion of a value that is not constant"
			break
		}
		key, fn := evCallee(info, x)
		switch {
		case key == evKindWord || key == evKindAlias:
			return
		case fn != nil && evIsModule(fn) && !evIsInstance(info, x):
			return
		case fn == nil && evBuiltin(info, x) == "":
			return
		}
		what = "the result of " + types.ExprString(x.Fun)
	case *ast.TypeAssertExpr:
		what = "a type assertion"
	case *ast.StarExpr:
		what = "a value read through a pointer"
	case *ast.UnaryExpr:
		what = "a channel receive"
	case *ast.SliceExpr:
		what = "a slice of command text"
	default:
		what = fmt.Sprintf("a %T", e)
	}
	f.finding(r, "4.1", e, "", what+" is not an allowed origin of "+tv.Type.String()+": use a constant, a + of allowed values, Word or AliasWord of a kinds.Kind, or a variable assigned only those")
}

// evIsInstance reports whether a call names an instantiated generic
// function.
func evIsInstance(info *types.Info, call *ast.CallExpr) bool {
	fun := ast.Unparen(call.Fun)
	switch f := fun.(type) {
	case *ast.IndexExpr:
		fun = f.X
	case *ast.IndexListExpr:
		fun = f.X
	}
	var id *ast.Ident
	switch f := ast.Unparen(fun).(type) {
	case *ast.Ident:
		id = f
	case *ast.SelectorExpr:
		id = f.Sel
	}
	if id == nil {
		return false
	}
	_, ok := info.Instances[id]
	return ok
}

// evKindField reports whether e writes the Name or Aliases of a kinds.Kind,
// or an element of Aliases.
func evKindField(info *types.Info, e ast.Expr, aliasesOnly bool) bool {
	e = ast.Unparen(e)
	if ix, ok := e.(*ast.IndexExpr); ok {
		e = ast.Unparen(ix.X)
		aliasesOnly = true
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	s, ok := info.Selections[sel]
	if !ok || s.Kind() != types.FieldVal || !evNamedIs(evFieldOwner(s), evKinds, "Kind") {
		return false
	}
	return sel.Sel.Name == "Aliases" || !aliasesOnly && sel.Sel.Name == "Name"
}

// scanKinds is the part of rule 4.1 that keeps a kinds.Kind holding the
// words of the kinds table, outside internal/engine/kinds.
func (p *evProgram) scanKinds(r *evResults) {
	for _, f := range p.files {
		if f.inPackage(evKinds) {
			continue
		}
		info := f.info
		ast.Inspect(f.file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				t := info.TypeOf(x)
				if p, ok := t.(*types.Pointer); ok {
					t = p.Elem()
				}
				if !evIsKind(t) {
					return true
				}
				for _, el := range x.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						f.finding(r, "4.1", x, "", "a kinds.Kind literal outside the kinds table sets its fields in order, Name included")
						break
					}
					if id, ok := kv.Key.(*ast.Ident); ok && (id.Name == "Name" || id.Name == "Aliases") {
						f.finding(r, "4.1", kv, "", "a kinds.Kind literal outside the kinds table sets "+id.Name+", which Word and AliasWord turn into command words")
					}
				}
			case *ast.AssignStmt:
				for _, l := range x.Lhs {
					if evKindField(info, l, false) {
						f.finding(r, "4.1", l, "", "an assignment to "+types.ExprString(l)+" outside the kinds table changes the words Word and AliasWord return")
					}
				}
			case *ast.CallExpr:
				switch b := evBuiltin(info, x); {
				case (b == "append" || b == "copy") && len(x.Args) > 0 && evKindField(info, x.Args[0], true):
					f.finding(r, "4.1", x, "", b+" of "+types.ExprString(x.Args[0])+" outside the kinds table can write the table's aliases")
					return true
				case b != "":
					return true
				}
				if tv, ok := info.Types[x.Fun]; ok && tv.IsType() && len(x.Args) == 1 {
					to, from := tv.Type, info.TypeOf(x.Args[0])
					if evIsUnsafePointer(to) && evHoldsKind(from) || evIsUnsafePointer(from) && evHoldsKind(to) {
						f.finding(r, "4.1", x, "", "an unsafe conversion of a kinds.Kind")
					}
					return true
				}
				_, fn := evCallee(info, x)
				if fn == nil || evIsModule(fn) {
					return true
				}
				for _, a := range x.Args {
					t := info.TypeOf(a)
					if t == nil {
						continue
					}
					_, ptr := t.Underlying().(*types.Pointer)
					switch {
					case fn.Pkg().Path() == "reflect" && evHoldsKind(t):
						f.finding(r, "4.1", a, "", "reflection on a kinds.Kind")
					case ptr && evHoldsKind(t):
						f.finding(r, "4.1", a, "", "the address of a kinds.Kind passed to "+types.ExprString(x.Fun)+", outside the module, which can write it")
					}
				}
			}
			return true
		})
	}
}

// evPartKind is the kind of a part of clierr.Text: "string", "lead",
// "command", "sequence", or the type's name.
func evPartKind(t types.Type) string {
	switch {
	case t == nil:
		return "unknown"
	case evIsClierrType(t, "Lead"):
		return "lead"
	case evIsClierrType(t, "Command"):
		return "command"
	case evIsClierrType(t, "Sequence"):
		return "sequence"
	}
	if b, ok := t.(*types.Basic); ok && (b.Kind() == types.String || b.Kind() == types.UntypedString) {
		return "string"
	}
	return t.String()
}

var evLeadInEnding = regexp.MustCompile(`(:|(^|\s)(with|of|run))\s*$`)

// scanTextCalls is rules 4.2 and 4.3 on every call of clierr.Text outside
// the builder's own file.
func (p *evProgram) scanTextCalls(r *evResults) {
	for _, f := range p.files {
		if p.exempt(f) {
			continue
		}
		ast.Inspect(f.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if key, _ := evCallee(f.info, call); key != evText {
				return true
			}
			if call.Ellipsis != token.NoPos {
				f.finding(r, "4.3", call, "", "clierr.Text is given a slice; pass each part, so each part's type is known")
				return true
			}
			kinds := make([]string, len(call.Args))
			for i, a := range call.Args {
				kinds[i] = evPartKind(f.info.TypeOf(a))
				switch kinds[i] {
				case "string", "lead", "command", "sequence":
				default:
					f.finding(r, "4.3", a, "", "a part of clierr.Text of type "+kinds[i]+"; a part is a string, a clierr.Lead, a clierr.Command or a clierr.Sequence")
				}
			}
			for i, a := range call.Args {
				switch kinds[i] {
				case "command", "sequence":
					if i > 0 && kinds[i-1] != "lead" {
						f.finding(r, "4.2", a, "", "the part before "+types.ExprString(a)+" is not a clierr.Lead; words that introduce a command go in a Lead, which is dropped with the command")
					}
				case "lead":
					if i+1 >= len(call.Args) || kinds[i+1] != "command" && kinds[i+1] != "sequence" {
						f.finding(r, "4.2", a, "", "a clierr.Lead not followed by a Command or Sequence")
					}
					values, ok := p.leadValues(f, a)
					if !ok {
						f.finding(r, "4.2", a, "", "a clierr.Lead whose words the scan cannot read; use a constant")
					}
					for _, v := range values {
						if strings.TrimSpace(v) == "" {
							f.finding(r, "4.2", a, "", "an empty or blank clierr.Lead")
						}
					}
					if i > 0 && kinds[i-1] == "string" {
						if tail, ok := p.trailingText(f, call.Args[i-1], 0); ok && evLeadInEnding.MatchString(tail) {
							f.finding(r, "4.2", call.Args[i-1], "", "the prose before a clierr.Lead ends with words that introduce a command ("+fmt.Sprintf("%q", tail)+"); move them into the Lead")
						}
					}
				}
			}
			return true
		})
	}
}

// leadValues is every value a Lead part can have: its constant, or the
// constants assigned to a local variable.
func (p *evProgram) leadValues(f *evFile, e ast.Expr) ([]string, bool) {
	if s, ok := constString(f.info, e); ok {
		return []string{s}, true
	}
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok {
		return nil, false
	}
	obj := f.info.Uses[id]
	root, _ := f.enclosing(id)
	vals := p.localsOf(f, root)[obj]
	if len(vals) == 0 {
		return nil, false
	}
	var out []string
	for _, v := range vals {
		s, ok := constString(f.info, v)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// trailingText is the constant text a string expression ends with, when the
// scan can read it.
func (p *evProgram) trailingText(f *evFile, e ast.Expr, depth int) (string, bool) {
	items := p.items(f, e, depth)
	if len(items) == 0 || !items[len(items)-1].isLit {
		if len(items) == 1 && depth < 3 {
			if id, ok := ast.Unparen(items[0].expr).(*ast.Ident); ok {
				root, _ := f.enclosing(id)
				if vals := p.localsOf(f, root)[f.info.Uses[id]]; len(vals) == 1 {
					return p.trailingText(f, vals[0], depth+1)
				}
			}
		}
		return "", false
	}
	return items[len(items)-1].lit, true
}

// scanShellQuote is rule 4.4: no clierr.ShellQuote output in a command, and
// no call of the positional quoting helper outside internal/clierr's quoting
// code.
func (p *evProgram) scanShellQuote(r *evResults) {
	for _, f := range p.files {
		ast.Inspect(f.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			key, _ := evCallee(f.info, call)
			_, fn := f.enclosing(call)
			inQuoting := f.inPackage(evClierr) && (fn == evShellQuote || evShellQuoteArgs[fn] || strings.HasPrefix(fn, "internal/clierr.Command.") || f.path == evBuilderFile)
			switch {
			case inQuoting:
			case evShellQuoteArgs[key]:
				f.finding(r, "4.4", call, "", "clierr.ShellQuoteArgs builds a command from strings; build it with clierr.NewCommand")
			case key == evShellQuote && p.quotedInCommand(f, call, 0):
				f.finding(r, "4.4", call, "", "clierr.ShellQuote output stands in a command; build the command with clierr.NewCommand and Arg or FlagValue")
			}
			return true
		})
	}
}

// textTop climbs from n through parentheses, + chains, the arguments of
// fmt.Sprintf, fmt.Sprint, fmt.Errorf and strings.Join, []string literals,
// append and conversions to a string type. viaArgs is true when it meets the
// positional quoting helper, which only builds commands.
func (p *evProgram) textTop(f *evFile, n ast.Node) (top ast.Expr, viaArgs bool) {
	cur := n
	for {
		switch x := f.parents[cur].(type) {
		case *ast.ParenExpr:
			cur = x
			continue
		case *ast.IndexExpr:
			if x.X == cur && evIsStringType(f.info.TypeOf(x)) {
				cur = x
				continue
			}
		case *ast.BinaryExpr:
			if x.Op == token.ADD {
				cur = x
				continue
			}
		case *ast.CompositeLit:
			if t := f.info.TypeOf(x); t != nil {
				if s, ok := t.Underlying().(*types.Slice); ok && evIsStringType(s.Elem()) {
					cur = x
					continue
				}
			}
		case *ast.CallExpr:
			key, _ := evCallee(f.info, x)
			if evShellQuoteArgs[key] {
				return x, true
			}
			tv := f.info.Types[x.Fun]
			if key == "fmt.Sprintf" || key == "fmt.Sprint" || key == "fmt.Errorf" || key == "strings.Join" || evBuiltin(f.info, x) == "append" || tv.IsType() && evIsStringType(f.info.TypeOf(x)) {
				cur = x
				continue
			}
		}
		e, _ := cur.(ast.Expr)
		return e, false
	}
}

// quoteItems is items for rule 4.4: a []string literal and append read as
// their elements joined by spaces.
func (p *evProgram) quoteItems(f *evFile, e ast.Expr) []evItem {
	switch x := ast.Unparen(e).(type) {
	case *ast.CompositeLit:
		var out []evItem
		for i, el := range x.Elts {
			if i > 0 {
				out = append(out, evItem{lit: " ", isLit: true})
			}
			out = append(out, p.items(f, el, 0)...)
		}
		return out
	case *ast.CallExpr:
		if evBuiltin(f.info, x) == "append" {
			var out []evItem
			for i, a := range x.Args {
				if i > 0 {
					out = append(out, evItem{lit: " ", isLit: true})
				}
				out = append(out, p.items(f, a, 0)...)
			}
			return out
		}
		if key, _ := evCallee(f.info, x); key == "strings.Join" && len(x.Args) == 2 {
			if _, ok := ast.Unparen(x.Args[0]).(*ast.CompositeLit); !ok {
				return p.items(f, x.Args[0], 0)
			}
		}
	}
	return p.items(f, e, 0)
}

// quotedInCommand reports whether node, a ShellQuote call or a variable
// that holds its output, stands in a command: a command start comes before
// it in its text with no clause end between; or the text flows into a local
// variable, an element of one, an append to one or a += on one that is
// read in a command or already holds an open command; or it is returned by
// a function called in a command.
func (p *evProgram) quotedInCommand(f *evFile, node ast.Expr, depth int) bool {
	if depth > 5 {
		return false
	}
	top, viaArgs := p.textTop(f, node)
	if viaArgs {
		return true
	}
	if top == nil {
		return false
	}
	items := p.quoteItems(f, top)
	for i, it := range items {
		if !it.isLit && it.expr != nil && it.expr.Pos() <= node.Pos() && node.End() <= it.expr.End() {
			if p.startBefore(f, items[:i], depth) {
				return true
			}
			break
		}
	}
	root, _ := f.enclosing(top)
	uses := func(target *types.Var) bool {
		if target == nil || target.Parent() == target.Pkg().Scope() {
			return false
		}
		found := false
		ast.Inspect(root, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || found || f.info.Uses[id] != target {
				return !found
			}
			if as, ok := f.parents[id].(*ast.AssignStmt); ok && slices.Contains(as.Lhs, ast.Expr(id)) {
				return true
			}
			found = p.quotedInCommand(f, id, depth+1)
			return true
		})
		return found
	}
	switch s := f.parents[top].(type) {
	case *ast.AssignStmt:
		for i, l := range s.Lhs {
			if len(s.Rhs) == len(s.Lhs) && s.Rhs[i] != top {
				continue
			}
			target, _ := evLhsTarget(f.info, l)
			if s.Tok == token.ADD_ASSIGN {
				if id, ok := l.(*ast.Ident); ok {
					if k, open := p.kindOf(f, id, 0); k.start() && open {
						return true
					}
				}
			}
			if uses(target) {
				return true
			}
		}
	case *ast.ValueSpec:
		for _, name := range s.Names {
			if v, ok := f.info.Defs[name].(*types.Var); ok && uses(v) {
				return true
			}
		}
	case *ast.ReturnStmt:
		if fd, ok := root.(*ast.FuncDecl); ok {
			if fn, ok := f.info.Defs[fd.Name].(*types.Func); ok {
				for _, site := range p.calls[funcKey(fn)] {
					if p.quotedInCommand(site.f, site.call, depth+1) {
						return true
					}
				}
			}
		}
	}
	return false
}

// startBefore reports whether the items before an operand open a command
// that is still open: a command start (a builder result, a clierr.Rancher
// command, the word rancher in constant text, a DirectoryFree command, or a
// variable that holds one), or a string parameter some caller passes a text
// that ends in an open command.
func (p *evProgram) startBefore(f *evFile, before []evItem, depth int) bool {
	for _, it := range slices.Backward(before) {

		if it.isLit {
			tail, ended := evAfterClauseEnd(it.lit)
			if evPlainStart.MatchString(tail) {
				return true
			}
			if ended {
				return false
			}
			continue
		}
		if it.kind == evBuilder || it.kind == evRancher {
			return true
		}
		if call, ok := ast.Unparen(it.expr).(*ast.CallExpr); ok {
			if key, _ := evCallee(f.info, call); key == evDirFree {
				return true
			}
		}
		if p.paramOpensCommand(f, it.expr, depth) {
			return true
		}
	}
	return false
}

// paramOpensCommand reports whether e is a string parameter of its function
// and some direct caller passes a text that ends in an open command.
func (p *evProgram) paramOpensCommand(f *evFile, e ast.Expr, depth int) bool {
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok || depth > 4 {
		return false
	}
	v, ok := f.info.Uses[id].(*types.Var)
	if !ok {
		return false
	}
	root, key := f.enclosing(id)
	fd, ok := root.(*ast.FuncDecl)
	if !ok {
		return false
	}
	idx, _ := evParamIndex(f.info, fd.Type, v)
	if idx < 0 {
		return false
	}
	for _, site := range p.calls[key] {
		if idx >= len(site.call.Args) {
			continue
		}
		items := p.quoteItems(site.f, site.call.Args[idx])
		if p.startBefore(site.f, items, depth+1) {
			return true
		}
	}
	return false
}

var evInstructionFlag = regexp.MustCompile(`(^|[^a-z])(pass|add|run it with|give the exec entry) ([^ ]+ )*?--[a-z][a-z0-9-]*[ =][^ ]*$`)

// evInstructionFragment matches a text whose current clause holds an
// instruction word and ends in a fragment, with no words after it: an operand
// that comes next continues what the reader types.
var evInstructionFragment = regexp.MustCompile(`(^|[^a-z])(pass|add|run it with|give the exec entry) ([^ ]+ )*?<fragment>[ =]*$`)

// scanTexts is rules 4.5 (operands after a command start, several commands
// in one text), the second part of 4.6 (a clierr.Text result followed by a
// step's lead-in) and 4.8 (fragments) on every text expression outside the
// builder's own file.
func (p *evProgram) scanTexts(r *evResults) {
	p.textRoots(func(f *evFile, root ast.Node, items []evItem) {
		if p.exempt(f) {
			return
		}
		open, starts, tail := false, 0, ""
		for i, it := range items {
			if it.isLit {
				if evHasClauseEnd(it.lit) {
					open = false
				}
				tail += it.lit
				if after, ok := evAfterClauseEnd(tail); ok {
					tail = after
				}
				continue
			}
			if it.kind == evTextCall && i+1 < len(items) && items[i+1].isLit {
				next := items[i+1].lit
				if strings.HasPrefix(strings.TrimLeft(next, " "), "then ") || strings.HasPrefix(next, "; then ") {
					f.finding(r, "4.6", it.expr, "", "a clierr.Text result is followed by the next step of a sequence; build the steps as one clierr.Sequence")
				}
			}
			switch {
			case it.kind == evConst:
				if it.expr != nil && evIsWordType(f.info.TypeOf(it.expr)) && !strings.HasSuffix(tail, "--") {
					tail += "--"
				}
				tail += "word"
				continue
			case it.kind == evFragment:
				tail += "<fragment>"
				continue
			case it.kind.start():
				starts++
				if evInstructionFlag.MatchString(tail) {
					f.finding(r, "4.8", it.expr, "", "an instruction asks for a flag value, and "+types.ExprString(it.expr)+" is no fragment; use clierr.FlagFragment, FlagFragmentWord or FlagFragmentAfterWord")
				}
				open = it.open
				tail += "<value>"
				continue
			}
			if open && it.expr != nil {
				f.finding(r, "4.5", it.expr, "", types.ExprString(it.expr)+" follows a command before its clause ends; only constants and builder results may")
			}
			if it.expr != nil && (evInstructionFlag.MatchString(tail) || evInstructionFragment.MatchString(tail)) {
				f.finding(r, "4.8", it.expr, "", "an instruction asks for a flag value, and "+types.ExprString(it.expr)+" is no fragment; use clierr.FlagFragment, FlagFragmentWord or FlagFragmentAfterWord")
			}
			tail += "<value>"
		}
		if starts >= 2 {
			f.finding(r, "4.5", root, "", fmt.Sprintf("%d commands in one text; build them as one clierr.Sequence or one clierr.Text, or list the site as independent commands", starts))
		}
	})
}

// scanHelpers is rule 4.6: a function whose string result holds a command
// start returns a clierr.Text result there.
func (p *evProgram) scanHelpers(r *evResults) {
	for _, f := range p.files {
		if p.exempt(f) {
			continue
		}
		check := func(sig *types.Signature, body *ast.BlockStmt) {
			if sig == nil || body == nil {
				return
			}
			ast.Inspect(body, func(n ast.Node) bool {
				if _, ok := n.(*ast.FuncLit); ok {
					return false
				}
				ret, ok := n.(*ast.ReturnStmt)
				if !ok || len(ret.Results) != sig.Results().Len() {
					return true
				}
				for i, e := range ret.Results {
					rt := sig.Results().At(i).Type()
					if !evIsStringType(rt) || evIsWordType(rt) {
						continue
					}
					if call, ok := ast.Unparen(e).(*ast.CallExpr); ok {
						if key, _ := evCallee(f.info, call); key == evText {
							continue
						}
					}
					if slices.ContainsFunc(p.items(f, e, 0), func(it evItem) bool { return !it.isLit && (it.kind == evBuilder || it.kind == evRancher) }) {
						f.finding(r, "4.6", e, "", "a function returns a command as a string; return a clierr.Command or clierr.Sequence, or build the text with clierr.Text")
					}
				}
				return true
			})
		}
		ast.Inspect(f.file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				if fn, ok := f.info.Defs[x.Name].(*types.Func); ok {
					check(fn.Type().(*types.Signature), x.Body)
				}
			case *ast.FuncLit:
				sig, _ := f.info.TypeOf(x).(*types.Signature)
				check(sig, x.Body)
			}
			return true
		})
	}
}

// wordValues is every constant value a CommandWord argument can have, or
// nil when the scan cannot read one.
func (p *evProgram) wordValues(f *evFile, e ast.Expr) []string {
	if s, ok := constString(f.info, e); ok {
		return []string{s}
	}
	vals, ok := p.leadValues(f, e)
	if !ok {
		return nil
	}
	return vals
}

var evRevokeWords = []string{"delete token ", "delete management-token ", "delete kubeconfig "}

// scanRevokes is rule 4.7: a command whose words start with a revoke's words
// is built only by Revoke, RevokeKubeconfig or FileRevokeKubeconfig.
func (p *evProgram) scanRevokes(r *evResults) {
	for _, f := range p.files {
		if p.exempt(f) {
			continue
		}
		ast.Inspect(f.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			key, _ := evCallee(f.info, call)
			idx := 0
			switch key {
			case evNewCommand, evFileCommand:
			case evFileCmdIn:
				idx = 1
			default:
				return true
			}
			if idx >= len(call.Args) {
				return true
			}
			words := p.wordValues(f, call.Args[idx])
			var cur ast.Node = call
			for len(words) > 0 {
				sel, ok := f.parents[cur].(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Word" {
					break
				}
				next, ok := f.parents[sel].(*ast.CallExpr)
				if !ok || len(next.Args) != 1 {
					break
				}
				more := p.wordValues(f, next.Args[0])
				if more == nil {
					break
				}
				var joined []string
				for _, w := range words {
					for _, m := range more {
						joined = append(joined, w+" "+m)
					}
				}
				words, cur = joined, next
			}
			for _, w := range words {
				norm := strings.Join(strings.Fields(w), " ") + " "
				if slices.ContainsFunc(evRevokeWords, func(rw string) bool { return strings.HasPrefix(norm, rw) }) {
					f.finding(r, "4.7", call, "", "the command "+strings.TrimSpace(norm)+" is a revoke; build it with clierr.Revoke, RevokeKubeconfig or FileRevokeKubeconfig, which keep its list command and its gates")
					break
				}
			}
			return true
		})
	}
}

// scanTypedLine is the part of rule 4.5 that keeps clierr.TypedLine in the
// line refusal hint: every use of the type, NewTypedLine or a method of it
// outside internal/clierr is a finding, which the one row of that file
// covers.
func (p *evProgram) scanTypedLine(r *evResults) {
	for _, f := range p.files {
		if f.inPackage(evClierr) {
			continue
		}
		ast.Inspect(f.file, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			obj := f.info.Uses[id]
			if obj == nil || obj.Pkg() == nil || obj.Pkg().Path() != modulePrefix+evClierr {
				return true
			}
			name := obj.Name()
			if fn, ok := obj.(*types.Func); ok {
				name = strings.TrimPrefix(funcKey(fn), "internal/clierr.")
			}
			if name == "TypedLine" || name == "NewTypedLine" || strings.HasPrefix(name, "TypedLine.") {
				f.finding(r, "4.5", id, "", "clierr."+name+" is used outside the line refusal hint; a TypedLine prints the typed words of a refused line and nothing else")
			}
			return true
		})
	}
}

// evRecoveryField reports whether a field holds one command for a
// consumer that runs it: CredentialRecovery.Command, ConfigRecovery.Command,
// a field named Rerun, or a field whose json name is recovery.
func evRecoveryField(owner *types.Named, name string) bool {
	if owner == nil {
		return false
	}
	switch {
	case name == "Command" && (evNamedIs(owner, evClierr, "CredentialRecovery") || evNamedIs(owner, evClierr, "ConfigRecovery")):
		return true
	case name == "Rerun":
		return true
	}
	st, ok := owner.Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for i := range st.NumFields() {
		if st.Field(i).Name() == name {
			tag, _, _ := strings.Cut(reflect.StructTag(st.Tag(i)).Get("json"), ",")
			return tag == "recovery"
		}
	}
	return false
}

func evHintField(owner *types.Named, name string) bool {
	return name == "Hint" && (evNamedIs(owner, evClierr, "Error") || evNamedIs(owner, evClierr, "PrimaryFailure"))
}

// holdsCommand reports whether a text holds a command start, or a
// clierr.Text call with a command part.
func (p *evProgram) holdsCommand(f *evFile, e ast.Expr) bool {
	return slices.ContainsFunc(p.items(f, e, 0), func(it evItem) bool {
		if it.isLit {
			return false
		}
		if it.kind.start() {
			return true
		}
		call, ok := ast.Unparen(it.expr).(*ast.CallExpr)
		if !ok {
			return false
		}
		key, _ := evCallee(f.info, call)
		return key == evText && textCallHoldsCommand(f, call)
	})
}

// holdsString reports whether e calls String of a Command or a Sequence,
// directly or through a local variable.
func (p *evProgram) holdsString(f *evFile, e ast.Expr, depth int) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if key, _ := evCallee(f.info, x); evStringMethods[key] {
				found = true
			}
		case *ast.Ident:
			v, ok := f.info.Uses[x].(*types.Var)
			if !ok || depth > 3 || v.Pkg() == nil || v.IsField() || v.Parent() == v.Pkg().Scope() {
				return true
			}
			root, _ := f.enclosing(x)
			for _, val := range p.localsOf(f, root)[v] {
				found = found || p.holdsString(f, val, depth+1)
			}
		}
		return !found
	})
	return found
}

// scanCommandFields is rule 4.9: a hint holds no command start but through
// WithHintCommand, and a field that holds one command for a consumer that
// runs it holds no String() of a Command or Sequence.
func (p *evProgram) scanCommandFields(r *evResults) {
	for _, f := range p.files {
		if p.exempt(f) {
			continue
		}
		info := f.info
		checkField := func(owner *types.Named, name string, value ast.Expr) {
			switch {
			case evHintField(owner, name) && p.holdsCommand(f, value):
				f.finding(r, "4.9", value, "", "a Hint holds a command; set it with WithHintCommand, which leaves out a command that cannot be printed and says so")
			case evRecoveryField(owner, name) && p.holdsString(f, value, 0):
				f.finding(r, "4.9", value, "", "the "+name+" field holds one command for a consumer that runs it; use Field(), which is empty for a left-out command, not String()")
			}
		}
		ast.Inspect(f.file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if key, _ := evCallee(info, x); key == "internal/clierr.Error.WithHint" && len(x.Args) == 1 && p.holdsCommand(f, x.Args[0]) {
					f.finding(r, "4.9", x.Args[0], "", "a WithHint argument holds a command; use WithHintCommand, which leaves out a command that cannot be printed and says so")
				}
			case *ast.CompositeLit:
				t := info.TypeOf(x)
				if p, ok := t.(*types.Pointer); ok {
					t = p.Elem()
				}
				owner, _ := types.Unalias(t).(*types.Named)
				if owner == nil {
					return true
				}
				if _, ok := owner.Underlying().(*types.Struct); !ok {
					return true
				}
				for _, el := range x.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if id, ok := kv.Key.(*ast.Ident); ok {
							checkField(owner, id.Name, kv.Value)
						}
					}
				}
			case *ast.AssignStmt:
				if len(x.Lhs) != len(x.Rhs) {
					return true
				}
				for i, l := range x.Lhs {
					sel, ok := ast.Unparen(l).(*ast.SelectorExpr)
					if !ok {
						continue
					}
					if s, ok := info.Selections[sel]; ok && s.Kind() == types.FieldVal {
						checkField(evFieldOwner(s), sel.Sel.Name, x.Rhs[i])
					}
				}
			}
			return true
		})
	}
}

// TestCommandWordsComeOnlyFromConstantsAndTheKindsTable asserts rule 4.1
// over the production tree, outside internal/clierr. No type is declared
// from clierr.CommandWord or clierr.Lead. Every expression of either type is
// a constant, a + of allowed expressions, Word or AliasWord of a kinds.Kind,
// a variable, parameter or field (whose every assignment, argument and
// literal value is such an expression, each checked on its own), an index
// of a container of them, or the result of a module function (whose return
// expressions are checked the same way). A conversion of a value that is
// not constant (outside the bodies of Word and AliasWord), a type assertion
// or type switch case to either type, a value read through a pointer, a
// channel receive, a slice of one and the result of a function outside the
// module fail; so do an instantiation of a type parameter with a type that
// holds one, taking the address of a value of a type that holds one (with &,
// new, or a method with a pointer receiver), an unsafe conversion to or from
// a pointer to one, and reflection on one. Outside internal/engine/kinds, no
// kinds.Kind literal sets Name or Aliases (the zero literal is allowed), no
// assignment writes Name, Aliases or an element of Aliases, no append or copy
// takes Aliases, no pointer to a kinds.Kind (or to a value that holds one)
// goes to a function outside the module, and no reflect call or unsafe
// conversion takes one: a Kind then always holds the table's words.
//
// Limits: "holds" counts fields of nested structs, array, slice, map and
// channel elements and pointers, but not the unexported fields of clierr's
// own types, which reflection cannot set. A function value that writes a
// Kind through a pointer it was given is not seen, nor is a slice of the
// table's aliases copied to a local and written there.
func TestCommandWordsComeOnlyFromConstantsAndTheKindsTable(t *testing.T) {
	t.Parallel()
	evReportRule(t, "4.1")
}

// TestEveryCommandInATextFollowsALead asserts rule 4.2 over the production
// tree: in every clierr.Text call the part right before a Command or a
// Sequence is a clierr.Lead (or the command is the first part); a Lead is
// followed by a Command or a Sequence; a Lead is a constant (or a local
// assigned only constants) that is not empty or blank; and the prose part
// right before a Lead does not end with ": " or the words with, of or run.
// Words that introduce a command then never sit in prose, so a left-out
// command never leaves them behind.
//
// Limits: the prose before a Lead is read only where it ends in constant
// text, directly or through a local assigned once.
func TestEveryCommandInATextFollowsALead(t *testing.T) {
	t.Parallel()
	evReportRule(t, "4.2")
}

// TestTextPartsAreProseLeadsCommandsOrSequences asserts rule 4.3 over the
// production tree: every argument of clierr.Text has the static type string,
// clierr.Lead, clierr.Command or clierr.Sequence, and no call passes a slice
// of parts.
func TestTextPartsAreProseLeadsCommandsOrSequences(t *testing.T) {
	t.Parallel()
	evReportRule(t, "4.3")
}

// TestNoShellQuoteOutputStandsInACommand asserts rule 4.4 over the
// production tree: no clierr.ShellQuote result stands in a command, and the
// positional quoting helper is called only by clierr's quoting code. A
// ShellQuote result stands in a command when a command start comes before it
// in its text with no clause end between (a builder result, a
// clierr.Rancher command or a variable built on it, the word rancher in
// constant text, a DirectoryFree command); when it is assigned to a local
// variable, an element of one (in a loop too), an append to one ([]string
// built by append) or a += on one, and that variable is read in a command or
// already holds an open command; when the string parameter it follows gets a
// command start from a caller; or when a function returns it and a caller
// puts the call in a command.
//
// Limits: a text is followed through at most five such steps, a function
// value is not followed, and a text that leaves its function other than by
// a return (a field, a pointer) is not seen.
func TestNoShellQuoteOutputStandsInACommand(t *testing.T) {
	t.Parallel()
	evReportRule(t, "4.4")
}

// TestEveryCommandStartIsFollowedOnlyByConstantsOrBuilderResults asserts
// rule 4.5 over the production tree. A builder result (a Command or
// Sequence method, a TypedLine.Field) or a clierr.Rancher command starts a
// command; in a text expression (a + chain, fmt.Sprintf,
// fmt.Errorf, fmt.Sprint, strings.Join of a literal, a += on a variable)
// every operand after a start, up to a clause end ("; ", ". ", a newline or
// the end of the text), is a constant, a CommandWord or a builder result. A
// text expression with two or more command starts fails unless its site has
// a row that says its commands are independent; starts inside one
// clierr.Text call or one Sequence count once. clierr.TypedLine is used only
// in the line refusal hint.
//
// Limits: commands built from DetailParts and commands written by a function
// value are not seen. A variable counts as a start when any value assigned
// to it holds one. A fragment result is no command start here: it is a whole
// flag with its value that prose names or asks for, the text after it is
// prose, and rule 4.8 checks where one is needed.
func TestEveryCommandStartIsFollowedOnlyByConstantsOrBuilderResults(t *testing.T) {
	t.Parallel()
	evReportRule(t, "4.5")
}

// TestNoStringHelperReturnsACommand asserts rule 4.6 over the production
// tree: a function (a function literal too) whose string result holds a
// builder result or a clierr.Rancher command returns a clierr.Text call
// there, and a clierr.Text result (or a call of a function that returns only
// one) followed by "then " or "; then " in another text fails, since that
// splits a sequence across texts.
//
// A fragment is not a command start here: it is a flag with its value that
// prose asks for, and clierr.Text takes no fragment part.
//
// Limits: a bare return of named results is not read.
func TestNoStringHelperReturnsACommand(t *testing.T) {
	t.Parallel()
	evReportRule(t, "4.6")
}

// TestRevokesAreBuiltOnlyByTheRevokeBuilders asserts rule 4.7 over the
// production tree: a command built with NewCommand, FileCommand or
// FileCommandIn whose words (with the constant words its Word calls add)
// start with delete token, delete management-token or delete kubeconfig
// fails: Revoke, RevokeKubeconfig and FileRevokeKubeconfig build those,
// with the list command and the gates.
//
// Limits: words read from the kinds table through Word are not known to the
// scan.
func TestRevokesAreBuiltOnlyByTheRevokeBuilders(t *testing.T) {
	t.Parallel()
	evReportRule(t, "4.7")
}

// TestEveryFlagValueAnInstructionAsksForIsAFragment asserts rule 4.8 over
// the production tree: where a clause holds an instruction word (pass, add,
// run it with, give the exec entry) followed by a flag spelling (a literal
// --flag or a CommandWord operand) and then an operand that is not constant,
// that operand is a fragment function's result. An operand that comes right
// after a fragment (only spaces or "=" between) is a fragment function's
// result too: a raw value after a fragment fails. Prose that names a flag
// value without an instruction word, or after words that follow a fragment,
// is not a fragment. The CommandWord spelling operand is trusted only because
// rule 4.1 holds.
//
// Limits: an operand that follows an instruction word with no flag spelling
// and no fragment before it ("pass " + v) is not read; a rule that read it
// would flag present text that is no flag value (config.CAFileArg results in
// token/revocationplan.go and token/token.go, whose callers pass constants)
// and a fragment kept in a local (token/entry.go entryNotNamedError). A raw
// operand after words that follow a fragment is not read either
// (token/noterminal.go names an auth config there, in prose).
func TestEveryFlagValueAnInstructionAsksForIsAFragment(t *testing.T) {
	t.Parallel()
	evReportRule(t, "4.8")
}

// TestCommandOnlyFieldsHoldOnlyACommand asserts rule 4.9 over the production
// tree: a WithHint argument, a Hint value of clierr.Error or
// clierr.PrimaryFailure (in a literal or an assignment) that holds a command
// start (or a clierr.Text call with a command part) fails, since
// WithHintCommand sets a hint; and CredentialRecovery.Command,
// ConfigRecovery.Command, a Rerun field and a field whose json name is
// recovery hold no String() of a Command or Sequence, since Field() is empty
// for a left-out command.
func TestCommandOnlyFieldsHoldOnlyACommand(t *testing.T) {
	t.Parallel()
	evReportRule(t, "4.9")
}
