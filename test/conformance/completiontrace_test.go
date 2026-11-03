package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// traceTally counts what completionTraceFindings found in one file.
type traceTally struct {
	declarations int
	debugfs      int
	writes       int
	filtered     int
}

func (a traceTally) plus(b traceTally) traceTally {
	return traceTally{
		declarations: a.declarations + b.declarations,
		debugfs:      a.debugfs + b.debugfs,
		writes:       a.writes + b.writes,
		filtered:     a.filtered + b.filtered,
	}
}

const (
	traceSink     = "completionDebugOut"
	traceFunction = "completionDebugf"
)

// completionTraceFindings checks one parsed file of internal/cli against the
// rules of TestCompletionTraceWritesOnlyFilteredText and returns a finding
// for each broken rule, each starting with the rule's name (T1 to T6), and
// what it found of the trace writer.
//
//   - T1: completionDebugOut occurs only in its declaration and in
//     completionDebugf.
//   - T2: it occurs in completionDebugf once, as the first argument of a call.
//   - T3: format and args occur in completionDebugf only inside one call
//     fmt.Sprintf(format, args...), which is the only argument of a call
//     runtime.PrintableRemedies.
//   - T4: os.Stderr (under the name the file gives the package os, a dot
//     import included) occurs only as the value paired with the name
//     completionDebugOut in its declaration.
//   - T6: every argument of the write after the sink is a string literal,
//     the elapsed time since processStart, or the variable the filter call is
//     assigned to; that variable is declared once, assigned once from the
//     filter and afterwards only from a call of ExpandText on itself; the
//     function holds no function literal, go statement or defer statement.
//   - T3 also requires the parameters of completionDebugf to be named format
//     and args.
func completionTraceFindings(fset *token.FileSet, file *ast.File) (findings []string, found traceTally) {
	add := func(rule string, n ast.Node, text string) {
		findings = append(findings, rule+": "+fset.Position(n.Pos()).String()+": "+text)
	}
	osName := traceOSName(file)
	fieldNames := traceFieldNames(file)
	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range value.Names {
					if name.Name == traceSink {
						found.declarations++
					}
				}
				for i, v := range value.Values {
					pairedWithSink := len(value.Values) == len(value.Names) && value.Names[i].Name == traceSink
					ast.Inspect(v, func(n ast.Node) bool {
						switch {
						case traceIsIdent(n, traceSink):
							add("T1", n, traceSink+" is used outside "+traceFunction)
						case traceIsOSStderr(n, osName, fieldNames) && !pairedWithSink:
							add("T4", n, "os.Stderr is used outside the initializer of "+traceSink)
						}
						return true
					})
				}
			}
		case *ast.FuncDecl:
			inDebugf := decl.Name.Name == traceFunction && decl.Recv == nil
			if inDebugf {
				found.debugfs++
				if !slices.Equal(traceParameterNames(decl.Type), []string{"format", "args"}) {
					add("T3", decl.Type, traceFunction+" must name its parameters format and args")
				}
			}
			parts := []ast.Node{decl.Type}
			if decl.Body != nil {
				parts = append(parts, decl.Body)
			}
			for _, part := range parts {
				ast.Inspect(part, func(n ast.Node) bool {
					switch {
					case traceIsIdent(n, traceSink) && !inDebugf:
						add("T1", n, traceSink+" is used outside "+traceFunction)
					case traceIsOSStderr(n, osName, fieldNames):
						add("T4", n, "os.Stderr is used outside the initializer of "+traceSink)
					}
					return true
				})
			}
			if inDebugf && decl.Body != nil {
				writes, filtered := checkTraceFunction(decl.Body, add)
				found.writes += writes
				found.filtered += filtered
			}
		}
	}
	return findings, found
}

func traceIsIdent(n ast.Node, name string) bool {
	id, ok := n.(*ast.Ident)
	return ok && id.Name == name
}

// traceOSName is the name file gives the package os: "os", the alias of a
// renaming import, "." for a dot import, or empty when the file does not
// import it.
func traceOSName(file *ast.File) string {
	for _, imp := range file.Imports {
		if imp.Path.Value != `"os"` {
			continue
		}
		if imp.Name == nil {
			return "os"
		}
		return imp.Name.Name
	}
	return ""
}

