package conformance

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pasteFixturesDir = "test/conformance/testdata/remedypaste"

	// pasteFlagsUnknown stands for the flags a call supplies at run time.
	pasteFlagsUnknown = "\uFFFB"

	pasteCheckSchema = "schema"
	pasteCheckEnd    = "end"
	pasteCheckDryRun = "dryrun"
)

var (
	pastePlaceholder = regexp.MustCompile(`^<[a-z-]+>$`)
	pasteAngle       = regexp.MustCompile(`<[^<>\s]*>`)
	pasteVerb        = regexp.MustCompile(`%(?:\[(\d+)\])?[-+# 0]*(?:\d+|\*)?(?:\.\d+)?([a-zA-Z%])`)
)

// pasteFlag and pasteCmd are one flag and one command of the schema golden.
type pasteFlag struct {
	takesValue bool
	required   bool
	name       string
}

type pasteCmd struct {
	name        string
	args        int
	requiredArg int
	flags       map[string]pasteFlag
	mutating    bool
	server      bool
	gates       []string
}

type pasteSchema struct {
	byName map[string]*pasteCmd
	global map[string]struct {
		flag      pasteFlag
		appliesTo string
	}
	groups  map[string]bool
	topWord *regexp.Regexp
}

// loadPasteSchema reads the commands, their flags and the global flags from
// the schema manifest golden. The global flags `-o/--output` and
// `-c/--config` are split into their spellings.
func loadPasteSchema(golden string) (*pasteSchema, error) {
	b, err := os.ReadFile(golden)
	if err != nil {
		return nil, err
	}
	type flagJSON struct {
		Name      string   `json:"name"`
		Aliases   []string `json:"aliases"`
		Type      string   `json:"type"`
		Required  bool     `json:"required"`
		AppliesTo string   `json:"appliesTo"`
	}
	var m struct {
		Commands []struct {
			Name         string     `json:"name"`
			Aliases      []string   `json:"aliases"`
			CommandType  string     `json:"commandType"`
			ServerAccess string     `json:"serverAccess"`
			Gates        []string   `json:"gates"`
			Flags        []flagJSON `json:"flags"`
			Args         []struct {
				Required bool `json:"required"`
			} `json:"args"`
		} `json:"commands"`
		GlobalFlags []flagJSON `json:"globalFlags"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	s := &pasteSchema{byName: map[string]*pasteCmd{}, groups: map[string]bool{},
		global: map[string]struct {
			flag      pasteFlag
			appliesTo string
		}{}}
	var tops []string
	for _, g := range m.GlobalFlags {
		for spelling := range strings.SplitSeq(g.Name, "/") {
			name := strings.TrimLeft(spelling, "-")
			s.global[name] = struct {
				flag      pasteFlag
				appliesTo string
			}{pasteFlag{takesValue: g.Type != "bool", name: name}, g.AppliesTo}
		}
	}
	for _, c := range m.Commands {
		cmd := &pasteCmd{name: c.Name, args: len(c.Args), flags: map[string]pasteFlag{},
			mutating: c.CommandType == "mutating", server: c.ServerAccess != "none", gates: c.Gates}
		for _, a := range c.Args {
			if a.Required {
				cmd.requiredArg++
			}
		}
		for _, f := range c.Flags {
			pf := pasteFlag{takesValue: f.Type != "bool", required: f.Required, name: f.Name}
			cmd.flags[f.Name] = pf
			for _, a := range f.Aliases {
				cmd.flags[a] = pf
			}
		}
		words := strings.Fields(c.Name)
		s.byName[c.Name] = cmd
		if len(words) == 2 {
			s.groups[words[0]] = true
			for _, a := range c.Aliases {
				s.byName[words[0]+" "+a] = cmd
			}
		}
		if !slices.Contains(tops, words[0]) {
			tops = append(tops, words[0])
		}
	}
	slices.SortFunc(tops, func(a, b string) int { return len(b) - len(a) })
	s.topWord = regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])rancher (?:--[a-z][a-z-]*|(?:` + quoteAll(tops) + `)` + wordEnd + `)`)
	return s, nil
}

var pasteSchemaOfTree = sync.OnceValues(func() (*pasteSchema, error) { return loadPasteSchema(schemaGolden) })

// pasteReader reads an expression as the text it prints, with the unresolved
// sentinel for a value known only at run time.
type pasteReader struct {
	fl     *flattener
	scope  fileScope
	locals map[string][]ast.Expr
	// funcs indexes the functions of the scanned tree by package and name.
	funcs map[string]pasteFunc
	// bound holds the readings of the arguments of the call being read, by
	// parameter name, when the reader is inside the function it calls.
	bound map[string]string
	depth int
}

// pasteFunc is a function of the scanned tree and the file scope it was
// declared in.
type pasteFunc struct {
	decl  *ast.FuncDecl
	scope fileScope
}

func isClierrCall(call *ast.CallExpr, name string) bool {
	switch f := call.Fun.(type) {
	case *ast.SelectorExpr:
		id, ok := f.X.(*ast.Ident)
		return ok && id.Name == "clierr" && f.Sel.Name == name
	case *ast.Ident:
		return f.Name == name
	}
	return false
}

func (r *pasteReader) read(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return unresolved
		}
		return unquoted(e)
	case *ast.ParenExpr:
		return r.read(e.X)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return unresolved
		}
		return r.read(e.X) + r.read(e.Y)
	case *ast.CallExpr:
		return r.readCall(e)
	case *ast.SelectorExpr:
		if id, ok := e.X.(*ast.Ident); ok && id.Name == "clierr" && e.Sel.Name == "Rancher" {
			return "rancher"
		}
		return r.flat(e)
	case *ast.Ident:
		if v, ok := r.bound[e.Name]; ok {
			return v
		}
		if _, local := r.locals[e.Name]; local {
			return unresolved
		}
		if e.Name == "Rancher" && r.scope.pkg == clierrPath {
			return "rancher"
		}
		return r.flat(e)
	}
	return unresolved
}

func (r *pasteReader) flat(e ast.Expr) string {
	r.fl.scope = r.scope
	out := r.fl.flatten(e, nil, 0)
	if len(out) == 0 {
		return unresolved
	}
	return out[0]
}

// pasteBuilderStarts are the constructors whose chain the reader reads as the
// command it prints. Revoke and the sequence builders are read where they are
// built, in package clierr.
var pasteBuilderStarts = map[string]bool{"NewCommand": true, "FileCommand": true, "FileCommandIn": true}

