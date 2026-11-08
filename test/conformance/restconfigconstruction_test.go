package conformance

import (
	"fmt"
	"go/ast"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// Every request to the Rancher server through client-go rides a transport
// that drops the Retry-After header of a 5xx and limits the one of a 429, and
// that transport is part of the config kube.RESTConfig builds. A config
// made any other way has none, and a command built on it waits on a server
// that is down. The scan below reports each place production code makes or
// copies a client-go config: a rest.Config composite literal (also behind &),
// new(rest.Config), a var of the type, and the functions that build or copy
// one (rest.CopyConfig, rest.InClusterConfig, rest.AnonymousClientConfig,
// exec.LoadExecCredentialFromEnv and the New, Build and Load functions of
// clientcmd); and, outside internal/client/kube, an assignment to the
// WrapTransport or Transport of a config and a call of its Wrap. Each place
// is allowlisted as "<file>:<function>" with its count and the reason its
// config needs no recorded transport. A value copy of a config that came
// from kube.RESTConfig (c := *rc) is no construction. The scan covers every
// non-test file of the module except the test servers and test support
// packages listed in restConfigSkipped.

const execPath = "k8s.io/client-go/tools/auth/exec"

// restConfigSkipped are the packages the scan does not read: test servers and
// test support that no release binary links.
var restConfigSkipped = []string{
	"internal/testrancher",
	"internal/envtestlog",
	"internal/testtransport",
}

// restConfigAllowed maps "<file>:<function>" to the places that make or copy
// a client-go config and why the config needs no recorded transport.
var restConfigAllowed = map[string]requestAllowance{
	"internal/client/kube/restconfig.go:RESTConfig":  {1, "the builder every Rancher config comes from: it adds the recorded transport"},
	"internal/client/kube/restconfig.go:TableClient": {1, "a copy of a config that came from kube.RESTConfig, with a content type for the table reads"},
	"internal/token/directwait.go:loadExecCluster":   {1, "the direct endpoint of a kubeconfig's exec entry: not the Rancher server, so it keeps no Rancher transport on purpose"},
}

// restConfigMatches returns one match per place pkg's production files make
// or copy a client-go config.
func restConfigMatches(pkg *packages.Package, wd string) []requestMatch {
	var out []requestMatch
	inKube := pkg.PkgPath == kubePackage
	for _, f := range pkg.Syntax {
		name := pkg.Fset.Position(f.Pos()).Filename
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		rel, err := filepath.Rel(wd, name)
		if err != nil {
			rel = name
		}
		rel = filepath.ToSlash(rel)
		for _, decl := range f.Decls {
			fn := "(package level)"
			if fd, ok := decl.(*ast.FuncDecl); ok {
				fn = fd.Name.Name
			}
			report := func(n ast.Node, what string) {
				out = append(out, requestMatch{rel + ":" + fn, fmt.Sprintf("%s:%d: %s", rel, pkg.Fset.Position(n.Pos()).Line, what)})
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CompositeLit:
					if isRESTConfig(pkg.TypesInfo.TypeOf(x)) {
						report(x, "rest.Config literal")
					}
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 && isRESTConfig(pkg.TypesInfo.TypeOf(x.Args[0])) {
						report(x, "new(rest.Config)")
					}
				case *ast.ValueSpec:
					if x.Type != nil && isRESTConfig(pkg.TypesInfo.TypeOf(x.Type)) {
						report(x, "var of rest.Config")
					}
				case *ast.AssignStmt:
					if inKube {
						return true
					}
					for _, lhs := range x.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if ok && (sel.Sel.Name == "WrapTransport" || sel.Sel.Name == "Transport") && isRESTConfig(configDeref(pkg.TypesInfo.TypeOf(sel.X))) {
							report(sel, "assignment to the "+sel.Sel.Name+" of a rest.Config")
						}
					}
				case *ast.Ident:
					fnObj, ok := pkg.TypesInfo.Uses[x].(*types.Func)
					if !ok || fnObj.Pkg() == nil {
						return true
					}
					if what := configFunction(fnObj, inKube); what != "" {
						report(x, what)
					}
				}
				return true
			})
		}
	}
	return out
}

// configDeref is t without one level of pointer.
func configDeref(t types.Type) types.Type {
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		return p.Elem()
	}
	return t
}

// isRESTConfig reports whether t is client-go's rest.Config.
func isRESTConfig(t types.Type) bool {
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == restPath && n.Obj().Name() == "Config"
}

