package conformance

import (
	"go/ast"
	"go/token"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The config directory is created only through config.EnsureDir, which sets
// the owner-only access list on Windows where the mode bits do nothing. The
// scan below fails on every other place in the production tree that creates
// a directory: a call of os.Mkdir, os.MkdirAll or os.CopyFS (which creates
// its target directory), or of a method named Mkdir, MkdirAll or CopyFS (an
// os.Root, or a wrapper of one), and a reference to any of them that is not
// a call, and a use of a package-level variable that holds one of them.
// Each place is a row of directoryCreateAllowlist, keyed by
// "<file>:<function>:<name>" with the number of matches and the reason the
// directory is not the config directory; the name is part of the key, so a
// row for Mkdir does not cover MkdirAll. A call inside a closure counts for
// the function that holds it. A different count fails, and so does a row
// with no match left. The functions that create the config directory must
// also call EnsureDir, which a second check reads from their bodies; it
// matches a call by the name EnsureDir and a function by its name in its
// file, which is enough for the functions it lists.
// os.MkdirTemp is outside the rule: it creates temporary directories, never
// the config directory.

// directoryCreateAllowlist maps "<file>:<func>:<name>" to the matches it
// covers.
var directoryCreateAllowlist = map[string]requestAllowance{
	"internal/config/configdir.go:EnsureDir:MkdirAll":              {4, "the parents of the config directory, and three calls that return the error os.MkdirAll returns for an empty name or an existing path that is not a directory, or accept a directory another process created first; they create the directory itself only in a race"},
	"internal/config/configdir.go:(package level):Mkdir":           {1, "the seam of EnsureDir's one create of the config directory"},
	"internal/config/configdir.go:EnsureDir:mkdirDir":              {1, "EnsureDir's one create of the config directory, through its seam; the access list is set right after it"},
	"internal/config/credstoreid.go:createIdentityRoot:Mkdir":      {1, "credstore-id inside the config directory; it sets its own owner-only access list on every write"},
	"internal/telemetry/fsroot.go:Mkdir:Mkdir":                     {1, "wrapper of os.Root.Mkdir inside the opened config directory"},
	"internal/telemetry/fsroot.go:MkdirAll:MkdirAll":               {1, "wrapper of os.Root.MkdirAll inside the opened config directory"},
	"internal/telemetry/store.go:ensureDir:Mkdir":                  {1, "telemetry/ inside the config directory; it sets the inheritable owner-only access list when it creates it"},
	"internal/telemetry/store.go:writeSlot:MkdirAll":               {1, "a subdirectory of telemetry/, which inherits its access list"},
	"internal/telemetry/store.go:WriteFacts:MkdirAll":              {1, "a subdirectory of telemetry/, which inherits its access list"},
	"internal/telemetry/uploadlock.go:ensureUploadDir:Mkdir":       {1, "a subdirectory of telemetry/, which inherits its access list"},
	"internal/skillinstall/ops.go:Acquire:MkdirAll":                {2, "the skill-locks folder under the OS user cache directory and the parent of a coding agent's skill folder, not the config directory"},
	"internal/skillinstall/ops.go:install:MkdirAll":                {2, "staging and destination folders of a coding agent's skills, not the config directory"},
	"internal/commands/cmdtest/prelude.go:RunPreludeRows:MkdirAll": {2, "test helper that puts a directory where a file belongs"},
	"internal/testfs/testfs.go:DirectoryInPlaceOfFile:MkdirAll":    {1, "test helper that puts a directory where a file belongs"},
	"internal/testrancher/fidelity/golden.go:SaveGolden:MkdirAll":  {1, "the folder of a golden file of the fidelity tests"},
}

// configDirectoryFunctions are the functions that create the config
// directory. None may have a row in the allowlist: each calls
// config.EnsureDir instead.
var configDirectoryFunctions = []string{
	"internal/config/write.go:atomicWriteFile",
	"internal/config/lock.go:lockFile",
	"internal/config/pluginlock.go:WithPluginLoginLock",
	"internal/config/helper_records.go:withHelperRecordsLock",
	"internal/config/credstore.go:withCacheLock",
	"internal/config/credtransition.go:writeTransition",
	"internal/commands/configcmd/config_edit.go:newTempCopy",
}

// ensureDirCallers are the functions that must call EnsureDir: the functions
// above and createIdentityRoot, whose own Mkdir creates credstore-id inside
// the config directory that EnsureDir has created first.
var ensureDirCallers = append([]string{"internal/config/credstoreid.go:createIdentityRoot"}, configDirectoryFunctions...)

// isDirectoryCreateName reports whether name is one of the creating calls.
func isDirectoryCreateName(name string) bool {
	return name == "Mkdir" || name == "MkdirAll" || name == "CopyFS"
}

// directoryCreateAliases returns the names of the package-level variables of
// f whose value is one of the creating functions (a test seam that holds
// os.Mkdir, for example): a call of such a variable creates a directory as
// the function does, so the scan matches every use of the name in the
// package.
func directoryCreateAliases(f *ast.File) []string {
	dotOS := importName(f, "os") == "."
	var out []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, v := range vs.Values {
				if i >= len(vs.Names) {
					break
				}
				if vs.Names[i].Name == "_" {
					continue
				}
				switch v := ast.Unparen(v).(type) {
				case *ast.SelectorExpr:
					if isDirectoryCreateName(v.Sel.Name) {
						out = append(out, vs.Names[i].Name)
					}
				case *ast.Ident:
					if dotOS && isDirectoryCreateName(v.Name) {
						out = append(out, vs.Names[i].Name)
					}
				}
			}
		}
	}
	return out
}

