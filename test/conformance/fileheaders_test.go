package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fileHeaderFixturesDir holds the packages the comment line scan runs over in
// the fixture tests.
const fileHeaderFixturesDir = "test/conformance/testdata/fileheaders"

// commentLinePattern matches a line that starts with a number sign, after
// any spaces or tabs: a comment, a markdown heading, a shebang. YAML and a
// POSIX shell read a number sign at the start of a line as a comment whether
// or not a space follows it.
var commentLinePattern = regexp.MustCompile(`^[ \t]*#`)

const (
	classConstant  = "constant:"
	classTerminal  = "terminal:"
	classGenerated = "generated:"
	classHeader    = "header:"
	classDocument  = "document:"
	classStatic    = "static:"
)

// commentLineHit is a string literal of a production file with a comment
// line in it. joined is true when the literal is an operand of a + chain
// that holds an operand that is not a string literal, so a value can follow
// the literal in the text.
type commentLineHit struct {
	file     string
	line     int
	function string
	text     string
	joined   bool
}

func (h commentLineHit) hasVerb() bool { return strings.Contains(h.text, "%") }

func (h commentLineHit) String() string {
	return fmt.Sprintf("%s:%d %q", h.file, h.line, h.text)
}

// fixtureString is the form the fixture table lists a hit in.
func (h commentLineHit) fixtureString() string {
	return fmt.Sprintf("%q joined=%t verb=%t", h.text, h.joined, h.hasVerb())
}

// constantKey names a constant by its package directory and its identifier.
type constantKey struct{ dir, name string }

// stringConstants returns the string literals each constant of the files is
// declared with, by package directory and name. A constant is found by name
// only: a constant of another package is not followed.
func stringConstants(files []*ast.File, paths []string) map[constantKey][]*ast.BasicLit {
	out := map[constantKey][]*ast.BasicLit{}
	for i, f := range files {
		dir := path.Dir(paths[i])
		ast.Inspect(f, func(n ast.Node) bool {
			decl, ok := n.(*ast.GenDecl)
			if !ok || decl.Tok != token.CONST {
				return true
			}
			for _, spec := range decl.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for j, name := range value.Names {
					if j >= len(value.Values) {
						continue
					}
					key := constantKey{dir, name.Name}
					ast.Inspect(value.Values[j], func(m ast.Node) bool {
						if lit, ok := m.(*ast.BasicLit); ok && lit.Kind == token.STRING {
							out[key] = append(out[key], lit)
						}
						return true
					})
				}
			}
			return true
		})
	}
	return out
}

// operandSet is what an expression holds as text: the string literals and
// constants it is made of, and whether it also holds a value that is neither.
type operandSet struct {
	literals []*ast.BasicLit
	names    []constantKey
	hasValue bool
}

func (s *operandSet) add(o operandSet) {
	s.literals = append(s.literals, o.literals...)
	s.names = append(s.names, o.names...)
	s.hasValue = s.hasValue || o.hasValue
}

