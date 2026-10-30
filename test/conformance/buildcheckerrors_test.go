package conformance

import (
	"go/ast"
	"path"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildCheckAllowlistEntry is a call of fmt.Errorf or errors.New in a file of
// internal/cli outside the three build-check files: the file, the literal
// text of its first argument and the reason the call is not a check that runs
// while the command tree is built.
type buildCheckAllowlistEntry struct {
	file    string
	literal string
	reason  string
}

// buildCheckAllowlist lists the calls of fmt.Errorf and errors.New in the
// non-test files of internal/cli other than root.go, livesource.go and
// completiontarget.go. A new call elsewhere fails the scan until it moves to
// one of those three files or gets an entry here with a reason.
var buildCheckAllowlist = []buildCheckAllowlistEntry{
	{"internal/cli/filesource.go", "no stdin available", "raised while a command reads standard input, not while the command tree is built"},
	{"internal/cli/projectcompletion.go", "completion: no path for steve request %+v", "a completion error that its callers drop, never shown"},
}

const internalErrorPrefix = "internal error: "

// errorCallLiteral reports whether call is fmt.Errorf or errors.New, and the
// text of its first argument when that is a string literal (ok is false when
// the first argument is anything else).
func errorCallLiteral(call *ast.CallExpr) (isErrorCall bool, literal string, ok bool) {
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel {
		return false, "", false
	}
	pkg, isIdent := sel.X.(*ast.Ident)
	if !isIdent {
		return false, "", false
	}
	isFmtErrorf := pkg.Name == "fmt" && sel.Sel.Name == "Errorf"
	isErrorsNew := pkg.Name == "errors" && sel.Sel.Name == "New"
	if !isFmtErrorf && !isErrorsNew {
		return false, "", false
	}
	if len(call.Args) == 0 {
		return true, "", false
	}
	lit, isLit := call.Args[0].(*ast.BasicLit)
	if !isLit {
		return true, "", false
	}
	text, err := strconv.Unquote(lit.Value)
	if err != nil {
		return true, "", false
	}
	return true, text, true
}

// TestBuildCheckErrorsLeadWithInternalError asserts that every check that
// runs while the command tree is built reports a defect of the CLI, and says
// so: the error text of every call of fmt.Errorf or errors.New in root.go
// starts with "internal error: ". The live completion setup checks in
// livesource.go and completiontarget.go get that lead from the wrap in
// root.go, so their own text does not start with it. The scan reads literals
// only: that the wraps in root.go apply the lead to those checks is asserted
// by TestValidateWiringLeadsWithInternalError in internal/cli. Each of the
// three files holds at least one such call, so the scan cannot pass without
// seeing one.
// Every call in any other non-test file directly in internal/cli is in
// buildCheckAllowlist with its reason, and an entry that matches no call
// fails.
//
// The scan does not see an error returned unchanged ("return err") from
// another function, or a check written outside internal/cli. The raw returns
// in root.go pass on an error that the lookup, compose or validate functions
// already built with the lead.
func TestBuildCheckErrorsLeadWithInternalError(t *testing.T) {
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	require.NotEmpty(t, tree.files, "the walk saw no source files")

	const (
		rootFile       = "internal/cli/root.go"
		liveSourceFile = "internal/cli/livesource.go"
		targetFile     = "internal/cli/completiontarget.go"
	)
	seen := map[string]int{}
	matched := make([]bool, len(buildCheckAllowlist))
	for i, f := range tree.files {
		file := tree.paths[i]
		if path.Dir(file) != "internal/cli" {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall {
				return true
			}
			isErrorCall, literal, isLiteral := errorCallLiteral(call)
			if !isErrorCall {
				return true
			}
			at := file + ":" + strconv.Itoa(tree.fset.Position(call.Pos()).Line)
			switch file {
			case rootFile:
				seen[file]++
				if !isLiteral {
					assert.Fail(t, at+": the first argument is not a string literal, so its text cannot be checked")
				} else {
					assert.True(t, strings.HasPrefix(literal, internalErrorPrefix),
						"%s: a check that runs while the command tree is built starts with %q: %q", at, internalErrorPrefix, literal)
				}
			case liveSourceFile, targetFile:
				seen[file]++
				if isLiteral {
					assert.False(t, strings.HasPrefix(literal, "internal error"),
						"%s: the lead comes from the wrap in root.go; drop it from %q", at, literal)
				}
			default:
				found := false
				for j, e := range buildCheckAllowlist {
					if e.file == file && isLiteral && e.literal == literal {
						matched[j] = true
						found = true
					}
				}
				assert.True(t, found, "%s: %q is not a build-time check in the three files and has no entry in buildCheckAllowlist", at, literal)
			}
			return true
		})
	}
	for _, file := range []string{rootFile, liveSourceFile, targetFile} {
		assert.Positive(t, seen[file], "%s holds no fmt.Errorf or errors.New call: the scan saw nothing", file)
	}
	for j, e := range buildCheckAllowlist {
		assert.True(t, matched[j], "stale allowlist entry: %s has no call with the text %q (%s)", e.file, e.literal, e.reason)
	}
}
