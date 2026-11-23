package provisioning

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readValueFunctions are the functions of the package that handle a text the
// user typed or piped before the command's filter can be relied on to hide it:
// the functions that read the flag arguments, and every helper they hand a flag argument or a
// part of it to. A function of the package that an entry of listedReadUses
// names as a callee is in this list or in formBuilders, so a helper an entry
// permits is itself scanned. A new function that handles such a text joins the
// list.
var readValueFunctions = []string{
	"readWords", "checkFieldFlags", "readFieldFlags", "splitFieldWords", "recordSetWords",
	"unknownFieldError", "secretInSetError", "quoteField", "checkRequired", "named", "decodesAsKubeconfig",
	"valueProblemError",
}

// formBuilder is a function that builds the forms of a text for the filter, on
// purpose: it is not scanned, because showing every form of a text is what it
// does for the filter, and the guard of TestFormBuildersReachOnlyTheFilter
// keeps its result from reaching any other function.
type formBuilder struct{ name, reason string }

// formBuilders are called by the entries of listedReadUses and are not scanned.
var formBuilders = []formBuilder{
	{"record", "adds the forms of a text to the filter and returns nothing, so no refusal can show them"},
	{"redactionTexts", "lists the forms of each text for the filter; its result only feeds the filter, which hides more when a form is added"},
	{"redactionForms", "lists the forms of one text for the filter; its result only feeds the filter, which hides more when a form is added"},
}

// formBuilderCaller is a function, or a method of the type receiver, that may
// use the result of a form builder. The receiver is empty for a function.
type formBuilderCaller struct{ name, receiver string }

// formBuilderCallers are the functions that may use the result of a form
// builder.
var formBuilderCallers = []formBuilderCaller{
	{"record", "readTexts"},
	{"redactionTexts", ""},
	{"responseFilter", ""},
}

// readValueNames are the identifiers those functions give a typed or read
// text: the bytes as read (raw), the text after the byte order marks (text),
// the cleaned value (cleaned), the two parts of a flag argument (field, value),
// the whole flag argument (w, words), the lists of typed flag arguments (sets,
// files) and the typed field name a refusal helper receives (typed). A flag
// argument is the text given to one --set or --from-file, as opposed to the
// argument of a call. Inside a function the scan also watches every name that
// is set from a watched text (see watchedUsesIn). The path of a --from-file
// flag argument is not watched; see resultHoldsNoText.
var readValueNames = []string{"raw", "text", "cleaned", "value", "field", "w", "words", "sets", "files", "typed"}

// readValueMembers are the members of a flag argument that have a part of it: the value
// of a flag argument, also as w.value, and its field, also as w.field. The scan adds
// every other member the source declares for fieldWord (a field or a method),
// whatever the name of the variable that has the flag argument.
var readValueMembers = []string{"value", "field"}

// wordPartsNotInput are the members of a flag argument that have no typed text: the
// name of the flag and the position of the flag argument on the command line.
var wordPartsNotInput = []string{"flag", "position"}

// wordType is the type of a flag argument.
const wordType = "fieldWord"

// readUse is one place where a typed or read text is used: the function, the
// operation (the callee of a call, an operator, an index, an assignment, a
// literal or a range), the expression that has the text and, for a call, the
// position of that expression among its arguments.
type readUse struct {
	fn, op, expr string
	arg          int
}

// readUseAt is a readUse with its position in the source.
type readUseAt struct {
	use readUse
	pos string
}

// listedUse is a readUse the scan permits, with the reason.
type listedUse struct {
	use    readUse
	reason string
}

// listedReadUses is the allowlist of the scan: every use of a typed or read
// text in readValueFunctions, including a use of a name set from one. An entry
// permits its operation on its expression in its function, at its argument
// position, only; the same call in another function, on another expression or
// with the arguments swapped is not permitted. The reason of an entry gives its
// class: a lookup, a comparison, a length, a range, a split, a map store or
// index, a sort comparison, a conversion, the call that records the text for
// the filter, a cleaning or encoding check, a literal of fieldWord or
// credentialValue, building, sorting or returning the list of flag arguments or of
// values to send, arithmetic on the position of a flag argument, a call of a scanned
// helper (checked in its own body), a call of the YAML decoder that only reports
// whether the text decodes, or the quote of a field name that matched a field of
// the driver and the message that joins it (the precondition is the caller's;
// the scan cannot prove the order).
var listedReadUses = []listedUse{
	{readUse{"splitFieldWords", "range", "words", 0}, "walks the flag arguments; nothing is shown"},
	{readUse{"splitFieldWords", "len", "words", 0}, "sizes the result; nothing is shown"},
	{readUse{"splitFieldWords", "strings.Cut", "w", 0}, "splits a flag argument at its first equals sign"},
	{readUse{"splitFieldWords", "==", "field", 0}, "compares the field part with the empty string"},
	{readUse{"splitFieldWords", "==", "value", 0}, "compares the value part with the empty string"},
	{readUse{"splitFieldWords", "literal fieldWord", "field", 0}, "builds the flag argument from its field part"},
	{readUse{"splitFieldWords", "literal fieldWord", "value", 0}, "builds the flag argument from its value part"},
	{readUse{"splitFieldWords", "+", "i", 0}, "numbers the position of the flag argument; a number, not text"},
	{readUse{"splitFieldWords", "append", "out", 0}, "adds the flag argument to the list of flag arguments; nothing is shown"},
	{readUse{"splitFieldWords", "use in *ast.ReturnStmt", "out", 0}, "returns the flag arguments to the caller; nothing is shown"},

	{readUse{"recordSetWords", "range", "words", 0}, "walks the --set flag arguments; nothing is shown"},
	{readUse{"recordSetWords", "strings.Cut", "w", 0}, "splits a flag argument at its first equals sign"},
	{readUse{"recordSetWords", "r.record", "value", 0}, "adds the value part to the filter"},

	{readUse{"readFieldFlags", "splitFieldWords", "in.Strings(flagSet)", 1}, "passes the --set flag arguments to a scanned helper, which names each flag argument in its refusals by flag and position"},
	{readUse{"readFieldFlags", "splitFieldWords", "in.Strings(flagFromFile)", 1}, "passes the --from-file flag arguments to a scanned helper, which names each flag argument in its refusals by flag and position"},
	{readUse{"readFieldFlags", "indexed", "seen", 0}, "looks up and stores a flag argument in the map of fields seen; nothing is shown"},
	{readUse{"readFieldFlags", "range", "files", 0}, "walks the --from-file flag arguments; nothing is shown"},
	{readUse{"readFieldFlags", "==", "f.value", 0}, "compares the value of a --from-file flag argument with the stdin marker"},
	{readUse{"readFieldFlags", "slices.Concat", "sets", 0}, "joins the lists of flag arguments to walk them; nothing is shown"},
	{readUse{"readFieldFlags", "slices.Concat", "files", 1}, "joins the lists of flag arguments to walk them; nothing is shown"},
	{readUse{"readFieldFlags", "index seen", "w.field", 0}, "looks up a field in the map of fields seen; nothing is shown"},
	{readUse{"readFieldFlags", "assign seen[w.field]", "w", 0}, "stores the flag argument under its field; nothing is shown"},
	{readUse{"readFieldFlags", "use in *ast.ReturnStmt", "sets", 0}, "returns the flag arguments to the caller; nothing is shown"},
	{readUse{"readFieldFlags", "use in *ast.ReturnStmt", "files", 0}, "returns the flag arguments to the caller; nothing is shown"},

	{readUse{"checkFieldFlags", "range", "sets", 0}, "walks the --set flag arguments; nothing is shown"},
	{readUse{"checkFieldFlags", "range", "files", 0}, "walks the --from-file flag arguments; nothing is shown"},
	{readUse{"checkFieldFlags", "d.Field", "w.field", 0}, "looks the field up in the driver"},
	{readUse{"checkFieldFlags", "d.SecretFolded", "w.field", 0}, "looks a secret field up in the driver without regard to case"},
	{readUse{"checkFieldFlags", "secretInSetError", "w.field", 0}, "called only after the flag argument matched a secret field of the driver, exactly or without regard to case; a scanned helper"},
	{readUse{"checkFieldFlags", "unknownFieldError", "w", 1}, "calls a scanned helper, which quotes the field name only when it equals a field of the driver without regard to case and names the flag argument by flag and position otherwise"},

	{readUse{"secretInSetError", "quoteField", "typed", 0}, "quotes the field name; the caller passes it only after the flag argument matched a secret field of the driver, exactly or without regard to case; a scanned helper"},
	{readUse{"secretInSetError", "!=", "typed", 0}, "compares the typed field name with the canonical one"},
	{readUse{"secretInSetError", "fmt.Sprintf", "quoteField(typed)", 1}, "writes the refusal with the quoted field name; the caller passes it only after the flag argument matched a secret field of the driver, exactly or without regard to case"},
	{readUse{"secretInSetError", "clierr.New", "msg", 1}, "makes the error of the message above; nothing else is added"},

	{readUse{"unknownFieldError", "call of w.named", "w.named", 0}, "names the flag argument by its flag and position; a scanned method"},
	{readUse{"unknownFieldError", "d.FieldFolded", "w.field", 0}, "looks the field up in the driver without regard to case"},
	{readUse{"unknownFieldError", "quoteField", "w.field", 0}, "quotes the field name only in the branch where d.FieldFolded matched it; a scanned helper"},
	{readUse{"unknownFieldError", "+", "quoteField(w.field)", 0}, "joins the quoted field name to the subject only in the branch where d.FieldFolded matched it"},
	{readUse{"unknownFieldError", "fmt.Sprintf", "subject", 1}, "shows the subject: the quoted field name when it matched a field of the driver without regard to case, the flag and position of the flag argument otherwise"},

	{readUse{"quoteField", "strconv.Quote", "field", 0}, "quotes the field name; every caller passes a name that matched a field of the driver"},

	{readUse{"checkRequired", "slices.Concat", "sets", 0}, "joins the lists of flag arguments to look for a field; nothing is shown"},
	{readUse{"checkRequired", "slices.Concat", "files", 1}, "joins the lists of flag arguments to look for a field; nothing is shown"},
	{readUse{"checkRequired", "slices.ContainsFunc", "slices.Concat(sets, files)", 0}, "looks for a flag argument with a required field; a boolean"},
	{readUse{"checkRequired", "==", "w.field", 0}, "compares the field part with the name of a required field"},

	{readUse{"decodesAsKubeconfig", "[]byte", "text", 0}, "converts the text for the YAML decoder, which only reports whether it decodes; shows nothing"},
	{readUse{"decodesAsKubeconfig", "yaml.Unmarshal", "[]byte(text)", 0}, "hands the text to the YAML decoder, which only reports whether it decodes; shows nothing"},
	{readUse{"decodesAsKubeconfig", "!=", "err", 0}, "compares the error of the decoder with nil; only whether the text decodes is used"},

	{readUse{"readWords", "range", "sets", 0}, "walks the --set flag arguments; nothing is shown"},
	{readUse{"readWords", "range", "files", 0}, "walks the --from-file flag arguments; nothing is shown"},
	{readUse{"readWords", "d.Field", "w.field", 0}, "looks the field up in the driver"},
	{readUse{"readWords", "record", "w.value", 0}, "adds the value to the filter"},
	{readUse{"readWords", "record", "string(raw)", 0}, "adds the bytes as read to the filter"},
	{readUse{"readWords", "record", "text", 0}, "adds the text after the byte order marks to the filter"},
	{readUse{"readWords", "record", "cleaned", 0}, "adds the cleaned value to the filter"},
	{readUse{"readWords", "cloudcredfields.Clean", "w.value", 1}, "applies the white space rules"},
	{readUse{"readWords", "cloudcredfields.Clean", "text", 1}, "applies the white space rules"},
	{readUse{"readWords", "assign path", "w.value", 0}, "the value of a --from-file flag argument is a path, which a refusal quotes as typed"},
	{readUse{"readWords", "cloudcredfields.RawEncodingProblem", "raw", 0}, "checks the encoding of the bytes"},
	{readUse{"readWords", "engine.DropLeadingByteOrderMarks", "raw", 0}, "drops the byte order marks"},
	{readUse{"readWords", "decodesAsKubeconfig", "cleaned", 0}, "checks the shape of the document; a scanned helper that shows nothing"},
	{readUse{"readWords", "literal credentialValue", "cleaned", 0}, "builds the value to send"},
	{readUse{"readWords", "strings.Compare", "a.field.Name", 0}, "sorts by the name of the driver's field, not by typed text"},
	{readUse{"readWords", "strings.Compare", "b.field.Name", 1}, "sorts by the name of the driver's field, not by typed text"},
	{readUse{"readWords", "append", "out", 0}, "adds the value to the list of values to send; nothing is shown"},
	{readUse{"readWords", "slices.SortFunc", "out", 0}, "sorts the list of values to send by the name of the driver's field"},
	{readUse{"readWords", "use in *ast.ReturnStmt", "out", 0}, "returns the values to send to the caller; nothing is shown"},
}

