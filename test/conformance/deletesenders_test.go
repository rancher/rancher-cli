package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// deleteMethod is a use of http.MethodDelete or of the string "DELETE".
	deleteMethod = "DELETE method"
	// clientGoDelete is a call of a method named Delete in a file that
	// imports a k8s.io/client-go package: a dynamic ResourceInterface.Delete,
	// a rest.Interface Delete() or a typed clientset Delete.
	clientGoDelete = "client-go Delete"
)

// deleteSenders are the places in non-test Go files that may name the DELETE
// method or call a client-go Delete, by file, enclosing function and pattern,
// each with the reason. Every delete of a Rancher or Kubernetes object goes
// through the two provider methods, which refuse a delete that is not a dry
// run and carries no precondition. internal/testrancher (the test servers and
// the fidelity harness, which deletes only the objects its probes created)
// and testdata trees are not scanned.
var deleteSenders = []struct {
	file, function, pattern, reason string
}{
	{"internal/client/provider/client.go", "dynamicClient.Delete", clientGoDelete, "the delete with a resourceVersion precondition"},
	{"internal/client/provider/client.go", "dynamicClient.Delete", deleteMethod, "names the method of the delete beside it for mapRequestErr; it builds no request"},
	{"internal/client/provider/client.go", "dynamicClient.DeleteWithUID", clientGoDelete, "the delete with a uid precondition"},
	{"internal/client/provider/client.go", "dynamicClient.DeleteWithUID", deleteMethod, "names the method of the delete beside it for mapRequestErr; it builds no request"},
	{"internal/cli/cloudcredclient.go", "cloudCredentialClient.Delete", deleteMethod, "the delete of a cloud credential through Norman, which takes no precondition"},
	{"internal/cli/cloudcredclient.go", "cloudCredentialClient.send", deleteMethod, "leaves out the request body for that delete"},
	{"internal/engine/provisioning/cloudcredwrite.go", "deleteCloudCredentialCmd.execute", deleteMethod, "names the method of the Norman delete in the error it returns; it builds no request"},
	{"internal/token/token.go", "samlAuth", deleteMethod, "the delete of the token the SAML login created for its own use, and clierr.ResponseError naming that method for its response"},
	{"internal/token/revoke.go", "revokeToken", deleteMethod, "the revoke of a token the CLI created for its own use"},
	{"cmd/rancher-credential-vault/vault.go", "vaultHandler.Erase", deleteMethod, "deletes a secret in Vault, not a Rancher object"},
}

// deleteSenderHit is one place a file names the DELETE method or calls a
// client-go Delete.
type deleteSenderHit struct {
	function, pattern, at string
}

// findDeleteSenders parses the Go source of path (read from the file when src
// is nil) and returns every use of http.MethodDelete or of the string literal
// "DELETE", and, when the file imports a k8s.io/client-go package, every call
// of a method named Delete, each with the function declaration it is in ("" at
// the package level).
func findDeleteSenders(path string, src any) ([]deleteSenderHit, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	clientGo := slices.ContainsFunc(file.Imports, func(spec *ast.ImportSpec) bool {
		p, _ := strconv.Unquote(spec.Path.Value)
		return p == "k8s.io/client-go" || strings.HasPrefix(p, "k8s.io/client-go/")
	})
	var hits []deleteSenderHit
	visit := func(function string) func(ast.Node) bool {
		return func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if x, ok := n.X.(*ast.Ident); ok && x.Name == "http" && n.Sel.Name == "MethodDelete" {
					hits = append(hits, deleteSenderHit{function, deleteMethod, fset.Position(n.Pos()).String()})
				}
			case *ast.BasicLit:
				if n.Kind == token.STRING && n.Value == `"DELETE"` {
					hits = append(hits, deleteSenderHit{function, deleteMethod, fset.Position(n.Pos()).String()})
				}
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && clientGo && sel.Sel.Name == "Delete" {
					hits = append(hits, deleteSenderHit{function, clientGoDelete, fset.Position(n.Pos()).String()})
				}
			}
			return true
		}
	}
	for _, decl := range file.Decls {
		function := ""
		if fd, ok := decl.(*ast.FuncDecl); ok {
			function = funcDeclName(fd)
		}
		ast.Inspect(decl, visit(function))
	}
	return hits, nil
}

