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

// On Windows a file or directory is owner-only only through its security
// descriptor, and a descriptor set after the create leaves a window in which
// another local user with access to the directory can open the object and
// keep the handle. internal/fsacl creates a file or directory with the
// descriptor in the same call (fsacl.CreateFile, fsacl.CreateTemp and
// fsacl.Mkdir). The scan below fails on every other way the non-test Go files
// of internal/config and internal/commands/configcmd create a file or a
// directory or set a descriptor afterwards: every reference, as a call or as a
// value, to os.CreateTemp, os.Create, os.OpenFile, os.WriteFile,
// os.MkdirTemp, os.Mkdir, os.MkdirAll or os.CopyFS; to a method of any value
// named CreateTemp, Create, CreateFile, OpenFile, WriteFile, MkdirTemp, Mkdir,
// MkdirAll or CopyFS (an os.Root, or a wrapper of one); to flock.New (it
// creates the lock file); to fsacl.Harden and fsacl.HardenInheritable (the
// descriptor set after the create); a bare name under a dot import of os; and a use
// of a package-level variable that holds one of them (a test seam, such as
// hardenACLHook). A package-level variable whose value contains one of them
// without being the plain reference is reported too: the scan cannot follow
// its uses. Each place is a row of ownerOnlyCreateAllowlist, keyed by
// "<file>:<function>:<name>" with the number of matches and the reason the
// object needs no descriptor of its own or has it by another means. The key
// has no line number, so moving code inside a function keeps its row; the
// name is part of the key, so a row for Mkdir does not cover MkdirAll. A
// call inside a closure counts for the function that holds it. A different
// count fails, and so does a row with no match left.
//
// Every .go file of the two directories is read whatever its build
// constraint, so a Windows-only file is scanned on every system. Test files
// and testdata are left out: they create fixtures, not the user's data.
// The calls of fsacl.CreateFile, fsacl.CreateTemp and fsacl.Mkdir are the
// helper and need no row.

const (
	ownerOnlyFsaclPath = modulePrefix + "internal/fsacl"
	ownerOnlyFlockPath = "github.com/gofrs/flock"
)

// ownerOnlyCreateAllowlist maps "<file>:<func>:<name>" to the matches it
// covers.
var ownerOnlyCreateAllowlist = map[string]requestAllowance{
	"internal/config/configdir.go:EnsureDir:MkdirAll":                 {4, "the parents of the config directory, which are made with mode 0700 and nothing else, and three calls that return the error os.MkdirAll returns for an empty name or an existing path that is not a directory, or accept a directory another process created first; they create the directory itself only in a race"},
	"internal/config/credstoreid.go:createIdentityRoot:hardenACLHook": {1, "the credential identity directory that already exists: the list is set again on every write; a new directory is created with its list by fsacl.Mkdir"},
	"internal/config/lock.go:tryDirLock:flock.New":                    {1, "a lock file: it holds no data, and it inherits the list of the config directory"},
	"internal/config/lock.go:waitForFlock:flock.New":                  {1, "a lock file: it holds no data, and it inherits the list of the config directory"},
	"internal/config/write.go:(package level):Harden":                 {1, "the seam of the repair of the credential identity directory that already exists"},
}

// ownerOnlyScanDirectories are the directories the scan reads, with the
// subdirectories each has.
var ownerOnlyScanDirectories = []string{"internal/config/", "internal/commands/configcmd/"}

// ownerOnlyCreateNames are the names of the creating functions and methods.
var ownerOnlyCreateNames = []string{
	"CreateTemp", "Create", "CreateFile", "OpenFile", "WriteFile", "MkdirTemp",
	"Mkdir", "MkdirAll", "CopyFS",
}

// ownerOnlySelectorName reports the name a selector adds to the scan, or ""
// when it adds none: the creating names on any value, flock.New, and the two
// Harden calls of fsacl; never a call of the helper.
func ownerOnlySelectorName(sel *ast.SelectorExpr, fsaclName, flockName string) string {
	pkg := ""
	if id, ok := sel.X.(*ast.Ident); ok {
		pkg = id.Name
	}
	name := sel.Sel.Name
	switch {
	case pkg != "" && pkg == fsaclName:
		if name == "Harden" || name == "HardenInheritable" {
			return name
		}
		return ""
	case pkg != "" && pkg == flockName:
		if name == "New" {
			return "flock.New"
		}
		return ""
	case slices.Contains(ownerOnlyCreateNames, name):
		return name
	}
	return ""
}

// ownerOnlyDotName reports the name a bare identifier adds under a dot import
// of os.
func ownerOnlyDotName(name string) string {
	for _, n := range ownerOnlyCreateNames {
		if n == name && n != "CreateFile" {
			return n
		}
	}
	return ""
}

