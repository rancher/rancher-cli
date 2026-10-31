package conformance

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/remedytest"
)

const (
	remedyLiteralsDir = "test/conformance/testdata/remedyliterals"
	schemaGolden      = "internal/cli/testdata/schema.golden.json"

	// The checks an exception names.
	remedyCheckMarker = "marker"
	remedyCheckWord   = "word"
)

// remedyHit is one text a check reports: the file and line it is written
// at, the rule of the check that reports it and its text. A word-less
// command also names its pair and whether the text is a hint the flattener
// resolved (not a literal of the source).
type remedyHit struct {
	file   string
	line   int
	rule   string
	text   string
	pair   string
	inHint bool
	pos    token.Pos
}

// shown is the text of the hit as the report and the fixtures write it.
func (h remedyHit) shown() string {
	switch {
	case h.pair == "":
		return h.text
	case h.inHint:
		return h.pair + " in hint " + h.text
	}
	return h.pair + " in " + h.text
}

func (h remedyHit) String() string { return h.rule + ": " + h.shown() }

// remedyHits are the hits of each check over one set of source files.
type remedyHits struct {
	marker, word, examples, prose []remedyHit
}

// remedyCommands is the command tree the checks read, from the schema
// manifest golden.
type remedyCommands struct {
	// directoryWords is the alternation of the top-level commands that read
	// the config directory.
	directoryWords string
	// topWords is every top-level command, and pairs every two-word command
	// name and alias, each as the two words.
	topWords []string
	pairs    []string
}

func loadRemedyCommands(golden string) (remedyCommands, error) {
	words, err := remedytest.DirectoryCommandWords(golden)
	if err != nil {
		return remedyCommands{}, err
	}
	b, err := os.ReadFile(golden)
	if err != nil {
		return remedyCommands{}, err
	}
	var m struct {
		Commands []struct {
			Name    string   `json:"name"`
			Aliases []string `json:"aliases"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return remedyCommands{}, err
	}
	out := remedyCommands{directoryWords: words}
	seenTop, seenPair := map[string]bool{}, map[string]bool{}
	for _, c := range m.Commands {
		parts := strings.Fields(c.Name)
		if len(parts) == 0 {
			continue
		}
		if !seenTop[parts[0]] {
			seenTop[parts[0]] = true
			out.topWords = append(out.topWords, parts[0])
		}
		if len(parts) != 2 {
			continue
		}
		for _, last := range append([]string{parts[1]}, c.Aliases...) {
			if pair := parts[0] + " " + last; !seenPair[pair] {
				seenPair[pair] = true
				out.pairs = append(out.pairs, pair)
			}
		}
	}
	// The longest first, so an alternation never stops at a shorter pair
	// that is a prefix of a longer one.
	sort.Slice(out.pairs, func(i, j int) bool { return len(out.pairs[i]) > len(out.pairs[j]) })
	return out, nil
}

func quoteAll(words []string) string {
	q := make([]string, len(words))
	for i, w := range words {
		q[i] = regexp.QuoteMeta(w)
	}
	return strings.Join(q, "|")
}

// wordEnd ends a command: anything that is not part of a word.
const wordEnd = `(?:[^A-Za-z0-9_-]|$)`

// The clause starts a command is read after: the start of the text, a
// colon, a semicolon, a ", then", and the words that introduce a command in
// any case. Every start allows one backtick before the command.
const clauseStart = `(?:(?:^|: |; |, then |(?:^|[^A-Za-z0-9_-])(?i:with|use|check|run|ask|to|is) )` + "`?)"

type remedyRegexps struct {
	plain       *regexp.Regexp
	clausePair  *regexp.Regexp
	proseQuoted *regexp.Regexp
	proseWord   *regexp.Regexp
}

func compileRemedyRegexps(c remedyCommands) remedyRegexps {
	pairs := quoteAll(c.pairs)
	return remedyRegexps{
		plain:       regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])rancher +(--[a-z][a-z-]*|(?:` + c.directoryWords + `)` + wordEnd + `)`),
		clausePair:  regexp.MustCompile(clauseStart + `(` + pairs + `)`),
		proseQuoted: regexp.MustCompile("(?:" + clauseStart + "|[`'])(?:" + pairs + `)` + wordEnd),
		proseWord:   regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])(rancher) (?:` + quoteAll(c.topWords) + `)` + wordEnd),
	}
}

var (
	// directoryFreeHelpPath is a command path followed by --help, which reads
	// no config directory.
	directoryFreeHelpPath = regexp.MustCompile(`^[a-z][a-z0-9-]*( [a-z][a-z0-9-]*)* --help` + wordEnd)
	// nameEndsInRancher is a text that is the word rancher, with or without
	// one space after it, or ends with it after a character that is not part
	// of a word.
	nameEndsInRancher = regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])rancher ?$`)
)

// remedyHelpKeys are the keys whose string value is a command's own help
// text or examples.
var remedyHelpKeys = map[string]bool{"Examples": true, "Desc": true, "Usage": true, "ArgDesc": true}

// remedyProseKeys are the keys whose string value the prose check reads,
// besides the help keys other than Examples.
var remedyProseKeys = map[string]bool{"Desc": true, "Usage": true, "ArgDesc": true, "NoDeleteReason": true, "Why": true}

// remedyHelpTables are the package-level variables that hold help text by
// name: group titles and descriptions.
var remedyHelpTables = map[string]bool{"groupUsages": true, "groupDescs": true}

// remedyStdFormats are the format functions of package fmt that matter
// here, each with the index of its format string.
var remedyStdFormats = map[string]int{"Sprintf": 0, "Errorf": 0, "Printf": 0, "Fprintf": 1}

// remedyPrintFuncs are the functions of package fmt that join their
// arguments with no format; only the rule that reads the word rancher as an
// argument applies to them.
var remedyPrintFuncs = map[string]bool{"Sprint": true, "Sprintln": true, "Print": true, "Println": true, "Fprint": true, "Fprintln": true}

// unquoted is the value of a string literal.
func unquoted(lit *ast.BasicLit) string {
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return lit.Value
	}
	return s
}

// chainLiterals is the string literals of e when e is one literal or a
// chain of operands joined by +, whatever the other operands are, and nil
// for any other expression: a literal inside a call or a composite value is
// not part of the chain.
func chainLiterals(e ast.Expr) []*ast.BasicLit {
	var out []*ast.BasicLit
	for _, op := range addOperands(ast.Unparen(e)) {
		if lit, ok := op.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			out = append(out, lit)
		}
	}
	return out
}

// addOperands is the operands of a chain joined by +, in order.
func addOperands(e ast.Expr) []ast.Expr {
	if b, ok := e.(*ast.BinaryExpr); ok && b.Op == token.ADD {
		return append(addOperands(b.X), b.Y)
	}
	return []ast.Expr{e}
}

func isAdd(n ast.Node) bool {
	b, ok := n.(*ast.BinaryExpr)
	return ok && b.Op == token.ADD
}