// builderChain reports whether e is a builder chain rooted at one of
// pasteBuilderStarts, followed by any number of builder methods and an
// optional String or Field.
func builderChain(e ast.Expr) bool {
	for {
		call, ok := ast.Unparen(e).(*ast.CallExpr)
		if !ok {
			return false
		}
		if name, ok := builderStart(call); ok {
			return pasteBuilderStarts[name]
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (!builderMethods[sel.Sel.Name] && sel.Sel.Name != "String" && sel.Sel.Name != "Field") {
			return false
		}
		e = sel.X
	}
}

// chainRooted reports whether e is a builder chain that starts at one of
// pasteBuilderStarts, or at a local variable assigned such a chain, through
// builder methods only.
func (r *pasteReader) chainRooted(e ast.Expr) bool {
	for {
		call, ok := ast.Unparen(e).(*ast.CallExpr)
		if !ok {
			return false
		}
		if name, ok := builderStart(call); ok {
			return pasteBuilderStarts[name]
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		if id, ok := ast.Unparen(sel.X).(*ast.Ident); ok {
			return builderMethods[sel.Sel.Name] && slices.ContainsFunc(r.locals[id.Name], builderChain)
		}
		if !builderMethods[sel.Sel.Name] && sel.Sel.Name != "String" && sel.Sel.Name != "Field" {
			return false
		}
		e = sel.X
	}
}

// chainBase is the constructor call a builder chain starts at, or the name of
// the local variable it starts at.
func chainBase(e ast.Expr) (start bool, name string) {
	for {
		call, ok := ast.Unparen(e).(*ast.CallExpr)
		if !ok {
			return false, ""
		}
		if name, ok := builderStart(call); ok {
			return pasteBuilderStarts[name], ""
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !builderMethods[sel.Sel.Name] {
			return false, ""
		}
		if id, ok := ast.Unparen(sel.X).(*ast.Ident); ok {
			return false, id.Name
		}
		e = sel.X
	}
}

// partialChains returns the right-hand sides of the assignments to a local
// that holds a command being built in steps: each extension of the variable by
// itself (x = x.Flag(...)), and the chain it starts as when another chain
// continues from the variable. Such a chain is not the command that prints;
// the chain that continues it reads as that command, and the extensions made
// in branches are not seen.
func partialChains(root ast.Node) map[ast.Expr]bool {
	type assignment struct {
		name string
		rhs  ast.Expr
	}
	var assigns []assignment
	continued := map[string]int{}
	ast.Inspect(root, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE || n.Tok == token.ASSIGN {
				for i, lhs := range n.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && i < len(n.Rhs) {
						assigns = append(assigns, assignment{id.Name, n.Rhs[i]})
					}
				}
			}
		case *ast.ValueSpec:
			for i, id := range n.Names {
				if i < len(n.Values) {
					assigns = append(assigns, assignment{id.Name, n.Values[i]})
				}
			}
		case *ast.CallExpr:
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && builderMethods[sel.Sel.Name] {
				if id, ok := ast.Unparen(sel.X).(*ast.Ident); ok {
					continued[id.Name]++
				}
			}
		}
		return true
	})
	own := map[string]int{}
	for _, a := range assigns {
		ast.Inspect(a.rhs, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && builderMethods[sel.Sel.Name] {
					if id, ok := ast.Unparen(sel.X).(*ast.Ident); ok && id.Name == a.name {
						own[a.name]++
					}
				}
			}
			return true
		})
	}
	out := map[ast.Expr]bool{}
	for _, a := range assigns {
		start, name := chainBase(a.rhs)
		if name == a.name || (start && continued[a.name] > own[a.name]) {
			out[a.rhs] = true
		}
	}
	return out
}

// readCommand reads a command builder chain, or a local variable assigned
// one, as the command it prints: the words, then the
// positional values in single quotes, then the flags, as the builder writes a
// command whose values do not start with a dash.
func (r *pasteReader) readCommand(e ast.Expr) (string, bool) {
	e = ast.Unparen(e)
	if id, ok := e.(*ast.Ident); ok {
		// A local that is assigned a chain and then extended with more
		// builder calls reads as its first chain: the calls added in a branch
		// are not seen.
		i := slices.IndexFunc(r.locals[id.Name], builderChain)
		if i < 0 {
			return "", false
		}
		return r.readCommand(r.locals[id.Name][i])
	}
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	if name, ok := builderStart(call); ok {
		switch name {
		case "NewCommand", "FileCommand":
			return "rancher " + r.read(call.Args[0]), true
		case "FileCommandIn":
			return "rancher --config '" + unresolved + "' " + r.read(call.Args[1]), true
		}
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	switch sel.Sel.Name {
	case "String", "Field", "On":
		return r.readCommand(sel.X)
	}
	if !builderMethods[sel.Sel.Name] {
		return "", false
	}
	chain := []*ast.CallExpr{call}
	var base ast.Expr
	for {
		x := ast.Unparen(chain[len(chain)-1].Fun.(*ast.SelectorExpr).X)
		recv, isCall := x.(*ast.CallExpr)
		if !isCall {
			base = x
			break
		}
		if _, isStart := builderStart(recv); isStart {
			base = recv
			break
		}
		if rs, ok := recv.Fun.(*ast.SelectorExpr); !ok || !builderMethods[rs.Sel.Name] {
			return "", false
		}
		chain = append(chain, recv)
	}
	start, ok := r.readCommand(base)
	if !ok {
		return "", false
	}
	var words, values, flags strings.Builder
	for _, c := range slices.Backward(chain) {
		switch c.Fun.(*ast.SelectorExpr).Sel.Name {
		case "Word":
			words.WriteString(" " + r.read(c.Args[0]))
		case "Arg":
			for range c.Args {
				values.WriteString(" '" + unresolved + "'")
			}
		case "Placeholder":
			values.WriteString(" " + r.read(c.Args[0]))
		case "Flag":
			flags.WriteString(" " + r.read(c.Args[0]))
		case "FlagValue":
			flags.WriteString(" " + r.read(c.Args[0]) + " '" + unresolved + "'")
		case "FlagText":
			flags.WriteString(r.read(c.Args[0]))
		case "With":
			flags.WriteString(pasteFlagsUnknown)
		}
	}
	return start + words.String() + values.String() + flags.String(), true
}

// readText reads the parts of a call of clierr.Text as one text: prose and
// leads as written, a command as a chain reads.
func (r *pasteReader) readText(call *ast.CallExpr) string {
	var sb strings.Builder
	for _, a := range call.Args {
		a = ast.Unparen(a)
		if c, ok := a.(*ast.CallExpr); ok && isClierrCall(c, "Lead") && len(c.Args) == 1 {
			sb.WriteString(r.read(c.Args[0]))
			continue
		}
		if text, ok := r.readCommand(a); ok {
			sb.WriteString(text)
			continue
		}
		sb.WriteString(r.read(a))
	}
	return sb.String()
}

func (r *pasteReader) readCall(call *ast.CallExpr) string {
	if isClierrCall(call, "Text") {
		return r.readText(call)
	}
	if text, ok := r.readCommand(call); ok {
		return text
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "fmt" && (sel.Sel.Name == "Sprintf" || sel.Sel.Name == "Errorf") && len(call.Args) > 0 {
			return r.readFormat(call.Args[0], call.Args[1:])
		}
		if sel.Sel.Name == "Flags" && len(call.Args) == 0 {
			return pasteFlagsUnknown
		}
	}
	switch {
	case isClierrCall(call, "ShellQuote") && len(call.Args) == 1:
		return "'" + unresolved + "'"
	case isClierrCall(call, "ShellQuoteArgs") && len(call.Args) >= 1:
		var sb strings.Builder
		if call.Ellipsis.IsValid() {
			sb.WriteString(" '" + unresolved + "'")
		} else {
			for range call.Args[1:] {
				sb.WriteString(" '" + unresolved + "'")
			}
		}
		sb.WriteString(r.readFlags(call.Args[0]))
		return sb.String()
	case isClierrCall(call, "MarkRemedy") && len(call.Args) == 1:
		return r.read(call.Args[0])
	case isClierrCall(call, "DirectoryFree") && len(call.Args) == 1:
		return "rancher " + r.read(call.Args[0])
	}
	if pf, ret := r.inlinable(call); ret != nil {
		return r.readCallee(pf, ret, call)
	}
	return unresolved
}

// readFlags reads the flags argument of clierr.ShellQuoteArgs: a quoted value
// reads as one quoted word, and any other operand the reader cannot resolve
// reads as flags it does not know.
func (r *pasteReader) readFlags(e ast.Expr) string {
	var sb strings.Builder
	for _, it := range pasteItems(e) {
		if it.expr == nil {
			sb.WriteString(it.lit)
			continue
		}
		text := r.read(it.expr)
		if call, ok := ast.Unparen(it.expr).(*ast.CallExpr); ok && isClierrCall(call, "ShellQuote") {
			sb.WriteString(text)
			continue
		}
		if strings.Contains(text, unresolved) {
			text = pasteFlagsUnknown
		}
		sb.WriteString(text)
	}
	return sb.String()
}

// inlinable returns the function a call names when the tree holds exactly one
// by that name and its body is a run of guards that return early, then one
// return of a single value: the text the call prints is that value.
func (r *pasteReader) inlinable(call *ast.CallExpr) (pasteFunc, *ast.ReturnStmt) {
	var key string
	switch f := call.Fun.(type) {
	case *ast.Ident:
		key = r.scope.pkg + "." + f.Name
	case *ast.SelectorExpr:
		id, ok := f.X.(*ast.Ident)
		if !ok {
			return pasteFunc{}, nil
		}
		pkg, ok := r.scope.imports[id.Name]
		if !ok {
			return pasteFunc{}, nil
		}
		key = pkg + "." + f.Sel.Name
	default:
		return pasteFunc{}, nil
	}
	pf, ok := r.funcs[key]
	if !ok || pf.decl.Body == nil || len(pf.decl.Body.List) == 0 || r.depth >= 6 {
		return pasteFunc{}, nil
	}
	list := pf.decl.Body.List
	for _, st := range list[:len(list)-1] {
		if _, ok := st.(*ast.IfStmt); !ok {
			return pasteFunc{}, nil
		}
	}
	ret, ok := list[len(list)-1].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return pasteFunc{}, nil
	}
	return pf, ret
}

