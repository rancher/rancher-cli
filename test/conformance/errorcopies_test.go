package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errorFields are the exported fields of clierr.Error.
var errorFields = []string{"Code", "Message", "Hint", "Candidates", "CandidatesTotal", "Retryable", "Details", "OutcomeUnknown"}

const (
	// errorLiteral is a composite literal of clierr.Error.
	errorLiteral = "clierr.Error literal"
	// fieldsInLiteral is a composite literal that reads two or more fields
	// of clierr.Error from one variable.
	fieldsInLiteral = "fields copied into a literal"
	// fieldsAssigned is an assignment whose values read two or more fields
	// of clierr.Error from one variable.
	fieldsAssigned = "fields assigned"
	// rebuiltError is a clierr.New or clierr.Retryable whose code is the
	// Code field of another value, read through a selector chain or not.
	rebuiltError = "error rebuilt from another's code"
	// fieldsInCallChain is a call chain started by clierr.New,
	// clierr.Retryable or clierr.Wrap, with no rebuilt code, whose arguments
	// read two or more fields of clierr.Error from one variable.
	fieldsInCallChain = "fields copied through a call chain"
)

// errorCopies are the places outside internal/clierr that build a
// clierr.Error, or copy two or more of its fields, field by field, each with
// whether the copy keeps OutcomeUnknown and why that is right.
var errorCopies = []struct {
	file, function, kind, reason string
}{
	{"internal/engine/userworkflow.go", "CommittedPartialSuccess", rebuiltError, "keeps OutcomeUnknown: it copies the field after it builds the error"},
	{"internal/engine/valuefilter.go", "FilterError", rebuiltError, "drops OutcomeUnknown; its callers pass errors of the action and Norman clients and of reads, never a write of the cluster client"},
	{"internal/engine/valuefilter.go", "RefusedError", rebuiltError, "drops OutcomeUnknown; it rebuilds a refusal of the action and Norman clients, which is never an unknown result"},
	{"internal/engine/resources/deletewait.go", "deleteTarget.refusedReadError", rebuiltError, "drops OutcomeUnknown; it rebuilds the error of a read after the delete, never a write"},
	{"internal/engine/resources/deleteproject.go", "readFailure", rebuiltError, "drops OutcomeUnknown; it rebuilds the error of a namespace read, never a write"},
	{"internal/engine/resources/deleteproject.go", "deleteProjectCmd.planDeleteNamespaces", rebuiltError, "drops OutcomeUnknown; it rebuilds the error of a dry-run delete, which changes nothing"},
	{"internal/engine/resources/deleteproject.go", "deleteProjectCmd.execDeleteNamespaces", fieldsAssigned, "drops OutcomeUnknown: it copies the code and message of a namespace delete into that namespace's outcome, and the partial-failure error does not carry the field yet"},
	{"internal/engine/resources/deleteproject.go", "namespaceFailure", rebuiltError, "drops OutcomeUnknown: the partial-failure error of delete project does not carry it yet, and REFERENCE.md says so"},
	{"internal/engine/resources/deleteproject.go", "deleteProjectCmd.projectFailed", fieldsAssigned, "keeps OutcomeUnknown on the error it returns, the project delete's own; the project's outcome copies the code and message only"},
	{"internal/engine/resources/deleteproject.go", "deleteProjectCmd.projectChanged", fieldsAssigned, "drops OutcomeUnknown; it follows a 409 of the project delete, a response that says the server did not apply it"},
	{"internal/engine/rbac/revoke.go", "partialFailure", fieldsAssigned, "drops OutcomeUnknown: the partial-failure error of revoke does not carry it yet, and REFERENCE.md says so"},
	{"internal/engine/access/candidateidentity.go", "resolveCandidate", rebuiltError, "drops OutcomeUnknown; it rebuilds the error of a read of the candidate's cached groups, never a write"},
	{"internal/engine/access/whocan.go", "whoCanCmd.Execute", rebuiltError, "drops OutcomeUnknown; it rebuilds the issue of a candidate's identity, which comes from reads, never a write"},
	{"internal/commands/skill/skill.go", "skillStatusCmd.Execute", fieldsInCallChain, "drops OutcomeUnknown; it rebuilds an error of the local skill status check, which sends no request"},
	{"internal/engine/access/types.go", "issueFrom", fieldsInLiteral, "drops OutcomeUnknown: access.Issue has no such field; it copies errors of reads and reviews, never a write"},
	{"internal/engine/access/chain.go", "chainWalker.requestIssue", fieldsInLiteral, "drops OutcomeUnknown: the chain issue has no such field; it copies errors of reads, never a write"},
	{"internal/engine/access/chain.go", "chainWalker.aggregationParents", fieldsInLiteral, "drops OutcomeUnknown: the chain issue has no such field; it copies errors of reads, never a write"},
	{"internal/engine/access/chain.go", "chainWalker.prtbFor", fieldsInLiteral, "drops OutcomeUnknown: the chain issue has no such field; it copies errors of reads, never a write"},
	{"internal/engine/access/explain.go", "groupsUnavailableChainIssue", fieldsInLiteral, "drops OutcomeUnknown: the chain issue has no such field; it copies the error of a read of the user's groups, never a write"},
	{"internal/token/noterminal.go", "tokenCmd.unsupportedConfigRemedy", rebuiltError, "drops OutcomeUnknown; it rebuilds an error of the exec plugin's sign-in, which sends no write of the cluster client"},
	{"internal/token/failure.go", "primaryOf", fieldsInLiteral, "drops OutcomeUnknown: clierr.PrimaryFailure has no such field; the login errors it projects come from the token POST, which sends no write of the cluster client"},
	{"internal/token/failure.go", "compose", rebuiltError, "drops OutcomeUnknown; the login errors it composes come from the token POST, which sends no write of the cluster client"},
	{"internal/token/signin.go", "createRefusal", rebuiltError, "drops OutcomeUnknown; it follows a 400 or 403 of the token create, responses that say the server did not apply it"},
	{"internal/token/signin.go", "unscopedCreateRefusal", rebuiltError, "drops OutcomeUnknown; it follows a 400 or 403 of the token create, responses that say the server did not apply it"},
	{"internal/token/owner.go", "userCheck.checkSignInOwner", fieldsAssigned, "drops OutcomeUnknown; it copies the error of the SelfSubjectReview the sign-in check posts, which stores nothing"},
}

