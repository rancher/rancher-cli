package provisioning

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// gen is a request or a status copy of the etcd snapshot: only a generation.
func gen(n int64) map[string]any { return map[string]any{"generation": n} }

// waitingFixture serves a cluster whose snapshot was last requested at
// generation 2 and finished, which makes a run write generation 3. Its
// RKEControlPlane then returns the given states to the wait, one after the
// other, the last repeating. It returns the fixture and the executor.
func waitingFixture(t *testing.T, states []*unstructured.Unstructured, extra ...string) (*upgradeFixture, runtime.ExecuteFunc) {
	t.Helper()
	planned := controlPlaneWith(gen(2), phaseFinished, "", "")
	f := saveFixture(t, map[string]any{}, append([]*unstructured.Unstructured{planned}, states...)...)
	_, exec, err := f.savePlan(t, saveInput(extra...))
	require.NoError(t, err)
	return f, exec
}

// checkCommitted asserts an error of the wait after the request was accepted:
// GENERIC, not retryable, with the committed details and the preview of the
// same command as its recovery.
func checkCommitted(t *testing.T, err error, message, recovery string) {
	t.Helper()
	ce := asCode(t, err)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.False(t, ce.Retryable)
	assert.Equal(t, message, ce.Message)
	assert.Equal(t, recovery, ce.Hint)
	details, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok)
	assert.True(t, details.Committed)
	assert.Equal(t, "provisioning-cluster", details.ResourceKind)
	assert.Equal(t, "prod", details.ResourceName)
	assert.Equal(t, recovery, details.Recovery)
}

var saveRecovery = clierr.NewCommand("cluster save-etcd-snapshot").Arg("prod").Flag("--dry-run").Field()

// TestSaveNoWait asserts --wait=false returns once the update is accepted:
// the request text and data, no read after the update.
func TestSaveNoWait(t *testing.T) {
	t.Parallel()
	f, exec := waitingFixture(t, nil, "wait", "false")
	before := f.local.requestsOf("GET ")
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "cluster/prod etcd snapshot requested\n", res.Text)
	assert.Equal(t, "Cluster", res.Kind)
	assert.Equal(t, map[string]any{
		"cluster": "prod", "namespace": "fleet-default", "clusterId": "c-m-prod1",
		"field": "spec.rkeConfig.etcdSnapshotCreate", "generation": int64(3), "waited": false, "written": true,
	}, dataOf(t, res))
	assert.Equal(t, before, f.local.requestsOf("GET "), "no read after the update")
	require.Len(t, f.local.updates, 2)
	assert.False(t, f.local.updates[1].dryRun)
	assert.Empty(t, res.Warnings)
}

// TestSaveWaitEndsOnTheWrittenGenerationFinishing asserts every state of the
// wait has one end: the wait goes on while the status copy lags or is an
// earlier generation, while the phase is Started, RestartCluster, empty or
// unknown, and ends when the copy equals the generation written and the phase
// is Finished. A Finished phase of an earlier generation does not end the
// wait.
func TestSaveWaitEndsOnTheWrittenGenerationFinishing(t *testing.T) {
	t.Parallel()
	done := controlPlaneWith(gen(3), phaseFinished, "", "")
	rows := map[string][]*unstructured.Unstructured{
		"done at once":                   {done},
		"status copy absent":             {controlPlaneWith(nil, "", "", ""), done},
		"status copy lagging, finished":  {controlPlaneWith(gen(2), phaseFinished, "", ""), done},
		"status copy lagging, failed":    {controlPlaneWith(gen(2), phaseFailed, "", ""), done},
		"started":                        {controlPlaneWith(gen(3), "Started", "", ""), done},
		"restarting the cluster":         {controlPlaneWith(gen(3), "RestartCluster", "", ""), done},
		"empty phase":                    {controlPlaneWith(gen(3), "", "", ""), done},
		"unknown phase":                  {controlPlaneWith(gen(3), "Pending", "", ""), done},
		"ready false":                    {controlPlaneWith(gen(3), "Started", "Waiting", "waiting for the init node"), done},
		"started, restarting, finished":  {controlPlaneWith(gen(3), "Started", "", ""), controlPlaneWith(gen(3), "RestartCluster", "", ""), done},
		"a generation of another number": {controlPlaneWith(gen(1), phaseFinished, "", ""), done},
	}
	for name, states := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, exec := waitingFixture(t, states)
			res, err := exec(t.Context())
			require.NoError(t, err)
			assert.Equal(t, "cluster/prod etcd snapshot taken\n", res.Text)
			data := dataOf(t, res)
			assert.Equal(t, true, data["waited"])
			assert.Equal(t, true, data["written"])
			assert.Equal(t, phaseFinished, data["phase"])
			assert.Equal(t, int64(3), data["generation"])
			// the Plan read once; the wait read every state before it ended
			// and none after
			assert.Equal(t, 1+len(states), f.local.requestsOf("GET "+objKey(rkeControlPlaneGVR, "fleet-default", "prod")))
		})
	}
}

