package nodes

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// commandUnderTest is one of the two node commands with the fixture node that
// the command accepts, a node Rancher offers the action on.
type commandUnderTest struct {
	name   string
	stop   bool
	newCmd func(*fakeRancher) runtime.Command
	node   func(*testing.T) *fakeRancher
	action string
}

var commandsUnderTest = []commandUnderTest{
	{"node drain", false, func(f *fakeRancher) runtime.Command { return newDrainCmd(f, false) }, idleNode, "drain"},
	{"node stop-drain", true, newStopCmd, drainingNode, "stopDrain"},
}

// ownerOf loads the server as a caller sees it whose access to the local
// cluster is refused: every list of the management nodes on the local route is
// a 403.
func ownerOf(t *testing.T, c commandUnderTest) *fakeRancher {
	t.Helper()
	f := c.node(t)
	f.listStatus = 403
	return f
}

// requireNoNorman asserts the fake received no Norman request.
func requireNoNorman(t *testing.T, f *fakeRancher) {
	t.Helper()
	assert.Empty(t, f.normanSeen(), "no Norman request")
	assert.Empty(t, f.postsSeen(), "no Norman action")
}

// checkPathFollowsTheFirstList asserts, for one command, what each response to
// the first list sends where: the Kubernetes API after a 200, Rancher's node
// action after a 403, and nothing to Norman after a refused write.
func checkPathFollowsTheFirstList(t *testing.T, c commandUnderTest) {
	t.Helper()
	t.Run("first list 200", func(t *testing.T) {
		t.Parallel()
		f := c.node(t)
		p, err := plan(t, c.newCmd(f), newInput("worker-1", "--wait=false"))
		require.NoError(t, err)
		assert.Equal(t, safety.WriteViaKubernetesAPI, p.eff.WriteVia)
		assert.Equal(t, safety.PreviewServerDryRun, p.eff.PreviewSource)
		assert.Equal(t, 1, f.kubernetesWrites(), "the dry-run")
		_, err = p.exec(t.Context())
		require.NoError(t, err)
		puts := f.writes()
		require.Len(t, puts, 2)
		assert.True(t, puts[0].dryRun)
		assert.False(t, puts[1].dryRun)
		requireNoNorman(t, f)
	})
	t.Run("first list 403", func(t *testing.T) {
		t.Parallel()
		f := ownerOf(t, c)
		p, err := plan(t, c.newCmd(f), newInput("worker-1", "--wait=false"))
		require.NoError(t, err)
		assert.Equal(t, safety.WriteViaRancherAction, p.eff.WriteVia)
		assert.Equal(t, safety.PreviewServerReads, p.eff.PreviewSource)
		assert.Len(t, f.normanSeen(), 1, "one GET for a dry-run")
		assert.Empty(t, f.postsSeen())
		_, err = p.exec(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 0, f.kubernetesWrites(), "no Kubernetes write")
		seen := f.normanSeen()
		require.Len(t, seen, 3, "two GETs and one POST for --apply")
		assert.True(t, strings.HasPrefix(seen[0], "GET /v3/nodes/c-1:m-1"), seen[0])
		assert.True(t, strings.HasPrefix(seen[1], "GET /v3/nodes/c-1:m-1"), seen[1])
		assert.Equal(t, "POST /v3/nodes/c-1:m-1?action="+c.action, seen[2])
	})
	t.Run("first list 200 then the dry-run is refused", func(t *testing.T) {
		t.Parallel()
		f := c.node(t)
		f.putStatus, f.putMessage = 403, `nodes.management.cattle.io "m-1" is forbidden: User "u" cannot update resource "nodes"`
		_, err := plan(t, c.newCmd(f), newInput("worker-1"))
		ce := requireCLIError(t, err)
		assert.Equal(t, clierr.CodeRBAC, ce.Code)
		assert.True(t, strings.HasSuffix(ce.Message, "; this command needs access to the local cluster, with update on nodes.management.cattle.io in namespace c-1 there"), ce.Message)
		assert.Equal(t, 1, f.kubernetesWrites(), "the refused dry-run only")
		requireNoNorman(t, f)
	})
	t.Run("first list 200 then the write is refused", func(t *testing.T) {
		t.Parallel()
		f := c.node(t)
		f.applyPutStatus, f.putMessage = 403, `nodes.management.cattle.io "m-1" is forbidden: User "u" cannot update resource "nodes"`
		p, err := plan(t, c.newCmd(f), newInput("worker-1"))
		require.NoError(t, err)
		_, err = p.exec(t.Context())
		ce := requireCLIError(t, err)
		assert.Equal(t, clierr.CodeRBAC, ce.Code)
		assert.Contains(t, ce.Message, `User "u" cannot update resource "nodes"`, "the server's text")
		assert.NotContains(t, ce.Message, "this command needs access to the local cluster", "the write error carries no local-cluster sentence")
		assert.Equal(t, 2, f.kubernetesWrites(), "the dry-run and the refused write")
		assert.Nil(t, ce.Details, "nothing was written, so no committed identity")
		requireNoNorman(t, f)
	})
	t.Run("first list 500", func(t *testing.T) {
		t.Parallel()
		f := c.node(t)
		f.listStatus = 500
		_, err := plan(t, c.newCmd(f), newInput("worker-1"))
		require.Error(t, err)
		status, ok := clierr.HTTPStatus(err)
		require.True(t, ok)
		assert.Equal(t, 500, status)
		assert.NotEqual(t, clierr.CodeRBAC, clierr.From(err).Code)
		requireNoNorman(t, f)
	})
}

