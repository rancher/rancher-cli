package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Rancher's Norman API (/v3) is outside the CLI's Kubernetes-style surface, and
// each use of it is a reviewed exception. Two scans keep every Norman request
// in the files and functions that carry that review:
//
//   - A string literal, interpreted or raw, that holds "/v3/" or "/v3?" or ends
//     with "/v3" is allowed only in an allowlisted declaration, with the
//     number of matching literals the row names. An API group and version
//     written <group>.io/v3 is not a Norman path: it is removed first where it
//     starts the literal or follows "/apis/", never after "//", the host of a
//     URL. A format string ("%s/v3/clusters") and a full URL
//     ("https://rancher.example.io/v3/clusters") match. A stale row, a count
//     that differs and a literal outside every row fail. Each allowlisted
//     constant may be referenced only from the functions its row names, each
//     once. A path built from pieces ("/v" + "3/") or read from a response is
//     not seen.
//   - A call of NodeActions, the factory method that hands out the node action
//     client, is allowed only in internal/engine/nodes and in the factory's own
//     file.
//
// The scans cover every non-test Go file outside testdata, whatever its build
// tags, and leave out internal/testrancher: the test servers and the fidelity
// recorder send Norman requests to imitate and record Rancher, never on a
// user's behalf, and no production package imports them.

// normanAllowance is one declaration that may hold a Norman path literal: the
// number of literals it holds, the number of references each listed function
// makes to it, why it may, and the functions that may reference it, each
// written "Type.Method" for a method or "name" for a function.
type normanAllowance struct {
	count int
	why   string
	refs  []string
}

// normanPathAllowlist maps "<file>:<declaration>" to the Norman path literal
// it holds.
var normanPathAllowlist = map[string]normanAllowance{
	"internal/token/clustertoken.go:normanTokensPath": {1, "the Norman /v3/tokens create that createV3ClusterToken sends", []string{"createV3ClusterToken"}},
	"internal/cli/nodeactionclient.go:normanNodesPath": {1, "the one file that sends Norman /v3/nodes requests",
		[]string{"nodeActionClient.GetNode", "nodeActionClient.PostNodeAction"}},
}

// groupVersionV3 is an API group and version, <group>.io/v3, where it starts a
// literal or follows "/apis/".
var groupVersionV3 = regexp.MustCompile(`(^|/apis/)[a-z0-9][a-z0-9.-]*\.io/v3`)

// isNormanPath reports whether a literal's value holds a Norman path.
func isNormanPath(value string) bool {
	value = groupVersionV3.ReplaceAllString(value, "$1")
	return strings.Contains(value, "/v3/") || strings.Contains(value, "/v3?") || strings.HasSuffix(value, "/v3")
}

// normanSource is a parsed file and its path relative to the module root.
type normanSource struct {
	path string
	file *ast.File
}

// normanDeclName is the name a declaration goes by in the allowlist: "Type.Method"
// or the name of a function, the first name of a constant or variable spec,
// the name of a type, or "(package level)".
func normanDeclName(decl ast.Decl, spec ast.Spec) string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv != nil && len(d.Recv.List) == 1 {
			typ := d.Recv.List[0].Type
			if star, ok := typ.(*ast.StarExpr); ok {
				typ = star.X
			}
			if id, ok := typ.(*ast.Ident); ok {
				return id.Name + "." + d.Name.Name
			}
		}
		return d.Name.Name
	case *ast.GenDecl:
		switch s := spec.(type) {
		case *ast.ValueSpec:
			return s.Names[0].Name
		case *ast.TypeSpec:
			return s.Name.Name
		}
	}
	return "(package level)"
}

