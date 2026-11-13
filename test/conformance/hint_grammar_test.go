package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unresolved marks a dynamic (non-literal) fragment inside a flattened hint
// string - a value flattenHint could not trace back to a literal (a function
// parameter, a field, a call to a function outside this file set, or a
// call/ident this walk's best-effort resolution gives up on). It uses the
// Unicode object-replacement character, which cannot appear in ordinary Go
// source text, so it can never collide with a real literal fragment and
// never accidentally bridges two literal fragments across a dynamic gap -
// the exact "string concatenation trick" this test must not be dodged by:
// "prefix" + dynamicPart + "suffix" flattens to "prefix￼suffix", never
// "prefixsuffix", so a banned pattern split across the dynamic boundary can
// never falsely disappear, and a real banned pattern wholly inside a
// literal fragment is still caught regardless of how many dynamic calls
// surround it.
const unresolved = "￼"

var (
	placeholderRE     = regexp.MustCompile(`<[^<>\s]+>`)
	optionalBracketRE = regexp.MustCompile(`\[[^\[\]]+\]`)
)

// hintGrammarViolation reports why literal (a hint string, or a candidate
// flattened from one) violates the hint grammar, or "" when it is clean.
// The grammar: every hint is a single paste-valid POSIX-sh command
// carrying only concrete, already-known values, or there is no hint at all.
func hintGrammarViolation(literal string) string {
	switch {
	case strings.Contains(literal, "run:"):
		return `contains a "run:" prefix`
	case strings.Contains(literal, " OR "):
		return `contains an " OR " double-command`
	case optionalBracketRE.MatchString(literal):
		return "contains [optional]-bracket syntax"
	case placeholderRE.MatchString(literal):
		return "contains a <placeholder> for a value that must either be spliced in or dropped"
	case strings.Count(literal, "rancher ") > 1:
		return "names more than one rancher invocation - not single-command-shaped"
	default:
		return ""
	}
}

// flattener resolves WithHint arguments to their literal string content on a
// best-effort basis, across the whole non-test source tree, so a hint built
// by string concatenation, fmt.Sprintf, a local variable, or a call to a
// same-tree helper function still gets its literal pieces checked. Anything
// it cannot trace to a literal becomes the unresolved sentinel rather than
// being silently skipped - see the sentinel's own doc for why that is safe.
type flattener struct {
	funcsByName map[string][]*ast.FuncDecl
	// consts holds every package-level constant by its package's import
	// path and name, so a hint built on a named command (config.LoginCommand,
	// a package's own const) is checked as the literal it is.
	consts map[string]map[string]constDecl
	// scope is the file the expression being walked is in: its package,
	// for an unqualified name, and its imports, for a qualified one.
	scope fileScope
	// funcScope is the file each function was declared in.
	funcScope map[*ast.FuncDecl]fileScope
	// read, when set, is called with every string literal flatten reads.
	read func(*ast.BasicLit)
}

// fileScope is what names in one file refer to: its own package's import
// path, and each import's name to its path.
type fileScope struct {
	pkg     string
	imports map[string]string
}

// constDecl is one package-level constant's value and the file it was
// declared in, whose names its value refers to.
type constDecl struct {
	value ast.Expr
	scope fileScope
}

