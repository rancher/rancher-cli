package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clientGoWriteMethods are the names of the client-go methods that send a
// write or start one: the dynamic, typed and rest clients' write methods, the
// subresource writes of the typed clients, and rest.Request's Verb, which
// sets the method of a request built by hand.
var clientGoWriteMethods = []string{
	"Create", "Update", "UpdateStatus", "Patch", "Apply", "ApplyStatus", "Delete", "DeleteCollection", "Post", "Put",
	"Verb", "Evict", "UpdateScale", "UpdateResize", "UpdateApproval", "UpdateEphemeralContainers", "Finalize", "Bind",
}

// clientGoClientPackages are the client-go import paths, and the trees under
// them, that give a client that sends requests. The other client-go
// packages, such as tools/clientcmd/api, tools/auth/exec and
// pkg/apis/clientauthentication, have only types and helpers.
var clientGoClientPackages = []string{
	"k8s.io/client-go/dynamic", "k8s.io/client-go/kubernetes", "k8s.io/client-go/rest",
	"k8s.io/client-go/discovery", "k8s.io/client-go/restmapper", "k8s.io/client-go/scale", "k8s.io/client-go/metadata",
}

// writeOutcomeExceptions are the functions of non-test Go files that call a
// method named in clientGoWriteMethods in a package that imports a client-go
// client package without recording whether the request was written and
// mapping the error as a write's, each with the reason.
var writeOutcomeExceptions = []struct {
	file, function, method, reason string
}{
	{"internal/client/kube/restconfig.go", "RequireLogin", "Apply", "applies the change a config write left unfinished to an error; it sends no request"},
	{"internal/client/provider/identity.go", "Identity", "Create", "a SelfUser create, which the server responds to without storing an object, like the reviews that PostReview sends"},
	{"internal/client/provider/owner.go", "Owner", "Create", "a SelfSubjectReview create, which the server responds to without storing an object, like the reviews that PostReview sends"},
	{"internal/token/noterminal.go", "tokenCmd.unsupportedConfigRemedy", "Apply", "applies the change a config write left unfinished to an error; it sends no request"},
	{"internal/token/noterminal.go", "tokenCmd.noTerminalError", "Apply", "applies the change a config write left unfinished to an error; it sends no request"},
}

// writeCallHit is one call of a client-go write method, with whether the
// function it is in calls traceWrite and mapWriteErr.
type writeCallHit struct {
	function, method, at string
	traced, mapped       bool
}

// importsClientGoClient reports whether file imports a package of
// clientGoClientPackages or one under them.
func importsClientGoClient(file *ast.File) bool {
	return slices.ContainsFunc(file.Imports, func(spec *ast.ImportSpec) bool {
		p, _ := strconv.Unquote(spec.Path.Value)
		return slices.ContainsFunc(clientGoClientPackages, func(c string) bool {
			return p == c || strings.HasPrefix(p, c+"/")
		})
	})
}

// clientGoPackages are the directories of the non-test Go files in paths, of
// which at least one non-test file imports a client-go client package: a
// file of such a package can call a client that another file of it built,
// without importing client-go itself.
func clientGoPackages(paths []string) (map[string]bool, error) {
	dirs := map[string]bool{}
	for _, path := range paths {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}
		if importsClientGoClient(file) {
			dirs[filepath.Dir(path)] = true
		}
	}
	return dirs, nil
}

// findClientGoWrites parses the Go source of path (read from the file when
// src is nil) and, when the file imports a client-go client package or
// inPackage says another file of its package does, returns every call of a
// method named in clientGoWriteMethods with the function declaration it is
// in ("" at the package level), and whether that declaration calls
// traceWrite and mapWriteErr anywhere in its body. A closure belongs to the
// declaration it is written in.
func findClientGoWrites(path string, src any, inPackage bool) ([]writeCallHit, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	if !inPackage && !importsClientGoClient(file) {
		return nil, nil
	}
	var hits []writeCallHit
	for _, decl := range file.Decls {
		function := ""
		if fd, ok := decl.(*ast.FuncDecl); ok {
			function = funcDeclName(fd)
		}
		var calls []writeCallHit
		traced, mapped := false, false
		ast.Inspect(decl, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				name = fn.Sel.Name
				if slices.Contains(clientGoWriteMethods, name) {
					calls = append(calls, writeCallHit{function: function, method: name, at: fset.Position(fn.Sel.Pos()).String()})
				}
			case *ast.Ident:
				name = fn.Name
			}
			traced = traced || name == "traceWrite"
			mapped = mapped || name == "mapWriteErr"
			return true
		})
		for _, c := range calls {
			c.traced, c.mapped = traced, mapped
			hits = append(hits, c)
		}
	}
	return hits, nil
}