// scanDirectoryCreates returns the matches of the rule in f: a selector
// whose name is one of the creating names on any value or package, a bare
// name under a dot import of os, and a use of a name in aliases (the
// package-level variables of f's package that hold a creating function, from
// directoryCreateAliases) other than its declaration, and a selector whose
// name is in treeAliases (the same variables of every package, for a call
// such as x.MkdirFn from another package). The key of a match is
// "<file>:<function>:<name>".
func scanDirectoryCreates(fset *token.FileSet, filePath string, f *ast.File, aliases, treeAliases []string) []requestMatch {
	dotOS := importName(f, "os") == "."
	declared := map[*ast.Ident]bool{}
	for _, decl := range f.Decls {
		if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.VAR {
			for _, spec := range gd.Specs {
				for _, name := range spec.(*ast.ValueSpec).Names {
					declared[name] = true
				}
			}
		}
	}
	var out []requestMatch
	for _, decl := range f.Decls {
		fn := "(package level)"
		if fd, ok := decl.(*ast.FuncDecl); ok {
			fn = fd.Name.Name
		}
		selected := map[*ast.Ident]bool{}
		ast.Inspect(decl, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				selected[sel.Sel] = true
			}
			return true
		})
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if isDirectoryCreateName(n.Sel.Name) || slices.Contains(treeAliases, n.Sel.Name) {
					out = append(out, requestMatch{filePath + ":" + fn + ":" + n.Sel.Name, fset.Position(n.Pos()).String() + ": " + n.Sel.Name})
				}
			case *ast.Ident:
				if !selected[n] && ((dotOS && isDirectoryCreateName(n.Name)) || (!declared[n] && slices.Contains(aliases, n.Name))) {
					out = append(out, requestMatch{filePath + ":" + fn + ":" + n.Name, fset.Position(n.Pos()).String() + ": " + n.Name})
				}
			}
			return true
		})
	}
	return out
}

// functionsCallingEnsureDir returns "<file>:<function>" for every function
// of f whose body calls EnsureDir, as EnsureDir or as a selector (a call from
// another package). A call inside a closure counts for the function that
// holds it.
func functionsCallingEnsureDir(filePath string, f *ast.File) []string {
	var out []string
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		called := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := ast.Unparen(call.Fun).(type) {
			case *ast.Ident:
				called = called || fun.Name == "EnsureDir"
			case *ast.SelectorExpr:
				called = called || fun.Sel.Name == "EnsureDir"
			}
			return true
		})
		if called {
			out = append(out, filePath+":"+fd.Name.Name)
		}
	}
	return out
}

// TestEveryDirectoryCreateIsListed asserts every place in the production
// tree that creates a directory is a row of the allowlist, that no row
// names a function that creates the config directory, and that each of those
// functions calls EnsureDir.
func TestEveryDirectoryCreateIsListed(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	aliases := map[string][]string{}
	var treeAliases []string
	for i, f := range tree.files {
		dir := path.Dir(tree.paths[i])
		found := directoryCreateAliases(f)
		aliases[dir] = append(aliases[dir], found...)
		treeAliases = append(treeAliases, found...)
	}
	var matches []requestMatch
	var callers []string
	for i, f := range tree.files {
		matches = append(matches, scanDirectoryCreates(tree.fset, tree.paths[i], f, aliases[path.Dir(tree.paths[i])], treeAliases)...)
		callers = append(callers, functionsCallingEnsureDir(tree.paths[i], f)...)
	}
	for _, problem := range checkRequestMatches(matches, directoryCreateAllowlist) {
		t.Error(problem + " (create the config directory with config.EnsureDir; list a directory that is not the config directory in directoryCreateAllowlist with the reason)")
	}
	for key, a := range directoryCreateAllowlist {
		assert.NotEmpty(t, a.why, key)
		for _, fn := range configDirectoryFunctions {
			assert.False(t, strings.HasPrefix(key, fn+":"), "%s creates the config directory: call config.EnsureDir", key)
		}
	}
	for _, fn := range ensureDirCallers {
		assert.Contains(t, callers, fn, "%s must call EnsureDir (the function is missing or does not call it)", fn)
	}
}

