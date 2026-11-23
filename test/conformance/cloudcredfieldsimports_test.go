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

// cloudcredfieldsDir is the package of the pure rules for the fields of a
// cloud credential that the create command and its completion both call.
const cloudcredfieldsDir = "internal/engine/provisioning/cloudcredfields"

// cloudcredfieldsImportAllowlist is every import path the package's
// production files may name: standard library packages that open no socket,
// read no file and run no process. A complete package may import the package
// only because it can never reach a client through it.
var cloudcredfieldsImportAllowlist = map[string]bool{
	"bytes": true, "cmp": true, "encoding/json": true, "errors": true, "fmt": true, "maps": true,
	"slices": true, "sort": true, "strconv": true, "strings": true, "unicode": true,
}

// scanCloudcredfieldsImports parses the production Go files of the package
// directory dir and returns one message per import outside the allowlist.
func scanCloudcredfieldsImports(t *testing.T, dir string) []string {
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
			if !cloudcredfieldsImportAllowlist[ip] {
				out = append(out, filepath.ToSlash(p)+": imports "+ip)
			}
		}
	}
	slices.Sort(out)
	return out
}

// TestCloudCredFieldsImportsOnlyTheAllowlist asserts the production files of
// the cloudcredfields package import only the allowlisted standard library
// packages: nothing that makes a request, reads the environment or a file,
// runs a process, or reaches a client.
func TestCloudCredFieldsImportsOnlyTheAllowlist(t *testing.T) {
	t.Parallel()
	assert.Empty(t, scanCloudcredfieldsImports(t, cloudcredfieldsDir))
}

// TestCloudCredFieldsImportScanReportsAForbiddenImport asserts the scan reports an
// import of a network package and of an internal package. Without this test,
// the test above could pass because the scan reports nothing.
func TestCloudCredFieldsImportScanReportsAForbiddenImport(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := "package cloudcredfields\n\nimport (\n\t\"net/http\"\n\t\"strings\"\n\n\t\"github.com/rancher/rancher-cli/internal/engine\"\n)\n\nvar _ = http.StatusOK\nvar _ = strings.ToLower\nvar _ = engine.LocalCluster\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a_test.go"), []byte("package cloudcredfields\n\nimport \"os\"\n\nvar _ = os.Args\n"), 0o600))
	got := scanCloudcredfieldsImports(t, dir)
	require.Len(t, got, 2)
	assert.Contains(t, got[0], "imports github.com/rancher/rancher-cli/internal/engine")
	assert.Contains(t, got[1], "imports net/http")
}
