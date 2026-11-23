package catalog

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// extensionCommandRow is one extension command that sends a catalog action:
// how to build it over the fake, the state it needs and the action it sends.
type extensionCommandRow struct {
	name string
	// constructor is the function the command registry calls to create the
	// command, as "package.Function".
	constructor string
	action      string
	// path is the request path of the action: the ClusterRepo's for install
	// and upgrade, the App's for uninstall.
	path string
	// withChart is true for the commands whose body names the chart (install
	// and upgrade); the body of the others is empty.
	withChart bool
	setup     func(*extFake)
	cmd       func(*extFake) runtime.MutatingCommand
}

// extensionCommandRows lists every extension command that sends an action.
// TestEveryRegisteredExtensionCommandHasARowOrAReason fails for a registered
// extension command that has no row and no reason to have none, and the tests
// below run for every row.
var extensionCommandRows = []extensionCommandRow{
	{name: "install", constructor: "catalog.NewExtensionInstall", action: "install", path: "/v1/catalog.cattle.io.clusterrepos/" + extRepo, withChart: true,
		cmd: func(f *extFake) runtime.MutatingCommand { return newTestExtensionInstall(f) }},
	{name: "upgrade", constructor: "catalog.NewExtensionUpgrade", action: "upgrade", path: "/v1/catalog.cattle.io.clusterrepos/" + extRepo, withChart: true,
		setup: func(f *extFake) { putExtApp(f, "deployed", extInstalledVersion, extRepo) },
		cmd:   func(f *extFake) runtime.MutatingCommand { return newTestExtensionUpgrade(f) }},
	{name: "uninstall", constructor: "catalog.NewExtensionUninstall", action: "uninstall", path: "/v1/catalog.cattle.io.apps/" + extUIPlugin + "/" + extChart,
		setup: func(f *extFake) { putExtApp(f, "deployed", extInstalledVersion, extRepo) },
		cmd:   func(f *extFake) runtime.MutatingCommand { return newTestExtensionUninstall(f) }},
}

// TestEveryExtensionActionBodyHasTheKeysOfTheRancherUI asserts, for each
// command of extensionCommandRows, the request it sends: the exact key set of
// the body (install and upgrade: namespace and charts, each chart with
// chartName, version, releaseName, values and annotations; uninstall: no key;
// no timeout, wait, noHooks or description), values always sent, the namespace
// cattle-ui-plugin-system, the chart name as the release name, and a path on
// the local cluster.
func TestEveryExtensionActionBodyHasTheKeysOfTheRancherUI(t *testing.T) {
	t.Parallel()
	for _, row := range extensionCommandRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			f := newExtFake(t)
			if row.setup != nil {
				row.setup(f)
			}
			_, exec := plan(t, row.cmd(f), extInput(extChart))
			_, err := exec(t.Context())
			require.NoError(t, err)
			require.Equal(t, 1, f.postCount())
			p := f.posts[0]
			assert.True(t, strings.HasSuffix(p.path, "?action="+row.action), p.path)
			assert.True(t, strings.HasPrefix(p.path, "/v1/"), p.path)
			assert.NotContains(t, p.path, "/k8s/clusters/")
			if !row.withChart {
				assert.Empty(t, p.body)
				assert.Equal(t, row.path+"?action="+row.action, p.path)
				return
			}
			assert.Equal(t, row.path+"?action="+row.action, p.path)
			assert.ElementsMatch(t, []string{"namespace", "charts"}, keysOf(p.body))
			assert.Equal(t, extUIPlugin, p.body["namespace"])
			charts, _ := p.body["charts"].([]any)
			require.Len(t, charts, 1)
			chart, _ := charts[0].(map[string]any)
			assert.ElementsMatch(t, []string{"chartName", "version", "releaseName", "values", "annotations"}, keysOf(chart))
			assert.Equal(t, extChart, chart["chartName"])
			assert.Equal(t, extChart, chart["releaseName"])
			values, isMap := chart["values"].(map[string]any)
			assert.True(t, isMap, "values is sent as an object")
			assert.Empty(t, values)
		})
	}
}

// extensionSharedCheckRow is one extension command that checks the feature
// and the namespace before any other read: how to run it over the fake up to
// those checks.
type extensionSharedCheckRow struct {
	name  string
	setup func(*extFake)
	run   func(context.Context, *extFake) error
}