// TestDirectoryCreateScanFixtures asserts the scan reports each form it
// covers: a call of os.Mkdir, os.MkdirAll and os.CopyFS, a method named
// Mkdir or MkdirAll, a reference that is not a call, a call inside a
// closure, a bare name under a dot import of os, a use of a package-level
// variable that holds a creating function, a selector naming such a
// variable of another package, each under its own name, and nothing for
// MkdirTemp or for the blank identifier of a `var _ =` declaration.
func TestDirectoryCreateScanFixtures(t *testing.T) {
	t.Parallel()
	const src = `package p

import "os"

func calls(r *os.Root) {
	_ = os.Mkdir("a", 0o700)
	_ = os.MkdirAll("a", 0o700)
	_ = r.Mkdir("a", 0o700)
	_ = r.MkdirAll("a", 0o700)
	_ = os.CopyFS("a", nil)
	_, _ = os.MkdirTemp("", "a")
}

func reference() {
	mk := os.MkdirAll
	_ = mk
}

func closure() {
	f := func() { _ = os.Mkdir("a", 0o700) }
	f()
}

var packageLevel = os.MkdirAll

func viaAlias() {
	_ = packageLevel("a", 0o700)
}

var _ = os.MkdirAll

func blank() {
	_ = 1
}

func viaSelector(other struct{ MkdirFn func(string, os.FileMode) error }) {
	_ = other.MkdirFn("a", 0o700)
}
`
	count := func(src string) map[string]int {
		fset, f := parseSnippet(t, "p.go", src)
		got := map[string]int{}
		for _, m := range scanDirectoryCreates(fset, "p.go", f, directoryCreateAliases(f), append(directoryCreateAliases(f), "MkdirFn")) {
			got[m.key]++
		}
		return got
	}
	assert.Equal(t, map[string]int{
		"p.go:calls:Mkdir":              2,
		"p.go:calls:MkdirAll":           2,
		"p.go:calls:CopyFS":             1,
		"p.go:reference:MkdirAll":       1,
		"p.go:closure:Mkdir":            1,
		"p.go:(package level):MkdirAll": 2,
		"p.go:viaAlias:packageLevel":    1,
		"p.go:viaSelector:MkdirFn":      1,
	}, count(src))
	assert.Equal(t, map[string]int{"p.go:dot:Mkdir": 1}, count(`package p

import . "os"

func dot() {
	_ = Mkdir("a", 0o700)
}
`))
}

// TestEnsureDirCallScanFixtures asserts the check finds a call of EnsureDir
// as a plain name, as a selector and inside a closure, and nothing in a
// function that calls os.MkdirAll or only mentions the name.
func TestEnsureDirCallScanFixtures(t *testing.T) {
	t.Parallel()
	const src = `package p

func plain() error { return EnsureDir("a") }

func selector() error { return config.EnsureDir("a") }

func closure() {
	f := func() error { return EnsureDir("a") }
	_ = f()
}

func direct() error { return os.MkdirAll("a", 0o700) }

func mention() { _ = EnsureDir }
`
	_, f := parseSnippet(t, "p.go", src)
	assert.ElementsMatch(t, []string{"p.go:plain", "p.go:selector", "p.go:closure"}, functionsCallingEnsureDir("p.go", f))
}

// TestDirectoryCreateAllowlistFailsOnDrift asserts the checker reports a
// match with no row, a row whose count differs and a row with no match.
func TestDirectoryCreateAllowlistFailsOnDrift(t *testing.T) {
	t.Parallel()
	allow := map[string]requestAllowance{
		"a.go:one":  {1, "a"},
		"a.go:two":  {1, "b"},
		"a.go:gone": {1, "c"},
	}
	matches := []requestMatch{
		{"a.go:one", "a.go:1: Mkdir"},
		{"a.go:two", "a.go:2: Mkdir"},
		{"a.go:two", "a.go:3: Mkdir"},
		{"a.go:new", "a.go:4: Mkdir"},
	}
	problems := strings.Join(checkRequestMatches(matches, allow), "\n")
	assert.Contains(t, problems, "a.go:4: Mkdir")
	assert.Contains(t, problems, "a.go:two: the allowlist covers 1 match(es)")
	assert.Contains(t, problems, "allowlist entry a.go:gone matched nothing")
	assert.NotContains(t, problems, "a.go:one")
}