// joinedStringLiterals returns the string literals of the files that have a
// value next to them in one expression, so a value can follow them in the
// text. An expression holds a value when one of its operands is neither a
// string literal nor a constant of the same package directory. The
// expressions are the operands of a + chain, the elements of a composite
// literal (a keyed element by its value), and the text arguments of a call
// that joins them into one value: fmt.Sprint, fmt.Sprintf, fmt.Sprintln,
// fmt.Fprint, fmt.Fprintf and fmt.Fprintln (after the writer), strings.Join
// and append. A conversion (string(x), []byte(x)) keeps the text of its
// argument. A literal inside any other call, or inside a slice or index
// expression, counts as joined when that expression is an operand of a +
// chain, a composite literal or a joining call, because its result counts as
// a value. The arguments of any other call are each looked at alone. The
// literal of a constant counts when the constant's name stands in such an
// expression; an identifier is a constant only when the parser did not
// resolve it to a variable, parameter or other local object.
func joinedStringLiterals(files []*ast.File, paths []string) map[*ast.BasicLit]bool {
	consts := stringConstants(files, paths)
	joined := map[*ast.BasicLit]bool{}
	for i, f := range files {
		dir := path.Dir(paths[i])
		var classify func(e ast.Expr) operandSet
		collect := func(exprs ...ast.Expr) operandSet {
			var set operandSet
			for _, e := range exprs {
				set.add(classify(e))
			}
			return set
		}
		combine := func(exprs ...ast.Expr) operandSet {
			set := collect(exprs...)
			if set.hasValue {
				for _, l := range set.literals {
					joined[l] = true
				}
				for _, name := range set.names {
					for _, l := range consts[name] {
						joined[l] = true
					}
				}
			}
			return set
		}
		classify = func(e ast.Expr) operandSet {
			switch x := e.(type) {
			case *ast.BasicLit:
				if x.Kind == token.STRING {
					return operandSet{literals: []*ast.BasicLit{x}}
				}
			case *ast.Ident:
				key := constantKey{dir, x.Name}
				if _, ok := consts[key]; ok && (x.Obj == nil || x.Obj.Kind == ast.Con) {
					return operandSet{names: []constantKey{key}}
				}
			case *ast.ParenExpr:
				return classify(x.X)
			case *ast.KeyValueExpr:
				return classify(x.Value)
			case *ast.BinaryExpr:
				if x.Op == token.ADD {
					return combine(x.X, x.Y)
				}
			case *ast.CompositeLit:
				return combine(x.Elts...)
			case *ast.CallExpr:
				if isConversion(x) {
					return combine(x.Args...)
				}
				if text, ok := joinedTextArguments(x); ok {
					return combine(text...)
				}
				set := collect(x.Args...)
				set.hasValue = true
				return set
			case *ast.SliceExpr:
				set := classify(x.X)
				set.hasValue = true
				return set
			case *ast.IndexExpr:
				set := classify(x.X)
				set.hasValue = true
				return set
			}
			return operandSet{hasValue: true}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BinaryExpr:
				if x.Op == token.ADD {
					combine(x.X, x.Y)
				}
			case *ast.CompositeLit:
				combine(x.Elts...)
			case *ast.CallExpr:
				if text, ok := joinedTextArguments(x); ok {
					combine(text...)
				}
			}
			return true
		})
	}
	return joined
}

// joinedTextArguments returns the arguments of call that become one text and
// true when call is one of the calls that join their text arguments into one
// value: fmt.Sprint, Sprintf and Sprintln; fmt.Fprint, Fprintf and Fprintln,
// whose first argument is the writer; strings.Join; append.
func joinedTextArguments(call *ast.CallExpr) ([]ast.Expr, bool) {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return call.Args, fun.Name == "append"
	case *ast.SelectorExpr:
		pkg, ok := fun.X.(*ast.Ident)
		if !ok {
			return nil, false
		}
		switch {
		case pkg.Name == "fmt" && (fun.Sel.Name == "Sprint" || fun.Sel.Name == "Sprintf" || fun.Sel.Name == "Sprintln"):
			return call.Args, true
		case pkg.Name == "fmt" && (fun.Sel.Name == "Fprint" || fun.Sel.Name == "Fprintf" || fun.Sel.Name == "Fprintln"):
			if len(call.Args) == 0 {
				return nil, false
			}
			return call.Args[1:], true
		case pkg.Name == "strings" && fun.Sel.Name == "Join":
			return call.Args, true
		}
	}
	return nil, false
}

// isConversion reports whether call converts its one argument to a type
// (string(x), []byte(x)), which keeps the text of the argument.
func isConversion(call *ast.CallExpr) bool {
	if len(call.Args) != 1 {
		return false
	}
	switch fun := call.Fun.(type) {
	case *ast.ArrayType:
		return true
	case *ast.Ident:
		return fun.Name == "string"
	}
	return false
}

// declFunction names the function a declaration is: Type.Method for a
// method, the name for a function, "" for any other declaration.
func declFunction(d ast.Decl) string {
	fd, ok := d.(*ast.FuncDecl)
	if !ok {
		return ""
	}
	if fd.Recv != nil && len(fd.Recv.List) == 1 {
		recv := fd.Recv.List[0].Type
		if star, ok := recv.(*ast.StarExpr); ok {
			recv = star.X
		}
		if id, ok := recv.(*ast.Ident); ok {
			return id.Name + "." + fd.Name.Name
		}
	}
	return fd.Name.Name
}