// TestEveryDeleteIsSentByTheProviderOrIsListed asserts that a non-test Go
// file outside internal/testrancher and testdata names the DELETE method, or
// calls a client-go Delete in a file that imports client-go, only in a place
// deleteSenders lists with its reason, and that every listed place still
// does.
func TestEveryDeleteIsSentByTheProviderOrIsListed(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	var offenders []string
	seen := map[string]bool{}
	for _, path := range files {
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || inTestdata(path) || strings.HasPrefix(path, "internal/testrancher/") {
			continue
		}
		hits, perr := findDeleteSenders(path, nil)
		require.NoError(t, perr, path)
		for _, hit := range hits {
			key := path + " " + hit.function + " " + hit.pattern
			seen[key] = true
			listed := slices.ContainsFunc(deleteSenders, func(l struct{ file, function, pattern, reason string }) bool {
				return l.file+" "+l.function+" "+l.pattern == key
			})
			if !listed {
				offenders = append(offenders, fmt.Sprintf("%s: %s in %q", hit.at, hit.pattern, hit.function))
			}
		}
	}
	assert.Empty(t, offenders, "a DELETE is sent only through the provider's two delete methods, or the place is listed with its reason")
	for _, l := range deleteSenders {
		assert.True(t, seen[l.file+" "+l.function+" "+l.pattern], "%s %s is listed for %s and no longer has it; remove the entry", l.file, l.function, l.pattern)
	}
}

