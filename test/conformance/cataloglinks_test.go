package conformance

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// The index link of a ClusterRepo is read in two places, and neither read
// asks Rancher for more or other than it lists for the cluster. In
// non-test Go code a string literal that holds "link=" or is exactly "link"
// belongs only to catalogLinkFile, which the commands read through, and to
// the completion path builder named in catalogLinkAllowlist, which the
// --chart and --version completion read through; no literal holds
// "skipFilter" or "k8sVersion" anywhere: the first two spellings build a
// link request, the others change which versions the server answers with.
const catalogLinkFile = "internal/engine/catalog/index.go"

// catalogLinkAllowlist names the files whose "link" literal is not a link
// request, each with its reason.
var catalogLinkAllowlist = map[string]string{
	"internal/telemetry/fsroot.go":        "the name of the file system operation Link, passed to the operation observer",
	"internal/runtime/completion/path.go": "the completion read of the same filtered index, for --chart and --version of app install; it never sets skipFilter or k8sVersion",
}

// catalogLiteralFinding is one constant string the scan reports.
type catalogLiteralFinding struct {
	path string
	line int
	// link is true for a link-request string, false for a filter-changing
	// parameter name.
	link bool
	text string
}

// stubImporter hands the type checker an empty package for every import. It
// serves only the files that no package load covers (the loads are for this
// machine, GOOS=windows and GOOS=linux): they are checked together with the
// other such files of their directory, so only their constants of the same
// directory are folded, and no constant of another package.
type stubImporter struct{}

func (stubImporter) Import(path string) (*types.Package, error) {
	pkg := types.NewPackage(path, filepath.Base(path))
	pkg.MarkComplete()
	return pkg, nil
}

// stubTypes type-checks files with stubImporter and ignores the errors the
// missing imports cause.
func stubTypes(fset *token.FileSet, files []*ast.File) *types.Info {
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
	conf := types.Config{Importer: stubImporter{}, Error: func(error) {}}
	_, _ = conf.Check("p", fset, files, info)
	return info
}

// scanCatalogLiterals reports, for files type-checked into info, every
// constant string expression that builds a link request or names a
// filter-changing parameter. An expression is folded the way the compiler
// folds it: string literals, named constants (of other packages too, when
// info comes from the real packages), "+" and parentheses, so a name split
// over several constants is one value.
func scanCatalogLiterals(fset *token.FileSet, files []*ast.File, info *types.Info) []catalogLiteralFinding {
	var out []catalogLiteralFinding
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			e, ok := n.(ast.Expr)
			if !ok {
				return true
			}
			tv, ok := info.Types[e]
			if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
				return true
			}
			text := constant.StringVal(tv.Value)
			pos := fset.Position(e.Pos())
			if strings.Contains(text, "link=") || text == "link" {
				out = append(out, catalogLiteralFinding{path: pos.Filename, line: pos.Line, link: true, text: text})
			}
			if strings.Contains(text, "skipFilter") || strings.Contains(text, "k8sVersion") {
				out = append(out, catalogLiteralFinding{path: pos.Filename, line: pos.Line, text: text})
			}
			return false
		})
	}
	return out
}

// catalogLiteralProblems turns findings per file into the reports: a link
// literal outside catalogLinkFile and the allowlist, a filter-changing
// literal anywhere, and an allowlist entry whose file has no link literal.
func catalogLiteralProblems(findings map[string][]catalogLiteralFinding, allow map[string]string) []string {
	var problems []string
	for _, path := range slices.Sorted(maps.Keys(findings)) {
		for _, f := range findings[path] {
			switch {
			case !f.link:
				problems = append(problems, path+":"+strconv.Itoa(f.line)+": "+strconv.Quote(f.text)+" changes which versions the server lists; the index read never sends it")
			case path != catalogLinkFile && allow[path] == "":
				problems = append(problems, path+":"+strconv.Itoa(f.line)+": "+strconv.Quote(f.text)+" builds an index link request outside "+catalogLinkFile)
			}
		}
	}
	for _, path := range slices.Sorted(maps.Keys(allow)) {
		if !slices.ContainsFunc(findings[path], func(f catalogLiteralFinding) bool { return f.link }) {
			problems = append(problems, "allowlist entry "+path+" matched nothing: remove it")
		}
	}
	return problems
}

// catalogRelPath turns a file name of a loaded package or a parsed file into
// the slash path relative to the module root that the findings are keyed by.
func catalogRelPath(root, name string) string {
	if filepath.IsAbs(name) {
		if rel, err := filepath.Rel(root, name); err == nil {
			name = rel
		}
	}
	return filepath.ToSlash(name)
}

// catalogScanPackages loads every package of the module for this machine, and
// for GOOS=windows and GOOS=linux when that is not this machine's, so each file
// the build constraints of one platform leave out is type-checked with the
// other files of its package.
var catalogScanPackages = sync.OnceValues(func() ([][]*packages.Package, error) {
	host, err := fieldManagerPackages()
	if err != nil {
		return nil, err
	}
	sets := [][]*packages.Package{host}
	for _, goos := range []string{"windows", "linux"} {
		if goos == runtime.GOOS {
			continue
		}
		pkgs, err := loadPackages("./...", goos)
		if err != nil {
			return nil, err
		}
		sets = append(sets, pkgs)
	}
	return sets, nil
})

