package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The YAML library behind sigs.k8s.io/yaml refuses DEL, U+0080 to U+009F,
// U+FFFE and U+FFFF and changes U+0085 into a space: every YAML write goes
// through internal/yamlwrite, which writes them as escapes. The scan reads
// every non-test Go file outside a testdata directory and refuses a use of a
// function that writes YAML, or an import of a package that reaches a YAML
// encoder another way, unless the file is under internal/yamlwrite or a
// reviewed row names the function and says why the write is safe. It matches a
// package by its import path: an import alias does not hide a call. It
// refuses every reference to a listed function, called or held as a value. It
// does not see a call through a function value made elsewhere, a method value,
// or a YAML encoder inside another dependency; the round trip tests of
// internal/config cover the writers known today.

const (
	sigsYAMLPath      = "sigs.k8s.io/yaml"
	yamlV2Path        = "gopkg.in/yaml.v2"
	yamlV3Path        = "gopkg.in/yaml.v3"
	goYAMLV2Path      = "go.yaml.in/yaml/v2"
	goYAMLV3Path      = "go.yaml.in/yaml/v3"
	clientcmdPath     = "k8s.io/client-go/tools/clientcmd"
	yamlWritePackage  = "internal/yamlwrite/"
	yamlImportMarker  = "import"
	yamlWriteGuidance = "route the write through yamlwrite.Marshal or add a reviewed row with a reason"
)

// yamlWriteFunctions lists, per import path, the functions that write YAML.
var yamlWriteFunctions = map[string][]string{
	sigsYAMLPath:  {"Marshal", "JSONToYAML"},
	yamlV2Path:    {"Marshal", "NewEncoder"},
	yamlV3Path:    {"Marshal", "NewEncoder"},
	goYAMLV2Path:  {"Marshal", "NewEncoder"},
	goYAMLV3Path:  {"Marshal", "NewEncoder"},
	clientcmdPath: {"Write", "WriteToFile", "ModifyConfig"},
}

// yamlWriteDefaultNames is the name a file gives each package it imports
// without an alias: the package's own name, which is not the last element of
// its import path for the YAML libraries.
var yamlWriteDefaultNames = map[string]string{
	sigsYAMLPath:  "yaml",
	yamlV2Path:    "yaml",
	yamlV3Path:    "yaml",
	goYAMLV2Path:  "yaml",
	goYAMLV3Path:  "yaml",
	clientcmdPath: "clientcmd",
}

// yamlWriteImports lists the packages that reach a YAML encoder without a
// call the scan can match; a non-test file may not import any of them.
var yamlWriteImports = []string{
	"sigs.k8s.io/yaml/goyaml.v2",
	"sigs.k8s.io/yaml/goyaml.v3",
	"k8s.io/apimachinery/pkg/runtime/serializer",
	"k8s.io/apimachinery/pkg/runtime/serializer/json",
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml",
	"k8s.io/client-go/tools/clientcmd/api/latest",
}

// yamlWriteRow is one reviewed function that writes YAML outside
// internal/yamlwrite.
type yamlWriteRow struct {
	file     string
	function string
	// reason says why the write is safe, as the code stands.
	reason string
}

// yamlWriteRows lists every function outside internal/yamlwrite that uses a
// function that writes YAML. A method is written Receiver.Method, without a
// pointer star; a row for an import names the function "import".
var yamlWriteRows = []yamlWriteRow{
	{"internal/skillinstall/content.go", "stampSkillMD", "writes the front matter of an embedded skill, text the build fixes"},
	{"internal/testrancher/testrancher.go", "kubeconfigValueFixture", "test support; the package is not reachable from production code"},
	{"internal/engine/kubeconfig/command.go", "kubeconfigCmd.Execute", "clientcmd.Write runs client-go's own YAML serializer, which has no hook for the escape"},
}

// yamlWriteUse is one use of a function that writes YAML, or one import of a
// package that reaches an encoder.
type yamlWriteUse struct {
	file     string
	function string
	what     string
	line     int
}

func (u yamlWriteUse) key() string { return u.file + ":" + u.function }

// yamlWriteImportName returns the name the file gives the package at
// importPath: the declared name, "." for a dot import, the package's default
// name for an import without an alias, and "" when the file does not import it
// or imports it only as _.
func yamlWriteImportName(f *ast.File, importPath string) string {
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil || p != importPath {
			continue
		}
		switch {
		case spec.Name == nil:
			if name, ok := yamlWriteDefaultNames[p]; ok {
				return name
			}
			return path.Base(p)
		case spec.Name.Name == "_":
			return ""
		default:
			return spec.Name.Name
		}
	}
	return ""
}