// formatParam is the index of the format string of a function type whose
// parameters end with a string and a variadic ...any, whatever their names,
// or -1.
func formatParam(ft *ast.FuncType) int {
	var types []ast.Expr
	for _, f := range ft.Params.List {
		for range max(len(f.Names), 1) {
			types = append(types, f.Type)
		}
	}
	if len(types) < 2 {
		return -1
	}
	ell, ok := types[len(types)-1].(*ast.Ellipsis)
	if !ok {
		return -1
	}
	if id, ok := ell.Elt.(*ast.Ident); !ok || id.Name != "any" {
		if it, ok := ell.Elt.(*ast.InterfaceType); !ok || len(it.Methods.List) != 0 {
			return -1
		}
	}
	if id, ok := types[len(types)-2].(*ast.Ident); !ok || id.Name != "string" {
		return -1
	}
	return len(types) - 2
}

// formatFunctions is every function and method of the files that takes a
// format string and arguments, and every function literal assigned to a
// package-level variable.
func formatFunctions(files []*ast.File) map[string]int {
	out := map[string]int{}
	for _, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if i := formatParam(d.Type); i >= 0 {
					out[d.Name.Name] = i
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for i, v := range vs.Values {
							if fl, ok := v.(*ast.FuncLit); ok && i < len(vs.Names) {
								if idx := formatParam(fl.Type); idx >= 0 {
									out[vs.Names[i].Name] = idx
								}
							}
						}
					}
				}
			}
		}
	}
	return out
}

// localFormatFunctions is the function literals assigned to a variable of
// the declaration, with the index of their format string.
func localFormatFunctions(decl ast.Decl) map[string]int {
	out := map[string]int{}
	ast.Inspect(decl, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range s.Rhs {
				if fl, ok := rhs.(*ast.FuncLit); ok && i < len(s.Lhs) {
					if id, ok := s.Lhs[i].(*ast.Ident); ok {
						if idx := formatParam(fl.Type); idx >= 0 {
							out[id.Name] = idx
						}
					}
				}
			}
		case *ast.ValueSpec:
			for i, v := range s.Values {
				if fl, ok := v.(*ast.FuncLit); ok && i < len(s.Names) {
					if idx := formatParam(fl.Type); idx >= 0 {
						out[s.Names[i].Name] = idx
					}
				}
			}
		}
		return true
	})
	return out
}

// formatIndex is the index of the format string when call is a call of a
// format function: one of package fmt, under any name the file gives the
// import, or one of the module.
func (s *remedyScan) formatIndex(call *ast.CallExpr, local map[string]int) (int, bool) {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		if id, ok := fun.X.(*ast.Ident); ok && s.fmtNames[id.Name] {
			i, ok := remedyStdFormats[fun.Sel.Name]
			return i, ok
		}
		i, ok := s.global[fun.Sel.Name]
		return i, ok
	case *ast.Ident:
		if i, ok := local[fun.Name]; ok {
			return i, true
		}
		i, ok := s.global[fun.Name]
		return i, ok
	}
	return 0, false
}

// remedyScan reads every file for the four checks.
type remedyScan struct {
	fset    *token.FileSet
	cmds    remedyCommands
	re      remedyRegexps
	global  map[string]int
	hits    remedyHits
	owned   map[*ast.BasicLit]bool
	skipped map[*ast.BasicLit]bool
	// clierrNames and fmtNames are the names the file being read gives the
	// clierr and fmt imports. inClierr is true inside package clierr itself
	// and clierrDot in a file that imports clierr with a dot.
	clierrNames map[string]bool
	fmtNames    map[string]bool
	inClierr    bool
	clierrDot   bool
}

func scanRemedies(fset *token.FileSet, files []*ast.File, paths []string, cmds remedyCommands) remedyHits {
	s := &remedyScan{fset: fset, cmds: cmds, re: compileRemedyRegexps(cmds), global: formatFunctions(files)}
	for i, f := range files {
		s.scanFile(f, paths[i])
	}
	s.scanHints(files, paths)
	return s.hits
}

// scanHints runs the word-less check on every hint the flattener resolves,
// which reads a hint built from variables and calls as well as the literals
// the other pass reads. A command the literal pass reported in a literal of
// the argument, or in one the flattener read for it (a constant, or a
// function's return value), is not reported again.
func (s *remedyScan) scanHints(files []*ast.File, paths []string) {
	eachHint(files, paths, func(path string, expr ast.Expr, pos token.Pos, literals []string, read []*ast.BasicLit) {
		seen := map[string]bool{}
		for _, literal := range literals {
			if seen[literal] {
				continue
			}
			seen[literal] = true
			for _, m := range s.findPairs(literal) {
				if !s.reportedIn(path, expr, read, m.pair) {
					s.addWordHit(path, pos, m.pair, literal, true)
				}
			}
		}
	})
}

// reportedIn reports whether the literal pass found the command pair in a
// literal inside e, or in one of the literals the flattener read for e.
func (s *remedyScan) reportedIn(path string, e ast.Expr, read []*ast.BasicLit, pair string) bool {
	return slices.ContainsFunc(s.hits.word, func(h remedyHit) bool {
		if h.inHint || h.pair != pair {
			return false
		}
		if h.file == path && h.pos >= e.Pos() && h.pos < e.End() {
			return true
		}
		return slices.ContainsFunc(read, func(lit *ast.BasicLit) bool { return lit.Pos() == h.pos })
	})
}

// classify finds the literals each key and table owns: help text, which is
// never a remedy, and the text of the prose check.
func (s *remedyScan) classify(f *ast.File, path string) {
	s.owned, s.skipped = map[*ast.BasicLit]bool{}, map[*ast.BasicLit]bool{}
	allLiterals := func(n ast.Node, into map[*ast.BasicLit]bool) {
		ast.Inspect(n, func(m ast.Node) bool {
			if lit, ok := m.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				into[lit] = true
			}
			return true
		})
	}
	helpTable := func(name string, value ast.Expr) {
		if !remedyHelpTables[name] {
			return
		}
		allLiterals(value, s.skipped)
		if cl, ok := value.(*ast.CompositeLit); ok {
			for _, elt := range cl.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					s.readProse(path, kv.Value)
				}
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.KeyValueExpr:
			id, ok := node.Key.(*ast.Ident)
			if !ok {
				return true
			}
			if id.Name == "Examples" {
				s.readExamples(path, node)
			} else if remedyHelpKeys[id.Name] {
				if lits := chainLiterals(node.Value); lits != nil {
					for _, l := range lits {
						s.owned[l] = true
					}
				}
			}
			if remedyProseKeys[id.Name] {
				s.readProse(path, node.Value)
			}
			if id.Name == "NoDeleteReason" {
				allLiterals(node.Value, s.skipped)
			}
		case *ast.ValueSpec:
			for i, name := range node.Names {
				if i < len(node.Values) {
					helpTable(name.Name, node.Values[i])
				}
			}
		case *ast.AssignStmt:
			for i, lhs := range node.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && i < len(node.Rhs) {
					helpTable(id.Name, node.Rhs[i])
				}
			}
		}
		return true
	})
}

func (s *remedyScan) addHit(into *[]remedyHit, path string, pos token.Pos, rule, text string) {
	*into = append(*into, remedyHit{file: path, line: s.fset.Position(pos).Line, rule: rule, text: text, pos: pos})
}

// addWordHit records a command written without the word rancher: the pair
// it names and the text it is found in, a literal or a flattened hint.
func (s *remedyScan) addWordHit(path string, pos token.Pos, pair, text string, inHint bool) {
	s.hits.word = append(s.hits.word, remedyHit{file: path, line: s.fset.Position(pos).Line, rule: "word", text: text, pair: pair, inHint: inHint, pos: pos})
}

