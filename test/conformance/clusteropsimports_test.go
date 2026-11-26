package conformance

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clusteropsDir is the package of the pure checks that a command and its
// completion both call.
const clusteropsDir = "internal/engine/provisioning/clusterops"

// clusteropsImportAllowlist is every import path the package's production
// files may name: standard library packages that open no socket, read no file
// and run no process, and the structured errors. A complete package may import
// the package only because it can never reach a client through it.
var clusteropsImportAllowlist = map[string]bool{
	"bytes": true, "cmp": true, "compress/gzip": true, "encoding/base64": true, "encoding/json": true, "errors": true, "fmt": true, "io": true, "maps": true, "slices": true, "sort": true, "strconv": true, "strings": true, "time": true,
	modulePrefix + "internal/clierr": true,
}

// scanClusteropsImports parses the production Go files of the package
// directory dir and returns one message per import outside the allowlist.
func scanClusteropsImports(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	fset := token.NewFileSet()
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		require.NoError(t, err)
		for _, imp := range f.Imports {
			ip := strings.Trim(imp.Path.Value, `"`)
			if !clusteropsImportAllowlist[ip] {
				out = append(out, filepath.ToSlash(p)+": imports "+ip)
			}
		}
	}
	slices.Sort(out)
	return out
}

// TestClusteropsImportsOnlyTheStandardLibraryAndClierr asserts the
// production files of the clusterops package import only the allowlisted
// standard library packages and internal/clierr: nothing that makes a
// request, reads the environment or a file, runs a process, or reaches a
// client. Its direct imports are read, as the complete packages' are.
func TestClusteropsImportsOnlyTheStandardLibraryAndClierr(t *testing.T) {
	t.Parallel()
	assert.Empty(t, scanClusteropsImports(t, clusteropsDir))
}

// TestClusteropsImportScanSeesAForbiddenImport asserts the scan reports an
// import of a network package and of an internal package outside the
// allowlist. Without this test, the test above could pass because the scan
// reports nothing.
func TestClusteropsImportScanSeesAForbiddenImport(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := "package clusterops\n\nimport (\n\t\"net/http\"\n\t\"strings\"\n\n\t\"github.com/rancher/rancher-cli/internal/engine\"\n)\n\nvar _ = http.StatusOK\nvar _ = strings.ToLower\nvar _ = engine.LocalCluster\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a_test.go"), []byte("package clusterops\n\nimport \"os\"\n\nvar _ = os.Args\n"), 0o600))
	got := scanClusteropsImports(t, dir)
	require.Len(t, got, 2)
	assert.Contains(t, got[0], "imports github.com/rancher/rancher-cli/internal/engine")
	assert.Contains(t, got[1], "imports net/http")
}