// scanNormanPaths returns one message per Norman path literal outside the
// allowlist, per row whose count differs from its literals, per stale row and
// per reference to an allowlisted constant that its row does not allow.
func scanNormanPaths(fset *token.FileSet, sources []normanSource, allow map[string]normanAllowance) []string {
	found := map[string][]string{}
	for _, src := range sources {
		for _, decl := range src.file.Decls {
			var specs []ast.Spec
			if gd, ok := decl.(*ast.GenDecl); ok {
				if gd.Tok == token.IMPORT {
					continue
				}
				specs = gd.Specs
			} else {
				specs = []ast.Spec{nil}
			}
			for _, spec := range specs {
				var root ast.Node = decl
				if spec != nil {
					root = spec
				}
				key := src.path + ":" + normanDeclName(decl, spec)
				ast.Inspect(root, func(n ast.Node) bool {
					lit, ok := n.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						return true
					}
					if value, err := strconv.Unquote(lit.Value); err == nil && isNormanPath(value) {
						found[key] = append(found[key], fset.Position(lit.Pos()).String()+": the literal "+lit.Value+" holds a Norman path")
					}
					return true
				})
			}
		}
	}
	var out []string
	for _, key := range slices.Sorted(maps.Keys(found)) {
		a, ok := allow[key]
		switch {
		case !ok:
			out = append(out, found[key]...)
		case len(found[key]) != a.count:
			out = append(out, fmt.Sprintf("%s: the allowlist covers %d literal(s) (%s), found %d: %s", key, a.count, a.why, len(found[key]), strings.Join(found[key], "; ")))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(allow)) {
		if len(found[key]) == 0 {
			out = append(out, "allowlist entry "+key+" matched nothing: remove it")
			continue
		}
		out = append(out, normanReferenceProblems(fset, sources, key, allow[key])...)
	}
	return out
}

// normanReferenceProblems checks the references to the constant a row names:
// from the files of its package, only from the functions the row lists, each
// as many times as the row's count says, and every listed function references
// it.
func normanReferenceProblems(fset *token.FileSet, sources []normanSource, key string, a normanAllowance) []string {
	file, name, _ := strings.Cut(key, ":")
	dir := path.Dir(file)
	counts := map[string]int{}
	var out []string
	for _, src := range sources {
		if path.Dir(src.path) != dir {
			continue
		}
		for _, decl := range src.file.Decls {
			where := normanDeclName(decl, nil)
			if gd, ok := decl.(*ast.GenDecl); ok {
				if gd.Tok == token.IMPORT {
					continue
				}
				where = "(package level)"
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok || id.Name != name || isDeclaration(decl, id) {
					return true
				}
				if !slices.Contains(a.refs, where) {
					out = append(out, fset.Position(id.Pos()).String()+": "+name+" is referenced from "+where+", which the allowlist does not name")
					return true
				}
				counts[where]++
				return true
			})
		}
	}
	for _, fn := range a.refs {
		switch got := counts[fn]; {
		case got == 0:
			out = append(out, key+": the allowlist names "+fn+", which does not reference "+name+"; remove it")
		case got != a.count:
			out = append(out, fmt.Sprintf("%s: %s references %s %d times, the allowlist says %d", key, fn, name, got, a.count))
		}
	}
	return out
}

// isDeclaration reports whether id is the name a constant or variable spec of
// decl declares.
func isDeclaration(decl ast.Decl, id *ast.Ident) bool {
	gd, ok := decl.(*ast.GenDecl)
	if !ok {
		return false
	}
	for _, spec := range gd.Specs {
		if vs, ok := spec.(*ast.ValueSpec); ok && slices.Contains(vs.Names, id) {
			return true
		}
	}
	return false
}

func normanSources(tree parsedSourceTree) []normanSource {
	var out []normanSource
	for i, f := range tree.files {
		if inTestrancher(tree.paths[i]) {
			continue
		}
		out = append(out, normanSource{tree.paths[i], f})
	}
	return out
}

// TestNormanPathsOnlyInTheListedDeclarations asserts every Norman path literal
// of the production tree is in an allowlisted declaration, with the allowed
// count, and that each allowlisted constant is referenced only from the
// functions its row names.
func TestNormanPathsOnlyInTheListedDeclarations(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	for _, problem := range scanNormanPaths(tree.fset, normanSources(tree), normanPathAllowlist) {
		t.Error(problem)
	}
	for key, a := range normanPathAllowlist {
		assert.NotEmpty(t, a.why, key)
		assert.NotEmpty(t, a.refs, key)
	}
}