// traceFieldNames is the identifiers of file that name a field or a key
// rather than a value: the selected name of a selector expression and the key
// of a key-value pair. A dot import of os makes a bare Stderr the writer, and
// these are not.
func traceFieldNames(file *ast.File) map[*ast.Ident]bool {
	names := map[*ast.Ident]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			names[n.Sel] = true
		case *ast.KeyValueExpr:
			if id, ok := n.Key.(*ast.Ident); ok {
				names[id] = true
			}
		}
		return true
	})
	return names
}

// traceIsOSStderr reports whether n is os.Stderr under the name osName, or
// the bare name Stderr when osName is a dot import.
func traceIsOSStderr(n ast.Node, osName string, fieldNames map[*ast.Ident]bool) bool {
	switch osName {
	case "", "_":
		return false
	case ".":
		id, ok := n.(*ast.Ident)
		return ok && id.Name == "Stderr" && !fieldNames[id]
	}
	return traceIsSelector(n, osName, "Stderr")
}

func traceParameterNames(fn *ast.FuncType) []string {
	var names []string
	if fn.Params == nil {
		return names
	}
	for _, field := range fn.Params.List {
		for _, name := range field.Names {
			names = append(names, name.Name)
		}
	}
	return names
}

func traceIsSelector(n ast.Node, pkg, name string) bool {
	sel, ok := n.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == pkg && sel.Sel.Name == name
}

// traceFilteredSprintf returns the call fmt.Sprintf(format, args...) when call is
// runtime.PrintableRemedies of exactly that call.
func traceFilteredSprintf(call *ast.CallExpr) (*ast.CallExpr, bool) {
	if !traceIsSelector(call.Fun, "runtime", "PrintableRemedies") || len(call.Args) != 1 {
		return nil, false
	}
	inner, ok := call.Args[0].(*ast.CallExpr)
	if !ok || !traceIsSelector(inner.Fun, "fmt", "Sprintf") || len(inner.Args) != 2 || !inner.Ellipsis.IsValid() {
		return nil, false
	}
	if !traceIsIdent(inner.Args[0], "format") || !traceIsIdent(inner.Args[1], "args") {
		return nil, false
	}
	return inner, true
}

// checkTraceFunction applies T2, T3 and T6 to the body of completionDebugf
// and returns the number of writes and of filtered Sprintf calls it found.
func checkTraceFunction(body *ast.BlockStmt, add func(rule string, n ast.Node, text string)) (writes, filtered int) {
	// Identifiers of the one allowed Sprintf call.
	allowed := map[*ast.Ident]bool{}
	var filterAssignedTo string
	var filterAssigns int
	sinkUses := 0
	var writeCalls []*ast.CallExpr
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if inner, ok := traceFilteredSprintf(n); ok {
				filtered++
				allowed[inner.Args[0].(*ast.Ident)] = true
				allowed[inner.Args[1].(*ast.Ident)] = true
			}
			if len(n.Args) > 0 && traceIsIdent(n.Args[0], traceSink) {
				writes++
				writeCalls = append(writeCalls, n)
			}
		case *ast.Ident:
			if n.Name == traceSink {
				sinkUses++
			}
		case *ast.FuncLit:
			add("T6", n, traceFunction+" holds a function literal")
		case *ast.GoStmt:
			add("T6", n, traceFunction+" holds a go statement")
		case *ast.DeferStmt:
			add("T6", n, traceFunction+" holds a defer statement")
		case *ast.AssignStmt:
			if len(n.Lhs) == 1 && len(n.Rhs) == 1 {
				if call, ok := n.Rhs[0].(*ast.CallExpr); ok {
					if _, ok := traceFilteredSprintf(call); ok {
						if id, ok := n.Lhs[0].(*ast.Ident); ok {
							filterAssignedTo = id.Name
							filterAssigns++
						}
					}
				}
			}
		}
		return true
	})
	if sinkUses != 1 || writes != 1 {
		add("T2", body, traceSink+" must occur once in "+traceFunction+", as the first argument of the one write")
	}
	if filtered != 1 {
		add("T3", body, traceFunction+" must pass fmt.Sprintf(format, args...) to runtime.PrintableRemedies once")
	}
	ast.Inspect(body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && (id.Name == "format" || id.Name == "args") && !allowed[id] {
			add("T3", id, id.Name+" is used outside the one filtered fmt.Sprintf")
		}
		return true
	})

	if filterAssigns != 1 {
		add("T6", body, "no single variable is assigned from the filtered text")
		return writes, filtered
	}
	for _, w := range writeCalls {
		for _, arg := range w.Args[1:] {
			if lit, ok := arg.(*ast.BasicLit); (ok && lit.Kind == token.STRING) || traceIsIdent(arg, filterAssignedTo) || traceIsElapsed(arg) {
				continue
			}
			add("T6", arg, "the write holds an argument that is not a string literal, the elapsed time or "+filterAssignedTo+", the filtered text")
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if !traceAssignsTo(n.Lhs, filterAssignedTo) {
				return true
			}
			if len(n.Lhs) == 1 && len(n.Rhs) == 1 {
				call, ok := n.Rhs[0].(*ast.CallExpr)
				if ok {
					if _, isFilter := traceFilteredSprintf(call); isFilter {
						return true
					}
					if n.Tok == token.ASSIGN && traceIsExpansionOf(call, filterAssignedTo) {
						return true
					}
				}
			}
			add("T6", n, filterAssignedTo+" is assigned from something other than the filter or its own expansion")
		case *ast.ValueSpec:
			if slices.ContainsFunc(n.Names, func(id *ast.Ident) bool { return id.Name == filterAssignedTo }) {
				add("T6", n, filterAssignedTo+" is declared a second time")
			}
		case *ast.IncDecStmt:
			if traceIsIdent(n.X, filterAssignedTo) {
				add("T6", n, filterAssignedTo+" is changed after the filter")
			}
		case *ast.RangeStmt:
			if (n.Key != nil && traceIsIdent(n.Key, filterAssignedTo)) || (n.Value != nil && traceIsIdent(n.Value, filterAssignedTo)) {
				add("T6", n, filterAssignedTo+" is assigned by a range after the filter")
			}
		case *ast.UnaryExpr:
			if n.Op == token.AND && traceIsIdent(n.X, filterAssignedTo) {
				add("T6", n, filterAssignedTo+" has its address taken")
			}
		}
		return true
	})
	return writes, filtered
}

