package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// On Windows, os.Rename (MoveFileEx) and os.Remove (DeleteFile) fail while
// any other handle holds the file open without FILE_SHARE_DELETE, and
// os.Open, os.OpenFile and os.ReadFile open without it, so each reader of
// a config-directory file blocks its replacement and its removal.
// internal/config renames, removes and reads through renameFile,
// removeFile, removeAll, readFile and openFile, which retry on Windows
// while another handle blocks the call. The scan below rejects any other
// reference in the package's production code to the functions that open,
// replace or delete a file by name - in os, and in the system call
// packages beneath it - so a new write, removal or read cannot skip the
// retry. os.Root's methods are not listed: they open with
// FILE_SHARE_DELETE and replace with POSIX semantics.
//
// Scope: internal/config's own files only. A package outside it that
// reads a config-directory file goes through an exported read of this
// package (config.ReadRawConfig) by convention, not by this scan.

const configPackageDir = "internal/config/"

// systemFileCalls are the system call wrappers that open, rename or
// delete a file by name, by their name in syscall, golang.org/x/sys/windows
// and golang.org/x/sys/unix (a name a package does not define never
// matches, so one list serves all three).
var systemFileCalls = map[string]bool{
	"Rename": true, "Open": true, "Unlink": true, "MoveFile": true,
	"MoveFileEx": true, "CreateFile": true, "DeleteFile": true, "ReplaceFile": true,
}

// configReplaceFuncs lists, per import path, the functions the scan
// rejects. os.DirFS is listed because its Open and ReadFile call os.Open
// and os.ReadFile; os.WriteFile and os.Create because they open the file
// in place.
var configReplaceFuncs = map[string]map[string]bool{
	"os": {
		"Rename": true, "ReadFile": true, "Open": true, "OpenFile": true,
		"Remove": true, "RemoveAll": true, "DirFS": true, "WriteFile": true, "Create": true,
	},
	"syscall":                  systemFileCalls,
	"golang.org/x/sys/windows": systemFileCalls,
	"golang.org/x/sys/unix":    systemFileCalls,
}

// configRetryHelpers are the retrying helpers, defined once per platform.
var configRetryHelpers = map[string]bool{
	"renameFile": true, "readFile": true, "openFile": true, "removeFile": true, "removeAll": true,
}

// configReplaceAllowlist names, per file, the functions that may refer to
// configReplaceFuncs: the retrying helpers themselves, and ResolveCA,
// which reads a CA file the user names and this package never writes.
var configReplaceAllowlist = map[string]map[string]bool{
	"internal/config/write_windows.go": configRetryHelpers,
	"internal/config/write_other.go":   configRetryHelpers,
	"internal/config/resolve.go":       {"ResolveCA": true},
}

// scanConfigReplace returns one message per reference in f to a function
// in configReplaceFuncs outside the allowlisted functions, and one per dot
// import of a package configReplaceFuncs lists. Files outside
// internal/config are not scanned.
func scanConfigReplace(fset *token.FileSet, path string, f *ast.File) []string {
	if !strings.HasPrefix(path, configPackageDir) || strings.Contains(strings.TrimPrefix(path, configPackageDir), "/") {
		return nil
	}
	var out []string
	// guarded maps a name a file refers to an import by to the import path.
	guarded := map[string]string{}
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		if configReplaceFuncs[p] == nil {
			continue
		}
		switch {
		case imp.Name == nil:
			guarded[p[strings.LastIndex(p, "/")+1:]] = p
		case imp.Name.Name == ".":
			out = append(out, fset.Position(imp.Pos()).String()+": dot import of "+p+" hides its calls from the scan")
		case imp.Name.Name != "_":
			guarded[imp.Name.Name] = p
		}
	}
	scan := func(n ast.Node, allowed bool) {
		ast.Inspect(n, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || allowed {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if p, ok := guarded[id.Name]; ok && configReplaceFuncs[p][sel.Sel.Name] {
				out = append(out, fset.Position(sel.Pos()).String()+": "+p+"."+sel.Sel.Name+
					" skips the Windows retry; use renameFile, removeFile, removeAll, readFile or openFile")
			}
			return true
		})
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		scan(d, ok && fn.Recv == nil && configReplaceAllowlist[path][fn.Name.Name])
	}
	return out
}

func scanConfigReplaceSnippet(t *testing.T, path, src string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.AllErrors)
	require.NoError(t, err)
	return scanConfigReplace(fset, path, f)
}

