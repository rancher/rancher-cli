package provisioning

import (
	"context"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// restoreRequestOf is a restore request or its status copy.
func restoreRequestOf(name string, generation int64, mode string) map[string]any {
	r := map[string]any{"name": name, "generation": generation}
	if mode != "" {
		r["restoreRKEConfig"] = mode
	}
	return r
}

// restoreWaitFixture serves a cluster whose restore was last requested at
// generation 2 and finished, which makes a run write generation 3 with mode
// none. Its RKEControlPlane then returns the given states to the wait, one
// after the other, the last repeating. It returns the fixture and the executor.
func restoreWaitFixture(t *testing.T, states []*unstructured.Unstructured, extra ...string) (*upgradeFixture, runtime.ExecuteFunc) {
	t.Helper()
	planned := restoreControlPlane(restoreRequestOf("old", 2, "none"), phaseFinished, "", "")
	f := restoreFixture(t, map[string]any{}, append([]*unstructured.Unstructured{planned}, states...)...)
	_, exec, err := f.restorePlan(t, restoreInput("s1", append([]string{restoreModeFlag, "none"}, extra...)...))
	require.NoError(t, err)
	return f, exec
}

var restoreWaitRecovery = restoreRecoveryOf("s1", "none").Field()

// restoreDone is the state of the written generation after Rancher ended it.
func restoreDone() *unstructured.Unstructured {
	return restoreControlPlane(restoreRequestOf("s1", 3, "none"), phaseFinished, "", "")
}

// TestRestoreNoWait asserts --wait=false returns once the update is accepted:
// the request text and data, no read after the update.
func TestRestoreNoWait(t *testing.T) {
	t.Parallel()
	f, exec := restoreWaitFixture(t, nil, "wait", "false")
	before := f.local.requestsOf("GET ")
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "cluster/prod etcd snapshot restore requested\n", res.Text)
	assert.Equal(t, "Cluster", res.Kind)
	assert.Equal(t, map[string]any{
		"cluster": "prod", "namespace": "fleet-default", "clusterId": "c-m-prod1",
		"field": "spec.rkeConfig.etcdSnapshotRestore", "generation": int64(3), "waited": false, "written": true,
		"snapshot": "s1", "restoreMode": "none",
	}, dataOf(t, res))
	assert.Equal(t, before, f.local.requestsOf("GET "), "no read after the update")
	require.Len(t, f.local.updates, 2)
	assert.False(t, f.local.updates[1].dryRun)
	assert.Empty(t, res.Warnings)
}

// TestRestoreWaitEndsOnTheWrittenGenerationFinishing asserts every state of
// the wait has one end: the wait goes on while the status copy lags or is an
// earlier generation, while the phase is Started, RestartCluster, empty or
// unknown, and ends when the copy equals the generation written and the phase
// is Finished. A Finished phase of an earlier generation does not end the
// wait.
func TestRestoreWaitEndsOnTheWrittenGenerationFinishing(t *testing.T) {
	t.Parallel()
	req := func(n int64) map[string]any { return restoreRequestOf("s1", n, "none") }
	rows := map[string][]*unstructured.Unstructured{
		"done at once":                   {restoreDone()},
		"status copy absent":             {restoreControlPlane(nil, "", "", ""), restoreDone()},
		"status copy lagging, finished":  {restoreControlPlane(req(2), phaseFinished, "", ""), restoreDone()},
		"status copy lagging, failed":    {restoreControlPlane(req(2), phaseFailed, "", ""), restoreDone()},
		"started":                        {restoreControlPlane(req(3), "Started", "", ""), restoreDone()},
		"restarting the cluster":         {restoreControlPlane(req(3), "RestartCluster", "", ""), restoreDone()},
		"empty phase":                    {restoreControlPlane(req(3), "", "", ""), restoreDone()},
		"unknown phase":                  {restoreControlPlane(req(3), "Pending", "", ""), restoreDone()},
		"ready false":                    {restoreControlPlane(req(3), "Started", "Waiting", "waiting for the init node"), restoreDone()},
		"started, restarting, finished":  {restoreControlPlane(req(3), "Started", "", ""), restoreControlPlane(req(3), "RestartCluster", "", ""), restoreDone()},
		"a generation of another number": {restoreControlPlane(req(1), phaseFinished, "", ""), restoreDone()},
	}
	for name, states := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, exec := restoreWaitFixture(t, states)
			res, err := exec(t.Context())
			require.NoError(t, err)
			assert.Equal(t, "cluster/prod restored from etcd snapshot s1\n", res.Text)
			data := dataOf(t, res)
			assert.Equal(t, true, data["waited"])
			assert.Equal(t, true, data["written"])
			assert.Equal(t, phaseFinished, data["phase"])
			assert.Equal(t, int64(3), data["generation"])
			assert.Equal(t, "s1", data["snapshot"])
			assert.Equal(t, "none", data["restoreMode"])
			assert.NotContains(t, data, "snapshots", "the restore lists no snapshots")
			assert.Equal(t, 1+len(states), f.local.requestsOf("GET "+objKey(rkeControlPlaneGVR, "fleet-default", "prod")))
			assert.Zero(t, f.local.requestsOf("LIST "), "no list after the restore")
		})
	}
}