// TestSaveWaitEndsWhenTheRequestIsReplaced asserts a status copy of a later
// generation than the one written ends the wait at once with a committed
// error, whatever its phase, and that no read follows; a waiting run ends the
// same way.
func TestSaveWaitEndsWhenTheRequestIsReplaced(t *testing.T) {
	t.Parallel()
	later := func(generation, last, state string) string {
		return "etcd snapshot of cluster prod: Rancher reports generation " + generation +
			" of the request, later than generation " + last + ", which this run waits for; last state: " + state +
			"; the etcd snapshot this run asked for may not have been taken"
	}
	done := controlPlaneWith(gen(3), phaseFinished, "", "")
	rows := map[string]struct {
		state *unstructured.Unstructured
		want  string
	}{
		"finished":    {controlPlaneWith(gen(4), phaseFinished, "", ""), later("4", "3", "Finished")},
		"in progress": {controlPlaneWith(gen(4), "Started", "Waiting", "waiting for the init node"), later("4", "3", "Started, waiting for the init node")},
		"failed":      {controlPlaneWith(gen(7), phaseFailed, "", ""), later("7", "3", "Failed")},
		"no phase":    {controlPlaneWith(gen(4), "", "", ""), later("4", "3", "requested, not started")},
	}
	for name, tc := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, exec := waitingFixture(t, []*unstructured.Unstructured{tc.state, done})
			_, err := exec(t.Context())
			checkCommitted(t, err, tc.want, saveRecovery)
			assert.Equal(t, 2, f.local.requestsOf("GET "+objKey(rkeControlPlaneGVR, "fleet-default", "prod")), "the Plan read once and the wait once")
		})
	}
	t.Run("a waiting run", func(t *testing.T) {
		t.Parallel()
		f := saveFixture(t, map[string]any{"etcdSnapshotCreate": gen(3)},
			controlPlaneWith(gen(3), "Started", "", ""), controlPlaneWith(gen(4), phaseFinished, "", ""), done)
		_, exec, err := f.savePlan(t, saveInput())
		require.NoError(t, err)
		_, err = exec(t.Context())
		checkCommitted(t, err, later("4", "3", "Finished"), saveRecovery)
	})
}

// TestSaveWaitFailed asserts a phase Failed with the status copy equal to the
// generation written ends the wait with the Ready condition's reason and
// message, and, when it has none, leaves the reason out and says Rancher
// reported no message; a Failed phase of an earlier generation does not end
// the wait.
func TestSaveWaitFailed(t *testing.T) {
	t.Parallel()
	rows := map[string]struct {
		state *unstructured.Unstructured
		want  string
	}{
		"with a reason and a message": {controlPlaneWith(gen(3), phaseFailed, "WaitingForNode", "failed to run the snapshot on node n1"),
			"etcd snapshot of cluster prod failed: WaitingForNode: failed to run the snapshot on node n1"},
		"with a reason only":  {controlPlaneWith(gen(3), phaseFailed, "WaitingForNode", ""), "etcd snapshot of cluster prod failed: WaitingForNode: Rancher reported no message"},
		"with no condition":   {controlPlaneWith(gen(3), phaseFailed, "", ""), "etcd snapshot of cluster prod failed: Rancher reported no message"},
		"with a message only": {controlPlaneWith(gen(3), phaseFailed, "", "disk full"), "etcd snapshot of cluster prod failed: disk full"},
	}
	for name, tc := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, exec := waitingFixture(t, []*unstructured.Unstructured{tc.state})
			_, err := exec(t.Context())
			checkCommitted(t, err, tc.want, saveRecovery)
		})
	}
}