// TestConfigReplaceScanFixtures asserts the scanner's rules from source
// snippets.
func TestConfigReplaceScanFixtures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, path, src string
		reject          bool
	}{
		{"rename", "internal/config/x.go", `package p; import "os"; func f() { _ = os.Rename("a", "b") }`, true},
		{"read file", "internal/config/x.go", `package p; import "os"; func f() { _, _ = os.ReadFile("a") }`, true},
		{"open", "internal/config/x.go", `package p; import "os"; func f() { _, _ = os.Open("a") }`, true},
		{"open file", "internal/config/x.go", `package p; import "os"; func f() { _, _ = os.OpenFile("a", 0, 0) }`, true},
		{"remove", "internal/config/x.go", `package p; import "os"; func f() { _ = os.Remove("a") }`, true},
		{"remove all", "internal/config/x.go", `package p; import "os"; func f() { _ = os.RemoveAll("a") }`, true},
		{"dir fs", "internal/config/x.go", `package p; import ("io/fs"; "os"); func f() { _, _ = fs.ReadFile(os.DirFS("d"), "a") }`, true},
		{"write file", "internal/config/x.go", `package p; import "os"; func f() { _ = os.WriteFile("a", nil, 0o600) }`, true},
		{"create", "internal/config/x.go", `package p; import "os"; func f() { _, _ = os.Create("a") }`, true},
		{"syscall rename", "internal/config/x.go", `package p; import "syscall"; func f() { _ = syscall.Rename("a", "b") }`, true},
		{"syscall open", "internal/config/x.go", `package p; import "syscall"; func f() { _, _ = syscall.Open("a", 0, 0) }`, true},
		{"syscall delete file", "internal/config/x.go", `package p; import "syscall"; func f(p *uint16) { _ = syscall.DeleteFile(p) }`, true},
		{"windows move file ex", "internal/config/x.go", `package p; import "golang.org/x/sys/windows"; func f(a, b *uint16) { _ = windows.MoveFileEx(a, b, 0) }`, true},
		{"windows rename", "internal/config/x.go", `package p; import "golang.org/x/sys/windows"; func f() { _ = windows.Rename("a", "b") }`, true},
		{"windows create file", "internal/config/x.go", `package p; import "golang.org/x/sys/windows"; func f(p *uint16) { _, _ = windows.CreateFile(p, 0, 0, nil, 0, 0, 0) }`, true},
		{"aliased windows import", "internal/config/x.go", `package p; import w "golang.org/x/sys/windows"; func f(a, b *uint16) { _ = w.MoveFileEx(a, b, 0) }`, true},
		{"unix unlink", "internal/config/x.go", `package p; import "golang.org/x/sys/unix"; func f() { _ = unix.Unlink("a") }`, true},
		{"dot import of syscall", "internal/config/x.go", `package p; import . "syscall"; func f() { _ = Rename("a", "b") }`, true},
		{"function value", "internal/config/x.go", `package p; import "os"; var rename = os.Rename`, true},
		{"aliased import", "internal/config/x.go", `package p; import o "os"; func f() { _ = o.Rename("a", "b") }`, true},
		{"dot import", "internal/config/x.go", `package p; import . "os"; func f() { _ = Rename("a", "b") }`, true},
		{"method named like an allowlisted helper", "internal/config/write_other.go", `package p; import "os"; type s struct{}; func (s) readFile() { _, _ = os.ReadFile("a") }`, true},
		{"other function in a helper file", "internal/config/write_other.go", `package p; import "os"; func g() { _, _ = os.ReadFile("a") }`, true},
		{"retrying helper", "internal/config/x.go", `package p; func f() { _ = renameFile("a", "b") }`, false},
		{"root rename", "internal/config/x.go", `package p; import "os"; func f(r *os.Root) { _ = r.Rename("a", "b") }`, false},
		{"other os function", "internal/config/x.go", `package p; import "os"; func f() { _, _ = os.CreateTemp("", "x") }`, false},
		{"other syscall use", "internal/config/x.go", `package p; import "syscall"; var _ syscall.Errno`, false},
		{"other windows use", "internal/config/x.go", `package p; import "golang.org/x/sys/windows"; var _ = windows.ERROR_ACCESS_DENIED`, false},
		{"root remove", "internal/config/x.go", `package p; import "os"; func f(r *os.Root) { _ = r.Remove("a") }`, false},
		{"other package's Rename", "internal/config/x.go", `package p; import "other"; func f() { _ = other.Rename("a", "b") }`, false},
		{"outside the package", "internal/cli/x.go", `package p; import "os"; func f() { _ = os.Rename("a", "b") }`, false},
		{"a subpackage", "internal/config/testdata/x.go", `package p; import "os"; func f() { _ = os.Rename("a", "b") }`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := scanConfigReplaceSnippet(t, tc.path, tc.src)
			if tc.reject {
				assert.NotEmpty(t, got, "expected a violation")
			} else {
				assert.Empty(t, got, "expected no violation, got %v", got)
			}
		})
	}
}

// TestConfigReplaceAllowlistIsExact asserts that each allowlisted function
// may refer to the os functions and nothing else in its file may.
func TestConfigReplaceAllowlistIsExact(t *testing.T) {
	t.Parallel()
	for path, funcs := range configReplaceAllowlist {
		for name := range funcs {
			src := `package p; import "os"; func ` + name + `() { _, _ = os.ReadFile("a"); _ = os.Rename("a", "b") }`
			assert.Empty(t, scanConfigReplaceSnippet(t, path, src), "%s %s", path, name)
			assert.NotEmpty(t, scanConfigReplaceSnippet(t, "internal/config/other.go", src), "%s outside %s", name, path)
		}
	}
}

// TestConfigWritesAndReadsUseTheWindowsRetry runs the scanner over every
// production file in internal/config, and asserts the allowlist names
// only functions that exist, so a renamed helper cannot leave a stale
// entry behind.
func TestConfigWritesAndReadsUseTheWindowsRetry(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	found := map[string]map[string]bool{}
	for i, f := range tree.files {
		path := tree.paths[i]
		for _, o := range scanConfigReplace(tree.fset, path, f) {
			t.Error(o)
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && configReplaceAllowlist[path][fn.Name.Name] {
				if found[path] == nil {
					found[path] = map[string]bool{}
				}
				found[path][fn.Name.Name] = true
			}
		}
	}
	assert.Equal(t, configReplaceAllowlist, found, "every allowlisted function exists")
}