// readCallee reads the returned value of a function with its parameters bound
// to the readings of the arguments of the call.
func (r *pasteReader) readCallee(pf pasteFunc, ret *ast.ReturnStmt, call *ast.CallExpr) string {
	bound := map[string]string{}
	i := 0
	for _, field := range pf.decl.Type.Params.List {
		for _, name := range field.Names {
			if i < len(call.Args) && !call.Ellipsis.IsValid() {
				bound[name.Name] = r.read(call.Args[i])
			} else {
				bound[name.Name] = unresolved
			}
			i++
		}
	}
	callee := &pasteReader{fl: r.fl, scope: pf.scope, funcs: r.funcs, bound: bound, depth: r.depth + 1}
	return callee.read(ret.Results[0])
}

func (r *pasteReader) readFormat(format ast.Expr, args []ast.Expr) string {
	text := r.read(format)
	next := 0
	return pasteVerb.ReplaceAllStringFunc(text, func(verb string) string {
		m := pasteVerb.FindStringSubmatch(verb)
		if m[2] == "%" {
			return "%"
		}
		i := next
		if m[1] != "" {
			i, _ = strconv.Atoi(m[1])
			i--
		} else {
			next++
		}
		if i < 0 || i >= len(args) {
			return unresolved
		}
		if m[2] == "q" {
			return `"` + unresolved + `"`
		}
		return r.read(args[i])
	})
}

// pasteShown is a reading as the entries write it.
func pasteShown(s string) string {
	return strings.NewReplacer(unresolved, "{v}", pasteFlagsUnknown, "{f}").Replace(s)
}

// pasteProblem is one defect of one command.
type pasteProblem struct {
	check, msg string
}

// pasteParsed is what one command in a reading is, and what is wrong with it.
type pasteParsed struct {
	cmd      *pasteCmd
	problems []pasteProblem
	opaque   bool
	// tokens are the words of the command after the command name's words,
	// and awaitingValue is true when the last one is a flag that takes a
	// value and has none yet.
	flags         map[string]bool
	positionals   int
	awaitingValue bool
	// unknown is true when a word the reader cannot resolve stands where only
	// flags can follow, or is part of a flag.
	unknown bool
	end     int
}

type pasteToken struct {
	text string
	glue string
}

// pasteTokens reads the words of a command from s[start:]: words are cut at
// white space, a ";" or a line break; a quoted part belongs to its word; a
// closing delimiter opened before the command ends it. Punctuation at the end
// of a word is its glue.
func pasteTokens(s string, start int, closer byte) (toks []pasteToken, ends []int) {
	i := start
	for i < len(s) {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) || s[i] == ';' || s[i] == '\n' || (closer != 0 && s[i] == closer) {
			return
		}
		j := i
	word:
		for j < len(s) {
			switch c := s[j]; {
			case c == ' ' || c == '\t' || c == ';' || c == '\n':
				break word
			case closer == '`' && c == '`':
				break word
			case c == '\'':
				if closer == '\'' && j > i && s[j-1] != '=' {
					break word
				}
				k := strings.IndexByte(s[j+1:], '\'')
				if k < 0 {
					j = len(s)
				} else {
					j += k + 2
				}
			default:
				j++
			}
		}
		text := s[i:j]
		glue := ""
		for len(text) > 0 && strings.ContainsRune(",.):", rune(text[len(text)-1])) && !strings.HasSuffix(text, "'") {
			glue = text[len(text)-1:] + glue
			text = text[:len(text)-1]
		}
		toks = append(toks, pasteToken{text, glue})
		i = j
		ends = append(ends, i)
		if glue != "" {
			return
		}
	}
	return
}