// scopeOf is the fileScope of f at path (relative to the module root).
func scopeOf(f *ast.File, path string) fileScope {
	s := fileScope{pkg: modulePrefix + filepath.ToSlash(filepath.Dir(path)), imports: map[string]string{}}
	for _, imp := range f.Imports {
		ip, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := ip[strings.LastIndex(ip, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		s.imports[name] = ip
	}
	return s
}

// flatten returns every literal string the expression could evaluate to, as
// far as this best-effort walk can trace it. localAssigns holds every
// right-hand side ever assigned to a local identifier within the enclosing
// function, so `hint := fmt.Sprintf(...); return hint`-shaped code resolves
// too. depth guards against runaway recursion through mutually referential
// helpers; it is not expected to be hit by real code.
func (fl *flattener) flatten(expr ast.Expr, localAssigns map[string][]ast.Expr, depth int) []string {
	if depth > 8 {
		return []string{unresolved}
	}
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return []string{unresolved}
		}
		s, err := strconv.Unquote(e.Value)
		if err != nil {
			return []string{unresolved}
		}
		if fl.read != nil {
			fl.read(e)
		}
		return []string{s}

	case *ast.ParenExpr:
		return fl.flatten(e.X, localAssigns, depth+1)

	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return []string{unresolved}
		}
		lefts := fl.flatten(e.X, localAssigns, depth+1)
		rights := fl.flatten(e.Y, localAssigns, depth+1)
		out := make([]string, 0, len(lefts)*len(rights))
		for _, l := range lefts {
			for _, r := range rights {
				out = append(out, l+r)
			}
		}
		return out

	case *ast.CallExpr:
		if out, ok := fl.flattenBuilder(e, localAssigns, depth); ok {
			return out
		}
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok {
			// clierr.Text joins its parts and clierr.Lead is the words it holds.
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "clierr" && !e.Ellipsis.IsValid() {
				switch {
				case sel.Sel.Name == "Lead" && len(e.Args) == 1:
					return fl.flatten(e.Args[0], localAssigns, depth+1)
				case sel.Sel.Name == "Text":
					out := []string{""}
					for _, a := range e.Args {
						out = joinCandidates(out, "", fl.flatten(a, localAssigns, depth+1))
					}
					return out
				}
			}
			// Command.Field() is the text of the command it is called on.
			if sel.Sel.Name == "Field" && len(e.Args) == 0 {
				return fl.flatten(sel.X, localAssigns, depth+1)
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "fmt" &&
				(sel.Sel.Name == "Sprintf" || sel.Sel.Name == "Errorf") && len(e.Args) > 0 {
				return fl.flatten(e.Args[0], localAssigns, depth+1)
			}
			// clierr.DirectoryFree(x) is the plain command "rancher " + x and
			// clierr.MarkRemedy(x) is x, the command the marker opens, so a
			// hint built with either is checked as the command it prints.
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "clierr" && len(e.Args) == 1 &&
				(sel.Sel.Name == "DirectoryFree" || sel.Sel.Name == "MarkRemedy") {
				rests := fl.flatten(e.Args[0], localAssigns, depth+1)
				if sel.Sel.Name == "DirectoryFree" {
					for i, r := range rests {
						rests[i] = "rancher " + r
					}
				}
				return rests
			}
			// Any other qualified call (clierr.ShellQuote, a package-scoped
			// helper, a method) is out of this walk's reach.
			return []string{unresolved}
		}
		if id, ok := e.Fun.(*ast.Ident); ok {
			if decls, ok := fl.funcsByName[id.Name]; ok {
				var out []string
				for _, fd := range decls {
					out = append(out, fl.flattenReturns(fd, depth+1)...)
				}
				if len(out) > 0 {
					return out
				}
			}
		}
		return []string{unresolved}

	case *ast.SelectorExpr:
		// clierr.Rancher opens every remedy command: the word rancher with
		// the marker the invocation expands, so a hint built on it is
		// checked as the command it prints.
		id, ok := e.X.(*ast.Ident)
		if !ok {
			return []string{unresolved}
		}
		if id.Name == "clierr" && e.Sel.Name == "Rancher" {
			return []string{"rancher"}
		}
		// Only a package's constant: a field or a method of a value is
		// runtime data.
		pkg, ok := fl.scope.imports[id.Name]
		if !ok {
			return []string{unresolved}
		}
		return fl.flattenConst(pkg, e.Sel.Name, depth)

	case *ast.Ident:
		if vals, ok := localAssigns[e.Name]; ok {
			var out []string
			spans := localAssigns[assignSpans(e.Name)]
			for i, v := range vals {
				// A variable declared in a block the use is not in is another
				// variable of the same name.
				if i < len(spans) {
					if span, ok := spans[i].(*ast.BadExpr); ok && span.To != 0 && (e.Pos() < span.From || e.Pos() >= span.To) {
						continue
					}
				}
				out = append(out, fl.flatten(v, localAssigns, depth+1)...)
			}
			if len(out) > 0 {
				return out
			}
		}
		// Inside package clierr the marked word is the unqualified Rancher,
		// read as the word rancher the way the selector clierr.Rancher is.
		if e.Name == "Rancher" && fl.scope.pkg == clierrPath {
			return []string{"rancher"}
		}
		return fl.flattenConst(fl.scope.pkg, e.Name, depth)

	default:
		return []string{unresolved}
	}
}

