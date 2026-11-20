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

// Rancher's Norman API (/v3) is not part of the Kubernetes API the CLI uses
// everywhere else. The CLI sends a Norman request from two declarations
// only: the cluster token create of internal/token (a Rancher without
// ext.cattle.io tokens) and the cloud credential client, the one port that
// sends a cloud credential write. The scans below fail on a /v3 path anywhere
// else, now or later.

// groupVersionPrefix matches an API group-version, <group>.io/v3, where it
// starts a literal or follows /apis/: the apiVersion of a Kubernetes object
// and a Kubernetes API path, which are not Norman.
var groupVersionPrefix = regexp.MustCompile(`(^|/apis/)[A-Za-z0-9.-]+\.io/v3`)

// holdsNormanPath reports whether the string literal value names a Norman
// path: it holds /v3/ or /v3?, or ends with /v3, once a group-version at the
// start of the literal or after /apis/ is removed. A group-version after
// anything else, such as the host of a URL (https://rancher.example.io/v3/x
// and //x.io/v3/y), is a Norman path. A format string and a full URL match.
// A path built from pieces or read from an answer is not seen.
func holdsNormanPath(value string) bool {
	rest := groupVersionPrefix.ReplaceAllString(value, "$1")
	return strings.Contains(rest, "/v3/") || strings.Contains(rest, "/v3?") || strings.HasSuffix(rest, "/v3")
}

// normanAllowance is the number of Norman path literals a declaration holds
// and why it may.
type normanAllowance struct {
	count int
	why   string
}

// normanPathAllowlist maps "<file>:<declaration>" to the literals it covers.
var normanPathAllowlist = map[string]normanAllowance{
	"internal/token/clustertoken.go:normanTokensPath":            {1, "the Norman /v3/tokens create createV3ClusterToken sends; token code stays unchanged"},
	"internal/cli/cloudcredclient.go:normanCloudCredentialsPath": {1, "the one file that sends Norman /v3/cloudcredentials requests"},
}

// normanPathReferences maps an allowlisted constant, "<file>:<name>", to the
// functions that may name it: a function name of the constant's file, or "*"
// for every function of that file. The constant is a package-level name, so a
// reference comes from the files of its directory.
var normanPathReferences = map[string][]string{
	"internal/token/clustertoken.go:normanTokensPath":            {"createV3ClusterToken"},
	"internal/cli/cloudcredclient.go:normanCloudCredentialsPath": {"*"},
}

// normanMatch is one finding of the scan: key is "<file>:<declaration>".
type normanMatch struct {
	key  string
	text string
}

// declarationName is the name of the declaration of file f that holds pos:
// the first name of a const or var spec, a function's name with its receiver
// type, or "(package level)".
func declarationName(f *ast.File, pos token.Pos) string {
	for _, decl := range f.Decls {
		if pos < decl.Pos() || pos >= decl.End() {
			continue
		}
		switch d := decl.(type) {
		case *ast.FuncDecl:
			return d.Name.Name
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok && pos >= vs.Pos() && pos < vs.End() && len(vs.Names) > 0 {
					return vs.Names[0].Name
				}
			}
		}
	}
	return "(package level)"
}

// scanNormanLiterals returns one match per string literal of f that holds a
// Norman path. The literals of import specs are not read.
func scanNormanLiterals(fset *token.FileSet, filePath string, f *ast.File) []normanMatch {
	imports := map[*ast.BasicLit]bool{}
	for _, imp := range f.Imports {
		imports[imp.Path] = true
	}
	var out []normanMatch
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || imports[lit] {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil || !holdsNormanPath(value) {
			return true
		}
		out = append(out, normanMatch{
			key:  filePath + ":" + declarationName(f, lit.Pos()),
			text: fset.Position(lit.Pos()).String() + ": the literal " + lit.Value + " holds a Norman path",
		})
		return true
	})
	return out
}

// checkNormanMatches returns one message per match outside the allowlist, one
// per allowlist row whose count differs from the matches it covers, and one
// per row that matches nothing.
func checkNormanMatches(matches []normanMatch, allow map[string]normanAllowance) []string {
	byKey := map[string][]string{}
	for _, m := range matches {
		byKey[m.key] = append(byKey[m.key], m.text)
	}
	var out []string
	for _, key := range slices.Sorted(maps.Keys(byKey)) {
		a, ok := allow[key]
		switch {
		case !ok:
			out = append(out, byKey[key]...)
		case len(byKey[key]) != a.count:
			out = append(out, fmt.Sprintf("%s: the allowlist covers %d literal(s) (%s), found %d: %s", key, a.count, a.why, len(byKey[key]), strings.Join(byKey[key], "; ")))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(allow)) {
		if len(byKey[key]) == 0 {
			out = append(out, "allowlist row "+key+" matched nothing: remove it")
		}
	}
	return out
}

// scanNormanReferences returns one message per reference to an allowlisted
// constant from a function the row does not name. files maps a path to its
// parsed file.
func scanNormanReferences(fset *token.FileSet, files map[string]*ast.File, refs map[string][]string) []string {
	var out []string
	for _, key := range slices.Sorted(maps.Keys(refs)) {
		file, name, _ := strings.Cut(key, ":")
		dir := path.Dir(file)
		allowed := refs[key]
		for _, p := range slices.Sorted(maps.Keys(files)) {
			if path.Dir(p) != dir {
				continue
			}
			f := files[p]
			ast.Inspect(f, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok || id.Name != name || isConstDeclaration(f, id) {
					return true
				}
				decl := declarationName(f, id.Pos())
				if p == file && slices.Contains(allowed, "*") && decl != "(package level)" {
					return true
				}
				if p == file && slices.Contains(allowed, decl) {
					return true
				}
				out = append(out, fset.Position(id.Pos()).String()+": "+name+" is named from "+decl+", which the allowlist does not cover")
				return true
			})
		}
	}
	return out
}

