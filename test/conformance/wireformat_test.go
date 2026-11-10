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

// packageStringConstants maps the name of each package-level constant of a directory
// to its value expression.
func packageStringConstants(files []*ast.File, paths []string) map[string]map[string]ast.Expr {
	out := map[string]map[string]ast.Expr{}
	for i, f := range files {
		dir := path.Dir(paths[i])
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
					out[dir] = map[string]ast.Expr{}
				}
				for j, name := range vs.Names {
					out[dir][name.Name] = vs.Values[j]
				}
			}
		}
	}
	return out
}

// foldString returns the value of e when it is a string constant built from
// literals, +, parentheses and constants of the same directory. A constant
// imported from another package, a call and a name built at run time are not
// followed.
func foldString(e ast.Expr, consts map[string]ast.Expr, seen map[string]bool) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return foldString(x.X, consts, seen)
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		l, ok := foldString(x.X, consts, seen)
		if !ok {
			return "", false
		}
		r, ok := foldString(x.Y, consts, seen)
		return l + r, ok
	case *ast.Ident:
		value, ok := consts[x.Name]
		if !ok || seen[x.Name] {
			return "", false
		}
		seen[x.Name] = true
		defer delete(seen, x.Name)
		return foldString(value, consts, seen)
	}
	return "", false
}

// identOf returns e when it is an identifier, and nil otherwise.
func identOf(e ast.Expr) *ast.Ident {
	id, _ := e.(*ast.Ident)
	return id
}

// wireFormatHits counts, per file and searched text, the string constants
// that contain the text, compared without regard to case. The outermost
// expression that folds to a string is the unit: a literal, a concatenation
// of literals, a named constant or a concatenation that uses one. A struct
// tag is a literal.
func wireFormatHits(files []*ast.File, paths []string, cfg wireFormatConfig) map[string]int {
	consts := packageStringConstants(files, paths)
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
			value, ok := foldString(e, consts[path.Dir(file)], map[string]bool{})
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

// TestOnlyTheEncoderKnowsTheWireFormat asserts the names only OTLP uses
// (resourceLogs, logRecords, partialSuccess, scopeLogs, /v1/logs) appear in
// string constants only in internal/telemetrysend/otlp.go, in every
// production Go file outside testdata, and that application/json does the
// same in the packages that encode, record, send or command telemetry, apart
// from the one reviewed use in the allowlist. A constant is found by folding
// literals, + and the constants of the same directory, not with type
// information: a constant imported from another package and a name built at
// run time are not followed. Matching ignores case and looks inside the
// constant, so "Application/JSON" and "https://h/v1/logs" match. The test
// also asserts every directory of the list exists, every production package
// that imports internal/telemetrysend is in the list, and every allowlist
// entry matches its count.
func TestOnlyTheEncoderKnowsTheWireFormat(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	cfg := wireFormatConfig{dirs: wireFormatDirs, allowed: wireFormatFile, allowlist: wireFormatAllowlist}
	assert.Empty(t, wireFormatFindings(wireFormatHits(tree.files, tree.paths, cfg), cfg, "."))
	assert.Empty(t, unlistedImporters(tree.files, tree.paths, wireFormatDirs), "a package that imports internal/telemetrysend must be in wireFormatDirs")
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
// different case in the fixture packages, one hit each, accepts the allowed
// file and a clean file, does not search application/json outside the listed
// directories, and reports an allowlist entry that matches nothing or has the
// wrong count.
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
	}, hits)

	findings := wireFormatFindings(hits, cfg, ".")
	assert.Len(t, findings, 6, findings)
	for _, f := range findings {
		assert.False(t, strings.HasPrefix(f, base+"allowed/allowed.go"), "the allowed file is accepted")
	}

	cfg.allowlist = []wireFormatAllowance{{base + "allowedrequest/allowedrequest.go", wireFormatContentType, 1, "a reason"}}
	assert.Len(t, wireFormatFindings(hits, cfg, "."), 5, "an allowlist entry covers its count")
	cfg.allowlist = []wireFormatAllowance{{base + "allowedrequest/allowedrequest.go", wireFormatContentType, 2, "a reason"}}
	assert.Len(t, wireFormatFindings(hits, cfg, "."), 6, "a different count fails")
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
