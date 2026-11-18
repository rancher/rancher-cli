package kinds

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// steveNode builds a management node as the Steve API serves it from the fields the
// drain rules read; an empty state leaves metadata.state out, and a drained
// status of "" leaves the Drained condition out.
func steveNode(worker, etcd, controlPlane bool, state, stateMessage string, unschedulable bool, drained, drainedMessage, request string) map[string]any {
	meta := map[string]any{"name": "m-1"}
	if state != "" {
		meta["state"] = map[string]any{"name": state, "message": stateMessage}
	}
	spec := map[string]any{"worker": worker, "etcd": etcd, "controlPlane": controlPlane, "internalNodeSpec": map[string]any{"unschedulable": unschedulable}}
	if request != "" {
		spec["desiredNodeUnschedulable"] = request
	}
	status := map[string]any{"nodeName": "node-1"}
	if drained != "" {
		status["conditions"] = []any{
			map[string]any{"type": "Ready", "status": "True"},
			map[string]any{"type": "Drained", "status": drained, "message": drainedMessage},
		}
	}
	return map[string]any{"metadata": meta, "spec": spec, "status": status}
}

// TestNodeDrainStateFrom asserts the decoder reads every field of
// NodeDrainState from a Steve API object: each spec field, metadata.state absent
// and present, and the Drained condition absent and present among other
// conditions.
func TestNodeDrainStateFrom(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		obj  map[string]any
		want NodeDrainState
	}{
		{"empty object", map[string]any{}, NodeDrainState{}},
		{"worker", steveNode(true, false, false, "", "", false, "", "", ""), NodeDrainState{Worker: true}},
		{"etcd", steveNode(false, true, false, "", "", false, "", "", ""), NodeDrainState{Etcd: true}},
		{"control plane", steveNode(false, false, true, "", "", false, "", "", ""), NodeDrainState{ControlPlane: true}},
		{"state with a message", steveNode(false, false, false, "unavailable", "node is not ready", false, "", "", ""),
			NodeDrainState{State: "unavailable", StateMessage: "node is not ready"}},
		{"unschedulable", steveNode(false, false, false, "", "", true, "", "", ""), NodeDrainState{Unschedulable: true}},
		{"drained true", steveNode(false, false, false, "", "", false, "True", "", ""), NodeDrainState{Drained: "True"}},
		{"drained false with a message", steveNode(false, false, false, "", "", false, "False", "use --force", ""),
			NodeDrainState{Drained: "False", DrainedMessage: "use --force"}},
		{"request", steveNode(false, false, false, "", "", false, "", "", "stopDrain"), NodeDrainState{Request: "stopDrain"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, NodeDrainStateFrom(tc.obj))
		})
	}
}

// TestNodeDrainStateFromIgnoresMalformedFields asserts a wrong-typed field
// reads as its zero value and a condition that is not a map is skipped.
func TestNodeDrainStateFromIgnoresMalformedFields(t *testing.T) {
	t.Parallel()
	obj := map[string]any{
		"spec":   map[string]any{"worker": "yes", "desiredNodeUnschedulable": 3},
		"status": map[string]any{"conditions": []any{"Drained", map[string]any{"type": "Drained", "status": "False", "message": 4}}},
	}
	assert.Equal(t, NodeDrainState{Drained: "False"}, NodeDrainStateFrom(obj))
}

// normanInput is what Rancher's Norman node formatter and store read.
type normanInput struct {
	worker, etcd, controlPlane bool
	state                      string
	unschedulable              bool
	drained                    string
	drainedMessage             string
}

// normanActions writes the rules by which Rancher's Norman API offers the
// drain actions of a node a second time, separately from the package code:
// the formatter reads a node with none of etcd, controlPlane and worker as a
// worker, the node store turns the state of a cordoned node into drained or
// cordoned unless it is draining, and the formatter offers actions only on a
// worker node in one of four states, only stop drain while draining (a
// failed drain whose message names a kubectl flag or a timeout reads as
// cordoned), and drain unless drained.
func normanActions(in normanInput) map[string]bool {
	actions := map[string]bool{}
	if !in.etcd && !in.controlPlane && !in.worker {
		in.worker = true
	}
	if !in.worker {
		return actions
	}
	state := in.state
	if state != "draining" && in.unschedulable {
		state = "cordoned"
		if in.drained == "True" {
			state = "drained"
		}
	}
	if state == "draining" && in.drained == "False" {
		for _, text := range []string{"--ignore-daemonsets", "--delete-emptydir-data", "--force", "did not complete within"} {
			if strings.Contains(in.drainedMessage, text) {
				state = "cordoned"
			}
		}
	}
	switch state {
	case "active", "cordoned", "draining", "drained":
	default:
		return actions
	}
	if state == "draining" {
		actions["stopDrain"] = true
		return actions
	}
	if state != "drained" {
		actions["drain"] = true
	}
	return actions
}

