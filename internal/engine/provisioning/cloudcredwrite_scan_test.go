package provisioning

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readValueFunctions are the functions of cloudcredwrite.go that hold a value
// the user typed or piped before the command's filter can hide it. A new
// function that holds such a value joins the list.
var readValueFunctions = []string{"readWords", "checkFieldFlags", "readFieldFlags", "splitFieldWords"}

// readValueNames are the identifiers those functions give a value they read:
// the bytes as read, the text after the byte order marks, the cleaned value
// and the value of a word (also the field of a word).
var readValueNames = []string{"raw", "text", "cleaned", "value"}

// refusalSinks are the calls whose arguments reach a message: the error and
// text constructors, the formatting and quoting functions, and the refusal
// helpers of the package that quote their arguments.
var refusalSinks = map[string][]string{
	"clierr":  {"New", "Text"},
	"fmt":     {"Sprintf", "Errorf"},
	"strconv": {"Quote", "QuoteToASCII", "QuoteToGraphic"},
	"":        {"quoteField", "secretInSetError", "unknownFieldError", "valueProblemError"},
}

// refusalSinkName returns the name of the sink a call is, or "".
func refusalSinkName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		if pkg, ok := fun.X.(*ast.Ident); ok && slices.Contains(refusalSinks[pkg.Name], fun.Sel.Name) {
			return pkg.Name + "." + fun.Sel.Name
		}
	case *ast.Ident:
		if slices.Contains(refusalSinks[""], fun.Name) {
			return fun.Name
		}
	}
	return ""
}

// readValuesInRefusals parses src and returns, for each function of funcs, the
// positions where an argument of a refusal sink, or a string joined into one
// with +, holds an identifier named in readValueNames (a selector such as
// w.value included), and the names of funcs the source does not declare.
func readValuesInRefusals(t *testing.T, src []byte, funcs []string) (found, missing []string) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "cloudcredwrite.go", src, 0)
	require.NoError(t, err)
	declared := map[string]*ast.FuncDecl{}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			declared[fn.Name.Name] = fn
		}
	}
	for _, name := range funcs {
		fn, ok := declared[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sink := refusalSinkName(call)
			if sink == "" {
				return true
			}
			for _, arg := range call.Args {
				ast.Inspect(arg, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok && slices.Contains(readValueNames, id.Name) {
						found = append(found, fset.Position(id.Pos()).String()+": "+name+" passes "+id.Name+" to "+sink)
					}
					return true
				})
			}
			return true
		})
	}
	return found, missing
}

// TestReadWordsRefusalsNameNoReadValue asserts that no refusal built by the
// functions that hold a value the user typed or piped has that value among its
// arguments: not the bytes read, the text, the cleaned value or the value of a
// word, alone or joined into a string. The filter that hides the read values in
// a refusal covers the forms it knows (the value, its lines, JSON-escaped,
// base64 and Go-quoted); this scan covers the forms no filter can foresee. A
// value copied into a variable of another name first is not seen: the filter
// covers that case. A function named in readValueFunctions that no longer
// exists fails the scan, so the list cannot go stale.
func TestReadWordsRefusalsNameNoReadValue(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("cloudcredwrite.go"))
	require.NoError(t, err)
	found, missing := readValuesInRefusals(t, src, readValueFunctions)
	assert.Empty(t, missing, "functions the scan names that cloudcredwrite.go does not declare")
	assert.Empty(t, found, "a refusal that names a value read from the user")
}

// TestReadWordsScanSeesAReadValue asserts the scan finds a refusal that quotes
// a read value in each way it knows, and passes one that names the flag and
// the position.
func TestReadWordsScanSeesAReadValue(t *testing.T) {
	const header = "package p\nfunc f(w word, raw []byte, text, cleaned, path string) error {\n"
	for name, body := range map[string]string{
		"a quoted text":       `return clierr.New("c", fmt.Sprintf("the value %q holds a hidden character", text))`,
		"a joined string":     `return clierr.New("c", "the value "+string(raw)+" holds a tab")`,
		"a word value":        `return clierr.New("c", "the value "+w.value+" is refused")`,
		"a helper argument":   `return secretInSetError(w.value, "name")`,
		"a quote call":        `q := strconv.QuoteToASCII(cleaned); _ = q`,
		"a nested conversion": `return clierr.New("c", strings.TrimSpace(text))`,
	} {
		t.Run(name, func(t *testing.T) {
			found, missing := readValuesInRefusals(t, []byte(header+body+"\n}\n"), []string{"f"})
			assert.Empty(t, missing)
			assert.NotEmpty(t, found)
		})
	}
	t.Run("a refusal that names the flag and the position", func(t *testing.T) {
		body := `return clierr.New("c", fmt.Sprintf("--%s argument #%d is refused at %s", w.flag, w.position, path))`
		found, _ := readValuesInRefusals(t, []byte(header+body+"\n}\n"), []string{"f"})
		assert.Empty(t, found)
	})
	t.Run("a function that is not declared", func(t *testing.T) {
		_, missing := readValuesInRefusals(t, []byte(header+"return nil\n}\n"), []string{"f", "gone"})
		assert.Equal(t, []string{"gone"}, missing)
	})
}