// ownerOnlyAliases returns the names of the package-level variables of f whose
// value is a plain reference the scan matches, and "unresolved:<name>" for a
// variable whose value contains a match without being one.
func ownerOnlyAliases(f *ast.File) (aliases, unresolved []string) {
	fsaclName := importName(f, ownerOnlyFsaclPath)
	flockName := importName(f, ownerOnlyFlockPath)
	dotOS := importName(f, "os") == "."
	matches := func(e ast.Expr) bool {
		switch e := ast.Unparen(e).(type) {
		case *ast.SelectorExpr:
			return ownerOnlySelectorName(e, fsaclName, flockName) != ""
		case *ast.Ident:
			return dotOS && ownerOnlyDotName(e.Name) != ""
		}
		return false
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, v := range vs.Values {
				if i >= len(vs.Names) || vs.Names[i].Name == "_" {
					continue
				}
				if matches(v) {
					aliases = append(aliases, vs.Names[i].Name)
					continue
				}
				found := false
				ast.Inspect(v, func(n ast.Node) bool {
					if e, ok := n.(ast.Expr); ok && matches(e) {
						found = true
					}
					return !found
				})
				if found {
					unresolved = append(unresolved, vs.Names[i].Name)
				}
			}
		}
	}
	return aliases, unresolved
}

// scanOwnerOnlyCreates returns the matches of the rule in f. The key of a
// match is "<file>:<function>:<name>"; aliases are the package-level
// variables of f's package that hold a matched reference.
func scanOwnerOnlyCreates(fset *token.FileSet, filePath string, f *ast.File, aliases []string) []requestMatch {
	fsaclName := importName(f, ownerOnlyFsaclPath)
	flockName := importName(f, ownerOnlyFlockPath)
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
	_, unresolved := ownerOnlyAliases(f)
	var out []requestMatch
	for _, decl := range f.Decls {
		fn := "(package level)"
		if fd, ok := decl.(*ast.FuncDecl); ok {
			fn = fd.Name.Name
		}
		add := func(pos token.Pos, name string) {
			out = append(out, requestMatch{filePath + ":" + fn + ":" + name, fset.Position(pos).String() + ": " + name})
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
				if name := ownerOnlySelectorName(n, fsaclName, flockName); name != "" {
					add(n.Pos(), name)
				}
			case *ast.Ident:
				if selected[n] {
					break
				}
				switch {
				case dotOS && ownerOnlyDotName(n.Name) != "":
					add(n.Pos(), n.Name)
				case !declared[n] && slices.Contains(aliases, n.Name):
					add(n.Pos(), n.Name)
				}
			}
			return true
		})
		if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.VAR {
			for _, spec := range gd.Specs {
				for _, name := range spec.(*ast.ValueSpec).Names {
					if slices.Contains(unresolved, name.Name) {
						add(name.Pos(), "unresolved:"+name.Name)
					}
				}
			}
		}
	}
	return out
}

func inOwnerOnlyScan(p string) bool {
	return slices.ContainsFunc(ownerOnlyScanDirectories, func(d string) bool { return strings.HasPrefix(p, d) })
}

// TestEveryFileAndDirectoryCreateSetsTheOwnerOnlyDescriptor asserts every
// match of the rule is a row of the allowlist with the count it names.
func TestEveryFileAndDirectoryCreateSetsTheOwnerOnlyDescriptor(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	aliases := map[string][]string{}
	for i, f := range tree.files {
		if !inOwnerOnlyScan(tree.paths[i]) {
			continue
		}
		found, _ := ownerOnlyAliases(f)
		dir := path.Dir(tree.paths[i])
		aliases[dir] = append(aliases[dir], found...)
	}
	var matches []requestMatch
	scanned := 0
	for i, f := range tree.files {
		if !inOwnerOnlyScan(tree.paths[i]) {
			continue
		}
		scanned++
		matches = append(matches, scanOwnerOnlyCreates(tree.fset, tree.paths[i], f, aliases[path.Dir(tree.paths[i])])...)
	}
	require.Greater(t, scanned, 20, "the scan must read the files of both directories")
	for _, problem := range checkRequestMatches(matches, ownerOnlyCreateAllowlist) {
		t.Error(problem + " (create a file or directory with fsacl.CreateFile, fsacl.CreateTemp or fsacl.Mkdir, which set the owner-only security descriptor in the create; list a place that needs none in ownerOnlyCreateAllowlist with the reason)")
	}
	for key, a := range ownerOnlyCreateAllowlist {
		assert.NotEmpty(t, a.why, key)
	}
}