// resultRow names an operation, with the reason for the rule the table that
// has the row states about its result.
type resultRow struct{ op, reason string }

// resultHoldsNoText are the operations whose result has no part of a typed
// or read text, so a name set from it, and the result itself, are not watched.
// The operation is named as describeReadValue names it.
var resultHoldsNoText = []resultRow{
	{"d.Field", "looks a name up in the driver; the result is the driver's field and whether it exists"},
	{"d.FieldFolded", "looks a name up in the driver without regard to case; the result is the driver's field and whether it exists"},
	{"d.SecretFolded", "looks a name up in the driver without regard to case; the result is the driver's secret field and whether it exists"},
	{"cloudcredfields.RawEncodingProblem", "reports which encoding problem the bytes have, not the bytes"},
	{"decodesAsKubeconfig", "reports whether the text has the shape of a kubeconfig; a boolean"},
	{"len", "a number"},
	{"==", "a boolean"},
	{"!=", "a boolean"},
	{"slices.ContainsFunc", "reports whether a flag argument matches; a boolean"},
	{"call of w.named", "names the flag argument by its flag and position, never by what the user typed; the method is scanned"},
	{"assign path", "the value of a --from-file flag argument is a path; a refusal quotes it as typed on purpose, and the command line has it, so the name path is not watched"},
	{"fmt.Sprintf", "joins its arguments into a message; the entry that permits the call states what it may show, and the message is not followed again"},
	{"clierr.New", "makes the error of a message; the entry that permits the call states what it may show, and the error is not followed again"},
}

// carryRow names an operation with several results and the positions of the
// results that have a part of the text. A result not listed has none.
// An operation that is in neither table has the text in every result.
type carryRow struct {
	op      string
	results []int
	reason  string
}

var resultsWithText = []carryRow{
	{"strings.Cut", []int{0, 1}, "the part before and the part after the first separator; the third result is a boolean"},
	{"cloudcredfields.Clean", []int{0}, "the cleaned value; the other results are the problem found and whether the value was trimmed"},
	{"splitFieldWords", []int{0}, "the typed flag arguments; the error text is built without them, and the function is scanned"},
	{"index seen", []int{0}, "the flag argument stored under a field; the second result is a boolean"},
	{"indexed", []int{0}, "the element of a list or the value of a map; the second result of a map lookup is a boolean"},
}

// holdsNoText reports whether the result of op has no text.
func holdsNoText(op string) bool {
	return slices.ContainsFunc(resultHoldsNoText, func(r resultRow) bool { return r.op == op })
}

// carriesText reports whether result number k of op has a part of the text.
func carriesText(op string, k int) bool {
	for _, r := range resultsWithText {
		if r.op == op {
			return slices.Contains(r.results, k)
		}
	}
	return true
}

// sourceFile is a Go file of the package under scan.
type sourceFile struct {
	name string
	src  []byte
}

// packageDir is the directory of the package whose files the scan reads.
const packageDir = "."

// packageSources returns every Go file of the package that is not a test file.
// The files are only parsed: build constraints are not evaluated, so a file of
// another platform is scanned too.
func packageSources(t *testing.T) []sourceFile {
	t.Helper()
	entries, err := os.ReadDir(packageDir)
	require.NoError(t, err)
	var out []sourceFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(packageDir, name))
		require.NoError(t, err)
		out = append(out, sourceFile{name: name, src: src})
	}
	return out
}

// parsedSource is the parsed Go source of a package with what the scan needs of
// it. decls has every function and method of every file, in order; declared
// has them by name (the last one of a name); duplicates the names declared
// more than once; imports the names of the imported packages; members the
// members of a flag argument that have input.
type parsedSource struct {
	fset       *token.FileSet
	files      []*ast.File
	decls      []*ast.FuncDecl
	declared   map[string]*ast.FuncDecl
	duplicates []string
	imports    []string
	members    []string
}

// receiverIs reports whether decl is a method of the type name.
func receiverIs(decl *ast.FuncDecl, name string) bool {
	if decl.Recv == nil || len(decl.Recv.List) == 0 {
		return false
	}
	typ := decl.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	id, ok := typ.(*ast.Ident)
	return ok && id.Name == name
}

// parseSource parses one source as the file cloudcredwrite.go.
func parseSource(t *testing.T, src []byte) parsedSource {
	t.Helper()
	return parseSources(t, []sourceFile{{name: "cloudcredwrite.go", src: src}})
}