// TestNodeDrainRuleMatchesNorman asserts, over roles, the Steve API's state, the
// unschedulable flag, the Drained condition and the pending request, that a
// drain action is allowed exactly where Norman offers it. The differences
// are a node whose Drained condition is still True while it is schedulable,
// which is refused so that Rancher can remove the condition first, and a node
// with no state, which is refused as unknown.
func TestNodeDrainRuleMatchesNorman(t *testing.T) {
	t.Parallel()
	roles := []struct {
		name                  string
		worker, etcd, control bool
	}{
		{"worker", true, false, false},
		{"worker and etcd", true, true, false},
		{"no role", false, false, false},
		{"etcd", false, true, false},
		{"control plane", false, false, true},
		{"worker and control plane", true, false, true},
		{"etcd and control plane", false, true, true},
	}
	states := []string{"active", "cordoned", "draining", "drained", "unavailable", "transitioning", ""}
	drains := []struct{ status, message string }{
		{"", ""}, {"Unknown", ""}, {"True", ""},
		{"False", "cannot delete Pods that declare no controller (use --force to override)"},
		{"False", "Drain failed: drain did not complete within 120s"},
		{"False", "some other failure"},
	}
	for _, role := range roles {
		for _, state := range states {
			for _, unschedulable := range []bool{false, true} {
				for _, d := range drains {
					for _, request := range []string{"", "drain", "stopDrain", "true", "false"} {
						name := fmt.Sprintf("%s/%q/unschedulable=%v/%s %q/request=%q", role.name, state, unschedulable, d.status, d.message, request)
						s := NodeDrainStateFrom(steveNode(role.worker, role.etcd, role.control, state, "", unschedulable, d.status, d.message, request))
						if state == "" {
							assert.Equal(t, "unknown", s.StateAfterRules(), name)
							assert.NotEqual(t, NodeDrainAllowed, NodeDrainRule(s, NodeDrain), name)
							assert.NotEqual(t, NodeDrainAllowed, NodeDrainRule(s, NodeStopDrain), name)
							continue
						}
						offered := normanActions(normanInput{role.worker, role.etcd, role.control, state, unschedulable, d.status, d.message})
						drain := NodeDrainRule(s, NodeDrain)
						stop := NodeDrainRule(s, NodeStopDrain)
						if drain == NodeDrainConditionLeft {
							assert.True(t, offered["drain"], name)
							assert.Equal(t, "True", d.status, name)
							assert.False(t, unschedulable, name)
						} else {
							assert.Equal(t, offered["drain"], drain == NodeDrainAllowed, name+" drain")
						}
						assert.Equal(t, offered["stopDrain"], stop == NodeDrainAllowed, name+" stop-drain")
					}
				}
			}
		}
	}
}

