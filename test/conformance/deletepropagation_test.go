package conformance

import (
	"fmt"
	"go/ast"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every DELETE the CLI sends through the cluster client sends a
// propagationPolicy of Background, as kubectl does. Without one the server
// applies the default for that kind: a Job (batch/v1) keeps its pods and the
// server's response carries a warning on each request, the dry-run preview
// included. The scans below fail on each of these shapes:
//
//   - a metav1.DeleteOptions anywhere in the production tree outside the
//     two functions that build the options of every delete through the
//     cluster client (deletePropagationSeams: the methods
//     (*dynamicClient).Delete and (*dynamicClient).DeleteWithUID), in any
//     form: a literal, a var, a function result, a struct field, an element
//     type, `new(metav1.DeleteOptions)` or a conversion; a zero value sends
//     no policy;
//   - a call of metav1.NewDeleteOptions or metav1.NewPreconditionDeleteOptions
//     anywhere; both return options with no policy;
//   - in each of those functions: not exactly one DeleteOptions, a literal
//     with no PropagationPolicy key, a function that does not name
//     metav1.DeletePropagationBackground, or one that names another
//     metav1.DeletePropagation constant;
//   - a reference to http.MethodDelete or the string "DELETE", a Verb call
//     whose argument is the string "delete" in any case, and a call of
//     Delete with no arguments (client-go's rest request), each outside
//     deletePropagationAllowlist (the file, the function and the number of
//     matches, with the reason it does not go through those functions).
//
// The scans cover the production tree: every non-test Go file outside
// testdata and outside internal/testrancher (the test servers and the
// fidelity recorder, which are not the CLI). A request built through a
// helper that takes its method from a variable is covered where the method
// is written. The request each ClusterClient delete sends is asserted by
// TestEveryClusterClientDeleteSendsTheBackgroundPropagationPolicy
// in internal/client/provider.
//
// Limits: a DeleteOptions value made through reflection or a decoder
// (json.Unmarshal into a value of another type) is not followed, and neither
// is a method written as another string constant or a Verb call whose
// argument is computed instead of a string literal. An assignment to
// PropagationPolicy after the literal, and a policy value built by a
// conversion (no metav1.DeletePropagation selector), are checked only by that
// provider test.

const (
	deletePropagationSeam    = "internal/client/provider/client.go:(*dynamicClient).Delete"
	deletePropagationUIDSeam = "internal/client/provider/client.go:(*dynamicClient).DeleteWithUID"
	deleteMethodText         = "DELETE"
)

// deletePropagationSeams are the functions that build the DeleteOptions of a
// delete through the cluster client: the delete with a resourceVersion
// precondition and the delete with a uid precondition.
var deletePropagationSeams = []string{deletePropagationSeam, deletePropagationUIDSeam}

// deletePropagationAllowlist maps "<file>:<func>" to the matches of the
// delete rule it covers. A different number of matches fails the test.
var deletePropagationAllowlist = map[string]requestAllowance{
	deletePropagationSeam: {1, "the delete with a resourceVersion precondition through the cluster client: the method name it gives the error mapper; its DeleteOptions are checked above"},
	deletePropagationUIDSeam: {4, "the delete with a uid precondition through the cluster client: the rest request it sends through tbl, which keeps the response, with its DeleteOptions as the body, " +
		"and the method name it gives the error mapper three times; its DeleteOptions are checked above"},
	"internal/token/revoke.go:revokeToken": {1, "the delete of one v3 token or ext token the CLI created, sent with no body outside the cluster client; " +
		"a v3 token is a custom resource, for which the policy changes nothing, and an ext token is stored as a Secret, " +
		"whose default policy already deletes dependents in the background; the revoke path is restricted credential code " +
		"(TestTokenRevokesOnlyFromTheFilesThatMayDeleteTokens lists the files that may send it)"},
	"internal/token/token.go:samlAuth":                                                   {2, "Rancher's /v1-public/authtokens/<id> delete and the error text that names it; Norman takes no DeleteOptions"},
	"internal/cli/cloudcredclient.go:(*cloudCredentialClient).Delete":                    {1, "the method name of Rancher's Norman /v3/cloudcredentials delete; Norman takes no DeleteOptions"},
	"internal/cli/cloudcredclient.go:(*cloudCredentialClient).send":                      {1, "the check that a Norman /v3/cloudcredentials delete sends no body; Norman takes no DeleteOptions"},
	"internal/engine/provisioning/cloudcredwrite.go:(*deleteCloudCredentialCmd).execute": {1, "the method name of Rancher's Norman /v3/cloudcredentials delete in the error it returns; Norman takes no DeleteOptions"},
	"cmd/rancher-credential-vault/vault.go:(*vaultHandler).Erase":                        {1, "HashiCorp Vault's API, not a delete through the cluster client"},
}

// deleteOptionsConstructors are the functions of apimachinery that return
// DeleteOptions with no propagationPolicy.
var deleteOptionsConstructors = []string{"NewDeleteOptions", "NewPreconditionDeleteOptions"}

// deleteScan is what the scan found in one file.
type deleteScan struct {
	requests []requestMatch
	findings []string
}

// scanDeletePropagation applies the delete rules to one parsed file.
func scanDeletePropagation(fset *token.FileSet, filePath string, f *ast.File) deleteScan {
	httpName := importName(f, netHTTPPath)
	metaName := importName(f, metav1Path)
	var out deleteScan
	for _, decl := range f.Decls {
		fn := "(package level)"
		if fd, ok := decl.(*ast.FuncDecl); ok {
			fn = byIDFunctionName(fd)
		}
		key := filePath + ":" + fn
		selected := map[*ast.Ident]bool{}
		ast.Inspect(decl, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				selected[sel.Sel] = true
			}
			return true
		})
		isOptions := func(e ast.Expr) bool {
			switch x := ast.Unparen(e).(type) {
			case *ast.SelectorExpr:
				id, ok := x.X.(*ast.Ident)
				return ok && metaName != "" && metaName != "." && id.Name == metaName && x.Sel.Name == "DeleteOptions"
			case *ast.Ident:
				return metaName == "." && x.Name == "DeleteOptions" && !selected[x]
			}
			return false
		}
		constructor := func(e ast.Expr) string {
			switch x := e.(type) {
			case *ast.SelectorExpr:
				id, ok := x.X.(*ast.Ident)
				if ok && metaName != "" && metaName != "." && id.Name == metaName && slices.Contains(deleteOptionsConstructors, x.Sel.Name) {
					return x.Sel.Name
				}
			case *ast.Ident:
				if metaName == "." && !selected[x] && slices.Contains(deleteOptionsConstructors, x.Name) {
					return x.Name
				}
			}
			return ""
		}
		var (
			options      []token.Pos
			literals     []*ast.CompositeLit
			constructors = map[token.Pos]string{}
			background   int
			otherKinds   []token.Pos
		)
		request := func(pos token.Pos, text string) {
			out.requests = append(out.requests, requestMatch{key, fset.Position(pos).String() + ": " + text})
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if isOptions(n) {
					options = append(options, n.Pos())
				}
				if name := constructor(n); name != "" {
					constructors[n.Pos()] = name
				}
				if id, ok := n.X.(*ast.Ident); ok && metaName != "" && id.Name == metaName {
					switch name := n.Sel.Name; {
					case name == "DeletePropagationBackground":
						background++
					case strings.HasPrefix(name, "DeletePropagation"):
						otherKinds = append(otherKinds, n.Pos())
					}
				}
				if httpName != "" && httpMember(n, httpName) == "MethodDelete" {
					request(n.Pos(), "reference to http.MethodDelete")
				}
			case *ast.Ident:
				if isOptions(n) {
					options = append(options, n.Pos())
				}
				if name := constructor(n); name != "" {
					constructors[n.Pos()] = name
				}
				if httpName == "." && !selected[n] && n.Name == "MethodDelete" {
					request(n.Pos(), "reference to MethodDelete")
				}
			case *ast.CompositeLit:
				if isOptions(n.Type) {
					literals = append(literals, n)
				}
			case *ast.BasicLit:
				if n.Kind == token.STRING {
					if s, err := strconv.Unquote(n.Value); err == nil && s == deleteMethodText {
						request(n.Pos(), `the string "DELETE"`)
					}
				}
			case *ast.CallExpr:
				if sel, ok := ast.Unparen(n.Fun).(*ast.SelectorExpr); ok && sel.Sel.Name == "Delete" && len(n.Args) == 0 {
					request(n.Pos(), "call of Delete with no arguments, a rest request that sends no DeleteOptions")
				}
				if sel, ok := ast.Unparen(n.Fun).(*ast.SelectorExpr); ok && sel.Sel.Name == "Verb" && len(n.Args) == 1 {
					if lit, ok := ast.Unparen(n.Args[0]).(*ast.BasicLit); ok && lit.Kind == token.STRING {
						// The exact string "DELETE" is reported by the string rule.
						if s, err := strconv.Unquote(lit.Value); err == nil && strings.EqualFold(s, deleteMethodText) && s != deleteMethodText {
							request(lit.Pos(), `a Verb of "delete" in a case other than the exact string "DELETE"; send the delete through ClusterClient.Delete`)
						}
					}
				}
			}
			return true
		})
		report := func(pos token.Pos, text string) {
			out.findings = append(out.findings, fset.Position(pos).String()+": "+text)
		}
		for pos, name := range constructors {
			report(pos, "metav1."+name+" returns DeleteOptions with no propagationPolicy; build the options in a function that builds the options of a delete through the cluster client")
		}
		if !slices.Contains(deletePropagationSeams, key) {
			for _, pos := range options {
				report(pos, "metav1.DeleteOptions outside the functions that build the options of a delete through the cluster client; a zero value sends no propagationPolicy")
			}
			continue
		}
		if len(options) != 1 || len(literals) != 1 {
			for _, pos := range options {
				report(pos, fmt.Sprintf("%s has %d DeleteOptions and %d literal(s); it builds exactly one literal", fn, len(options), len(literals)))
			}
			if len(options) == 0 {
				report(decl.Pos(), fn+" builds no DeleteOptions")
			}
			continue
		}
		hasPolicy := slices.ContainsFunc(literals[0].Elts, func(e ast.Expr) bool {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				return false
			}
			id, ok := kv.Key.(*ast.Ident)
			return ok && id.Name == "PropagationPolicy"
		})
		if !hasPolicy {
			report(literals[0].Pos(), "DeleteOptions literal with no PropagationPolicy")
		}
		if background != 1 {
			report(decl.Pos(), fmt.Sprintf("%s names metav1.DeletePropagationBackground %d times, want 1", fn, background))
		}
		for _, pos := range otherKinds {
			report(pos, "a propagation policy other than Background; the delete options of a delete through the cluster client carry Background")
		}
	}
	return out
}