// TestSaveWaitEnds asserts the other three ends are committed errors: a
// timeout naming the last phase and the Ready message, a stopped command and a
// provisioning Cluster that was deleted.
func TestSaveWaitEnds(t *testing.T) {
	t.Parallel()
	t.Run("timeout names the phase and the Ready message", func(t *testing.T) {
		t.Parallel()
		_, exec := waitingFixture(t, []*unstructured.Unstructured{controlPlaneWith(gen(3), "Started", "Waiting", "waiting for the init node")}, "timeout", "1s")
		_, err := exec(t.Context())
		checkCommitted(t, err, "timed out after 1s waiting for the etcd snapshot of cluster prod; it was requested and goes on; last state: Started, waiting for the init node; to wait again, run the same command: while the etcd snapshot is in progress it changes nothing and waits; once the etcd snapshot has ended it requests a new one, and --dry-run previews that request", saveRecovery)
	})
	t.Run("timeout with no message", func(t *testing.T) {
		t.Parallel()
		_, exec := waitingFixture(t, []*unstructured.Unstructured{controlPlaneWith(gen(3), "RestartCluster", "", "")}, "timeout", "1s")
		_, err := exec(t.Context())
		require.ErrorContains(t, err, "last state: RestartCluster; to wait again")
	})
	t.Run("timeout before Rancher started the request", func(t *testing.T) {
		t.Parallel()
		_, exec := waitingFixture(t, []*unstructured.Unstructured{controlPlaneWith(gen(2), phaseFinished, "", "")}, "timeout", "1s")
		_, err := exec(t.Context())
		require.ErrorContains(t, err, "last state: Finished; to wait again")
	})
	t.Run("stopped", func(t *testing.T) {
		t.Parallel()
		_, exec := waitingFixture(t, []*unstructured.Unstructured{controlPlaneWith(gen(3), "Started", "", "")})
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(50*time.Millisecond, cancel)
		_, err := exec(ctx)
		checkCommitted(t, err, "stopped waiting for the etcd snapshot of cluster prod; it was requested and goes on", saveRecovery)
	})
	t.Run("deleted", func(t *testing.T) {
		t.Parallel()
		f, exec := waitingFixture(t, []*unstructured.Unstructured{controlPlaneWith(gen(3), "Started", "", "")})
		f.local.errSeq["GET "+objKey(rkeControlPlaneGVR, "fleet-default", "prod")] = []error{clierr.New(clierr.CodeNotFound, "gone")}
		_, err := exec(t.Context())
		checkCommitted(t, err, "cluster prod was deleted during the wait", clierr.NewCommand("get provisioning-clusters").Field())
	})
}

// TestSaveWaitRefusedRead asserts a 403 on the RKEControlPlane during the
// wait ends it with exit 0, the request text, waited false and a warning.
func TestSaveWaitRefusedRead(t *testing.T) {
	t.Parallel()
	f, exec := waitingFixture(t, []*unstructured.Unstructured{controlPlaneWith(gen(3), "Started", "", "")})
	f.local.errSeq["GET "+objKey(rkeControlPlaneGVR, "fleet-default", "prod")] = []error{clierr.New(clierr.CodeRBAC, "forbidden")}
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "cluster/prod etcd snapshot requested\n", res.Text)
	assert.Equal(t, false, dataOf(t, res)["waited"])
	assert.Equal(t, []string{"your access to the RKEControlPlane of cluster prod ended after the etcd snapshot request; the etcd snapshot was requested and not confirmed"}, res.Warnings)
}

// TestSaveWaitRetriesAServerError asserts a server error on a read of the
// wait does not end it.
func TestSaveWaitRetriesAServerError(t *testing.T) {
	t.Parallel()
	f, exec := waitingFixture(t, []*unstructured.Unstructured{controlPlaneWith(gen(3), phaseFinished, "", "")})
	f.local.errSeq["GET "+objKey(rkeControlPlaneGVR, "fleet-default", "prod")] = []error{clierr.New(clierr.CodeGeneric, "the server responded with 503").WithRetryable(true)}
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "cluster/prod etcd snapshot taken\n", res.Text)
}