func hasCommentLine(value string) bool {
	for line := range strings.SplitSeq(value, "\n") {
		if commentLinePattern.MatchString(line) {
			return true
		}
	}
	return false
}

// scanCommentLines returns every string literal of the files that holds a
// comment line, one hit for each literal.
func scanCommentLines(fset *token.FileSet, files []*ast.File, paths []string) ([]commentLineHit, error) {
	var hits []commentLineHit
	joined := joinedStringLiterals(files, paths)
	for i, f := range files {
		for _, decl := range f.Decls {
			function := declFunction(decl)
			var scanErr error
			ast.Inspect(decl, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					scanErr = fmt.Errorf("%s: %w", fset.Position(lit.Pos()), err)
					return false
				}
				if hasCommentLine(value) {
					hits = append(hits, commentLineHit{
						file: paths[i], line: fset.Position(lit.Pos()).Line,
						function: function, text: value, joined: joined[lit],
					})
				}
				return true
			})
			if scanErr != nil {
				return nil, scanErr
			}
		}
	}
	return hits, nil
}

// commentLineEntry is one reviewed literal with a comment line in it: the
// production file, the text, the class and the reason (which starts with the
// class). A constant entry matches a literal that holds its text, so the text
// is the first line of the literal; every other class matches the exact text.
// A header entry also names the test of its package that puts hostile values
// in, and the command constructor that test must call.
type commentLineEntry struct {
	file, text, class, reason string
	test, covers              string
}

func (e commentLineEntry) matches(h commentLineHit) bool {
	if e.file != h.file {
		return false
	}
	if e.class == classConstant {
		return strings.Contains(h.text, e.text)
	}
	return h.text == e.text
}

