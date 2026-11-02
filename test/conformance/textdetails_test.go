package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// A details payload (a type with a TextDetails method) can hold a remedy
// command, and a command is written with the remedy marker so the
// invocation that prints it can name its config directory. Such a payload
// maps its text through MapText; one with fields that hold a command and
// nothing else also has MapCommands, which the renderer uses to offer no
// command. The scan below fails a payload with TextDetails and no MapText
// unless it is in the allowlist with one of two reasons, and fails a type
// with MapText that has no test of its own in its package.

const (
	holdsNoCommand    = "holds no command"
	mappedWithPackage = "holds a command; mapped together with its package's remedy texts"
)

// textDetailsAllowed are the details payloads with no MapText, each with the
// reason, keyed by "<package directory> <type>". An entry whose type gains a
// MapText, or that matches no type with TextDetails, fails.
var textDetailsAllowed = map[string]string{
	"internal/clierr PrimeRequiredDetails":         holdsNoCommand,
	"internal/clierr CapabilityUnsupportedDetails": holdsNoCommand,
	"internal/skillinstall Report":                 holdsNoCommand,
	"internal/token directWaitDetails":             holdsNoCommand,
	"internal/client/tlstrust Details":             holdsNoCommand,
}

// textDetailsType is which of the methods the rule reads a type declares.
type textDetailsType struct {
	textDetails, mapText bool
}

// methodReceiverName is the name of the type fd is a method of, "" for a
// function.
func methodReceiverName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) != 1 {
		return ""
	}
	t := fd.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if idx, ok := t.(*ast.IndexExpr); ok {
		t = idx.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// textDetailsTypes is, by "<package directory> <type>", the methods the rule
// reads that each type of files declares; paths[i] is files[i]'s path.
func textDetailsTypes(files []*ast.File, paths []string) map[string]*textDetailsType {
	out := map[string]*textDetailsType{}
	for i, f := range files {
		dir := path.Dir(paths[i])
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			name := methodReceiverName(fd)
			if name == "" || (fd.Name.Name != "TextDetails" && fd.Name.Name != "MapText") {
				continue
			}
			key := dir + " " + name
			if out[key] == nil {
				out[key] = &textDetailsType{}
			}
			switch fd.Name.Name {
			case "TextDetails":
				out[key].textDetails = true
			case "MapText":
				out[key].mapText = true
			}
		}
	}
	return out
}

// checkTextDetails applies the rule to the types and returns each failure.
// tests holds, by package directory, the names of the test functions of its
// test files.
func checkTextDetails(types map[string]*textDetailsType, tests map[string][]string, allowed map[string]string) []string {
	var out []string
	for _, key := range slices.Sorted(maps.Keys(types)) {
		ty := types[key]
		dir, name, _ := strings.Cut(key, " ")
		_, listed := allowed[key]
		if ty.textDetails && !ty.mapText && !listed {
			out = append(out, key+": has TextDetails and no MapText; map its text, or list it with the reason it holds no command")
		}
		if ty.mapText && !slices.ContainsFunc(tests[dir], func(test string) bool {
			return strings.HasPrefix(test, "Test"+name+"MapText")
		}) {
			out = append(out, key+": has MapText and no test in its package whose name starts with Test"+name+"MapText")
		}
	}
	for _, key := range slices.Sorted(maps.Keys(allowed)) {
		ty := types[key]
		switch {
		case allowed[key] != holdsNoCommand && allowed[key] != mappedWithPackage:
			out = append(out, "allowlist entry "+key+": the reason is neither of the two accepted")
		case ty == nil || !ty.textDetails:
			out = append(out, "allowlist entry "+key+" matches no type with TextDetails: remove it")
		case ty.mapText:
			out = append(out, "allowlist entry "+key+": the type has MapText now; remove the entry")
		}
	}
	return out
}

// testFunctionNames is the names of the top-level functions of every test
// file in dir.
func testFunctionNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil {
				out = append(out, fd.Name.Name)
			}
		}
	}
	return out
}

// TestEveryDetailsPayloadMapsItsText asserts every type of the production
// tree with a TextDetails method has MapText, or is in the allowlist with
// its reason, and that every type with MapText has a test of it.
func TestEveryDetailsPayloadMapsItsText(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	found := textDetailsTypes(tree.files, tree.paths)
	require.Greater(t, len(found), 10, "the scan found the details payloads")
	for _, want := range []string{"internal/clierr LoginFailureDetails", "internal/clierr CommittedContextDetails", "internal/token RevocationReport"} {
		require.Contains(t, found, want)
	}
	tests := map[string][]string{}
	for key := range found {
		dir, _, _ := strings.Cut(key, " ")
		if _, ok := tests[dir]; !ok {
			tests[dir] = testFunctionNames(t, dir)
		}
	}
	for _, f := range checkTextDetails(found, tests, textDetailsAllowed) {
		t.Error(f)
	}
}