// readExamples checks the value under an Examples key: a list of examples, a
// literal or a chain of operands joined by + is read; a selector of a field
// named Examples (the examples of another descriptor) is trusted, not
// followed to where that field is set; any other value is reported, since
// its entries cannot be read.
func (s *remedyScan) readExamples(path string, kv *ast.KeyValueExpr) {
	switch v := kv.Value.(type) {
	case *ast.CompositeLit:
		for _, elt := range v.Elts {
			s.readExample(path, elt)
		}
	case *ast.BasicLit:
		s.readExample(path, v)
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			s.readExample(path, v)
			return
		}
		s.addHit(&s.hits.examples, path, v.Pos(), "examples", examplesNotLiteral)
	case *ast.SelectorExpr:
		if v.Sel.Name != "Examples" {
			s.addHit(&s.hits.examples, path, v.Pos(), "examples", examplesNotLiteral)
		}
	default:
		s.addHit(&s.hits.examples, path, kv.Value.Pos(), "examples", examplesNotLiteral)
	}
}

// examplesNotLiteral is the text of a hit for an Examples value the check
// cannot read.
const examplesNotLiteral = "(an Examples value that is not a list of literals)"

// readExample checks one entry of an Examples list, a literal or a chain of
// operands joined by +, and marks its literals as help text.
func (s *remedyScan) readExample(path string, e ast.Expr) {
	var sb strings.Builder
	for _, op := range addOperands(e) {
		if lit, ok := op.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			s.owned[lit] = true
			sb.WriteString(unquoted(lit))
		} else {
			sb.WriteString("X")
		}
	}
	text := sb.String()
	if strings.HasPrefix(text, "rancher ") || strings.Contains(text, "| rancher ") || strings.Contains(text, "<(rancher ") {
		return
	}
	s.addHit(&s.hits.examples, path, e.Pos(), "examples", text)
}

// readProse checks each literal of a help text, a literal or a chain of
// operands joined by +, for a command named another way than as
// rancher <command> in backticks. Each literal starts a clause.
func (s *remedyScan) readProse(path string, e ast.Expr) {
	switch e.(type) {
	case *ast.BasicLit, *ast.BinaryExpr:
	default:
		return
	}
	backticks := 0
	for _, op := range addOperands(e) {
		lit, ok := op.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		text := unquoted(lit)
		hit := s.re.proseQuoted.MatchString(text)
		if !hit {
			for _, loc := range s.re.proseWord.FindAllStringSubmatchIndex(text, -1) {
				if (backticks+strings.Count(text[:loc[2]], "`"))%2 == 0 {
					hit = true
					break
				}
			}
		}
		if hit {
			s.addHit(&s.hits.prose, path, lit.Pos(), "prose", text)
		}
		backticks += strings.Count(text, "`")
	}
}

func (s *remedyScan) scanFile(f *ast.File, path string) {
	s.classify(f, path)

	s.inClierr = f.Name.Name == "clierr"
	s.clierrNames, s.fmtNames, s.clierrDot = map[string]bool{}, map[string]bool{}, false
	for _, imp := range f.Imports {
		ip, err := strconv.Unquote(imp.Path.Value)
		if err != nil || (ip != clierrPath && ip != "fmt") {
			continue
		}
		name := defaultImportName(ip)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		switch {
		case name == "." && ip == clierrPath:
			s.clierrDot = true
		case ip == clierrPath:
			s.clierrNames[name] = true
		case name != "." && name != "_":
			s.fmtNames[name] = true
		}
	}
	for _, decl := range f.Decls {
		local := localFormatFunctions(decl)
		var stack []ast.Node
		ast.Inspect(decl, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			var parent ast.Node
			if len(stack) > 1 {
				parent = stack[len(stack)-2]
			}
			switch node := n.(type) {
			case *ast.BasicLit:
				if node.Kind == token.STRING {
					s.scanLiteral(node, parent, stack, path, local)
				}
			case *ast.BinaryExpr:
				if node.Op == token.ADD {
					if parent == nil || !isAdd(parent) {
						s.scanChain(node, path, local)
					}
				}
			case *ast.CallExpr:
				s.scanFormatMarker(node, path, local)
				s.scanPrintMarker(node, path)
			}
			return true
		})
	}
}

// scanLiteral applies the checks that read one literal.
func (s *remedyScan) scanLiteral(lit *ast.BasicLit, parent ast.Node, stack []ast.Node, path string, local map[string]int) {
	text := unquoted(lit)
	if !s.owned[lit] && !s.skipped[lit] {
		s.scanMarker(lit, text, parent, path, local)
	}
	if s.owned[lit] || isAdd(parent) {
		return
	}
	s.scanWord([]textSegment{{lit: lit, text: text}}, parent, stack, path)
}

// scanMarker reports the ways a literal writes a command without the marker.
func (s *remedyScan) scanMarker(lit *ast.BasicLit, text string, parent ast.Node, path string, local map[string]int) {
	hit := func(rule string) {
		s.addHit(&s.hits.marker, path, lit.Pos(), rule, text)
	}
	// A: a command that reads the config directory, written plain.
	for _, m := range s.re.plain.FindAllStringSubmatchIndex(text, -1) {
		word := text[m[2]:m[3]]
		if word == "--help" || word == "--version" {
			continue
		}
		if directoryFreeHelpPath.MatchString(text[m[2]:]) {
			continue
		}
		hit("A")
		return
	}
	switch p := parent.(type) {
	case *ast.BinaryExpr:
		// B: the word rancher as an operand of +.
		if p.Op == token.ADD && nameEndsInRancher.MatchString(text) {
			hit("B")
			return
		}
	case *ast.CallExpr:
		idx, isFormat := s.formatIndex(p, local)
		position := slices.Index(p.Args, ast.Expr(lit))
		if position < 0 {
			return
		}
		switch {
		case isFormat && position == idx:
			// C: a command in the middle of a format.
			if strings.Contains(text, "rancher %") {
				hit("C")
			}
		case nameEndsInRancher.MatchString(text):
			// B: the word rancher as another argument of a format call, as
			// an argument of a print function, or written with WriteString.
			if sel, ok := p.Fun.(*ast.SelectorExpr); isFormat || s.isPrintCall(p) || (ok && sel.Sel.Name == "WriteString") {
				hit("B")
			}
		}
	case *ast.CompositeLit:
		// E: a word list that starts with the word rancher.
		if at, ok := p.Type.(*ast.ArrayType); ok && at.Len == nil && len(p.Elts) > 0 && p.Elts[0] == ast.Expr(lit) && text == "rancher" {
			if id, ok := at.Elt.(*ast.Ident); ok && id.Name == "string" {
				hit("E")
			}
		}
	}
}

