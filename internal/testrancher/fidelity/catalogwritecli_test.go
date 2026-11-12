package fidelity

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"maps"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseCLIFile parses one file of the CLI's source, relative to this
// package's directory.
func parseCLIFile(t *testing.T, rel string) (*token.FileSet, *ast.File) {
	t.Helper()
	src, err := os.ReadFile(rel)
	require.NoError(t, err)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, rel, src, 0)
	require.NoError(t, err)
	return fset, file
}

// stringConsts returns the string constants a file declares, by name.
func stringConsts(file *ast.File) map[string]string {
	out := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range spec.Names {
			if i >= len(spec.Values) {
				continue
			}
			if lit, ok := spec.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if v, err := strconv.Unquote(lit.Value); err == nil {
					out[name.Name] = v
				}
			}
		}
		return true
	})
	return out
}

// TestCatalogWriteActionsSendTheCLIsFieldManagerAndAccept asserts the
// install and uninstall observations send the field manager query
// parameter and the Accept header the CLI's action client sends: the
// constants of internal/client/kube/fieldmanager.go, and the Accept value
// the function of internal/cli/engine.go that builds the action request
// with kube.NewWriteRequest sets.
func TestCatalogWriteActionsSendTheCLIsFieldManagerAndAccept(t *testing.T) {
	t.Parallel()
	_, fm := parseCLIFile(t, "../../client/kube/fieldmanager.go")
	consts := stringConsts(fm)
	assert.Equal(t, consts["FieldManager"], catalogFieldManager)
	assert.Equal(t, consts["fieldManagerParam"], catalogFieldManagerParam)

	_, engine := parseCLIFile(t, "../../cli/engine.go")
	var accepts []string
	for _, decl := range engine.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		builds := false
		var fnAccepts []string
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "kube" && sel.Sel.Name == "NewWriteRequest" {
				builds = true
			}
			if sel.Sel.Name == "Set" && len(call.Args) == 2 {
				key, kok := call.Args[0].(*ast.BasicLit)
				val, vok := call.Args[1].(*ast.BasicLit)
				if kok && vok && key.Value == `"Accept"` {
					v, _ := strconv.Unquote(val.Value)
					fnAccepts = append(fnAccepts, v)
				}
			}
			return true
		})
		if builds {
			accepts = append(accepts, fnAccepts...)
		}
	}
	require.NotEmpty(t, accepts, "the action client of internal/cli/engine.go sets an Accept header")
	for _, a := range accepts {
		assert.Equal(t, a, catalogActionAccept)
	}

	sent := 0
	for _, p := range catalogWriteProbes() {
		for _, o := range p.Observations {
			if o.Request.Method != http.MethodPost {
				continue
			}
			sent++
			assert.Equal(t, []string{catalogFieldManager}, o.Request.Query[catalogFieldManagerParam], "%s %s", p.Name, o.Name)
			assert.Equal(t, catalogActionAccept, o.Request.Headers["Accept"], "%s %s", p.Name, o.Name)
		}
	}
	assert.Equal(t, 3, sent, "two installs and one uninstall")
}

// jsonTags returns, for the struct type name in file, each field's JSON
// name and whether the tag says omitempty.
func jsonTags(t *testing.T, file *ast.File, name string) map[string]bool {
	t.Helper()
	tag := regexp.MustCompile(`json:"([^",]+)(,omitempty)?"`)
	out := map[string]bool{}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok || spec.Name.Name != name {
			return true
		}
		st, ok := spec.Type.(*ast.StructType)
		require.True(t, ok, "%s is a struct", name)
		found = true
		for _, f := range st.Fields.List {
			if f.Tag == nil {
				continue
			}
			raw, _ := strconv.Unquote(f.Tag.Value)
			if m := tag.FindStringSubmatch(raw); m != nil {
				out[m[1]] = m[2] != ""
			}
		}
		return false
	})
	require.True(t, found, "type %s", name)
	return out
}

// TestCatalogInstallBodyIsTheCLIs asserts the install body's field names
// and defaults are the CLI's (internal/engine/catalog/action.go and
// install.go): every field the CLI always sends, the optional fields
// app install sends for this chart (values), none it leaves out, the
// timeout of defaultHelmTimeout as the CLI writes it, and wait true, which
// app install sends unless --wait is set otherwise.
func TestCatalogInstallBodyIsTheCLIs(t *testing.T) {
	t.Parallel()
	_, action := parseCLIFile(t, "../../engine/catalog/action.go")
	body := decodedInstallBody(t)

	required := func(tags map[string]bool, sentOptional ...string) []string {
		var keys []string
		for k, omit := range tags {
			if !omit || slices.Contains(sentOptional, k) {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		return keys
	}
	assert.Equal(t, required(jsonTags(t, action, "installBody")), slices.Sorted(maps.Keys(body)), "the install body's fields")
	chart := installChart(t, body)
	assert.Equal(t, required(jsonTags(t, action, "installChart"), "values"), slices.Sorted(maps.Keys(chart)), "the chart entry's fields")

	var timeout time.Duration
	ast.Inspect(action, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range spec.Names {
			if name.Name != "defaultHelmTimeout" || i >= len(spec.Values) {
				continue
			}
			expr, ok := spec.Values[i].(*ast.BinaryExpr)
			require.True(t, ok, "defaultHelmTimeout is <number> * time.<unit>")
			lit, lok := expr.X.(*ast.BasicLit)
			unit, uok := expr.Y.(*ast.SelectorExpr)
			require.True(t, lok && uok && expr.Op == token.MUL, "defaultHelmTimeout is <number> * time.<unit>")
			n, err := strconv.Atoi(lit.Value)
			require.NoError(t, err)
			units := map[string]time.Duration{"Second": time.Second, "Minute": time.Minute, "Hour": time.Hour}
			require.Contains(t, units, unit.Sel.Name)
			timeout = time.Duration(n) * units[unit.Sel.Name]
		}
		return true
	})
	require.NotZero(t, timeout, "defaultHelmTimeout is declared in action.go")
	assert.Equal(t, timeout.String(), body["timeout"], "the timeout app install sends by default")

	fset, install := parseCLIFile(t, "../../engine/catalog/install.go")
	var wait string
	ast.Inspect(install, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Wait" {
			var b strings.Builder
			require.NoError(t, printer.Fprint(&b, fset, kv.Value))
			wait = b.String()
		}
		return true
	})
	assert.Equal(t, "!in.IsSet(flagHelmWait) || in.Bool(flagHelmWait)", wait, "app install sends wait true unless --wait is set otherwise")
	assert.Equal(t, true, body["wait"])
}