// TestNodeDrainPathFollowsTheFirstList asserts node drain goes through the
// Kubernetes API after a 200 on the first list and through Rancher's node
// action after a 403, and never changes path after that.
func TestNodeDrainPathFollowsTheFirstList(t *testing.T) {
	t.Parallel()
	checkPathFollowsTheFirstList(t, commandsUnderTest[0])
}

// TestNodeStopDrainPathFollowsTheFirstList asserts the same for node
// stop-drain.
func TestNodeStopDrainPathFollowsTheFirstList(t *testing.T) {
	t.Parallel()
	checkPathFollowsTheFirstList(t, commandsUnderTest[1])
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
// drain actions of a node, separately from the package code: the formatter
// reads a node with none of etcd, controlPlane and worker as a worker, the
// node store turns the state of a cordoned node into drained or cordoned
// unless it is draining, and the formatter offers actions only on a worker
// node in one of four states, only stop drain while draining (a failed drain
// whose message names a kubectl flag or a timeout reads as cordoned), and
// drain unless drained.
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

// TestNormanPathRulesAgainstTheActionsMap asserts, over the roles, the state,
// the cordon flag, the Drained condition and the request, that the Norman path
// allows an action exactly where Rancher's actions map offers it, and refuses
// with the text of the rules of the Kubernetes path when they name the case.
// The two inputs where the rules and the map differ have their own results: a
// Drained condition that is True on a schedulable node is allowed because the
// map offers drain, and a node with no state is refused as unknown.
func TestNormanPathRulesAgainstTheActionsMap(t *testing.T) {
	t.Parallel()
	roles := []struct {
		name               string
		worker, etcd, ctrl bool
	}{
		{"worker", true, false, false}, {"no role", false, false, false}, {"etcd", false, true, false},
		{"control plane", false, false, true}, {"worker and etcd", true, true, false}, {"etcd and control plane", false, true, true},
	}
	states := []string{"active", "cordoned", "draining", "drained", "unavailable", "transitioning", ""}
	drains := []struct{ status, message string }{
		{"", ""}, {"Unknown", ""}, {"True", ""}, {"False", "use --force to override"}, {"False", "drain did not complete within 120s"}, {"False", "other failure"},
	}
	tt := target{node: "worker-1", cluster: "prod", id: "c-1", mid: "m-1"}
	for _, role := range roles {
		for _, state := range states {
			for _, unschedulable := range []bool{false, true} {
				for _, d := range drains {
					for _, request := range []string{"", "drain", "stopDrain"} {
						name := fmt.Sprintf("%s/%q/unschedulable=%v/%s %q/request=%q", role.name, state, unschedulable, d.status, d.message, request)
						obj := steveNodeFor(role.worker, role.etcd, role.ctrl, state, unschedulable, d.status, d.message, request)
						offered := normanActions(normanInput{role.worker, role.etcd, role.ctrl, state, unschedulable, d.status, d.message})
						var names []string
						for _, a := range []string{"drain", "stopDrain"} {
							if offered[a] {
								names = append(names, a)
							}
						}
						node := engine.NormanNode{State: state, Actions: names, HasActions: len(names) > 0}
						for _, action := range []kinds.NodeDrainAction{kinds.NodeDrain, kinds.NodeStopDrain} {
							noActions, ce := normanRule(node, obj, action, tt)
							s := kinds.NodeDrainStateFrom(obj)
							s1 := refusal(kinds.NodeDrainRule(s, action), s, action, tt)
							if !node.HasActions {
								assert.True(t, noActions, name)
								assert.Equal(t, s1 == nil, ce == nil, name+" falls back to the rules of the Kubernetes path")
								continue
							}
							assert.False(t, noActions, name)
							if node.Offers(nodeAction(action)) {
								assert.Nil(t, ce, name+" "+offerPhrase(action, "worker-1"))
								continue
							}
							require.NotNil(t, ce, name+" "+offerPhrase(action, "worker-1"))
							assert.Equal(t, clierr.CodeValidation == ce.Code || ce.Code == clierr.CodeConflict, true, name)
							if s1 != nil {
								assert.Equal(t, s1.Message, ce.Message, name)
							} else {
								assert.Equal(t, noOfferText(action, state), ce.Message, name)
							}
						}
					}
				}
			}
		}
	}
}

// noOfferText is the refusal of an action that Rancher's map lacks and the rules
// allow, written out here and not built with the package's own function.
func noOfferText(action kinds.NodeDrainAction, state string) string {
	what := "drain node worker-1"
	if action == kinds.NodeStopDrain {
		what = "stop the drain of node worker-1"
	}
	if state == "" {
		return "Rancher does not offer to " + what + ": Rancher gave no state for the node"
	}
	return "Rancher does not offer to " + what + " while it is " + state
}

// steveNodeFor is a management node as the Steve API serves it, built from the
// fields the rules read.
func steveNodeFor(worker, etcd, ctrl bool, state string, unschedulable bool, drained, message, request string) map[string]any {
	spec := map[string]any{"worker": worker, "etcd": etcd, "controlPlane": ctrl, "internalNodeSpec": map[string]any{"unschedulable": unschedulable}}
	if request != "" {
		spec["desiredNodeUnschedulable"] = request
	}
	var conds []map[string]any
	if drained != "" {
		conds = append(conds, drainedCond(drained, message, "t0"))
	}
	return steveView(managementNode("m-1", "worker-1", "7", spec, conds...), state, "")
}

// TestNormanPathNamedRules asserts the two inputs where the rules and the map
// differ, a map with no key and an empty map, which fall back to the rules with
// a warning, and a map that lacks an action the rules allow, which gets the
// generic text with the state of Rancher's node.
func TestNormanPathNamedRules(t *testing.T) {
	t.Parallel()
	t.Run("drained condition left on a schedulable node is allowed when the map offers drain", func(t *testing.T) {
		t.Parallel()
		f := ownerOf(t, commandsUnderTest[0])
		obj := managementNode("m-1", "worker-1", "7", workerSpec(), drainedCond("True", "", "t0"))
		f.steveItems = []map[string]any{steveView(obj, "active", "")}
		f.norman = []map[string]any{normanNode("c-1:m-1", "active", "drain")}
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		require.NoError(t, err)
	})
	t.Run("no state is refused as unknown", func(t *testing.T) {
		t.Parallel()
		f := ownerOf(t, commandsUnderTest[0])
		obj := managementNode("m-1", "worker-1", "7", workerSpec())
		f.steveItems = []map[string]any{steveView(obj, "", "")}
		delete(f.steveItems[0]["metadata"].(map[string]any), "state")
		f.norman = []map[string]any{normanNode("c-1:m-1", "", "stopDrain")}
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		ce := requireCLIError(t, err)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Equal(t, "Rancher offers no action on node worker-1 while it is unknown", ce.Message)
	})
	for _, tc := range []struct {
		name   string
		answer map[string]any
	}{
		{"no actions key", normanNode("c-1:m-1", "active")},
		{"empty actions map", map[string]any{"id": "c-1:m-1", "state": "active", "actions": map[string]any{}}},
	} {
		t.Run(tc.name+" falls back to the rules with a warning", func(t *testing.T) {
			t.Parallel()
			cordoned := map[string]any{"unschedulable": true}
			f := ownerOf(t, commandsUnderTest[0])
			f.norman = []map[string]any{tc.answer}
			p, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
			require.NoError(t, err)
			assert.Contains(t, p.eff.Warnings, "Rancher's API listed no actions for node worker-1; the command checked the node's state against the rules the Rancher UI uses")
			g := ownerOf(t, commandsUnderTest[0])
			obj := managementNode("m-1", "worker-1", "7", workerSpec("internalNodeSpec", cordoned), drainedCond("True", "", "t0"))
			g.steveItems = []map[string]any{steveView(obj, "active", "")}
			g.norman = []map[string]any{tc.answer}
			_, err = plan(t, newDrainCmd(g, false), newInput("worker-1"))
			ce := requireCLIError(t, err)
			assert.Equal(t, "node worker-1 is already drained", ce.Message)
			h := ownerOf(t, commandsUnderTest[0])
			obj = managementNode("m-1", "worker-1", "7", map[string]any{"etcd": true})
			h.steveItems = []map[string]any{steveView(obj, "active", "")}
			h.norman = []map[string]any{tc.answer}
			_, err = plan(t, newDrainCmd(h, false), newInput("worker-1"))
			ce = requireCLIError(t, err)
			assert.Equal(t, "Rancher offers drain only for worker nodes; node worker-1 has the roles etcd", ce.Message)
		})
	}
	t.Run("a map that lacks an action the rules allow gets the generic text", func(t *testing.T) {
		t.Parallel()
		f := ownerOf(t, commandsUnderTest[0])
		f.norman = []map[string]any{normanNode("c-1:m-1", "transitioning", "stopDrain")}
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		ce := requireCLIError(t, err)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Equal(t, "Rancher does not offer to drain node worker-1 while it is transitioning", ce.Message)
		g := ownerOf(t, commandsUnderTest[1])
		g.norman = []map[string]any{normanNode("c-1:m-1", "draining", "drain")}
		_, err = plan(t, newStopCmd(g), newInput("worker-1"))
		ce = requireCLIError(t, err)
		assert.Equal(t, "Rancher does not offer to stop the drain of node worker-1 while it is draining", ce.Message)
		h := ownerOf(t, commandsUnderTest[0])
		h.norman = []map[string]any{normanNode("c-1:m-1", "", "stopDrain")}
		_, err = plan(t, newDrainCmd(h, false), newInput("worker-1"))
		ce = requireCLIError(t, err)
		assert.Equal(t, "Rancher does not offer to drain node worker-1: Rancher gave no state for the node", ce.Message)
	})
}

// TestNormanPathStateComesFromTheSteveMetadataState asserts the refusal of a
// node whose map lacks the action is the one the rules give for the state in
// the Steve item's metadata.state, for items that differ only in that state,
// and that the client of the wait returns the Steve body with metadata.state,
// id and links kept.
func TestNormanPathStateComesFromTheSteveMetadataState(t *testing.T) {
	t.Parallel()
	cordoned := map[string]any{"unschedulable": true}
	for _, tc := range []struct {
		name, state, message, want string
		spec                       map[string]any
		conds                      []map[string]any
	}{
		{"unavailable", "unavailable", "node is not ready", "Rancher offers no action on node worker-1 while it is unavailable: node is not ready; to drain it anyway, cordon it first by setting spec.unschedulable to true on the node: Rancher then shows it cordoned and offers drain", workerSpec(), nil},
		{"draining", "draining", "", "node worker-1 is already draining", workerSpec("desiredNodeUnschedulable", "drain", "internalNodeSpec", cordoned), []map[string]any{drainedCond("Unknown", "", "t0")}},
		{"no state", "", "", "Rancher offers no action on node worker-1 while it is unknown", workerSpec(), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := ownerOf(t, commandsUnderTest[0])
			obj := managementNode("m-1", "worker-1", "7", tc.spec, tc.conds...)
			item := steveView(obj, tc.state, tc.message)
			if tc.state == "" {
				delete(item["metadata"].(map[string]any), "state")
			}
			f.steveItems = []map[string]any{item}
			f.norman = []map[string]any{normanNode("c-1:m-1", "active", "stopDrain")}
			_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
			ce := requireCLIError(t, err)
			assert.Equal(t, tc.want, ce.Message)
		})
	}
	f := idleNode(t)
	f.steve = steveView(managementNode("m-1", "worker-1", "7", workerSpec()), "active", "")
	row, ok := kinds.ByKey("managementnode")
	require.True(t, ok)
	client := steveWaitClient{get: steve.RootFactoryGetter(testRoots{f: f})}
	got, err := client.Get(t.Context(), row.GVR, "c-1", "m-1")
	require.NoError(t, err)
	state, found, _ := unstructured.NestedString(got.Object, "metadata", "state", "name")
	assert.True(t, found)
	assert.Equal(t, "active", state)
	assert.Equal(t, "c-1/m-1", got.Object["id"])
	assert.Contains(t, got.Object, "links")
}

