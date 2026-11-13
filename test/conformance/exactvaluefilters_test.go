package conformance

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// evFilters is the closed set of whole-text filters: each removes or
// replaces characters anywhere in a text it is given.
var evFilters = map[string]bool{
	"internal/output.Printable":                  true,
	"internal/output.PrintableLines":             true,
	"internal/runtime.PrintableRemedies":         true,
	"internal/clierr.WithoutUnsafeToDisplay":     true,
	"internal/engine/catalog.Values.Filter":      true,
	"internal/engine/catalog.Values.FilterError": true,
	"internal/engine/catalog.Values.filterFunc":  true,
}

// evFilterAnyText are the filters a text that holds commands may pass: the
// two keep the remedy marker and cannot change a value NotQuotable accepts,
// and every result of the catalog values filter holds <hidden>, which no
// command accepts as a value and no Kubernetes name can hold.
var evFilterAnyText = map[string]bool{
	"internal/output.PrintableLines":             true,
	"internal/runtime.PrintableRemedies":         true,
	"internal/engine/catalog.Values.Filter":      true,
	"internal/engine/catalog.Values.FilterError": true,
	"internal/engine/catalog.Values.filterFunc":  true,
}

// evFilterCommandWriters are the functions whose filtered text may hold a
// command whatever the filter: the table, wide and name writers of result
// data, which is not a remedy channel.
var evFilterCommandWriters = map[string]bool{
	"internal/output.writePlainTable": true,
	"internal/output.renderName":      true,
}

// evFilterRow is the allowlist row of guard 3b for one function and one
// filter. command is whether the filtered text may hold a command. relays,
// when set, is why a relayed text the walk reaches in a row whose text holds
// no command still holds none.
type evFilterRow struct {
	fn, filter string
	command    bool
	relays     string
	reason     string
}

// scanFilters records every reference to a filter of the closed set and,
// for a call, what the walk of its first argument reaches.
func (p *evProgram) scanFilters(r *evResults) {
	for _, f := range p.files {
		ast.Inspect(f.file, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			fn, ok := f.info.Uses[id].(*types.Func)
			if !ok || fn.Pkg() == nil || !evFilters[funcKey(fn)] {
				return true
			}
			_, encl := f.enclosing(id)
			ref := evFilterRef{at: f.at(id), fn: encl, filter: funcKey(fn)}
			if evCalledName(f, id) {
				ref.call = true
				var cur ast.Node = id
				for {
					cur = f.parents[cur]
					if call, ok := cur.(*ast.CallExpr); ok {
						if len(call.Args) > 0 {
							ref.relayed = p.relayed(f, call.Args[0], 0, map[string]bool{})
						}
						break
					}
				}
			}
			r.refs = append(r.refs, ref)
			return true
		})
	}
}

// relayed walks a filter's argument back within its function (locals, +,
// fmt.Sprintf, fmt.Sprint, fmt.Errorf, strings.Join, conversions, indexes,
// and the return expressions of module functions that return a string) and
// lists each text that may hold a command: a builder result, a clierr.Text
// result, a fragment, a clierr.Rancher command, an Error.Message or
// Error.Hint field, a Warnings() element, an error's Error(), and the
// stored error text of a revoke outcome.
func (p *evProgram) relayed(f *evFile, e ast.Expr, depth int, seen map[string]bool) []string {
	if e == nil || depth > 8 {
		return nil
	}
	e = ast.Unparen(e)
	if _, ok := constString(f.info, e); ok {
		return nil
	}
	hit := func(what string) []string { return []string{f.at(e) + ": " + what} }
	switch x := e.(type) {
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return append(p.relayed(f, x.X, depth+1, seen), p.relayed(f, x.Y, depth+1, seen)...)
		}
	case *ast.IndexExpr:
		return p.relayed(f, x.X, depth+1, seen)
	case *ast.CallExpr:
		if tv, ok := f.info.Types[x.Fun]; ok && tv.IsType() && len(x.Args) == 1 {
			return p.relayed(f, x.Args[0], depth+1, seen)
		}
		key, fn := evCallee(f.info, x)
		if fn == nil {
			return nil
		}
		sig := fn.Type().(*types.Signature)
		if recv := sig.Recv(); recv != nil {
			t := recv.Type()
			if evIsClierrType(t, "Command") || evIsClierrType(t, "Sequence") || evIsClierrType(t, "TypedLine") {
				return hit("a builder result (" + key + ")")
			}
			switch {
			case fn.Name() == "Warnings":
				return hit("a Warnings() element (" + key + ")")
			case fn.Name() == "Error" && sig.Params().Len() == 0 && sig.Results().Len() == 1 && evIsStringType(sig.Results().At(0).Type()):
				return hit("an error's Error() (" + types.ExprString(x.Fun) + ")")
			}
		}
		switch {
		case key == evText || evFragmentFuncs[key]:
			return hit("a builder result (" + key + ")")
		case key == evMarkRemedy:
			return hit("a clierr.Rancher command (" + key + ")")
		case key == "fmt.Sprintf" || key == "fmt.Sprint" || key == "fmt.Errorf" || key == "strings.Join":
			var out []string
			for _, a := range x.Args {
				out = append(out, p.relayed(f, a, depth+1, seen)...)
			}
			return out
		}
		d, ok := p.funcs[key]
		if !ok || seen[key] || !evStringish(f.info.TypeOf(x)) {
			return nil
		}
		seen[key] = true
		var out []string
		ast.Inspect(d.decl.Body, func(n ast.Node) bool {
			if _, ok := n.(*ast.FuncLit); ok {
				return false
			}
			if ret, ok := n.(*ast.ReturnStmt); ok {
				for _, res := range ret.Results {
					if evStringish(d.f.info.TypeOf(res)) {
						out = append(out, p.relayed(d.f, res, depth+1, seen)...)
					}
				}
			}
			return true
		})
		return out
	case *ast.SelectorExpr:
		if sel, ok := f.info.Selections[x]; ok {
			if sel.Kind() != types.FieldVal {
				return nil
			}
			owner := evFieldOwner(sel)
			switch {
			case evNamedIs(owner, evClierr, "Error") && (x.Sel.Name == "Message" || x.Sel.Name == "Hint"):
				return hit("an Error." + x.Sel.Name + " field")
			case evNamedIs(owner, evClierr, "RevokeOutcome") && x.Sel.Name == "Error":
				return hit("stored error text (RevokeOutcome.Error)")
			}
			return nil
		}
		if v, ok := f.info.Uses[x.Sel].(*types.Var); ok && v.Pkg() != nil {
			if k := objectKey(v); k == evRancherVar || k == evMarkerVar || p.members[k] {
				return hit("a clierr.Rancher command (" + k + ")")
			}
		}
	case *ast.Ident:
		v, ok := f.info.Uses[x].(*types.Var)
		if !ok || v.Pkg() == nil {
			return nil
		}
		if k := objectKey(v); k == evRancherVar || k == evMarkerVar || p.members[k] {
			return hit("a clierr.Rancher command (" + k + ")")
		}
		if v.Parent() == v.Pkg().Scope() || v.IsField() {
			return nil
		}
		key := fmt.Sprintf("%s:%d", f.path, v.Pos())
		if seen[key] {
			return nil
		}
		seen[key] = true
		root, _ := f.enclosing(x)
		var out []string
		for _, val := range p.localsOf(f, root)[v] {
			out = append(out, p.relayed(f, val, depth+1, seen)...)
		}
		return out
	}
	return nil
}