// scanChain applies the checks that read a chain of operands joined by +:
// the operand after the marker (a marker followed by text that does not
// start with a space prints the word and the text run together), and the
// text a word-less command appears in.
func (s *remedyScan) scanChain(root *ast.BinaryExpr, path string, local map[string]int) {
	operands := addOperands(root)
	for i, op := range operands {
		if !s.isMarker(op) || i+1 >= len(operands) {
			continue
		}
		next := operands[i+1]
		switch n := next.(type) {
		case *ast.BasicLit:
			if n.Kind == token.STRING && strings.HasPrefix(unquoted(n), " ") {
				continue
			}
			s.addHit(&s.hits.marker, path, n.Pos(), "D", unquoted(n))
		case *ast.CallExpr:
			if idx, ok := s.formatIndex(n, local); ok && idx < len(n.Args) {
				if lit, ok := n.Args[idx].(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.HasPrefix(unquoted(lit), " ") {
					continue
				}
			}
			s.addHit(&s.hits.marker, path, n.Pos(), "D", "(an operand that is not a literal starting with a space)")
		default:
			s.addHit(&s.hits.marker, path, next.Pos(), "D", "(an operand that is not a literal starting with a space)")
		}
	}
	segments := make([]textSegment, len(operands))
	for i, op := range operands {
		switch bl, ok := op.(*ast.BasicLit); {
		case s.isMarker(op):
			segments[i] = textSegment{text: "rancher"}
		case ok && bl.Kind == token.STRING:
			segments[i] = textSegment{lit: bl, text: unquoted(bl)}
		default:
			segments[i] = textSegment{text: "X"}
		}
	}
	if first := segments[0]; first.lit != nil && s.owned[first.lit] {
		return
	}
	s.scanWord(segments, nil, nil, path)
}

// textSegment is one operand of the text a word-less command is looked for
// in: a literal, or a value read as one word.
type textSegment struct {
	lit  *ast.BasicLit
	text string
}

// scanWord reports every command written without the word rancher after a
// word that introduces one, in the text the segments make when joined with
// every run of spaces read as one: one hit for each command, named by the
// literal it starts in. A single literal that is a command name in a place
// names belong is exempt (parent and stack say where it is; they are nil for
// a chain).
func (s *remedyScan) scanWord(segments []textSegment, parent ast.Node, stack []ast.Node, path string) {
	var raw strings.Builder
	starts := make([]int, len(segments))
	for i, g := range segments {
		starts[i] = raw.Len()
		raw.WriteString(g.text)
	}
	for _, m := range s.findPairs(raw.String()) {
		hit := segments[0].lit
		for i, g := range segments {
			if starts[i] <= m.at && g.lit != nil {
				hit = g.lit
			}
		}
		if hit == nil || (len(segments) == 1 && s.wordExemptByPosition(hit, parent, stack)) {
			continue
		}
		s.addWordHit(path, hit.Pos(), m.pair, unquoted(hit), false)
	}
}

// pairAt is a command pair found in a text, and the index it starts at.
type pairAt struct {
	pair string
	at   int
}

// findPairs is every command pair of the golden that follows a clause start
// in text, in order, with every run of spaces and tabs read as one space. A
// clause start belongs to the match after it only: the search for the next
// command begins where the pair ends, so a delimiter is never shared by two
// commands.
func (s *remedyScan) findPairs(text string) []pairAt {
	norm := make([]byte, 0, len(text))
	origin := make([]int, 0, len(text))
	for i := range len(text) {
		c := text[i]
		if c == '\t' {
			c = ' '
		}
		if c == ' ' && len(norm) > 0 && norm[len(norm)-1] == ' ' {
			continue
		}
		norm = append(norm, c)
		origin = append(origin, i)
	}
	var out []pairAt
	for _, m := range s.re.clausePair.FindAllSubmatchIndex(norm, -1) {
		if m[3] < len(norm) && isWordByte(norm[m[3]]) {
			continue
		}
		out = append(out, pairAt{pair: strings.ToLower(string(norm[m[2]:m[3]])), at: origin[m[2]]})
	}
	return out
}

// isWordByte reports whether b can be part of a word of a command.
func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-'
}

// isMarker reports whether e is clierr.Rancher under any name the file
// gives the import, or Rancher inside package clierr or in a file that
// imports clierr with a dot.
func (s *remedyScan) isMarker(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.SelectorExpr:
		id, ok := e.X.(*ast.Ident)
		return ok && s.clierrNames[id.Name] && e.Sel.Name == "Rancher"
	case *ast.Ident:
		return (s.inClierr || s.clierrDot) && e.Name == "Rancher"
	}
	return false
}

// isPrintCall reports whether call is a call of one of the print functions
// of package fmt, under any name the file gives the import.
func (s *remedyScan) isPrintCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && s.fmtNames[id.Name] && remedyPrintFuncs[sel.Sel.Name]
}

// defaultImportName is the name an import path gives by default: its last
// element.
func defaultImportName(importPath string) string {
	return importPath[strings.LastIndex(importPath, "/")+1:]
}

// scanFormatMarker reports a format call that takes the marker as an
// argument when the format text right after the verb the marker fills does
// not start with a space and the verb does not end the format: the word
// rancher and that text would be printed run together. The format is read
// as written, a literal or a chain of literals; a format that holds an
// operand that is not a literal, or an argument index or a star, is not
// read.
func (s *remedyScan) scanFormatMarker(call *ast.CallExpr, path string, local map[string]int) {
	idx, ok := s.formatIndex(call, local)
	if !ok || idx >= len(call.Args) {
		return
	}
	var format strings.Builder
	for _, op := range addOperands(ast.Unparen(call.Args[idx])) {
		lit, ok := op.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return
		}
		format.WriteString(unquoted(lit))
	}
	ends, ok := verbEnds(format.String())
	if !ok {
		return
	}
	for i, arg := range call.Args[idx+1:] {
		if i >= len(ends) || !s.isMarker(arg) {
			continue
		}
		if !markerVerbEndsWell(format.String()[ends[i]:]) {
			s.addHit(&s.hits.marker, path, call.Args[idx].Pos(), "D", format.String())
		}
	}
}

// markerVerbEndsWell reports whether the text of a format right after the
// verb the marker fills prints the word rancher apart from what follows: it
// is empty, starts with a space or a newline, or starts with another verb
// (what that verb prints is not read).
func markerVerbEndsWell(rest string) bool {
	switch {
	case rest == "", rest[0] == ' ', rest[0] == '\n':
		return true
	}
	return rest[0] == '%' && !strings.HasPrefix(rest, "%%")
}

// scanPrintMarker reports a call of a print function that takes the marker
// as an argument when the string literal argument right after it does not
// start with a space: Sprint, Print and Fprint join two strings with nothing
// between them. The Ln forms put a space there, and an argument that is not
// a literal is not read.
func (s *remedyScan) scanPrintMarker(call *ast.CallExpr, path string) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || len(call.Args) < 2 || !s.isPrintCall(call) || strings.HasSuffix(sel.Sel.Name, "ln") {
		return
	}
	for i, arg := range call.Args[:len(call.Args)-1] {
		next, ok := call.Args[i+1].(*ast.BasicLit)
		if !s.isMarker(arg) || !ok || next.Kind != token.STRING || strings.HasPrefix(unquoted(next), " ") {
			continue
		}
		s.addHit(&s.hits.marker, path, next.Pos(), "D", unquoted(next))
	}
}

// verbEnds is the index right after each verb of a format, in order, and
// false for a format with an argument index or a star, whose arguments do
// not follow the verbs one by one.
func verbEnds(format string) ([]int, bool) {
	var ends []int
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		i++
		for i < len(format) && strings.IndexByte("+-# 0123456789.", format[i]) >= 0 {
			i++
		}
		if i >= len(format) {
			break
		}
		switch format[i] {
		case '%':
			continue
		case '*', '[':
			return nil, false
		}
		ends = append(ends, i+1)
	}
	return ends, true
}