// isConstDeclaration reports whether id is a name declared by a const or var
// spec of f.
func isConstDeclaration(f *ast.File, id *ast.Ident) bool {
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gd.Specs {
			if vs, ok := spec.(*ast.ValueSpec); ok && slices.Contains(vs.Names, id) {
				return true
			}
		}
	}
	return false
}

// TestNormanPathsOnlyInTheListedDeclarations asserts a string literal that
// names a Norman path (/v3/, /v3? or a final /v3, apart from the API
// group-versions of Kubernetes objects) appears only in the declarations of
// normanPathAllowlist, each with its number of literals, and that each
// allowlisted constant is named only from the functions its row lists. It
// covers every non-test Go file outside testdata and internal/testrancher,
// every build tag. internal/testrancher, which holds the test Rancher and the
// fidelity recorder, sends Norman requests to imitate Rancher and to create
// fixtures, never on a user's behalf; TestNoProductionPackageImportsTestRancher
// keeps that package out of the CLI.
func TestNormanPathsOnlyInTheListedDeclarations(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	var matches []normanMatch
	files := map[string]*ast.File{}
	for i, f := range tree.files {
		p := tree.paths[i]
		if inTestrancher(p) {
			continue
		}
		files[p] = f
		matches = append(matches, scanNormanLiterals(tree.fset, p, f)...)
	}
	for _, problem := range checkNormanMatches(matches, normanPathAllowlist) {
		t.Error(problem)
	}
	for _, problem := range scanNormanReferences(tree.fset, files, normanPathReferences) {
		t.Error(problem)
	}
	for key, a := range normanPathAllowlist {
		assert.NotEmpty(t, a.why, key)
	}
}

// TestNormanPathRule asserts the literal rule: a group-version at the start
// of a literal or after /apis/ passes, and a Norman path in any other form
// fails, a URL's host that ends in .io included.
func TestNormanPathRule(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]bool{
		"management.cattle.io/v3":                                 false,
		"/k8s/clusters/local/apis/management.cattle.io/v3/tokens": false,
		"management.cattle.io/v3/something":                       false,
		"/v3/cloudcredentials":                                    true,
		"%s/v3/clusters":                                          true,
		"https://h/v3/x":                                          true,
		"https://rancher.example.io/v3/clusters":                  true,
		"//x.io/v3/y":                                             true,
		"/v3?action=x":                                            true,
		"https://rancher.example.io/v3":                           true,
		"/v3-public/authproviders":                                false,
		"/v1/management.cattle.io.clusters":                       false,
		"/k8s/clusters/c-x/apis/management.cattle.io/v3/namespaces/x/foos": false,
	} {
		assert.Equal(t, want, holdsNormanPath(value), value)
	}
}

// normanFixture parses one source for the mutation rows below.
func normanFixture(t *testing.T, files map[string]string) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	for p, src := range files {
		f, err := parser.ParseFile(fset, p, src, parser.AllErrors)
		require.NoError(t, err, p)
		parsed[p] = f
	}
	return fset, parsed
}

