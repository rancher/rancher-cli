package provisioning

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// provisionedAt is the provisioning Cluster as the wait reads it: the
// generation Rancher has seen and the Updated condition.
func provisionedAt(version string, observed int64, updated, message string) *unstructured.Unstructured {
	obj := provisionedObject(version, nil)
	obj.SetGeneration(4)
	_ = unstructured.SetNestedField(obj.Object, observed, "status", "observedGeneration")
	_ = unstructured.SetNestedSlice(obj.Object, []any{map[string]any{"type": "Updated", "status": updated, "message": message}}, "status", "conditions")
	return obj
}

const upgradeTarget = "v1.31.5+rke2r1"

// provisionedWait prepares a provisioned cluster at v1.31.4 whose wait reads
// the given states one after the other, and returns the executor of an upgrade
// to v1.31.5 with the given extra flags.
func provisionedWait(t *testing.T, f *upgradeFixture, states []*unstructured.Unstructured, nodeLists [][]unstructured.Unstructured, extra ...string) runtime.ExecuteFunc {
	t.Helper()
	f.provisioned("v1.31.4+rke2r1")
	f.local.put(provisioningClusterGVR, "fleet-default", "prod", append([]*unstructured.Unstructured{provisionedObject("v1.31.4+rke2r1", nil)}, states...)...)
	f.downstream.putList(nodeRow.GVR, "", append([][]unstructured.Unstructured{{node("n1", "v1.31.4+rke2r1"), node("n2", "v1.31.4+rke2r1")}}, nodeLists...)...)
	_, exec, err := f.plan(t, upgradeInput("prod", upgradeTarget, extra...))
	require.NoError(t, err)
	return exec
}

func allAt(version string) []unstructured.Unstructured {
	return []unstructured.Unstructured{node("n1", version), node("n2", version)}
}

func dataOf(t *testing.T, res runtime.Result) map[string]any {
	t.Helper()
	data, ok := res.Data.(map[string]any)
	require.True(t, ok)
	return data
}

// TestUpgradeNoWait asserts --wait=false returns once the update is accepted:
// the request text and data, no read after the update.
func TestUpgradeNoWait(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	exec := provisionedWait(t, f, nil, nil, "wait", "false")
	getsBefore := f.local.requestsOf("GET ")
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "cluster/prod upgrade requested\n", res.Text)
	assert.Equal(t, map[string]any{"cluster": "prod", "namespace": "fleet-default", "clusterId": "c-m-prod1", "from": "v1.31.4+rke2r1", "to": upgradeTarget, "waited": false}, dataOf(t, res))
	assert.Equal(t, "Cluster", res.Kind)
	assert.Equal(t, getsBefore, f.local.requestsOf("GET "), "no read after the update")
	require.Len(t, f.local.updates, 2)
	assert.False(t, f.local.updates[1].dryRun)
}

// TestUpgradeWaitProvisioned asserts every state of the provisioned wait has
// one end: it waits while Rancher has not seen the generation, while Updated
// is not True and while a Node runs another version, and it ends when all hold.
func TestUpgradeWaitProvisioned(t *testing.T) {
	t.Parallel()
	done := provisionedAt(upgradeTarget, 4, "True", "")
	rows := map[string]struct {
		states []*unstructured.Unstructured
		nodes  [][]unstructured.Unstructured
	}{
		"done at once":              {[]*unstructured.Unstructured{done}, [][]unstructured.Unstructured{allAt(upgradeTarget)}},
		"generation lagging":        {[]*unstructured.Unstructured{provisionedAt(upgradeTarget, 3, "True", ""), done}, [][]unstructured.Unstructured{allAt(upgradeTarget)}},
		"Updated Unknown":           {[]*unstructured.Unstructured{provisionedAt(upgradeTarget, 4, "Unknown", "waiting"), done}, [][]unstructured.Unstructured{allAt(upgradeTarget)}},
		"Updated False":             {[]*unstructured.Unstructured{provisionedAt(upgradeTarget, 4, "False", "planner error"), done}, [][]unstructured.Unstructured{allAt(upgradeTarget)}},
		"some nodes, then all":      {[]*unstructured.Unstructured{done, done}, [][]unstructured.Unstructured{{node("n1", upgradeTarget), node("n2", "v1.31.4+rke2r1")}, allAt(upgradeTarget)}},
		"a node of the old build":   {[]*unstructured.Unstructured{done, done}, [][]unstructured.Unstructured{{node("n1", upgradeTarget), node("n2", "v1.31.5+rke2r0")}, allAt(upgradeTarget)}},
		"node with no build suffix": {[]*unstructured.Unstructured{done}, [][]unstructured.Unstructured{{node("n1", "v1.31.5"), node("n2", "v1.31.5")}}},
	}
	for name, tc := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			exec := provisionedWait(t, f, tc.states, tc.nodes)
			res, err := exec(t.Context())
			require.NoError(t, err)
			assert.Equal(t, "cluster/prod upgraded to "+upgradeTarget+"\n", res.Text)
			data := dataOf(t, res)
			assert.Equal(t, true, data["waited"])
			nodes, ok := data["nodes"].([]nodeVersion)
			require.True(t, ok)
			assert.Len(t, nodes, 2)
			assert.Empty(t, res.Warnings)
		})
	}
}