// builderStarts are the functions that open a command builder chain; the
// methods are the chain's links.
var (
	builderStarts  = map[string]bool{"NewCommand": true, "FileCommand": true, "FileCommandIn": true, "Revoke": true, "RevokeKubeconfig": true, "FileRevokeKubeconfig": true}
	builderMethods = map[string]bool{"Word": true, "Arg": true, "Placeholder": true, "FlagValue": true, "Flag": true, "FlagText": true, "With": true, "On": true}
)

// builderStart is the name of the constructor call e is, whether written
// clierr.NewCommand(...) or, inside package clierr, NewCommand(...).
func builderStart(e *ast.CallExpr) (string, bool) {
	switch f := e.Fun.(type) {
	case *ast.Ident:
		return f.Name, builderStarts[f.Name]
	case *ast.SelectorExpr:
		if id, ok := f.X.(*ast.Ident); ok && id.Name == "clierr" {
			return f.Sel.Name, builderStarts[f.Sel.Name]
		}
	}
	return "", false
}

// flattenBuilder reads a command builder chain as the command it prints: the
// word rancher, each constant word and flag spelling as written, and every
// runtime value as the unresolved sentinel. A placeholder is written as its
// text, so a placeholder in a hint fails the grammar. ok is false when e is
// no builder chain.
func (fl *flattener) flattenBuilder(e *ast.CallExpr, localAssigns map[string][]ast.Expr, depth int) ([]string, bool) {
	if name, ok := builderStart(e); ok {
		out := []string{"rancher"}
		switch name {
		case "NewCommand", "FileCommand":
			out = joinCandidates(out, " ", fl.flatten(e.Args[0], localAssigns, depth+1))
		case "FileCommandIn":
			out = joinCandidates(out, " ", fl.flatten(e.Args[1], localAssigns, depth+1))
		case "Revoke":
			out = []string{"rancher delete token " + unresolved + " --apply --yes"}
		case "RevokeKubeconfig", "FileRevokeKubeconfig":
			out = []string{"rancher delete kubeconfig " + unresolved + " --apply --yes"}
		}
		return out, true
	}
	sel, ok := e.Fun.(*ast.SelectorExpr)
	if !ok || !builderMethods[sel.Sel.Name] {
		return nil, false
	}
	recv, ok := sel.X.(*ast.CallExpr)
	if !ok {
		return nil, false
	}
	base, ok := fl.flattenBuilder(recv, localAssigns, depth)
	if !ok {
		return nil, false
	}
	arg := func(i int) []string {
		if i >= len(e.Args) {
			return []string{unresolved}
		}
		return fl.flatten(e.Args[i], localAssigns, depth+1)
	}
	switch sel.Sel.Name {
	case "Word", "Placeholder", "Flag":
		return joinCandidates(base, " ", arg(0)), true
	case "FlagText":
		return joinCandidates(base, "", arg(0)), true
	case "FlagValue":
		return joinCandidates(joinCandidates(base, " ", arg(0)), " ", []string{unresolved}), true
	case "Arg":
		for range e.Args {
			base = joinCandidates(base, " ", []string{unresolved})
		}
		return base, true
	}
	return base, true
}

// joinCandidates is every combination of a candidate of left, sep and a
// candidate of right.
func joinCandidates(left []string, sep string, right []string) []string {
	out := make([]string, 0, len(left)*len(right))
	for _, l := range left {
		for _, r := range right {
			out = append(out, l+sep+r)
		}
	}
	return out
}

// flattenConst resolves the constant or variable name of package pkg
// through its declaration, walked in the scope of the file that declares
// it; unresolved when the package declares none by that name.
func (fl *flattener) flattenConst(pkg, name string, depth int) []string {
	c, ok := fl.consts[pkg][name]
	if !ok {
		return []string{unresolved}
	}
	outer := fl.scope
	fl.scope = c.scope
	defer func() { fl.scope = outer }()
	return fl.flatten(c.value, nil, depth+1)
}