func traceAssignsTo(lhs []ast.Expr, name string) bool {
	for _, e := range lhs {
		if traceIsIdent(e, name) {
			return true
		}
	}
	return false
}

// traceIsElapsed reports whether e is time.Since(processStart), or a method
// call on it whose arguments are literals or names of the package time, such
// as Round(time.Millisecond).
func traceIsElapsed(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	if traceIsSelector(call.Fun, "time", "Since") {
		return len(call.Args) == 1 && traceIsIdent(call.Args[0], "processStart")
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	for _, arg := range call.Args {
		if _, literal := arg.(*ast.BasicLit); literal {
			continue
		}
		if name, ok := arg.(*ast.SelectorExpr); !ok || !traceIsSelector(arg, "time", name.Sel.Name) {
			return false
		}
	}
	return traceIsElapsed(sel.X)
}

// traceIsExpansionOf reports whether call is <receiver>.ExpandText(name).
func traceIsExpansionOf(call *ast.CallExpr, name string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "ExpandText" && len(call.Args) == 1 && traceIsIdent(call.Args[0], name)
}

// traceFixtureHead is the production file's shape up to the function.
const traceFixtureHead = `package cli

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

var completionDebugOut io.Writer = os.Stderr

var processStart = time.Now()

`

// traceFixtureProduction is a hand copy of the shape completionDebugf is
// expected to have in internal/cli/completion_debug.go. Nothing compares it
// with that file; the scan of the tree checks the real function.
const traceFixtureProduction = traceFixtureHead + `func completionDebugf(format string, args ...any) {
	line := runtime.PrintableRemedies(fmt.Sprintf(format, args...))
	if clierr.HasRemedy(line) {
		line = completionTraceIO().ExpandText(line)
	}
	_, _ = fmt.Fprintf(completionDebugOut, "completion +%s %s\n",
		time.Since(processStart).Round(time.Millisecond), line)
}
`

// traceFixtureFunction is a file whose completionDebugf has the body given.
func traceFixtureFunction(body string) string {
	return traceFixtureHead + "func completionDebugf(format string, args ...any) {\n" + body + "}\n"
}

const (
	traceFixtureFilter = "\tline := runtime.PrintableRemedies(fmt.Sprintf(format, args...))\n"
	traceFixtureExpand = "\tif clierr.HasRemedy(line) {\n\t\tline = completionTraceIO().ExpandText(line)\n\t}\n"
	traceFixtureWrite  = "\t_, _ = fmt.Fprintf(completionDebugOut, \"completion +%s %s\\n\", time.Since(processStart), line)\n"
)

// traceRuleCounts is the number of findings per rule.
type traceRuleCounts map[string]int

func countTraceFindings(findings []string) traceRuleCounts {
	counts := traceRuleCounts{}
	for _, f := range findings {
		rule, _, _ := strings.Cut(f, ":")
		counts[rule]++
	}
	return counts
}

// TestCompletionTraceDetectorFixtures asserts the detector of
// TestCompletionTraceWritesOnlyFilteredText on source text: the production
// shape has no finding and each way of reaching the trace sink with text the
// filter did not see has the findings named here, per rule.
func TestCompletionTraceDetectorFixtures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		source string
		want   traceRuleCounts
		found  traceTally
	}{
		{
			name:   "production shape",
			source: traceFixtureProduction,
			want:   traceRuleCounts{},
			found:  traceTally{declarations: 1, debugfs: 1, writes: 1, filtered: 1},
		},
		{
			name:   "write without the filter",
			source: traceFixtureFunction("\tline := fmt.Sprintf(format, args...)\n" + traceFixtureExpand + traceFixtureWrite),
			want:   traceRuleCounts{"T3": 3, "T6": 1},
			found:  traceTally{declarations: 1, debugfs: 1, writes: 1},
		},
		{
			name:   "filter on the format alone",
			source: traceFixtureFunction("\tline := fmt.Sprintf(runtime.PrintableRemedies(format), args...)\n" + traceFixtureExpand + traceFixtureWrite),
			want:   traceRuleCounts{"T3": 3, "T6": 1},
			found:  traceTally{declarations: 1, debugfs: 1, writes: 1},
		},
		{
			name: "second Sprintf of the arguments written to the sink",
			source: traceFixtureFunction(traceFixtureFilter + traceFixtureExpand +
				"\t_, _ = fmt.Fprintf(completionDebugOut, \"%s\", fmt.Sprintf(\"%v\", args...))\n"),
			want:  traceRuleCounts{"T3": 1, "T6": 1},
			found: traceTally{declarations: 1, debugfs: 1, writes: 1, filtered: 1},
		},
		{
			name:   "another callee in place of the filter",
			source: traceFixtureFunction("\tline := strings.TrimSpace(fmt.Sprintf(format, args...))\n" + traceFixtureExpand + traceFixtureWrite),
			want:   traceRuleCounts{"T3": 3, "T6": 1},
			found:  traceTally{declarations: 1, debugfs: 1, writes: 1},
		},
		{
			name:   "second function that writes to the sink",
			source: traceFixtureProduction + "\nfunc other() {\n\t_, _ = fmt.Fprintln(completionDebugOut, \"x\")\n}\n",
			want:   traceRuleCounts{"T1": 1},
			found:  traceTally{declarations: 1, debugfs: 1, writes: 1, filtered: 1},
		},
		{
			name: "alias of the sink",
			source: traceFixtureFunction(traceFixtureFilter + traceFixtureExpand +
				"\tw := completionDebugOut\n\t_, _ = fmt.Fprintf(w, \"%s\", line)\n"),
			want:  traceRuleCounts{"T2": 1},
			found: traceTally{declarations: 1, debugfs: 1, filtered: 1},
		},
		{
			name: "second write to the sink",
			source: traceFixtureFunction(traceFixtureFilter + traceFixtureExpand + traceFixtureWrite +
				"\t_, _ = fmt.Fprintln(completionDebugOut, line)\n"),
			want:  traceRuleCounts{"T2": 1},
			found: traceTally{declarations: 1, debugfs: 1, writes: 2, filtered: 1},
		},
		{
			name:   "os.Stderr in another function of the same file",
			source: traceFixtureProduction + "\nfunc other() {\n\t_, _ = fmt.Fprintln(os.Stderr, \"x\")\n}\n",
			want:   traceRuleCounts{"T4": 1},
			found:  traceTally{declarations: 1, debugfs: 1, writes: 1, filtered: 1},
		},
		{
			name:   "os.Stderr in another file",
			source: "package cli\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\nfunc show() {\n\t_, _ = fmt.Fprintln(os.Stderr, \"x\")\n}\n",
			want:   traceRuleCounts{"T4": 1},
		},
		{
			name:   "text appended to the filtered text",
			source: traceFixtureFunction(traceFixtureFilter + traceFixtureExpand + "\tline = line + os.Getenv(\"HOME\")\n" + traceFixtureWrite),
			want:   traceRuleCounts{"T6": 1},
			found:  traceTally{declarations: 1, debugfs: 1, writes: 1, filtered: 1},
		},
		{
			name:   "second assignment from another value",
			source: traceFixtureFunction(traceFixtureFilter + traceFixtureExpand + "\tline = traceGlobal\n" + traceFixtureWrite),
			want:   traceRuleCounts{"T6": 1},
			found:  traceTally{declarations: 1, debugfs: 1, writes: 1, filtered: 1},
		},
		{
			name:   "write ends with another value",
			source: traceFixtureFunction(traceFixtureFilter + traceFixtureExpand + "\t_, _ = fmt.Fprintf(completionDebugOut, \"%s\", os.Getenv(\"HOME\"))\n"),
			want:   traceRuleCounts{"T6": 1},
			found:  traceTally{declarations: 1, debugfs: 1, writes: 1, filtered: 1},
		},
		{
			name: "middle argument from a package variable",
			source: traceFixtureFunction(traceFixtureFilter + traceFixtureExpand +
				"\t_, _ = fmt.Fprintf(completionDebugOut, \"completion +%s %s %s\\n\", time.Since(processStart), storedConfigDir, line)\n"),
			want:  traceRuleCounts{"T6": 1},
			found: traceTally{declarations: 1, debugfs: 1, writes: 1, filtered: 1},
		},
		{
			name: "middle argument from the environment",
			source: traceFixtureFunction(traceFixtureFilter + traceFixtureExpand +
				"\t_, _ = fmt.Fprintf(completionDebugOut, \"completion +%s %s %s\\n\", time.Since(processStart), os.Getenv(\"HOME\"), line)\n"),
			want:  traceRuleCounts{"T6": 1},
			found: traceTally{declarations: 1, debugfs: 1, writes: 1, filtered: 1},
		},
		{
			name:   "renamed parameters",
			source: traceFixtureHead + "func completionDebugf(f string, a ...any) {\n" + traceFixtureFilter + traceFixtureExpand + traceFixtureWrite + "}\n",
			want:   traceRuleCounts{"T3": 1},
			found:  traceTally{declarations: 1, debugfs: 1, writes: 1, filtered: 1},
		},
		{
			name: "renamed parameters and a Sprintf of them written to the sink",
			source: traceFixtureHead + "func completionDebugf(f string, a ...any) {\n" + traceFixtureFilter + traceFixtureExpand +
				"\t_, _ = fmt.Fprintf(completionDebugOut, \"%s %s\", fmt.Sprintf(f, a...), line)\n}\n",
			want:  traceRuleCounts{"T3": 1, "T6": 1},
			found: traceTally{declarations: 1, debugfs: 1, writes: 1, filtered: 1},
		},
		{
			name: "function literal run by a go statement",
			source: traceFixtureFunction(traceFixtureFilter + traceFixtureExpand +
				"\tgo func(line string) {\n\t\t_, _ = fmt.Fprintf(completionDebugOut, \"%s\", line)\n\t}(os.Getenv(\"X\"))\n"),
			want:  traceRuleCounts{"T6": 2},
			found: traceTally{declarations: 1, debugfs: 1, writes: 1, filtered: 1},
		},
		{
			name: "function literal run by a defer statement",
			source: traceFixtureFunction(traceFixtureFilter + traceFixtureExpand +
				"\tdefer func(line string) {\n\t\t_, _ = fmt.Fprintf(completionDebugOut, \"%s\", line)\n\t}(os.Getenv(\"X\"))\n"),
			want:  traceRuleCounts{"T6": 2},
			found: traceTally{declarations: 1, debugfs: 1, writes: 1, filtered: 1},
		},
		{
			name: "second declaration of the filtered variable in a block",
			source: traceFixtureFunction(traceFixtureFilter + traceFixtureExpand +
				"\t{\n\t\tvar line = os.Getenv(\"X\")\n\t\t_, _ = fmt.Fprintf(completionDebugOut, \"%s\", line)\n\t}\n"),
			want:  traceRuleCounts{"T6": 1},
			found: traceTally{declarations: 1, debugfs: 1, writes: 1, filtered: 1},
		},
		{
			name:   "os imported under another name",
			source: "package cli\n\nimport (\n\t\"fmt\"\n\tstdos \"os\"\n)\n\nfunc show() {\n\t_, _ = fmt.Fprintln(stdos.Stderr, \"x\")\n}\n",
			want:   traceRuleCounts{"T4": 1},
		},
		{
			name:   "os dot imported",
			source: "package cli\n\nimport (\n\t\"fmt\"\n\t. \"os\"\n)\n\nfunc show() {\n\t_, _ = fmt.Fprintln(Stderr, \"x\")\n}\n",
			want:   traceRuleCounts{"T4": 1},
		},
		{
			name:   "a field named Stderr with os dot imported",
			source: "package cli\n\nimport . \"os\"\n\ntype holder struct{ Stderr int }\n\nfunc read(h holder) int { return h.Stderr }\n",
			want:   traceRuleCounts{},
		},
		{
			name:   "os.Stderr paired with another name in the declaration of the sink",
			source: strings.Replace(traceFixtureProduction, "var completionDebugOut io.Writer = os.Stderr", "var completionDebugOut, other io.Writer = os.Stderr, os.Stderr", 1),
			want:   traceRuleCounts{"T4": 1},
			found:  traceTally{declarations: 1, debugfs: 1, writes: 1, filtered: 1},
		},
		{
			name:   "file with none of the names",
			source: "package cli\n\nfunc plain() int { return 1 }\n",
			want:   traceRuleCounts{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", tc.source, 0)
			require.NoError(t, err)
			findings, found := completionTraceFindings(fset, file)
			assert.Equal(t, tc.want, countTraceFindings(findings), "%v", findings)
			assert.Equal(t, tc.found, found)
		})
	}
}