// TestRestoreWaitEndsWhenTheRequestIsReplaced asserts a status copy of a later
// generation than the one written ends the wait at once with a committed
// error, whatever its phase, and that no read follows; a waiting run ends the
// same way.
func TestRestoreWaitEndsWhenTheRequestIsReplaced(t *testing.T) {
	t.Parallel()
	later := func(generation, last, state string) string {
		return "etcd snapshot restore of cluster prod: Rancher reports generation " + generation +
			" of the request, later than generation " + last + ", which this run waits for; last state: " + state +
			"; the etcd snapshot restore this run asked for may not have been taken"
	}
	req := func(n int64) map[string]any { return restoreRequestOf("s9", n, "all") }
	rows := map[string]struct {
		state *unstructured.Unstructured
		want  string
	}{
		"finished":    {restoreControlPlane(req(4), phaseFinished, "", ""), later("4", "3", "Finished")},
		"in progress": {restoreControlPlane(req(4), "Started", "Waiting", "waiting for the init node"), later("4", "3", "Started, waiting for the init node")},
		"failed":      {restoreControlPlane(req(7), phaseFailed, "", ""), later("7", "3", "Failed")},
		"no phase":    {restoreControlPlane(req(4), "", "", ""), later("4", "3", "requested, not started")},
	}
	for name, tc := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, exec := restoreWaitFixture(t, []*unstructured.Unstructured{tc.state, restoreDone()})
			_, err := exec(t.Context())
			checkCommitted(t, err, tc.want, restoreWaitRecovery)
			assert.Equal(t, 2, f.local.requestsOf("GET "+objKey(rkeControlPlaneGVR, "fleet-default", "prod")), "the Plan read once and the wait once")
		})
	}
	t.Run("a waiting run", func(t *testing.T) {
		t.Parallel()
		f := restoreFixture(t, map[string]any{"etcdSnapshotRestore": restoreRequestOf("s1", 3, "none")},
			restoreControlPlane(restoreRequestOf("s1", 3, "none"), "Started", "", ""), restoreControlPlane(req(4), phaseFinished, "", ""), restoreDone())
		_, exec, err := f.restorePlan(t, restoreInput("s1"))
		require.NoError(t, err)
		_, err = exec(t.Context())
		checkCommitted(t, err, later("4", "3", "Finished"), restoreRecoveryOf("s1", "").Field())
	})
}

