package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every HTTP client in production code is built by internal/client/httpx
// (net/http) or internal/client/kube (client-go), so one redirect policy and
// one trust decision cover every request. The scanner below rejects any
// other construction path: composite literals, new, zero-value declarations
// and non-pointer type uses of http.Client, the package-level default client
// and helpers, client-go's constructor-shaped functions, the client-go
// transport package, and dot imports of any of these packages (a dot import
// hides the selector the other checks rely on). Pointer-typed parameters,
// fields, and variables stay legal: they are the dependency-injection seam.

const netHTTPPath = "net/http"

var clientGoPaths = map[string]bool{
	"k8s.io/client-go/rest":       true,
	"k8s.io/client-go/dynamic":    true,
	"k8s.io/client-go/discovery":  true,
	"k8s.io/client-go/kubernetes": true,
}

const clientGoTransportPath = "k8s.io/client-go/transport"

var forbiddenHTTPSelectors = map[string]bool{
	"DefaultClient": true, "Get": true, "Post": true, "Head": true, "PostForm": true,
}

// netHTTPAllowlist may construct net/http clients. The Vault credential
// helper talks to Vault, not Rancher, so the Rancher redirect policy is not
// its concern.
var netHTTPAllowlist = map[string]bool{
	"internal/client/httpx/httpx.go":        true,
	"cmd/rancher-credential-vault/vault.go": true,
}

func clientGoAllowed(path string) bool { return strings.HasPrefix(path, "internal/client/kube/") }

// scanClientConstruction returns one message per violation in f.
func scanClientConstruction(fset *token.FileSet, path string, f *ast.File) []string {
	var out []string
	// Deduplicated by position: the type walker and ast.Inspect can both
	// reach the same field or type expression, and one violation must read
	// as one.
	seen := map[token.Pos]bool{}
	report := func(pos token.Pos, msg string) {
		if seen[pos] {
			return
		}
		seen[pos] = true
		out = append(out, fset.Position(pos).String()+": "+msg)
	}

	// Resolve import names to paths; a dot import is a violation on its own.
	names := map[string]string{}
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		name := ""
		if imp.Name != nil {
			name = imp.Name.Name
		} else {
			name = p[strings.LastIndex(p, "/")+1:]
		}
		protected := p == netHTTPPath || clientGoPaths[p] || p == clientGoTransportPath
		if name == "." && protected {
			report(imp.Pos(), "dot import of "+p+" hides client construction from the scan")
			continue
		}
		if p == clientGoTransportPath && !clientGoAllowed(path) {
			report(imp.Pos(), "import of "+p+" builds a policy-free client; use kube.HTTPClient")
		}
		if name != "_" {
			names[name] = p
		}
	}
	pkgOf := func(x ast.Expr) string {
		id, ok := x.(*ast.Ident)
		if !ok {
			return ""
		}
		return names[id.Name]
	}
	isHTTPClientType := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "Client" && pkgOf(sel.X) == netHTTPPath
	}

	// reachesConcreteClient walks a type expression and reports whether it
	// reaches net/http.Client without passing through a pointer.
	var reachesConcreteClient func(e ast.Expr) bool
	reachesConcreteClient = func(e ast.Expr) bool {
		switch t := e.(type) {
		case *ast.StarExpr:
			// *http.Client is the dependency-injection seam and is allowed;
			// a pointer to an aggregate of concrete clients is not.
			if isHTTPClientType(t.X) {
				return false
			}
			return reachesConcreteClient(t.X)
		case *ast.SelectorExpr:
			return isHTTPClientType(t)
		case *ast.IndexExpr:
			// A generic instantiation: zero[http.Client]() constructs a
			// concrete client through the type argument.
			return reachesConcreteClient(t.X) || reachesConcreteClient(t.Index)
		case *ast.IndexListExpr:
			return reachesConcreteClient(t.X) || slices.ContainsFunc(t.Indices, reachesConcreteClient)
		case *ast.ArrayType:
			return reachesConcreteClient(t.Elt)
		case *ast.MapType:
			return reachesConcreteClient(t.Key) || reachesConcreteClient(t.Value)
		case *ast.ChanType:
			return reachesConcreteClient(t.Value)
		case *ast.ParenExpr:
			return reachesConcreteClient(t.X)
		case *ast.Ellipsis:
			return reachesConcreteClient(t.Elt)
			// Struct and func types are not walked here: ast.Inspect visits their
			// inner Field nodes itself, and the Field rule reports each offending
			// field or parameter once, at its own position.
		}
		return false
	}

	httpAllowed := netHTTPAllowlist[path]
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			// The full type: []http.Client{{}} and map[string]http.Client{}
			// construct concrete clients as surely as http.Client{}.
			if !httpAllowed && x.Type != nil && reachesConcreteClient(x.Type) {
				report(x.Pos(), "composite literal constructing http.Client; build clients with httpx.New or httpx.NewExternal")
			}
		case *ast.CallExpr:
			// Only the predeclared make/new construct values from a type
			// argument; a user function named new is a different identifier
			// resolution and is not inspected here.
			if id, ok := x.Fun.(*ast.Ident); ok && (id.Name == "new" || id.Name == "make") && len(x.Args) >= 1 && !httpAllowed && reachesConcreteClient(x.Args[0]) {
				report(x.Pos(), id.Name+" of a concrete http.Client; build clients with httpx.New or httpx.NewExternal")
			}
		case *ast.ValueSpec:
			if !httpAllowed && x.Type != nil && reachesConcreteClient(x.Type) {
				report(x.Pos(), "zero-value http.Client; hold a *http.Client built by httpx")
			}
		case *ast.Field:
			if !httpAllowed && reachesConcreteClient(x.Type) {
				report(x.Pos(), "by-value http.Client in a field or parameter; use *http.Client")
			}
		case *ast.TypeSpec:
			if !httpAllowed && reachesConcreteClient(x.Type) {
				report(x.Pos(), "type over concrete http.Client; use *http.Client")
			}
		case *ast.IndexExpr, *ast.IndexListExpr:
			// Generic instantiations in expression position (a call's Fun or
			// a bare instantiation) carry type arguments no other arm sees.
			if !httpAllowed && reachesConcreteClient(n.(ast.Expr)) {
				report(n.Pos(), "generic instantiation over concrete http.Client; use *http.Client")
			}
		case *ast.SelectorExpr:
			pkg := pkgOf(x.X)
			switch {
			case pkg == netHTTPPath && !httpAllowed && forbiddenHTTPSelectors[x.Sel.Name]:
				report(x.Pos(), "http."+x.Sel.Name+" uses the default client; build clients with httpx")
			case clientGoPaths[pkg] && !clientGoAllowed(path) && (strings.HasPrefix(x.Sel.Name, "New") || strings.Contains(x.Sel.Name, "For")):
				report(x.Pos(), pkg+"."+x.Sel.Name+" builds a client-go client outside kube; use kube.Dynamic/Discovery/Clientset")
			}
		}
		return true
	})
	return out
}