// TestOwnerOnlyCreateScanFixtures asserts the scan reports each form it
// covers, under its own name: calls of os.CreateTemp, os.Create, os.OpenFile,
// os.WriteFile, os.MkdirTemp, os.Mkdir and os.MkdirAll; a method named
// OpenFile, Create, WriteFile or Mkdir; flock.New; fsacl.Harden and
// fsacl.HardenInheritable; a reference that is not a call; a call inside a
// closure; a bare name under a dot import of os; a use of a package-level
// variable that holds a matched reference; a package-level variable whose
// value only contains one; and nothing for fsacl.CreateFile, fsacl.CreateTemp,
// fsacl.Mkdir, a variable that holds one of those, os.Open, os.Remove or the
// blank identifier of a `var _ =` declaration.
func TestOwnerOnlyCreateScanFixtures(t *testing.T) {
	t.Parallel()
	const src = `package p

import (
	"os"

	"github.com/gofrs/flock"
	"github.com/rancher/rancher-cli/internal/fsacl"
)

func calls(r *os.Root) {
	_, _ = os.CreateTemp("", "a")
	_, _ = os.Create("a")
	_, _ = os.OpenFile("a", 0, 0)
	_ = os.WriteFile("a", nil, 0o600)
	_, _ = os.MkdirTemp("", "a")
	_ = os.Mkdir("a", 0o700)
	_ = os.MkdirAll("a", 0o700)
	_, _ = r.OpenFile("a", 0, 0)
	_, _ = r.Create("a")
	_ = r.WriteFile("a", nil, 0o600)
	_ = r.Mkdir("a", 0o700)
	_ = flock.New("a")
	_ = fsacl.Harden("a")
	_ = fsacl.HardenInheritable("a")
}

func helper(r *os.Root) {
	_, _ = fsacl.CreateFile(r, "a", 0o600)
	_, _ = fsacl.CreateTemp(r, "a", 0o600)
	_ = fsacl.Mkdir(r, "a", 0o700, false)
	_, _ = os.Open("a")
	_ = os.Remove("a")
}

func reference() {
	f := os.Create
	_ = f
}

func closure() {
	f := func() { _, _ = os.Create("a") }
	f()
}

var seam = fsacl.Harden

func viaSeam() {
	_ = seam("a")
}

var helperSeam = fsacl.CreateFile

func viaHelperSeam(r *os.Root) {
	_, _ = helperSeam(r, "a", 0o600)
}

var wrapped = func() error { return os.WriteFile("a", nil, 0o600) }

func viaWrapped() {
	_ = wrapped()
}

var _ = os.Create
`
	count := func(src string) map[string]int {
		fset, f := parseSnippet(t, "p.go", src)
		aliases, _ := ownerOnlyAliases(f)
		got := map[string]int{}
		for _, m := range scanOwnerOnlyCreates(fset, "p.go", f, aliases) {
			got[m.key]++
		}
		return got
	}
	assert.Equal(t, map[string]int{
		"p.go:calls:CreateTemp":                   1,
		"p.go:calls:Create":                       2,
		"p.go:calls:OpenFile":                     2,
		"p.go:calls:WriteFile":                    2,
		"p.go:calls:MkdirTemp":                    1,
		"p.go:calls:Mkdir":                        2,
		"p.go:calls:MkdirAll":                     1,
		"p.go:calls:flock.New":                    1,
		"p.go:calls:Harden":                       1,
		"p.go:calls:HardenInheritable":            1,
		"p.go:reference:Create":                   1,
		"p.go:closure:Create":                     1,
		"p.go:(package level):Harden":             1,
		"p.go:viaSeam:seam":                       1,
		"p.go:(package level):WriteFile":          1,
		"p.go:(package level):unresolved:wrapped": 1,
		"p.go:(package level):Create":             1,
	}, count(src))
	assert.Equal(t, map[string]int{"p.go:dot:Mkdir": 1, "p.go:dot:Create": 1}, count(`package p

import . "os"

func dot() {
	_ = Mkdir("a", 0o700)
	_, _ = Create("a")
	_ = Remove("a")
}
`))
}

// TestOwnerOnlyCreateAllowlistFailsOnDrift asserts the checker reports a
// match with no row, a row whose count differs and a row with no match.
func TestOwnerOnlyCreateAllowlistFailsOnDrift(t *testing.T) {
	t.Parallel()
	allow := map[string]requestAllowance{
		"a.go:one":  {1, "a"},
		"a.go:two":  {1, "b"},
		"a.go:gone": {1, "c"},
	}
	matches := []requestMatch{
		{"a.go:one", "a.go:1: Create"},
		{"a.go:two", "a.go:2: Create"},
		{"a.go:two", "a.go:3: Create"},
		{"a.go:new", "a.go:4: Create"},
	}
	problems := strings.Join(checkRequestMatches(matches, allow), "\n")
	assert.Contains(t, problems, "a.go:4: Create")
	assert.Contains(t, problems, "a.go:two: the allowlist covers 1 match(es)")
	assert.Contains(t, problems, "allowlist entry a.go:gone matched nothing")
	assert.NotContains(t, problems, "a.go:one")
}