// errorCopyHit is one place a file builds a clierr.Error, or copies its
// fields, field by field.
type errorCopyHit struct {
	function, kind, at string
}

// errorRoot is the identifier a selector chain starts from ("cached" for
// cached.Unavailable.Code), the called function followed by "()" when the
// chain starts from a call result ("clierr.From()" for clierr.From(err).Code),
// and "" for any other expression.
func errorRoot(e ast.Expr) string {
	for {
		switch x := e.(type) {
		case *ast.Ident:
			return x.Name
		case *ast.SelectorExpr:
			e = x.X
		case *ast.CallExpr:
			if errorRoot(x.Fun) == "" {
				return ""
			}
			return types.ExprString(x.Fun) + "()"
		default:
			return ""
		}
	}
}

// errorFieldRead is the root of v when v reads a field of errorFields
// through a selector chain from an identifier other than clierr, and "".
func errorFieldRead(v ast.Expr) string {
	if kv, ok := v.(*ast.KeyValueExpr); ok {
		v = kv.Value
	}
	sel, ok := v.(*ast.SelectorExpr)
	if !ok || !slices.Contains(errorFields, sel.Sel.Name) {
		return ""
	}
	if root := errorRoot(sel.X); root != "clierr" {
		return root
	}
	return ""
}

// twoErrorFieldsFromOne reports whether values read two or more fields of errorFields
// from one root.
func twoErrorFieldsFromOne(values []ast.Expr) bool {
	reads := map[string]int{}
	for _, v := range values {
		if root := errorFieldRead(v); root != "" {
			reads[root]++
			if reads[root] >= 2 {
				return true
			}
		}
	}
	return false
}