// TestRestoreWaitFailed asserts a phase Failed with the status copy equal to
// the generation written ends the wait with the Ready condition's reason and
// message, and, when it has none, says Rancher reported no message; a Failed
// phase of an earlier generation does not end the wait.
func TestRestoreWaitFailed(t *testing.T) {
	t.Parallel()
	req := restoreRequestOf("s1", 3, "none")
	rows := map[string]struct {
		state *unstructured.Unstructured
		want  string
	}{
		"with a reason and a message": {restoreControlPlane(req, phaseFailed, "WaitingForNode", "failed to restore on node n1"),
			"etcd snapshot restore of cluster prod failed: WaitingForNode: failed to restore on node n1"},
		"with a reason only":  {restoreControlPlane(req, phaseFailed, "WaitingForNode", ""), "etcd snapshot restore of cluster prod failed: WaitingForNode: Rancher reported no message"},
		"with no condition":   {restoreControlPlane(req, phaseFailed, "", ""), "etcd snapshot restore of cluster prod failed: Rancher reported no message"},
		"with a message only": {restoreControlPlane(req, phaseFailed, "", "disk full"), "etcd snapshot restore of cluster prod failed: disk full"},
	}
	for name, tc := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, exec := restoreWaitFixture(t, []*unstructured.Unstructured{tc.state})
			_, err := exec(t.Context())
			checkCommitted(t, err, tc.want, restoreWaitRecovery)
		})
	}
}

// TestRestoreWaitEnds asserts the other three ends are committed errors: a
// timeout naming the last phase and the Ready message, a stopped command and a
// provisioning Cluster that was deleted.
func TestRestoreWaitEnds(t *testing.T) {
	t.Parallel()
	started := restoreControlPlane(restoreRequestOf("s1", 3, "none"), "Started", "Waiting", "waiting for the init node")
	t.Run("timeout names the phase and the Ready message", func(t *testing.T) {
		t.Parallel()
		_, exec := restoreWaitFixture(t, []*unstructured.Unstructured{started}, "timeout", "1s")
		_, err := exec(t.Context())
		checkCommitted(t, err, "timed out after 1s waiting for the etcd snapshot restore of cluster prod; it was requested and goes on; last state: Started, waiting for the init node; to see its phase, run the command in the hint, which has --dry-run: while the restore is in progress it sends no write and shows the phase; once the restore has ended it previews a new restore, which --apply --yes would request", restoreWaitRecovery)
	})
	t.Run("timeout before Rancher started the request", func(t *testing.T) {
		t.Parallel()
		_, exec := restoreWaitFixture(t, []*unstructured.Unstructured{restoreControlPlane(restoreRequestOf("old", 2, "none"), phaseFinished, "", "")}, "timeout", "1s")
		_, err := exec(t.Context())
		require.ErrorContains(t, err, "last state: Finished; to see its phase")
	})
	t.Run("stopped", func(t *testing.T) {
		t.Parallel()
		_, exec := restoreWaitFixture(t, []*unstructured.Unstructured{started})
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(50*time.Millisecond, cancel)
		_, err := exec(ctx)
		checkCommitted(t, err, "stopped waiting for the etcd snapshot restore of cluster prod; it was requested and goes on", restoreWaitRecovery)
	})
	t.Run("deleted", func(t *testing.T) {
		t.Parallel()
		f, exec := restoreWaitFixture(t, []*unstructured.Unstructured{started})
		f.local.errSeq["GET "+objKey(rkeControlPlaneGVR, "fleet-default", "prod")] = []error{clierr.New(clierr.CodeNotFound, "gone")}
		_, err := exec(t.Context())
		checkCommitted(t, err, "cluster prod was deleted during the wait", clierr.NewCommand("get provisioning-clusters").Field())
	})
}