// scanYAMLWrites returns every use in f, a file at filePath.
func scanYAMLWrites(fset *token.FileSet, filePath string, f *ast.File) []yamlWriteUse {
	if strings.HasPrefix(filePath, yamlWritePackage) {
		return nil
	}
	var uses []yamlWriteUse
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err == nil && slices.Contains(yamlWriteImports, p) {
			uses = append(uses, yamlWriteUse{filePath, yamlImportMarker, "an import of " + p, fset.Position(spec.Pos()).Line})
		}
	}
	names := map[string]string{}
	var dotImports []string
	for p := range yamlWriteFunctions {
		switch name := yamlWriteImportName(f, p); name {
		case "":
		case ".":
			dotImports = append(dotImports, p)
		default:
			names[name] = p
		}
	}
	if len(names) == 0 && len(dotImports) == 0 {
		return uses
	}
	for _, decl := range f.Decls {
		function := packageLevelFunction
		if fd, ok := decl.(*ast.FuncDecl); ok {
			function = functionKey(fd)
		}
		add := func(n ast.Node, what string) {
			uses = append(uses, yamlWriteUse{filePath, function, what, fset.Position(n.Pos()).Line})
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				x, ok := ast.Unparen(n.X).(*ast.Ident)
				if !ok {
					return true
				}
				if importPath, imported := names[x.Name]; imported && slices.Contains(yamlWriteFunctions[importPath], n.Sel.Name) {
					add(n, x.Name+"."+n.Sel.Name+" of "+importPath)
				}
			case *ast.Ident:
				for _, p := range dotImports {
					if slices.Contains(yamlWriteFunctions[p], n.Name) {
						add(n, n.Name+" from a dot import of "+p)
					}
				}
			}
			return true
		})
	}
	return uses
}

// checkYAMLWrites returns one message per problem: a use with no row, a row
// that matches no use, a duplicate row and a row with no reason.
func checkYAMLWrites(uses []yamlWriteUse, rows []yamlWriteRow) []string {
	var problems []string
	listed := map[string]bool{}
	for _, row := range rows {
		key := row.file + ":" + row.function
		if listed[key] {
			problems = append(problems, fmt.Sprintf("the row for %s in %s is listed twice", row.function, row.file))
		}
		listed[key] = true
		if strings.TrimSpace(row.reason) == "" {
			problems = append(problems, fmt.Sprintf("the row for %s in %s has no reason", row.function, row.file))
		}
	}
	found := map[string]bool{}
	for _, u := range uses {
		found[u.key()] = true
		if !listed[u.key()] {
			problems = append(problems, fmt.Sprintf("%s:%d: %s in %s uses %s: %s", u.file, u.line, u.function, u.file, u.what, yamlWriteGuidance))
		}
	}
	for _, row := range rows {
		if !found[row.file+":"+row.function] {
			problems = append(problems, fmt.Sprintf("the row for %s in %s matches no use of a function that writes YAML", row.function, row.file))
		}
	}
	slices.Sort(problems)
	return problems
}

// TestEveryYAMLWriteGoesThroughYamlwrite asserts no non-test Go file outside
// internal/yamlwrite uses a function that writes YAML or imports a package
// that reaches a YAML encoder, except in a reviewed row.
func TestEveryYAMLWriteGoesThroughYamlwrite(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	var uses []yamlWriteUse
	for i, f := range tree.files {
		uses = append(uses, scanYAMLWrites(tree.fset, tree.paths[i], f)...)
	}
	assert.Empty(t, checkYAMLWrites(uses, yamlWriteRows))
}