// TestNormanPathSteveListErrors asserts the errors of the list of the
// management nodes through the Steve API on the Norman path.
func TestNormanPathSteveListErrors(t *testing.T) {
	t.Parallel()
	t.Run("403 and 404 are RBAC with the path", func(t *testing.T) {
		t.Parallel()
		for _, status := range []int{403, 404} {
			f := ownerOf(t, commandsUnderTest[0])
			f.steveListStatus = status
			_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
			ce := requireCLIError(t, err)
			assert.Equal(t, clierr.CodeRBAC, ce.Code)
			assert.Equal(t, fmt.Sprintf("you cannot read the management nodes of cluster c-1 through Rancher's Steve API (GET /v1/management.cattle.io.nodes/c-1 was refused with HTTP %d)", status), ce.Message)
			got, ok := clierr.HTTPStatus(err)
			assert.True(t, ok)
			assert.Equal(t, status, got, "the cause is kept")
			requireNoNorman(t, f)
		}
	})
	t.Run("another status keeps its error", func(t *testing.T) {
		t.Parallel()
		f := ownerOf(t, commandsUnderTest[0])
		f.steveListStatus = 503
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		ce := requireCLIError(t, err)
		assert.NotEqual(t, clierr.CodeRBAC, ce.Code)
		requireNoNorman(t, f)
	})
	t.Run("no management node is NOT_FOUND", func(t *testing.T) {
		t.Parallel()
		f := ownerOf(t, commandsUnderTest[0])
		f.steveItems = nil
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		ce := requireCLIError(t, err)
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
		assert.Equal(t, "cluster c-1 has no node worker-1 that Rancher manages", ce.Message)
		requireNoNorman(t, f)
	})
	t.Run("two management nodes are ambiguous", func(t *testing.T) {
		t.Parallel()
		f := ownerOf(t, commandsUnderTest[0])
		other := steveView(managementNode("m-2", "worker-1", "3", workerSpec()), "active", "")
		f.steveItems = []map[string]any{other, f.steveItems[0]}
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		ce := requireCLIError(t, err)
		assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
		assert.Equal(t, "cluster c-1 has 2 management nodes for node worker-1", ce.Message)
		assert.Equal(t, []clierr.Candidate{{ID: "m-1", Name: "worker-1"}, {ID: "m-2", Name: "worker-1"}}, ce.Candidates)
	})
	t.Run("the label is preferred and the node name is the fallback", func(t *testing.T) {
		t.Parallel()
		f := ownerOf(t, commandsUnderTest[0])
		other := managementNode("m-2", "worker-1", "3", workerSpec())
		other["metadata"].(map[string]any)["labels"] = map[string]any{}
		f.steveItems = []map[string]any{steveView(other, "active", ""), f.steveItems[0]}
		p, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		require.NoError(t, err, "only the labeled item counts when one has the label")
		assert.Equal(t, "nodes/m-1", p.eff.Target)
		g := ownerOf(t, commandsUnderTest[0])
		unlabeled := managementNode("m-3", "worker-1", "3", workerSpec())
		unlabeled["metadata"].(map[string]any)["labels"] = map[string]any{}
		g.steveItems = []map[string]any{steveView(unlabeled, "active", ""), steveView(managementNode("m-4", "other", "3", workerSpec()), "active", "")}
		g.norman = []map[string]any{normanNode("c-1:m-3", "active", "drain")}
		p, err = plan(t, newDrainCmd(g, false), newInput("worker-1"))
		require.NoError(t, err, "no labeled item: every item with the node name counts")
		assert.Equal(t, "nodes/m-3", p.eff.Target)
	})
	t.Run("an item that is not an object or has no id", func(t *testing.T) {
		t.Parallel()
		for name, body := range map[string]string{
			"not an object": `{"data":["x"],"count":1,"revision":"1"}`,
			"no id":         `{"data":[{"metadata":{"name":"m-1"}}],"count":1,"revision":"1"}`,
		} {
			f := ownerOf(t, commandsUnderTest[0])
			f.steveListBody = body
			_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
			ce := requireCLIError(t, err)
			assert.Equal(t, clierr.CodeGeneric, ce.Code, name)
			assert.False(t, ce.Retryable, name)
			assert.True(t, strings.HasPrefix(ce.Message, "could not read the management nodes of cluster c-1 from GET /v1/management.cattle.io.nodes/c-1: "), ce.Message)
		}
	})
	t.Run("a page with no revision passes through", func(t *testing.T) {
		t.Parallel()
		f := ownerOf(t, commandsUnderTest[0])
		f.steveListBody = `{"data":[],"count":0}`
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		require.ErrorContains(t, err, "a page has no revision")
	})
}