// flattenReturns resolves every return statement in fd's body against fd's
// own local assignments, unioning every candidate result expression's
// flattened literals - a helper with more than one return path (an enum
// list vs. a formatted command, say) gets every path checked.
func (fl *flattener) flattenReturns(fd *ast.FuncDecl, depth int) []string {
	if fd.Body == nil {
		return nil
	}
	if s, ok := fl.funcScope[fd]; ok {
		outer := fl.scope
		fl.scope = s
		defer func() { fl.scope = outer }()
	}
	assigns := collectLocalAssigns(fd.Body)
	var out []string
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, r := range ret.Results {
			out = append(out, fl.flatten(r, assigns, depth+1)...)
		}
		return true
	})
	return out
}

// collectLocalAssigns scans body for every identifier assignment (`:=`, `=`,
// `+=`, and `var` declarations) and returns every right-hand side ever bound
// to each name. It is deliberately imprecise about control flow - collecting
// every candidate value a name could hold, not just the one live at a given
// call site - because this is a lint-level check, not a compiler: a false
// negative (missing a literal fragment) only means a hint that dodges
// detection here, not a wrongly-failed build; a false positive would.
func collectLocalAssigns(body *ast.BlockStmt) map[string][]ast.Expr {
	out := map[string][]ast.Expr{}
	var stack []ast.Node
	// add records value as assigned to name. The value of a := or var
	// declaration also gets the span of the block it is visible in, in the
	// entry of name's spans at the same index; an assignment to an existing
	// variable has a span with no end.
	add := func(name string, value ast.Expr, declares bool) {
		out[name] = append(out[name], value)
		span := &ast.BadExpr{}
		if declares {
			for _, s := range slices.Backward(stack) {
				switch s.(type) {
				case *ast.BlockStmt, *ast.CaseClause, *ast.CommClause, *ast.IfStmt, *ast.ForStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt:
					span = &ast.BadExpr{From: s.Pos(), To: s.End()}
				default:
					continue
				}
				break
			}
		}
		out[assignSpans(name)] = append(out[assignSpans(name)], span)
	}
	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range s.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || i >= len(s.Rhs) {
					continue
				}
				add(id.Name, s.Rhs[i], s.Tok == token.DEFINE)
			}
		case *ast.ValueSpec:
			for i, name := range s.Names {
				if i < len(s.Values) {
					add(name.Name, s.Values[i], true)
				}
			}
		}
		stack = append(stack, n)
		return true
	})
	return out
}

// assignSpans is the key under which collectLocalAssigns keeps, for each
// value assigned to name, the span of the block a declaration of it is visible
// in: an ast.BadExpr whose To is zero when the value is no declaration. No
// identifier has a NUL in its name, so the key never collides with one.
func assignSpans(name string) string { return name + "\x00" }

// TestHintGrammarConformance is the corpus-grammar tripwire: it statically
// resolves every WithHint(...) call site's argument, and every composite
// literal's Hint: field value, to their literal string content (as far as a
// best-effort AST walk can trace it - see flattener) and fails the build on
// any literal that violates the hint grammar (a "run:" prefix,
// an " OR " double-command, [optional] brackets, a <placeholder>, or more
// than one rancher invocation in one hint). The Hint: field check catches
// hints that reach the wire through a struct literal instead of
// clierr.Error.WithHint - e.g. a domain-specific "hint" JSON field on a
// command's own result payload.
//
// Static AST resolution - rather than grepping the built binary's string
// table or the raw source text - is the one approach here that cannot be
// dodged by string-concatenation tricks: banned substrings split across a
// "prefix" + dynamicCall() + "suffix" expression are joined through the
// dynamic gap by the unresolved sentinel (see its doc), so a pattern that
// spans the gap is preserved, and fmt.Sprintf format strings and same-tree
// helper functions are resolved through their own literal content, not just
// grepped as opaque calls.
//
// A hint this walk cannot trace to any literal at all (built entirely from
// values outside this file set, e.g. a config-file value) produces only the
// unresolved sentinel, which matches no banned pattern - that is this
// approach's known blind spot, not a loophole: nothing here CAN dodge it by
// construction (a real banned literal fragment, wherever it lives in the
// expression tree, is still walked and checked), it is simply the limit of
// what a static, no-execution check can see into pure runtime data.
func TestHintGrammarConformance(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	eachHint(tree.files, tree.paths, func(path string, _ ast.Expr, pos token.Pos, literals []string, _ []*ast.BasicLit) {
		site := path + ":" + strconv.Itoa(tree.fset.Position(pos).Line)
		for _, literal := range literals {
			if reason := hintGrammarViolation(literal); reason != "" {
				t.Errorf("%s: hint %q %s", site, literal, reason)
			}
		}
	})
}