// commentLineLiterals lists every string literal of the production source
// that holds a comment line, a markdown heading or a shebang, so no file
// builds the first line of generated text from a server value unseen:
//   - constant: a text with no format verb and no value next to it in one
//     expression, checked here. An expression is a + chain, a composite
//     literal, or the text arguments of fmt.Sprint, Sprintf, Sprintln,
//     fmt.Fprint, Fprintf, Fprintln (after the writer), strings.Join or
//     append; a literal inside another call, a slice or an index counts as
//     joined when that expression is an operand of a + chain, a composite
//     literal or a joining call, because its result counts as a value; the
//     name of a constant of the same package counts for its literal. A
//     parameter or a local variable with the name of a constant is a value.
//   - terminal: text printed to a terminal through output.Printable or
//     output.PrintableLines, never written to a file. Not checked beyond the
//     exact text.
//   - generated: a document built from package constants. Not checked beyond
//     the exact text.
//   - header: the first line of a file that holds a server value. Its test
//     must exist in the package and call the command it covers.
var commentLineLiterals = []commentLineEntry{
	{
		file: "internal/commands/configcmd/config_edit.go", class: classConstant,
		text:   "# No rancher CLI config file exists yet at this path.",
		reason: "constant: the text a new config file is seeded with in the editor; no value enters",
	},
	{
		file: "internal/commands/configcmd/set_completion.go", class: classTerminal,
		text:   "# ",
		reason: "terminal: the note the command prints; the runtime filters the text of a result before it is written",
	},
	{
		file: "internal/runtime/pipeline.go", class: classTerminal,
		text:   "# warning:",
		reason: "terminal: the prefix of a warning line mirrored to stderr, with the warning text passed through output.Printable",
	},
	{
		file: "internal/runtime/pipeline.go", class: classTerminal,
		text:   "# warning:",
		reason: "terminal: the prefix of a warning line in an error text, with the warning text passed through output.Printable",
	},
	{
		file: "internal/telemetry/contract.go", class: classGenerated,
		text:   "# Rancher usage events: what is recorded (contract %s)\n\n",
		reason: "generated: the published contract document, built from package constants",
	},
	{
		file: "internal/telemetry/contract.go", class: classGenerated,
		text:   "## Keys outside JSON Schema\n\n",
		reason: "generated: the published contract document, built from package constants",
	},
	{
		file: "internal/telemetry/contract.go", class: classGenerated,
		text:   "## The flat event\n\n",
		reason: "generated: the published contract document, built from package constants",
	},
	{
		file: "internal/telemetry/contract.go", class: classGenerated,
		text:   "## Limits\n\n",
		reason: "generated: the published contract document, built from package constants",
	},
	{
		file: "internal/telemetry/contract.go", class: classGenerated,
		text:   "## Never recorded\n\n",
		reason: "generated: the published contract document, built from package constants",
	},
	{
		file: "internal/telemetry/contract.go", class: classGenerated,
		text:   "## Never uploaded\n\n",
		reason: "generated: the published contract document, built from package constants",
	},
	{
		file: "internal/telemetry/contract.go", class: classGenerated,
		text:   "## Command and flag names\n\n",
		reason: "generated: the published contract document, built from package constants",
	},
	{
		file: "internal/telemetry/contract.go", class: classGenerated,
		text:   "## Version rule\n\n",
		reason: "generated: the published contract document, built from package constants",
	},
	{
		file: "internal/telemetry/contract.go", class: classGenerated,
		text:   "## Event `%s`\n\n%s\n\n",
		reason: "generated: the published contract document, built from package constants",
	},
	{
		file: "internal/telemetry/contract.go", class: classGenerated,
		text:   "## Rules between attributes\n\n",
		reason: "generated: the published contract document, built from package constants",
	},
	{
		file: "internal/telemetry/contract.go", class: classGenerated,
		text:   "# Mapping the flat event to OTLP (contract %s)\n\n",
		reason: "generated: the published contract document, built from package constants",
	},
	{
		file: "internal/clierr/recovery.go", class: classTerminal,
		text:   "   # log in as user ",
		reason: "terminal: a comment after a command in recovery text (config info, token records, warnings, errors); the user id has control characters removed and is shell-quoted",
	},
	{
		file: "internal/telemetry/contract.go", class: classGenerated,
		text:   "#/$defs/",
		reason: "generated: a JSON Schema reference built from package constants",
	},
	{
		file: "internal/engine/extres/kubeconfig.go", class: classHeader,
		text:   "# Kubeconfig %q - shown once; revoke with: rancher delete kubeconfig %s\n",
		reason: "header: the name in the server's reply, in Go's quoted form, and in the command only when it prints as it is",
		test:   "TestCreateKubeconfigHeaderStaysOneCommentLine", covers: "NewCreateKubeconfig",
	},
	{
		file: "internal/engine/extres/kubeconfig.go", class: classHeader,
		text:   "# Kubeconfig %q - shown once; no revoke command is shown because the name is empty or holds characters a terminal cannot show\n",
		reason: "header: the name in the server's reply, in Go's quoted form, with no command",
		test:   "TestCreateKubeconfigHeaderStaysOneCommentLine", covers: "NewCreateKubeconfig",
	},
}

// packageTestFiles parses the _test.go files of one package directory, by
// slash path.
func packageTestFiles(dir string) ([]*ast.File, error) {
	all, err := repositoryFiles()
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, p := range all {
		if path.Dir(p) != dir || !strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
}

// isTestFunction reports whether fd is a function go test runs: no receiver,
// a name that starts with Test, one parameter of type *testing.T, and a body.
func isTestFunction(fd *ast.FuncDecl) bool {
	if fd.Recv != nil || fd.Body == nil || !strings.HasPrefix(fd.Name.Name, "Test") {
		return false
	}
	params := fd.Type.Params.List
	if len(params) != 1 || len(params[0].Names) > 1 {
		return false
	}
	star, ok := params[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "T" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "testing"
}

// headerEntryProblem reports why the test a header entry names does not
// cover it: no test function of that name is in the package, or its body
// never refers to the command constructor the entry covers. It returns "" when the entry is
// covered.
func headerEntryProblem(e commentLineEntry) string {
	dir := path.Dir(e.file)
	files, err := packageTestFiles(dir)
	if err != nil {
		return err.Error()
	}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || !isTestFunction(fd) || fd.Name.Name != e.test {
				continue
			}
			found := false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == e.covers {
					found = true
				}
				return !found
			})
			if !found {
				return fmt.Sprintf("the test %s in %s does not refer to %s", e.test, dir, e.covers)
			}
			return ""
		}
	}
	return fmt.Sprintf("the test %s is missing from the test files of %s", e.test, dir)
}