// extensionSharedCheckRows is every row of extensionCommandRows, which stops at
// the Plan, and extension list, which has no action and stops at its checks.
func extensionSharedCheckRows() []extensionSharedCheckRow {
	var rows []extensionSharedCheckRow
	for _, row := range extensionCommandRows {
		rows = append(rows, extensionSharedCheckRow{name: row.name, setup: row.setup, run: func(ctx context.Context, f *extFake) error {
			_, _, err := row.cmd(f).Plan(ctx, extInput(extChart))
			return err
		}})
	}
	return append(rows, extensionSharedCheckRow{name: "list", run: func(ctx context.Context, f *extFake) error {
		_, err := NewExtensionList(f, f).(runtime.ReadCommand).Execute(ctx, extInput(extChart), nil)
		return err
	}})
}

// TestEveryExtensionCommandRunsTheSharedChecksFirst asserts, for each command
// of extensionSharedCheckRows, that the feature turned off and then the missing
// namespace end the command before any other read.
func TestEveryExtensionCommandRunsTheSharedChecksFirst(t *testing.T) {
	t.Parallel()
	for _, row := range extensionSharedCheckRows() {
		t.Run(row.name+" feature off", func(t *testing.T) {
			t.Parallel()
			f := newExtFake(t)
			if row.setup != nil {
				row.setup(f)
			}
			f.put("features", "", "uiextension", map[string]any{"spec": map[string]any{"value": false}})
			err := row.run(t.Context(), f)
			require.ErrorContains(t, err, "UI extensions are turned off on this server (feature uiextension)")
			assert.Equal(t, clierr.CodeValidation, errCode(err))
			assert.Equal(t, []string{"get:features//uiextension"}, f.reads)
			assert.Empty(t, f.root.requests())
		})
		t.Run(row.name+" namespace missing", func(t *testing.T) {
			t.Parallel()
			f := newExtFake(t)
			if row.setup != nil {
				row.setup(f)
			}
			f.remove("namespaces", "", extUIPlugin)
			err := row.run(t.Context(), f)
			require.ErrorContains(t, err, "namespace cattle-ui-plugin-system not found")
			assert.Equal(t, clierr.CodeNotFound, errCode(err))
			assert.Equal(t, []string{"get:features//uiextension", "get:namespaces//cattle-ui-plugin-system"}, f.reads)
			assert.Empty(t, f.root.requests())
		})
	}
}

// extensionCommandsWithoutAnAction lists the registered extension commands
// that send no catalog action, by constructor, with the way each one writes.
var extensionCommandsWithoutAnAction = map[string]string{
	"catalog.NewExtensionList":          "reads only",
	"catalog.NewExtensionAddRepository": "creates a ClusterRepo through the create effect",
}

// TestEveryRegisteredExtensionCommandHasARowOrAReason asserts that every
// command the command registry (internal/cli) puts in the extension group,
// whatever its constructor is called and takes, has a row in
// extensionCommandRows or a reason in extensionCommandsWithoutAnAction, and
// that every row and reason names a registered command. A command that
// sends an action cannot skip the tests of the rows.
func TestEveryRegisteredExtensionCommandHasARowOrAReason(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("..", "..", "cli")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var registered []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, 0)
		require.NoError(t, err)
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			var inGroup bool
			var maker *ast.FuncLit
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, _ := kv.Key.(*ast.Ident)
				switch {
				case key != nil && key.Name == "group":
					value, _ := kv.Value.(*ast.BasicLit)
					inGroup = value != nil && value.Value == `"extension"`
				case key != nil && key.Name == "make":
					maker, _ = kv.Value.(*ast.FuncLit)
				}
			}
			if !inGroup || maker == nil {
				return true
			}
			ast.Inspect(maker, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
						if pkg, ok := sel.X.(*ast.Ident); ok {
							registered = append(registered, pkg.Name+"."+sel.Sel.Name)
						}
					}
				}
				return true
			})
			return true
		})
	}
	require.NotEmpty(t, registered, "the registry has entries of the extension group")
	var accounted []string
	for _, row := range extensionCommandRows {
		accounted = append(accounted, row.constructor)
	}
	for name := range extensionCommandsWithoutAnAction {
		accounted = append(accounted, name)
	}
	assert.ElementsMatch(t, accounted, registered,
		"a registered extension command needs a row in extensionCommandRows or a reason in extensionCommandsWithoutAnAction, and every row and reason names a registered command")
}