// wordExemptByPosition reports whether a literal that equals a command name
// is written where a name belongs: the value of a Name field, a key of a
// map, an element of the table of first login commands, an operand of a
// comparison, or a case value.
func (s *remedyScan) wordExemptByPosition(lit *ast.BasicLit, parent ast.Node, stack []ast.Node) bool {
	switch p := parent.(type) {
	case *ast.KeyValueExpr:
		if p.Key == ast.Expr(lit) {
			return true
		}
		if id, ok := p.Key.(*ast.Ident); ok && id.Name == "Name" && p.Value == ast.Expr(lit) {
			return true
		}
	case *ast.BinaryExpr:
		return p.Op == token.EQL || p.Op == token.NEQ
	case *ast.CaseClause:
		return slices.Contains(p.List, ast.Expr(lit))
	}
	for _, a := range stack {
		if vs, ok := a.(*ast.ValueSpec); ok {
			for _, name := range vs.Names {
				if name.Name == "firstLoginCommands" {
					return true
				}
			}
		}
	}
	return false
}

// remedyException is one reviewed hit that stays: the file, the check, the
// exact text of the hit (or a substring of it, for a long text), why it is
// not a remedy command printed by this process, and, for a command written
// without the word rancher, the command pair it names (empty for the other
// checks).
type remedyException struct {
	file, check, text string
	substring         bool
	why               string
	pair              string
}

// matches reports whether h is the hit the exception names.
func (e remedyException) matches(file string, h remedyHit) bool {
	if e.file != file || e.pair != h.pair {
		return false
	}
	if e.substring {
		return strings.Contains(h.text, e.text)
	}
	return h.text == e.text
}

// applyExceptions matches each exception to one hit of its file and returns
// the hits no exception covers and the exceptions that cover no hit. A new
// hit, a removed one and a swapped one each leave something over.
func applyExceptions(hits []remedyHit, check string, exceptions []remedyException) (left []remedyHit, unused []remedyException) {
	used := make([]bool, len(hits))
	for _, e := range exceptions {
		if e.check != check {
			continue
		}
		found := false
		for i, h := range hits {
			if !used[i] && e.matches(h.file, h) {
				used[i], found = true, true
				break
			}
		}
		if !found {
			unused = append(unused, e)
		}
	}
	for i, h := range hits {
		if !used[i] {
			left = append(left, h)
		}
	}
	return left, unused
}

const (
	whyLoginPackage = "the details payloads of this package (LogoutAllPartialFailure) print the same commands and expand them through no MapText; marking the texts alone would print one command two ways"
	whyRecoveryData = "also fills result data of other commands; the exec plugin marks it where it prints it (clierr.MarkRemedy)"
	whyKubeconfig   = "written into the kubeconfig file, read later from any shell"
	whyProviderMsg  = "the errors of this package are printed by the exec plugin too, whose remedy texts are checked in internal/token/remedyparse_test.go"
)