// commentLineProblems matches the hits to the entries one to one and
// returns every problem: a literal no entry covers, an entry that covers no
// literal, an entry that is not well formed, a constant literal with a verb
// or an operand joined to it, a header entry whose test is missing or never
// calls its command, and a test that serves literals of two functions.
func commentLineProblems(paths []string, hits []commentLineHit, entries []commentLineEntry) []string {
	var problems []string
	classes := []string{classConstant, classTerminal, classGenerated, classHeader}
	for _, e := range entries {
		if !slices.Contains(classes, e.class) {
			problems = append(problems, fmt.Sprintf("%s %q has the class %q, not one of %v", e.file, e.text, e.class, classes))
		}
		if e.text == "" || e.reason == "" || !strings.HasPrefix(e.reason, e.class) {
			problems = append(problems, fmt.Sprintf("%s %q needs a text and a reason that starts with %q", e.file, e.text, e.class))
		}
		if !slices.Contains(paths, e.file) {
			problems = append(problems, fmt.Sprintf("%s is not a production file", e.file))
		}
		if (e.class == classHeader) != (e.test != "" && e.covers != "") || (e.class != classHeader && (e.test != "" || e.covers != "")) {
			problems = append(problems, fmt.Sprintf("%s %q: only a header entry names a test and the command it covers, and it must name both", e.file, e.text))
		}
	}
	used := make([]bool, len(hits))
	testFunctions := map[string]string{}
	for _, e := range entries {
		matched := -1
		for i, h := range hits {
			if !used[i] && e.matches(h) {
				used[i], matched = true, i
				break
			}
		}
		if matched < 0 {
			problems = append(problems, fmt.Sprintf("%s: the %s entry for %q matches no literal; remove it or fix its text", e.file, e.class, e.text))
			continue
		}
		h := hits[matched]
		switch e.class {
		case classConstant:
			if h.hasVerb() || h.joined {
				problems = append(problems, fmt.Sprintf("%s is listed as a constant and has a format verb or a value joined to it", h))
			}
		case classHeader:
			if p := headerEntryProblem(e); p != "" {
				problems = append(problems, fmt.Sprintf("%s: %s", h, p))
			}
			key := path.Dir(e.file) + "." + e.test
			if first, ok := testFunctions[key]; ok && first != h.function {
				problems = append(problems, fmt.Sprintf("%s: the test %s covers %s and %s; a test serves one function", h, e.test, first, h.function))
			} else if !ok {
				testFunctions[key] = h.function
			}
		}
	}
	for i, h := range hits {
		if !used[i] {
			problems = append(problems, fmt.Sprintf("%s writes a comment line no entry covers; list it in commentLineLiterals with its class", h))
		}
	}
	return problems
}

// TestEveryCommentLineLiteralIsListed asserts that every string literal of a
// production file that holds a comment line (a line that starts with a number
// sign and a space, more number signs or a bang) is listed with a class and a
// reason, so a file that builds its first line from a server value is found
// in review. A file header that holds a server value names the test that puts
// hostile values in and the command that test calls.
//
// The scan reads literals. A line is a comment line when its first character
// that is not a space or a tab is a number sign. It does not see a number sign
// built at run time, a comment after other text on the same line ("key: value
// # "), a value appended to a header in a later statement or in another call
// on the same writer or buffer, a value that joins a variable (local or of
// the package) or a constant of another package, a literal that a call
// outside the listed ones fills with a value (strings.Replace or ReplaceAll,
// a text/template, fmt.Append, bytes.Join), a listed call made through a
// renamed import of fmt or strings, a package variable used outside its own
// file when a function of the package declares a constant with the same
// name, another comment syntax (a
// double slash, a semicolon, an HTML comment), a header read from an embedded
// file that is not Go source, or a file written without Result.RawBytes from
// text built out of a server value. It does not check what a header test
// asserts, only that it is a test function of the package that calls the
// command, and the terminal and generated classes are not checked beyond the
// exact text.
func TestEveryCommentLineLiteralIsListed(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	hits, err := scanCommentLines(tree.fset, tree.files, tree.paths)
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	for _, p := range commentLineProblems(tree.paths, hits, commentLineLiterals) {
		t.Error(p)
	}
}