// TestDetailsPayloadRules asserts the rules on declared sources: a payload
// with TextDetails and no MapText fails unless listed, a listed one that
// gains MapText or does not exist fails, a type with MapText and no test
// fails, and a reason outside the two fails.
func TestDetailsPayloadRules(t *testing.T) {
	t.Parallel()
	parse := func(src string) ([]*ast.File, []string) {
		f, err := parser.ParseFile(token.NewFileSet(), "x/p/p.go", "package p\n"+src, 0)
		require.NoError(t, err)
		return []*ast.File{f}, []string{"x/p/p.go"}
	}
	const declared = `
type A struct{}
func (A) TextDetails() []string { return nil }
type B struct{}
func (B) TextDetails() []string { return nil }
func (B) MapText(f func(string) string) TextDetails { return B{} }
type c struct{}
func (*c) TextDetails() []string { return nil }
`
	files, paths := parse(declared)
	found := textDetailsTypes(files, paths)
	assert.Equal(t, map[string]*textDetailsType{
		"x/p A": {textDetails: true}, "x/p B": {textDetails: true, mapText: true}, "x/p c": {textDetails: true},
	}, found)

	tests := map[string][]string{"x/p": {"TestBMapTextMapsEverything"}}
	assert.Equal(t, []string{
		"x/p A: has TextDetails and no MapText; map its text, or list it with the reason it holds no command",
		"x/p c: has TextDetails and no MapText; map its text, or list it with the reason it holds no command",
	}, checkTextDetails(found, tests, nil))
	assert.Empty(t, checkTextDetails(found, tests, map[string]string{"x/p A": holdsNoCommand, "x/p c": mappedWithPackage}))

	assert.Equal(t, []string{"allowlist entry x/p B: the type has MapText now; remove the entry"},
		checkTextDetails(found, tests, map[string]string{"x/p A": holdsNoCommand, "x/p B": holdsNoCommand, "x/p c": holdsNoCommand}))
	assert.Equal(t, []string{"allowlist entry x/p gone matches no type with TextDetails: remove it"},
		checkTextDetails(found, tests, map[string]string{"x/p A": holdsNoCommand, "x/p c": holdsNoCommand, "x/p gone": holdsNoCommand}))
	assert.Equal(t, []string{"allowlist entry x/p A: the reason is neither of the two accepted"},
		checkTextDetails(found, tests, map[string]string{"x/p A": "because", "x/p c": holdsNoCommand}))
	assert.Equal(t, []string{"x/p B: has MapText and no test in its package whose name starts with TestBMapText"},
		checkTextDetails(found, map[string][]string{"x/p": {"TestBMaps", "TestMapText", "BMapText", "TestMapTextB", "TestXBMapText"}},
			map[string]string{"x/p A": holdsNoCommand, "x/p c": holdsNoCommand}))
}

const (
	markedCandidateRule = "a clierr.Candidate field is built from the remedy marker; candidates are never expanded, so the command would print without the directory"
	markedCandidateDir  = "test/conformance/testdata/candidatemarker"
)

// fromRemedyMarker reports whether e holds a use of clierr.Rancher,
// clierr.RemedyMarker or clierr.MarkRemedy, or of a local variable in
// locals.
func fromRemedyMarker(info *types.Info, e ast.Expr, locals map[types.Object]bool) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return !found
		}
		obj := info.Uses[id]
		if obj == nil {
			return true
		}
		if locals[obj] {
			found = true
			return false
		}
		if obj.Pkg() == nil || obj.Pkg().Path() != clierrPath {
			return true
		}
		switch obj.(type) {
		case *types.Const, *types.Var:
			found = found || id.Name == "Rancher" || id.Name == "RemedyMarker"
		case *types.Func:
			found = found || id.Name == "MarkRemedy"
		}
		return !found
	})
	return found
}