// TestNormanPathScanSeesEachForbiddenForm asserts the scan reports a Norman
// literal in another declaration or file, in a format string and in a full
// URL, and a reference to an allowlisted constant from another function, and
// reports none for the allowlisted declarations. Without it the scan above
// could pass because it reports nothing.
func TestNormanPathScanSeesEachForbiddenForm(t *testing.T) {
	t.Parallel()
	clean := map[string]string{
		"internal/token/clustertoken.go": "package token\n\nconst normanTokensPath = \"/v3/tokens\"\n\nfunc createV3ClusterToken() string { return normanTokensPath }\n",
		"internal/cli/cloudcredclient.go": "package cli\n\nconst normanCloudCredentialsPath = \"/v3/cloudcredentials\"\n\n" +
			"func (c *x) Create() string { return normanCloudCredentialsPath }\n\nfunc (c *x) Delete() string { return normanCloudCredentialsPath + \"/a\" }\n",
	}
	scan := func(extra map[string]string) []string {
		sources := maps.Clone(clean)
		maps.Copy(sources, extra)
		fset, files := normanFixture(t, sources)
		var matches []normanMatch
		for _, p := range slices.Sorted(maps.Keys(files)) {
			matches = append(matches, scanNormanLiterals(fset, p, files[p])...)
		}
		out := checkNormanMatches(matches, normanPathAllowlist)
		return append(out, scanNormanReferences(fset, files, normanPathReferences)...)
	}
	assert.Empty(t, scan(nil))

	for name, extra := range map[string]map[string]string{
		"a literal in the command file": {"internal/engine/provisioning/cloudcredwrite.go": "package provisioning\n\nfunc f() string { return \"/v3/cloudcredentials\" }\n"},
		"a path joined to a base":       {"internal/x/x.go": "package x\n\nfunc f(base string) string { return base + \"/v3/clusters\" }\n"},
		"a format string":               {"internal/x/x.go": "package x\n\nimport \"fmt\"\n\nfunc f(base string) string { return fmt.Sprintf(\"%s/v3/clusters\", base) }\n"},
		"a full URL":                    {"internal/x/x.go": "package x\n\nconst u = \"https://rancher.example/v3/clusters\"\n"},
		"a second literal in a row":     {"internal/token/clustertoken.go": "package token\n\nconst normanTokensPath = \"/v3/tokens\"\nconst other = \"/v3/x\"\n\nfunc createV3ClusterToken() string { return normanTokensPath }\n"},
		"a reference from another function": {"internal/token/clustertoken.go": "package token\n\nconst normanTokensPath = \"/v3/tokens\"\n\n" +
			"func createV3ClusterToken() string { return normanTokensPath }\n\nfunc other() string { return normanTokensPath }\n"},
		"a reference from another file of the package": {"internal/token/other.go": "package token\n\nfunc other() string { return normanTokensPath }\n"},
		"a reference at package level of another file": {"internal/token/other.go": "package token\n\nvar v = normanTokensPath\n"},
	} {
		assert.NotEmpty(t, scan(extra), name)
	}

	// A stale row: the file holds no Norman literal.
	fset, files := normanFixture(t, map[string]string{"internal/token/clustertoken.go": "package token\n\nconst normanTokensPath = \"x\"\n"})
	matches := scanNormanLiterals(fset, "internal/token/clustertoken.go", files["internal/token/clustertoken.go"])
	stale := checkNormanMatches(matches, normanPathAllowlist)
	assert.NotEmpty(t, stale)
}

// normanPortCallers lists the files that may call CloudCredentials(), the
// factory method that hands out the Norman client: the command file and the
// file that implements the factory.
var normanPortCallers = []string{
	"internal/engine/provisioning/cloudcredwrite.go",
	"internal/cli/cloudcredclient.go",
}

// scanNormanPortCalls returns one message per call of a method named
// CloudCredentials, with no arguments, in a file outside allowed.
func scanNormanPortCalls(fset *token.FileSet, filePath string, f *ast.File, allowed []string) []string {
	if slices.Contains(allowed, filePath) {
		return nil
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 0 {
			return true
		}
		if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok && sel.Sel.Name == "CloudCredentials" {
			out = append(out, fset.Position(call.Pos()).String()+": a call of CloudCredentials() outside the cloud credential commands")
		}
		return true
	})
	return out
}

// TestOnlyTheCloudCredentialCommandsCallTheNormanPort asserts no production
// file but the cloud credential command file and the factory's own file calls
// CloudCredentials(), the method that hands out the client of Norman
// requests.
func TestOnlyTheCloudCredentialCommandsCallTheNormanPort(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	for i, f := range tree.files {
		if inTestrancher(tree.paths[i]) {
			continue
		}
		for _, problem := range scanNormanPortCalls(tree.fset, tree.paths[i], f, normanPortCallers) {
			t.Error(problem)
		}
	}
}

// TestNormanPortCallScanSeesAForeignCall asserts the call scan reports a call
// from another file and none from the listed files.
func TestNormanPortCallScanSeesAForeignCall(t *testing.T) {
	t.Parallel()
	src := "package p\n\nfunc f(c interface{ CloudCredentials() (int, error) }) { _, _ = c.CloudCredentials() }\n"
	fset, files := normanFixture(t, map[string]string{
		"internal/engine/catalog/x.go":                   src,
		"internal/engine/provisioning/cloudcredwrite.go": src,
	})
	assert.NotEmpty(t, scanNormanPortCalls(fset, "internal/engine/catalog/x.go", files["internal/engine/catalog/x.go"], normanPortCallers))
	assert.Empty(t, scanNormanPortCalls(fset, "internal/engine/provisioning/cloudcredwrite.go", files["internal/engine/provisioning/cloudcredwrite.go"], normanPortCallers))
}