// TestUpgradeWaitImported asserts an imported cluster is done when its
// Upgraded condition is True and it runs the target or newer, and not before.
func TestUpgradeWaitImported(t *testing.T) {
	t.Parallel()
	yes := "true"
	state := func(running, upgraded string) *unstructured.Unstructured {
		return importedManagement(&yes, running, upgradeTarget, upgraded)
	}
	noVersion := state("v1.31.4+rke2r1", "True")
	unstructured.RemoveNestedField(noVersion.Object, "status", "version")
	rows := map[string][]*unstructured.Unstructured{
		"True with the target":        {state(upgradeTarget, "True")},
		"True with a newer version":   {state("v1.32.0+rke2r1", "True")},
		"True with the old version":   {state("v1.31.4+rke2r1", "True"), state(upgradeTarget, "True")},
		"Unknown with the target":     {state(upgradeTarget, "Unknown"), state(upgradeTarget, "True")},
		"no version, then the target": {noVersion, state(upgradeTarget, "True")},
	}
	for name, states := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.imported(&yes, "v1.31.4+rke2r1", "v1.31.4+rke2r1", "True")
			f.local.put(managementClusterRow.GVR, "", "c-m-edge1", states...)
			_, exec, err := f.plan(t, upgradeInput("edge", upgradeTarget))
			require.NoError(t, err)
			res, err := exec(t.Context())
			require.NoError(t, err)
			assert.Equal(t, len(states), f.local.requestsOf("GET clusters//c-m-edge1"), "the wait reads every state up to the one that ends it, and no further")
			assert.Equal(t, "cluster/edge upgraded to "+upgradeTarget+"\n", res.Text)
			assert.Equal(t, true, dataOf(t, res)["waited"])
			assert.NotContains(t, dataOf(t, res), "nodes")
		})
	}
}

// TestUpgradeWaitRefusedReads asserts a 403 on the object's read, and on the
// Node List after the Plan listed the nodes, ends the wait with exit 0, the
// request text, waited false and a warning.
func TestUpgradeWaitRefusedReads(t *testing.T) {
	t.Parallel()
	const warning = "your access to cluster prod ended after the upgrade request; the upgrade was requested and not confirmed"
	t.Run("object", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		exec := provisionedWait(t, f, nil, nil)
		f.local.errSeq["GET "+objKey(provisioningClusterGVR, "fleet-default", "prod")] = []error{clierr.New(clierr.CodeRBAC, "forbidden")}
		res, err := exec(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "cluster/prod upgrade requested\n", res.Text)
		assert.Equal(t, false, dataOf(t, res)["waited"])
		assert.Equal(t, []string{warning}, res.Warnings)
	})
	t.Run("node list", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		exec := provisionedWait(t, f, []*unstructured.Unstructured{provisionedAt(upgradeTarget, 4, "True", "")}, nil)
		f.downstream.errSeq["LIST "+objKey(nodeRow.GVR, "", "")] = []error{clierr.New(clierr.CodeRBAC, "forbidden")}
		res, err := exec(t.Context())
		require.NoError(t, err)
		assert.Equal(t, false, dataOf(t, res)["waited"])
		assert.Equal(t, []string{warning}, res.Warnings)
	})
}

// nodeListResponse is a Node List error as the client returns it for an HTTP
// response with the given status.
func nodeListResponse(status int) error {
	return apierrors.NewGenericServerResponse(status, "list", schema.GroupResource{Resource: "nodes"}, "", "the downstream API server responded", 0, false)
}