// TestNormanPathNodeReadErrors asserts the Norman read of the node: a refused
// read keeps the status table's code, and a response that is not a node fails
// before any write.
func TestNormanPathNodeReadErrors(t *testing.T) {
	t.Parallel()
	for status, code := range map[int]string{403: clierr.CodeRBAC, 404: clierr.CodeNotFound, 422: clierr.CodeValidation} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			f := ownerOf(t, commandsUnderTest[0])
			f.normanStatus = status
			_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
			ce := requireCLIError(t, err)
			assert.Equal(t, code, ce.Code)
			assert.Empty(t, f.postsSeen())
			assert.Equal(t, 0, f.kubernetesWrites())
		})
	}
}

// TestNormanPathAccessReview asserts the review before the Norman read: an
// allowed review passes, a refused one is RBAC with nothing sent to Norman,
// an evaluation error is named, a refused request returns its mapped error,
// and the review asks for update on nodes of management.cattle.io in the
// cluster's namespace.
func TestNormanPathAccessReview(t *testing.T) {
	t.Parallel()
	t.Run("allowed asks the question", func(t *testing.T) {
		t.Parallel()
		f := ownerOf(t, commandsUnderTest[0])
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		require.NoError(t, err)
		require.Len(t, f.reviews, 1)
		assert.Equal(t, "authorization.k8s.io/v1", f.reviews[0]["apiVersion"])
		assert.Equal(t, "SelfSubjectAccessReview", f.reviews[0]["kind"])
		spec := f.reviews[0]["spec"].(map[string]any)
		assert.Equal(t, map[string]any{"group": "management.cattle.io", "resource": "nodes", "verb": "update", "namespace": "c-1"}, spec["resourceAttributes"])
	})
	t.Run("not allowed", func(t *testing.T) {
		t.Parallel()
		f := ownerOf(t, commandsUnderTest[0])
		f.reviewDenied = true
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		ce := requireCLIError(t, err)
		assert.Equal(t, clierr.CodeRBAC, ce.Code)
		assert.Equal(t, "you cannot change management node m-1 of cluster c-1: the local cluster does not allow you to update nodes.management.cattle.io in namespace c-1; the cluster roles Cluster Owner and Manage Nodes grant that", ce.Message)
		requireNoNorman(t, f)
	})
	t.Run("not allowed with an evaluation error", func(t *testing.T) {
		t.Parallel()
		f := ownerOf(t, commandsUnderTest[0])
		f.reviewDenied, f.reviewEvalError = true, "the rules could not be read"
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		ce := requireCLIError(t, err)
		assert.Equal(t, clierr.CodeRBAC, ce.Code)
		assert.True(t, strings.HasSuffix(ce.Message, "; the review reported: the rules could not be read"), ce.Message)
		requireNoNorman(t, f)
	})
	t.Run("a refused review returns the mapped error", func(t *testing.T) {
		t.Parallel()
		for status, code := range map[int]string{403: clierr.CodeRBAC, 404: clierr.CodeNotFound} {
			f := ownerOf(t, commandsUnderTest[0])
			f.reviewStatus = status
			_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
			ce := requireCLIError(t, err)
			assert.Equal(t, code, ce.Code, status)
			requireNoNorman(t, f)
		}
	})
}