// TestCommentLineChecksReportEachProblem asserts the checks over the hits and
// the entries fail for a literal no entry covers, an entry that covers no
// literal, an entry that is not well formed, and a constant literal with a
// verb or a joined value.
func TestCommentLineChecksReportEachProblem(t *testing.T) {
	t.Parallel()
	paths := []string{"p/a.go"}
	hit := func(text string, joined bool) commentLineHit {
		return commentLineHit{file: "p/a.go", line: 3, function: "f", text: text, joined: joined}
	}
	for _, tc := range []struct {
		name    string
		hits    []commentLineHit
		entries []commentLineEntry
		want    string
	}{
		{
			name: "listed",
			hits: []commentLineHit{hit("# a\n", false)},
			entries: []commentLineEntry{
				{file: "p/a.go", class: classConstant, text: "# a", reason: "constant: fixed"},
			},
		},
		{
			name: "unlisted literal",
			hits: []commentLineHit{hit("# a\n", false)},
			want: "no entry covers",
		},
		{
			name:    "unused entry",
			entries: []commentLineEntry{{file: "p/a.go", class: classTerminal, text: "# a", reason: "terminal: fixed"}},
			want:    "matches no literal",
		},
		{
			name: "constant with a verb",
			hits: []commentLineHit{hit("# a %s\n", false)},
			entries: []commentLineEntry{
				{file: "p/a.go", class: classConstant, text: "# a", reason: "constant: fixed"},
			},
			want: "listed as a constant",
		},
		{
			name: "constant with a joined value",
			hits: []commentLineHit{hit("# a ", true)},
			entries: []commentLineEntry{
				{file: "p/a.go", class: classConstant, text: "# a", reason: "constant: fixed"},
			},
			want: "listed as a constant",
		},
		{
			name: "exact text only for other classes",
			hits: []commentLineHit{hit("# a\n", false)},
			entries: []commentLineEntry{
				{file: "p/a.go", class: classTerminal, text: "# a", reason: "terminal: fixed"},
			},
			want: "matches no literal",
		},
		{
			name: "reason without the class",
			hits: []commentLineHit{hit("# a", false)},
			entries: []commentLineEntry{
				{file: "p/a.go", class: classTerminal, text: "# a", reason: "fixed"},
			},
			want: "needs a text and a reason",
		},
		{
			name: "unknown class",
			hits: []commentLineHit{hit("# a", false)},
			entries: []commentLineEntry{
				{file: "p/a.go", class: "other:", text: "# a", reason: "other: fixed"},
			},
			want: "has the class",
		},
		{
			name: "file that is not production source",
			hits: []commentLineHit{hit("# a", false)},
			entries: []commentLineEntry{
				{file: "p/b.go", class: classTerminal, text: "# a", reason: "terminal: fixed"},
			},
			want: "is not a production file",
		},
		{
			name: "header entry with no test",
			hits: []commentLineHit{hit("# a", false)},
			entries: []commentLineEntry{
				{file: "p/a.go", class: classHeader, text: "# a", reason: "header: fixed"},
			},
			want: "must name both",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			problems := strings.Join(commentLineProblems(paths, tc.hits, tc.entries), "\n")
			if tc.want == "" {
				assert.Empty(t, problems)
				return
			}
			assert.Contains(t, problems, tc.want)
		})
	}
}