// parsePasteCommand reads the command that starts at s[start:], the word
// rancher already skipped. opener is the character just before the word
// rancher.
func (sc *pasteSchema) parse(s string, start int, opener byte, hint bool) pasteParsed {
	var closer byte
	if opener == '`' || opener == '\'' {
		closer = opener
	}
	toks, ends := pasteTokens(s, start, closer)
	out := pasteParsed{flags: map[string]bool{}, end: start}
	problem := func(check, format string, a ...any) {
		out.problems = append(out.problems, pasteProblem{check, fmt.Sprintf(format, a...)})
	}
	i := 0
	// Flags before the command name.
	for i < len(toks) && strings.HasPrefix(toks[i].text, "-") {
		name := strings.TrimLeft(toks[i].text, "-")
		name, _, hasValue := strings.Cut(name, "=")
		g, ok := sc.global[name]
		i++
		if ok && g.flag.takesValue && !hasValue {
			i++
		}
	}
	if i >= len(toks) {
		return out
	}
	if strings.Contains(toks[i].text, unresolved) {
		out.opaque = true
		out.end = ends[len(ends)-1]
		return out
	}
	var cmd *pasteCmd
	used := 0
	if i+1 < len(toks) {
		if strings.Contains(toks[i+1].text, unresolved) && sc.groups[toks[i].text] {
			out.opaque = true
			out.end = ends[len(ends)-1]
			return out
		}
		if c, ok := sc.byName[toks[i].text+" "+toks[i+1].text]; ok {
			cmd, used = c, 2
		}
	}
	if cmd == nil {
		if c, ok := sc.byName[toks[i].text]; ok {
			cmd, used = c, 1
		}
	}
	if cmd == nil {
		problem(pasteCheckSchema, "%q names no command", toks[i].text)
		return out
	}
	out.cmd = cmd
	lastIndex := i + used - 1
	if len(toks) > lastIndex {
		out.end = ends[lastIndex]
	}
	for _, p := range toks[:lastIndex+1] {
		if p.glue != "" {
			problem(pasteCheckEnd, "%q glued after the command name", p.glue)
		}
	}
	rest := toks[i+used:]
	ended := false
	for k := 0; k < len(rest); k++ {
		t := rest[k]
		ti := i + used + k
		consume := func() { out.end = ends[ti] }
		text := t.text
		switch {
		case ended:
		case text == "rancher":
			problem(pasteCheckEnd, "a second command in the same clause")
			return out
		case text == "--":
			ended = true
			consume()
			continue
		case strings.Contains(text, pasteFlagsUnknown):
			out.unknown = true
			consume()
			continue
		case strings.HasPrefix(text, "-") && len(text) > 1 && strings.Contains(text, unresolved):
			out.unknown = true
			consume()
			continue
		case text == unresolved && out.positionals >= cmd.args:
			out.unknown = true
			consume()
			continue
		case strings.HasPrefix(text, "-") && len(text) > 1:
			name, _, hasValue := strings.Cut(strings.TrimLeft(text, "-"), "=")
			f, ok := cmd.flags[name]
			if !ok {
				if g, gok := sc.global[name]; gok && (g.appliesTo == "all" || (g.appliesTo == "mutating" && cmd.mutating) || (g.appliesTo == "server" && cmd.server)) {
					f, ok = g.flag, true
				}
			}
			if !ok && (name == "help" || name == "h") {
				f, ok = pasteFlag{}, true
			}
			if !ok {
				problem(pasteCheckSchema, "the flag %q is not a flag of %q", text, cmd.name)
				consume()
				out.flags[name] = true
				out.awaitingValue = k == len(rest)-1 && !hasValue
				continue
			}
			out.flags[f.name] = true
			out.flags[name] = true
			consume()
			if f.takesValue && !hasValue {
				if k+1 < len(rest) {
					k++
					ti++
					consume()
					if rest[k].glue != "" {
						problem(pasteCheckEnd, "%q glued after the last word", rest[k].glue)
						return out
					}
				} else {
					out.awaitingValue = true
				}
			}
			if t.glue != "" {
				problem(pasteCheckEnd, "%q glued after the last word", t.glue)
				return out
			}
			continue
		}
		// A positional.
		if out.positionals >= cmd.args {
			return out
		}
		out.positionals++
		consume()
		if t.glue != "" {
			problem(pasteCheckEnd, "%q glued after the last word", t.glue)
			return out
		}
		if strings.HasPrefix(text, "<") && !pastePlaceholder.MatchString(text) {
			problem(pasteCheckSchema, "the placeholder %q is not <lowercase-words>", text)
		}
	}
	for _, a := range pasteAngle.FindAllString(s[start:out.end], -1) {
		if !pastePlaceholder.MatchString(a) {
			problem(pasteCheckSchema, "the placeholder %q is not <lowercase-words>", a)
		} else if hint {
			problem(pasteCheckSchema, "the hint holds the placeholder %q", a)
		}
	}
	return out
}

// check runs the three checks of one command against its schema entry.
func (sc *pasteSchema) check(p *pasteParsed, text string, start int, gated []string, hasFlagsUnknown bool) {
	if p.cmd == nil || p.opaque {
		return
	}
	cmd := p.cmd
	hasFlagsUnknown = hasFlagsUnknown || p.unknown
	if !hasFlagsUnknown && !p.flags["help"] && !p.flags["h"] {
		for name, f := range cmd.flags {
			// An alias key shares its flag's entry; the parser records the
			// flag's own name whichever spelling the text uses, so the
			// flag's own key alone decides whether it was given.
			if name != f.name {
				continue
			}
			if f.required && !p.flags[f.name] {
				p.problems = append(p.problems, pasteProblem{pasteCheckSchema, fmt.Sprintf("%q needs the required flag --%s", cmd.name, f.name)})
			}
		}
		if p.positionals < cmd.requiredArg && !strings.Contains(text[start:p.end], unresolved) {
			p.problems = append(p.problems, pasteProblem{pasteCheckSchema, fmt.Sprintf("%q needs %d argument(s), the text gives %d", cmd.name, cmd.requiredArg, p.positionals)})
		}
	}
	if cmd.mutating && !p.flags["help"] && !p.flags["h"] {
		if gated != nil {
			for _, g := range gated {
				if !hasFlagsUnknown && !p.flags[strings.TrimLeft(g, "-")] {
					p.problems = append(p.problems, pasteProblem{pasteCheckDryRun, "a gated command must carry " + g})
				}
			}
			return
		}
		if !p.flags["dry-run"] {
			p.problems = append(p.problems, pasteProblem{pasteCheckDryRun, "a mutating command carries --dry-run"})
		}
		for _, g := range cmd.gates {
			if !hasFlagsUnknown && p.flags[strings.TrimLeft(g, "-")] {
				p.problems = append(p.problems, pasteProblem{pasteCheckDryRun, "a mutating command carries no " + g})
			}
		}
	}
}

