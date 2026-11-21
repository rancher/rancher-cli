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

// nodesemverDir is the package that reads Node.js semver versions and ranges.
// The extension package imports it, so it must reach no client either.
const nodesemverDir = "internal/nodesemver"

// extensionImportAllowlist is every import path the package's production files
// may name: standard library packages that open no socket, read no file and run
// no process (embed reads only what the build compiled in) and the Node.js
// semver reader. The complete packages may import the package only because it
// can never reach a client through it.
var extensionImportAllowlist = map[string]bool{
	"cmp": true, "embed": true, "encoding/json": true, "fmt": true, "regexp": true,
	"slices": true, "strings": true, "sync": true,
	modulePrefix + "internal/nodesemver": true,
}

// nodesemverImportAllowlist is every import path the Node.js semver reader's
// production files may name: standard library packages that open no socket,
// read no file and run no process.
var nodesemverImportAllowlist = map[string]bool{
	"regexp": true, "strconv": true, "strings": true,
}

// scanImports parses the production Go files of the package directory dir and
// returns one message per import outside the allowlist.
func scanImports(t *testing.T, dir string, allowlist map[string]bool) []string {
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
			if !allowlist[ip] {
				out = append(out, filepath.ToSlash(p)+": imports "+ip)
			}
		}
	}
	slices.Sort(out)
	return out
}

// TestExtensionImportsOnlyTheStandardLibraryAndTheReaders asserts the
// production files of the extension package import only the allowlisted
// standard library packages and internal/nodesemver: nothing that makes a
// request, reads the environment or a file, runs a process, or reaches a
// client. The scan reads the direct imports, as the one of the clusterops
// package does; the test below covers the imports of internal/nodesemver.
func TestExtensionImportsOnlyTheStandardLibraryAndTheReaders(t *testing.T) {
	t.Parallel()
	assert.Empty(t, scanImports(t, extensionDir, extensionImportAllowlist))
}

// TestNodeSemverImportsOnlyTheStandardLibrary asserts the production files of
// internal/nodesemver import only the allowlisted standard library packages.
// The extension package and the complete packages that import it reach
// nothing beyond these through it.
func TestNodeSemverImportsOnlyTheStandardLibrary(t *testing.T) {
	t.Parallel()
	assert.Empty(t, scanImports(t, nodesemverDir, nodesemverImportAllowlist))
}

// TestExtensionImportScanReportsAForbiddenImport asserts the scan reports an
// import of a client package and of a network package. Without this test, the
// test above could pass because the scan reports nothing.
func TestExtensionImportScanReportsAForbiddenImport(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := "package extension\n\nimport (\n\t\"net/http\"\n\t\"strings\"\n\n\t\"github.com/rancher/rancher-cli/internal/client/provider\"\n)\n\nvar _ = http.StatusOK\nvar _ = strings.ToLower\nvar _ = provider.EditionPrime\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o600))
	got := scanImports(t, dir, extensionImportAllowlist)
	require.Len(t, got, 2)
	assert.Contains(t, got[0], "imports github.com/rancher/rancher-cli/internal/client/provider")
	assert.Contains(t, got[1], "imports net/http")
}