func scanSnippet(t *testing.T, path, src string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.AllErrors)
	require.NoError(t, err)
	return scanClientConstruction(fset, path, f)
}

// TestClientScanFixtures asserts the scanner's shape rules from source
// snippets, so the whole-tree test below and this table cannot drift.
func TestClientScanFixtures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, src string
		reject    bool
	}{
		{"zero value var", `package p; import "net/http"; var c http.Client`, true},
		{"make slice", `package p; import "net/http"; var _ = make([]http.Client, 1)`, true},
		{"new array", `package p; import "net/http"; var _ = new([1]http.Client)`, true},
		{"slice literal", `package p; import "net/http"; var _ = []http.Client{{}}`, true},
		{"map literal", `package p; import "net/http"; var _ = map[string]http.Client{}`, true},
		{"generic instantiation", `package p; import "net/http"; func zero[T any]() T { var v T; return v }; var _ = zero[http.Client]()`, true},
		{"pointer to aggregate", `package p; import "net/http"; var _ = new(*[1]http.Client)`, true},
		{"struct field", `package p; import "net/http"; type holder struct{ client http.Client }`, true},
		{"by-value param", `package p; import "net/http"; func f(c http.Client) {}`, true},
		{"alias", `package p; import "net/http"; type localClient = http.Client`, true},
		{"new", `package p; import "net/http"; var _ = new(http.Client)`, true},
		{"literal", `package p; import "net/http"; var _ = &http.Client{}`, true},
		{"aliased import literal", `package p; import h "net/http"; var _ = h.Client{}`, true},
		{"default client", `package p; import "net/http"; var _ = http.DefaultClient`, true},
		{"http.Get", `package p; import "net/http"; func f() { _, _ = http.Get("x") }`, true},
		{"dot import net/http", `package p; import . "net/http"; var _ = DefaultClient`, true},
		{"dot import rest", `package p; import . "k8s.io/client-go/rest"; var _ = RESTClientFor`, true},
		{"rest.RESTClientFor", `package p; import "k8s.io/client-go/rest"; var _ = rest.RESTClientFor`, true},
		{"rest.HTTPClientFor", `package p; import "k8s.io/client-go/rest"; var _ = rest.HTTPClientFor`, true},
		{"dynamic.NewForConfig", `package p; import "k8s.io/client-go/dynamic"; var _ = dynamic.NewForConfig`, true},
		{"discovery.NewDiscoveryClientForConfig", `package p; import "k8s.io/client-go/discovery"; var _ = discovery.NewDiscoveryClientForConfig`, true},
		{"discovery.NewDiscoveryClientForConfigAndClient", `package p; import "k8s.io/client-go/discovery"; var _ = discovery.NewDiscoveryClientForConfigAndClient`, true},
		{"kubernetes.NewForConfig", `package p; import "k8s.io/client-go/kubernetes"; var _ = kubernetes.NewForConfig`, true},
		{"kubernetes.NewForConfigAndClient", `package p; import "k8s.io/client-go/kubernetes"; var _ = kubernetes.NewForConfigAndClient`, true},
		{"dynamic.NewForConfigAndClient", `package p; import "k8s.io/client-go/dynamic"; var _ = dynamic.NewForConfigAndClient`, true},
		{"transport import", `package p; import _ "k8s.io/client-go/transport"`, true},
		{"pointer param", `package p; import "net/http"; func f(c *http.Client) {}`, false},
		{"pointer field", `package p; import "net/http"; type S struct{ client *http.Client }`, false},
		{"pointer slice", `package p; import "net/http"; var _ = make([]*http.Client, 1)`, false},
		{"pointer array new", `package p; import "net/http"; var _ = new([1]*http.Client)`, false},
		{"pointer slice literal", `package p; import "net/http"; var _ = []*http.Client{nil}`, false},
		{"generic pointer instantiation", `package p; import "net/http"; func zero[T any]() T { var v T; return v }; var _ = zero[*http.Client]()`, false},
		{"pointer to pointer aggregate", `package p; import "net/http"; var _ = new(*[1]*http.Client)`, false},
		{"pointer alias", `package p; import "net/http"; type localClient = *http.Client`, false},
		{"rest.Config type", `package p; import "k8s.io/client-go/rest"; var _ *rest.Config`, false},
		{"dynamic.Interface type", `package p; import "k8s.io/client-go/dynamic"; var _ dynamic.Interface`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := scanSnippet(t, "internal/example/x.go", tc.src)
			if tc.reject {
				assert.NotEmpty(t, got, "expected a violation")
			} else {
				assert.Empty(t, got, "expected no violation, got %v", got)
			}
		})
	}
}