// TestNodeDrainRuleNamedCases asserts the case each named node gets: a
// pending drain with no condition, a pending cordon and uncordon, a Drained
// condition that is True on a schedulable node, a NotReady node, a cordoned
// NotReady node and a node with no role.
func TestNodeDrainRuleNamedCases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		obj         map[string]any
		drain, stop NodeDrainCase
	}{
		{"pending drain with no condition", steveNode(true, false, false, "active", "", false, "", "", "drain"), NodeDrainAllowed, NodeDrainNotStarted},
		{"pending cordon", steveNode(true, false, false, "active", "", false, "", "", "true"), NodeDrainAllowed, NodeDrainNotDraining},
		{"pending uncordon", steveNode(true, false, false, "active", "", false, "", "", "false"), NodeDrainAllowed, NodeDrainNotDraining},
		{"condition left on a schedulable node", steveNode(true, false, false, "active", "", false, "True", "", ""), NodeDrainConditionLeft, NodeDrainNotDraining},
		{"unavailable", steveNode(true, false, false, "unavailable", "node is not ready", false, "", "", ""), NodeDrainNoAction, NodeDrainNoAction},
		{"cordoned and not ready", steveNode(true, false, false, "unavailable", "node is not ready", true, "", "", ""), NodeDrainAllowed, NodeDrainNotDraining},
		{"drained", steveNode(true, false, false, "active", "", true, "True", "", ""), NodeDrainAlreadyDrained, NodeDrainNotDrainingDrained},
		{"draining", steveNode(true, false, false, "draining", "", true, "Unknown", "", "drain"), NodeDrainAlreadyDraining, NodeDrainAllowed},
		{"failed drain that names a flag", steveNode(true, false, false, "draining", "", true, "False", "use --force", ""), NodeDrainAllowed, NodeDrainNotDraining},
		{"failed drain that names nothing", steveNode(true, false, false, "draining", "", true, "False", "boom", "drain"), NodeDrainAlreadyDraining, NodeDrainAllowed},
		{"no role is a worker", steveNode(false, false, false, "active", "", false, "", "", ""), NodeDrainAllowed, NodeDrainNotDraining},
		{"etcd and no worker role", steveNode(false, true, false, "active", "", false, "", "", ""), NodeDrainNotWorker, NodeDrainNotWorker},
		{"no state", steveNode(true, false, false, "", "", false, "", "", ""), NodeDrainNoAction, NodeDrainNoAction},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := NodeDrainStateFrom(tc.obj)
			assert.Equal(t, tc.drain, NodeDrainRule(s, NodeDrain), "drain")
			assert.Equal(t, tc.stop, NodeDrainRule(s, NodeStopDrain), "stop-drain")
		})
	}
}

// TestManagementNodeRow asserts every field of the management-node row, its
// lookup by plural name and by group, version and resource, and that its
// columns show the node name, the Drained condition, the request and the
// worker flag.
func TestManagementNodeRow(t *testing.T) {
	t.Parallel()
	k, ok := ByKey("managementnode")
	require.True(t, ok)
	assert.Equal(t, "management-node", k.Name)
	assert.Equal(t, []string{"managementnodes", "management-nodes"}, k.Aliases)
	assert.Equal(t, "management.cattle.io", k.GVR.Group)
	assert.Equal(t, "v3", k.GVR.Version)
	assert.Equal(t, "nodes", k.GVR.Resource)
	assert.Equal(t, NSCluster, k.Policy)
	assert.Equal(t, "Node", k.K8sKind)
	assert.Equal(t, Management, k.Scope)
	assert.False(t, k.Deletable)
	assert.Equal(t, "Rancher deletes a management node when its node leaves the cluster", k.NoDeleteReason)
	assert.Contains(t, k.NoPatchReason, "node drain <node> --cluster <cluster> --dry-run")
	assert.Contains(t, k.NoPatchReason, "node stop-drain <node> --cluster <cluster> --dry-run")
	assert.Equal(t, "Rancher creates a management node for each node of a cluster", k.NoCreateViaManifest)
	assert.Equal(t, "condition=Drained", k.WaitDefault)
	assert.True(t, k.Completable)
	assert.Equal(t, [][]string{{"spec", "customConfig", "sshKey"}}, k.RedactPaths)
	require.Len(t, k.Columns, 4)
	assert.Equal(t, Column{Header: "NODE", Path: []string{"status", "nodeName"}}, k.Columns[0])
	assert.Equal(t, Column{Header: "DRAINED", Condition: "Drained"}, k.Columns[1])
	assert.Equal(t, Column{Header: "REQUEST", Path: []string{"spec", "desiredNodeUnschedulable"}}, k.Columns[2])
	assert.Equal(t, Column{Header: "WORKER", Path: []string{"spec", "worker"}, Bool: true}, k.Columns[3])

	for _, name := range []string{"management-node", "management-nodes", "managementnodes"} {
		got, ok := Lookup(name)
		require.True(t, ok, name)
		assert.Equal(t, "managementnode", got.Key, name)
	}
	byGVR, ok := ByGVR(k.GVR)
	require.True(t, ok)
	assert.Equal(t, "managementnode", byGVR.Key)
}