// configFunction names a function that builds or copies a client-go config,
// or the Wrap method of a config outside internal/client/kube, or "".
func configFunction(fn *types.Func, inKube bool) string {
	path, name := fn.Pkg().Path(), fn.Name()
	recv := receiverName(fn)
	switch {
	case path == restPath && recv == "" && (name == "CopyConfig" || name == "InClusterConfig" || name == "AnonymousClientConfig"):
		return "rest." + name
	case path == restPath && recv == "Config" && name == "Wrap" && !inKube:
		return "call of Wrap on a rest.Config"
	case path == execPath && recv == "" && name == "LoadExecCredentialFromEnv":
		return "exec." + name
	case path == clientcmdPath && recv == "" && (strings.HasPrefix(name, "New") || strings.HasPrefix(name, "Build") || strings.HasPrefix(name, "Load")):
		return "clientcmd." + name
	}
	return ""
}

// TestEveryClientGoConfigIsMadeWhereListed asserts every place production
// code makes or copies a client-go config is in restConfigAllowed with its
// count and its reason, so a config built any other way, with no recorded
// transport, fails here. A new place, a changed count and a stale row fail.
func TestEveryClientGoConfigIsMadeWhereListed(t *testing.T) {
	t.Parallel()
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	var matches []requestMatch
	for _, pkg := range pkgs {
		skipped := false
		for _, s := range restConfigSkipped {
			skipped = skipped || strings.HasPrefix(pkg.PkgPath, modulePrefix+s)
		}
		if !skipped {
			matches = append(matches, restConfigMatches(pkg, wd)...)
		}
	}
	for _, problem := range checkRequestMatches(matches, restConfigAllowed) {
		t.Error(problem)
	}
	for key, a := range restConfigAllowed {
		assert.NotEmpty(t, a.why, key)
	}
}

// loadRESTConfigFixture type-checks one fixture package of the config scan.
func loadRESTConfigFixture(t *testing.T, name string) *packages.Package {
	t.Helper()
	pkgs, err := loadPackages("./test/conformance/testdata/restconfig/"+name, "")
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	return pkgs[0]
}

// TestRESTConfigScanFixtures asserts the scan reports each shape of
// construction in a planted fixture, one file per shape, and nothing for a
// fixture that only hands configs around.
func TestRESTConfigScanFixtures(t *testing.T) {
	t.Parallel()
	wd, err := os.Getwd()
	require.NoError(t, err)
	kinds := func(ms []requestMatch) []string {
		var out []string
		for _, m := range ms {
			_, rest, _ := strings.Cut(m.text, ": ")
			out = append(out, filepath.Base(m.key[:strings.LastIndex(m.key, ":")])+" "+m.key[strings.LastIndex(m.key, ":")+1:]+" "+rest)
		}
		slices.Sort(out)
		return out
	}
	assert.Empty(t, restConfigMatches(loadRESTConfigFixture(t, "clean"), wd))
	assert.Equal(t, []string{
		"functions.go Functions clientcmd.BuildConfigFromFlags",
		"functions.go Functions clientcmd.LoadFromFile",
		"functions.go Functions clientcmd.NewDefaultClientConfigLoadingRules",
		"functions.go Functions exec.LoadExecCredentialFromEnv",
		"functions.go Functions rest.AnonymousClientConfig",
		"functions.go Functions rest.CopyConfig",
		"functions.go Functions rest.InClusterConfig",
		"literal.go Literal rest.Config literal",
		"newvar.go New new(rest.Config)",
		"newvar.go Var var of rest.Config",
		"transport.go Transport assignment to the Transport of a rest.Config",
		"transport.go Transport assignment to the WrapTransport of a rest.Config",
		"transport.go Transport call of Wrap on a rest.Config",
	}, kinds(restConfigMatches(loadRESTConfigFixture(t, "planted"), wd)))
}

// TestRESTConfigAllowlistRules asserts a construction outside the list is
// reported, a changed count fails and an entry that matches nothing is stale.
func TestRESTConfigAllowlistRules(t *testing.T) {
	t.Parallel()
	row := "internal/client/kube/restconfig.go:RESTConfig"
	one := requestMatch{row, "internal/client/kube/restconfig.go:90: rest.Config literal"}
	allowed := map[string]requestAllowance{row: restConfigAllowed[row]}
	assert.Empty(t, checkRequestMatches([]requestMatch{one}, allowed))
	elsewhere := requestMatch{"internal/cli/engine.go:Root", "internal/cli/engine.go:9: rest.Config literal"}
	assert.Len(t, checkRequestMatches([]requestMatch{one, elsewhere}, allowed), 1, "a literal in a function with no row")
	assert.Len(t, checkRequestMatches([]requestMatch{one, {row, "internal/client/kube/restconfig.go:95: rest.Config literal"}}, allowed), 1, "a second literal in a listed function")
	assert.Len(t, checkRequestMatches(nil, allowed), 1, "a stale row")
}