// markedLocals is the local variables of body that are bound, by := or by
// var, from a use of the remedy marker or from another such variable, in
// source order.
func markedLocals(info *types.Info, body *ast.BlockStmt) map[types.Object]bool {
	locals := map[types.Object]bool{}
	bind := func(id *ast.Ident, value ast.Expr) {
		if obj := info.Defs[id]; obj != nil && fromRemedyMarker(info, value, locals) {
			locals[obj] = true
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if n.Tok != token.DEFINE || len(n.Lhs) != len(n.Rhs) {
				break
			}
			for i, lhs := range n.Lhs {
				if id, ok := lhs.(*ast.Ident); ok {
					bind(id, n.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			if len(n.Names) != len(n.Values) {
				break
			}
			for i, id := range n.Names {
				bind(id, n.Values[i])
			}
		}
		return true
	})
	return locals
}

// scanMarkedCandidates returns the refusals in the non-test files of pkg, as
// "file:line: rule", and how many clierr.Candidate literals and assignments
// to a field of one it read. A candidate's text is never expanded for the
// invocation, so a candidate built from the remedy marker would print
// without the directory. The scan fails a literal, or an assignment to a
// field of a candidate, whose value holds clierr.Rancher, clierr.MarkRemedy
// or clierr.RemedyMarker, or a local variable of the same function body
// bound from one of them by := or var. It does not follow a package-level
// variable, a value returned by a helper, or a local assigned with = after
// its declaration.
func scanMarkedCandidates(pkg *packages.Package, wd string) (findings []string, read int) {
	info := pkg.TypesInfo
	for _, f := range pkg.Syntax {
		name := pkg.Fset.Position(f.Pos()).Filename
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		rel, err := filepath.Rel(wd, name)
		if err != nil {
			rel = name
		}
		report := func(pos token.Pos) {
			findings = append(findings, fmt.Sprintf("%s:%d: %s", filepath.ToSlash(rel), pkg.Fset.Position(pos).Line, markedCandidateRule))
		}
		scan := func(root ast.Node, locals map[types.Object]bool) {
			ast.Inspect(root, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CompositeLit:
					if !isNamedType(derefType(info.TypeOf(n)), clierrPath, "Candidate") {
						break
					}
					read++
					for _, elt := range n.Elts {
						value := elt
						if kv, ok := elt.(*ast.KeyValueExpr); ok {
							value = kv.Value
						}
						if fromRemedyMarker(info, value, locals) {
							report(n.Pos())
							break
						}
					}
				case *ast.AssignStmt:
					for i, lhs := range n.Lhs {
						sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr)
						if !ok || !isNamedType(derefType(info.TypeOf(sel.X)), clierrPath, "Candidate") {
							continue
						}
						read++
						if len(n.Lhs) == len(n.Rhs) && fromRemedyMarker(info, n.Rhs[i], locals) {
							report(n.Pos())
						}
					}
				}
				return true
			})
		}
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Body != nil {
				scan(fd.Body, markedLocals(info, fd.Body))
			} else {
				scan(decl, nil)
			}
		}
	}
	return findings, read
}

// TestNoCandidateIsBuiltFromTheRemedyMarker asserts the scan finds nothing in
// the production tree.
func TestNoCandidateIsBuiltFromTheRemedyMarker(t *testing.T) {
	t.Parallel()
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	total := 0
	var findings []string
	for _, pkg := range pkgs {
		got, n := scanMarkedCandidates(pkg, wd)
		findings = append(findings, got...)
		total += n
	}
	require.Positive(t, total, "the scan read no clierr.Candidate literal")
	slices.Sort(findings)
	for _, f := range findings {
		t.Error(f)
	}
}

// markedCandidateFixtures type-checks the fixture packages once.
var markedCandidateFixtures = sync.OnceValues(func() (map[string]*packages.Package, error) {
	pkgs, err := loadPackages("./" + markedCandidateDir + "/...")
	if err != nil {
		return nil, err
	}
	out := map[string]*packages.Package{}
	for _, pkg := range pkgs {
		out[path.Base(pkg.PkgPath)] = pkg
	}
	return out, nil
})

var markedCandidateCases = []struct {
	fixture  string
	findings int
}{
	{"keyed", 1},
	{"positional", 1},
	{"markremedy", 1},
	{"markerconstant", 1},
	{"assigned", 1},
	{"shortvar", 1},
	{"declaredvar", 1},
	{"plain", 0},
	{"othername", 0},
	{"othervar", 0},
}

var markedCandidatePosition = regexp.MustCompile(`^[^:]+:[0-9]+: `)

// TestMarkedCandidateScanFixtures asserts the scan on type-checked fixture
// packages: a candidate built from clierr.Rancher, clierr.RemedyMarker or
// clierr.MarkRemedy, keyed or positional, a field of a candidate assigned
// from one, and a local variable bound from one and used in a candidate are
// reported, and a candidate of plain text, one built from a name of another
// package that looks alike, one built from another variable of clierr, and a
// message built from clierr.Rancher outside a candidate are accepted.
func TestMarkedCandidateScanFixtures(t *testing.T) {
	t.Parallel()
	pkgs, err := markedCandidateFixtures()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	for _, tc := range markedCandidateCases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			pkg, ok := pkgs[tc.fixture]
			require.True(t, ok, "fixture package %s", tc.fixture)
			findings, literals := scanMarkedCandidates(pkg, wd)
			assert.Positive(t, literals)
			assert.Len(t, findings, tc.findings)
			for _, f := range findings {
				assert.Equal(t, markedCandidateRule, markedCandidatePosition.ReplaceAllString(f, ""))
			}
		})
	}
}

// TestMarkedCandidateFixturesAreAllListed asserts every fixture package on
// disk is in the table above, so a fixture added without its row fails.
func TestMarkedCandidateFixturesAreAllListed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(markedCandidateDir)
	require.NoError(t, err)
	pkgs, err := markedCandidateFixtures()
	require.NoError(t, err)
	var onDisk, listed []string
	for _, e := range entries {
		if e.IsDir() {
			onDisk = append(onDisk, e.Name())
		}
	}
	for _, tc := range markedCandidateCases {
		listed = append(listed, tc.fixture)
	}
	assert.ElementsMatch(t, onDisk, listed)
	assert.ElementsMatch(t, listed, slices.Collect(maps.Keys(pkgs)))
}
