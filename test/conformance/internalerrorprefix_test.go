package conformance

import (
	"go/ast"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var internalErrorPrefixFiles = []string{
	"internal/runtime/pipeline.go",
	"internal/runtime/project.go",
}

// internalErrorAllowance is the number of GENERIC literals of a file that do
// not start with the prefix because they are not defects of the CLI, and why.
type internalErrorAllowance struct {
	path   string
	count  int
	reason string
}

// internalErrorPrefixAllowlist is empty: every GENERIC message of the two
// files is a defect of the CLI. An entry whose count differs from the scan's
// count fails, so a stale entry cannot stay.
var internalErrorPrefixAllowlist = []internalErrorAllowance{}

// leadingMessageLiteral returns the string literal that starts the message
// expression e, when e is one of the followed forms.
func leadingMessageLiteral(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return leadingMessageLiteral(x.X)
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return leadingMessageLiteral(x.X)
		}
	case *ast.CallExpr:
		if isPackageCall(x, "fmt", "Sprintf") && len(x.Args) > 0 {
			return leadingMessageLiteral(x.Args[0])
		}
	}
	return "", false
}

// isPackageCall reports whether call is a call of pkg.name.
func isPackageCall(call *ast.CallExpr, pkg, name string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

// TestGenericErrorMessagesOfTheRuntimeStartWithInternalError asserts that
// every followed GENERIC message in internal/runtime/pipeline.go and
// project.go starts with "internal error: ", and that the scan reaches each
// file.
//
// A message for a defect in the CLI itself starts with "internal error: ",
// so a bug report carries a label that finds the line. The scan holds that
// rule for the messages the runtime pipeline and the project stage build
// with the GENERIC code, now and later: the text of each such call must start
// with the prefix.
//
// Forms the scan follows: a call of clierr.New whose code is
// clierr.CodeGeneric and whose message is a string literal, the first
// argument of a fmt.Sprintf call, or a "+" concatenation that starts with
// either one.
//
// Forms it does not follow: a message that starts with a variable or a
// constant, or is built by another function and passed in
// (committedContextError takes the text its callers build), and a GENERIC
// error made in another way than clierr.New.
//
// Limit: the scan covers internal/runtime/pipeline.go and project.go only. A
// GENERIC message in another file of internal/runtime is not seen.
//
// The two table tests in internal/runtime
// (TestInternalErrorMessagesStartWithThePrefix and
// TestProjectInternalErrorMessagesStartWithThePrefix) assert the whole
// message of each of their rows at run time.
func TestGenericErrorMessagesOfTheRuntimeStartWithInternalError(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)

	followed := map[string]int{}
	missing := map[string]int{}
	for i, f := range tree.files {
		path := tree.paths[i]
		if !slices.Contains(internalErrorPrefixFiles, path) {
			continue
		}
		followed[path] += 0
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isPackageCall(call, "clierr", "New") || len(call.Args) < 2 {
				return true
			}
			code, ok := call.Args[0].(*ast.SelectorExpr)
			if !ok || code.Sel.Name != "CodeGeneric" {
				return true
			}
			text, ok := leadingMessageLiteral(call.Args[1])
			if !ok {
				return true
			}
			followed[path]++
			if !strings.HasPrefix(text, internalErrorPrefix) {
				missing[path]++
				allowed := slices.ContainsFunc(internalErrorPrefixAllowlist, func(a internalErrorAllowance) bool { return a.path == path })
				if !allowed {
					pos := tree.fset.Position(call.Pos())
					t.Errorf("%s:%d: the GENERIC message %q does not start with %q", path, pos.Line, text, internalErrorPrefix)
				}
			}
			return true
		})
	}

	for _, path := range internalErrorPrefixFiles {
		count, scanned := followed[path]
		require.True(t, scanned, "%s was not scanned", path)
		assert.Positive(t, count, "%s: the scan followed no GENERIC message", path)
	}
	for _, a := range internalErrorPrefixAllowlist {
		assert.Equal(t, a.count, missing[a.path], "%s: the allowlist says %d (%s)", a.path, a.count, a.reason)
	}
}