// TestCompletionTraceWritesOnlyFilteredText asserts that every line of the
// completion trace reaches its sink through one write that holds only the
// text runtime.PrintableRemedies returned, the remedy commands of this
// process expanded: in the Go files that sit directly in internal/cli,
// completionDebugOut occurs only in its declaration and in the one write of
// completionDebugf (T1, T2); the formatted text of format and args reaches
// that write only through fmt.Sprintf(format, args...) given to the filter
// (T3); os.Stderr occurs only as the value of completionDebugOut in its
// declaration, under whatever name the file imports os by (T4); the scan
// found the writer, so it cannot pass on a tree that no longer holds it (T5);
// and every argument of the write is a string literal, the elapsed time or the
// variable that holds the filtered text, which is declared once, assigned from
// the filter and afterwards only from its own expansion, in a function with no
// function literal, go statement or defer statement (T6).
//
// Limits:
//   - it matches names, not types: a local that shadows fmt, runtime or os is
//     not seen, and fmt and runtime are matched by their own names, so an
//     import of either under another name is not seen (os is resolved per
//     file);
//   - it sees os.Stderr and completionDebugOut only; a write to the terminal
//     through another route (os.NewFile, syscall, the print builtin, log, the
//     root command's writers, runtime.IO.Err) is not seen; the stray stderr
//     scan and the output writes scans cover the others in part;
//   - it does not check what PrintableRemedies removes (the tests of
//     internal/output and internal/runtime do) and does not check the
//     elapsed time, which is the one argument of the write besides string
//     literals and the filtered text;
//   - it does not check what the expansion function itself adds, and it does
//     not filter the expansion's own text, which names a directory
//     RemedyPrefix has refused unless it prints as it is;
//   - the values written are covered by the behavior test of internal/cli
//     (TestCompletionTraceRemovesWhatATerminalCannotShow), not by this scan.
func TestCompletionTraceWritesOnlyFilteredText(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	var total traceTally
	scanned := 0
	for _, p := range files {
		if path.Dir(p) != "internal/cli" || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			continue
		}
		scanned++
		src, err := os.ReadFile(p)
		require.NoError(t, err)
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, p, src, 0)
		require.NoError(t, err)
		findings, found := completionTraceFindings(fset, file)
		for _, f := range findings {
			t.Errorf("%s", f)
		}
		total = total.plus(found)
	}
	assert.Greater(t, scanned, 20, "the scan read too few files of internal/cli")
	assert.Equal(t, traceTally{declarations: 1, debugfs: 1, writes: 1, filtered: 1}, total,
		"T5: the scan must find the declaration of %s, %s, its one write and its one filtered Sprintf", traceSink, traceFunction)
}