// TestRestoreWaitRefusedRead asserts a 403 on the RKEControlPlane during the
// wait ends it with exit 0, the request text, waited false and a warning.
func TestRestoreWaitRefusedRead(t *testing.T) {
	t.Parallel()
	f, exec := restoreWaitFixture(t, []*unstructured.Unstructured{restoreControlPlane(restoreRequestOf("s1", 3, "none"), "Started", "", "")})
	f.local.errSeq["GET "+objKey(rkeControlPlaneGVR, "fleet-default", "prod")] = []error{clierr.New(clierr.CodeRBAC, "forbidden")}
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "cluster/prod etcd snapshot restore requested\n", res.Text)
	assert.Equal(t, false, dataOf(t, res)["waited"])
	assert.Equal(t, []string{"your access to the RKEControlPlane of cluster prod ended after the etcd snapshot restore request; the etcd snapshot restore was requested and not confirmed"}, res.Warnings)
}

// TestRestoreWithAnUnreadableRKEControlPlaneDoesNotWait asserts a refused or
// failed read of the RKEControlPlane in the Plan is a warning, and the result
// says the CLI cannot wait.
func TestRestoreWithAnUnreadableRKEControlPlaneDoesNotWait(t *testing.T) {
	t.Parallel()
	for name, readErr := range map[string]error{
		"forbidden": clierr.New(clierr.CodeRBAC, "forbidden"),
		"failed":    clierr.New(clierr.CodeGeneric, "boom"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := restoreFixture(t, map[string]any{})
			f.local.errs["GET "+objKey(rkeControlPlaneGVR, "fleet-default", "prod")] = readErr
			eff, exec, err := f.restorePlan(t, restoreInput("s1", restoreModeFlag, "none"))
			require.NoError(t, err)
			assert.Contains(t, eff.Warnings, "cannot read the RKEControlPlane of cluster prod ("+clierr.From(readErr).Code+"); this command cannot see whether an operation is in progress, and if one is, Rancher runs the etcd snapshot restore after it ends")
			res, err := exec(t.Context())
			require.NoError(t, err)
			assert.Equal(t, "cluster/prod etcd snapshot restore requested\n", res.Text)
			assert.Equal(t, false, dataOf(t, res)["waited"])
			assert.Equal(t, []string{"cannot read the RKEControlPlane of cluster prod; the etcd snapshot restore was requested and this CLI cannot wait for it"}, res.Warnings)
		})
	}
}