// TestTheDeleteSenderScanFindsEveryPattern asserts the rule of
// findDeleteSenders on small sources: http.MethodDelete and the string
// "DELETE" are found in any file, inside a function or at the package level,
// and a call of a method named Delete only in a file that imports client-go.
func TestTheDeleteSenderScanFindsEveryPattern(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, src string
		want      []string
	}{
		{"http.MethodDelete in a function", "package p\n\nimport \"net/http\"\n\nfunc f() string { return http.MethodDelete }\n", []string{"f " + deleteMethod}},
		{"the string DELETE at the package level", "package p\n\nconst m = \"DELETE\"\n", []string{" " + deleteMethod}},
		{"a Delete call in a file that imports client-go", "package p\n\nimport \"k8s.io/client-go/rest\"\n\nfunc (c *T) g(r rest.Interface) { r.Delete() }\n", []string{"T.g " + clientGoDelete}},
		{"a Delete call in a file that does not import client-go", "package p\n\nfunc g(m M) { m.Delete() }\n", nil},
		{"a map delete", "package p\n\nimport \"k8s.io/client-go/rest\"\n\nvar _ rest.Interface\n\nfunc g(m map[string]int) { delete(m, \"x\") }\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hits, err := findDeleteSenders("p.go", tc.src)
			require.NoError(t, err)
			var got []string
			for _, h := range hits {
				got = append(got, h.function+" "+h.pattern)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// uidDeleteCaller is a function of a non-test Go file that may name a method
// called DeleteWithUID or planWithUID, with the reason.
type uidDeleteCaller struct {
	file, function, method, reason string
}

// uidDeleteCallers are the only functions that send a delete with a uid
// precondition or plan the delete of an object the caller cannot get: the
// uid precondition is chosen by the Kubernetes authorizer's refusal of get on
// the object and by nothing else. internal/testrancher and testdata trees are
// not scanned. A call through reflection is not seen.
var uidDeleteCallers = []uidDeleteCaller{
	{"internal/engine/resources/delete.go", "deleteCmd.planWithUID", "DeleteWithUID", "the dry-run delete and the delete of an object the Kubernetes authorizer refuses the caller permission to get"},
	{"internal/engine/resources/delete.go", "deleteCmd.planWith", "planWithUID", "the one function that may call it; that it calls it only after clierr.AuthorizerRefused matched the error of the read is asserted by TestEveryDeleteDeletesAnObjectTheCallerCannotGet"},
}

// uidDeleteHit is one use of a method named DeleteWithUID or planWithUID.
type uidDeleteHit struct {
	function, method, at string
}

// findUIDDeleteUses parses the Go source of path (read from the file when src
// is nil) and returns every selector that names DeleteWithUID or
// planWithUID, a call or a method value, each with the function
// declaration it is in ("" at the package level). A closure belongs to the
// declaration that holds it; a declaration of the method is not a use.
func findUIDDeleteUses(path string, src any) ([]uidDeleteHit, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	var hits []uidDeleteHit
	for _, decl := range file.Decls {
		function := ""
		if fd, ok := decl.(*ast.FuncDecl); ok {
			function = funcDeclName(fd)
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && (sel.Sel.Name == "DeleteWithUID" || sel.Sel.Name == "planWithUID") {
				hits = append(hits, uidDeleteHit{function, sel.Sel.Name, fset.Position(sel.Sel.Pos()).String()})
			}
			return true
		})
	}
	return hits, nil
}

// TestTheUIDPreconditionIsChosenOnlyByTheAuthorizersRefusal asserts that a
// non-test Go file outside internal/testrancher and testdata names the method
// DeleteWithUID or planWithUID only in a function uidDeleteCallers lists
// with its reason, and that every listed function still does.
func TestTheUIDPreconditionIsChosenOnlyByTheAuthorizersRefusal(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	var offenders []string
	seen := map[string]bool{}
	for _, path := range files {
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || inTestdata(path) || strings.HasPrefix(path, "internal/testrancher/") {
			continue
		}
		hits, perr := findUIDDeleteUses(path, nil)
		require.NoError(t, perr, path)
		for _, hit := range hits {
			key := path + " " + hit.function + " " + hit.method
			seen[key] = true
			listed := slices.ContainsFunc(uidDeleteCallers, func(l uidDeleteCaller) bool {
				return l.file+" "+l.function+" "+l.method == key
			})
			if !listed {
				offenders = append(offenders, fmt.Sprintf("%s: %s in %q", hit.at, hit.method, hit.function))
			}
		}
	}
	assert.Empty(t, offenders, "a delete with a uid precondition is sent, and the delete of an object the caller cannot get is planned, only by the functions uidDeleteCallers lists")
	for _, l := range uidDeleteCallers {
		assert.True(t, seen[l.file+" "+l.function+" "+l.method], "%s %s is listed for %s and no longer has it; remove the entry", l.file, l.function, l.method)
	}
}

// TestTheUIDDeleteScanFindsEveryUse asserts the rule of findUIDDeleteUses on
// small sources: a call and a method value of either name are found, a use in
// a closure belongs to the function that holds it, and a declaration of the
// method or another method name is not a use.
func TestTheUIDDeleteScanFindsEveryUse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, src string
		want      []string
	}{
		{"a DeleteWithUID call in a method", "package p\n\nfunc (c *T) f(d D) { d.DeleteWithUID() }\n", []string{"T.f DeleteWithUID"}},
		{"a planWithUID call in a function", "package p\n\nfunc f(c C) { c.planWithUID() }\n", []string{"f planWithUID"}},
		{"a call through a type assertion in a closure", "package p\n\nfunc f(c C) { func() { c.(D).DeleteWithUID() }() }\n", []string{"f DeleteWithUID"}},
		{"a method value", "package p\n\nfunc f(d D) { _ = d.DeleteWithUID }\n", []string{"f DeleteWithUID"}},
		{"the declaration of the method", "package p\n\nfunc (c *T) DeleteWithUID() {}\n\ntype D interface{ DeleteWithUID() }\n", nil},
		{"another method", "package p\n\nfunc f(d D) { d.Delete() }\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hits, err := findUIDDeleteUses("p.go", tc.src)
			require.NoError(t, err)
			var got []string
			for _, h := range hits {
				got = append(got, h.function+" "+h.method)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
