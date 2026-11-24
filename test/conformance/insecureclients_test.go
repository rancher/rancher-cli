package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// insecureAllowed are the production functions that may turn TLS
// verification off, by file and function, each at one site, with the
// reason.
var insecureAllowed = map[string]string{
	"internal/token/callclients.go signInClient": "the client of a new sign-in of a call that passes --skip-verify; a stored login goes over it only when its entry is a plugin login entry that has no saved CA and whose login came from a sign-in that did not verify the server",
	"internal/client/httpx/httpx.go New":         "the TLS setting the Insecure option of httpx.Options turns on",
}

// signInClientCallers are the functions that may ask for the sign-in
// client, by file and function, with the reason.
var signInClientCallers = map[string]string{
	"internal/token/token.go storedCredToken":   "the reuse of a plugin login entry that has no saved CA and whose login came from a sign-in that did not verify the server, by a call that passes --skip-verify",
	"internal/token/pluginlogin.go pluginLogin": "the plugin's own sign-in, which records on the entry whether it skipped verification",
	"internal/token/token.go runWithParams":     "the interactive sign-in of a call that keeps no login",
}

// goSource is one Go file: its path from the module root and its text.
type goSource struct {
	path, src string
}

// insecureSites lists, as "path function", the function of every site in
// files that turns TLS verification off: a composite literal key Insecure
// or InsecureSkipVerify, of any type, or an assignment to a field of either
// name, whose value is not the constant false.
func insecureSites(files []goSource) ([]string, error) {
	var out []string
	err := eachFunc(files, func(path, fn string, body ast.Node) {
		ast.Inspect(body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.KeyValueExpr:
				if k, ok := v.Key.(*ast.Ident); ok && isInsecureField(k.Name) && !isFalse(v.Value) {
					out = append(out, path+" "+fn)
				}
			case *ast.AssignStmt:
				for i, lhs := range v.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || !isInsecureField(sel.Sel.Name) {
						continue
					}
					if len(v.Rhs) != len(v.Lhs) || !isFalse(v.Rhs[i]) {
						out = append(out, path+" "+fn)
					}
				}
			}
			return true
		})
	})
	return out, err
}

// signInClientCalls lists, as "path function", the function of every use
// of the clients value's signInClient method in files: a call or a method
// value.
func signInClientCalls(files []goSource) ([]string, error) {
	var out []string
	err := eachFunc(files, func(path, fn string, body ast.Node) {
		ast.Inspect(body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "signInClient" {
				out = append(out, path+" "+fn)
			}
			return true
		})
	})
	return out, err
}