// remedyExceptions are the texts the checks report that stay as they are.
// The login and token code, and the two clierr files its payloads print,
// are recorded text by text; an added, a removed or a swapped text fails.
var remedyExceptions = []remedyException{
	{"internal/commands/auth/login.go", remedyCheckMarker, "rancher config update-server ", false, whyLoginPackage, ""},
	{"internal/commands/auth/login.go", remedyCheckMarker, "save it for this entry with: rancher config update-server ", false, whyLoginPackage, ""},
	{"internal/commands/auth/login.go", remedyCheckMarker, "to save the CA, run: rancher config update-server ", false, whyLoginPackage, ""},
	{"internal/commands/auth/login.go", remedyCheckMarker, "to replace it, run: rancher config update-server ", false, whyLoginPackage, ""},
	{"internal/commands/auth/login.go", remedyCheckMarker, "rancher config update-server %s --url %s", false, whyLoginPackage, ""},
	{"internal/commands/auth/login.go", remedyCheckMarker, "rancher config use-server ", false, whyLoginPackage, ""},
	{"internal/commands/auth/login.go", remedyCheckMarker, "; %s is already registered - sign in to it directly: rancher auth login %s", false, whyLoginPackage, ""},
	{"internal/commands/auth/login.go", remedyCheckMarker, "reading stored CA for server entry %q: %s; restore the file, or run: rancher config delete-server %s", false, whyLoginPackage, ""},
	{"internal/commands/auth/login.go", remedyCheckMarker, "rancher auth login %s", false, whyLoginPackage, ""},
	{"internal/commands/auth/login.go", remedyCheckMarker, "the current server is not set - switch to it with: rancher config use-server %s", false, whyLoginPackage, ""},
	{"internal/commands/auth/login.go", remedyCheckMarker, "the current server remains %s - switch to %s with: rancher config use-server %s", false, whyLoginPackage, ""},
	{"internal/commands/auth/login.go", remedyCheckMarker, " (server entry %q may have changed since registration; check it with: rancher config list-servers)", false, whyLoginPackage, ""},
	{"internal/commands/auth/login.go", remedyCheckMarker, "; undo the registration with: rancher config delete-server %s", false, whyLoginPackage, ""},
	{"internal/commands/auth/logintarget.go", remedyCheckMarker, "rancher config update-server %s %s", false, whyLoginPackage, ""},
	{"internal/commands/auth/logintarget.go", remedyCheckMarker, "--cacert without --url and without a name has no entry to attach trust to; pass a name to change an existing entry's trust with rancher config update-server", false, whyLoginPackage, ""},
	{"internal/commands/auth/logintarget.go", remedyCheckMarker, "no servers configured; add one with: rancher auth login --url <url>", false, whyLoginPackage, ""},
	{"internal/commands/auth/logintarget.go", remedyCheckMarker, "a server name cannot be derived from an IPv6 address; pass an explicit name: rancher auth login <name> --url <url>", false, whyLoginPackage, ""},
	{"internal/commands/auth/logout.go", remedyCheckMarker, "specify a server name or --all, not both (e.g. rancher auth logout prod, or rancher auth logout --all)", false, whyLoginPackage, ""},
	{"internal/commands/auth/logout.go", remedyCheckMarker, "no current server; specify a server name or --all (e.g. rancher auth logout prod, or rancher auth logout --all)", false, whyLoginPackage, ""},
	{"internal/commands/auth/authcheck.go", remedyCheckMarker, "token metadata unavailable: stored token not found on the server (revoked or rotated) - re-authenticate with rancher auth login", false, whyLoginPackage, ""},
	{"internal/token/deletetoken.go", remedyCheckMarker, "specify either a credential name or --all, not both (e.g. rancher auth delete-token prod, or rancher auth delete-token --all)", false, "auth delete-token's own usage error; its two examples show the forms of the command that is running", ""},
	{"internal/token/entry.go", remedyCheckMarker, "more than one server entry for %s has a stored login (%s) and the legacy rancher token command cannot name one; regenerate the kubeconfig from the entry you want with %s, and replace this kubeconfig with the one it prints", false, "names the command whose kubeconfig this is; it runs nothing", ""},
	{"internal/clierr/recovery.go", remedyCheckMarker, "rancher auth login ", false, "RecoverySequence " + whyRecoveryData, ""},
	{"internal/clierr/recovery.go", remedyCheckMarker, "rancher config use-server ", false, "RecoverySequence " + whyRecoveryData, ""},
	{"internal/clierr/recovery.go", remedyCheckMarker, "rancher auth logout ", false, "RecoverySequence " + whyRecoveryData, ""},
	{"internal/clierr/loginfailure.go", remedyCheckMarker, "rancher get management-tokens", false, "RecoveryCommands " + whyRecoveryData, ""},
	{"internal/clierr/loginfailure.go", remedyCheckMarker, "rancher delete management-token ", false, "RecoveryCommands " + whyRecoveryData, ""},
	{"internal/clierr/loginfailure.go", remedyCheckMarker, "rancher get tokens", false, "RecoveryCommands " + whyRecoveryData, ""},
	{"internal/clierr/loginfailure.go", remedyCheckMarker, "rancher delete token ", false, "RecoveryCommands " + whyRecoveryData, ""},
	{"internal/engine/kubeconfig/records.go", remedyCheckMarker, " set: rancher", false, whyKubeconfig, ""},
	{"internal/engine/kubeconfig/records.go", remedyCheckMarker, "rancher", false, whyKubeconfig, ""},
	{"internal/client/provider/client.go", remedyCheckWord, "token management: auth list-tokens; kubeconfig: the default exec-credential mode", false, whyProviderMsg, "auth list-tokens"},
	{"internal/testrancher/fidelity/probes.go", remedyCheckWord, "delete user ", false, "a fidelity probe's step name, naming the request the step sends; never printed to a user", "delete user"},

	{"internal/engine/extres/kubeconfig.go", remedyCheckMarker, "# Kubeconfig %q - shown once; revoke with: rancher delete kubeconfig %s\n", false, "a header written into the kubeconfig file, read later from any shell", ""},
	{"internal/engine/observe/bundle.go", remedyCheckMarker, "This is a rancher-cli support bundle.", true, "README text inside the support bundle, read later from any shell", ""},
	{"internal/commands/configcmd/config_edit.go", remedyCheckMarker, "# No rancher CLI config file exists yet at this path.", true, "the comment of the file the editor opens", ""},
	{"internal/runtime/schema_render.go", remedyCheckMarker, "rancher ", false, "a schema card's heading names the command it describes; schema output reads no directory", ""},
	{"internal/runtime/schema_render.go", remedyCheckMarker, "rancher %s - %d commands", true, "the product name and version in the schema header", ""},
	{"internal/clierr/remedy.go", remedyCheckMarker, "rancher", false, "the marker's definition", ""},
	{"internal/clierr/remedy.go", remedyCheckMarker, "rancher", false, "ExpandRemedies writes the expanded word", ""},
	{"internal/clierr/remedy.go", remedyCheckMarker, "rancher ", false, "DirectoryFree returns a command checked against the closed set", ""},
	{"internal/clierr/remedy.go", remedyCheckMarker, "rancher ", false, "DirectoryFree returns a command checked against the closed set", ""},
	{"internal/remedytest/textfields.go", remedyCheckMarker, "rancher", false, "a test helper's expected value", ""},
	{"internal/cli/linerefusal.go", remedyCheckMarker, "rancher", false, "rebuilds the line the user typed, --config included when it was typed", ""},
	{"internal/cli/parseline.go", remedyCheckMarker, "rancher", false, "names the command a message is about", ""},
	{"internal/cli/execcommand_completion.go", remedyCheckMarker, "rancher", false, "offers the program name as a --exec-command value", ""},

	{"internal/commands/configcmd/config_edit.go", remedyCheckWord, "config edit needs a TTY - edit %s directly", false, "names the command that is running in its own refusal", "config edit"},
	{"internal/commands/configcmd/config_edit.go", remedyCheckWord, "config edit needs $VISUAL or $EDITOR set to launch an editor - edit %s directly", false, "names the command that is running in its own refusal", "config edit"},
	{"internal/commands/configcmd/config_edit.go", remedyCheckWord, "config edit aborted: invalid config; your edits are preserved at %s (%v)", false, "names the command that is running in its own refusal", "config edit"},
	{"internal/commands/configcmd/config_edit.go", remedyCheckWord, "config edit cannot change: ", false, "names the command that is running before the list of refused changes", "config edit"},
	{"internal/commands/configcmd/set_token_reuse.go", remedyCheckWord, "token reuse off: auth get-token logs in per cluster again", false, "describes what auth get-token does with the setting off; nothing to run", "auth get-token"},
	{"internal/engine/resources/deleteproject.go", remedyCheckWord, "delete project partially applied: deletion started for %d namespaces, failed on %s, %d not attempted; the project was kept; re-run the same command; it reads the project's namespaces again", false, "names the command that is running in its partial-failure message", "delete project"},
	{"internal/engine/projects/membership.go", remedyCheckWord, "internal error: project add-namespace ran without --project", false, "an internal error that names the command that is running", "project add-namespace"},
	{"internal/engine/resources/create.go", remedyCheckWord, "internal error: create resource read manifest %q and was then given %q", false, "an internal error that names the command that is running", "create resource"},
	{"internal/commands/telemetrycmd/reset.go", remedyCheckWord, "telemetry reset is incomplete", false, "names the command that is running in its failure message", "telemetry reset"},
	{"internal/commands/skill/skill.go", remedyCheckWord, "skill install failed for part of the selected targets", false, "names the command that is running in its failure message", "skill install"},
	{"internal/commands/skill/skill.go", remedyCheckWord, "skill uninstall changed only part of the selected targets", false, "names the command that is running in its failure message", "skill uninstall"},
	{"internal/config/config.go", remedyCheckWord, "config path is empty", false, "config path is the path of the config file, not a command", "config path"},
	{"internal/engine/rbac/deleteuser.go", remedyCheckWord, "refusing to delete user ", false, "delete user is the verb of the refusal of the running command", "delete user"},
	{"internal/engine/rbac/inspectresolve.go", remedyCheckWord, "%q is a group principal; inspect user only supports individual users; use grant/revoke to work with group role bindings", false, "names `inspect user` as a noun in its own refusal; nothing to run", "inspect user"},
	{"internal/engine/access/review.go", remedyCheckWord, "unable to create token for impersonated ServiceAccount:", false, "create token is the verb of the server's TokenRequest that failed", "create token"},
	{"internal/engine/access/clusters.go", remedyCheckWord, "reachable = Rancher's proxy admits the identity to this cluster; what it may do there is access list", false, "the definition line names the command as a noun; nothing to run", "access list"},
	{"internal/engine/provisioning/scale.go", remedyCheckWord, "cluster scale", false, "the name of the running command, passed as the verb parameter of planClusterFamilyMutation, which writes it into its own messages", "cluster scale"},
	{"internal/engine/provisioning/autoscaling.go", remedyCheckWord, "cluster set-autoscaling", false, "the name of the running autoscaling command, passed as the verb parameter of planClusterFamilyMutation", "cluster set-autoscaling"},
	{"internal/engine/provisioning/autoscaling.go", remedyCheckWord, "cluster clear-autoscaling", false, "the name of the running autoscaling command, passed as the verb parameter of planClusterFamilyMutation", "cluster clear-autoscaling"},
	{"internal/engine/extres/kubeconfigrefusal.go", remedyCheckWord, "create kubeconfig", false, "the Feature of the capability details: the command the server cannot serve, named as data; nothing to run", "create kubeconfig"},
}

// remedyScanOfTree is the checks over every production file of the
// repository, computed once.
var remedyScanOfTree = sync.OnceValues(func() (remedyHits, error) {
	tree := parseSourceTree()
	if tree.err != nil {
		return remedyHits{}, tree.err
	}
	cmds, err := loadRemedyCommands(schemaGolden)
	if err != nil {
		return remedyHits{}, err
	}
	return scanRemedies(tree.fset, tree.files, tree.paths, cmds), nil
})