// pasteMarkerWord is the word rancher before a space, where the text an
// expression built on the marker prints names a command.
var pasteMarkerWord = regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])rancher `)

// builtOnMarker reports whether the expression holds clierr.Rancher, or a
// variable built on it: its reading names a command whatever word follows
// rancher.
func builtOnMarker(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "clierr" && x.Sel.Name == "Rancher" {
				found = true
			}
			for _, key := range builtOnRancherReviewed {
				if strings.HasSuffix(key, "."+x.Sel.Name) {
					found = true
				}
			}
		case *ast.Ident:
			for _, key := range builtOnRancherReviewed {
				if strings.HasSuffix(key, "."+x.Name) {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// pasteSite is one text expression of a production file whose reading holds
// a rancher command.
type pasteSite struct {
	file, fn string
	line     int
	expr     string
	text     string
	hint     bool
	problems []pasteProblem
	flagsHit bool
	// gatesHeld is the gate flags the commands of the text carry.
	mutating bool
}

func (s pasteSite) shown() string { return pasteShown(s.text) }

func (s pasteSite) has(check string) bool {
	return slices.ContainsFunc(s.problems, func(p pasteProblem) bool { return p.check == check })
}

// pasteGated is a site that keeps the gates of its command: the file, the
// function, the reading of the text, the gate flags it must carry and why.
type pasteGated struct {
	file, fn, text string
	gates          []string
	why            string
}

// pasteTextException is a reading that fails one check and stays, with the
// reason.
type pasteTextException struct {
	file, check, text, why string
}

// pasteUnresolved is a site whose flags a call supplies at run time: the
// source text of the expression, why, and the hand test that renders the text
// and parses it.
type pasteUnresolved struct {
	file, fn, expr, why, test string
}

// pasteHints marks the expressions that are a hint: the argument of
// WithHint or WithHintCommand and the value of Hint or NotFoundHint.
func pasteHints(f *ast.File) map[ast.Expr]bool {
	out := map[ast.Expr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "WithHint" || sel.Sel.Name == "WithHintCommand") && len(node.Args) > 0 {
				out[ast.Unparen(node.Args[0])] = true
			}
		case *ast.KeyValueExpr:
			if id, ok := node.Key.(*ast.Ident); ok && (id.Name == "Hint" || id.Name == "NotFoundHint") {
				out[ast.Unparen(node.Value)] = true
			}
		}
		return true
	})
	return out
}

// pasteSkipped marks the subtrees whose text is help prose: the values of
// Examples, Desc, Usage and ArgDesc, and the tables groupUsages and
// groupDescs.
func pasteSkipped(n ast.Node) bool {
	switch node := n.(type) {
	case *ast.KeyValueExpr:
		if id, ok := node.Key.(*ast.Ident); ok && remedyHelpKeys[id.Name] {
			return true
		}
	case *ast.ValueSpec:
		for _, name := range node.Names {
			if remedyHelpTables[name.Name] {
				return true
			}
		}
	case *ast.AssignStmt:
		for _, lhs := range node.Lhs {
			if id, ok := lhs.(*ast.Ident); ok && remedyHelpTables[id.Name] {
				return true
			}
		}
	}
	return false
}

// isTextExpression reports whether e is a string literal, a chain joined by
// + or a call of fmt.Sprintf or fmt.Errorf.
func isTextExpression(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.BasicLit:
		return e.Kind == token.STRING
	case *ast.BinaryExpr:
		return e.Op == token.ADD
	case *ast.CallExpr:
		sel, ok := e.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == "fmt" && (sel.Sel.Name == "Sprintf" || sel.Sel.Name == "Errorf")
	}
	return false
}

// pasteScanner reads the production files.
type pasteScanner struct {
	fset   *token.FileSet
	files  []*ast.File
	paths  []string
	schema *pasteSchema
	fl     *flattener
	gated  []pasteGated
	funcs  map[string]pasteFunc
}

func newPasteScanner(fset *token.FileSet, files []*ast.File, paths []string, schema *pasteSchema, gated []pasteGated) *pasteScanner {
	funcs := map[string][]*ast.FuncDecl{}
	index := map[string]pasteFunc{}
	ambiguous := map[string]bool{}
	var keptFiles []*ast.File
	var keptPaths []string
	for i, f := range files {
		if strings.HasPrefix(paths[i], "internal/remedytest/") {
			continue
		}
		keptFiles, keptPaths = append(keptFiles, f), append(keptPaths, paths[i])
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				funcs[fd.Name.Name] = append(funcs[fd.Name.Name], fd)
				if fd.Recv == nil {
					scope := scopeOf(f, paths[i])
					key := scope.pkg + "." + fd.Name.Name
					if _, dup := index[key]; dup {
						ambiguous[key] = true
					}
					index[key] = pasteFunc{decl: fd, scope: scope}
				}
			}
		}
	}
	for key := range ambiguous {
		delete(index, key)
	}
	return &pasteScanner{fset: fset, files: keptFiles, paths: keptPaths, schema: schema, gated: gated, funcs: index,
		fl: &flattener{funcsByName: funcs, consts: packageConsts(keptFiles, keptPaths), funcScope: map[*ast.FuncDecl]fileScope{}}}
}

// sites reads every text expression of every file and returns those whose
// reading holds a rancher command.
func (ps *pasteScanner) sites() []pasteSite {
	var out []pasteSite
	for i, f := range ps.files {
		path := ps.paths[i]
		hints := pasteHints(f)
		scope := scopeOf(f, path)
		visit := func(fn string, locals map[string][]ast.Expr, root ast.Node) {
			r := &pasteReader{fl: ps.fl, scope: scope, locals: locals, funcs: ps.funcs}
			partial := partialChains(root)
			ast.Inspect(root, func(n ast.Node) bool {
				if n == nil || pasteSkipped(n) {
					return false
				}
				e, ok := n.(ast.Expr)
				if !ok {
					return true
				}
				if partial[e] {
					return false
				}
				viaCall := false
				builder := false
				if call, isCall := e.(*ast.CallExpr); isCall && !isTextExpression(e) {
					if builder = isClierrCall(call, "Text") || r.chainRooted(call); !builder {
						if _, ret := r.inlinable(call); ret == nil {
							return true
						}
						viaCall = true
					}
				} else if !isTextExpression(e) {
					return true
				}
				text := r.read(e)
				marker := !viaCall && (builder || builtOnMarker(e))
				if ps.schema.topWord.MatchString(text) || (marker && pasteMarkerWord.MatchString(text)) {
					out = append(out, ps.site(path, fn, e, text, hints[ast.Unparen(e)] || hints[e], marker, viaCall))
					return false
				}
				return viaCall
			})
		}
		for _, d := range f.Decls {
			switch decl := d.(type) {
			case *ast.FuncDecl:
				if decl.Body != nil {
					visit(decl.Name.Name, collectLocalAssigns(decl.Body), decl.Body)
				}
			case *ast.GenDecl:
				visit("", nil, decl)
			}
		}
	}
	return out
}

func (ps *pasteScanner) site(path, fn string, e ast.Expr, text string, hint, marker, viaCall bool) pasteSite {
	s := pasteSite{file: path, fn: fn, line: ps.fset.Position(e.Pos()).Line, expr: types.ExprString(e), text: text, hint: hint}
	flagsUnknown := strings.Contains(text, pasteFlagsUnknown)
	s.flagsHit = flagsUnknown
	var gated []string
	if g := ps.gatedFor(s); g != nil {
		gated = g.gates
	}
	words := ps.schema.topWord
	if marker {
		words = pasteMarkerWord
	}
	for _, loc := range words.FindAllStringIndex(text, -1) {
		word := strings.Index(text[loc[0]:], "rancher") + loc[0]
		var opener byte
		if word > 0 {
			opener = text[word-1]
		}
		p := ps.schema.parse(text, word+len("rancher "), opener, hint)
		ps.schema.check(&p, text, word+len("rancher "), gated, flagsUnknown)
		if p.unknown {
			s.flagsHit = true
		}
		if p.cmd != nil && p.cmd.mutating {
			s.mutating = true
		}
		s.problems = append(s.problems, p.problems...)
		// What follows the command.
		if p.cmd != nil {
			rest := strings.TrimLeft(text[p.end:], " \t")
			closer := ""
			if opener == '`' || opener == '\'' {
				closer = string(opener)
			}
			if rest != "" && !strings.HasPrefix(rest, ";") && !strings.HasPrefix(rest, "\n") && (closer == "" || !strings.HasPrefix(rest, closer)) && !slices.ContainsFunc(s.problems, func(q pasteProblem) bool { return q.check == pasteCheckEnd }) {
				s.problems = append(s.problems, pasteProblem{pasteCheckEnd, fmt.Sprintf("text after the command: %q", pasteShown(rest))})
			}
		}
	}
	if viaCall {
		s.problems = slices.DeleteFunc(s.problems, func(q pasteProblem) bool { return q.check == pasteCheckDryRun })
	}
	if s.flagsHit && !slices.ContainsFunc(pasteUnresolvedList, func(u pasteUnresolved) bool {
		return u.file == path && u.fn == fn && u.expr == s.expr
	}) {
		s.problems = append(s.problems, pasteProblem{pasteCheckSchema, "the flags come from a call the reader cannot resolve"})
	}
	return s
}