// eachHint calls visit for every WithHint call argument and every composite
// literal Hint: value of the files (paths[i] is files[i]'s path relative to
// the module root) with the expression, the position of its call or key and
// every literal string the flattener resolves it to and every string literal
// of the source it read to get there (in the expression, in a constant or in
// a function it returns from).
func eachHint(files []*ast.File, paths []string, visit func(path string, expr ast.Expr, pos token.Pos, literals []string, read []*ast.BasicLit)) {
	funcsByName := map[string][]*ast.FuncDecl{}
	for _, f := range files {
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok {
				funcsByName[fd.Name.Name] = append(funcsByName[fd.Name.Name], fd)
			}
		}
	}
	fl := &flattener{funcsByName: funcsByName, consts: packageConsts(files, paths), funcScope: map[*ast.FuncDecl]fileScope{}}
	for i, f := range files {
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok {
				fl.funcScope[fd] = scopeOf(f, paths[i])
			}
		}
	}

	for i, f := range files {
		path := paths[i]
		fl.scope = scopeOf(f, path)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			localAssigns := collectLocalAssigns(fd.Body)
			check := func(expr ast.Expr, pos token.Pos) {
				var read []*ast.BasicLit
				fl.read = func(lit *ast.BasicLit) { read = append(read, lit) }
				literals := fl.flatten(expr, localAssigns, 0)
				fl.read = nil
				visit(path, expr, pos, literals, read)
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.CallExpr:
					sel, ok := node.Fun.(*ast.SelectorExpr)
					if ok && (sel.Sel.Name == "WithHint" || sel.Sel.Name == "WithHintCommand") && len(node.Args) > 0 {
						check(node.Args[0], node.Pos())
					}
				case *ast.KeyValueExpr:
					if id, ok := node.Key.(*ast.Ident); ok && id.Name == "Hint" {
						check(node.Value, node.Pos())
					}
				}
				return true
			})
		}
	}
}

// packageConsts collects every package-level constant that has a value, and
// every package-level variable whose value is a string expression, by its
// package's import path and name; paths[i] is files[i]'s path relative to
// the module root. A string expression is made of literals, +, parentheses,
// and identifiers and selectors that name other declarations: a variable
// whose value holds a call, a composite literal or a function literal is
// left out, since the walk cannot read it.
func packageConsts(files []*ast.File, paths []string) map[string]map[string]constDecl {
	out := map[string]map[string]constDecl{}
	for fi, f := range files {
		scope := scopeOf(f, paths[fi])
		if out[scope.pkg] == nil {
			out[scope.pkg] = map[string]constDecl{}
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != len(vs.Values) {
					continue
				}
				for i, n := range vs.Names {
					if gd.Tok == token.VAR && !isStringExpr(vs.Values[i]) {
						continue
					}
					out[scope.pkg][n.Name] = constDecl{value: vs.Values[i], scope: scope}
				}
			}
		}
	}
	return out
}

// isStringExpr reports whether e is built only from string literals, +,
// parentheses, identifiers and selectors of an identifier.
func isStringExpr(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.BasicLit:
		return e.Kind == token.STRING
	case *ast.ParenExpr:
		return isStringExpr(e.X)
	case *ast.BinaryExpr:
		return e.Op == token.ADD && isStringExpr(e.X) && isStringExpr(e.Y)
	case *ast.Ident:
		return true
	case *ast.SelectorExpr:
		_, ok := e.X.(*ast.Ident)
		return ok
	case *ast.CallExpr:
		// X.Field() is the text of the command X, and a builder chain is
		// the command itself.
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Field" && len(e.Args) == 0 {
			return isStringExpr(sel.X) || isBuilderChain(sel.X)
		}
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "clierr" && !e.Ellipsis.IsValid() {
				switch sel.Sel.Name {
				case "Lead":
					return len(e.Args) == 1 && isStringExpr(e.Args[0])
				case "Text":
					return !slices.ContainsFunc(e.Args, func(a ast.Expr) bool { return !isStringExpr(a) && !isBuilderChain(a) })
				}
			}
		}
		return isBuilderChain(e)
	}
	return false
}