// parseSources parses the files of one package. Imports, functions and members
// of a flag argument come from every file: the value and field of a flag argument, and every other
// field or method the source declares for fieldWord except flag and position.
func parseSources(t *testing.T, sources []sourceFile) parsedSource {
	t.Helper()
	ps := parsedSource{fset: token.NewFileSet(), declared: map[string]*ast.FuncDecl{}, members: slices.Clone(readValueMembers)}
	addMember := func(name string) {
		if !slices.Contains(ps.members, name) && !slices.Contains(wordPartsNotInput, name) {
			ps.members = append(ps.members, name)
		}
	}
	for _, source := range sources {
		file, err := parser.ParseFile(ps.fset, source.name, source.src, 0)
		require.NoError(t, err)
		ps.files = append(ps.files, file)
		for _, imp := range file.Imports {
			name := path.Base(strings.Trim(imp.Path.Value, `"`))
			if imp.Name != nil {
				name = imp.Name.Name
			}
			if !slices.Contains(ps.imports, name) {
				ps.imports = append(ps.imports, name)
			}
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if _, again := ps.declared[d.Name.Name]; again && !slices.Contains(ps.duplicates, d.Name.Name) {
					ps.duplicates = append(ps.duplicates, d.Name.Name)
				}
				ps.declared[d.Name.Name] = d
				ps.decls = append(ps.decls, d)
				if receiverIs(d, wordType) {
					addMember(d.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok || ts.Name.Name != wordType {
						continue
					}
					if st, ok := ts.Type.(*ast.StructType); ok {
						for _, field := range st.Fields.List {
							for _, name := range field.Names {
								addMember(name.Name)
							}
						}
					}
				}
			}
		}
	}
	return ps
}

// watchSet is what the scan watches in one function: the members of a flag argument, the
// names (the identifiers of readValueNames and the names set from a watched
// text) and the calls whose result has a watched text.
type watchSet struct {
	members []string
	names   []string
	derived map[ast.Node]bool
}

// operandOfOperation reports whether the expression that ends at node, the last
// of stack, is an argument of a call or an operand of a binary operation, after
// parentheses, member accesses and string conversions.
func operandOfOperation(stack []ast.Node) bool {
	i := climbOperand(stack)
	if i == 0 {
		return false
	}
	switch p := stack[i-1].(type) {
	case *ast.CallExpr:
		return slices.Contains(p.Args, stack[i].(ast.Expr))
	case *ast.BinaryExpr:
		return true
	}
	return false
}

// isReadValue reports whether node, the last of stack, has a typed or read
// text: a selector for a member of a flag argument that has input (every member of
// members, whatever the variable is called), an identifier in names, a call of
// in.Strings, or a call whose result has a watched text and that is an
// argument of a call or an operand. A declaration, the target of an assignment,
// a key of a composite literal, the name after a dot, and the flag argument of a selector
// whose member has no text (w.flag, w.position) have none.
func (ws watchSet) isReadValue(stack []ast.Node) bool {
	switch n := stack[len(stack)-1].(type) {
	case *ast.CallExpr:
		if types.ExprString(n.Fun) == "in.Strings" {
			return true
		}
		return ws.derived[n] && operandOfOperation(stack)
	case *ast.SelectorExpr:
		return slices.Contains(ws.members, n.Sel.Name)
	case *ast.Ident:
		if !slices.Contains(ws.names, n.Name) || len(stack) < 2 {
			return false
		}
		switch p := stack[len(stack)-2].(type) {
		case *ast.SelectorExpr:
			if p.Sel == n || slices.Contains(ws.members, p.Sel.Name) || slices.Contains(wordPartsNotInput, p.Sel.Name) {
				return false
			}
		case *ast.KeyValueExpr:
			if p.Key == n {
				return false
			}
		case *ast.AssignStmt:
			if (p.Tok == token.DEFINE || p.Tok == token.ASSIGN) && slices.Contains(p.Lhs, ast.Expr(n)) {
				return false
			}
		case *ast.RangeStmt:
			if (p.Tok == token.DEFINE || p.Tok == token.ASSIGN) && (p.Key == ast.Expr(n) || p.Value == ast.Expr(n)) {
				return false
			}
		case *ast.ValueSpec:
			if slices.Contains(p.Names, n) {
				return false
			}
		case *ast.Field:
			return false
		}
		return true
	}
	return false
}

// climbOperand returns the index in stack of the expression that the operation
// takes: the last of stack, climbed through a member access, parentheses and a
// string conversion, which do not end the expression.
func climbOperand(stack []ast.Node) int {
	i := len(stack) - 1
	for i > 0 {
		cur := stack[i]
		switch p := stack[i-1].(type) {
		case *ast.ParenExpr:
			i--
			continue
		case *ast.SelectorExpr:
			if p.X == cur {
				i--
				continue
			}
		case *ast.CallExpr:
			if id, ok := p.Fun.(*ast.Ident); ok && id.Name == "string" && slices.Contains(p.Args, cur.(ast.Expr)) {
				i--
				continue
			}
		}
		break
	}
	return i
}

// describeReadValue names the operation that takes the text at the end of
// stack, and the expression that has it. A member access, parentheses and a
// string conversion do not end the expression: the operation is the one that
// takes the whole of it.
func describeReadValue(fn string, stack []ast.Node) readUse {
	i := climbOperand(stack)
	expr := stack[i].(ast.Expr)
	use := readUse{fn: fn, expr: types.ExprString(expr)}
	if i == 0 {
		use.op = "use"
		return use
	}
	switch p := stack[i-1].(type) {
	case *ast.CallExpr:
		if k := slices.Index(p.Args, expr); k >= 0 {
			use.op = types.ExprString(p.Fun)
			use.arg = k
		} else {
			use.op = "call of " + types.ExprString(p.Fun)
		}
	case *ast.BinaryExpr:
		use.op = p.Op.String()
	case *ast.IndexExpr:
		use.op = "indexed"
		if p.Index == expr {
			use.op = "index " + types.ExprString(p.X)
		}
	case *ast.CompositeLit:
		use.op = "literal " + types.ExprString(p.Type)
	case *ast.KeyValueExpr:
		use.op = "key"
		if i >= 2 {
			if lit, ok := stack[i-2].(*ast.CompositeLit); ok && p.Value == expr {
				use.op = "literal " + types.ExprString(lit.Type)
			}
		}
	case *ast.AssignStmt:
		use.op = "assigned"
		for k, rhs := range p.Rhs {
			if rhs == expr && k < len(p.Lhs) {
				use.op = "assign " + types.ExprString(p.Lhs[k])
			}
		}
	case *ast.RangeStmt:
		use.op = "range"
	default:
		use.op = fmt.Sprintf("use in %T", p)
	}
	return use
}

// rootName returns the name a target of an assignment sets: the identifier at
// the root of an index, a member access, a dereference or parentheses, and "_"
// for anything else.
func rootName(e ast.Expr) string {
	for {
		switch x := e.(type) {
		case *ast.Ident:
			return x.Name
		case *ast.IndexExpr:
			e = x.X
		case *ast.SelectorExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		default:
			return "_"
		}
	}
}

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// boundTargets returns the names a statement sets from the right side that
// has the child. When one call sets several names, only the results that have
// text count (carriesText), if the operation takes the text directly.
func boundTargets(lhs, rhs []ast.Expr, child, operation ast.Node, use readUse) []string {
	k := slices.IndexFunc(rhs, func(e ast.Expr) bool { return ast.Node(e) == child })
	if k < 0 {
		return nil
	}
	direct := ast.Node(unparen(rhs[k])) == operation
	if len(lhs) == len(rhs) {
		if direct && !carriesText(use.op, 0) {
			return nil
		}
		return []string{rootName(lhs[k])}
	}
	var out []string
	for j, target := range lhs {
		if !direct || carriesText(use.op, j) {
			out = append(out, rootName(target))
		}
	}
	return out
}

// boundNames returns the names that the statement around the text at the end of
// stack sets from it: the targets of an assignment or a declaration, and the key
// and value of a range over it. A statement inside a function literal sets none.
func boundNames(stack []ast.Node, use readUse) []string {
	i := climbOperand(stack)
	if i == 0 {
		return nil
	}
	operation := stack[i-1]
	for j := i - 1; j >= 0; j-- {
		child := stack[j+1]
		switch s := stack[j].(type) {
		case *ast.FuncLit:
			return nil
		case *ast.AssignStmt:
			return boundTargets(s.Lhs, s.Rhs, child, operation, use)
		case *ast.ValueSpec:
			names := make([]ast.Expr, len(s.Names))
			for k, id := range s.Names {
				names[k] = id
			}
			return boundTargets(names, s.Values, child, operation, use)
		case *ast.RangeStmt:
			if child != ast.Node(s.X) {
				return nil
			}
			var out []string
			for _, e := range []ast.Expr{s.Key, s.Value} {
				if e != nil {
					out = append(out, rootName(e))
				}
			}
			return out
		case ast.Stmt:
			return nil
		}
	}
	return nil
}

// walkWithStack calls visit for every node under root with the path of nodes
// from root to it.
func walkWithStack(root ast.Node, visit func(stack []ast.Node)) {
	var stack []ast.Node
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		visit(stack)
		return true
	})
}

