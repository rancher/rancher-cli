package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The OTLP/HTTP wire format has one home: internal/telemetrysend/otlp.go. The
// names only OTLP uses are searched in every production Go file of the
// repository; the content type application/json is also the Accept header of
// requests to Rancher, so it is searched only in the packages that can send
// or encode events, with the allowlist below.

const wireFormatFile = "internal/telemetrysend/otlp.go"

// wireFormatDirs are the packages that encode, record, send or command
// telemetry. Every production package that imports internal/telemetrysend is
// in the list.
var wireFormatDirs = []string{
	"internal/telemetrysend",
	"internal/telemetry",
	"internal/cli",
	"internal/commands/telemetrycmd",
}

// wireFormatOnlyOTLP are the names only OTLP uses.
var wireFormatOnlyOTLP = []string{"resourceLogs", "logRecords", "partialSuccess", "scopeLogs", "/v1/logs"}

// wireFormatContentType is searched in wireFormatDirs only.
const wireFormatContentType = "application/json"

// wireFormatAllowance is one reviewed use of a searched text outside the
// encoder: the file, the text, how many times and why.
type wireFormatAllowance struct {
	file, text string
	count      int
	reason     string
}

var wireFormatAllowlist = []wireFormatAllowance{
	{"internal/cli/engine.go", wireFormatContentType, 3, "the Accept header of requests to Rancher, and the Accept and Content-Type of the steve action POST; not telemetry"},
}

// wireFormatConfig is what the search needs, so the fixtures can use their
// own.
type wireFormatConfig struct {
	dirs      []string
	allowed   string
	allowlist []wireFormatAllowance
}

// constantScope is the context a constant is folded in: the directory of its
// file and the import names of that file. Each import name maps to the
// directory of the package of this module that it names.
type constantScope struct {
	dir     string
	imports map[string]string
}

// stringConstant is a package-level constant with the scope of the file that
// declares it.
type stringConstant struct {
	value ast.Expr
	scope constantScope
}

// directoryPackageNames returns the sorted package clause names of the files of
// each directory.
func directoryPackageNames(files []*ast.File, paths []string) map[string][]string {
	out := map[string][]string{}
	for i, f := range files {
		dir := path.Dir(paths[i])
		if !slices.Contains(out[dir], f.Name.Name) {
			out[dir] = append(out[dir], f.Name.Name)
			slices.Sort(out[dir])
		}
	}
	return out
}

// fileScopes returns the scope of each file. The import names are the explicit
// names of imports of packages of this module, and the package clause name of
// the imported directory when an import has none. A directory whose files
// declare several package names is imported under each of them, and
// mixedPackageImports reports it. Imports of a directory outside the parsed
// files are left out.
func fileScopes(files []*ast.File, paths []string) []constantScope {
	packageNames := directoryPackageNames(files, paths)
	scopes := make([]constantScope, len(files))
	for i, f := range files {
		scope := constantScope{dir: path.Dir(paths[i]), imports: map[string]string{}}
		for _, imp := range f.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil || !strings.HasPrefix(importPath, modulePrefix) {
				continue
			}
			dir := strings.TrimPrefix(importPath, modulePrefix)
			names := packageNames[dir]
			if len(names) == 0 {
				continue
			}
			if imp.Name != nil {
				names = []string{imp.Name.Name}
			}
			for _, name := range names {
				scope.imports[name] = dir
			}
		}
		scopes[i] = scope
	}
	return scopes
}