// TestClientScanAllowlistIsExact asserts that the allowlisted files may build
// clients and nothing else in their packages may.
func TestClientScanAllowlistIsExact(t *testing.T) {
	t.Parallel()
	src := `package p; import "net/http"; var _ = &http.Client{}`
	assert.Empty(t, scanSnippet(t, "internal/client/httpx/httpx.go", src))
	assert.NotEmpty(t, scanSnippet(t, "internal/client/httpx/other.go", src))
	assert.Empty(t, scanSnippet(t, "cmd/rancher-credential-vault/vault.go", `package p; import "net/http"; var _ = http.DefaultClient`))
	kube := `package p; import "k8s.io/client-go/rest"; var _ = rest.HTTPClientFor`
	assert.Empty(t, scanSnippet(t, "internal/client/kube/restconfig.go", kube))
	assert.NotEmpty(t, scanSnippet(t, "internal/client/provider/x.go", kube))
}

// TestNoClientConstructionOutsideHTTPXAndKube runs the same scanner over
// every production file in the module.
func TestNoClientConstructionOutsideHTTPXAndKube(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	var offenders []string
	for i, f := range tree.files {
		offenders = append(offenders, scanClientConstruction(tree.fset, tree.paths[i], f)...)
	}
	for _, o := range offenders {
		t.Error(o)
	}
}

// TestClientScanReportsOneViolationPerSite asserts that a single offending
// site is reported once even though more than one rule can reach it.
func TestClientScanReportsOneViolationPerSite(t *testing.T) {
	t.Parallel()
	got := scanSnippet(t, "internal/example/x.go", `package p; import "net/http"; type holder struct{ client http.Client }`)
	assert.Len(t, got, 1, "got %v", got)
}
