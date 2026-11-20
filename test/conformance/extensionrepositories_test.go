package conformance

import (
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

// The one function that may create a ClusterRepo: it reads the table of the
// two Rancher extension repositories and makes the one create call.
const (
	clusterRepoCreatorFile     = "internal/engine/catalog/extensionadd.go"
	clusterRepoCreatorFunction = "addRepositoryCmd.Plan"
	clusterRepoTableFunction   = "RepositoryNamed"
)

// clusterRepoCreateCalls are the names of the calls that create an object.
var clusterRepoCreateCalls = []string{"Create", "PlanCreate", "PlanCreateObserved", "NewWriteRequest"}

// clusterRepoCreateSites returns, for every function of the non-test Go files
// under internal/ that names the ClusterRepo kind (the literal "clusterrepo",
// or any literal that holds "clusterrepos") and calls a create, the file and
// function with the number of create calls and whether it calls
// RepositoryNamed, the read of the table of Rancher extension repositories.
func clusterRepoCreateSites(t *testing.T, files []string, parse func(string) *ast.File) map[string][2]int {
	t.Helper()
	found := map[string][2]int{}
	for _, path := range files {
		if !strings.HasPrefix(path, "internal/") || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.Contains(path, "/testdata/") {
			continue
		}
		file := parse(path)
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			function := fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) == 1 {
				function = createCallReceiver(fd.Recv.List[0].Type) + "." + function
			}
			var namesKind bool
			var creates, reads int
			ast.Inspect(fd, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.BasicLit:
					if v.Kind == token.STRING {
						if s, err := strconv.Unquote(v.Value); err == nil && (s == "clusterrepo" || strings.Contains(s, "clusterrepos")) {
							namesKind = true
						}
					}
				case *ast.CallExpr:
					name := ""
					switch f := v.Fun.(type) {
					case *ast.SelectorExpr:
						name = f.Sel.Name
					case *ast.Ident:
						name = f.Name
					}
					if slices.Contains(clusterRepoCreateCalls, name) {
						creates++
					}
					if name == clusterRepoTableFunction {
						reads++
					}
				}
				return true
			})
			if namesKind && creates > 0 {
				found[filepath.ToSlash(path)+" "+function] = [2]int{creates, reads}
			}
		}
	}
	return found
}

// TestOnlyTheRancherExtensionRepositoriesAreCreated asserts that the only
// function of the production code that names the ClusterRepo kind and creates
// an object is the add-repository plan, that it reads the table of the two
// Rancher extension repositories, and that it makes exactly one create call. A
// second create call in it, or another function that creates a ClusterRepo,
// fails here.
func TestOnlyTheRancherExtensionRepositoriesAreCreated(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	fset := token.NewFileSet()
	found := clusterRepoCreateSites(t, files, func(path string) *ast.File {
		f, perr := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, perr, path)
		return f
	})
	want := clusterRepoCreatorFile + " " + clusterRepoCreatorFunction
	for key, counts := range found {
		assert.Equal(t, want, key, "this function names the ClusterRepo kind and creates an object: this CLI creates only the Rancher extension repositories, in one function that reads their table")
		assert.Equal(t, 1, counts[0], "%s must make exactly one create call", key)
		assert.Positive(t, counts[1], "%s must read the table of Rancher extension repositories", key)
	}
	assert.Contains(t, found, want, "the add-repository plan no longer names the ClusterRepo kind and creates it")
}

// TestClusterRepoCreateScanSeesAnotherCreator asserts the scan reports a
// function that creates a ClusterRepo, and a second create call.
func TestClusterRepoCreateScanSeesAnotherCreator(t *testing.T) {
	t.Parallel()
	src := `package x

func other() { kind("clusterrepo"); client.Create(); engine.PlanCreate() }

func fromResource() { _ = "clusterrepos"; kube.NewWriteRequest() }

func unrelated() { client.Create() }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "internal/x/x.go", src, 0)
	require.NoError(t, err)
	found := clusterRepoCreateSites(t, []string{"internal/x/x.go"}, func(string) *ast.File { return f })
	assert.Equal(t, map[string][2]int{"internal/x/x.go other": {2, 0}, "internal/x/x.go fromResource": {1, 0}}, found)
}