// TestRestoreRefusesAnotherRunningOperation asserts the command refuses while
// any operation but its own restore is in progress, with the operation and its
// state in the text and no dry-run sent; the hint is the command that waits
// for it where the operation has one; the refusal for another restore is not
// retryable.
func TestRestoreRefusesAnotherRunningOperation(t *testing.T) {
	t.Parallel()
	gen := func(n int64) map[string]any { return map[string]any{"generation": n} }
	snapshotCmd := saveEtcdSnapshotRecovery("prod", "fleet-default").Field()
	rows := []struct {
		name    string
		rke     map[string]any
		status  map[string]any
		running string
		hint    string
	}{
		{"snapshot", map[string]any{"etcdSnapshotCreate": gen(2)}, map[string]any{}, "an etcd snapshot", snapshotCmd},
		{"certificate rotation", map[string]any{"rotateCertificates": gen(2)}, map[string]any{"certificateRotationGeneration": int64(1)}, "a certificate rotation", ""},
		{"encryption key rotation", map[string]any{"rotateEncryptionKeys": gen(1)}, map[string]any{}, "an encryption key rotation", ""},
		{"upgrade", map[string]any{}, map[string]any{"appliedSpec": map[string]any{"kubernetesVersion": "v1.30.4+rke2r1"}}, "a Kubernetes upgrade", clierr.NewCommand("cluster upgrade").Arg("prod").FlagValue("--k8s-version", "v1.31.4+rke2r1").Flag("--dry-run").Field()},
		{"restore of another snapshot", map[string]any{"etcdSnapshotRestore": restoreRequestOf("s0", 1, "all")}, map[string]any{}, "an etcd snapshot restore",
			clierr.NewCommand("cluster restore-etcd-snapshot").Arg("prod").FlagValue("--snapshot", "s0").FlagValue("--restore-mode", "all").Flag("--dry-run").Field()},
		{"restore with another mode", map[string]any{"etcdSnapshotRestore": restoreRequestOf("s1", 1, "kubernetesVersion")}, map[string]any{}, "an etcd snapshot restore",
			clierr.NewCommand("cluster restore-etcd-snapshot").Arg("prod").FlagValue("--snapshot", "s1").FlagValue("--restore-mode", "kubernetesVersion").Flag("--dry-run").Field()},
		{"restore with an empty mode against all", map[string]any{"etcdSnapshotRestore": restoreRequestOf("s1", 1, "")}, map[string]any{}, "an etcd snapshot restore",
			clierr.NewCommand("cluster restore-etcd-snapshot").Arg("prod").FlagValue("--snapshot", "s1").FlagValue("--restore-mode", "none").Flag("--dry-run").Field()},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := restoreFixture(t, tc.rke, &unstructured.Unstructured{Object: map[string]any{"status": tc.status}})
			_, _, err := f.restorePlan(t, restoreInput("s1", restoreModeFlag, "all"))
			ce := asCode(t, err)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.Equal(t, tc.running != "an etcd snapshot restore", ce.Retryable, "only the refusal for a restore is not retryable")
			assert.Contains(t, ce.Message, "cluster prod has "+tc.running+" in progress")
			if tc.hint != "" {
				assert.Equal(t, tc.hint, ce.Hint)
			} else {
				assert.Empty(t, ce.Hint)
			}
			assert.Empty(t, f.local.updates, "no dry-run")
			assert.Zero(t, f.local.requestsOf("GET "+objKey(etcdSnapshotRow.GVR, "fleet-default", "s1")), "the etcd snapshot is not read")
		})
	}
	t.Run("an ended restore, an ended rotation and a restore field with no name do not refuse", func(t *testing.T) {
		t.Parallel()
		f := restoreFixture(t, map[string]any{"etcdSnapshotRestore": restoreRequestOf("old", 1, "none"), "rotateCertificates": gen(2)},
			&unstructured.Unstructured{Object: map[string]any{"status": map[string]any{
				"etcdSnapshotRestore": restoreRequestOf("old", 1, "none"), "etcdSnapshotRestorePhase": phaseFinished, "certificateRotationGeneration": int64(2),
			}}})
		_, _, err := f.restorePlan(t, restoreInput("s1", restoreModeFlag, "none"))
		require.NoError(t, err)
	})
}

