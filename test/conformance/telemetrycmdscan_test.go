package conformance

import (
	"go/ast"
	"go/types"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// The telemetry commands make no request and read no credential. Their
// direct imports are closed (TestTelemetryDirectImportsAreClosed), and the
// scan below limits what they use of internal/config, the one allowed
// package that can reach a credential store: the names Load, FilePath,
// WithLock and Config of the package, and the Write method of a Config,
// whether it is called as a method value, as a method expression or through
// an embedded field. LoadAndStore and every other function or method that
// opens a store or a server entry is out. A call through an interface is not
// seen: a value of a Config method set would first have to be converted to an
// interface, and that conversion names no method.

const (
	telemetrycmdDir = "internal/commands/telemetrycmd"
	configPath      = modulePrefix + "internal/config"
)

var (
	allowedConfigNames   = []string{"Load", "FilePath", "WithLock", "Config"}
	allowedConfigMethods = []string{"Write"}
)

// configUseFindings loads the package at pattern and returns one line per
// use of internal/config the rule does not allow: a dot import, a
// package-level name outside allowedConfigNames (an alias is resolved to the
// package it imports), and a method call on a config.Config value outside
// allowedConfigMethods. Test files are not read.
func configUseFindings(t *testing.T, pattern string) []string {
	t.Helper()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports,
	}
	pkgs, err := packages.Load(cfg, pattern)
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	require.Empty(t, pkgs[0].Errors)
	p := pkgs[0]

	var out []string
	pos := func(n ast.Node) string {
		position := p.Fset.Position(n.Pos())
		return position.Filename[strings.LastIndex(position.Filename, "/")+1:] + ":" + strconv.Itoa(position.Line)
	}
	for _, f := range p.Syntax {
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == configPath && imp.Name != nil && imp.Name.Name == "." {
				out = append(out, pos(imp)+": dot import of internal/config")
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok {
				if pn, ok := p.TypesInfo.Uses[id].(*types.PkgName); ok && pn.Imported().Path() == configPath {
					if !slices.Contains(allowedConfigNames, sel.Sel.Name) {
						out = append(out, pos(sel)+": config."+sel.Sel.Name)
					}
				}
			}
			if s, ok := p.TypesInfo.Selections[sel]; ok && (s.Kind() == types.MethodVal || s.Kind() == types.MethodExpr) {
				// The method is judged by the receiver it is declared on, so a
				// method value, a method expression and a method promoted
				// through an embedded field are one case.
				if fn, ok := s.Obj().(*types.Func); ok && fn.Signature().Recv() != nil {
					recv := fn.Signature().Recv().Type()
					if ptr, ok := recv.(*types.Pointer); ok {
						recv = ptr.Elem()
					}
					if named, ok := types.Unalias(recv).(*types.Named); ok &&
						named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == configPath && named.Obj().Name() == "Config" &&
						!slices.Contains(allowedConfigMethods, sel.Sel.Name) {
						out = append(out, pos(sel)+": Config."+sel.Sel.Name)
					}
				}
			}
			return true
		})
	}
	slices.Sort(out)
	return out
}

// TestTelemetryCommandsUseOnlyTheAllowedConfigNames asserts the telemetry
// command package uses only the allowed names and methods of the config
// package, so it opens no credential store and picks no server.
func TestTelemetryCommandsUseOnlyTheAllowedConfigNames(t *testing.T) {
	assert.Empty(t, configUseFindings(t, "./"+telemetrycmdDir))
}

// TestConfigUseScanCatchesFixtures asserts the scan reports a call of
// config.LoadAndStore, a call of another Config method, as a method value,
// as a method expression and through an embedded field, the same call
// through an import alias and a dot import, and accepts a package that uses
// only the allowed names through an alias.
func TestConfigUseScanCatchesFixtures(t *testing.T) {
	const base = "./test/conformance/testdata/telemetrycmdscan/"
	for fixture, want := range map[string]string{
		"loadandstore": "loadandstore.go:9: config.LoadAndStore",
		"othermethod":  "othermethod.go:13: Config.Path",
		"aliased":      "aliased.go:9: config.LoadAndStore",
		"dotimport":    "dotimport.go:5: dot import of internal/config",
		"methodexpr":   "methodexpr.go:13: Config.Path",
		"embedded":     "embedded.go:16: Config.Path",
	} {
		got := configUseFindings(t, base+fixture)
		require.NotEmpty(t, got, fixture)
		assert.Equal(t, want, got[0], fixture)
	}
	assert.Empty(t, configUseFindings(t, base+"clean"))
}