// TestNormanDrainBodyCarriesTheFiveFields asserts the drain request body has
// all five options with Rancher's defaults when no flag is typed and with each
// typed value, as JSON, and that stop-drain sends no body and no content type.
func TestNormanDrainBodyCarriesTheFiveFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		flags []string
		want  map[string]any
	}{
		{"no flag", nil, map[string]any{"force": false, "ignoreDaemonSets": true, "deleteLocalData": false, "gracePeriod": float64(-1), "timeout": float64(120)}},
		{"every flag", []string{"--force", "--delete-emptydir-data", "--grace-period=30", "--drain-timeout=5m"},
			map[string]any{"force": true, "ignoreDaemonSets": true, "deleteLocalData": true, "gracePeriod": float64(30), "timeout": float64(300)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := ownerOf(t, commandsUnderTest[0])
			p, err := plan(t, newDrainCmd(f, false), newInput("worker-1", append([]string{"--wait=false"}, tc.flags...)...))
			require.NoError(t, err)
			_, err = p.exec(t.Context())
			require.NoError(t, err)
			posts := f.postsSeen()
			require.Len(t, posts, 1)
			assert.Equal(t, "action=drain", posts[0].query)
			assert.Equal(t, "application/json", posts[0].contentType)
			var body map[string]any
			require.NoError(t, json.Unmarshal(posts[0].body, &body))
			assert.Equal(t, tc.want, body)
		})
	}
	t.Run("stop-drain sends nothing", func(t *testing.T) {
		t.Parallel()
		f := ownerOf(t, commandsUnderTest[1])
		p, err := plan(t, newStopCmd(f), newInput("worker-1", "--wait=false"))
		require.NoError(t, err)
		_, err = p.exec(t.Context())
		require.NoError(t, err)
		posts := f.postsSeen()
		require.Len(t, posts, 1)
		assert.Equal(t, "action=stopDrain", posts[0].query)
		assert.Empty(t, posts[0].body)
		assert.Empty(t, posts[0].contentType)
	})
}

