package telemetry

import (
	"go/ast"
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

// bannedOSNames are the os package names no file but fsroot.go may use: the
// path functions, os.Root and OpenRoot, and os.File. Constants such as
// os.O_EXCL are fine.
var bannedOSNames = []string{
	"ReadFile", "Open", "OpenFile", "Stat", "Lstat", "ReadDir", "Mkdir", "MkdirAll", "Remove",
	"RemoveAll", "Rename", "Chtimes", "WriteFile", "Create", "CreateTemp", "MkdirTemp",
	"OpenRoot", "Root", "File", "Truncate", "Symlink", "Link", "Readlink", "Chmod", "Chown",
}

// wrapperFindings returns one line per use, in the given sources (file name
// to text), of an os path function, os.Root, os.File, a call of FS, or the
// wrapper's inner field. Only the wrapper file may do those.
func wrapperFindings(t *testing.T, sources map[string]string) []string {
	t.Helper()
	fset := token.NewFileSet()
	var out []string
	for name, src := range sources {
		f, err := parser.ParseFile(fset, name, src, 0)
		require.NoError(t, err)
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pos := fset.Position(sel.Pos()).String()
			switch x := sel.X.(type) {
			case *ast.Ident:
				if x.Name == "os" && slices.Contains(bannedOSNames, sel.Sel.Name) {
					out = append(out, pos+": os."+sel.Sel.Name)
				}
			}
			switch sel.Sel.Name {
			case "FS":
				out = append(out, pos+": FS")
			case "inner":
				out = append(out, pos+": inner")
			}
			return true
		})
	}
	slices.Sort(out)
	return out
}

// TestOnlyTheWrapperTouchesTheFileSystem asserts no production file of the
// package outside fsroot.go calls an os path function, uses os.Root or
// os.File, calls FS, or reaches the wrapper's inner root; every file
// operation is seen by the observer.
func TestOnlyTheWrapperTouchesTheFileSystem(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	sources := map[string]string{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") || n == "fsroot.go" {
			continue
		}
		data, err := os.ReadFile(filepath.Clean(n))
		require.NoError(t, err)
		sources[n] = string(data)
	}
	require.NotEmpty(t, sources)
	assert.Empty(t, wrapperFindings(t, sources))
}

// TestWrapperScanCatchesFixtures asserts the scan reports a direct call on
// the inner root, a read of a path with the os package, an os.OpenRoot, and a
// call of FS, and accepts a file that only uses constants.
func TestWrapperScanCatchesFixtures(t *testing.T) {
	cases := map[string]string{
		"inner call":   "package p\nfunc f(s *S) { _, _ = s.tel.inner.Stat(\"x\") }\n",
		"os.ReadFile":  "package p\nimport \"os\"\nfunc f() { _, _ = os.ReadFile(\"telemetry/config-directory-id\") }\n",
		"os.OpenRoot":  "package p\nimport \"os\"\nfunc f() { _, _ = os.OpenRoot(\".\") }\n",
		"FS call":      "package p\nfunc f(r R) { _ = r.FS() }\n",
		"os.File type": "package p\nimport \"os\"\nvar x *os.File\n",
	}
	for name, src := range cases {
		assert.NotEmpty(t, wrapperFindings(t, map[string]string{"x.go": src}), name)
	}
	assert.Empty(t, wrapperFindings(t, map[string]string{"x.go": "package p\nimport \"os\"\nvar f = os.O_EXCL | os.O_CREATE\n"}))
}