// mixedPackageImports returns one line for each directory of this module that
// a file imports and whose files declare more than one package name: without
// type information the scan cannot tell which name an import of it means.
func mixedPackageImports(files []*ast.File, paths []string) []string {
	packageNames := directoryPackageNames(files, paths)
	var out []string
	for _, f := range files {
		for _, imp := range f.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil || !strings.HasPrefix(importPath, modulePrefix) {
				continue
			}
			dir := strings.TrimPrefix(importPath, modulePrefix)
			if names := packageNames[dir]; len(names) > 1 {
				out = append(out, dir+": files declare the packages "+strings.Join(names, ", ")+"; keep one package name in each directory the scan follows")
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// packageStringConstants maps the name of each package-level constant of a
// directory to its value expression and the scope of the file that declares
// it.
func packageStringConstants(files []*ast.File, scopes []constantScope) map[string]map[string]stringConstant {
	out := map[string]map[string]stringConstant{}
	for i, f := range files {
		dir := scopes[i].dir
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				if len(vs.Values) != len(vs.Names) {
					continue
				}
				if out[dir] == nil {
					out[dir] = map[string]stringConstant{}
				}
				for j, name := range vs.Names {
					out[dir][name.Name] = stringConstant{value: vs.Values[j], scope: scopes[i]}
				}
			}
		}
	}
	return out
}

// foldString returns the value of e, folded in scope, when it is a string
// constant built from literals, +, parentheses, constants of the same
// directory and constants of other packages of this module named through an
// import name of the file. The value of a constant is folded in the scope of
// the file that declares it. The scan does not track Go scopes: a local name
// equal to an import name of the file is treated as that import. Not
// followed: package-level variables, function results, names built at run
// time, constants of packages outside this module, a constant with an
// implicitly repeated value (const ( A = "x"; B )) and a constant built by a
// conversion (const T = Kind("x")).
func foldString(e ast.Expr, scope constantScope, consts map[string]map[string]stringConstant, seen map[string]bool) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return foldString(x.X, scope, consts, seen)
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		l, ok := foldString(x.X, scope, consts, seen)
		if !ok {
			return "", false
		}
		r, ok := foldString(x.Y, scope, consts, seen)
		return l + r, ok
	case *ast.Ident:
		return foldConstant(scope.dir, x.Name, consts, seen)
	case *ast.SelectorExpr:
		pkg := identOf(x.X)
		if pkg == nil {
			return "", false
		}
		dir, ok := scope.imports[pkg.Name]
		if !ok {
			return "", false
		}
		return foldConstant(dir, x.Sel.Name, consts, seen)
	}
	return "", false
}

// foldConstant folds the constant name of the directory dir in the scope of
// the file that declares it. A constant that reaches itself is not followed.
func foldConstant(dir, name string, consts map[string]map[string]stringConstant, seen map[string]bool) (string, bool) {
	c, ok := consts[dir][name]
	key := dir + "\x00" + name
	if !ok || seen[key] {
		return "", false
	}
	seen[key] = true
	defer delete(seen, key)
	return foldString(c.value, c.scope, consts, seen)
}

// identOf returns e when it is an identifier, and nil otherwise.
func identOf(e ast.Expr) *ast.Ident {
	id, _ := e.(*ast.Ident)
	return id
}

// wireFormatHits counts, per file and searched text, the string constants
// that contain the text, compared without regard to case. The outermost
// expression that folds to a string is the unit: a literal, a concatenation
// of literals, a named constant of this module or a concatenation that uses
// one. A constant counts at every use in every file of the module, whichever
// package declares it, and once at its own declaration. A struct tag is a
// literal.
func wireFormatHits(files []*ast.File, paths []string, cfg wireFormatConfig) map[string]int {
	scopes := fileScopes(files, paths)
	consts := packageStringConstants(files, scopes)
	hits := map[string]int{}
	for i, f := range files {
		file := paths[i]
		searched := wireFormatOnlyOTLP
		if slices.Contains(cfg.dirs, path.Dir(file)) {
			searched = append(slices.Clone(searched), wireFormatContentType)
		}
		declared := map[*ast.Ident]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ImportSpec:
				return false
			case *ast.ValueSpec:
				for _, name := range x.Names {
					declared[name] = true
				}
			case *ast.SelectorExpr:
				declared[x.Sel] = true
			}
			e, ok := n.(ast.Expr)
			if !ok || declared[identOf(e)] {
				return true
			}
			value, ok := foldString(e, scopes[i], consts, map[string]bool{})
			if !ok {
				return true
			}
			for _, text := range searched {
				if strings.Contains(strings.ToLower(value), strings.ToLower(text)) {
					hits[file+"\x00"+text]++
				}
			}
			return false
		})
	}
	return hits
}

// wireFormatFindings reports every searched text outside the encoder's file
// and outside the allowlist, every allowlist entry whose count differs from
// the hits, and every directory of the list that does not exist.
func wireFormatFindings(hits map[string]int, cfg wireFormatConfig, root string) []string {
	var out []string
	allowed := map[string]wireFormatAllowance{}
	for _, a := range cfg.allowlist {
		allowed[a.file+"\x00"+a.text] = a
	}
	for key, n := range hits {
		file, text, _ := strings.Cut(key, "\x00")
		if file == cfg.allowed {
			continue
		}
		if a, ok := allowed[key]; ok {
			if a.count != n {
				out = append(out, fmt.Sprintf("%s holds %q %d time(s); the allowlist says %d", file, text, n, a.count))
			}
			continue
		}
		out = append(out, fmt.Sprintf("%s holds %q; only %s may", file, text, cfg.allowed))
	}
	for key, a := range allowed {
		if hits[key] == 0 {
			out = append(out, fmt.Sprintf("allowlist entry %s %q matched nothing: remove it", a.file, a.text))
		}
	}
	for _, dir := range cfg.dirs {
		if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir))); err != nil || !info.IsDir() {
			out = append(out, "the directory "+dir+" does not exist; update the list")
		}
	}
	slices.Sort(out)
	return out
}

