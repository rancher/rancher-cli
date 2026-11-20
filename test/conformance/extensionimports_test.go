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

// extensionDir is the package of the pure rules for Rancher UI extensions that
// a command and its completion both call.
const extensionDir = "internal/engine/catalog/extension"

// extensionImportAllowlist is every import path the package's production files
// may name: standard library packages that open no socket, read no file and run
// no process (embed reads only what the build compiled in), the Node.js semver
// reader and the structured errors. The complete packages may import the
// package only because it can never reach a client through it.
var extensionImportAllowlist = map[string]bool{
	"cmp": true, "embed": true, "encoding/json": true, "errors": true, "fmt": true, "maps": true, "regexp": true,
	"slices": true, "sort": true, "strconv": true, "strings": true, "sync": true,
	modulePrefix + "internal/clierr":     true,
	modulePrefix + "internal/nodesemver": true,
}

// scanExtensionImports parses the production Go files of the package
// directory dir and returns one message per import outside the allowlist.
func scanExtensionImports(t *testing.T, dir string) []string {
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
			if !extensionImportAllowlist[ip] {
				out = append(out, filepath.ToSlash(p)+": imports "+ip)
			}
		}
	}
	slices.Sort(out)
	return out
}

// TestExtensionImportsOnlyTheStandardLibraryAndTheReaders asserts the
// production files of the extension package import only the allowlisted
// standard library packages, internal/nodesemver and internal/clierr: nothing
// that makes a request, reads the environment or a file, runs a process, or
// reaches a client. The scan reads the direct imports, as the one of the clusterops package does.
func TestExtensionImportsOnlyTheStandardLibraryAndTheReaders(t *testing.T) {
	t.Parallel()
	assert.Empty(t, scanExtensionImports(t, extensionDir))
}

// TestExtensionImportScanSeesAForbiddenImport asserts the scan reports an
// import of a client package and of a network package. Without this test, the
// test above could pass because the scan reports nothing.
func TestExtensionImportScanSeesAForbiddenImport(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := "package extension\n\nimport (\n\t\"net/http\"\n\t\"strings\"\n\n\t\"github.com/rancher/rancher-cli/internal/client/provider\"\n)\n\nvar _ = http.StatusOK\nvar _ = strings.ToLower\nvar _ = provider.EditionPrime\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o600))
	got := scanExtensionImports(t, dir)
	require.Len(t, got, 2)
	assert.Contains(t, got[0], "imports github.com/rancher/rancher-cli/internal/client/provider")
	assert.Contains(t, got[1], "imports net/http")
}