func reportHits(t *testing.T, hits []remedyHit, what string) {
	t.Helper()
	for _, h := range hits {
		t.Errorf("%s:%d (%s) %q %s", h.file, h.line, h.rule, h.shown(), what)
	}
}

func reportExceptions(t *testing.T, unused []remedyException) {
	t.Helper()
	for _, e := range unused {
		t.Errorf("%s: the %s exception for %q matches no text; remove it or fix its text", e.file, e.check, e.text)
	}
}

// TestNoRemedyCommandIsWrittenWithoutTheMarker asserts every production
// file writes a command that reads the config directory on clierr.Rancher,
// so the invocation can name its directory in it: no plain rancher command,
// no word rancher joined to text that does not start with a space, no word
// list that starts with the word. The texts that stay plain are listed.
//
// Limits: the check reads literals. A variable that holds the word or the
// marker is not read: prog := "rancher" then prog + " get users", or
// r := clierr.Rancher then r + "get users". A printf-style function whose
// parameters do not end with a string and ...any is not a format function,
// and a word list whose first element is not "rancher" is not a word list.
// The format of a call that takes the marker is read when it is a literal or
// a chain of literals; a format with an argument index or a star, or with an
// operand that is not a literal, is not read, and the argument of a verb
// that follows the marker's verb is not read. A string literal after the
// marker in a call of Sprint, Print or Fprint is read; an operand that is
// not a literal is not. Package fmt imported with a dot is not read. The
// marker in parentheses ((clierr.Rancher) + " get users") is not read. A tab
// right after the marker's verb is reported, though the command prints
// correctly. The word rancher at the end of a literal is read when one space
// or none follows it, not two. The Markdown files the CLI installs are not
// Go source and are not scanned.
func TestNoRemedyCommandIsWrittenWithoutTheMarker(t *testing.T) {
	t.Parallel()
	hits, err := remedyScanOfTree()
	require.NoError(t, err)
	left, unused := applyExceptions(hits.marker, remedyCheckMarker, remedyExceptions)
	reportHits(t, left, "writes a rancher command as plain text; build it on clierr.Rancher")
	reportExceptions(t, unused)
}

// TestNoRemedyCommandIsWrittenWithoutTheWordRancher asserts every production
// file names a command in a remedy with the word rancher: no command written
// after a word that introduces one without it, in a literal, in a chain of
// operands or in a hint the hint-grammar flattener resolves. A command is
// read after the start of the text, a colon, a semicolon, a ", then" or one
// of the words with, use, check, run, ask, to and is in any case, with one
// backtick allowed before it. A run of spaces and tabs counts as one space.
//
// Limits: the words that introduce a command are a fixed list. A command
// after other words (needs `create resource -f`), a top-level command with
// no subcommands (grant, revoke, kubeconfig, logs) and a command after a
// period, a newline or a U+00A0 NO-BREAK SPACE are found only after the word
// rancher. Text that names a command
// as a noun, and text that names no command, are found by no scan and are
// decided by hand. A hint the flattener cannot trace to a literal is not
// read.
func TestNoRemedyCommandIsWrittenWithoutTheWordRancher(t *testing.T) {
	t.Parallel()
	hits, err := remedyScanOfTree()
	require.NoError(t, err)
	left, unused := applyExceptions(hits.word, remedyCheckWord, remedyExceptions)
	reportHits(t, left, "writes a command without the word rancher; build it on clierr.Rancher")
	reportExceptions(t, unused)
}

// TestEveryExampleStartsWithRancher asserts every example of a command is
// a command line that starts with the word rancher, or runs it after a pipe
// or in a process substitution.
//
// Limits: a selector of a field named Examples (the examples of another
// descriptor) is trusted. An assignment to such a field elsewhere is not
// read.
func TestEveryExampleStartsWithRancher(t *testing.T) {
	t.Parallel()
	hits, err := remedyScanOfTree()
	require.NoError(t, err)
	reportHits(t, hits.examples, "is an example that does not start with the word rancher")
}

// TestHelpTextWritesACommandAsRancherInBackticks asserts the help text a
// description, a usage line, an argument description, a kind note, a group
// title or description and a telemetry reason holds names a command as
// rancher and the command in backticks.
//
// Limits: only values written directly under a help key, in the two group
// tables and in the NoDeleteReason and Why fields are read. Help text built
// through a constant or a variable is read as an ordinary literal only. A
// command after a parenthesis or a period is not at a clause start. The
// word rancher is read after one space, not after two.
func TestHelpTextWritesACommandAsRancherInBackticks(t *testing.T) {
	t.Parallel()
	hits, err := remedyScanOfTree()
	require.NoError(t, err)
	reportHits(t, hits.prose, "names a command in help text another way than as rancher <command> in backticks")
}

// TestRemedyExceptionsAreReviewed asserts every exception names a check, a
// file in the repository and a reason.
func TestRemedyExceptionsAreReviewed(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	for _, e := range remedyExceptions {
		assert.NotEmpty(t, e.why, "%s %q has no reason", e.file, e.text)
		assert.Contains(t, []string{remedyCheckMarker, remedyCheckWord}, e.check, "%s %q", e.file, e.text)
		assert.Contains(t, tree.paths, e.file, "%s is not a production file", e.file)
	}
}