// completionTraceURLFindings checks one parsed file of internal/cli: for every
// call of completionDebugf, an argument after the format that is a bare
// identifier or a selector whose last name ends in "url", in any case, is a
// finding, since a server URL passed as it is reaches the trace with its path
// and query. A call such as config.DisplayServerURL(x) is not a finding. It
// returns the findings and the number of completionDebugf calls it saw.
func completionTraceURLFindings(fset *token.FileSet, file *ast.File) (findings []string, calls int) {
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.Ident)
		if !ok || fn.Name != traceFunction {
			return true
		}
		calls++
		for _, arg := range call.Args[min(1, len(call.Args)):] {
			var name string
			switch arg := arg.(type) {
			case *ast.Ident:
				name = arg.Name
			case *ast.SelectorExpr:
				name = arg.Sel.Name
			}
			if strings.HasSuffix(strings.ToLower(name), "url") {
				findings = append(findings, fset.Position(arg.Pos()).String()+": "+name+" is passed to "+traceFunction+" as it is")
			}
		}
		return true
	})
	return findings, calls
}

// TestCompletionTraceURLDetectorFixtures asserts the detector of
// TestCompletionTraceNamesNoRawServerURL on source text: a bare name and a
// selector that end in "URL" are findings, and a call, a name that does not
// end in it, a format with no argument and another function are not.
func TestCompletionTraceURLDetectorFixtures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, call string
		findings   int
		calls      int
	}{
		{"a variable named effectiveURL", `completionDebugf("no stored server has URL %s", effectiveURL)`, 1, 1},
		{"a field named ServerURL", `completionDebugf("url=%s", conn.ServerURL)`, 1, 1},
		{"a lower case name", `completionDebugf("url=%s", serverurl)`, 1, 1},
		{"a call of the display function", `completionDebugf("url=%s", config.DisplayServerURL(effectiveURL))`, 0, 1},
		{"a name that does not end in URL", `completionDebugf("connection: url=%s", canonical)`, 0, 1},
		{"a format with no argument", `completionDebugf("stop: no server URL")`, 0, 1},
		{"another function", `debugf("url=%s", rawURL)`, 0, 0},
		{"a call with the URL in the format position", `completionDebugf(rawURL)`, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := "package cli\n\nfunc f() {\n\t" + tc.call + "\n}\n"
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", src, 0)
			require.NoError(t, err)
			findings, calls := completionTraceURLFindings(fset, file)
			assert.Len(t, findings, tc.findings, "%v", findings)
			assert.Equal(t, tc.calls, calls)
		})
	}
}

