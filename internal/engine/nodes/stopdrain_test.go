package nodes

import (
	"encoding/json"
	"strings"
	"testing"

	kindscomplete "github.com/rancher/rancher-cli/internal/engine/kinds/complete"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// drainingNode loads the server with a worker whose drain is running.
func drainingNode(t *testing.T) *fakeRancher {
	t.Helper()
	f := newFakeRancher(t)
	obj := managementNode("m-1", "worker-1", "7", workerSpec("desiredNodeUnschedulable", "drain", "internalNodeSpec", map[string]any{"unschedulable": true}),
		drainedCond("Unknown", "", "t0"))
	f.labeled = []map[string]any{obj}
	f.steve = steveView(obj, "draining", "")
	f.reads = []map[string]any{obj}
	return f
}

// TestStopDrainPlanDisplayIdentity asserts the Effect of node stop-drain
// names the node by its downstream name (DisplayKind node, DisplayName the
// node name, Target the management node), is not destructive, and a committed
// result names the node.
func TestStopDrainPlanDisplayIdentity(t *testing.T) {
	t.Parallel()
	f := drainingNode(t)
	p, err := plan(t, newStopCmd(f), newInput("worker-1", "--wait=false"))
	require.NoError(t, err)
	assert.Equal(t, "node", p.eff.DisplayKind)
	assert.Equal(t, "worker-1", p.eff.DisplayName)
	assert.Equal(t, "nodes/m-1", p.eff.Target)
	assert.Equal(t, "c-1", p.eff.Cluster)
	assert.Equal(t, safety.PreviewServerDryRun, p.eff.PreviewSource)
	assert.False(t, p.eff.Destructive)
	assert.False(t, p.eff.Reversible)

	res, err := p.exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "node/worker-1 stop requested", strings.Split(res.Text, "\n")[0])
	assert.Equal(t, map[string]any{"node": "worker-1", "managementNode": "c-1/m-1", "waited": false}, res.Data)
}

// TestStopDrainBodyChangesOnlyTheRequest asserts stop-drain writes only
// spec.desiredNodeUnschedulable.
func TestStopDrainBodyChangesOnlyTheRequest(t *testing.T) {
	t.Parallel()
	f := drainingNode(t)
	p, err := plan(t, newStopCmd(f), newInput("worker-1"))
	require.NoError(t, err)
	assert.False(t, p.eff.Destructive)
	assert.Equal(t, []string{"the node stays cordoned; evicted pods are not put back; uncordon it by setting spec.unschedulable to false on the node"}, p.eff.Warnings)
	assert.Empty(t, p.eff.Objects)
	puts := f.writes()
	require.Len(t, puts, 1)
	spec := puts[0].body["spec"].(map[string]any)
	assert.Equal(t, "stopDrain", spec["desiredNodeUnschedulable"])
	assert.NotContains(t, spec, "nodeDrainInput")
	assert.Equal(t, []string{"/spec/desiredNodeUnschedulable"}, diffPaths(p.eff))
}

// TestPlanAndTheNodeSourceAllowTheSameNodes asserts the Plan of each command
// and the completion source agree on one fixture: a node is offered exactly
// where the Plan goes through, for both commands.
func TestPlanAndTheNodeSourceAllowTheSameNodes(t *testing.T) {
	t.Parallel()
	cordoned := map[string]any{"unschedulable": true}
	type fixture struct {
		node  string
		spec  map[string]any
		conds []map[string]any
		state string
	}
	fixtures := []fixture{
		{"active-node", workerSpec(), nil, "active"},
		{"draining-node", workerSpec("desiredNodeUnschedulable", "drain", "internalNodeSpec", cordoned), []map[string]any{drainedCond("Unknown", "", "t0")}, "draining"},
		{"drained-node", workerSpec("internalNodeSpec", cordoned), []map[string]any{drainedCond("True", "", "t0")}, "active"},
		{"failed-node", workerSpec("internalNodeSpec", cordoned), []map[string]any{drainedCond("False", "use --force", "t0")}, "draining"},
		{"unavailable-node", workerSpec(), nil, "unavailable"},
		{"cordoned-unavailable-node", workerSpec("internalNodeSpec", cordoned), nil, "unavailable"},
		{"control-node", map[string]any{"controlPlane": true}, nil, "active"},
		{"norole-node", map[string]any{"internalNodeSpec": map[string]any{}}, nil, "active"},
		{"stateless-node", workerSpec(), nil, ""},
		{"condition-left-node", workerSpec(), []map[string]any{drainedCond("True", "", "t0")}, "active"},
	}
	var items []any
	for _, fx := range fixtures {
		obj := managementNode("m-"+fx.node, fx.node, "7", fx.spec, fx.conds...)
		items = append(items, steveView(obj, fx.state, ""))
	}
	body, err := json.Marshal(map[string]any{"data": items})
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		cmd    func(*fakeRancher) runtime.Command
		source string
	}{
		{"drain", func(f *fakeRancher) runtime.Command { return newDrainCmd(f, false) }, "node drain"},
		{"stop-drain", func(f *fakeRancher) runtime.Command { return newStopCmd(f) }, "node stop-drain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var planned []string
			for _, fx := range fixtures {
				f := newFakeRancher(t)
				obj := managementNode("m-"+fx.node, fx.node, "7", fx.spec, fx.conds...)
				f.labeled = []map[string]any{obj}
				f.steve = steveView(obj, fx.state, "")
				if _, err := plan(t, tc.cmd(f), newInput(fx.node, "--wait=false")); err == nil {
					planned = append(planned, fx.node)
				}
			}
			ls := completiontest.NewFixed(t, string(body))
			got := kindscomplete.DrainNodeSource().Complete(t.Context(), ls.Conn(1), completion.Request{Command: tc.source, ClusterID: "c-1"})
			assert.ElementsMatch(t, planned, got.Names)
			assert.NotEmpty(t, planned)
		})
	}
}