// remedyFixtureCases lists every fixture package, with the hits each check
// finds in it. A hit is written rule: text.
var remedyFixtureCases = []struct {
	fixture                       string
	marker, word, examples, prose []string
}{
	{fixture: "plain", marker: []string{"A: rancher get users", "A: run rancher --config x get roletemplates"}},
	{fixture: "joinedoperand", marker: []string{"B: then rancher ", "B: rancher"}},
	{fixture: "writestring", marker: []string{"B: rancher "}},
	{fixture: "formatmiddle", marker: []string{"C: check with rancher %s", "C: rancher %s failed"}},
	{fixture: "formatargument", marker: []string{"B: rancher", "B: rancher "}},
	{fixture: "formatfunctions", marker: []string{"C: rancher %s", "C: see rancher %s", "C: then rancher %s"}},
	{fixture: "missingspace", marker: []string{"D: get users", "D: (an operand that is not a literal starting with a space)"}},
	{fixture: "wordlist", marker: []string{"E: rancher"}},
	{fixture: "wordless", word: []string{"word: delete token in recover: delete token t-1"}},
	{fixture: "joinedwordless", word: []string{"word: access list in  access list --cluster x"}},
	{fixture: "namebuilder", word: []string{"word: get users in get users", "word: access clusters in access clusters", "word: delete user in delete user "}},
	{fixture: "positions"},
	{fixture: "helpfields"},
	{fixture: "compositehelp", marker: []string{"A: rancher get users", "A: rancher get users %s"}},
	{fixture: "directoryfree"},
	{fixture: "helptables", prose: []string{"prose: use rancher get users", "prose: rancher get users"}},
	{fixture: "prosebackticks", prose: []string{"prose: create it with `create cluster` first", "prose: use 'create cluster'", "prose: (see: rancher get users)"}},
	{fixture: "examples", examples: []string{"examples: get users", "examples: get X"}},
	{fixture: "aliaspairs", word: []string{"word: get users in get users", "word: delete tokens in x; use delete tokens", "word: get user in get user u-1 -o json"}},
	{fixture: "clierrmarker", marker: []string{"D: get users"}},
	{fixture: "chainedcommands", word: []string{
		"word: get users in get users; get roletemplates",
		"word: get roletemplates in get users; get roletemplates",
		"word: get users in x, then get users, then get roletemplates",
		"word: get roletemplates in x, then get users, then get roletemplates",
		"word: get users in get users ",
		"word: access list in ; use access list",
	}},
	{fixture: "clausestarts", word: []string{
		"word: get roletemplates in role not found; get roletemplates -o json",
		"word: access clusters in Use access clusters",
		"word: access list in malformed value; use `access list --cluster x`",
	}},
	{fixture: "backtickword", marker: []string{"B: x; run `rancher "}},
	{fixture: "printfamily", marker: []string{"B: rancher", "B: rancher ", "B: then rancher"}},
	{fixture: "formatmarker", marker: []string{
		"D: %sget users",
		"D: %s %sget %s",
		"D: %sget roletemplates",
		"D: 100%% %sget clusters",
		"D: %sget tokens",
	}},
	{fixture: "renamedimport", marker: []string{"D: get users", "D: %sget roletemplates"}},
	{fixture: "hintflattened", word: []string{
		"word: get users in hint list them with: get users",
		"word: get roletemplates in get roletemplates",
		"word: access clusters in use access clusters %s",
		"word: access list in hint see: access list",
	}},
	{fixture: "examplesvalue", examples: []string{
		"examples: (an Examples value that is not a list of literals)",
		"examples: (an Examples value that is not a list of literals)",
	}},
	{fixture: "backtickstarts", word: []string{
		"word: get users in run it: `get users`",
		"word: get roletemplates in malformed value; `get roletemplates`",
		"word: access clusters in x, then `access clusters`",
		"word: access list in `access list` first",
		"word: get users in use\tget users",
	}},
	{fixture: "markerverbs", marker: []string{"D: %s%%x"}},
	{fixture: "printmarker", marker: []string{"D: get", "D: fetch"}},
	{fixture: "renamedfmt", marker: []string{"D: %sget users", "B: rancher", "D: get"}},
	{fixture: "dotimport", marker: []string{"D: get users", "D: %sget roletemplates"}},
	{fixture: "hintindirect", word: []string{"word: get users in use get users", "word: access clusters in use access clusters"}},
	{fixture: "extraspaces", marker: []string{"A: rancher  get users", "A: rancher   --config x get roletemplates"}},
}

// remedyFixtureHits scans one fixture package.
func remedyFixtureHits(t *testing.T, dir string) remedyHits {
	t.Helper()
	cmds, err := loadRemedyCommands(schemaGolden)
	require.NoError(t, err)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	fset := token.NewFileSet()
	var files []*ast.File
	var paths []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		path := filepath.ToSlash(filepath.Join(dir, e.Name()))
		f, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
		require.NoError(t, err)
		files = append(files, f)
		paths = append(paths, path)
	}
	require.NotEmpty(t, files, dir)
	return scanRemedies(fset, files, paths, cmds)
}

func hitStrings(hits []remedyHit) []string {
	out := []string{}
	for _, h := range hits {
		out = append(out, h.String())
	}
	slices.Sort(out)
	return out
}

func sorted(in []string) []string {
	out := slices.Clone(in)
	if out == nil {
		out = []string{}
	}
	slices.Sort(out)
	return out
}

// TestRemedyLiteralScanFixtures asserts each check finds exactly the listed
// hits in its fixture packages: a command written plain, joined to the word
// without a space or from a word list, after a format, a format function
// or a closure; a command without the word, alone or joined from operands;
// help text read or skipped by field; and the examples.
func TestRemedyLiteralScanFixtures(t *testing.T) {
	t.Parallel()
	for _, tc := range remedyFixtureCases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			hits := remedyFixtureHits(t, filepath.Join(remedyLiteralsDir, tc.fixture))
			assert.Equal(t, sorted(tc.marker), hitStrings(hits.marker), "marker")
			assert.Equal(t, sorted(tc.word), hitStrings(hits.word), "word")
			assert.Equal(t, sorted(tc.examples), hitStrings(hits.examples), "examples")
			assert.Equal(t, sorted(tc.prose), hitStrings(hits.prose), "prose")
		})
	}
}

// TestRemedyLiteralFixturesAreAllListed asserts every fixture package on
// disk is in the table above.
func TestRemedyLiteralFixturesAreAllListed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(remedyLiteralsDir)
	require.NoError(t, err)
	var onDisk, listed []string
	for _, e := range entries {
		if e.IsDir() {
			onDisk = append(onDisk, e.Name())
		}
	}
	for _, tc := range remedyFixtureCases {
		listed = append(listed, tc.fixture)
	}
	assert.ElementsMatch(t, listed, onDisk)
}

// TestRemedyExceptionsApplyExactly asserts the exceptions record a text
// exactly: a text that is added, removed or swapped leaves a hit or an
// exception over, and a command written without the word rancher is covered
// by the exception that names its pair.
func TestRemedyExceptionsApplyExactly(t *testing.T) {
	t.Parallel()
	entries := []remedyException{
		{"a.go", remedyCheckMarker, "rancher get users", false, "reason", ""},
		{"a.go", remedyCheckMarker, "long text", true, "reason", ""},
	}
	hit := func(file, text string) remedyHit { return remedyHit{file: file, text: text} }

	left, unused := applyExceptions([]remedyHit{hit("a.go", "rancher get users"), hit("a.go", "a long text here")}, remedyCheckMarker, entries)
	assert.Empty(t, left)
	assert.Empty(t, unused)

	left, unused = applyExceptions([]remedyHit{hit("a.go", "rancher get users")}, remedyCheckMarker, entries)
	assert.Empty(t, left)
	assert.Len(t, unused, 1, "a removed text leaves its exception unused")

	left, unused = applyExceptions([]remedyHit{hit("a.go", "rancher get users"), hit("a.go", "a long text here"), hit("a.go", "rancher config status")}, remedyCheckMarker, entries)
	assert.Len(t, left, 1, "an added text is a hit no exception covers")
	assert.Empty(t, unused)

	left, unused = applyExceptions([]remedyHit{hit("a.go", "rancher config status"), hit("a.go", "a long text here")}, remedyCheckMarker, entries)
	assert.Len(t, left, 1, "a swapped text leaves a hit and an exception")
	assert.Len(t, unused, 1)

	left, unused = applyExceptions([]remedyHit{hit("b.go", "rancher get users"), hit("a.go", "a long text here")}, remedyCheckMarker, entries)
	assert.Len(t, left, 1, "an exception covers its own file only")
	assert.Len(t, unused, 1)

	words := []remedyException{{"a.go", remedyCheckWord, "get users; get roletemplates", false, "reason", "get users"}}
	pairHit := func(pair string) remedyHit {
		return remedyHit{file: "a.go", text: "get users; get roletemplates", pair: pair}
	}
	left, unused = applyExceptions([]remedyHit{pairHit("get users"), pairHit("get roletemplates")}, remedyCheckWord, words)
	require.Len(t, left, 1, "an exception covers the one command it names, not every command of the text")
	assert.Equal(t, "get roletemplates", left[0].pair)
	assert.Empty(t, unused)
}