// TestUpgradeWaitRetriesNodeListFailures asserts a 502, 503 or 504 on the Node
// List is retried by its status and the wait ends once the Nodes read.
func TestUpgradeWaitRetriesNodeListFailures(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			done := provisionedAt(upgradeTarget, 4, "True", "")
			exec := provisionedWait(t, f, []*unstructured.Unstructured{done}, [][]unstructured.Unstructured{allAt(upgradeTarget)})
			f.downstream.errSeq["LIST "+objKey(nodeRow.GVR, "", "")] = []error{nodeListResponse(status), nil}
			listsBefore := f.downstream.requestsOf("LIST ")
			res, err := exec(t.Context())
			require.NoError(t, err)
			assert.Equal(t, "cluster/prod upgraded to "+upgradeTarget+"\n", res.Text)
			assert.Equal(t, listsBefore+2, f.downstream.requestsOf("LIST "), "the failed list and the retry")
		})
	}
}

// TestUpgradeWaitEndsOnOtherNodeListErrors asserts any Node List error other
// than a 403 and a 502, 503 or 504 ends the wait at once with the code of the
// read, the committed details and the get command of the cluster as the hint
// and the recovery, without a second list.
func TestUpgradeWaitEndsOnOtherNodeListErrors(t *testing.T) {
	t.Parallel()
	for name, listErr := range map[string]error{
		"400":              nodeListResponse(http.StatusBadRequest),
		"401":              nodeListResponse(http.StatusUnauthorized),
		"404":              nodeListResponse(http.StatusNotFound),
		"500":              nodeListResponse(http.StatusInternalServerError),
		"no HTTP response": errors.New("connection reset by peer"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			done := provisionedAt(upgradeTarget, 4, "True", "")
			exec := provisionedWait(t, f, []*unstructured.Unstructured{done}, [][]unstructured.Unstructured{allAt(upgradeTarget)}, "timeout", "5s")
			f.downstream.errSeq["LIST "+objKey(nodeRow.GVR, "", "")] = []error{listErr, nil}
			listsBefore := f.downstream.requestsOf("LIST ")
			_, err := exec(t.Context())
			ce := asCode(t, err)
			assert.Equal(t, clierr.From(listErr).Code, ce.Code)
			assert.False(t, ce.Retryable)
			assert.Equal(t, provisionedEnd.recovery, ce.Hint)
			details, ok := ce.Details.(clierr.CommittedContextDetails)
			require.True(t, ok)
			assert.True(t, details.Committed)
			assert.Equal(t, "provisioning-cluster", details.ResourceKind)
			assert.Equal(t, "prod", details.ResourceName)
			assert.Equal(t, provisionedEnd.recovery, details.Recovery)
			assert.Equal(t, listsBefore+1, f.downstream.requestsOf("LIST "), "no second list")
		})
	}
}

// TestUpgradeWaitWithoutNodes asserts a Plan that could not list the Nodes
// waits on the generation and Updated only.
func TestUpgradeWaitWithoutNodes(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	f.provisioned("v1.31.4+rke2r1")
	f.local.put(provisioningClusterGVR, "fleet-default", "prod", provisionedObject("v1.31.4+rke2r1", nil), provisionedAt(upgradeTarget, 4, "True", ""))
	f.downstream.errs["LIST "+objKey(nodeRow.GVR, "", "")] = clierr.New(clierr.CodeRBAC, "forbidden")
	_, exec, err := f.plan(t, upgradeInput("prod", upgradeTarget))
	require.NoError(t, err)
	listsBefore := f.downstream.requestsOf("LIST ")
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "cluster/prod upgraded to "+upgradeTarget+"\n", res.Text)
	assert.Equal(t, listsBefore, f.downstream.requestsOf("LIST "), "the wait lists no node")
	assert.NotContains(t, dataOf(t, res), "nodes")
}

// waitEndCase names what a failed end of the wait commits for one kind of
// cluster: the resource kind and name of the committed details and the get
// command that reads the cluster.
type waitEndCase struct {
	kind, name, recovery string
}

var (
	provisionedEnd = waitEndCase{"provisioning-cluster", "prod", clierr.NewCommand("get provisioning-cluster").Arg("prod").FlagValue("--namespace", "fleet-default").Field()}
	importedEnd    = waitEndCase{"cluster", "c-m-edge1", clierr.NewCommand("get cluster").Arg("c-m-edge1").Field()}
)