// watchedUsesIn returns every use of a typed or read text in the function. A
// name that is set from a watched text (by :=, =, var, or as the key or value
// of a range) is watched too, and so is a call that takes a watched text and
// whose result has it, when its result is the argument of another call or an
// operand; the scan repeats this until no name or call is added. An operation
// of resultHoldsNoText adds nothing; a call named in resultsWithText adds only
// the names set from the results that have text.
func watchedUsesIn(ps parsedSource, name string, fn *ast.FuncDecl) []readUseAt {
	ws := watchSet{members: ps.members, names: slices.Clone(readValueNames), derived: map[ast.Node]bool{}}
	for grew := true; grew; {
		grew = false
		walkWithStack(fn.Body, func(stack []ast.Node) {
			if !ws.isReadValue(stack) {
				return
			}
			use := describeReadValue(name, stack)
			if holdsNoText(use.op) {
				return
			}
			if i := climbOperand(stack); i > 0 {
				if call, ok := stack[i-1].(*ast.CallExpr); ok && !ws.derived[call] {
					ws.derived[call] = true
					grew = true
				}
			}
			for _, b := range boundNames(stack, use) {
				if b != "_" && !slices.Contains(ws.names, b) {
					ws.names = append(ws.names, b)
					grew = true
				}
			}
		})
	}
	var found []readUseAt
	walkWithStack(fn.Body, func(stack []ast.Node) {
		if ws.isReadValue(stack) {
			found = append(found, readUseAt{use: describeReadValue(name, stack), pos: ps.fset.Position(stack[len(stack)-1].Pos()).String()})
		}
	})
	return found
}

// readUsesIn parses src and returns, for each function of funcs, every use of a
// typed or read text, and the names of funcs the source does not declare.
func readUsesIn(t *testing.T, src []byte, funcs []string) (found []readUseAt, missing []string) {
	t.Helper()
	return readUsesInSources(t, parseSource(t, src), funcs)
}

// readUsesInSources is readUsesIn for a package.
func readUsesInSources(t *testing.T, ps parsedSource, funcs []string) (found []readUseAt, missing []string) {
	t.Helper()
	for _, name := range funcs {
		fn, ok := ps.declared[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		found = append(found, watchedUsesIn(ps, name, fn)...)
	}
	return found, missing
}

// unlistedReadUses returns the uses that no entry of allowed permits.
func unlistedReadUses(found []readUseAt, allowed []listedUse) []string {
	var out []string
	for _, f := range found {
		if !slices.ContainsFunc(allowed, func(l listedUse) bool { return l.use == f.use }) {
			out = append(out, fmt.Sprintf("%s: %s uses %s in %s (argument %d)", f.pos, f.use.fn, f.use.expr, f.use.op, f.use.arg))
		}
	}
	return out
}

var dottedName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)

// operations that name no callee.
var notCallOps = []string{"range", "use", "assigned", "indexed", "key"}

// calleeOf returns the name of the function or method an operation calls, as
// the source declares it: the last name of the operation, without the words
// "call of ". An operator, a conversion, a literal, an assignment and a call
// of an imported package's function (strings.Cut) name none.
func calleeOf(op string, imports []string) (string, bool) {
	op = strings.TrimPrefix(op, "call of ")
	if !dottedName.MatchString(op) || slices.Contains(notCallOps, op) {
		return "", false
	}
	if i := strings.LastIndexByte(op, '.'); i >= 0 {
		if slices.Contains(imports, op[:i]) {
			return "", false
		}
		op = op[i+1:]
	}
	return op, true
}

// unscannedCallees returns the entries whose callee the source declares and
// that is neither in scanned nor a form builder: a helper an entry permits is
// scanned too, or it would be a way out of the scan.
func unscannedCallees(ps parsedSource, entries []listedUse, scanned []string, builders []formBuilder) []string {
	var out []string
	for _, l := range entries {
		name, ok := calleeOf(l.use.op, ps.imports)
		if !ok {
			continue
		}
		if _, declared := ps.declared[name]; !declared {
			continue
		}
		if slices.Contains(scanned, name) || slices.ContainsFunc(builders, func(f formBuilder) bool { return f.name == name }) {
			continue
		}
		out = append(out, fmt.Sprintf("%s passes %s to %s, which the source declares and the scan does not cover", l.use.fn, l.use.expr, name))
	}
	return out
}

// declaredMoreThanOnce returns the names of names that the source declares
// more than once, and the names it does not declare.
func declaredMoreThanOnce(ps parsedSource, names []string) []string {
	var out []string
	for _, name := range names {
		if slices.Contains(ps.duplicates, name) {
			out = append(out, name)
		}
	}
	return out
}

// TestReadValuesReachOnlyListedUses asserts that, in the functions that handle
// a text the user typed or piped, such a text is used only where
// listedReadUses permits it. A name is watched when it is one of readValueNames
// (raw, text, cleaned, value, field, w, words, sets, files or typed), when it is
// set from a watched text (by :=, =, var, or as the key or value of a range;
// the scan repeats this until no name is added), and when it is a member of a
// flag argument other than its flag and position (also the value, as w.value, and the
// field, as w.field, and a method of fieldWord, on any variable). So is a call
// of in.Strings, and a call that takes a watched text and whose result has it,
// when that result is the argument of another call or an operand. A watched text
// may be the argument of a call, an operand, an index, a literal element, a
// range or the right side of an assignment only when an entry names that
// function, that operation, that argument position and that expression; any
// other use fails, whatever helper it goes through, whether it joins the text
// into a string or copies it into a variable of another name, and whether it
// passes the result of an allowed call on to another. The operations of
// resultHoldsNoText add nothing watched, and resultsWithText names the results
// of a call that have text; every other result of every other operation does.
// The scan reads every non-test Go file of the package, and covers the helpers
// an entry lets a flag argument reach: an entry whose callee the package declares fails
// unless the callee is in readValueFunctions or is a form builder. The path of a
// --from-file flag argument is not watched: a refusal quotes it as typed, and the
// command line has it. The scan describes the nearest operation of a use and
// follows its result through names and nested calls; a text that reaches a
// message in another way is covered by the refusal tests.
//
// Two layers hide a typed text. The filter that Plan builds (read.refuse)
// hides the --set values in every error of these functions and, from readWords
// on, the values read from files and stdin, in the forms it covers. The part of
// a typed flag argument before its first equals sign is not in the filter, because it
// can equal a field name that the refusals list: this scan and the refusal
// tables cover it. A function named in readValueFunctions that no longer exists
// fails the scan, and so does a name declared twice, an entry no code uses and
// a row of the result tables no code uses, so the lists cannot go stale.
func TestReadValuesReachOnlyListedUses(t *testing.T) {
	sources := packageSources(t)
	ps := parseSources(t, sources)
	found, missing := readUsesInSources(t, ps, readValueFunctions)
	assert.Empty(t, missing, "functions the scan names that the package does not declare")
	assert.Empty(t, declaredMoreThanOnce(ps, readValueFunctions), "scanned names that the package declares more than once")
	assert.Empty(t, unlistedReadUses(found, listedReadUses), "a use of a typed or read text that no entry permits")
	assert.Empty(t, unscannedCallees(ps, listedReadUses, readValueFunctions, formBuilders), "a function an entry permits that the scan does not cover")
	for _, l := range listedReadUses {
		assert.NotEmpty(t, l.reason, "the entry for %v has a reason", l.use)
		assert.True(t, slices.ContainsFunc(found, func(f readUseAt) bool { return f.use == l.use }), "the entry %v is used by the code", l.use)
	}
	usedOp := func(op string) bool {
		return slices.ContainsFunc(found, func(f readUseAt) bool { return f.use.op == op })
	}
	for _, r := range resultHoldsNoText {
		assert.NotEmpty(t, r.reason, "the row for %s has a reason", r.op)
		assert.True(t, usedOp(r.op), "the row %s of resultHoldsNoText is used by the code", r.op)
	}
	for _, r := range resultsWithText {
		assert.NotEmpty(t, r.reason, "the row for %s has a reason", r.op)
		assert.True(t, usedOp(r.op), "the row %s of resultsWithText is used by the code", r.op)
	}
}

// TestScanReadsEveryPackageFile asserts that the files the scan and the guards
// read are the non-test Go files of the package: cloudcredwrite.go and at least
// one other, and none of a test.
func TestScanReadsEveryPackageFile(t *testing.T) {
	var names []string
	for _, source := range packageSources(t) {
		names = append(names, source.name)
		assert.False(t, strings.HasSuffix(source.name, "_test.go"), "%s is a test file", source.name)
	}
	assert.Contains(t, names, "cloudcredwrite.go")
	assert.Greater(t, len(names), 1, "the package has more than one file")
}

