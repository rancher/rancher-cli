package conformance

import (
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// urfaveModule is the module path of the command-line library the CLI does not
// use, at any major version.
const urfaveModule = "github.com/urfave/cli"

// isUrfavePath reports whether p is the module path or a package path below
// it, at any major version.
func isUrfavePath(p string) bool {
	return p == urfaveModule || strings.HasPrefix(p, urfaveModule+"/")
}

// goModNamesUrfave reports whether the text of a go.mod names the library in
// a requirement, a replacement or an exclusion.
func goModNamesUrfave(data []byte) bool {
	for line := range strings.Lines(string(data)) {
		line, _, _ = strings.Cut(line, "//")
		if slices.ContainsFunc(strings.Fields(line), isUrfavePath) {
			return true
		}
	}
	return false
}

// TestNoFileImportsUrfave asserts that no Go file of the repository, test and
// testdata files included, imports the command-line library the CLI replaced,
// and that go.mod does not require it.
func TestNoFileImportsUrfave(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	fset := token.NewFileSet()
	scanned := 0
	var offenders []string
	for _, path := range files {
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		require.NoError(t, perr, path)
		scanned++
		for _, imp := range file.Imports {
			if p, uerr := strconv.Unquote(imp.Path.Value); uerr == nil && isUrfavePath(p) {
				offenders = append(offenders, fset.Position(imp.Pos()).String()+": imports "+p)
			}
		}
	}
	assert.Positive(t, scanned, "the scan found no Go file")
	assert.Empty(t, offenders, "no file imports the command-line library")

	goMod, err := os.ReadFile("go.mod")
	require.NoError(t, err)
	assert.False(t, goModNamesUrfave(goMod), "go.mod names the command-line library")
}

// TestGoModNamesUrfaveReadsEveryFormOfALine asserts the go.mod check finds
// the library in a requirement of either form, a replacement and an
// exclusion, at any major version, and finds nothing in a comment, in another
// module whose path starts with the same characters, or in a file without it.
func TestGoModNamesUrfaveReadsEveryFormOfALine(t *testing.T) {
	t.Parallel()
	const header = "module example.test/m\n\ngo 1.27\n\n"
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"a requirement", header + "require github.com/urfave/cli/v3 v3.14.0\n", true},
		{"a requirement in a block", header + "require (\n\tgithub.com/stretchr/testify v1.11.1\n\tgithub.com/urfave/cli/v3 v3.14.0\n)\n", true},
		{"another major version", header + "require github.com/urfave/cli/v2 v2.27.0\n", true},
		{"the first major version", header + "require github.com/urfave/cli v1.22.0\n", true},
		{"an indirect requirement", header + "require github.com/urfave/cli/v3 v3.14.0 // indirect\n", true},
		{"a replacement", header + "replace github.com/urfave/cli/v3 => ../cli\n", true},
		{"an exclusion", header + "exclude github.com/urfave/cli/v3 v3.0.0\n", true},
		{"a comment", header + "// the CLI no longer uses github.com/urfave/cli/v3\n", false},
		{"another module with the same start", header + "require github.com/urfave/cligen v1.0.0\n", false},
		{"no requirement", header + "require github.com/stretchr/testify v1.11.1\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, goModNamesUrfave([]byte(tc.text)))
		})
	}
}