// TestEveryClientGoWriteRecordsWhetherItWasSent asserts that every call of a
// client-go write method, in a non-test Go file of a package that imports a
// client-go client package, is in a function that calls traceWrite and
// mapWriteErr: a write whose response is lost then says the change may have
// been applied. A function listed in writeOutcomeExceptions with its reason
// is the only other case, and every listed function still makes such a call.
// internal/testrancher, the test servers and the fidelity harness, which no
// non-test file outside it imports and which is never part of the binary,
// and testdata trees are not scanned.
func TestEveryClientGoWriteRecordsWhetherItWasSent(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	var sources []string
	for _, path := range files {
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") && !inTestdata(path) && !strings.HasPrefix(path, "internal/testrancher/") {
			sources = append(sources, path)
		}
	}
	packages, err := clientGoPackages(sources)
	require.NoError(t, err)
	var offenders []string
	seen := map[string]bool{}
	for _, path := range sources {
		hits, perr := findClientGoWrites(path, nil, packages[filepath.Dir(path)])
		require.NoError(t, perr, path)
		for _, hit := range hits {
			key := path + " " + hit.function + " " + hit.method
			seen[key] = true
			listed := slices.ContainsFunc(writeOutcomeExceptions, func(l struct{ file, function, method, reason string }) bool {
				return l.file+" "+l.function+" "+l.method == key
			})
			if !listed && (!hit.traced || !hit.mapped) {
				offenders = append(offenders, fmt.Sprintf("%s: %s in %q (traceWrite %t, mapWriteErr %t)", hit.at, hit.method, hit.function, hit.traced, hit.mapped))
			}
		}
	}
	assert.Empty(t, offenders, "a client-go write records whether it was sent and maps its error as a write's, or the function is listed with its reason")
	for _, l := range writeOutcomeExceptions {
		assert.True(t, seen[l.file+" "+l.function+" "+l.method], "%s %s is listed for %s and no longer calls it; remove the entry", l.file, l.function, l.method)
	}
}

// TestTheClientGoWriteScanFindsEveryCall asserts the rule of
// findClientGoWrites on small sources: a write method called in a file that
// imports a client-go client package, or in a package another file of which
// does, is found with whether its function calls traceWrite and
// mapWriteErr; a closure counts for the declaration it is written in; a file
// that imports only a client-go package of types, and a file of a package
// that imports no client package, have no hits.
func TestTheClientGoWriteScanFindsEveryCall(t *testing.T) {
	t.Parallel()
	const imp = "package p\n\nimport \"k8s.io/client-go/dynamic\"\n\nvar _ dynamic.Interface\n\n"
	for _, tc := range []struct {
		name, src string
		inPackage bool
		want      []string
	}{
		{"a traced and mapped create", imp + "func (c *T) Create() error { ctx, w := traceWrite(nil); _, err := c.ri.Create(ctx); return c.mapWriteErr(w, err) }\n", false, []string{"T.Create Create true true"}},
		{"an update with neither", imp + "func (c *T) Update() error { _, err := c.ri.Update(nil); return err }\n", false, []string{"T.Update Update false false"}},
		{"a delete that is only traced", imp + "func (c *T) Delete() error { traceWrite(nil); return c.ri.Delete(nil) }\n", false, []string{"T.Delete Delete true false"}},
		{"a write in a closure", imp + "func f() { g := func() { traceWrite(nil); mapWriteErr(r.Post()) }; g() }\n", false, []string{"f Post true true"}},
		{"an eviction", imp + "func (c *T) evict() error { return c.cs.PolicyV1().Evictions(\"ns\").Evict(nil, nil) }\n", false, []string{"T.evict Evict false false"}},
		{"a request with a verb", "package p\n\nimport \"k8s.io/client-go/rest\"\n\nfunc f(r rest.Interface) { r.Verb(\"POST\").Do(nil) }\n", false, []string{"f Verb false false"}},
		{"a file of a package that imports client-go", "package p\n\nfunc (c *T) Patch() error { return c.ri.Patch() }\n", true, []string{"T.Patch Patch false false"}},
		{"a file that imports only a package of types", "package p\n\nimport clientcmdapi \"k8s.io/client-go/tools/clientcmd/api\"\n\nvar _ clientcmdapi.Config\n\nfunc (c *T) Delete() error { return c.store.Delete() }\n", false, nil},
		{"a file of a package that does not import client-go", "package p\n\nfunc (c *T) Create() error { return c.ri.Create() }\n", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hits, err := findClientGoWrites("p.go", tc.src, tc.inPackage)
			require.NoError(t, err)
			var got []string
			for _, h := range hits {
				got = append(got, fmt.Sprintf("%s %s %t %t", h.function, h.method, h.traced, h.mapped))
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