// evCheckFilterRows checks the references against the rows.
func evCheckFilterRows(refs []evFilterRef, rows []evFilterRow) []string {
	var problems []string
	matched := make([]bool, len(rows))
	relayed := make([]bool, len(rows))
	for _, ref := range refs {
		i := slices.IndexFunc(rows, func(row evFilterRow) bool { return row.fn == ref.fn && row.filter == ref.filter })
		if i < 0 {
			problems = append(problems, fmt.Sprintf("%s (%s): a reference to %s with no allowlist row", ref.at, ref.fn, ref.filter))
			continue
		}
		matched[i] = true
		row := rows[i]
		if row.command {
			continue
		}
		if !ref.call {
			relayed[i] = true
			if row.relays == "" {
				problems = append(problems, fmt.Sprintf("%s (%s): %s is used as a value, so its text cannot be walked; give the row a reason", ref.at, ref.fn, ref.filter))
			}
			continue
		}
		for _, h := range ref.relayed {
			relayed[i] = true
			if row.relays == "" {
				problems = append(problems, fmt.Sprintf("%s (%s): the text %s filters may hold a command, which it would change: %s; use runtime.PrintableRemedies, or give the row a reason", ref.at, ref.fn, ref.filter, h))
			}
		}
	}
	for i, row := range rows {
		switch {
		case !matched[i]:
			problems = append(problems, fmt.Sprintf("the row %s / %s matched no reference; remove it", row.fn, row.filter))
		case row.command && !evFilterAnyText[row.filter] && !evFilterCommandWriters[row.fn]:
			problems = append(problems, fmt.Sprintf("the row %s / %s says the text may hold a command; only PrintableLines, PrintableRemedies, the catalog values filter and the table, wide and name writers may filter one", row.fn, row.filter))
		case row.relays != "" && !relayed[i]:
			problems = append(problems, fmt.Sprintf("the row %s / %s gives a reason for relayed text the walk no longer reaches; remove the reason", row.fn, row.filter))
		}
	}
	return problems
}

// TestEveryWholeTextFilterReferenceHasAReviewedRow asserts guard 3b over the
// production tree: every reference to output.Printable,
// output.PrintableLines, runtime.PrintableRemedies,
// clierr.WithoutUnsafeToDisplay and the catalog values filter
// (Values.Filter, Values.FilterError, Values.filterFunc), called or used as
// a value, has a row in evFilterRows named by its enclosing function and the
// filter, and every row matches a reference. A row whose text may hold a
// command is allowed only for PrintableLines, PrintableRemedies, the values
// filter and the table, wide and name writers. For a row whose text holds no
// command, the filter's argument is walked back within its function (locals,
// +, fmt.Sprintf, and the return expressions of module functions that return
// a string), and the test fails when the walk reaches a builder result, a
// clierr.Text result, a fragment, a clierr.Rancher command, an Error.Message
// or Error.Hint field, a Warnings() element, an error's Error() or stored
// error text (RevokeOutcome.Error), unless the row gives the reason the
// relayed text holds no command.
//
// Limits: a filter function outside the closed set is not seen, and a text
// that reaches the filter through a parameter is not walked past the
// parameter.
func TestEveryWholeTextFilterReferenceHasAReviewedRow(t *testing.T) {
	t.Parallel()
	r, err := evProductionResults()
	require.NoError(t, err)
	for _, p := range evCheckFilterRows(r.refs, evFilterRows) {
		t.Error(p)
	}
}