// clierrBuilder reports whether call is clierr.New, clierr.Retryable or
// clierr.Wrap.
func clierrBuilder(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "New" && sel.Sel.Name != "Retryable" && sel.Sel.Name != "Wrap") {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "clierr"
}

// errorBuilderChain is the call chain that call ends: the calls from the
// outermost (call) to a clierr.New, clierr.Retryable or clierr.Wrap at its
// start, each a method call on the one before (clierr.New(...).WithHint(...)).
// Nil when the chain does not start with one of the three.
func errorBuilderChain(call *ast.CallExpr) []*ast.CallExpr {
	var chain []*ast.CallExpr
	for {
		chain = append(chain, call)
		if clierrBuilder(call) {
			return chain
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return nil
		}
		inner, ok := sel.X.(*ast.CallExpr)
		if !ok {
			return nil
		}
		call = inner
	}
}

// findErrorCopies parses the Go source of path (read from the file when src
// is nil) and, when the file imports internal/clierr, returns every
// composite literal of clierr.Error, every composite literal or assignment
// that reads two or more fields of errorFields from one root, every
// clierr.New or clierr.Retryable whose code is the Code field of another
// value (through a selector chain or a call result too), and every other call
// chain started by clierr.New, clierr.Retryable or clierr.Wrap whose arguments
// read two or more fields of errorFields from one root, each with the function
// declaration it is in ("" at the package level).
func findErrorCopies(path string, src any) ([]errorCopyHit, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(file.Imports, func(spec *ast.ImportSpec) bool {
		p, _ := strconv.Unquote(spec.Path.Value)
		return p == "github.com/rancher/rancher-cli/internal/clierr"
	}) {
		return nil, nil
	}
	var hits []errorCopyHit
	for _, decl := range file.Decls {
		function := ""
		if fd, ok := decl.(*ast.FuncDecl); ok {
			function = funcDeclName(fd)
		}
		inChain := map[*ast.CallExpr]bool{}
		ast.Inspect(decl, func(n ast.Node) bool {
			add := func(kind string) {
				hits = append(hits, errorCopyHit{function, kind, fset.Position(n.Pos()).String()})
			}
			switch n := n.(type) {
			case *ast.CompositeLit:
				if sel, ok := n.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Error" {
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "clierr" {
						add(errorLiteral)
						return true
					}
				}
				if twoErrorFieldsFromOne(n.Elts) {
					add(fieldsInLiteral)
				}
			case *ast.AssignStmt:
				if twoErrorFieldsFromOne(n.Rhs) {
					add(fieldsAssigned)
				}
			case *ast.CallExpr:
				if inChain[n] {
					return true
				}
				chain := errorBuilderChain(n)
				if chain == nil {
					return true
				}
				var args []ast.Expr
				for _, c := range chain {
					inChain[c] = true
					args = append(args, c.Args...)
				}
				start := chain[len(chain)-1]
				if sel := start.Fun.(*ast.SelectorExpr); sel.Sel.Name != "Wrap" && len(start.Args) > 0 {
					if code, ok := start.Args[0].(*ast.SelectorExpr); ok && code.Sel.Name == "Code" {
						if root := errorRoot(code.X); root != "" && root != "clierr" {
							add(rebuiltError)
							return true
						}
					}
				}
				if twoErrorFieldsFromOne(args) {
					add(fieldsInCallChain)
				}
			}
			return true
		})
	}
	return hits, nil
}