// TestEveryDeleteSendsTheBackgroundPropagationPolicyOrIsListed asserts the
// scans above over the production tree: the DeleteOptions of the two
// functions, and every other DELETE written in the tree, which the allowlist
// says does not go through those functions.
func TestEveryDeleteSendsTheBackgroundPropagationPolicyOrIsListed(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	var (
		requests []requestMatch
		findings []string
		seamSeen = map[string]bool{}
	)
	for i, f := range tree.files {
		if inTestrancher(tree.paths[i]) {
			continue
		}
		scan := scanDeletePropagation(tree.fset, tree.paths[i], f)
		requests = append(requests, scan.requests...)
		findings = append(findings, scan.findings...)
		for _, seam := range deletePropagationSeams {
			if seamFile, seamName, _ := strings.Cut(seam, ":"); tree.paths[i] == seamFile {
				seamSeen[seam] = slices.ContainsFunc(f.Decls, func(d ast.Decl) bool {
					fd, ok := d.(*ast.FuncDecl)
					return ok && byIDFunctionName(fd) == seamName
				})
			}
		}
	}
	for _, seam := range deletePropagationSeams {
		assert.True(t, seamSeen[seam], "the function %s exists", seam)
	}
	slices.Sort(findings)
	for _, finding := range findings {
		t.Error(finding)
	}
	for _, problem := range checkRequestMatches(requests, deletePropagationAllowlist) {
		t.Error(problem)
	}
	for key, a := range deletePropagationAllowlist {
		assert.NotEmpty(t, a.why, key)
	}
}