// TestYAMLWriteScanFixtures asserts the scan on fixture sources: a plain
// call, a call through an import alias, JSONToYAML, a yaml.v3 NewEncoder, a
// clientcmd.Write, an import of one of the other packages and a dot import
// are refused; a use in internal/yamlwrite, a use in a function that has a
// row, and a Marshal of encoding/json are not.
func TestYAMLWriteScanFixtures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		path string
		src  string
		want []string
	}{
		{"a plain call", "internal/x/x.go", `package x
import "sigs.k8s.io/yaml"
func Save(v any) { yaml.Marshal(v) }
`, []string{"internal/x/x.go:Save"}},
		{"a call through an import alias", "internal/x/x.go", `package x
import y "sigs.k8s.io/yaml"
func Save(v any) { y.Marshal(v) }
`, []string{"internal/x/x.go:Save"}},
		{"JSONToYAML", "internal/x/x.go", `package x
import "sigs.k8s.io/yaml"
func Save(j []byte) { yaml.JSONToYAML(j) }
`, []string{"internal/x/x.go:Save"}},
		{"a yaml.v3 NewEncoder", "internal/x/x.go", `package x
import "gopkg.in/yaml.v3"
import "io"
func Save(w io.Writer) { yaml.NewEncoder(w) }
`, []string{"internal/x/x.go:Save"}},
		{"a go.yaml.in Marshal", "internal/x/x.go", `package x
import "go.yaml.in/yaml/v3"
func Save(v any) { yaml.Marshal(v) }
`, []string{"internal/x/x.go:Save"}},
		{"a clientcmd.Write", "internal/x/x.go", `package x
import "k8s.io/client-go/tools/clientcmd"
func Save() { clientcmd.Write(nil) }
`, []string{"internal/x/x.go:Save"}},
		{"a method", "internal/x/x.go", `package x
import "sigs.k8s.io/yaml"
type T struct{}
func (t *T) Save(v any) { yaml.Marshal(v) }
`, []string{"internal/x/x.go:T.Save"}},
		{"a package variable", "internal/x/x.go", `package x
import "sigs.k8s.io/yaml"
var save = yaml.Marshal
`, []string{"internal/x/x.go:package-level"}},
		{"a dot import", "internal/x/x.go", `package x
import . "sigs.k8s.io/yaml"
func Save(v any) { Marshal(v) }
`, []string{"internal/x/x.go:Save"}},
		{"an import of another way to an encoder", "internal/x/x.go", `package x
import _ "k8s.io/client-go/tools/clientcmd/api/latest"
`, []string{"internal/x/x.go:import"}},
		{"an import of the goyaml copy", "internal/x/x.go", `package x
import _ "sigs.k8s.io/yaml/goyaml.v3"
`, []string{"internal/x/x.go:import"}},
		{"an import of the apimachinery codec factory", "internal/x/x.go", `package x
import "k8s.io/apimachinery/pkg/runtime/serializer"
var _ = serializer.NewCodecFactory
`, []string{"internal/x/x.go:import"}},
		{"an import of the apimachinery json serializer", "internal/x/x.go", `package x
import _ "k8s.io/apimachinery/pkg/runtime/serializer/json"
`, []string{"internal/x/x.go:import"}},
		{"an import of the apimachinery yaml serializer", "internal/x/x.go", `package x
import _ "k8s.io/apimachinery/pkg/runtime/serializer/yaml"
`, []string{"internal/x/x.go:import"}},
		{"a call in internal/yamlwrite", "internal/yamlwrite/yamlwrite.go", `package yamlwrite
import "sigs.k8s.io/yaml"
func Save(v any) { yaml.Marshal(v) }
`, nil},
		{"a Marshal of encoding/json", "internal/x/x.go", `package x
import "encoding/json"
func Save(v any) { json.Marshal(v) }
`, nil},
		{"an Unmarshal", "internal/x/x.go", `package x
import "sigs.k8s.io/yaml"
func Load(b []byte, v any) { yaml.Unmarshal(b, v) }
`, nil},
		{"a local value named yaml", "internal/x/x.go", `package x
type w struct{}
func (w) Marshal(any) {}
func Save(v any) { yaml := w{}; yaml.Marshal(v) }
`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, tc.path, tc.src, parser.AllErrors)
			require.NoError(t, err)
			var got []string
			for _, u := range scanYAMLWrites(fset, tc.path, f) {
				got = append(got, u.key())
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestYAMLWriteScanSkipsTestFiles asserts a call in a test file is not
// scanned. The skip is the file walk's (parseSourceTree leaves out every
// _test.go file), and the case runs that walk:
// internal/config/yamlwriters_test.go calls sigs.k8s.io/yaml Marshal, the
// scan reports it when it reads the file directly, and the walk never hands
// it over.
func TestYAMLWriteScanSkipsTestFiles(t *testing.T) {
	t.Parallel()
	const testFile = "internal/config/yamlwriters_test.go"
	files, err := repositoryFiles()
	require.NoError(t, err)
	require.Contains(t, files, testFile)

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, testFile, nil, parser.AllErrors)
	require.NoError(t, err)
	assert.NotEmpty(t, scanYAMLWrites(fset, testFile, f), "%s holds no use the scan reports", testFile)

	tree := parseSourceTree()
	require.NoError(t, tree.err)
	assert.NotContains(t, tree.paths, testFile)
	for _, p := range tree.paths {
		assert.False(t, strings.HasSuffix(p, "_test.go"), "the file walk hands over the test file %s", p)
	}
}

// TestYAMLWriteRowCheckFixtures asserts the row check refuses a use with no
// row (the message names the file, the function and the line and says what to
// do), a row that matches no use, a duplicate row and a row with an empty
// reason, and accepts a row for each use.
func TestYAMLWriteRowCheckFixtures(t *testing.T) {
	t.Parallel()
	use := yamlWriteUse{file: "internal/x/x.go", function: "Save", what: "yaml.Marshal of sigs.k8s.io/yaml", line: 7}
	row := yamlWriteRow{"internal/x/x.go", "Save", "the text is fixed"}

	assert.Empty(t, checkYAMLWrites([]yamlWriteUse{use}, []yamlWriteRow{row}))

	got := checkYAMLWrites([]yamlWriteUse{use}, nil)
	require.Len(t, got, 1)
	for _, want := range []string{"internal/x/x.go:7", "Save", "yamlwrite.Marshal", "reviewed row with a reason"} {
		assert.Contains(t, got[0], want)
	}

	got = checkYAMLWrites(nil, []yamlWriteRow{row})
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "matches no use")

	got = checkYAMLWrites([]yamlWriteUse{use}, []yamlWriteRow{row, row})
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "listed twice")

	got = checkYAMLWrites([]yamlWriteUse{use}, []yamlWriteRow{{"internal/x/x.go", "Save", " "}})
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "no reason")
}