// TestReadValueScanFindsEachUse asserts the scan finds a use of a typed or read
// text in each way it knows, passes a use an entry permits, does not let an
// entry for one function, one expression or one argument position permit
// another, and reaches the helpers an entry permits.
func TestReadValueScanFindsEachUse(t *testing.T) {
	const word = "type fieldWord struct {\n\tflag, field, value string\n\tposition int\n}\n"
	const header = "func f(w fieldWord, words []string, sets, files []fieldWord, raw []byte, text, cleaned, path string) error {\n"
	const helper = "func describe(s string) string { return \"value \" + s }\n"
	const method = "func (w fieldWord) shown() string { return w.value }\n"
	runSource := func(src string, funcs []string, allowed ...listedUse) []string {
		found, missing := readUsesIn(t, []byte("package p\n"+word+src), funcs)
		require.Empty(t, missing)
		return unlistedReadUses(found, allowed)
	}
	run := func(body string, allowed ...listedUse) []string {
		return runSource(header+body+"\n}\n"+helper+method, []string{"f"}, allowed...)
	}
	for name, body := range map[string]string{
		"a quoted text":                                   `return clierr.New("c", fmt.Sprintf("the value %q holds a hidden character", text))`,
		"a joined string":                                 `return clierr.New("c", "the value "+string(raw)+" holds a tab")`,
		"a flag argument value":                           `return clierr.New("c", "the value "+w.value+" is refused")`,
		"a flag argument field":                           `return clierr.New("c", "the field "+w.field+" is refused")`,
		"a helper argument":                               `return secretInSetError(w.value, "name")`,
		"a field named by a quote":                        `return clierr.New("c", quoteField(field))`,
		"a quote call":                                    `q := strconv.QuoteToASCII(cleaned); _ = q`,
		"a nested conversion":                             `return clierr.New("c", strings.TrimSpace(text))`,
		"a hint":                                          `return clierr.New("c", "x").WithHint("remove it from " + url.QueryEscape(text))`,
		"a new helper":                                    `return clierr.New("c", describe(text))`,
		"a new helper with a flag argument":               `return clierr.New("c", describe(w.field))`,
		"the whole argument":                              `return clierr.New("c", fmt.Sprintf("%q", w))`,
		"a copy":                                          `t := text; _ = t`,
		"a conversion joined":                             `return clierr.New("c", fmt.Sprint(string(raw)))`,
		"one of the arguments":                            `return clierr.New("c", words[0])`,
		"the typed set flag arguments":                    `return clierr.New("c", fmt.Sprintf("%x", sets))`,
		"the typed file flag arguments":                   `return clierr.New("c", fmt.Sprint(files))`,
		"a method of the flag argument":                   `return clierr.New("c", w.shown())`,
		"a method of a flag argument in another variable": `x := sets[0]; return clierr.New("c", x.shown())`,
		"a member of a flag argument in another variable": `x := files[0]; return clierr.New("c", x.field)`,
	} {
		t.Run(name, func(t *testing.T) {
			assert.NotEmpty(t, run(body))
		})
	}
	t.Run("a value of a range over the flag arguments, with the range permitted", func(t *testing.T) {
		body := `for _, x := range words { return clierr.New("c", fmt.Sprintf("%q", x)) }`
		assert.NotEmpty(t, run(body, listedUse{readUse{"f", "range", "words", 0}, "r"}))
	})
	t.Run("a refusal that names the flag, the position and the path", func(t *testing.T) {
		body := `return clierr.New("c", fmt.Sprintf("--%s argument #%d is refused at %s", w.flag, w.position, path))`
		assert.Empty(t, run(body))
	})
	t.Run("a declaration and a literal key", func(t *testing.T) {
		body := `field, value, ok := strings.Cut(path, "="); _ = ok; return x(fieldWord{field: nil, value: nil, flag: path})`
		assert.Empty(t, run(body))
	})
	t.Run("a use an entry permits", func(t *testing.T) {
		assert.Empty(t, run(`d.Field(w.field)`, listedUse{readUse{"f", "d.Field", "w.field", 0}, "r"}))
		assert.Empty(t, run(`record(string(raw))`, listedUse{readUse{"f", "record", "string(raw)", 0}, "r"}))
		assert.Empty(t, run(`return strings.Compare(a.field.Name, b.field.Name)`,
			listedUse{readUse{"f", "strings.Compare", "a.field.Name", 0}, "r"}, listedUse{readUse{"f", "strings.Compare", "b.field.Name", 1}, "r"}))
		assert.Empty(t, run(`x := credentialValue{field: f, value: cleaned}`, listedUse{readUse{"f", "literal credentialValue", "cleaned", 0}, "r"}))
		assert.Empty(t, run(`return clierr.New("c", w.named())`, listedUse{readUse{"f", "call of w.named", "w.named", 0}, "r"}))
	})
	t.Run("an entry for another function, another operation, another expression or another argument", func(t *testing.T) {
		entry := listedUse{readUse{"g", "d.Field", "w.field", 0}, "r"}
		assert.NotEmpty(t, run(`d.Field(w.field)`, entry))
		entry = listedUse{readUse{"f", "secretInSetError", "w.field", 0}, "r"}
		assert.Empty(t, run(`return secretInSetError(w.field, "name")`, entry))
		assert.NotEmpty(t, run(`return secretInSetError(w.value, "name")`, entry))
		assert.NotEmpty(t, run(`return secretInSetError("name", w.field)`, entry))
		assert.NotEmpty(t, run(`return quoteField(w.field)`, entry))
		assert.NotEmpty(t, run(`return fmt.Sprintf("%s", w.field)`, entry))
		assert.NotEmpty(t, run(`record(string(raw)+"x")`, listedUse{readUse{"f", "record", "string(raw)", 0}, "r"}))
	})
	t.Run("a helper an entry permits is scanned too", func(t *testing.T) {
		call := listedUse{readUse{"f", "quoteField", "w.field", 0}, "r"}
		const callHelper = header + "return quoteField(w.field)\n}\n"
		funcs := []string{"f", "quoteField"}
		assert.Empty(t, runSource(callHelper+"func quoteField(field string) error { return nil }\n", funcs, call))
		assert.Empty(t, runSource(callHelper+"func quoteField(field string) error { return strconv.Quote(field) }\n", funcs, call,
			listedUse{readUse{"quoteField", "strconv.Quote", "field", 0}, "r"}))
		assert.NotEmpty(t, runSource(callHelper+"func quoteField(field string) error { return fmt.Errorf(\"%x\", field) }\n", funcs, call))
		assert.NotEmpty(t, runSource(callHelper+"func quoteField(field string) error { return fmt.Errorf(\"%x\", field) }\n", funcs, call,
			listedUse{readUse{"quoteField", "fmt.Errorf", "field", 0}, "r"}), "an entry for another argument position")
	})
	t.Run("a new method of the flag argument that returns its value", func(t *testing.T) {
		const use = header + "return clierr.New(\"c\", w.shown())\n}\n"
		assert.NotEmpty(t, runSource(use+method, []string{"f"}))
		assert.NotEmpty(t, runSource(use+"func (w *fieldWord) shown() string { return w.value }\n", []string{"f"}))
	})
	t.Run("a function that is not declared", func(t *testing.T) {
		_, missing := readUsesIn(t, []byte("package p\n"+header+"return nil\n}\n"), []string{"f", "gone"})
		assert.Equal(t, []string{"gone"}, missing)
	})
}