// TestCompletionTraceNamesNoRawServerURL asserts that no call of
// completionDebugf in the Go files that sit directly in internal/cli passes a
// server URL variable or field as it is: the trace prints a server URL as
// scheme and host only (config.DisplayServerURL or the canonical form), and a
// raw URL holds the path and query its user or file wrote. The scan read more
// than 20 files and more than 30 calls, so it cannot pass on a tree that no
// longer holds them.
//
// Limits:
//   - it matches names, not types: a URL passed under a name that does not
//     end in "URL" (server, raw), or inside a larger expression, is not seen;
//     the last name of the argument is matched, in any case, so serverURLs,
//     urlText and a method call such as u.String() are not seen;
//   - a variable whose name ends in "URL" and that holds only scheme and host
//     (canonicalURL) is a finding too, and the scan has no allowlist: such a
//     variable gets another name;
//   - it sees only direct calls of completionDebugf in the files that sit
//     directly in internal/cli: a wrapper function that calls it, or a file
//     in another directory, is not seen;
//   - it does not see an error text that holds a URL, such as the transport
//     error of a request;
//   - the values written are covered by the behavior test of internal/cli
//     (TestCompletionTraceShowsServerURLsAsSchemeAndHost), not by this scan.
func TestCompletionTraceNamesNoRawServerURL(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	scanned, total := 0, 0
	for _, p := range files {
		if path.Dir(p) != "internal/cli" || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			continue
		}
		scanned++
		src, err := os.ReadFile(p)
		require.NoError(t, err)
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, p, src, 0)
		require.NoError(t, err)
		findings, calls := completionTraceURLFindings(fset, file)
		for _, f := range findings {
			t.Errorf("%s", f)
		}
		total += calls
	}
	assert.Greater(t, scanned, 20, "the scan read too few files of internal/cli")
	assert.Greater(t, total, 30, "the scan found too few calls of %s", traceFunction)
}