// isBuilderChain reports whether e is a command builder chain: a constructor
// call, or a call of a builder method on a chain.
func isBuilderChain(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	if _, ok := builderStart(call); ok {
		return true
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && builderMethods[sel.Sel.Name] && isBuilderChain(sel.X)
}

// TestHintGrammarReadsBuilderChains asserts the hint grammar reads the
// argument of WithHintCommand through the builder chain: constant words and
// flag text as written, values as unresolved, so a placeholder or a second
// command in a hint fails.
func TestHintGrammarReadsBuilderChains(t *testing.T) {
	t.Parallel()
	read := func(src string) []string {
		expr, err := parser.ParseExpr(src)
		require.NoError(t, err)
		fl := &flattener{funcsByName: map[string][]*ast.FuncDecl{}}
		return fl.flatten(expr, nil, 0)
	}
	require.Equal(t, []string{"rancher get users " + unresolved + " --cluster " + unresolved},
		read(`clierr.NewCommand("get users").Arg(id).FlagValue("--cluster", cluster)`))
	require.Equal(t, []string{"rancher get " + unresolved + " --apply"},
		read(`clierr.NewCommand("get").Word(k.Word()).Flag("--apply")`))
	for _, src := range []string{
		// M8.1: a placeholder in flag text.
		`clierr.NewCommand("get users").FlagText(" <id>")`,
		`clierr.NewCommand("config use-server").Placeholder("<entry>")`,
		`clierr.NewCommand("get users").FlagText(" or run: rancher get tokens")`,
	} {
		got := read(src)
		require.Len(t, got, 1, src)
		require.NotEmpty(t, hintGrammarViolation(got[0]), src)
	}
	require.Empty(t, hintGrammarViolation(read(`clierr.NewCommand("get users").Arg(id)`)[0]))
	require.Empty(t, hintGrammarViolation(read(`clierr.Revoke(ref)`)[0]))
	require.Nil(t, func() []string {
		expr, err := parser.ParseExpr(`other.NewCommand("x").Arg(id)`)
		require.NoError(t, err)
		fl := &flattener{funcsByName: map[string][]*ast.FuncDecl{}}
		if out, ok := fl.flattenBuilder(expr.(*ast.CallExpr), nil, 0); ok {
			return out
		}
		return nil
	}(), "a chain that does not start at the builder is not read as one")
}

// TestHintFlattenerReadsDirectoryFreeAndMarkRemedy asserts a hint built with
// clierr.DirectoryFree(x) is read as "rancher " + x and one built with
// clierr.MarkRemedy(x) as x, so each is checked as the command it prints.
func TestHintFlattenerReadsDirectoryFreeAndMarkRemedy(t *testing.T) {
	t.Parallel()
	fl := &flattener{funcsByName: map[string][]*ast.FuncDecl{}}
	directoryFree, err := parser.ParseExpr(`clierr.DirectoryFree("get pod --help")`)
	require.NoError(t, err)
	require.Equal(t, []string{"rancher get pod --help"}, fl.flatten(directoryFree, nil, 0))

	marked, err := parser.ParseExpr(`clierr.MarkRemedy("rancher auth login 'p'")`)
	require.NoError(t, err)
	require.Equal(t, []string{"rancher auth login 'p'"}, fl.flatten(marked, nil, 0))

	// A dynamic argument stays unresolved on both sides of the join, and a
	// second invocation in one hint is still one too many.
	built, err := parser.ParseExpr(`clierr.DirectoryFree(path + " --help") + ", then " + clierr.MarkRemedy("rancher config status")`)
	require.NoError(t, err)
	got := fl.flatten(built, nil, 0)
	require.Equal(t, []string{"rancher " + unresolved + " --help, then rancher config status"}, got)
	require.NotEmpty(t, hintGrammarViolation(got[0]))
}

// TestHintFlattenerReadsTheRemedyMarkerAsRancher asserts a hint built on
// clierr.Rancher is checked as the command it prints: two invocations in
// one hint are still one too many.
func TestHintFlattenerReadsTheRemedyMarkerAsRancher(t *testing.T) {
	t.Parallel()
	expr, err := parser.ParseExpr(`clierr.Rancher + " auth login 'p', then " + clierr.Rancher + " config status"`)
	require.NoError(t, err)
	fl := &flattener{funcsByName: map[string][]*ast.FuncDecl{}}
	got := fl.flatten(expr, nil, 0)
	require.Equal(t, []string{"rancher auth login 'p', then rancher config status"}, got)
	require.NotEmpty(t, hintGrammarViolation(got[0]))

	// A named command is read through its declaration, in its own package;
	// a field of the same name is runtime data.
	decl, err := parser.ParseFile(token.NewFileSet(), "internal/config/x.go", `package config
import "github.com/rancher/rancher-cli/internal/clierr"
const LoginCommand = clierr.Rancher + " auth login"
const Hint = "rancher auth login, then rancher config status"`, 0)
	require.NoError(t, err)
	user, err := parser.ParseFile(token.NewFileSet(), "internal/token/y.go", `package token
import "github.com/rancher/rancher-cli/internal/config"`, 0)
	require.NoError(t, err)
	fl = &flattener{funcsByName: map[string][]*ast.FuncDecl{},
		consts: packageConsts([]*ast.File{decl, user}, []string{"internal/config/x.go", "internal/token/y.go"}),
		scope:  scopeOf(user, "internal/token/y.go")}
	named, err := parser.ParseExpr(`config.LoginCommand + ", then " + clierr.Rancher + " config status"`)
	require.NoError(t, err)
	got = fl.flatten(named, nil, 0)
	require.Equal(t, []string{"rancher auth login, then rancher config status"}, got)
	require.NotEmpty(t, hintGrammarViolation(got[0]))
	field, err := parser.ParseExpr(`ce.Hint`)
	require.NoError(t, err)
	require.Equal(t, []string{unresolved}, fl.flatten(field, nil, 0))
	unqualified, err := parser.ParseExpr(`Hint`)
	require.NoError(t, err)
	require.Equal(t, []string{unresolved}, fl.flatten(unqualified, nil, 0), "another package's constant is not this one's")

	// A named command that is a variable is read the same way.
	varDecl, err := parser.ParseFile(token.NewFileSet(), "internal/config/x.go", `package config
import "github.com/rancher/rancher-cli/internal/clierr"
var LoginCommand = clierr.Rancher + " auth login"
var Built = fmt.Sprint("a")
var Table = map[string]string{"a": "b"}`, 0)
	require.NoError(t, err)
	vars := packageConsts([]*ast.File{varDecl, user}, []string{"internal/config/x.go", "internal/token/y.go"})
	fl = &flattener{funcsByName: map[string][]*ast.FuncDecl{}, consts: vars, scope: scopeOf(user, "internal/token/y.go")}
	got = fl.flatten(named, nil, 0)
	require.Equal(t, []string{"rancher auth login, then rancher config status"}, got)
	require.NotEmpty(t, hintGrammarViolation(got[0]))
	assert.Contains(t, vars["github.com/rancher/rancher-cli/internal/config"], "LoginCommand")
	assert.NotContains(t, vars["github.com/rancher/rancher-cli/internal/config"], "Built", "a variable whose value is a call is not read")
	assert.NotContains(t, vars["github.com/rancher/rancher-cli/internal/config"], "Table", "a variable whose value is a composite literal is not read")
}

// TestHintFlattenerReadsTheRealAuthStatusRemedy asserts the production
// tree's AuthStatusRemedy, which package clierr declares from its own
// unqualified Rancher, flattens to exactly the command it prints, both
// inside the package and from another one.
func TestHintFlattenerReadsTheRealAuthStatusRemedy(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	funcs := map[string][]*ast.FuncDecl{}
	fl := &flattener{funcsByName: funcs, consts: packageConsts(tree.files, tree.paths)}

	fl.scope = fileScope{pkg: clierrPath}
	inside, err := parser.ParseExpr(`AuthStatusRemedy`)
	require.NoError(t, err)
	require.Equal(t, []string{"rancher auth status"}, fl.flatten(inside, nil, 0))

	fl.scope = fileScope{pkg: modulePrefix + "internal/token", imports: map[string]string{"clierr": clierrPath}}
	outside, err := parser.ParseExpr(`clierr.AuthStatusRemedy`)
	require.NoError(t, err)
	require.Equal(t, []string{"rancher auth status"}, fl.flatten(outside, nil, 0))
}