// unlistedImporters returns the directories of production files that import
// internal/telemetrysend and are not in dirs.
func unlistedImporters(files []*ast.File, paths []string, dirs []string) []string {
	var out []string
	for i, f := range files {
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == modulePrefix+"internal/telemetrysend" && !slices.Contains(dirs, path.Dir(paths[i])) {
				out = append(out, path.Dir(paths[i]))
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// dotImportedModulePackages returns one line for each file that imports a
// package of this module with the name ".". The constants of such a package
// are bare names, which the search cannot tell from local ones.
func dotImportedModulePackages(files []*ast.File, paths []string) []string {
	var out []string
	for i, f := range files {
		for _, imp := range f.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			if err == nil && imp.Name != nil && imp.Name.Name == "." && strings.HasPrefix(importPath, modulePrefix) {
				out = append(out, paths[i]+": dot import of "+importPath+" hides its constants from the wire-format scan; import it by name")
			}
		}
	}
	slices.Sort(out)
	return out
}

// TestOnlyTheEncoderKnowsTheWireFormat asserts the names only OTLP uses
// (resourceLogs, logRecords, partialSuccess, scopeLogs, /v1/logs) appear in
// string constants only in internal/telemetrysend/otlp.go, in every
// production Go file outside testdata, and that application/json does the
// same in the packages that encode, record, send or command telemetry, apart
// from the one reviewed use in the allowlist. A constant is found by folding
// literals, + and the constants of this module, whether declared in the same
// package or imported from another through the import name of the file, not
// with type information. Each constant folds in the file that declares it.
// Not followed: package-level variables, function results, names built at run
// time, constants of packages outside this module, a constant with an
// implicitly repeated value and a constant built by a conversion. A local name
// equal to an import name of its file is treated as that import, which can add a
// hit and never hides one. Matching ignores case and looks inside the
// constant, so "Application/JSON" and "https://h/v1/logs" match. The test
// also asserts every directory of the list exists, every production package
// that imports internal/telemetrysend is in the list, no production file
// imports a package of this module with the name ".", no directory it imports
// declares two package names, and every allowlist entry matches its count.
func TestOnlyTheEncoderKnowsTheWireFormat(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	cfg := wireFormatConfig{dirs: wireFormatDirs, allowed: wireFormatFile, allowlist: wireFormatAllowlist}
	assert.Empty(t, wireFormatFindings(wireFormatHits(tree.files, tree.paths, cfg), cfg, "."))
	assert.Empty(t, unlistedImporters(tree.files, tree.paths, wireFormatDirs), "a package that imports internal/telemetrysend must be in wireFormatDirs")
	assert.Empty(t, dotImportedModulePackages(tree.files, tree.paths))
	assert.Empty(t, mixedPackageImports(tree.files, tree.paths))
	assert.Contains(t, wireFormatHits(tree.files, tree.paths, cfg), wireFormatFile+"\x00"+wireFormatContentType, "the encoder's file holds the content type")
	for _, a := range wireFormatAllowlist {
		assert.NotEmpty(t, a.reason, a.file)
	}
}

// parseFixtureDirs parses every Go file under testdata/wireformat, with the
// path of each file as the search sees it.
func parseFixtureDirs(t *testing.T) ([]*ast.File, []string) {
	t.Helper()
	const root = "test/conformance/testdata/wireformat"
	fset := token.NewFileSet()
	var files []*ast.File
	var paths []string
	require.NoError(t, filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.AllErrors)
		require.NoError(t, err)
		files = append(files, f)
		paths = append(paths, filepath.ToSlash(p))
		return nil
	}))
	require.NotEmpty(t, files)
	return files, paths
}