// TestNormanExecuteRefusesWhenTheActionIsGone asserts Execute reads the node
// again and refuses, with nothing posted, when the second read lacks the
// action or has no actions map at all.
func TestNormanExecuteRefusesWhenTheActionIsGone(t *testing.T) {
	t.Parallel()
	for _, c := range commandsUnderTest {
		for name, second := range map[string]map[string]any{
			"another action":    normanNode("c-1:m-1", "active", map[bool]string{false: "stopDrain", true: "drain"}[c.stop]),
			"no actions at all": normanNode("c-1:m-1", "active"),
		} {
			t.Run(c.name+" "+name, func(t *testing.T) {
				t.Parallel()
				f := ownerOf(t, c)
				f.norman = []map[string]any{f.norman[0], second}
				p, err := plan(t, c.newCmd(f), newInput("worker-1", "--wait=false"))
				require.NoError(t, err)
				_, err = p.exec(t.Context())
				ce := requireCLIError(t, err)
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				assert.True(t, strings.HasPrefix(ce.Message, "Rancher does not offer to "), ce.Message)
				assert.Empty(t, f.postsSeen(), "nothing is sent")
				assert.Len(t, f.normanSeen(), 2)
			})
		}
	}
}

// TestNormanPostResponses asserts what each response to the one action request
// ends with: 2xx accepted, 403, 404 and 422 as the status table gives them, a
// 409 as a retryable CONFLICT, and 500, 502, 504 and an error after the request
// was written as an outcome that is unknown, GENERIC and not retryable with the
// committed details and the command that reads the management node. Each
// response is one POST and never a retry.
func TestNormanPostResponses(t *testing.T) {
	t.Parallel()
	for _, c := range commandsUnderTest {
		sent := "the request to drain node worker-1"
		if c.stop {
			sent = "the request to stop the drain of node worker-1"
		}
		recovery := clierr.NewCommand("get management-node").Arg("m-1").FlagValue("--cluster", "prod")
		unknown := func(cause string) string {
			return clierr.Text(sent+" was sent, but the answer did not say whether Rancher accepted it ("+cause+"); before you run the command again, read the management node", clierr.Lead(": "), recovery)
		}
		for _, tc := range []struct {
			name      string
			status    int
			drop      bool
			code      string
			retryable bool
			message   string
		}{
			{"200", 200, false, "", false, ""},
			{"403", 403, false, clierr.CodeRBAC, false, ""},
			{"404", 404, false, clierr.CodeNotFound, false, ""},
			{"409", 409, false, clierr.CodeConflict, true, "Rancher reported a conflict (HTTP 409) on node worker-1; the request may or may not have been written; read the management node, then run the command again"},
			{"422", 422, false, clierr.CodeValidation, false, ""},
			{"500", 500, false, clierr.CodeGeneric, false, unknown("the server answered 500")},
			{"502", 502, false, clierr.CodeGeneric, false, unknown("the server answered 502")},
			{"504", 504, false, clierr.CodeGeneric, false, unknown("the server answered 504")},
			{"connection dropped after the request", 0, true, clierr.CodeGeneric, false, unknown("the connection failed")},
		} {
			t.Run(c.name+" "+tc.name, func(t *testing.T) {
				t.Parallel()
				f := ownerOf(t, c)
				f.normanPostStatus, f.normanPostDrop = tc.status, tc.drop
				if tc.status == 200 {
					f.normanPostStatus = 0
				}
				p, err := plan(t, c.newCmd(f), newInput("worker-1", "--wait=false"))
				require.NoError(t, err)
				_, err = p.exec(t.Context())
				assert.Len(t, f.postsSeen(), 1, "one POST, never retried")
				if tc.code == "" {
					require.NoError(t, err)
					return
				}
				ce := requireCLIError(t, err)
				assert.Equal(t, tc.code, ce.Code)
				assert.Equal(t, tc.retryable, ce.Retryable)
				if tc.message != "" {
					assert.Equal(t, tc.message, ce.Message)
				}
				if tc.code == clierr.CodeGeneric {
					assert.Equal(t, recovery.Field(), ce.Hint)
					assert.Equal(t, clierr.CommittedContextDetails{Committed: true, ResourceKind: "node", ResourceName: "worker-1", Recovery: recovery.Field()}, ce.Details)
				}
			})
		}
	}
}