// checkWaitEnd asserts err is GENERIC, not retryable, carries the committed
// details and the get command of the case, and has exactly the message.
func checkWaitEnd(t *testing.T, err error, end waitEndCase, message string) {
	t.Helper()
	ce := asCode(t, err)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.False(t, ce.Retryable)
	assert.Equal(t, message, ce.Message)
	assert.Equal(t, end.recovery, ce.Hint)
	details, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok)
	assert.True(t, details.Committed)
	assert.Equal(t, end.kind, details.ResourceKind)
	assert.Equal(t, end.name, details.ResourceName)
	assert.Equal(t, end.recovery, details.Recovery)
}

// TestUpgradeWaitEnds asserts the three failed ends of a provisioned cluster's
// wait are GENERIC, not retryable and carry the committed details and the get
// command: a timeout naming the last state, a cancel and an object that was
// deleted. A timeout or a cancel that comes while the Node List runs ends the
// same way.
func TestUpgradeWaitEnds(t *testing.T) {
	t.Parallel()
	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		exec := provisionedWait(t, f, []*unstructured.Unstructured{provisionedAt(upgradeTarget, 4, "Unknown", "waiting on the plan")}, [][]unstructured.Unstructured{{node("n1", upgradeTarget), node("n2", "v1.31.4+rke2r1")}}, "timeout", "100ms")
		_, err := exec(t.Context())
		checkWaitEnd(t, err, provisionedEnd, "timed out after 100ms waiting for cluster prod to run "+upgradeTarget+"; the upgrade was requested and goes on; last state: waiting on the plan, 1/2 nodes at "+upgradeTarget+"; to wait again, run this command again with the same version: it changes nothing and waits")
	})
	t.Run("timeout while the node list responds with 503", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		exec := provisionedWait(t, f, []*unstructured.Unstructured{provisionedAt(upgradeTarget, 4, "True", "")}, nil, "timeout", "100ms")
		f.downstream.errs["LIST "+objKey(nodeRow.GVR, "", "")] = nodeListResponse(http.StatusServiceUnavailable)
		_, err := exec(t.Context())
		checkWaitEnd(t, err, provisionedEnd, "timed out after 100ms waiting for cluster prod to run "+upgradeTarget+"; the upgrade was requested and goes on; last state: Rancher reported no message; could not list the nodes: 503 Service Unavailable; to wait again, run this command again with the same version: it changes nothing and waits")
	})
	t.Run("timeout after the node list responded with 503 and then read", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		exec := provisionedWait(t, f, []*unstructured.Unstructured{provisionedAt(upgradeTarget, 4, "True", "")}, [][]unstructured.Unstructured{{node("n1", upgradeTarget), node("n2", "v1.31.4+rke2r1")}}, "timeout", "100ms")
		busy := nodeListResponse(http.StatusServiceUnavailable)
		f.downstream.errSeq["LIST "+objKey(nodeRow.GVR, "", "")] = []error{busy, busy, busy}
		_, err := exec(t.Context())
		checkWaitEnd(t, err, provisionedEnd, "timed out after 100ms waiting for cluster prod to run "+upgradeTarget+"; the upgrade was requested and goes on; last state: 1/2 nodes at "+upgradeTarget+"; to wait again, run this command again with the same version: it changes nothing and waits")
	})
	t.Run("timeout when no state was read", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		exec := provisionedWait(t, f, nil, nil, "timeout", "100ms")
		f.local.errs["GET "+objKey(provisioningClusterGVR, "fleet-default", "prod")] = nodeListResponse(http.StatusServiceUnavailable)
		_, err := exec(t.Context())
		checkWaitEnd(t, err, provisionedEnd, "timed out after 100ms waiting for cluster prod to run "+upgradeTarget+"; the upgrade was requested and goes on; last state: no state seen yet; to wait again, run this command again with the same version: it changes nothing and waits")
	})
	t.Run("timeout during the node list", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		exec := provisionedWait(t, f, []*unstructured.Unstructured{provisionedAt(upgradeTarget, 4, "True", "")}, nil, "timeout", "100ms")
		f.downstream.blockLists = true
		_, err := exec(t.Context())
		checkWaitEnd(t, err, provisionedEnd, "timed out after 100ms waiting for cluster prod to run "+upgradeTarget+"; the upgrade was requested and goes on; last state: Rancher reported no message; to wait again, run this command again with the same version: it changes nothing and waits")
	})
	t.Run("cancel during the node list", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		exec := provisionedWait(t, f, []*unstructured.Unstructured{provisionedAt(upgradeTarget, 4, "True", "")}, nil)
		f.downstream.blockLists = true
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(50*time.Millisecond, cancel)
		_, err := exec(ctx)
		checkWaitEnd(t, err, provisionedEnd, "stopped waiting for cluster prod to run "+upgradeTarget+"; the upgrade was requested and goes on")
	})
	t.Run("cancel", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		exec := provisionedWait(t, f, []*unstructured.Unstructured{provisionedAt(upgradeTarget, 4, "Unknown", "waiting")}, nil)
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(50*time.Millisecond, cancel)
		_, err := exec(ctx)
		checkWaitEnd(t, err, provisionedEnd, "stopped waiting for cluster prod to run "+upgradeTarget+"; the upgrade was requested and goes on")
	})
	t.Run("deleted", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		exec := provisionedWait(t, f, nil, nil)
		f.local.errSeq["GET "+objKey(provisioningClusterGVR, "fleet-default", "prod")] = []error{clierr.New(clierr.CodeNotFound, "gone")}
		_, err := exec(t.Context())
		checkWaitEnd(t, err, provisionedEnd, "cluster prod was deleted during the upgrade")
	})
}

