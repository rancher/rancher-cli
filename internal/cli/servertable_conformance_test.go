package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serverTableReason is why every kind in serverTableInventory has no
// Columns of its own.
const serverTableReason = "served by kube-apiserver's printers"

// serverTableInventory is the reviewed, closed set of kinds whose text
// tables come from the cluster's API server: the built-in Kubernetes kinds,
// whose printers define the columns kubectl shows. Checked both ways
// against the kind table by TestServerTableKindsConformance: a row marked
// ServerTable without an entry here, or an entry whose row is not marked,
// fails the build, so a kind moves between the server's printers and the
// CLI's own columns deliberately.
var serverTableInventory = map[string]string{
	"node":                  serverTableReason,
	"namespace":             serverTableReason,
	"pod":                   serverTableReason,
	"deployment":            serverTableReason,
	"service":               serverTableReason,
	"configmap":             serverTableReason,
	"secret":                serverTableReason,
	"statefulset":           serverTableReason,
	"daemonset":             serverTableReason,
	"replicaset":            serverTableReason,
	"job":                   serverTableReason,
	"cronjob":               serverTableReason,
	"ingress":               serverTableReason,
	"persistentvolume":      serverTableReason,
	"persistentvolumeclaim": serverTableReason,
	"serviceaccount":        serverTableReason,
	"event":                 serverTableReason,
}

// TestServerTableKindsConformance asserts the kinds marked ServerTable are
// exactly the reviewed inventory, in both directions, and that none carries
// Columns, a SystemFilter or RedactPaths, which the server's table never
// shows.
func TestServerTableKindsConformance(t *testing.T) {
	t.Parallel()
	marked := map[string]bool{}
	for _, k := range kinds.All() {
		if !k.ServerTable {
			continue
		}
		marked[k.Key] = true
		_, ok := serverTableInventory[k.Key]
		assert.True(t, ok, "kind %q is marked ServerTable but has no reviewed entry in serverTableInventory - add one deliberately", k.Key)
		assert.Empty(t, k.Columns, "ServerTable kind %q must not carry Columns", k.Key)
		assert.Nil(t, k.SystemFilter, "ServerTable kind %q must not carry a SystemFilter", k.Key)
		assert.Empty(t, k.RedactPaths, "ServerTable kind %q must not carry RedactPaths", k.Key)
	}
	for key := range serverTableInventory {
		_, ok := kinds.ByKey(key)
		require.True(t, ok, "serverTableInventory entry %q names no kind - remove it or check the spelling", key)
		assert.True(t, marked[key], "serverTableInventory entry %q is not marked ServerTable in the kind table", key)
	}
}

// TestServerTableRendererNeverReadsKindColumns asserts the file that
// renders the server's table names neither a kind's Columns nor the Column
// type: its headers and cells come from the server's answer alone.
func TestServerTableRendererNeverReadsKindColumns(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "../engine/resources/servertable.go", nil, 0)
	require.NoError(t, err)
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "kinds" && sel.Sel.Name == "Column" {
			t.Errorf("servertable.go uses kinds.Column; the server's table has its own columns")
		}
		if sel.Sel.Name == "Columns" {
			if inner, ok := sel.X.(*ast.SelectorExpr); (ok && inner.Sel.Name == "k") || isIdent(sel.X, "k") {
				t.Errorf("servertable.go reads a kind's Columns; the server's table has its own columns")
			}
		}
		return true
	})
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}