// TestReadValueScanFollowsCopies asserts the scan follows a typed or read text
// into a name set from it and into the result of a call, and stops where the
// result has no text: a name set by :=, = or var, the value and the key of a
// range, the results of a split that have text, a call of in.Strings, a list
// built from a text, and the result of an allowed call passed on to another
// call. A use an entry permits stays permitted, and the path of a --from-file
// flag argument is not watched.
func TestReadValueScanFollowsCopies(t *testing.T) {
	const word = "type fieldWord struct {\n\tflag, field, value string\n\tposition int\n}\n"
	const header = "func f(w fieldWord, words []string, sets, files []fieldWord, raw []byte, text, cleaned, path string) error {\n"
	run := func(body string, allowed ...listedUse) []string {
		found, missing := readUsesIn(t, []byte("package p\n"+word+header+body+"\n}\n"), []string{"f"})
		require.Empty(t, missing)
		return unlistedReadUses(found, allowed)
	}
	use := func(op, expr string, arg int) listedUse { return listedUse{readUse{"f", op, expr, arg}, "r"} }
	t.Run("a name set by := from a watched text", func(t *testing.T) {
		body := `v := text; return clierr.New("c", fmt.Sprintf("%x", v))`
		assert.NotEmpty(t, run(body, use("assign v", "text", 0)))
		assert.Empty(t, run(body, use("assign v", "text", 0), use("fmt.Sprintf", "v", 1)), "an entry for the later use permits it")
	})
	t.Run("a name set by = from a watched text", func(t *testing.T) {
		assert.NotEmpty(t, run(`var v string; v = text; return clierr.New("c", fmt.Sprintf("%x", v))`, use("assign v", "text", 0)))
	})
	t.Run("a name set from a name set from a watched text", func(t *testing.T) {
		assert.NotEmpty(t, run(`a := text; b := a; return clierr.New("c", fmt.Sprint(b))`, use("assign a", "text", 0), use("assign b", "a", 0)))
	})
	t.Run("a name declared with var from a watched text", func(t *testing.T) {
		body := `var v = text; return clierr.New("c", fmt.Sprintf("%x", v))`
		assert.NotEmpty(t, run(body))
		assert.NotEmpty(t, run(body, use("use in *ast.ValueSpec", "text", 0)))
	})
	t.Run("the value of a range over the flag arguments", func(t *testing.T) {
		body := `for _, x := range words { return clierr.New("c", fmt.Sprintf("%q", x)) }`
		assert.NotEmpty(t, run(body, use("range", "words", 0)))
		assert.Empty(t, run(body, use("range", "words", 0), use("fmt.Sprintf", "x", 1)))
	})
	t.Run("the key of a range over the flag arguments", func(t *testing.T) {
		assert.NotEmpty(t, run(`for i := range words { return clierr.New("c", fmt.Sprint(i)) }`, use("range", "words", 0)))
	})
	t.Run("the parts of a split under other names", func(t *testing.T) {
		cut := use("strings.Cut", "text", 0)
		assert.NotEmpty(t, run(`k, _, _ := strings.Cut(text, "="); return clierr.New("c", fmt.Sprintf("%x", k))`, cut))
		assert.NotEmpty(t, run(`_, v, _ := strings.Cut(text, "="); return clierr.New("c", fmt.Sprintf("%x", v))`, cut))
		assert.Empty(t, run(`_, _, ok := strings.Cut(text, "="); return clierr.New("c", fmt.Sprint(ok))`, cut), "the boolean has no text")
	})
	t.Run("a call of in.Strings", func(t *testing.T) {
		assert.NotEmpty(t, run(`return clierr.New("c", strings.Join(in.Strings("set"), " "))`))
	})
	t.Run("a list built from a text", func(t *testing.T) {
		body := `var out []string; out = append(out, text); return clierr.New("c", fmt.Sprintf("%v", out))`
		assert.NotEmpty(t, run(body, use("append", "text", 1)))
		assert.Empty(t, run(body, use("append", "text", 1), use("append", "out", 0), use("fmt.Sprintf", "out", 1)))
	})
	t.Run("the result of an allowed call passed to another call", func(t *testing.T) {
		drop := use("engine.DropLeadingByteOrderMarks", "string(raw)", 0)
		assert.NotEmpty(t, run(`return clierr.New("c", fmt.Sprintf("%x", engine.DropLeadingByteOrderMarks(string(raw))))`, drop))
		assert.NotEmpty(t, run(`return clierr.New("c", fmt.Sprint(slices.Concat(sets, files)))`, use("slices.Concat", "sets", 0), use("slices.Concat", "files", 1)))
		assert.Empty(t, run(`v := engine.DropLeadingByteOrderMarks(string(raw)); _ = 1`, drop), "a result that is only set to a name is not a use")
	})
	t.Run("a result that has no text", func(t *testing.T) {
		assert.Empty(t, run(`f, ok := d.Field(w.field); return clierr.New("c", fmt.Sprint(f, ok))`, use("d.Field", "w.field", 0)))
		assert.Empty(t, run(`n := len(words); return clierr.New("c", fmt.Sprint(n))`, use("len", "words", 0)))
	})
	t.Run("the path of a --from-file flag argument", func(t *testing.T) {
		assert.Empty(t, run(`path := w.value; return clierr.New("c", fmt.Sprintf("%s", path))`, use("assign path", "w.value", 0)))
	})
}

// TestReadValueScanReadsEveryFile asserts a use of a typed or read text in a
// second file of the package is found, and a helper that only the second file
// declares is reported when an entry permits a call of it and the scan does not
// cover it.
func TestReadValueScanReadsEveryFile(t *testing.T) {
	const word = "type fieldWord struct {\n\tflag, field, value string\n\tposition int\n}\n"
	first := sourceFile{name: "first.go", src: []byte("package p\n" + word + "func f(w fieldWord) error { return clierr.New(\"c\", describe(w.flag)) }\n")}
	second := sourceFile{name: "second.go", src: []byte("package p\n" +
		"func describe(s string) string { return s }\n" +
		"func g(w fieldWord) error { return clierr.New(\"c\", fmt.Sprintf(\"%x\", w.field)) }\n")}
	ps := parseSources(t, []sourceFile{first, second})
	found, missing := readUsesInSources(t, ps, []string{"f", "g"})
	assert.Empty(t, missing, "a function of the second file is declared")
	problems := unlistedReadUses(found, nil)
	require.Len(t, problems, 1)
	assert.Contains(t, problems[0], "second.go")
	entry := []listedUse{{readUse{"f", "describe", "w.field", 0}, "r"}}
	problems = unscannedCallees(ps, entry, []string{"f"}, nil)
	require.Len(t, problems, 1)
	assert.Contains(t, problems[0], "describe")
	assert.Empty(t, unscannedCallees(ps, entry, []string{"f", "describe"}, nil))
	assert.Equal(t, []string{"describe"}, declaredMoreThanOnce(parseSources(t, []sourceFile{first, second, {name: "third.go", src: []byte("package p\nfunc describe() {}\n")}}), []string{"f", "describe"}))
}

// TestReadValueScanReachesPermittedCallees asserts the scan fails an entry
// whose callee the source declares and the scan does not cover, passes one that
// is scanned, a form builder, an imported package's function or a function the
// source does not declare, and fails a scanned name that is declared twice.
func TestReadValueScanReachesPermittedCallees(t *testing.T) {
	const src = "package p\nimport \"strings\"\n" +
		"func shown(s string) string { return s }\nfunc record() {}\nfunc f() {}\n" +
		"func (r *readTexts) shown() {}\n"
	ps := parseSource(t, []byte(src))
	entry := func(op string) []listedUse { return []listedUse{{readUse{"f", op, "w.field", 0}, "r"}} }
	builders := []formBuilder{{"record", "r"}}
	problems := unscannedCallees(ps, entry("shown"), []string{"f"}, builders)
	require.Len(t, problems, 1)
	assert.Contains(t, problems[0], "shown")
	assert.NotEmpty(t, unscannedCallees(ps, entry("call of w.shown"), []string{"f"}, builders), "a method")
	assert.NotEmpty(t, unscannedCallees(ps, entry("r.shown"), []string{"f"}, builders), "a call on a variable")
	assert.Empty(t, unscannedCallees(ps, entry("shown"), []string{"f", "shown"}, builders), "a scanned callee")
	assert.Empty(t, unscannedCallees(ps, entry("r.record"), []string{"f"}, builders), "a form builder")
	assert.Empty(t, unscannedCallees(ps, entry("strings.Cut"), []string{"f"}, builders), "an imported package's function")
	assert.Empty(t, unscannedCallees(ps, entry("d.Field"), []string{"f"}, builders), "a function the source does not declare")
	assert.Empty(t, unscannedCallees(ps, entry("=="), []string{"f"}, builders), "an operator")
	assert.Empty(t, unscannedCallees(ps, entry("range"), []string{"f"}, builders), "a range")
	assert.Equal(t, []string{"shown"}, declaredMoreThanOnce(ps, []string{"f", "shown"}))
}

// isFormBuilderCaller reports whether decl is one of callers: the name and the
// receiver type both match, so a method of another type with the name of a
// caller is not one.
func isFormBuilderCaller(decl *ast.FuncDecl, callers []formBuilderCaller) bool {
	return slices.ContainsFunc(callers, func(c formBuilderCaller) bool {
		if decl.Name.Name != c.name {
			return false
		}
		if c.receiver == "" {
			return decl.Recv == nil
		}
		return receiverIs(decl, c.receiver)
	})
}

// callerProblems returns each of callers that the package does not declare
// exactly once, so a row that names a function no longer there fails.
func callerProblems(ps parsedSource, callers []formBuilderCaller) []string {
	var out []string
	for _, c := range callers {
		n := 0
		for _, d := range ps.decls {
			if isFormBuilderCaller(d, []formBuilderCaller{c}) {
				n++
			}
		}
		if n != 1 && c.receiver == "" {
			out = append(out, fmt.Sprintf("the function %s is declared %d times, not once", c.name, n))
		} else if n != 1 {
			out = append(out, fmt.Sprintf("the method %s of %s is declared %d times, not once", c.name, c.receiver, n))
		}
	}
	return out
}