// importedWait prepares an imported cluster at v1.31.4 whose wait reads the
// given states one after the other, and returns the executor of an upgrade to
// v1.31.5.
func importedWait(t *testing.T, f *upgradeFixture, states []*unstructured.Unstructured, extra ...string) runtime.ExecuteFunc {
	t.Helper()
	yes := "true"
	f.imported(&yes, "v1.31.4+rke2r1", "v1.31.4+rke2r1", "True")
	f.local.put(managementClusterRow.GVR, "", "c-m-edge1", append([]*unstructured.Unstructured{importedManagement(&yes, "v1.31.4+rke2r1", "v1.31.4+rke2r1", "True")}, states...)...)
	_, exec, err := f.plan(t, upgradeInput("edge", upgradeTarget, extra...))
	require.NoError(t, err)
	return exec
}

// TestUpgradeWaitEndsOfAnImportedCluster asserts the ends of an imported
// cluster's wait name its management Cluster: the refused read warns with the
// request text, and the timeout, the cancel and a deleted object commit the
// resource kind cluster, the id of the management Cluster and the get command
// that reads it.
func TestUpgradeWaitEndsOfAnImportedCluster(t *testing.T) {
	t.Parallel()
	yes := "true"
	running := importedManagement(&yes, "v1.31.4+rke2r1", upgradeTarget, "Unknown")
	key := "GET " + objKey(managementClusterRow.GVR, "", "c-m-edge1")
	t.Run("refused read", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		exec := importedWait(t, f, nil)
		f.local.errSeq[key] = []error{clierr.New(clierr.CodeRBAC, "forbidden")}
		res, err := exec(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "cluster/edge upgrade requested\n", res.Text)
		assert.Equal(t, false, dataOf(t, res)["waited"])
		assert.Equal(t, []string{"your access to cluster edge ended after the upgrade request; the upgrade was requested and not confirmed"}, res.Warnings)
	})
	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		exec := importedWait(t, f, []*unstructured.Unstructured{running}, "timeout", "100ms")
		_, err := exec(t.Context())
		checkWaitEnd(t, err, importedEnd, "timed out after 100ms waiting for cluster edge to run "+upgradeTarget+"; the upgrade was requested and goes on; last state: worker node [w1] being upgraded; to wait again, run this command again with the same version: it changes nothing and waits")
	})
	t.Run("cancel", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		exec := importedWait(t, f, []*unstructured.Unstructured{running})
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(50*time.Millisecond, cancel)
		_, err := exec(ctx)
		checkWaitEnd(t, err, importedEnd, "stopped waiting for cluster edge to run "+upgradeTarget+"; the upgrade was requested and goes on")
	})
	t.Run("deleted", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		exec := importedWait(t, f, nil)
		f.local.errSeq[key] = []error{clierr.New(clierr.CodeNotFound, "gone")}
		_, err := exec(t.Context())
		checkWaitEnd(t, err, importedEnd, "cluster edge was deleted during the upgrade")
	})
}