func (ps *pasteScanner) gatedFor(s pasteSite) *pasteGated {
	for i := range ps.gated {
		if ps.gated[i].file == s.file && ps.gated[i].fn == s.fn && ps.gated[i].text == s.shown() {
			return &ps.gated[i]
		}
	}
	return nil
}

// pasteItem is one operand of a text expression: a literal part or an
// expression.
type pasteItem struct {
	lit  string
	expr ast.Expr
}

// pasteItems is the operands of a chain joined by +, or the pieces of a
// fmt.Sprintf format with its arguments, in the order they print.
func pasteItems(e ast.Expr) []pasteItem {
	e = ast.Unparen(e)
	switch e := e.(type) {
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			return append(pasteItems(e.X), pasteItems(e.Y)...)
		}
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			return []pasteItem{{lit: unquoted(e)}}
		}
	case *ast.CallExpr:
		if isTextExpression(e) && len(e.Args) > 0 {
			lit, ok := ast.Unparen(e.Args[0]).(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return []pasteItem{{expr: e}}
			}
			format := unquoted(lit)
			var out []pasteItem
			last, next := 0, 1
			for _, loc := range pasteVerb.FindAllStringSubmatchIndex(format, -1) {
				verb := format[loc[4]:loc[5]]
				out = append(out, pasteItem{lit: format[last:loc[0]]})
				last = loc[1]
				if verb == "%" {
					out = append(out, pasteItem{lit: "%"})
					continue
				}
				if loc[2] >= 0 || next >= len(e.Args) {
					out = append(out, pasteItem{lit: unresolved})
					continue
				}
				out = append(out, pasteItem{expr: e.Args[next]})
				next++
			}
			return append(out, pasteItem{lit: format[last:]})
		}
	}
	return []pasteItem{{expr: e}}
}

// pasteTree is the scan of the production tree, computed once.
var pasteScanOfTree = sync.OnceValues(func() (*pasteScanner, error) {
	tree := parseSourceTree()
	if tree.err != nil {
		return nil, tree.err
	}
	schema, err := pasteSchemaOfTree()
	if err != nil {
		return nil, err
	}
	return newPasteScanner(tree.fset, tree.files, tree.paths, schema, pasteGatedList), nil
})

var pasteSitesOfTree = sync.OnceValue(func() []pasteSite {
	ps, err := pasteScanOfTree()
	if err != nil {
		return nil
	}
	return ps.sites()
})

func pasteTextExceptions() []pasteTextException {
	return slices.Clone(pasteLocalTextExceptions)
}

// applyPasteExceptions matches each exception to one site of its file and
// returns the sites no exception covers and the exceptions that cover no
// site. A new, a removed and a swapped text each leave something over.
func applyPasteExceptions(sites []pasteSite, check string, exceptions []pasteTextException) (left []pasteSite, unused []pasteTextException) {
	used := make([]bool, len(sites))
	for _, e := range exceptions {
		if e.check != check {
			continue
		}
		found := false
		for i, s := range sites {
			if !used[i] && s.has(check) && s.file == e.file && s.shown() == e.text {
				used[i], found = true, true
				break
			}
		}
		if !found {
			unused = append(unused, e)
		}
	}
	for i, s := range sites {
		if s.has(check) && !used[i] {
			left = append(left, s)
		}
	}
	return left, unused
}

func reportPasteSites(t *testing.T, sites []pasteSite, check string) {
	t.Helper()
	for _, s := range sites {
		for _, p := range s.problems {
			if p.check == check {
				t.Errorf("%s:%d (%s) %q: %s", s.file, s.line, s.fn, s.shown(), p.msg)
			}
		}
	}
}

func reportPasteExceptions(t *testing.T, unused []pasteTextException) {
	t.Helper()
	for _, e := range unused {
		t.Errorf("%s: the %s exception for %q matches no text; remove it or fix its text", e.file, e.check, e.text)
	}
}

// pasteChecked reads the tree for one check: the sites it reports that no
// exception covers, and the exceptions that cover none.
func pasteChecked(t *testing.T, check string) ([]pasteSite, []pasteTextException) {
	t.Helper()
	_, err := pasteScanOfTree()
	require.NoError(t, err)
	return applyPasteExceptions(pasteSitesOfTree(), check, pasteTextExceptions())
}