// insecureViolations are the sites of insecureSites that insecureAllowed
// does not cover: any site outside its functions, and every site after
// the first in one of them.
func insecureViolations(files []goSource) ([]string, error) {
	sites, err := insecureSites(files)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range sites {
		if _, ok := insecureAllowed[s]; ok && !seen[s] {
			seen[s] = true
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// signInClientViolations are the calls of signInClientCalls from a
// function signInClientCallers does not list.
func signInClientViolations(files []goSource) ([]string, error) {
	calls, err := signInClientCalls(files)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range calls {
		if _, ok := signInClientCallers[c]; !ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// eachFunc parses every file and calls visit with every top-level
// declaration that can hold code: the body of each function and method,
// named without its receiver, and each value of a package-level var or
// const, named by its first name.
func eachFunc(files []goSource, visit func(path, fn string, body ast.Node)) error {
	for _, f := range files {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, f.path, f.src, 0)
		if err != nil {
			return err
		}
		for _, d := range file.Decls {
			switch v := d.(type) {
			case *ast.FuncDecl:
				if v.Body != nil {
					visit(f.path, v.Name.Name, v.Body)
				}
			case *ast.GenDecl:
				for _, spec := range v.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || len(vs.Names) == 0 {
						continue
					}
					for _, val := range vs.Values {
						visit(f.path, vs.Names[0].Name, val)
					}
				}
			}
		}
	}
	return nil
}

func isInsecureField(name string) bool {
	return name == "Insecure" || name == "InsecureSkipVerify"
}

func isFalse(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "false"
}

// productionSources is every non-test Go file of the repository outside
// testdata.
func productionSources(t *testing.T) []goSource {
	t.Helper()
	paths, err := repositoryFiles()
	require.NoError(t, err)
	var out []goSource
	for _, p := range paths {
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") || inTestdata(p) {
			continue
		}
		raw, err := os.ReadFile(p)
		require.NoError(t, err, p)
		out = append(out, goSource{path: p, src: string(raw)})
	}
	return out
}

// TestNoClientSkipsVerificationExceptTheSignInClient asserts no production
// code turns TLS verification off except the sign-in client of a call that
// passes --skip-verify and the httpx option it sets, each at one site, so a
// stored login cannot reach a client that skips verification
// through any other.
func TestNoClientSkipsVerificationExceptTheSignInClient(t *testing.T) {
	t.Run("the repository", func(t *testing.T) {
		files := productionSources(t)
		got, err := insecureViolations(files)
		require.NoError(t, err)
		assert.Empty(t, got, "TLS verification is turned off only where insecureAllowed says")
		sites, err := insecureSites(files)
		require.NoError(t, err)
		for s := range insecureAllowed {
			assert.Contains(t, sites, s, "an allowed site that no longer exists is removed from insecureAllowed")
		}
	})
	const signIn = "package token\n\nfunc (c *callClients) signInClient() { _ = httpx.Options{Insecure: true} }\n"
	for _, tc := range []struct {
		name  string
		files []goSource
		want  []string
	}{
		{"the sign-in client", []goSource{{"internal/token/callclients.go", signIn}}, nil},
		{"a second Insecure site", []goSource{{"internal/token/callclients.go", signIn + "func other(skip bool) { _ = httpx.Options{Insecure: skip} }\n"}},
			[]string{"internal/token/callclients.go other"}},
		{"a second Insecure site in the sign-in client", []goSource{{"internal/token/callclients.go",
			"package token\n\nfunc (c *callClients) signInClient(skip bool) { _ = httpx.Options{Insecure: true}; _ = httpx.Options{Insecure: skip} }\n"}},
			[]string{"internal/token/callclients.go signInClient"}},
		{"an assignment to an option", []goSource{{"internal/token/token.go", "package token\n\nfunc f(o httpx.Options, skip bool) { o.Insecure = skip }\n"}},
			[]string{"internal/token/token.go f"}},
		{"an assignment to a TLS setting outside httpx", []goSource{{"internal/kube/kube.go", "package kube\n\nfunc f(c *tls.Config) { c.InsecureSkipVerify = true }\n"}},
			[]string{"internal/kube/kube.go f"}},
		{"a Kubernetes TLS setting", []goSource{{"internal/kube/kube.go", "package kube\n\nfunc f(x bool) { _ = rest.TLSClientConfig{Insecure: x} }\n"}},
			[]string{"internal/kube/kube.go f"}},
		{"a TLS setting turned on in a literal", []goSource{{"internal/kube/kube.go", "package kube\n\nfunc f() { _ = &tls.Config{InsecureSkipVerify: true} }\n"}},
			[]string{"internal/kube/kube.go f"}},
		{"a package-level TLS setting", []goSource{{"internal/kube/kube.go", "package kube\n\nvar insecure = &tls.Config{InsecureSkipVerify: true}\n"}},
			[]string{"internal/kube/kube.go insecure"}},
		{"a package-level func literal", []goSource{{"internal/kube/kube.go", "package kube\n\nvar options = func(skip bool) httpx.Options { return httpx.Options{Insecure: skip} }\n"}},
			[]string{"internal/kube/kube.go options"}},
		{"the constant false", []goSource{{"internal/kube/kube.go", "package kube\n\nfunc f(o httpx.Options) { _ = httpx.Options{Insecure: false}; o.Insecure = false }\n"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := insecureViolations(tc.files)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestOnlyTheListedFunctionsAskForTheSignInClient asserts the sign-in
// client, the one client that may skip verification, is asked for only by
// the functions signInClientCallers lists.
func TestOnlyTheListedFunctionsAskForTheSignInClient(t *testing.T) {
	t.Run("the repository", func(t *testing.T) {
		files := productionSources(t)
		got, err := signInClientViolations(files)
		require.NoError(t, err)
		assert.Empty(t, got, "the sign-in client is asked for only by the functions signInClientCallers lists")
		calls, err := signInClientCalls(files)
		require.NoError(t, err)
		for c := range signInClientCallers {
			assert.Contains(t, calls, c, "a caller that no longer asks for the sign-in client is removed from signInClientCallers")
		}
	})
	call := func(path, fn string) goSource {
		return goSource{path, fmt.Sprintf("package token\n\nfunc (t tokenCmd) %s(c *callClients) { _, _ = c.signInClient() }\n", fn)}
	}
	for _, tc := range []struct {
		name  string
		files []goSource
		want  []string
	}{
		{"a listed function", []goSource{call("internal/token/token.go", "storedCredToken"), call("internal/token/pluginlogin.go", "pluginLogin")}, nil},
		{"a function not on the list", []goSource{call("internal/token/token.go", "resolveConn")}, []string{"internal/token/token.go resolveConn"}},
		{"a listed name in another file", []goSource{call("internal/token/completion.go", "storedCredToken")}, []string{"internal/token/completion.go storedCredToken"}},
		{"a method value", []goSource{{"internal/token/token.go", "package token\n\nfunc (t tokenCmd) resolveConn(c *callClients) { f := c.signInClient; _, _ = f() }\n"}},
			[]string{"internal/token/token.go resolveConn"}},
		{"a package-level method value", []goSource{{"internal/token/token.go", "package token\n\nvar signIn = (*callClients).signInClient\n"}},
			[]string{"internal/token/token.go signIn"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := signInClientViolations(tc.files)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