// builderProblems returns the references to a form builder that has a result,
// from a function of any file of the package that is not in callers. Such a
// result is the forms of a typed text: only the filter may use it.
func builderProblems(t *testing.T, ps parsedSource, builders []formBuilder, callers []formBuilderCaller) []string {
	t.Helper()
	var out []string
	for _, f := range builders {
		fn, ok := ps.declared[f.name]
		if !ok {
			out = append(out, "the form builder "+f.name+" is not declared")
			continue
		}
		if fn.Type.Results == nil {
			continue
		}
		inspect := func(owner string, node ast.Node) {
			ast.Inspect(node, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == f.name {
					out = append(out, fmt.Sprintf("%s: %s uses the result of %s", ps.fset.Position(id.Pos()), owner, f.name))
				}
				return true
			})
		}
		for _, file := range ps.files {
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if !isFormBuilderCaller(d, callers) && d.Body != nil {
						inspect(d.Name.Name, d.Body)
					}
				case *ast.GenDecl:
					inspect("a declaration of the package", d)
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

// TestFormBuildersReachOnlyTheFilter asserts that the functions that build the
// forms of a typed text for the filter are declared, have a reason, and that
// the result of the ones that return the forms is used, in any file of the
// package, only by the record method of readTexts and the functions
// redactionTexts and responseFilter, each of which the package declares once.
// A refusal can then not show a form of a typed text through them, which is why
// the scan does not cover them.
func TestFormBuildersReachOnlyTheFilter(t *testing.T) {
	ps := parseSources(t, packageSources(t))
	for _, f := range formBuilders {
		assert.NotEmpty(t, f.reason, "the form builder %s has a reason", f.name)
	}
	assert.Empty(t, callerProblems(ps, formBuilderCallers))
	assert.Empty(t, builderProblems(t, ps, formBuilders, formBuilderCallers))
	assert.Empty(t, declaredMoreThanOnce(ps, []string{"record", "redactionTexts", "redactionForms"}))
}

// TestFormBuilderGuardFindsEachUse asserts the guard of the form builders passes
// their uses by the filter, and fails a use in a refusal, a use that is not a
// call, a use in a declaration of the package, a use in a method of another
// type that has the name of a caller, a builder that is not declared and a
// builder that gains a result. It asserts the check of the callers fails a row
// that names no declared function, and a row whose receiver type differs.
func TestFormBuilderGuardFindsEachUse(t *testing.T) {
	builders := []formBuilder{{"record", "r"}, {"redactionTexts", "r"}, {"redactionForms", "r"}}
	const base = "package p\n" +
		"func (r *readTexts) record(texts ...string) { r.forms = append(r.forms, redactionTexts(texts)...) }\n" +
		"func redactionTexts(values []string) []string { return redactionForms(values[0]) }\n" +
		"func redactionForms(value string) []string { return []string{value} }\n" +
		"func responseFilter(values []string) func(string) string { return NewTextFilter(redactionTexts(values), true) }\n"
	callers := formBuilderCallers
	check := func(extra string) []string {
		return builderProblems(t, parseSource(t, []byte(base+extra)), builders, callers)
	}
	assert.Empty(t, check(""))
	assert.NotEmpty(t, check("func unknownFieldError(w fieldWord) error { return clierr.New(\"c\", strings.Join(redactionForms(\"x\"), \",\")) }\n"))
	assert.NotEmpty(t, check("func refuse(w fieldWord) error { return clierr.New(\"c\", strings.Join(redactionTexts(nil), \",\")) }\n"))
	assert.NotEmpty(t, check("func alias() { again := redactionForms; _ = again }\n"))
	assert.NotEmpty(t, check("var forms = redactionForms(\"x\")\n"))
	assert.NotEmpty(t, check("func (o *other) responseFilter() []string { return redactionForms(\"x\") }\n"), "a method of another type with a caller's name")
	assert.NotEmpty(t, check("func (o *other) record() []string { return redactionForms(\"x\") }\n"), "a method of another type named record")
	assert.NotEmpty(t, check("func record() []string { return redactionForms(\"x\") }\n"), "a function with the name of a method caller")
	assert.NotEmpty(t, check("func (r *readTexts) responseFilter() []string { return redactionForms(\"x\") }\n"), "a method with the name of a function caller")
	assert.Empty(t, callerProblems(parseSource(t, []byte(base)), callers), "every caller is declared once")
	assert.NotEmpty(t, callerProblems(parseSource(t, []byte(base)), append(slices.Clone(callers), formBuilderCaller{"answerFilter", ""})), "a row that names no declared function")
	assert.NotEmpty(t, callerProblems(parseSource(t, []byte(base)), []formBuilderCaller{{"record", "other"}}), "a row with another receiver type")
	assert.NotEmpty(t, callerProblems(parseSource(t, []byte(base)), []formBuilderCaller{{"record", ""}}), "a method named as a function")
	problems := builderProblems(t, parseSource(t, []byte("package p\nfunc record() {}\n")), builders, callers)
	assert.Equal(t, 2, len(problems), "two builders are not declared")
	assert.Empty(t, builderProblems(t, parseSource(t, []byte(base+"func use() { record() }\n")), builders[:1], callers), "a builder without a result is not checked")
	gains := strings.Replace(base, "func (r *readTexts) record(texts ...string) {", "func (r *readTexts) record(texts ...string) []string {", 1)
	assert.NotEmpty(t, builderProblems(t, parseSource(t, []byte(gains+"func other() { r.record(\"x\") }\n")), builders[:1], callers), "a builder that returns the forms")
}

// planReadCalls are the calls of Plan whose error can have a text the user
// typed or piped.
var planReadCalls = []string{"readFieldFlags", "checkFieldFlags", "readWords"}

// planReceiver is the type whose Plan method is guarded.
const planReceiver = "createCloudCredentialCmd"

// calleeName returns the name of the function a call calls: the identifier, or
// the member of a selector.
func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}

// calledByNameAt returns the call whose function is the identifier at the end
// of stack (the identifier itself, or the member of a selector), or nil when
// the identifier is used in any other way: an alias, an argument, an
// assignment.
func calledByNameAt(stack []ast.Node) *ast.CallExpr {
	id := stack[len(stack)-1]
	if len(stack) >= 2 {
		if call, ok := stack[len(stack)-2].(*ast.CallExpr); ok && call.Fun == id {
			return call
		}
	}
	if len(stack) >= 3 {
		if sel, ok := stack[len(stack)-2].(*ast.SelectorExpr); ok && sel.Sel == id {
			if call, ok := stack[len(stack)-3].(*ast.CallExpr); ok && call.Fun == sel {
				return call
			}
		}
	}
	return nil
}

// guardedCall returns "" when assign has one call of the function, its error
// is the last result, and the if statement that follows it (its own init
// statement, or the next statement of the block) returns that error only
// through read.refuse.
func guardedCall(assign *ast.AssignStmt, ifStmt *ast.IfStmt) string {
	id, ok := assign.Lhs[len(assign.Lhs)-1].(*ast.Ident)
	if !ok || id.Name == "_" {
		return "the error is not kept in a variable"
	}
	if ifStmt == nil || ifStmt.Else != nil || len(ifStmt.Body.List) != 1 {
		return "the error is not checked by an if statement with one return"
	}
	cond, ok := ifStmt.Cond.(*ast.BinaryExpr)
	if !ok || cond.Op != token.NEQ || types.ExprString(cond.X) != id.Name || types.ExprString(cond.Y) != "nil" {
		return "the if statement does not test the error against nil"
	}
	ret, ok := ifStmt.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) == 0 {
		return "the if statement does not return"
	}
	for _, res := range ret.Results[:len(ret.Results)-1] {
		holdsErr := false
		ast.Inspect(res, func(n ast.Node) bool {
			if i, ok := n.(*ast.Ident); ok && i.Name == id.Name {
				holdsErr = true
			}
			return true
		})
		if holdsErr {
			return "the error is returned in more than the last result"
		}
	}
	last, ok := ret.Results[len(ret.Results)-1].(*ast.CallExpr)
	if !ok || types.ExprString(last.Fun) != "read.refuse" || len(last.Args) != 1 || types.ExprString(last.Args[0]) != id.Name {
		return "the error is returned without read.refuse(" + id.Name + ")"
	}
	return ""
}