// TestEveryRemedyCommandMatchesTheCommandSchema asserts every remedy command
// a production file writes names a command of the schema manifest golden,
// gives only flags that command or a global flag that applies to it has,
// gives every required flag and argument and no more arguments than the
// command takes, and writes a placeholder only as <lowercase-words> and
// never in a hint. The commands read are the literals that hold the word
// rancher and a command word or a flag, and every expression built on
// clierr.Rancher or a variable built on it, whatever word follows rancher (an
// unknown command name fails), as one text: the operands of a + chain, the
// format of fmt.Sprintf with its arguments read, a call of a builder.
//
// Limits: the golden's required bit differs from the command in places
// (config use-server with no name fails though name is optional in the
// golden; revoke needs a scope; a bare group name needs --type group, which
// the golden marks optional); a site that names such a command has a hand
// test. A text a variable carries to another expression is read where it is
// written. A command built in steps in a local variable reads where a chain
// continues from the variable, with the calls made in a branch (x = x.Flag(...))
// not seen. A call reads as the text it returns only for a function (no method,
// no function value) declared once in its package, with a body of if
// statements that return early followed by one return, at most 6 calls deep;
// only that last return is read. Any other call reads as one word, and text
// glued after it is not seen. A value the reader cannot resolve stands for one
// word, and an operand inside the flags argument of clierr.ShellQuoteArgs
// reads as flags it does not know, which needs an unresolved-site entry with a
// hand test. An operand the reader cannot resolve that is written against the
// word before it, with no space (a flag value or a command word), is read as
// part of that word: flags it carries are not seen, and a command word read
// that way leaves its command unchecked. A plain literal names a command only
// through a top-level command word of the golden, and an expression counts as
// built on the marker only when clierr.Rancher or a reviewed variable appears
// in it: an unknown word after rancher is not seen in a plain literal or after
// a call that returns the marker. No scan reads what a placeholder stands
// for. A flag value that is exactly a registered flag spelling stays refused
// by the CLI; the scan does not read flag values.
func TestEveryRemedyCommandMatchesTheCommandSchema(t *testing.T) {
	t.Parallel()
	left, unused := pasteChecked(t, pasteCheckSchema)
	reportPasteSites(t, left, pasteCheckSchema)
	reportPasteExceptions(t, unused)
}

// TestEveryRemedyCommandEndsItsSentence asserts text follows a remedy
// command only after the end of the text, a semicolon, a line break or the
// closing backtick or single quote opened before it: no comma, period,
// parenthesis or word glued after its last argument, no second command in the
// clause, no comment after it.
func TestEveryRemedyCommandEndsItsSentence(t *testing.T) {
	t.Parallel()
	left, unused := pasteChecked(t, pasteCheckEnd)
	reportPasteSites(t, left, pasteCheckEnd)
	reportPasteExceptions(t, unused)
}

// TestEveryMutatingRemedyCarriesDryRun asserts a remedy command of a
// mutating command carries --dry-run and none of the command's gate flags,
// unless its site is in the gated list, where it carries every gate flag. A
// site whose flags the reader does not know is still required to carry
// --dry-run; its gate flags are not read.
//
// Limits: a call of a builder whose flags come from a parameter is not checked
// for --dry-run or gate flags at the call site: a caller can pass gate flags
// unseen.
func TestEveryMutatingRemedyCarriesDryRun(t *testing.T) {
	t.Parallel()
	left, unused := pasteChecked(t, pasteCheckDryRun)
	reportPasteSites(t, left, pasteCheckDryRun)
	reportPasteExceptions(t, unused)
}

// TestRemedyPasteExceptionsAreReviewed asserts every entry of the lists a
// check reads matches one site: an entry that matches none, a gated site that
// no longer holds its gated command, an unresolved-site entry whose site is
// gone and a named hand test its package does not hold each fail.
func TestRemedyPasteExceptionsAreReviewed(t *testing.T) {
	t.Parallel()
	_, err := pasteScanOfTree()
	require.NoError(t, err)
	sites := pasteSitesOfTree()
	for _, check := range []string{pasteCheckSchema, pasteCheckEnd, pasteCheckDryRun} {
		_, unused := applyPasteExceptions(sites, check, pasteTextExceptions())
		reportPasteExceptions(t, unused)
	}
	for _, g := range pasteGatedList {
		i := slices.IndexFunc(sites, func(s pasteSite) bool { return s.file == g.file && s.fn == g.fn && s.shown() == g.text })
		if i < 0 {
			t.Errorf("%s (%s): the gated entry for %q matches no text", g.file, g.fn, g.text)
			continue
		}
		for _, gate := range g.gates {
			if !strings.Contains(sites[i].text, gate) {
				t.Errorf("%s (%s): the gated site %q no longer carries %s", g.file, g.fn, g.text, gate)
			}
		}
	}
	for _, u := range pasteUnresolvedList {
		i := slices.IndexFunc(sites, func(s pasteSite) bool { return s.file == u.file && s.fn == u.fn && s.expr == u.expr && s.flagsHit })
		if i < 0 {
			t.Errorf("%s (%s): the unresolved-site entry for %q matches no site", u.file, u.fn, u.expr)
			continue
		}
		assert.True(t, pasteHasTest(t, filepath.Dir(u.file), u.test), "%s: the package holds no test %s", u.file, u.test)
	}
}

// pasteHasTest reports whether a test file in dir declares the function.
func pasteHasTest(t *testing.T, dir, name string) bool {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	require.NoError(t, err)
	for _, m := range matches {
		f, err := parser.ParseFile(token.NewFileSet(), m, nil, 0)
		require.NoError(t, err)
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name {
				return true
			}
		}
	}
	return false
}

// pasteFixtureCases lists each fixture package, the check it feeds and the
// lines of its source that must be reported.
var pasteFixtureCases = []struct {
	dir, check string
}{
	{"schema", pasteCheckSchema},
	{"end", pasteCheckEnd},
	{"dryrun", pasteCheckDryRun},
}

// pasteFixtureScan reads one fixture package: the lines the check reports.
func pasteFixtureScan(t *testing.T, dir, check string) (got, want []int) {
	t.Helper()
	schema, err := pasteSchemaOfTree()
	require.NoError(t, err)
	fset := token.NewFileSet()
	path := filepath.Join(pasteFixturesDir, dir, "fixture.go")
	src, err := os.ReadFile(path)
	require.NoError(t, err)
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	require.NoError(t, err)
	for i, line := range strings.Split(string(src), "\n") {
		if strings.Contains(line, "// hit") {
			want = append(want, i+1)
		}
	}
	rel := "internal/fixture/" + dir + ".go"
	ps := newPasteScanner(fset, []*ast.File{f}, []string{rel}, schema, []pasteGated{{rel, "gated", "rancher delete token '{v}' --apply --yes", []string{"--apply", "--yes"}, "a fixture"}, {rel, "withheld", "rancher delete token '{v}' --apply", []string{"--apply", "--yes"}, "a fixture"}})
	for _, s := range ps.sites() {
		if s.has(check) {
			got = append(got, s.line)
		}
	}
	slices.Sort(got)
	return slices.Compact(got), want
}