// TestNormanEffect asserts every field of the Effect of the Norman path, the
// warnings in order, the diff of the two fields (one for stop-drain) and the
// display identity.
func TestNormanEffect(t *testing.T) {
	t.Parallel()
	norman := []string{normanNoLocalAccess, normanNoDryRun, normanTaints}
	t.Run("drain", func(t *testing.T) {
		t.Parallel()
		f := ownerOf(t, commandsUnderTest[0])
		p, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		require.NoError(t, err)
		assert.Equal(t, safety.ActionUpdate, p.eff.Action)
		assert.Equal(t, "nodes/m-1", p.eff.Target)
		assert.Equal(t, "node", p.eff.DisplayKind)
		assert.Equal(t, "worker-1", p.eff.DisplayName)
		assert.Equal(t, safety.PreviewServerReads, p.eff.PreviewSource)
		assert.Equal(t, safety.WriteViaRancherAction, p.eff.WriteVia)
		assert.Equal(t, "c-1", p.eff.Cluster)
		assert.False(t, p.eff.ConflictDetection)
		assert.Empty(t, p.eff.ResourceVersion)
		assert.True(t, p.eff.Destructive)
		assert.False(t, p.eff.Reversible)
		assert.Equal(t, append(append([]string(nil), norman...),
			"Rancher runs kubectl drain, which cordons the node and then evicts the pods listed here through the Eviction API, respecting PodDisruptionBudgets",
			"Rancher runs the drain as its system user for cluster c-1, not with your permissions",
			"stopping a drain leaves the node cordoned; evicted pods are not put back",
			"this pod list is read now; Rancher lists the pods again when the drain runs",
		), p.eff.Warnings)
		for _, path := range diffPaths(p.eff) {
			assert.True(t, strings.HasPrefix(path, "/spec/desiredNodeUnschedulable") || strings.HasPrefix(path, "/spec/nodeDrainInput"), path)
		}
		assert.Contains(t, diffPaths(p.eff), "/spec/desiredNodeUnschedulable")
		assert.Contains(t, strings.Join(diffPaths(p.eff), " "), "/spec/nodeDrainInput")
	})
	t.Run("stop-drain", func(t *testing.T) {
		t.Parallel()
		f := ownerOf(t, commandsUnderTest[1])
		p, err := plan(t, newStopCmd(f), newInput("worker-1"))
		require.NoError(t, err)
		assert.Equal(t, safety.WriteViaRancherAction, p.eff.WriteVia)
		assert.Equal(t, safety.PreviewServerReads, p.eff.PreviewSource)
		assert.False(t, p.eff.Destructive)
		assert.False(t, p.eff.Reversible)
		assert.False(t, p.eff.ConflictDetection)
		assert.Empty(t, p.eff.ResourceVersion)
		assert.Equal(t, append(append([]string(nil), norman...), "the node stays cordoned; evicted pods are not put back; uncordon it by setting spec.unschedulable to false on the node"), p.eff.Warnings)
		assert.Equal(t, []string{"/spec/desiredNodeUnschedulable"}, diffPaths(p.eff))
	})
	t.Run("the warning of the missing actions follows the three", func(t *testing.T) {
		t.Parallel()
		f := ownerOf(t, commandsUnderTest[1])
		f.norman = []map[string]any{normanNode("c-1:m-1", "draining")}
		p, err := plan(t, newStopCmd(f), newInput("worker-1"))
		require.NoError(t, err)
		assert.Equal(t, "Rancher's API listed no actions for node worker-1; the command checked the node's state against the rules the Rancher UI uses", p.eff.Warnings[3])
	})
	t.Run("the diff hides the ssh key", func(t *testing.T) {
		t.Parallel()
		row, ok := kinds.ByKey("managementnode")
		require.True(t, ok)
		listed := unstr(map[string]any{"spec": map[string]any{"customConfig": map[string]any{"sshKey": "old"}}})
		changed := listed.DeepCopy()
		require.NoError(t, unstructured.SetNestedField(changed.Object, "new", "spec", "customConfig", "sshKey"))
		fd := found{t: target{node: "worker-1", id: "c-1", mid: "m-1"}, row: row, listed: listed, norman: &normanPath{}}
		eff, _ := fd.normanUpdate(changed, kinds.NodeDrain, nil)
		require.Len(t, eff.Diff, 1)
		assert.Equal(t, engine.Redacted, eff.Diff[0].From)
		assert.Equal(t, engine.Redacted, eff.Diff[0].To)
	})
}