// TestHeaderEntryTestMustBeATestFunction asserts a function counts as the test
// of a header entry only when go test runs it: no receiver, a name that starts
// with Test, one *testing.T parameter, and a body.
func TestHeaderEntryTestMustBeATestFunction(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		decl string
		want bool
	}{
		{"test function", "func TestX(t *testing.T) { _ = New }", true},
		{"helper with no parameter", "func helper() { _ = New }", false},
		{"helper with the testing parameter", "func helperX(t *testing.T) { _ = New }", false},
		{"lower case test", "func testX(t *testing.T) { _ = New }", false},
		{"method", "func (r receiver) TestX(t *testing.T) { _ = New }", false},
		{"no parameter", "func TestX() { _ = New }", false},
		{"two parameters", "func TestX(t *testing.T, n int) { _ = New }", false},
		{"benchmark parameter", "func TestX(b *testing.B) { _ = New }", false},
		{"declaration with no body", "func TestX(t *testing.T)", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, err := parser.ParseFile(token.NewFileSet(), "x_test.go", "package p\n"+tc.decl+"\n", parser.SkipObjectResolution)
			require.NoError(t, err)
			fd, ok := f.Decls[0].(*ast.FuncDecl)
			require.True(t, ok)
			assert.Equal(t, tc.want, isTestFunction(fd))
		})
	}
}

// fileHeaderFixtureCases lists every fixture package under
// testdata/fileheaders with the hits the scan finds in it. A hit is the
// quoted text, then whether a value is joined to it and whether it holds a
// format verb.
var fileHeaderFixtureCases = []struct {
	fixture string
	hits    []string
}{
	{fixture: "format", hits: []string{`"# generated for %s\n" joined=true verb=true`}},
	{fixture: "joined", hits: []string{`"# " joined=true verb=false`, `"# note: " joined=true verb=false`}},
	{fixture: "multiline", hits: []string{`"first line\n# a comment line in the middle\nlast line\n" joined=false verb=false`}},
	{fixture: "heading", hits: []string{`"## Title" joined=false verb=false`}},
	{fixture: "shebang", hits: []string{`"#!/bin/sh\n" joined=false verb=false`}},
	{fixture: "notcomment"},
	{fixture: "nospace", hits: []string{`"#tag" joined=false verb=false`, `"#/$defs/x" joined=false verb=false`}},
	{fixture: "indented", hits: []string{`"  # note " joined=true verb=false`, `"\t# tab\n" joined=false verb=false`}},
	{fixture: "callarg", hits: []string{
		`"# Kubeconfig " joined=true verb=false`, `"# Kubeconfig " joined=true verb=false`,
		`"# a" joined=true verb=false`, `"# fixed" joined=false verb=false`,
	}},
	{fixture: "nested", hits: []string{
		`"# parameter " joined=true verb=false`, `"# call result " joined=true verb=false`,
		`"# sliced text" joined=true verb=false`, `"# keyed " joined=true verb=false`,
		`"# written\n" joined=false verb=false`, `"# fixed header\n" joined=false verb=false`,
		`"# variable " joined=false verb=false`, `"# literal argument\n" joined=false verb=false`,
	}},
	{fixture: "constantoperand", hits: []string{`"# Kubeconfig " joined=true verb=false`, `"# fixed\n" joined=false verb=false`}},
	{fixture: "constant", hits: []string{`"# a" joined=false verb=false`, `"# fixed text\n" joined=false verb=false`}},
}

// fixtureCommentLineHits scans one fixture package.
func fixtureCommentLineHits(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	fset := token.NewFileSet()
	var files []*ast.File
	var paths []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		p := path.Join(dir, e.Name())
		f, err := parser.ParseFile(fset, p, nil, parser.AllErrors)
		require.NoError(t, err)
		files = append(files, f)
		paths = append(paths, p)
	}
	require.NotEmpty(t, files, dir)
	hits, err := scanCommentLines(fset, files, paths)
	require.NoError(t, err)
	out := []string{}
	for _, h := range hits {
		out = append(out, h.fixtureString())
	}
	slices.Sort(out)
	return out
}