// TestEveryCopyOfAnErrorOutsideClierrIsListed asserts that a non-test Go
// file outside internal/clierr, internal/testrancher and testdata builds a
// clierr.Error, or copies two or more of its fields, field by field only in
// a place errorCopies lists with whether the copy keeps OutcomeUnknown, and
// that every listed place still does. A copy is a literal, an assignment or
// a call chain started by clierr.New, clierr.Retryable or clierr.Wrap; a
// field may be read through a selector chain (c.Issue.Code) or off a call
// result (clierr.From(err).Code). The scan reads field names, not types. A
// copy field by field drops a field the copy does not name, OutcomeUnknown
// among them; the methods of clierr.Error copy the whole struct.
func TestEveryCopyOfAnErrorOutsideClierrIsListed(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	var offenders []string
	seen := map[string]bool{}
	for _, path := range files {
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || inTestdata(path) ||
			strings.HasPrefix(path, "internal/testrancher/") || strings.HasPrefix(path, "internal/clierr/") {
			continue
		}
		hits, perr := findErrorCopies(path, nil)
		require.NoError(t, perr, path)
		for _, hit := range hits {
			key := path + " " + hit.function + " " + hit.kind
			seen[key] = true
			if !slices.ContainsFunc(errorCopies, func(l struct{ file, function, kind, reason string }) bool {
				return l.file+" "+l.function+" "+l.kind == key
			}) {
				offenders = append(offenders, fmt.Sprintf("%s: %s in %q", hit.at, hit.kind, hit.function))
			}
		}
	}
	assert.Empty(t, offenders, "a clierr.Error is built or copied field by field only where errorCopies lists it, with whether the copy keeps OutcomeUnknown")
	for _, l := range errorCopies {
		assert.True(t, seen[l.file+" "+l.function+" "+l.kind], "%s %s is listed for %s and no longer has it; remove the entry", l.file, l.function, l.kind)
	}
}

// TestTheErrorCopyScanFindsEveryCopy asserts the rule of findErrorCopies on
// small sources: a clierr.Error literal, a literal, an assignment and a call
// chain that read two fields of one error, and a clierr.New with another
// error's code, read directly, through a selector chain or off a call result,
// are found; one field read, in a literal or a call chain, a clierr code
// constant and a file that does not import clierr are not.
func TestTheErrorCopyScanFindsEveryCopy(t *testing.T) {
	t.Parallel()
	const imp = "package p\n\nimport \"github.com/rancher/rancher-cli/internal/clierr\"\n\n"
	for _, tc := range []struct {
		name, src string
		want      []string
	}{
		{"an Error literal", imp + "func f() *clierr.Error { return &clierr.Error{Code: clierr.CodeGeneric} }\n", []string{"f " + errorLiteral}},
		{"two fields into a literal", imp + "func f(ce *clierr.Error) T { return T{C: ce.Code, M: ce.Message} }\n", []string{"f " + fieldsInLiteral}},
		{"two fields assigned", imp + "func f(ce *clierr.Error) { code, msg := ce.Code, ce.Message; _, _ = code, msg }\n", []string{"f " + fieldsAssigned}},
		{"an error rebuilt from another's code", imp + "func f(ce *clierr.Error) error { return clierr.New(ce.Code, \"x\") }\n", []string{"f " + rebuiltError}},
		{"an error rebuilt from a code in a selector chain", imp + "func f(c C) error { return clierr.New(c.Issue.Code, c.Issue.Message).WithRetryable(c.Issue.Retryable) }\n", []string{"f " + rebuiltError}},
		{"an error rebuilt from a code read off a call result", imp + "func f(err error) error { return clierr.New(clierr.From(err).Code, \"x\") }\n", []string{"f " + rebuiltError}},
		{"two fields through a call chain", imp + "func f(ce *clierr.Error) error { return clierr.New(clierr.CodeGeneric, ce.Message).WithHint(ce.Hint) }\n", []string{"f " + fieldsInCallChain}},
		{"one field through a call chain", imp + "func f(ce *clierr.Error) error { return clierr.New(clierr.CodeGeneric, \"x\").WithHint(ce.Hint) }\n", nil},
		{"one field read", imp + "func f(ce *clierr.Error) T { return T{C: ce.Code} }\n", nil},
		{"a code constant", imp + "func f() error { return clierr.New(clierr.CodeGeneric, \"x\") }\n", nil},
		{"a file that does not import clierr", "package p\n\nfunc f(ce E) T { return T{C: ce.Code, M: ce.Message} }\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hits, err := findErrorCopies("p.go", tc.src)
			require.NoError(t, err)
			var got []string
			for _, h := range hits {
				got = append(got, h.function+" "+h.kind)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