// TestDeletePropagationScanFixtures asserts the scans on source snippets: a
// literal with no policy, a literal with another policy, a literal outside
// the two functions, each other use of the type, the constructors that return
// options with no policy, an aliased and a dot import, each form of a DELETE
// request, and the accepted forms. A looser scan fails its row.
func TestDeletePropagationScanFixtures(t *testing.T) {
	t.Parallel()
	const (
		meta = `import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"; `
		web  = `import "net/http"; `
		seam = "internal/client/provider/client.go"
	)
	for _, tc := range []struct {
		name, filePath, src string
		findings            string
		requests            string
	}{
		{"the seam with Background", seam, `package p; ` + meta + `func (c *dynamicClient) Delete() { b := metav1.DeletePropagationBackground; _ = metav1.DeleteOptions{PropagationPolicy: &b} }`, "", ""},
		{"the seam with no policy", seam, `package p; ` + meta + `func (c *dynamicClient) Delete() { b := metav1.DeletePropagationBackground; _ = b; _ = metav1.DeleteOptions{} }`, "DeleteOptions literal with no PropagationPolicy", ""},
		{"the seam with Foreground", seam, `package p; ` + meta + `func (c *dynamicClient) Delete() { b := metav1.DeletePropagationForeground; _ = metav1.DeleteOptions{PropagationPolicy: &b} }`, "a propagation policy other than Background", ""},
		{"the seam with a policy and Foreground beside it", seam, `package p; ` + meta + `func (c *dynamicClient) Delete() { b := metav1.DeletePropagationBackground; f := metav1.DeletePropagationForeground; _, _ = f, metav1.DeleteOptions{PropagationPolicy: &b} }`, "a propagation policy other than Background", ""},
		{"the seam with a policy that is not a constant", seam, `package p; ` + meta + `func (c *dynamicClient) Delete(b metav1.DeletionPropagation) { _ = metav1.DeleteOptions{PropagationPolicy: &b} }`, "names metav1.DeletePropagationBackground 0 times, want 1", ""},
		{"the seam with two literals", seam, `package p; ` + meta + `func (c *dynamicClient) Delete() { b := metav1.DeletePropagationBackground; _, _ = metav1.DeleteOptions{PropagationPolicy: &b}, metav1.DeleteOptions{PropagationPolicy: &b} }`, "has 2 DeleteOptions and 2 literal(s)", ""},
		{"the seam with no options", seam, `package p; ` + meta + `func (c *dynamicClient) Delete() { _ = metav1.DeletePropagationBackground }`, "Delete builds no DeleteOptions", ""},
		{"the uid seam with Background", seam, `package p; ` + meta + `func (c *dynamicClient) DeleteWithUID() { b := metav1.DeletePropagationBackground; _ = metav1.DeleteOptions{PropagationPolicy: &b} }`, "", ""},
		{"the uid seam with no policy", seam, `package p; ` + meta + `func (c *dynamicClient) DeleteWithUID() { b := metav1.DeletePropagationBackground; _ = b; _ = metav1.DeleteOptions{} }`, "DeleteOptions literal with no PropagationPolicy", ""},
		{"the uid seam with two literals", seam, `package p; ` + meta + `func (c *dynamicClient) DeleteWithUID() { b := metav1.DeletePropagationBackground; _, _ = metav1.DeleteOptions{PropagationPolicy: &b}, metav1.DeleteOptions{PropagationPolicy: &b} }`, "has 2 DeleteOptions and 2 literal(s)", ""},
		{"a third method of the client in the seam file", seam, `package p; ` + meta + `func (c *dynamicClient) Update() { b := metav1.DeletePropagationBackground; _ = metav1.DeleteOptions{PropagationPolicy: &b} }`, "metav1.DeleteOptions outside the functions", ""},
		{"the seam with a constructor", seam, `package p; ` + meta + `func (c *dynamicClient) Delete() { b := metav1.DeletePropagationBackground; _ = metav1.DeleteOptions{PropagationPolicy: &b}; _ = metav1.NewDeleteOptions(0) }`, "metav1.NewDeleteOptions returns DeleteOptions with no propagationPolicy", ""},
		{"a function named Delete in the seam file", seam, `package p; ` + meta + `func Delete() { b := metav1.DeletePropagationBackground; _ = metav1.DeleteOptions{PropagationPolicy: &b} }`, "metav1.DeleteOptions outside the functions", ""},
		{"a method named Delete of another type in the seam file", seam, `package p; ` + meta + `func (o *other) Delete() { b := metav1.DeletePropagationBackground; _ = metav1.DeleteOptions{PropagationPolicy: &b} }`, "metav1.DeleteOptions outside the functions", ""},
		{"the seam name in another file", "internal/cli/x.go", `package p; ` + meta + `func (c *dynamicClient) Delete() { b := metav1.DeletePropagationBackground; _ = metav1.DeleteOptions{PropagationPolicy: &b} }`, "metav1.DeleteOptions outside the functions", ""},
		{"a literal elsewhere", "internal/cli/x.go", `package p; ` + meta + `func f() { _ = metav1.DeleteOptions{} }`, "metav1.DeleteOptions outside the functions", ""},
		{"a literal with a policy elsewhere", "internal/cli/x.go", `package p; ` + meta + `func f(b *metav1.DeletionPropagation) { _ = metav1.DeleteOptions{PropagationPolicy: b} }`, "metav1.DeleteOptions outside the functions", ""},
		{"a var declaration", "internal/cli/x.go", `package p; ` + meta + `func f() { var o metav1.DeleteOptions; _ = o }`, "metav1.DeleteOptions outside the functions", ""},
		{"the new keyword", "internal/cli/x.go", `package p; ` + meta + `func f() { _ = new(metav1.DeleteOptions) }`, "metav1.DeleteOptions outside the functions", ""},
		{"a parameter", "internal/cli/x.go", `package p; ` + meta + `func f(o metav1.DeleteOptions) {}`, "metav1.DeleteOptions outside the functions", ""},
		{"a struct field", "internal/cli/x.go", `package p; ` + meta + `type s struct { o metav1.DeleteOptions }`, "metav1.DeleteOptions outside the functions", ""},
		{"an aliased import", "internal/cli/x.go", `package p; import mv "k8s.io/apimachinery/pkg/apis/meta/v1"; func f() { _ = mv.DeleteOptions{} }`, "metav1.DeleteOptions outside the functions", ""},
		{"a dot import", "internal/cli/x.go", `package p; import . "k8s.io/apimachinery/pkg/apis/meta/v1"; func f() { _ = DeleteOptions{} }`, "metav1.DeleteOptions outside the functions", ""},
		{"the constructor NewDeleteOptions", "internal/cli/x.go", `package p; ` + meta + `func f() { _ = *metav1.NewDeleteOptions(0) }`, "metav1.NewDeleteOptions returns DeleteOptions with no propagationPolicy", ""},
		{"the constructor NewPreconditionDeleteOptions", "internal/cli/x.go", `package p; ` + meta + `func f() { _ = *metav1.NewPreconditionDeleteOptions("u") }`, "metav1.NewPreconditionDeleteOptions returns DeleteOptions with no propagationPolicy", ""},
		{"a constructor through an aliased import", "internal/cli/x.go", `package p; import mv "k8s.io/apimachinery/pkg/apis/meta/v1"; func f() { _ = mv.NewDeleteOptions(0) }`, "metav1.NewDeleteOptions returns DeleteOptions with no propagationPolicy", ""},
		{"a constructor through a dot import", "internal/cli/x.go", `package p; import . "k8s.io/apimachinery/pkg/apis/meta/v1"; func f() { _ = NewDeleteOptions(0) }`, "metav1.NewDeleteOptions returns DeleteOptions with no propagationPolicy", ""},
		{"another options type", "internal/cli/x.go", `package p; ` + meta + `func f() { _ = metav1.GetOptions{}; _ = metav1.DeletionPropagation("") }`, "", ""},
		{"another package with the same name", "internal/cli/x.go", `package p; import metav1 "example.com/other"; func f() { _ = metav1.DeleteOptions{}; _ = metav1.NewDeleteOptions(0) }`, "", ""},
		{"http.MethodDelete", "internal/cli/x.go", `package p; ` + web + `func f() { _, _ = http.NewRequest(http.MethodDelete, "", nil) }`, "", "reference to http.MethodDelete"},
		{"the string DELETE", "internal/cli/x.go", `package p; ` + web + `func f() { _, _ = http.NewRequest("DELETE", "", nil) }`, "", `the string "DELETE"`},
		{"the string DELETE in a request literal", "internal/cli/x.go", `package p; ` + web + `func f() { _ = &http.Request{Method: "DELETE"} }`, "", `the string "DELETE"`},
		{"MethodDelete through a dot import", "internal/cli/x.go", `package p; import . "net/http"; func f() { _, _ = NewRequest(MethodDelete, "", nil) }`, "", "reference to MethodDelete"},
		{"a rest request delete", "internal/cli/x.go", `package p; func f(c interface{ Delete() int }) { _ = c.Delete() }`, "", "call of Delete with no arguments"},
		{"a rest request verb in lower case", "internal/cli/x.go", `package p; func f(c interface{ Verb(string) int }) { _ = c.Verb("delete") }`, "", `a Verb of "delete" in a case other than the exact string "DELETE"`},
		{"a rest request verb in mixed case", "internal/cli/x.go", `package p; func f(c interface{ Verb(string) int }) { _ = c.Verb("Delete") }`, "", `a Verb of "delete" in a case other than the exact string "DELETE"`},
		{"a rest request verb in upper case", "internal/cli/x.go", `package p; func f(c interface{ Verb(string) int }) { _ = c.Verb("DELETE") }`, "", `the string "DELETE"`},
		{"a rest request verb of another method", "internal/cli/x.go", `package p; func f(c interface{ Verb(string) int }) { _ = c.Verb("get") }`, "", ""},
		{"a GET", "internal/cli/x.go", `package p; ` + web + `func f() { _, _ = http.NewRequest(http.MethodGet, "", nil) }`, "", ""},
		{"Delete with arguments", "internal/cli/x.go", `package p; func f(c interface{ Delete(string) int }) { _ = c.Delete("x") }`, "", ""},
		{"the word delete", "internal/cli/x.go", `package p; func f() { _ = "delete"; _ = "DELETED" }`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fset, f := parseSnippet(t, tc.filePath, tc.src)
			got := scanDeletePropagation(fset, tc.filePath, f)
			findings := strings.Join(got.findings, "\n")
			var requests []string
			for _, r := range got.requests {
				requests = append(requests, r.text)
			}
			if tc.findings == "" {
				assert.Empty(t, got.findings)
			} else {
				assert.Contains(t, findings, tc.findings)
			}
			if tc.requests == "" {
				assert.Empty(t, requests)
			} else {
				assert.Contains(t, strings.Join(requests, "\n"), tc.requests)
			}
		})
	}
}

// TestDeletePropagationAllowlistRules asserts the allowlist check reports a
// match outside the list, a count that differs from the entry's, and an entry
// that matches nothing.
func TestDeletePropagationAllowlistRules(t *testing.T) {
	t.Parallel()
	matches := []requestMatch{{"a.go:f", "a.go:1: the string \"DELETE\""}, {"b.go:g", "b.go:2: x"}, {"b.go:g", "b.go:3: y"}}
	allow := map[string]requestAllowance{"a.go:f": {1, "r"}, "b.go:g": {1, "r"}, "c.go:h": {1, "r"}}
	problems := strings.Join(checkRequestMatches(matches, allow), "\n")
	assert.Contains(t, problems, "b.go:g: the allowlist covers 1 match(es)")
	assert.Contains(t, problems, "allowlist entry c.go:h matched nothing")
	assert.NotContains(t, problems, "a.go:f")
	assert.Contains(t, strings.Join(checkRequestMatches([]requestMatch{{"d.go:k", "d.go:1: x"}}, nil), "\n"), "d.go:1: x")
}