// TestCommentLineScanFixtures asserts the scan finds exactly the listed hits
// in its fixture packages: a format literal, a literal joined to a value
// (by +, as an argument of a call, in a composite literal, through a
// constant), a comment line in the middle of a raw string, a heading, a
// shebang, a number sign with no space after it, an indented comment line,
// text that only looks like a comment line, and a constant.
func TestCommentLineScanFixtures(t *testing.T) {
	t.Parallel()
	for _, tc := range fileHeaderFixtureCases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			want := slices.Clone(tc.hits)
			if want == nil {
				want = []string{}
			}
			slices.Sort(want)
			assert.Equal(t, want, fixtureCommentLineHits(t, path.Join(fileHeaderFixturesDir, tc.fixture)))
		})
	}
}

// TestCommentLineFixturesAreAllListed asserts every fixture package on disk
// is in the table of the fixture test.
func TestCommentLineFixturesAreAllListed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(fileHeaderFixturesDir)
	require.NoError(t, err)
	var listed, onDisk []string
	for _, tc := range fileHeaderFixtureCases {
		listed = append(listed, tc.fixture)
	}
	for _, e := range entries {
		if e.IsDir() {
			onDisk = append(onDisk, e.Name())
		}
	}
	assert.ElementsMatch(t, listed, onDisk)
}

// fileOutputWriters classifies each function that writes Result.RawBytes by
// what the bytes begin with. Its keys are the keys of rawBytesWriters: a
// writer in one table and not in the other fails the test below.
//   - header: the bytes begin with a comment line that can hold a server
//     value; the package has a header entry in commentLineLiterals, covered
//     by a test.
//   - document: the bytes are a marshaled document with no text of its own.
//   - static: the bytes are embedded in the binary.
var fileOutputWriters = map[string]struct{ class, reason string }{
	"internal/engine/extres.createKubeconfigCmd.Plan": {
		classHeader, "header: a comment line with the name of the Kubeconfig the server created, then the kubeconfig the server returned",
	},
	"internal/engine/kubeconfig.kubeconfigCmd.Execute": {
		classDocument, "document: the bytes of clientcmd.Write; the command adds no text of its own",
	},
	"internal/cli.completionCommand.Execute": {
		classStatic, "static: the completion script embedded in the binary",
	},
}

// writerPackage returns the slash path of the package directory in the name
// of a function as outputwrites_test.go lists it
// ("internal/engine/extres.createKubeconfigCmd.Plan").
func writerPackage(function string) (string, bool) {
	dir, _, ok := strings.Cut(function, ".")
	return dir, ok && dir != "" && !strings.Contains(dir, `\`)
}

// TestEveryRawBytesWriterIsClassified asserts every function that writes
// Result.RawBytes has a class here, a class for a function that is gone is
// removed, a writer of the header class has a header entry in
// commentLineLiterals whose test covers it, and a writer of another class has
// none in its package.
func TestEveryRawBytesWriterIsClassified(t *testing.T) {
	t.Parallel()
	assert.Equal(t, slices.Sorted(maps.Keys(rawBytesWriters)), slices.Sorted(maps.Keys(fileOutputWriters)),
		"the writers of Result.RawBytes and the writers classified by what they begin with differ")
	for _, function := range slices.Sorted(maps.Keys(fileOutputWriters)) {
		w := fileOutputWriters[function]
		assert.True(t, strings.HasPrefix(w.reason, w.class), "%s: the reason starts with %q", function, w.class)
		assert.Contains(t, []string{classHeader, classDocument, classStatic}, w.class, function)
		dir, ok := writerPackage(function)
		require.True(t, ok, "%s names no package directory", function)
		var headers []commentLineEntry
		for _, e := range commentLineLiterals {
			if e.class == classHeader && path.Dir(e.file) == dir {
				headers = append(headers, e)
			}
		}
		if w.class == classHeader {
			require.NotEmpty(t, headers, "%s is a header writer and its package lists no header entry", function)
			for _, e := range headers {
				assert.Empty(t, headerEntryProblem(e), "%s %q", function, e.text)
			}
			continue
		}
		assert.Empty(t, headers, "%s is classified as %s and its package lists a header entry", function, w.class)
	}
}
