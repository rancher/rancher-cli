package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// shellHelperFile is the one completions test file that may look up a
// shell or skip a test: it holds requireShell and shellMissing, which skip
// a missing shell locally and fail the test when CI is set.
const shellHelperFile = "internal/cli/completions/shell_test.go"

// completionTestFiles parses every test file in internal/cli/completions
// except the helper, from the files git sees, so a new file is scanned too.
func completionTestFiles(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	files, err := repositoryFiles()
	require.NoError(t, err)
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	for _, path := range files {
		if !strings.HasPrefix(path, "internal/cli/completions/") || !strings.HasSuffix(path, "_test.go") || path == shellHelperFile {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err)
		parsed[path] = file
	}
	require.NotEmpty(t, parsed, "no completions test files found")
	return fset, parsed
}

// execImportName returns the name a file gives the os/exec package: "exec"
// unless the import is renamed, "." for a dot import, and "" when the file
// does not import it (or imports it only as _).
func execImportName(file *ast.File) string {
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != "os/exec" {
			continue
		}
		switch {
		case spec.Name == nil:
			return "exec"
		case spec.Name.Name == "_":
			return ""
		default:
			return spec.Name.Name
		}
	}
	return ""
}

// TestCompletionTestsFindShellsOnlyThroughTheHelper asserts that no test
// file in internal/cli/completions refers to LookPath of os/exec - called,
// aliased, dot-imported or held as a value - outside the shared helper. A
// direct lookup followed by a skip passes silently on a machine without the
// shell, so a shell's tests run nowhere.
func TestCompletionTestsFindShellsOnlyThroughTheHelper(t *testing.T) {
	t.Parallel()
	fset, files := completionTestFiles(t)
	var offenders []string
	for path, file := range files {
		name := execImportName(file)
		if name == "" {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name == name && n.Sel.Name == "LookPath" {
					offenders = append(offenders, path+":"+strconv.Itoa(fset.Position(n.Pos()).Line))
				}
			case *ast.Ident:
				if name == "." && n.Name == "LookPath" {
					offenders = append(offenders, path+":"+strconv.Itoa(fset.Position(n.Pos()).Line))
				}
			}
			return true
		})
	}
	for _, o := range offenders {
		t.Errorf("find a shell with requireShell (internal/cli/completions/shell_test.go), not exec.LookPath: %s", o)
	}
}

// TestCompletionTestsSkipOnlyThroughTheHelper asserts that no test file in
// internal/cli/completions calls or refers to Skip, Skipf or SkipNow
// outside the shared helper. A test that finds its shell missing some
// other way (running the shell, running which, stat of its path) and then
// skips is refused here, and a skip that has nothing to do with a shell
// would pass unseen in CI. A test that returns early without skipping is
// not caught by any scan; the CI step's count of tests per shell is the
// check for that.
func TestCompletionTestsSkipOnlyThroughTheHelper(t *testing.T) {
	t.Parallel()
	fset, files := completionTestFiles(t)
	var offenders []string
	for path, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "Skip", "Skipf", "SkipNow":
				offenders = append(offenders, path+":"+strconv.Itoa(fset.Position(sel.Pos()).Line))
			}
			return true
		})
	}
	for _, o := range offenders {
		t.Errorf("skip a shell test with requireShell or shellMissing (internal/cli/completions/shell_test.go), not a direct Skip: %s", o)
	}
}