// TestRemedyPasteScanFixtures asserts each check on a fixture package: every
// line marked "hit" is reported and no other line is.
func TestRemedyPasteScanFixtures(t *testing.T) {
	t.Parallel()
	for _, c := range pasteFixtureCases {
		t.Run(c.dir, func(t *testing.T) {
			got, want := pasteFixtureScan(t, c.dir, c.check)
			assert.NotEmpty(t, want, "the fixture marks no line")
			assert.Equal(t, want, got)
		})
	}
}

// TestRemedyPasteFixturesAreAllListed asserts every fixture directory is a
// case of the table.
func TestRemedyPasteFixturesAreAllListed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(pasteFixturesDir)
	require.NoError(t, err)
	var dirs, listed []string
	for _, e := range entries {
		dirs = append(dirs, e.Name())
	}
	for _, c := range pasteFixtureCases {
		listed = append(listed, c.dir)
	}
	slices.Sort(dirs)
	slices.Sort(listed)
	assert.Equal(t, listed, dirs)
}

// pasteGatedList is the sites that keep the gates of their command.
var pasteGatedList = []pasteGated{
	{"internal/clierr/command.go", "Revoke", "rancher delete management-token '{v}' --apply --yes --allow-deprecated", []string{"--apply", "--yes", "--allow-deprecated"}, "deletes a token or kubeconfig the CLI created or recorded: the command keeps the gates of its command"},
	{"internal/clierr/command.go", "Revoke", "rancher delete token '{v}' --apply --yes", []string{"--apply", "--yes"}, "deletes a token or kubeconfig the CLI created or recorded: the command keeps the gates of its command"},
	{"internal/clierr/command.go", "RevokeKubeconfig", "rancher delete kubeconfig '{v}' --apply --yes", []string{"--apply", "--yes"}, "deletes a token or kubeconfig the CLI created or recorded: the command keeps the gates of its command"},
	{"internal/clierr/command.go", "FileRevokeKubeconfig", "rancher delete kubeconfig '{v}' --apply --yes", []string{"--apply", "--yes"}, "deletes a token or kubeconfig the CLI created or recorded: the command keeps the gates of its command"},
	{"internal/token/noterminal.go", "CreateKubeconfigRemedy", "rancher create kubeconfig --apply --yes", []string{"--apply", "--yes"}, "the exec plugin's no-terminal remedy: it creates a kubeconfig for kubectl to use at once; the command keeps the gates of its command"},
}

// pasteUnresolvedList is the sites whose flags a call supplies at run time.
var pasteUnresolvedList = []pasteUnresolved{
	{"internal/engine/rbac/grant.go", "Plan", `aliasRevokeHint(upn, role, scope)`, "the flags are the scope flags of the binding, built by scope.Flags()", "TestAliasRevokeHintPreviewsTheRevoke"},
	{"internal/engine/rbac/grant.go", "aliasRevokeHint", `clierr.NewCommand("revoke").Arg(upn).FlagValue("--role", role).With(scope.Flags()).Flag("--dry-run")`, "the scope flags of the binding are built by scope.Flags()", "TestAliasRevokeHintPreviewsTheRevoke"},
	{"internal/commands/register/register.go", "commitCreateWith", `clierr.NewCommand("config update-server").Arg(name).With(CAUpdateArg(probe.CASelector))`, "the flags are the --cacert or --clear-cacert text CAUpdateArg builds from the probe", "TestCommitCreateExistingSameURLDifferentCAConflictsWithCacertHint"},
	{"internal/engine/resources/refusepatch.go", "Plan", `clierr.Text(c.k.Name + ": patch is not supported - " + c.k.NoPatchReason, clierr.Lead("; read it with "), clierr.NewCommand("get").Arg(c.k.Name).With(c.readFlags(in)))`, "the flags are the --cluster and --namespace text readFlags builds from the typed input", "TestRefusePatchRefusesBeforeAnyRequestAndNamesTheRead"},
}

// pasteLocalTextExceptions are the texts outside the login and token code
// that a check reports and that stay.
var pasteLocalTextExceptions = []pasteTextException{
	{"internal/cli/argtail.go", pasteCheckEnd, "The rancher completion script in this shell is out of date. To regenerate it, see: rancher completion --help", "the product name followed by the name of a command in a sentence; it names the completion script and no command to run"},
	{"internal/commands/configcmd/config_edit.go", pasteCheckSchema, "# No rancher CLI config file exists yet at this path.\n# Save this file as-is (or empty) to leave nothing configured.\n# config edit cannot add or change servers or the current server - use\n# rancher config add-server / update-server / delete-server / use-server.\n", "the comment of the file the editor opens; it names commands as examples"},
	{"internal/commands/configcmd/config_edit.go", pasteCheckEnd, "# No rancher CLI config file exists yet at this path.\n# Save this file as-is (or empty) to leave nothing configured.\n# config edit cannot add or change servers or the current server - use\n# rancher config add-server / update-server / delete-server / use-server.\n", "the comment of the file the editor opens; it names commands as examples"},
	{"internal/engine/observe/bundle.go", pasteCheckSchema, "This is a rancher-cli support bundle.\n\nContents:\n  manifest.json            what was collected, skipped, or failed, and why\n                            (a target whose container logs could not all be\n                            read is an error item naming the first failure)\n  logs/<cluster>/<target>/ logs of Rancher components (see \"rancher logs\")\n  clusters/<cluster>/      cluster/node state and per-namespace events\n  rancher/                 CLI version, a snapshot of a fixed list of\n                            settings, feature flags, and this CLI's own\n                            schema manifest\n\nThis bundle may contain sensitive log content (application output,\nidentifiers, timestamps) from the collected components. Review before\nsharing outside your organization.\n\nThis bundle covers only what Rancher's Kubernetes API can reach. For\nnode-level diagnostics (journald, the rancher-system-agent unit, OS state)\nsee rancherlabs/support-tools' node collector; the two bundles complement\neach other.\n", "README text inside the support bundle, read later from any shell; it names a command in quotes"},
	{"internal/runtime/schema_cmd.go", pasteCheckEnd, "rancher schema -o json lists every kind name", "the refusal of the schema command's own selection names commands in running text"},
	{"internal/runtime/schema_cmd.go", pasteCheckEnd, "see the full command list (rancher schema -o name) or the kind names (rancher schema -o json)", "the refusal of the schema command's own selection names commands in running text"},
}