// TestSaveWaitingRunWaits asserts a run while its own snapshot is in progress
// waits on the generation read, reports it changed nothing, and lists the
// snapshots.
func TestSaveWaitingRunWaits(t *testing.T) {
	t.Parallel()
	f := saveFixture(t, map[string]any{"etcdSnapshotCreate": gen(3)},
		controlPlaneWith(gen(3), "Started", "", ""), controlPlaneWith(gen(3), phaseFinished, "", ""))
	_, exec, err := f.savePlan(t, saveInput())
	require.NoError(t, err)
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "cluster/prod changed nothing; waiting for the etcd snapshot already requested\ncluster/prod etcd snapshot taken\n", res.Text)
	data := dataOf(t, res)
	assert.Equal(t, false, data["written"])
	assert.Equal(t, true, data["waited"])
	assert.Equal(t, int64(3), data["generation"])
	require.Len(t, f.local.updates, 2)
	read := provisionedObject("v1.31.4+rke2r1", map[string]any{"etcdSnapshotCreate": gen(3)})
	assert.Equal(t, read.Object, f.local.updates[1].obj.Object, "the PUT has the object as read")
}

func snapshotRecordOf(name, cluster, node, status, createdAt string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata":     map[string]any{"name": name},
		"spec":         map[string]any{"clusterName": cluster},
		"snapshotFile": map[string]any{"name": name + ".zip", "nodeName": node, "status": status, "createdAt": createdAt},
	}}
}

// TestSaveListsTheClustersSnapshots asserts a wait that ends done lists the
// cluster's own snapshots, newest first, up to ten, with the four values of a
// row, and no snapshot of another cluster.
func TestSaveListsTheClustersSnapshots(t *testing.T) {
	t.Parallel()
	var records []unstructured.Unstructured
	for i := range 12 {
		records = append(records, snapshotRecordOf(fmt.Sprintf("snap-%02d", i), "prod", "n1", "successful", fmt.Sprintf("2026-11-%02dT10:00:00Z", i+1)))
	}
	records = append(records, snapshotRecordOf("other", "elsewhere", "n9", "successful", "2026-12-01T10:00:00Z"))
	f, exec := waitingFixture(t, []*unstructured.Unstructured{controlPlaneWith(gen(3), phaseFinished, "", "")})
	f.local.putList(etcdSnapshotRow.GVR, "fleet-default", records)
	res, err := exec(t.Context())
	require.NoError(t, err)
	entries, ok := dataOf(t, res)["snapshots"].([]any)
	require.True(t, ok)
	require.Len(t, entries, 10)
	assert.Equal(t, map[string]any{"name": "snap-11", "nodeName": "n1", "createdAt": "2026-11-12T10:00:00Z", "status": "successful"}, entries[0])
	assert.Equal(t, "snap-02", entries[9].(map[string]any)["name"])
	assert.Empty(t, res.Warnings)
	assert.Equal(t, 1, f.local.requestsOf("LIST "))
}

// TestSaveListsSnapshotsThroughSteveAfterForbidden asserts a Kubernetes List
// that responds with 403 is read again through Steve as the caller, and the
// cluster's own snapshots, newest first, are in the data with no warning.
func TestSaveListsSnapshotsThroughSteveAfterForbidden(t *testing.T) {
	t.Parallel()
	f, exec := waitingFixture(t, []*unstructured.Unstructured{controlPlaneWith(gen(3), phaseFinished, "", "")})
	f.local.errs["LIST "+objKey(etcdSnapshotRow.GVR, "fleet-default", "")] = snapshotListResponse(http.StatusForbidden)
	f.steve.setSnapshots("fleet-default", []unstructured.Unstructured{
		snapshotRecordOf("old", "prod", "n1", "successful", "2026-11-01T10:00:00Z"),
		snapshotRecordOf("new", "prod", "n1", "successful", "2026-11-02T10:00:00Z"),
		snapshotRecordOf("other", "elsewhere", "n9", "successful", "2026-12-01T10:00:00Z"),
	})
	res, err := exec(t.Context())
	require.NoError(t, err)
	entries, ok := dataOf(t, res)["snapshots"].([]any)
	require.True(t, ok)
	require.Len(t, entries, 2)
	assert.Equal(t, map[string]any{"name": "new", "nodeName": "n1", "createdAt": "2026-11-02T10:00:00Z", "status": "successful"}, entries[0])
	assert.Equal(t, "old", entries[1].(map[string]any)["name"])
	assert.Empty(t, res.Warnings)
	assert.Equal(t, 1, f.steve.snapshotRequests(), "one Steve list of etcd snapshots")
}