// TestRestoreWaitingRun asserts a run while its own restore is in progress,
// with the same etcd snapshot object and a mode that equals the running one,
// that is not typed, or that reads empty as none, writes the object as read,
// previews no change, carries only the two common warnings and the waiting-run
// warning, reads no etcd snapshot object and waits on the generation read.
func TestRestoreWaitingRun(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name    string
		running map[string]any
		typed   string
		mode    string
	}{
		{"same mode typed", restoreRequestOf("s1", 3, "all"), "all", "all"},
		{"no mode typed against all", restoreRequestOf("s1", 3, "all"), "", "all"},
		{"no mode typed against kubernetesVersion", restoreRequestOf("s1", 3, "kubernetesVersion"), "", "kubernetesVersion"},
		{"none typed against an empty mode", restoreRequestOf("s1", 3, ""), "none", "none"},
		{"no mode typed against an empty mode", restoreRequestOf("s1", 3, ""), "", "none"},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := restoreFixture(t, map[string]any{"etcdSnapshotRestore": tc.running},
				restoreControlPlane(tc.running, "Started", "", ""), restoreControlPlane(tc.running, phaseFinished, "", ""))
			read := restoreProvisioning(map[string]any{"etcdSnapshotRestore": tc.running})
			in := restoreInput("s1")
			if tc.typed != "" {
				in = restoreInput("s1", restoreModeFlag, tc.typed)
			}
			eff, exec, err := f.restorePlan(t, in)
			require.NoError(t, err)
			assert.Empty(t, eff.Diff)
			assert.False(t, eff.Reversible)
			assert.Equal(t, []string{
				restoreDryRunWarning,
				oneOperationWarning,
				"an etcd snapshot restore of cluster prod is in progress (phase Started): nothing changes; the command waits for it; the restore in progress is of etcd-snapshot s1 with restore mode " + tc.mode,
			}, eff.Warnings)
			res, err := exec(t.Context())
			require.NoError(t, err)
			require.Len(t, f.local.updates, 2)
			for _, update := range f.local.updates {
				assert.Equal(t, read.Object, update.obj.Object, "the object as read")
			}
			assert.Equal(t, "cluster/prod changed nothing; waiting for the etcd snapshot restore already requested\ncluster/prod restored from etcd snapshot s1\n", res.Text)
			data := dataOf(t, res)
			assert.Equal(t, false, data["written"])
			assert.Equal(t, true, data["waited"])
			assert.Equal(t, int64(3), data["generation"])
			assert.Equal(t, "s1", data["snapshot"])
			assert.Equal(t, tc.mode, data["restoreMode"], "the running mode")
			assert.Zero(t, f.local.requestsOf("GET "+objKey(etcdSnapshotRow.GVR, "fleet-default", "s1")), "no etcd snapshot object is read")
			assert.Zero(t, f.local.requestsOf("LIST "))
		})
	}
	t.Run("no wait", func(t *testing.T) {
		t.Parallel()
		running := restoreRequestOf("s1", 3, "all")
		f := restoreFixture(t, map[string]any{"etcdSnapshotRestore": running}, restoreControlPlane(running, "Started", "", ""))
		eff, exec, err := f.restorePlan(t, restoreInput("s1", "wait", "false"))
		require.NoError(t, err)
		assert.Equal(t, "an etcd snapshot restore of cluster prod is in progress (phase Started): nothing changes; the command does not wait for it; the restore in progress is of etcd-snapshot s1 with restore mode all", eff.Warnings[2])
		res, err := exec(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "cluster/prod changed nothing; the etcd snapshot restore already requested is in progress\n", res.Text)
	})
}

// TestRestoreWaitingRunWithAnUpgradeItem asserts the upgrade item that Rancher's
// restore of mode kubernetesVersion or all raises, by writing the snapshot's
// Kubernetes version into the provisioning Cluster, does not refuse the
// waiting run of that restore, and refuses it for a restore of mode none, or
// of another etcd snapshot object.
func TestRestoreWaitingRunWithAnUpgradeItem(t *testing.T) {
	t.Parallel()
	applied := map[string]any{"kubernetesVersion": "v1.30.4+rke2r1"}
	rows := []struct {
		name    string
		running map[string]any
		wait    bool
	}{
		{"all", restoreRequestOf("s1", 3, "all"), true},
		{"kubernetesVersion", restoreRequestOf("s1", 3, "kubernetesVersion"), true},
		{"none", restoreRequestOf("s1", 3, "none"), false},
		{"empty mode", restoreRequestOf("s1", 3, ""), false},
		{"another snapshot", restoreRequestOf("s0", 3, "all"), false},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cp := restoreControlPlane(tc.running, "Started", "", "")
			_ = unstructured.SetNestedField(cp.Object, applied, "status", "appliedSpec")
			f := restoreFixture(t, map[string]any{"etcdSnapshotRestore": tc.running}, cp)
			eff, _, err := f.restorePlan(t, restoreInput("s1"))
			if !tc.wait {
				ce := asCode(t, err)
				assert.Equal(t, clierr.CodeGeneric, ce.Code)
				assert.Empty(t, f.local.updates)
				return
			}
			require.NoError(t, err)
			assert.Empty(t, eff.Diff)
			assert.Len(t, eff.Warnings, 3)
		})
	}
}