// normanFixtureProblems parses files, a map from path to source, and returns
// the scan's messages with the positions removed.
func normanFixtureProblems(t *testing.T, files map[string]string, allow map[string]normanAllowance) []string {
	t.Helper()
	fset := token.NewFileSet()
	var sources []normanSource
	for _, p := range slices.Sorted(maps.Keys(files)) {
		f := parseSnippetIn(t, fset, p, files[p])
		sources = append(sources, normanSource{p, f})
	}
	var out []string
	for _, m := range scanNormanPaths(fset, sources, allow) {
		out = append(out, positionPrefix.ReplaceAllString(m, ""))
	}
	return out
}

// positionPrefix is the file, line and column that start a message.
var positionPrefix = regexp.MustCompile(`^[^ ]+\.go:\d+:\d+: `)

// parseSnippetIn parses src as the file at filePath into fset.
func parseSnippetIn(t *testing.T, fset *token.FileSet, filePath, src string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(fset, filePath, src, parser.AllErrors)
	require.NoError(t, err)
	return f
}

// TestNormanPathScanFixtures asserts the scan on source snippets. The form of a
// Kubernetes group and version, bare or after /apis/, passes, and so does an
// allowlisted constant used once by each function its row names. Each of a
// path in a file of the nodes package, a format string, a full URL, a literal
// that starts with // and a literal that ends with /v3, a raw string, a
// reference from another function, two references from one function, a row
// whose declaration does not exist, a row whose count differs and a row that
// names a function that does not reference the constant fails.
func TestNormanPathScanFixtures(t *testing.T) {
	t.Parallel()
	const header = "package p\n"
	allow := map[string]normanAllowance{"internal/cli/x.go:normanPath": {1, "why", []string{"client.Get", "client.Post"}}}
	allowedSource := header + `
const normanPath = "/v3/nodes"
type client struct{}
func (c *client) Get() string { return normanPath + "/a" }
func (c *client) Post() string { return normanPath + "/b" }
`
	for _, tc := range []struct {
		name  string
		files map[string]string
		allow map[string]normanAllowance
		want  []string
	}{
		{"the group and version of a Kubernetes API path", map[string]string{"internal/token/revoke.go": header + `const V3TokensPath = "/k8s/clusters/local/apis/management.cattle.io/v3/tokens"`}, nil, nil},
		{"a bare group and version", map[string]string{"internal/engine/a.go": header + `const gv = "management.cattle.io/v3"`}, nil, nil},
		{"a group and version with a path", map[string]string{"internal/engine/a.go": header + `const gv = "management.cattle.io/v3/namespaces/x"`}, nil, nil},
		{"an allowlisted constant used once by each listed function", map[string]string{"internal/cli/x.go": allowedSource}, allow, nil},
		{"a path in the nodes package", map[string]string{"internal/engine/nodes/a.go": header + `const p = "/v3/nodes"`},
			nil, []string{`the literal "/v3/nodes" holds a Norman path`}},
		{"a format string", map[string]string{"internal/engine/a.go": header + `func f(base string) string { return fmt.Sprintf("%s/v3/clusters", base) }`},
			nil, []string{`the literal "%s/v3/clusters" holds a Norman path`}},
		{"a full URL", map[string]string{"internal/engine/a.go": header + `var u = "https://rancher.example.io/v3/clusters"`},
			nil, []string{`the literal "https://rancher.example.io/v3/clusters" holds a Norman path`}},
		{"a literal that starts with two slashes", map[string]string{"internal/engine/a.go": header + `var u = "//x.io/v3/y"`},
			nil, []string{`the literal "//x.io/v3/y" holds a Norman path`}},
		{"a literal that ends with /v3", map[string]string{"internal/engine/a.go": header + `var u = "https://h/v3"`},
			nil, []string{`the literal "https://h/v3" holds a Norman path`}},
		{"a literal with /v3 and a query", map[string]string{"internal/engine/a.go": header + `var u = "/v3?x=1"`},
			nil, []string{`the literal "/v3?x=1" holds a Norman path`}},
		{"a raw string", map[string]string{"internal/engine/a.go": header + "var u = `/v3/nodes`"},
			nil, []string{"the literal `/v3/nodes` holds a Norman path"}},
		{"a reference from another function", map[string]string{"internal/cli/x.go": allowedSource + `func other() string { return normanPath }`}, allow,
			[]string{"normanPath is referenced from other, which the allowlist does not name"}},
		{"two references from one function", map[string]string{"internal/cli/x.go": header + `
const normanPath = "/v3/nodes"
type client struct{}
func (c *client) Get() string { return normanPath + normanPath }
func (c *client) Post() string { return normanPath }
`}, allow, []string{"internal/cli/x.go:normanPath: client.Get references normanPath 2 times, the allowlist says 1"}},
		{"a row whose declaration does not exist", map[string]string{"internal/cli/x.go": header + "const other = 1"}, allow,
			[]string{"allowlist entry internal/cli/x.go:normanPath matched nothing: remove it"}},
		{"a row whose count differs", map[string]string{"internal/cli/x.go": header + `
const normanPath = "/v3/nodes" + "/v3/more"
type client struct{}
func (c *client) Get() string { return normanPath }
func (c *client) Post() string { return normanPath }
`}, allow, []string{"internal/cli/x.go:normanPath: the allowlist covers 1 literal(s) (why), found 2: "}},
		{"a row that names a function that does not reference the constant", map[string]string{"internal/cli/x.go": header + `
const normanPath = "/v3/nodes"
type client struct{}
func (c *client) Get() string { return normanPath }
`}, allow, []string{"internal/cli/x.go:normanPath: the allowlist names client.Post, which does not reference normanPath; remove it"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normanFixtureProblems(t, tc.files, tc.allow)
			if len(tc.want) == 1 && strings.HasSuffix(tc.want[0], ": ") {
				require.Len(t, got, 1)
				assert.True(t, strings.HasPrefix(got[0], tc.want[0]), got[0])
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// nodeActionCallProblems returns one message per use in f of the NodeActions
// factory method outside internal/engine/nodes and the factory's own file.
func nodeActionCallProblems(fset *token.FileSet, filePath string, f *ast.File) []string {
	if path.Dir(filePath) == "internal/engine/nodes" || filePath == "internal/cli/nodeactionclient.go" {
		return nil
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "NodeActions" {
			out = append(out, fset.Position(sel.Pos()).String()+": NodeActions is called outside internal/engine/nodes and the factory's own file")
		}
		return true
	})
	return out
}

// TestOnlyTheNodeCommandsCallTheNodeActionPort asserts no production file
// outside internal/engine/nodes and the factory's own file uses the factory
// method that hands out the node action client.
func TestOnlyTheNodeCommandsCallTheNodeActionPort(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	for i, f := range tree.files {
		for _, problem := range nodeActionCallProblems(tree.fset, tree.paths[i], f) {
			t.Error(problem)
		}
	}
}

// TestNodeActionPortCallFixtures asserts the call rule: a call from a file of
// the catalog package fails, and a call from a file of the nodes package and
// from the factory's own file passes.
func TestNodeActionPortCallFixtures(t *testing.T) {
	t.Parallel()
	const src = "package p\nfunc f(x interface{ NodeActions() }) { x.NodeActions() }\n"
	for _, tc := range []struct {
		path string
		want int
	}{
		{"internal/engine/catalog/a.go", 1},
		{"internal/engine/nodes/a.go", 0},
		{"internal/cli/nodeactionclient.go", 0},
		{"internal/cli/other.go", 1},
	} {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			f := parseSnippetIn(t, fset, tc.path, src)
			assert.Len(t, nodeActionCallProblems(fset, tc.path, f), tc.want)
		})
	}
}
