package complete

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// managementNodes is a Steve API collection of management nodes: a worker that is
// active, a draining worker, a drained worker, a worker whose node is not
// ready, a control plane node with no worker role, and a worker with no
// state.
const managementNodes = `{"data":[
 {"metadata":{"name":"m-active","state":{"name":"active"}},"status":{"nodeName":"active-node"},"spec":{"worker":true}},
 {"metadata":{"name":"m-draining","state":{"name":"draining"}},"status":{"nodeName":"draining-node","conditions":[{"type":"Drained","status":"Unknown"}]},"spec":{"worker":true,"desiredNodeUnschedulable":"drain","internalNodeSpec":{"unschedulable":true}}},
 {"metadata":{"name":"m-drained","state":{"name":"active"}},"status":{"nodeName":"drained-node","conditions":[{"type":"Drained","status":"True"}]},"spec":{"worker":true,"internalNodeSpec":{"unschedulable":true}}},
 {"metadata":{"name":"m-unavailable","state":{"name":"unavailable"}},"status":{"nodeName":"unavailable-node"},"spec":{"worker":true}},
 {"metadata":{"name":"m-control","state":{"name":"active"}},"status":{"nodeName":"control-node"},"spec":{"controlPlane":true}},
 {"metadata":{"name":"m-nostate"},"status":{"nodeName":"nostate-node"},"spec":{"worker":true}}
]}`

// TestDrainNodeSourceOffersTheNodesTheCommandAllows asserts the source reads
// one Steve API collection of the cluster's management nodes and offers the
// names of the nodes each command is allowed on: a draining node only to
// stop-drain, a drained or unavailable node, a control plane node and a node
// with no state to neither.
func TestDrainNodeSourceOffersTheNodesTheCommandAllows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		command string
		want    []string
	}{
		{"node drain", []string{"active-node"}},
		{"node stop-drain", []string{"draining-node"}},
	} {
		t.Run(tc.command, func(t *testing.T) {
			t.Parallel()
			ls := completiontest.NewFixed(t, managementNodes)
			req := completion.Request{Command: tc.command, ClusterID: "c-x"}
			got := DrainNodeSource().Complete(t.Context(), ls.Conn(1), req)
			assert.Equal(t, tc.want, got.Names)
			assert.Equal(t, []string{"/v1/management.cattle.io.nodes/c-x"}, ls.Requested())
		})
	}
}

// TestDrainNodeSourceClaimsOnlyTheNodePositional asserts the source claims
// the first positional of node drain and node stop-drain with the one
// request of a cluster listing, and no flag value and no other command, so
// the shared --node flag keeps its own source.
func TestDrainNodeSourceClaimsOnlyTheNodePositional(t *testing.T) {
	t.Parallel()
	src := DrainNodeSource()
	for _, command := range []string{"node drain", "node stop-drain"} {
		reqs, ok := src.Claim(completion.Request{Command: command})
		require.True(t, ok, command)
		assert.Equal(t, completion.ClusterListing(), reqs)
		for _, flag := range []string{"node", "cluster", "timeout"} {
			_, ok := src.Claim(completion.Request{Command: command, Flag: flag})
			assert.False(t, ok, command+" --"+flag)
		}
		_, ok = src.Claim(completion.Request{Command: command, Index: 1})
		assert.False(t, ok, command+" second positional")
	}
	for _, command := range []string{"get pod", "logs", "inspect cluster", ""} {
		_, ok := src.Claim(completion.Request{Command: command})
		assert.False(t, ok, command)
		_, ok = src.Claim(completion.Request{Command: command, Flag: "node"})
		assert.False(t, ok, command+" --node")
	}
	_, ok := NodeSource().Claim(completion.Request{Command: "node drain", Flag: "node"})
	assert.True(t, ok, "the shared --node source is unchanged")
}

// TestDrainNodeSourceOffersNoCandidatesOnFailure asserts a refused read, an
// unusable cluster id and an exhausted allowance leave the source offering no
// candidates.
func TestDrainNodeSourceOffersNoCandidatesOnFailure(t *testing.T) {
	t.Parallel()
	refused := completiontest.NewStatus(t, 403)
	got := DrainNodeSource().Complete(t.Context(), refused.Conn(1), completion.Request{Command: "node drain", ClusterID: "c-x"})
	assert.Empty(t, got.Names)

	ls := completiontest.NewFixed(t, managementNodes)
	got = DrainNodeSource().Complete(t.Context(), ls.Conn(1), completion.Request{Command: "node drain", ClusterID: "a/b"})
	assert.Empty(t, got.Names)
	assert.Empty(t, ls.Requested())

	conn := ls.Conn(0)
	got = DrainNodeSource().Complete(t.Context(), conn, completion.Request{Command: "node drain", ClusterID: "c-x"})
	assert.Empty(t, got.Names)
	assert.True(t, conn.Exceeded())
}

// TestManagementNodeIDSourceOffersEveryIDWithItsNodeName asserts the id
// source reads the same collection and offers every management node id with
// no state filter, each described by the name of its node.
func TestManagementNodeIDSourceOffersEveryIDWithItsNodeName(t *testing.T) {
	t.Parallel()
	ls := completiontest.NewFixed(t, managementNodes)
	src := ManagementNodeIDSource()
	reqs, ok := src.Claim(completion.Request{})
	require.True(t, ok)
	assert.Equal(t, completion.ClusterListing(), reqs)
	_, ok = src.Claim(completion.Request{Flag: "cluster"})
	assert.False(t, ok)

	got := src.Complete(t.Context(), ls.Conn(1), completion.Request{ClusterID: "c-x"})
	assert.Equal(t, []string{"m-active", "m-draining", "m-drained", "m-unavailable", "m-control", "m-nostate"}, got.Names)
	assert.Equal(t, "drained-node", got.Descriptions["m-drained"])
	assert.Equal(t, []string{"/v1/management.cattle.io.nodes/c-x"}, ls.Requested())
}

// TestDrainNodeSourceDoesNotOfferANameTwoNodesShare asserts two management
// nodes with one node name leave the name out, since the command refuses it
// as ambiguous, and that a name is offered once.
func TestDrainNodeSourceDoesNotOfferANameTwoNodesShare(t *testing.T) {
	t.Parallel()
	body := `{"data":[
 {"metadata":{"name":"m-1","state":{"name":"active"}},"status":{"nodeName":"shared"},"spec":{"worker":true}},
 {"metadata":{"name":"m-2","state":{"name":"active"}},"status":{"nodeName":"shared"},"spec":{"worker":true}},
 {"metadata":{"name":"m-3","state":{"name":"active"}},"status":{"nodeName":"alone"},"spec":{"worker":true}},
 {"metadata":{"name":"m-4","state":{"name":"unavailable"}},"status":{"nodeName":"alone-too"},"spec":{"worker":true}},
 {"metadata":{"name":"m-5","state":{"name":"active"}},"status":{"nodeName":"alone-too"},"spec":{"worker":true,"internalNodeSpec":{"unschedulable":true}}}
]}`
	ls := completiontest.NewFixed(t, body)
	got := DrainNodeSource().Complete(t.Context(), ls.Conn(1), completion.Request{Command: "node drain", ClusterID: "c-x"})
	assert.Equal(t, []string{"alone"}, got.Names)
}
