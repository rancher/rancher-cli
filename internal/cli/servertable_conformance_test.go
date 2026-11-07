package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/testrancher/fidelity"
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

	"horizontalpodautoscaler": serverTableReason,
	"limitrange":              serverTableReason,
	"networkpolicy":           serverTableReason,
	"poddisruptionbudget":     serverTableReason,
	"resourcequota":           serverTableReason,
	"storageclass":            serverTableReason,
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

// serverTableProbeName is the recorded probe whose responses are the
// evidence for every ServerTable kind. The probe sends list requests
// through Rancher's cluster proxy with the Accept header that asks for a
// Table.
const serverTableProbeName = "server-table-downstream/nodes-and-pods"

// serverTableProbeRef names a recorded probe and the observation in it that
// is the evidence for one ServerTable kind.
type serverTableProbeRef struct {
	probe       string
	observation string
}

var (
	// serverTableNamespaced is the observation for a namespaced kind: a list of
	// pods across all namespaces.
	serverTableNamespaced = serverTableProbeRef{serverTableProbeName, "pods"}
	// serverTableClusterScoped is the observation for a cluster-scoped kind: a
	// list of nodes.
	serverTableClusterScoped = serverTableProbeRef{serverTableProbeName, "nodes"}
)

// serverTableProbes maps each ServerTable kind to the recorded observation
// that is its evidence. TestEveryServerTableKindHasAProbe asserts that every
// ServerTable kind has an entry and that every entry names a ServerTable
// kind.
var serverTableProbes = map[string]serverTableProbeRef{
	"node":                    serverTableClusterScoped,
	"namespace":               serverTableClusterScoped,
	"persistentvolume":        serverTableClusterScoped,
	"storageclass":            serverTableClusterScoped,
	"pod":                     serverTableNamespaced,
	"deployment":              serverTableNamespaced,
	"service":                 serverTableNamespaced,
	"configmap":               serverTableNamespaced,
	"secret":                  serverTableNamespaced,
	"statefulset":             serverTableNamespaced,
	"daemonset":               serverTableNamespaced,
	"replicaset":              serverTableNamespaced,
	"job":                     serverTableNamespaced,
	"cronjob":                 serverTableNamespaced,
	"ingress":                 serverTableNamespaced,
	"persistentvolumeclaim":   serverTableNamespaced,
	"serviceaccount":          serverTableNamespaced,
	"event":                   serverTableNamespaced,
	"horizontalpodautoscaler": serverTableNamespaced,
	"limitrange":              serverTableNamespaced,
	"networkpolicy":           serverTableNamespaced,
	"poddisruptionbudget":     serverTableNamespaced,
	"resourcequota":           serverTableNamespaced,
}

// TestEveryServerTableKindHasAProbe asserts that every kind marked
// ServerTable has an entry in serverTableProbes and that every entry names a
// ServerTable kind. It also asserts that each entry's probe is recorded on
// every Rancher minor against a downstream cluster, has the named
// observation, and sends a GET through the cluster proxy with the Accept
// header that asks for a Table. The observation must fit the kind's namespace
// policy: it is named pods and its request path ends in /pods for a
// namespaced kind, and it is named nodes and its request path ends in /nodes
// for a cluster-scoped kind.
//
// It does not check the columns of any kind. The recorded response shows that
// Rancher's proxy returns a Table when the request has the Accept header that
// asks for one; the columns of each kind come from kube-apiserver.
func TestEveryServerTableKindHasAProbe(t *testing.T) {
	t.Parallel()
	probes := map[string]fidelity.Probe{}
	for _, p := range fidelity.All() {
		probes[p.Name] = p
	}
	marked := map[string]kinds.Kind{}
	for _, k := range kinds.All() {
		if !k.ServerTable {
			continue
		}
		marked[k.Key] = k
		_, ok := serverTableProbes[k.Key]
		assert.True(t, ok, "kind %q is marked ServerTable but has no entry in serverTableProbes - add an entry with the recorded observation that is its evidence", k.Key)
	}
	for key, ref := range serverTableProbes {
		k, ok := marked[key]
		if !ok {
			assert.Fail(t, "serverTableProbes entry names no ServerTable kind", "entry %q: remove it or check the spelling", key)
			continue
		}
		p, ok := probes[ref.probe]
		if !assert.True(t, ok, "kind %q names probe %q, which fidelity.All() does not list", key, ref.probe) {
			continue
		}
		assert.True(t, p.Downstream, "probe %q of kind %q must run against a downstream cluster", p.Name, key)
		assert.False(t, p.OracleOnly, "probe %q of kind %q must not be marked OracleOnly", p.Name, key)
		assert.Empty(t, p.MaxMinor, "probe %q of kind %q must run on every Rancher minor", p.Name, key)

		i := slices.IndexFunc(p.Observations, func(o fidelity.Observation) bool { return o.Name == ref.observation })
		require.GreaterOrEqual(t, i, 0, "probe %q has no observation %q named by kind %q", p.Name, ref.observation, key)
		req := p.Observations[i].Request
		assert.Equal(t, http.MethodGet, req.Method, "observation %q of kind %q must be a GET", ref.observation, key)
		assert.True(t, strings.HasPrefix(req.Path, "/k8s/clusters/{downstream}/"), "observation %q of kind %q must go through the cluster proxy, got %q", ref.observation, key, req.Path)
		assert.Contains(t, req.Headers["Accept"], "as=Table", "observation %q of kind %q must ask for a Table", ref.observation, key)

		want := "pods"
		switch k.Policy {
		case kinds.NSPlain:
		case kinds.NSNone:
			want = "nodes"
		default:
			assert.Fail(t, "ServerTable kind has a namespace policy other than NSPlain and NSNone", "kind %q has policy %q", key, k.Policy)
		}
		assert.Equal(t, want, ref.observation, "kind %q (%s) must name the observation that fits its namespace policy", key, k.Policy)
		assert.True(t, strings.HasSuffix(req.Path, "/"+want), "observation %q of kind %q must request a path that ends in /%s, got %q", ref.observation, key, want, req.Path)
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