// TestWireFormatScanFixtures asserts the search finds a literal, a
// concatenation, a constant built from a named constant, a struct tag and a
// different case in the fixture packages, one hit each. It also asserts the
// search counts a constant imported from another package of the module, through
// a renamed import and through an import whose package name differs from its
// directory, folds a constant in the file that declares it (in the same package
// and across packages, and when two files of one directory import different
// packages under one name), and does not count a constant that holds no
// searched text. It also asserts a directory that holds files of two package
// names is reported when another file imports it. It accepts the allowed file and a clean file, does not search
// application/json outside the listed directories, and reports an allowlist
// entry that matches nothing or has the wrong count.
func TestWireFormatScanFixtures(t *testing.T) {
	t.Parallel()
	files, paths := parseFixtureDirs(t)
	const base = "test/conformance/testdata/wireformat/"
	var dirs []string
	for _, p := range paths {
		if !strings.HasPrefix(p, base+"outside/") {
			dirs = append(dirs, path.Dir(p))
		}
	}
	cfg := wireFormatConfig{dirs: dirs, allowed: base + "allowed/allowed.go"}
	hits := wireFormatHits(files, paths, cfg)
	assert.Equal(t, map[string]int{
		base + "literal/literal.go\x00/v1/logs":                       1,
		base + "concatenated/concatenated.go\x00/v1/logs":             1,
		base + "named/named.go\x00/v1/logs":                           1,
		base + "tag/tag.go\x00resourceLogs":                           1,
		base + "variant/variant.go\x00application/json":               1,
		base + "allowed/allowed.go\x00scopeLogs":                      1,
		base + "allowedrequest/allowedrequest.go\x00application/json": 1,
		base + "importeddef/importeddef.go\x00/v1/logs":               1,
		base + "importeddef/importeddef.go\x00application/json":       1,
		base + "importedchain/importedchain.go\x00/v1/logs":           1,
		base + "importeduse/importeduse.go\x00/v1/logs":               1,
		base + "importeduse/importeduse.go\x00application/json":       1,
		base + "importedsamedir/declare.go\x00/v1/logs":               1,
		base + "importedsamedir/use.go\x00/v1/logs":                   1,
		base + "importedsamename/first.go\x00/v1/logs":                1,
	}, hits)

	assert.Equal(t, []string{base + "importedmixed: files declare the packages mixeda, mixedb; keep one package name in each directory the scan follows"}, mixedPackageImports(files, paths))

	findings := wireFormatFindings(hits, cfg, ".")
	assert.Len(t, findings, 14, findings)
	for _, f := range findings {
		assert.False(t, strings.HasPrefix(f, base+"allowed/allowed.go"), "the allowed file is accepted")
	}

	cfg.allowlist = []wireFormatAllowance{{base + "allowedrequest/allowedrequest.go", wireFormatContentType, 1, "a reason"}}
	assert.Len(t, wireFormatFindings(hits, cfg, "."), 13, "an allowlist entry covers its count")
	cfg.allowlist = []wireFormatAllowance{{base + "allowedrequest/allowedrequest.go", wireFormatContentType, 2, "a reason"}}
	assert.Len(t, wireFormatFindings(hits, cfg, "."), 14, "a different count fails")
	cfg.allowlist = []wireFormatAllowance{{base + "clean/clean.go", wireFormatContentType, 1, "a reason"}}
	assert.Contains(t, wireFormatFindings(hits, cfg, "."), "allowlist entry "+base+"clean/clean.go \"application/json\" matched nothing: remove it")

	cfg.dirs = []string{"internal/nowhere"}
	assert.Contains(t, wireFormatFindings(nil, cfg, "."), "the directory internal/nowhere does not exist; update the list")
}

// TestWireFormatImporterRule asserts a production package outside the list
// that imports internal/telemetrysend is reported, and a listed one is not.
func TestWireFormatImporterRule(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	src := "package p\nimport _ \"" + modulePrefix + "internal/telemetrysend\"\n"
	f, err := parser.ParseFile(fset, "internal/other/p.go", src, parser.ImportsOnly)
	require.NoError(t, err)
	assert.Equal(t, []string{"internal/other"}, unlistedImporters([]*ast.File{f}, []string{"internal/other/p.go"}, wireFormatDirs))
	assert.Empty(t, unlistedImporters([]*ast.File{f}, []string{"internal/cli/p.go"}, wireFormatDirs))
}

// TestWireFormatDotImportRule asserts a production file that imports a package
// of this module with the name "." is reported, and a dot import of another
// package and a named import of a package of this module are not.
func TestWireFormatDotImportRule(t *testing.T) {
	t.Parallel()
	parse := func(name, imports string) *ast.File {
		f, err := parser.ParseFile(token.NewFileSet(), name, "package p\nimport (\n"+imports+"\n)\n", parser.ImportsOnly)
		require.NoError(t, err)
		return f
	}
	dotted := parse("internal/other/p.go", `. "`+modulePrefix+`internal/telemetrysend"`)
	assert.Equal(t,
		[]string{"internal/other/p.go: dot import of " + modulePrefix + "internal/telemetrysend hides its constants from the wire-format scan; import it by name"},
		dotImportedModulePackages([]*ast.File{dotted}, []string{"internal/other/p.go"}))

	standard := parse("internal/other/q.go", `. "net/http"`)
	named := parse("internal/other/r.go", `ts "`+modulePrefix+`internal/telemetrysend"`)
	assert.Empty(t, dotImportedModulePackages([]*ast.File{standard, named}, []string{"internal/other/q.go", "internal/other/r.go"}))
}