// TestCatalogIndexLinkIsBuiltInOnePlace asserts, over every non-test Go
// file, that only the link read of the catalog package holds a constant
// string that builds an index link request, however it is split into
// literals and constants, of this package or another; that it holds one
// (so the scan sees the builder); and that no file holds a constant string
// that changes the server's filter of versions. Every file is checked with
// the real types of its package, under the build of this machine, of
// GOOS=windows or of GOOS=linux; a file none of the three builds is checked
// with the constants of the other such files of its directory only.
func TestCatalogIndexLinkIsBuiltInOnePlace(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	sets, err := catalogScanPackages()
	require.NoError(t, err)
	root, err := os.Getwd()
	require.NoError(t, err)

	findings := map[string][]catalogLiteralFinding{}
	add := func(got []catalogLiteralFinding) {
		for _, f := range got {
			f.path = catalogRelPath(root, f.path)
			if !slices.Contains(findings[f.path], f) {
				findings[f.path] = append(findings[f.path], f)
			}
		}
	}
	loaded := map[string]bool{}
	for _, pkgs := range sets {
		for _, pkg := range pkgs {
			for _, f := range pkg.Syntax {
				loaded[catalogRelPath(root, pkg.Fset.Position(f.Pos()).Filename)] = true
			}
			add(scanCatalogLiterals(pkg.Fset, pkg.Syntax, pkg.TypesInfo))
		}
	}
	byDir := map[string][]*ast.File{}
	for i, f := range tree.files {
		if !loaded[catalogRelPath(root, tree.paths[i])] {
			dir := filepath.Dir(tree.paths[i])
			byDir[dir] = append(byDir[dir], f)
		}
	}
	for _, dir := range slices.Sorted(maps.Keys(byDir)) {
		add(scanCatalogLiterals(tree.fset, byDir[dir], stubTypes(tree.fset, byDir[dir])))
	}
	require.NotEmpty(t, findings[catalogLinkFile], "the link read's file holds the builder")
	assert.Empty(t, catalogLiteralProblems(findings, catalogLinkAllowlist))
}

const catalogLinkFixturesDir = "test/conformance/testdata/cataloglinks"

// catalogLinkFixtureFiles are the fixture entries the tests below read.
var catalogLinkFixtureFiles = []string{
	"handpath.go", "queryparam.go", "concat.go", "split.go", "cross.go",
	"head", "unfiltered.go", "clean.go",
}

// catalogLinkFixtures loads the fixture packages with their real types, once.
var catalogLinkFixtures = sync.OnceValues(func() (map[string][]catalogLiteralFinding, error) {
	pkgs, err := loadPackages("./"+catalogLinkFixturesDir+"/...", "")
	if err != nil {
		return nil, err
	}
	byFile := map[string][]catalogLiteralFinding{}
	for _, pkg := range pkgs {
		for _, f := range scanCatalogLiterals(pkg.Fset, pkg.Syntax, pkg.TypesInfo) {
			name := filepath.Base(f.path)
			byFile[name] = append(byFile[name], f)
		}
	}
	return byFile, nil
})

func scanCatalogLinkFixture(t *testing.T, name string) []catalogLiteralFinding {
	t.Helper()
	byFile, err := catalogLinkFixtures()
	require.NoError(t, err)
	return byFile[name]
}

// TestCatalogLiteralScanFixtures asserts the scan sees each way to build
// the link or to change the filter: a path written by hand, the link as a
// query parameter, a name split from its value, a name split over a named
// constant and a literal, a name completed from a constant of another
// package, and each filter-changing parameter; that words in comments and
// identifiers are not reported; that a link literal is reported outside the
// link file and the allowlist and accepted in either; and that an allowlist
// entry with no link literal is reported.
func TestCatalogLiteralScanFixtures(t *testing.T) {
	t.Parallel()
	hand := scanCatalogLinkFixture(t, "handpath.go")
	require.Len(t, hand, 1)
	assert.True(t, hand[0].link)
	query := scanCatalogLinkFixture(t, "queryparam.go")
	require.Len(t, query, 1)
	assert.True(t, query[0].link)
	concat := scanCatalogLinkFixture(t, "concat.go")
	require.Len(t, concat, 1)
	assert.True(t, concat[0].link)
	split := scanCatalogLinkFixture(t, "split.go")
	require.Len(t, split, 2, "a named constant and a literal joined by +, and a parenthesized split")
	assert.True(t, split[0].link)
	assert.True(t, split[1].link)
	assert.Equal(t, "?link=index", split[0].text)
	cross := scanCatalogLinkFixture(t, "cross.go")
	require.Len(t, cross, 1, "a constant of another package")
	assert.True(t, cross[0].link)
	assert.Equal(t, "?link=index", cross[0].text)
	unfiltered := scanCatalogLinkFixture(t, "unfiltered.go")
	require.Len(t, unfiltered, 2)
	assert.False(t, unfiltered[0].link)
	assert.False(t, unfiltered[1].link)
	assert.Empty(t, scanCatalogLinkFixture(t, "clean.go"))

	assert.Len(t, catalogLiteralProblems(map[string][]catalogLiteralFinding{"a/b.go": hand}, nil), 1)
	assert.Empty(t, catalogLiteralProblems(map[string][]catalogLiteralFinding{catalogLinkFile: hand}, nil))
	assert.Len(t, catalogLiteralProblems(map[string][]catalogLiteralFinding{catalogLinkFile: unfiltered}, nil), 2,
		"a filter-changing literal is reported in the link file too")
	byHand := map[string][]catalogLiteralFinding{"a/b.go": hand}
	assert.Empty(t, catalogLiteralProblems(byHand, map[string]string{"a/b.go": "a reason"}), "the allowlist silences its file")
	assert.Equal(t, []string{"allowlist entry c/d.go matched nothing: remove it"},
		catalogLiteralProblems(byHand, map[string]string{"a/b.go": "a reason", "c/d.go": "a reason"}))
}

// TestCatalogLinkFixturesAreAllListed asserts the fixture directory holds
// exactly the files the scan's tests read.
func TestCatalogLinkFixturesAreAllListed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(catalogLinkFixturesDir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, catalogLinkFixtureFiles, names)
}