// normanWaitFixture is a drain through Rancher's node action whose waits read
// the Steve API: the management node as the plan listed it, with a Drained
// condition read at t0.
func normanWaitFixture(t *testing.T) *fakeRancher {
	t.Helper()
	f := ownerOf(t, commandsUnderTest[0])
	obj := managementNode("m-1", "worker-1", "7", workerSpec(), drainedCond("False", "old failure", "t0"))
	f.steveItems = []map[string]any{steveView(obj, "active", "")}
	return f
}

// TestNormanWaitReadsSteve asserts the wait after the request reads through
// the Steve API only: no Kubernetes request follows the POST, a drained node
// ends the wait, a 403 ends it with exit 0 and the warning of the Kubernetes
// path, a 404 is the removed text, and a Drained condition unchanged since
// Plan does not end the wait.
func TestNormanWaitReadsSteve(t *testing.T) {
	fastWait(t)
	running := steveView(managementNode("m-1", "worker-1", "8", workerSpec("desiredNodeUnschedulable", "drain"), drainedCond("Unknown", "", "t1")), "draining", "")
	done := steveView(managementNode("m-1", "worker-1", "9", workerSpec(), map[string]any{"type": "Drained", "status": "True", "reason": "Drained", "message": "node drained", "lastUpdateTime": "t2"}), "active", "")
	run := func(t *testing.T, f *fakeRancher, flags ...string) (runtime.Result, error) {
		t.Helper()
		p, err := plan(t, newDrainCmd(f, false), newInput("worker-1", flags...))
		require.NoError(t, err)
		return p.exec(t.Context())
	}
	t.Run("drained", func(t *testing.T) {
		f := normanWaitFixture(t)
		f.steveReads = []map[string]any{running, done}
		res, err := run(t, f)
		require.NoError(t, err)
		assert.Equal(t, "node/worker-1 drained", strings.Split(res.Text, "\n")[0])
		data := res.Data.(map[string]any)
		assert.Equal(t, true, data["waited"])
		assert.Equal(t, true, data["drained"])
		afterPost := false
		for _, r := range f.seen() {
			if strings.HasPrefix(r, "POST /v3/nodes/") {
				afterPost = true
				continue
			}
			if afterPost {
				assert.True(t, strings.HasPrefix(r, "GET /v1/management.cattle.io.nodes/c-1/m-1"), "after the POST only the Steve read: %s", r)
			}
		}
		assert.True(t, afterPost)
	})
	t.Run("a Steve 403 ends the wait with a warning", func(t *testing.T) {
		f := normanWaitFixture(t)
		f.steveStatus = 403
		res, err := run(t, f)
		require.NoError(t, err)
		assert.Equal(t, "node/worker-1 drain requested", strings.Split(res.Text, "\n")[0])
		assert.Equal(t, false, res.Data.(map[string]any)["waited"])
		assert.Equal(t, []string{"your access to node worker-1 ended after the drain request; the drain was requested and not confirmed"}, res.Warnings)
	})
	t.Run("a Steve 404 is the removed text", func(t *testing.T) {
		f := normanWaitFixture(t)
		f.steveStatus = 404
		_, err := run(t, f)
		ce := requireCLIError(t, err)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.True(t, strings.HasPrefix(ce.Message, "node worker-1 was removed while it was draining"), ce.Message)
	})
	t.Run("a condition unchanged since the plan does not end the wait", func(t *testing.T) {
		f := normanWaitFixture(t)
		f.steveReads = []map[string]any{steveView(managementNode("m-1", "worker-1", "8", workerSpec("desiredNodeUnschedulable", "drain"), drainedCond("False", "old failure", "t0")), "draining", "")}
		_, err := run(t, f, "--timeout=60ms")
		ce := requireCLIError(t, err)
		assert.True(t, strings.HasPrefix(ce.Message, "timed out after 60ms waiting for node worker-1 to drain"), ce.Message)
		assert.NotContains(t, ce.Message, "Rancher could not drain")
	})
	t.Run("stop-drain waits on Steve", func(t *testing.T) {
		f := ownerOf(t, commandsUnderTest[1])
		stopped := steveView(managementNode("m-1", "worker-1", "9", workerSpec("internalNodeSpec", map[string]any{"unschedulable": true})), "cordoned", "")
		f.steveReads = []map[string]any{stopped}
		p, err := plan(t, newStopCmd(f), newInput("worker-1"))
		require.NoError(t, err)
		res, err := p.exec(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "node/worker-1 drain stopped; the node stays cordoned", strings.Split(res.Text, "\n")[0])
	})
}