// TestSaveSnapshotListWarning asserts a List that is refused both ways, fails
// or finds no etcd snapshot object of the cluster gives the warning with the
// get command, and no snapshots in the data, and that only a Kubernetes 403
// makes a Steve request.
func TestSaveSnapshotListWarning(t *testing.T) {
	t.Parallel()
	want := "Rancher creates an etcd snapshot object for each snapshot file once it reads the file from the cluster, and an object it created moments ago may not be readable yet; `" +
		clierr.NewCommand("get etcd-snapshot").FlagValue("--namespace", "fleet-default").Field() + "` lists them for an admin or a user with the Create Clusters role"
	listKey := "LIST " + objKey(etcdSnapshotRow.GVR, "fleet-default", "")
	rows := map[string]struct {
		setup      func(*upgradeFixture)
		steveCalls int
	}{
		"refused": {func(f *upgradeFixture) {
			f.local.errs[listKey] = snapshotListResponse(http.StatusForbidden)
			f.steve.failSnapshots("fleet-default", http.StatusForbidden)
		}, 1},
		"refused and Steve has none": {func(f *upgradeFixture) {
			f.local.errs[listKey] = snapshotListResponse(http.StatusForbidden)
		}, 1},
		"refused with no HTTP status": {func(f *upgradeFixture) {
			f.local.errs[listKey] = clierr.New(clierr.CodeRBAC, "forbidden")
			f.steve.setSnapshots("fleet-default", []unstructured.Unstructured{snapshotRecordOf("x", "prod", "n1", "successful", "2026-11-01T10:00:00Z")})
		}, 0},
		"failed": {func(f *upgradeFixture) {
			f.local.errs[listKey] = clierr.New(clierr.CodeGeneric, "boom")
			f.steve.setSnapshots("fleet-default", []unstructured.Unstructured{snapshotRecordOf("x", "prod", "n1", "successful", "2026-11-01T10:00:00Z")})
		}, 0},
		"no records": {func(f *upgradeFixture) {}, 0},
		"only of other": {func(f *upgradeFixture) {
			f.local.putList(etcdSnapshotRow.GVR, "fleet-default", []unstructured.Unstructured{snapshotRecordOf("x", "elsewhere", "n1", "successful", "2026-11-01T10:00:00Z")})
		}, 0},
	}
	for name, tc := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, exec := waitingFixture(t, []*unstructured.Unstructured{controlPlaneWith(gen(3), phaseFinished, "", "")})
			tc.setup(f)
			res, err := exec(t.Context())
			require.NoError(t, err)
			assert.Equal(t, "cluster/prod etcd snapshot taken\n", res.Text)
			assert.Equal(t, []string{want}, res.Warnings)
			assert.NotContains(t, dataOf(t, res), "snapshots")
			assert.Equal(t, true, dataOf(t, res)["waited"])
			assert.Equal(t, tc.steveCalls, f.steve.snapshotRequests(), "the Steve lists of etcd snapshots")
		})
	}
}

// TestSaveWaitUsesTheNamespace asserts the recovery, the snapshot List and
// the warning name a namespace that is not fleet-default.
func TestSaveWaitUsesTheNamespace(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	prov := provisionedObject("v1.31.4+rke2r1", map[string]any{})
	prov.SetNamespace("ws-two")
	f.local.put(provisioningClusterGVR, "ws-two", "prod", prov)
	f.local.put(rkeControlPlaneGVR, "ws-two", "prod", controlPlaneWith(nil, "", "", ""), controlPlaneWith(gen(1), "Started", "", ""))
	f.steve.set(readyManagement())
	_, exec, err := f.savePlan(t, saveInput("namespace", "ws-two", "timeout", "1s"))
	require.NoError(t, err)
	_, err = exec(t.Context())
	recovery := clierr.NewCommand("cluster save-etcd-snapshot").Arg("prod").FlagValue("--namespace", "ws-two").Flag("--dry-run").Field()
	checkCommitted(t, err, "timed out after 1s waiting for the etcd snapshot of cluster prod; it was requested and goes on; last state: Started; to wait again, run the same command: while the etcd snapshot is in progress it changes nothing and waits; once the etcd snapshot has ended it requests a new one, and --dry-run previews that request", recovery)
}
