package conformance

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testOnlyModules are modules the tests may use and production code must
// not: a release binary that links one carries its code and its CVEs.
// gjson reads JSON paths in the e2e script commands (test/e2e).
var testOnlyModules = []string{
	"github.com/tidwall/gjson",
}

// TestProductionCodeImportsNoTestOnlyModule asserts that no non-test Go
// file in the production trees imports a package of a test-only module.
func TestProductionCodeImportsNoTestOnlyModule(t *testing.T) {
	t.Parallel()
	for pkg := range packagesOnDisk(t) {
		entries, err := os.ReadDir(pkg)
		require.NoError(t, err)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(pkg, e.Name())
			src, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Empty(t, testOnlyImports(t, path, src), "%s imports a test-only module", filepath.ToSlash(path))
		}
	}
}

// TestTestOnlyImportScanFlagsAnImport asserts that the scan reports an
// import of a test-only module, and of a package inside one, also when the
// path is a raw string, and nothing for a module that only shares a prefix
// with it.
func TestTestOnlyImportScanFlagsAnImport(t *testing.T) {
	t.Parallel()
	src := []byte(`package p

import (
	"github.com/tidwall/gjson"
	sub "github.com/tidwall/gjson/sub"
	"github.com/tidwall/gjsonx"
	raw ` + "`github.com/tidwall/gjson/raw`" + `
)
`)
	assert.Equal(t, []string{"github.com/tidwall/gjson", "github.com/tidwall/gjson/sub", "github.com/tidwall/gjson/raw"}, testOnlyImports(t, "p.go", src))
}

// testOnlyImports returns the imports in src that belong to a module in
// testOnlyModules.
func testOnlyImports(t *testing.T, name string, src []byte) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly)
	require.NoError(t, err)
	var found []string
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		require.NoError(t, err)
		for _, m := range testOnlyModules {
			if p == m || strings.HasPrefix(p, m+"/") {
				found = append(found, p)
			}
		}
	}
	return found
}