// unguardedReadCalls returns what is wrong with the references to the functions
// of calls in the method plan of the type recv: a call that is missing or given
// twice, a call whose error does not leave through read.refuse, any reference
// that is not such a call (an alias, a function passed as an argument, an
// assignment), and any reference in another function or method of any file of
// the package. Several methods of the package are named plan; the one whose
// receiver is recv is the one checked, and every other declaration is searched
// for references, whatever its name.
func unguardedReadCalls(ps parsedSource, plan, recv string, calls []string) []string {
	var fn *ast.FuncDecl
	for _, d := range ps.decls {
		if d.Name.Name == plan && receiverIs(d, recv) {
			fn = d
		}
	}
	if fn == nil {
		return []string{"the method " + plan + " of " + recv + " is not declared"}
	}
	var problems []string
	structured := map[*ast.CallExpr]string{}
	check := func(assign *ast.AssignStmt, ifStmt *ast.IfStmt) {
		if len(assign.Rhs) != 1 {
			return
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok || !slices.Contains(calls, calleeName(call)) {
			return
		}
		structured[call] = guardedCall(assign, ifStmt)
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BlockStmt:
			for k, stmt := range n.List {
				assign, ok := stmt.(*ast.AssignStmt)
				if !ok {
					continue
				}
				var next *ast.IfStmt
				if k+1 < len(n.List) {
					next, _ = n.List[k+1].(*ast.IfStmt)
					if next != nil && next.Init != nil {
						next = nil
					}
				}
				check(assign, next)
			}
		case *ast.IfStmt:
			if assign, ok := n.Init.(*ast.AssignStmt); ok {
				check(assign, n)
			}
		}
		return true
	})
	seen := map[string]int{}
	var stack []ast.Node
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		id, ok := n.(*ast.Ident)
		if !ok || !slices.Contains(calls, id.Name) {
			return true
		}
		seen[id.Name]++
		call := calledByNameAt(stack)
		switch problem, isStructured := structured[call]; {
		case call == nil:
			problems = append(problems, fmt.Sprintf("%s: %s is referenced without being called", ps.fset.Position(id.Pos()), id.Name))
		case !isStructured:
			problems = append(problems, fmt.Sprintf("%s: the call of %s is not assigned to a result list with its error", ps.fset.Position(call.Pos()), id.Name))
		case problem != "":
			problems = append(problems, fmt.Sprintf("%s: the call of %s: %s", ps.fset.Position(call.Pos()), id.Name, problem))
		}
		return true
	})
	for _, name := range calls {
		if seen[name] != 1 {
			problems = append(problems, fmt.Sprintf("%s refers to %s %d times, not once", plan, name, seen[name]))
		}
	}
	for _, other := range ps.decls {
		if other == fn || other.Body == nil {
			continue
		}
		ast.Inspect(other.Body, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && slices.Contains(calls, id.Name) {
				problems = append(problems, fmt.Sprintf("%s: %s refers to %s", ps.fset.Position(id.Pos()), other.Name.Name, id.Name))
			}
			return true
		})
	}
	return problems
}

// TestPlanReturnsReadErrorsOnlyThroughTheFilter asserts that every error of
// the three calls of Plan that read the command line (readFieldFlags,
// checkFieldFlags and readWords) leaves Plan only as the argument of
// read.refuse, in an if statement that returns nothing else, that each of the
// three names is referenced once in Plan of the cloud credential command, as
// that call, and that no other function or method of any file of the package
// refers to them. A refusal of those functions can then not skip the
// filter, whether through a call, an alias or a function value.
func TestPlanReturnsReadErrorsOnlyThroughTheFilter(t *testing.T) {
	ps := parseSources(t, packageSources(t))
	assert.Empty(t, unguardedReadCalls(ps, "Plan", planReceiver, planReadCalls))
}

// TestPlanGuardFindsEachBypass asserts the guard of Plan passes the wrapped
// shapes and fails an error returned as it is, an error wrapped by another
// name, a call whose error is dropped, a missing call, a repeated call, an
// alias, a function passed as an argument, and a reference from another
// function.
func TestPlanGuardFindsEachBypass(t *testing.T) {
	plan := func(first, second, third string) string {
		return "package p\nfunc (c *cmd) Plan() {\n" +
			"\tvar read readTexts\n" +
			"\tsets, files, err := readFieldFlags(in)\n" + first + "\n" +
			"\tif err := checkFieldFlags(driver, sets, files); err != nil {\n" + second + "\n\t}\n" +
			"\tvalues, warnings, err := c.readWords(driver, sets, files, read.record)\n" + third + "\n}\n"
	}
	const wrapped = "\treturn safety.Effect{}, nil, read.refuse(err)"
	ifWrapped := func(body string) string { return "\tif err != nil {\n" + body + "\n\t}" }
	good := plan(ifWrapped(wrapped), wrapped, ifWrapped(wrapped))
	calls := planReadCalls
	guard := func(src string) []string {
		return unguardedReadCalls(parseSource(t, []byte(src)), "Plan", "cmd", calls)
	}
	t.Run("every error wrapped", func(t *testing.T) {
		assert.Empty(t, guard(good))
	})
	const bare = "\treturn safety.Effect{}, nil, err"
	for name, src := range map[string]string{
		"readFieldFlags returned as it is":  plan(ifWrapped(bare), wrapped, ifWrapped(wrapped)),
		"checkFieldFlags returned as it is": plan(ifWrapped(wrapped), bare, ifWrapped(wrapped)),
		"readWords returned as it is":       plan(ifWrapped(wrapped), wrapped, ifWrapped(bare)),
		"another wrapper":                   plan(ifWrapped(wrapped), "\treturn safety.Effect{}, nil, scrub(err)", ifWrapped(wrapped)),
		"another receiver":                  plan(ifWrapped(wrapped), "\treturn safety.Effect{}, nil, other.refuse(err)", ifWrapped(wrapped)),
		"the error in two results":          plan(ifWrapped(wrapped), "\treturn err, nil, read.refuse(err)", ifWrapped(wrapped)),
		"a second statement":                plan(ifWrapped(wrapped), "\tlog(err)\n"+wrapped, ifWrapped(wrapped)),
		"no check of the error":             plan("", wrapped, ifWrapped(wrapped)),
		"a check against another error":     plan("\tif other != nil {\n"+wrapped+"\n\t}", wrapped, ifWrapped(wrapped)),
		"an alias of a method":              strings.Replace(good, "\tvar read readTexts\n", "\tvar read readTexts\n\tagain := c.readWords\n\t_, _, err2 := again(driver, sets, files, nil)\n", 1),
		"an alias of a function":            strings.Replace(good, "\tvar read readTexts\n", "\tvar read readTexts\n\tagain := readFieldFlags\n", 1),
		"a function passed as an argument":  strings.Replace(good, "\tvar read readTexts\n", "\tvar read readTexts\n\trun(readFieldFlags)\n", 1),
		"a method passed as an argument":    strings.Replace(good, "\tvar read readTexts\n", "\tvar read readTexts\n\trun(c.readWords)\n", 1),
		"a function stored in a variable":   strings.Replace(good, "\tvar read readTexts\n", "\tvar read readTexts\n\tvar again = checkFieldFlags\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			assert.NotEmpty(t, guard(src))
		})
	}
	t.Run("a dropped error", func(t *testing.T) {
		src := "package p\nfunc (c *cmd) Plan() {\n\treadFieldFlags(in)\n\tcheckFieldFlags(d, s, f)\n\tc.readWords(d, s, f, r)\n}\n"
		assert.NotEmpty(t, guard(src))
	})
	t.Run("a missing call", func(t *testing.T) {
		src := "package p\nfunc (c *cmd) Plan() {\n\t_, _, err := readFieldFlags(in)\n" + ifWrapped(wrapped) + "\n}\n"
		assert.NotEmpty(t, guard(src))
	})
	t.Run("a call from another function", func(t *testing.T) {
		assert.NotEmpty(t, guard(good+"func other() error { _, _, err := readFieldFlags(in); return err }\n"))
	})
	t.Run("a function value in another function", func(t *testing.T) {
		assert.NotEmpty(t, guard(good+"func other() { run(readFieldFlags) }\n"))
		assert.NotEmpty(t, guard(good+"func other(c *cmd) { again := c.readWords; _ = again }\n"))
	})
	t.Run("a function that is not declared", func(t *testing.T) {
		assert.NotEmpty(t, unguardedReadCalls(parseSource(t, []byte(good)), "Gone", "cmd", calls))
	})
	t.Run("files of a package with other Plan methods", func(t *testing.T) {
		guardFiles := func(files ...string) []string {
			var sources []sourceFile
			for k, src := range files {
				sources = append(sources, sourceFile{name: fmt.Sprintf("file%d.go", k), src: []byte(src)})
			}
			return unguardedReadCalls(parseSources(t, sources), "Plan", "cmd", calls)
		}
		const other = "package p\nfunc (o *other) Plan() { run() }\n"
		assert.Empty(t, guardFiles(good, other), "the Plan of the type is the one checked, after others")
		assert.Empty(t, guardFiles(other, good), "the Plan of the type is the one checked, before others")
		assert.NotEmpty(t, guardFiles(other), "no Plan of the type")
		assert.NotEmpty(t, guardFiles(good, "package p\nfunc (c *cmd) again(d, s, f any) { c.readWords(d, s, f, nil) }\n"), "a method of another file calls readWords")
		assert.NotEmpty(t, guardFiles(good, "package p\nfunc (o *other) Plan() { run(readFieldFlags) }\n"), "a Plan of another type refers to a guarded function")
		assert.NotEmpty(t, guardFiles(good, "package p\nfunc dup() {}\n", "package p\nfunc dup() { run(readFieldFlags) }\n"), "a name declared twice is searched too")
	})
}
