package provisioning

import (
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// saveInput is the input of cluster save-etcd-snapshot prod: pairs of a flag
// name and its value, "wait" taking "true" or "false".
func saveInput(extra ...string) mcInput {
	in := mcInput{a: []string{"prod"}, s: map[string]string{}, b: map[string]bool{}, is: map[string]bool{}}
	for i := 0; i+1 < len(extra); i += 2 {
		switch extra[i] {
		case "wait":
			in.b["wait"] = extra[i+1] == "true"
			in.is["wait"] = true
		default:
			in.s[extra[i]] = extra[i+1]
			in.is[extra[i]] = true
		}
	}
	return in
}

// controlPlaneWith is an RKEControlPlane whose status copy of the snapshot
// request, its phase and its Ready condition are the given ones.
func controlPlaneWith(request map[string]any, phase, reason, message string) *unstructured.Unstructured {
	status := map[string]any{}
	if request != nil {
		status["etcdSnapshotCreate"] = request
	}
	if phase != "" {
		status["etcdSnapshotCreatePhase"] = phase
	}
	if reason != "" || message != "" {
		status["conditions"] = []any{map[string]any{"type": "Ready", "status": "False", "reason": reason, "message": message}}
	}
	return &unstructured.Unstructured{Object: map[string]any{"status": status}}
}

// managementWith is the management Cluster the readiness check reads.
func managementWith(driver string, conditions ...map[string]any) *unstructured.Unstructured {
	items := make([]any, 0, len(conditions))
	for _, c := range conditions {
		items = append(items, c)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "Cluster",
		"metadata": map[string]any{"name": "c-m-prod1", "resourceVersion": "9"},
		"status":   map[string]any{"driver": driver, "conditions": items},
	}}
}

func condition(typ, status, message string) map[string]any {
	return map[string]any{"type": typ, "status": status, "message": message}
}

func readyManagement() *unstructured.Unstructured {
	return managementWith("rke2", condition("Connected", "True", ""))
}

// saveFixture serves a provisioned cluster prod whose spec.rkeConfig is rke,
// with the given RKEControlPlane states one after the other, and a ready
// management Cluster.
func saveFixture(t *testing.T, rke map[string]any, states ...*unstructured.Unstructured) *upgradeFixture {
	t.Helper()
	f := newUpgradeFixture(t)
	if len(states) == 0 {
		states = []*unstructured.Unstructured{controlPlaneWith(nil, "", "", "")}
	}
	f.local.put(provisioningClusterGVR, "fleet-default", "prod", provisionedObject("v1.31.4+rke2r1", rke))
	f.local.put(rkeControlPlaneGVR, "fleet-default", "prod", states...)
	f.steve.set(readyManagement())
	return f
}

func (f *upgradeFixture) savePlan(t *testing.T, in mcInput) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	cmd := NewSaveEtcdSnapshot(f.factory, fakeRootFactory{rc: classifiedRoot{srv: f.srv}}).(runtime.MutatingCommand)
	return cmd.Plan(t.Context(), in)
}

// snapshotField returns spec.rkeConfig.etcdSnapshotCreate of an object.
func snapshotField(obj *unstructured.Unstructured) (map[string]any, bool) {
	v, found, _ := unstructured.NestedMap(obj.Object, "spec", "rkeConfig", "etcdSnapshotCreate")
	return v, found
}

// TestSaveFlagRefusalsMakeNoRequest asserts --wait=false with --timeout and a
// bad --timeout are refused before any request.
func TestSaveFlagRefusalsMakeNoRequest(t *testing.T) {
	t.Parallel()
	rows := map[string]struct {
		in   mcInput
		want string
	}{
		"wait false with timeout": {saveInput("wait", "false", "timeout", "5m"), "--wait=false conflicts with --timeout; remove one"},
		"bad duration":            {saveInput("timeout", "soon"), `--timeout "soon" is not a valid duration, e.g. 30m`},
		"zero duration":           {saveInput("timeout", "0s"), `--timeout "0s" must be a positive duration, e.g. 30m`},
	}
	for name, tc := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := saveFixture(t, map[string]any{})
			_, _, err := f.savePlan(t, tc.in)
			ce := asCode(t, err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			require.ErrorContains(t, err, tc.want)
			assert.Empty(t, f.local.requests)
			assert.Empty(t, *f.ids, "no client was asked for")
		})
	}
}

// TestSaveNeedsACluster asserts a missing <cluster> is refused with no
// request.
func TestSaveNeedsACluster(t *testing.T) {
	t.Parallel()
	f := saveFixture(t, map[string]any{})
	in := saveInput()
	in.a = nil
	_, _, err := f.savePlan(t, in)
	ce := asCode(t, err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Empty(t, f.local.requests)
}

// TestSaveNoProvisioningCluster asserts the NOT_FOUND refusal with its text
// and its hint, with the namespace in the hint only when it is not
// fleet-default.
func TestSaveNoProvisioningCluster(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	_, _, err := f.savePlan(t, saveInput())
	ce := asCode(t, err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "cluster prod has no provisioning cluster in namespace fleet-default; an admin or a user with the Create Clusters role can list them", ce.Message)
	assert.Equal(t, clierr.NewCommand("get provisioning-clusters").Field(), ce.Hint)

	_, _, err = f.savePlan(t, saveInput("namespace", "ws-two"))
	ce = asCode(t, err)
	assert.Equal(t, clierr.NewCommand("get provisioning-clusters").FlagValue("--namespace", "ws-two").Field(), ce.Hint)
	assert.Contains(t, ce.Message, "in namespace ws-two;")
	assert.Empty(t, f.local.updates)
}

// TestSavePreview asserts the preview of a request: the Effect is a server
// dry-run, gated, not reversible, with the one field in its diff, and the
// warnings in order.
func TestSavePreview(t *testing.T) {
	t.Parallel()
	f := saveFixture(t, map[string]any{})
	eff, exec, err := f.savePlan(t, saveInput())
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, safety.PreviewServerDryRun, eff.PreviewSource)
	assert.Equal(t, safety.ActionUpdate, eff.Action)
	assert.False(t, eff.Reversible)
	assert.True(t, eff.Destructive)
	assert.True(t, eff.ConflictDetection)
	assert.Equal(t, "7", eff.ResourceVersion)
	assert.Equal(t, "provisioning-cluster", eff.DisplayKind)
	require.Len(t, eff.Diff, 1)
	assert.Contains(t, eff.Diff[0].Path, "etcdSnapshotCreate")
	assert.Equal(t, []string{
		"the server-side dry-run checks the request; it does not run the etcd snapshot",
		"Rancher runs one operation at a time on a cluster; while one is in progress, Rancher does not apply an upgrade or a configuration change",
		"Rancher runs `rke2 etcd-snapshot save` on every etcd node of cluster prod; each etcd node keeps its snapshot file; Rancher creates an etcd snapshot object in namespace fleet-default for each snapshot file once it reads the file from the cluster",
	}, eff.Warnings)
	require.Len(t, f.local.updates, 1)
	assert.True(t, f.local.updates[0].dryRun)
}

// TestSaveWarningNamesTheRuntimeAndS3 asserts the runtime in the save warning
// follows the Kubernetes version, and the S3 clause appears only when
// spec.rkeConfig.etcd.s3 is set, with the bucket name or the cloud
// credential's bucket.
func TestSaveWarningNamesTheRuntimeAndS3(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name    string
		version string
		rke     map[string]any
		want    string
	}{
		{"k3s", "v1.31.4+k3s1", map[string]any{},
			"Rancher runs `k3s etcd-snapshot save` on every etcd node of cluster prod; each etcd node keeps its snapshot file; Rancher creates an etcd snapshot object in namespace fleet-default for each snapshot file once it reads the file from the cluster"},
		{"s3 with a bucket", "v1.31.4+rke2r1", map[string]any{"etcd": map[string]any{"s3": map[string]any{"bucket": "backups", "cloudCredentialName": "cattle-global-data:cc-1"}}},
			"Rancher runs `rke2 etcd-snapshot save` on every etcd node of cluster prod; each etcd node keeps its snapshot file, and because spec.rkeConfig.etcd.s3 is set, Rancher also sends the file to S3 bucket backups; Rancher creates an etcd snapshot object in namespace fleet-default for each snapshot file once it reads the file from the cluster"},
		{"s3 with no bucket", "v1.31.4+rke2r1", map[string]any{"etcd": map[string]any{"s3": map[string]any{"cloudCredentialName": "cattle-global-data:cc-1"}}},
			"Rancher runs `rke2 etcd-snapshot save` on every etcd node of cluster prod; each etcd node keeps its snapshot file, and because spec.rkeConfig.etcd.s3 is set, Rancher also sends the file to S3 (the bucket of the cloud credential); Rancher creates an etcd snapshot object in namespace fleet-default for each snapshot file once it reads the file from the cluster"},
		{"etcd with no s3", "v1.31.4+rke2r1", map[string]any{"etcd": map[string]any{"snapshotRetention": int64(5)}},
			"Rancher runs `rke2 etcd-snapshot save` on every etcd node of cluster prod; each etcd node keeps its snapshot file; Rancher creates an etcd snapshot object in namespace fleet-default for each snapshot file once it reads the file from the cluster"},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := saveFixture(t, tc.rke)
			obj := provisionedObject(tc.version, tc.rke)
			f.local.put(provisioningClusterGVR, "fleet-default", "prod", obj)
			eff, _, err := f.savePlan(t, saveInput())
			require.NoError(t, err)
			require.Len(t, eff.Warnings, 3)
			assert.Equal(t, tc.want, eff.Warnings[2])
		})
	}
}

// TestSaveBodyChangesOnlyTheOperationField asserts the dry-run and the real
// PUT have the object as read with only spec.rkeConfig.etcdSnapshotCreate
// set to the next generation, with no s3, whatever else the cluster carries.
func TestSaveBodyChangesOnlyTheOperationField(t *testing.T) {
	t.Parallel()
	ended := map[string]any{"etcdSnapshotCreate": map[string]any{"generation": int64(2)}, "etcdSnapshotCreatePhase": phaseFinished}
	rows := map[string]struct {
		rke    map[string]any
		status map[string]any
	}{
		"empty rkeConfig": {map[string]any{}, ended},
		"with s3":         {map[string]any{"etcd": map[string]any{"s3": map[string]any{"bucket": "b", "cloudCredentialName": "cc"}}}, ended},
		"fields of other operations": {
			map[string]any{
				"rotateCertificates":  map[string]any{"generation": int64(2)},
				"etcdSnapshotRestore": map[string]any{"name": "old", "generation": int64(1), "restoreRKEConfig": "none"},
				"machinePools":        []any{map[string]any{"name": "pool1", "quantity": int64(3)}},
			},
			map[string]any{
				"etcdSnapshotCreate": map[string]any{"generation": int64(2)}, "etcdSnapshotCreatePhase": phaseFinished,
				"certificateRotationGeneration": int64(2),
				"etcdSnapshotRestore":           map[string]any{"name": "old", "generation": int64(1), "restoreRKEConfig": "none"},
				"etcdSnapshotRestorePhase":      phaseFinished,
			},
		},
	}
	for name, tc := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := saveFixture(t, tc.rke, &unstructured.Unstructured{Object: map[string]any{"status": tc.status}})
			read := provisionedObject("v1.31.4+rke2r1", tc.rke)
			_, exec, err := f.savePlan(t, saveInput("wait", "false"))
			require.NoError(t, err)
			_, err = exec(t.Context())
			require.NoError(t, err)
			require.Len(t, f.local.updates, 2)
			for i, update := range f.local.updates {
				assert.Equal(t, i == 0, update.dryRun)
				field, found := snapshotField(update.obj)
				require.True(t, found)
				assert.Equal(t, map[string]any{"generation": int64(3)}, field, "no s3 and no other key")
				unstructured.RemoveNestedField(update.obj.Object, "spec", "rkeConfig", "etcdSnapshotCreate")
				want := read.DeepCopy()
				unstructured.RemoveNestedField(want.Object, "spec", "rkeConfig", "etcdSnapshotCreate")
				assert.Equal(t, want.Object, update.obj.Object, "everything else is as read")
			}
		})
	}
}

// TestSaveGeneration asserts the generation written is one more than the
// higher of the request's and the status copy's, an absent one counting as 0,
// and the request's plus one when the RKEControlPlane cannot be read.
func TestSaveGeneration(t *testing.T) {
	t.Parallel()
	gen := func(n int64) map[string]any { return map[string]any{"generation": n} }
	rows := []struct {
		name  string
		rke   map[string]any
		cp    *unstructured.Unstructured
		cpErr error
		want  int64
	}{
		{"nothing written, no status copy", map[string]any{}, controlPlaneWith(nil, "", "", ""), nil, 1},
		{"nothing written, status copy 2", map[string]any{}, controlPlaneWith(gen(2), phaseFinished, "", ""), nil, 3},
		{"nothing written, status copy 3", map[string]any{}, controlPlaneWith(gen(3), phaseFinished, "", ""), nil, 4},
		{"nothing written, status copy 5", map[string]any{}, controlPlaneWith(gen(5), phaseFinished, "", ""), nil, 6},
		{"request 3 ended", map[string]any{"etcdSnapshotCreate": gen(3)}, controlPlaneWith(gen(3), phaseFinished, "", ""), nil, 4},
		{"request 3 failed", map[string]any{"etcdSnapshotCreate": gen(3)}, controlPlaneWith(gen(3), phaseFailed, "", ""), nil, 4},
		{"RKEControlPlane forbidden, nothing written", map[string]any{}, nil, clierr.New(clierr.CodeRBAC, "forbidden"), 1},
		{"RKEControlPlane forbidden, request 3", map[string]any{"etcdSnapshotCreate": gen(3)}, nil, clierr.New(clierr.CodeRBAC, "forbidden"), 4},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := saveFixture(t, tc.rke)
			if tc.cpErr != nil {
				f.local.errs["GET "+objKey(rkeControlPlaneGVR, "fleet-default", "prod")] = tc.cpErr
			} else {
				f.local.put(rkeControlPlaneGVR, "fleet-default", "prod", tc.cp)
			}
			_, _, err := f.savePlan(t, saveInput())
			require.NoError(t, err)
			require.Len(t, f.local.updates, 1)
			field, _ := snapshotField(f.local.updates[0].obj)
			assert.Equal(t, tc.want, field["generation"])
		})
	}
}

// TestSaveRefusesAnotherRunningOperation asserts the command refuses while
// any other operation is in progress, with the operation and its state in the
// text and no dry-run sent; an operation that ended does not refuse it.
func TestSaveRefusesAnotherRunningOperation(t *testing.T) {
	t.Parallel()
	gen := func(n int64) map[string]any { return map[string]any{"generation": n} }
	restore := func(n int64) map[string]any { return map[string]any{"name": "s1", "generation": n} }
	rows := []struct {
		name    string
		rke     map[string]any
		status  map[string]any
		running string
		state   string
		hint    bool
	}{
		{"restore, no status copy", map[string]any{"etcdSnapshotRestore": restore(1)}, map[string]any{}, "an etcd snapshot restore", "phase requested, not started", false},
		{"restore, copy differs", map[string]any{"etcdSnapshotRestore": restore(2)}, map[string]any{"etcdSnapshotRestore": restore(1), "etcdSnapshotRestorePhase": phaseFinished}, "an etcd snapshot restore", "phase Finished", false},
		{"restore, started", map[string]any{"etcdSnapshotRestore": restore(1)}, map[string]any{"etcdSnapshotRestore": restore(1), "etcdSnapshotRestorePhase": "Started"}, "an etcd snapshot restore", "phase Started", false},
		{"restore, finished", map[string]any{"etcdSnapshotRestore": restore(1)}, map[string]any{"etcdSnapshotRestore": restore(1), "etcdSnapshotRestorePhase": phaseFinished}, "", "", false},
		{"restore, failed", map[string]any{"etcdSnapshotRestore": restore(1)}, map[string]any{"etcdSnapshotRestore": restore(1), "etcdSnapshotRestorePhase": phaseFailed}, "", "", false},
		{"restore field absent", map[string]any{}, map[string]any{}, "", "", false},
		{"certificate rotation, not applied", map[string]any{"rotateCertificates": gen(2)}, map[string]any{"certificateRotationGeneration": int64(1)}, "a certificate rotation", "", false},
		{"certificate rotation, applied", map[string]any{"rotateCertificates": gen(2)}, map[string]any{"certificateRotationGeneration": int64(2)}, "", "", false},
		{"encryption key rotation, no status copy", map[string]any{"rotateEncryptionKeys": gen(1)}, map[string]any{}, "an encryption key rotation", "phase requested, not started", false},
		{"encryption key rotation, rotating", map[string]any{"rotateEncryptionKeys": gen(1)}, map[string]any{"rotateEncryptionKeys": gen(1), "rotateEncryptionKeysPhase": "Rotate"}, "an encryption key rotation", "phase Rotate", false},
		{"encryption key rotation, done", map[string]any{"rotateEncryptionKeys": gen(1)}, map[string]any{"rotateEncryptionKeys": gen(1), "rotateEncryptionKeysPhase": "Done"}, "", "", false},
		{"encryption key rotation, failed", map[string]any{"rotateEncryptionKeys": gen(1)}, map[string]any{"rotateEncryptionKeys": gen(1), "rotateEncryptionKeysPhase": phaseFailed}, "", "", false},
		{"upgrade, applied version differs", map[string]any{}, map[string]any{"appliedSpec": map[string]any{"kubernetesVersion": "v1.30.4+rke2r1"}}, "a Kubernetes upgrade", "", true},
		{"upgrade, applied version equal", map[string]any{}, map[string]any{"appliedSpec": map[string]any{"kubernetesVersion": "v1.31.4+rke2r1"}}, "", "", false},
		{"upgrade, no applied spec", map[string]any{}, map[string]any{}, "", "", false},
		{"snapshot and restore together", map[string]any{"etcdSnapshotCreate": gen(1), "etcdSnapshotRestore": restore(1)}, map[string]any{}, "an etcd snapshot restore", "phase requested, not started", false},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := saveFixture(t, tc.rke, &unstructured.Unstructured{Object: map[string]any{"status": tc.status}})
			_, _, err := f.savePlan(t, saveInput())
			if tc.running == "" {
				require.NoError(t, err)
				return
			}
			ce := asCode(t, err)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.True(t, ce.Retryable)
			want := "cluster prod has " + tc.running + " in progress;"
			if tc.state != "" {
				want = "cluster prod has " + tc.running + " in progress (" + tc.state + ");"
			}
			assert.Contains(t, ce.Message, want)
			if tc.hint {
				assert.Equal(t, clierr.NewCommand("cluster upgrade").Arg("prod").FlagValue("--k8s-version", "v1.31.4+rke2r1").Flag("--dry-run").Field(), ce.Hint)
			} else {
				assert.Empty(t, ce.Hint)
				assert.Contains(t, ce.Message, "Rancher runs one operation at a time; try again after it ends")
			}
			assert.Empty(t, f.local.updates, "no dry-run")
		})
	}
}

// TestSaveWaitingRun asserts a run while its own snapshot is in progress, in
// every phase Rancher counts as in progress, writes the object as read,
// previews no change, carries only the two common warnings and the
// waiting-run warning, and reports that it changed nothing.
func TestSaveWaitingRun(t *testing.T) {
	t.Parallel()
	gen := func(n int64) map[string]any { return map[string]any{"generation": n} }
	rows := []struct {
		name   string
		cp     *unstructured.Unstructured
		phase  string
		reason string
	}{
		{"status copy absent", controlPlaneWith(nil, "", "", ""), notStartedPhase, ""},
		{"status copy earlier", controlPlaneWith(gen(2), phaseFinished, "", ""), phaseFinished, ""},
		{"status copy later", controlPlaneWith(gen(5), phaseFinished, "", ""), phaseFinished, ""},
		{"copy equal, no phase", controlPlaneWith(gen(3), "", "", ""), notStartedPhase, ""},
		{"copy equal, started", controlPlaneWith(gen(3), "Started", "", ""), "Started", ""},
		{"copy equal, restarting", controlPlaneWith(gen(3), "RestartCluster", "", ""), "RestartCluster", ""},
		{"copy equal, unknown phase", controlPlaneWith(gen(3), "Pending", "", ""), "Pending", ""},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := saveFixture(t, map[string]any{"etcdSnapshotCreate": gen(3)}, tc.cp)
			read := provisionedObject("v1.31.4+rke2r1", map[string]any{"etcdSnapshotCreate": gen(3)})
			eff, exec, err := f.savePlan(t, saveInput("wait", "false"))
			require.NoError(t, err)
			assert.Empty(t, eff.Diff)
			assert.False(t, eff.Reversible)
			assert.Equal(t, []string{
				"the server-side dry-run checks the request; it does not run the etcd snapshot",
				"Rancher runs one operation at a time on a cluster; while one is in progress, Rancher does not apply an upgrade or a configuration change",
				"an etcd snapshot of cluster prod is in progress (phase " + tc.phase + "): nothing changes; the command does not wait for it",
			}, eff.Warnings)
			res, err := exec(t.Context())
			require.NoError(t, err)
			require.Len(t, f.local.updates, 2)
			for _, update := range f.local.updates {
				assert.Equal(t, read.Object, update.obj.Object, "the object as read")
			}
			assert.Equal(t, "cluster/prod changed nothing; the etcd snapshot already requested is in progress\n", res.Text)
			data := dataOf(t, res)
			assert.Equal(t, false, data["written"])
			assert.Equal(t, false, data["waited"])
			assert.Equal(t, int64(3), data["generation"])
		})
	}
}

// TestSaveWaitingRunWarningWhenWaiting asserts the waiting-run warning says
// the command waits when it does.
func TestSaveWaitingRunWarningWhenWaiting(t *testing.T) {
	t.Parallel()
	f := saveFixture(t, map[string]any{"etcdSnapshotCreate": map[string]any{"generation": int64(3)}},
		controlPlaneWith(map[string]any{"generation": int64(3)}, "Started", "", ""))
	eff, _, err := f.savePlan(t, saveInput())
	require.NoError(t, err)
	require.Len(t, eff.Warnings, 3)
	assert.Equal(t, "an etcd snapshot of cluster prod is in progress (phase Started): nothing changes; the command waits for it", eff.Warnings[2])
}

// TestSaveWaitingRunSkipsTheReadinessCheck asserts a run while its own
// snapshot is in progress is not refused when the management Cluster is not
// connected:
// the check that the cluster is ready applies only to a request that writes.
func TestSaveWaitingRunSkipsTheReadinessCheck(t *testing.T) {
	t.Parallel()
	gen := map[string]any{"generation": int64(3)}
	f := saveFixture(t, map[string]any{"etcdSnapshotCreate": gen}, controlPlaneWith(gen, "Started", "", ""))
	f.steve.set(managementWith("rke2", condition("Connected", "False", "agent offline")))
	eff, _, err := f.savePlan(t, saveInput())
	require.NoError(t, err)
	assert.Equal(t, []string{
		"the server-side dry-run checks the request; it does not run the etcd snapshot",
		"Rancher runs one operation at a time on a cluster; while one is in progress, Rancher does not apply an upgrade or a configuration change",
		"an etcd snapshot of cluster prod is in progress (phase Started): nothing changes; the command waits for it",
	}, eff.Warnings)
}

// TestSaveEndedSnapshotIsRequestedAgain asserts a snapshot that Finished or
// Failed is not in progress: the command writes the next generation.
func TestSaveEndedSnapshotIsRequestedAgain(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{phaseFinished, phaseFailed} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			gen := map[string]any{"generation": int64(3)}
			f := saveFixture(t, map[string]any{"etcdSnapshotCreate": gen}, controlPlaneWith(gen, phase, "", ""))
			eff, _, err := f.savePlan(t, saveInput())
			require.NoError(t, err)
			require.Len(t, f.local.updates, 1)
			field, _ := snapshotField(f.local.updates[0].obj)
			assert.Equal(t, map[string]any{"generation": int64(4)}, field)
			assert.NotEmpty(t, eff.Diff)
		})
	}
}

// TestSaveControlPlaneReads asserts a missing RKEControlPlane is refused with
// a retryable error and no dry-run, and a refused or failed read is a warning
// that skips the running-operation check and the wait.
func TestSaveControlPlaneReads(t *testing.T) {
	t.Parallel()
	t.Run("not found", func(t *testing.T) {
		t.Parallel()
		f := saveFixture(t, map[string]any{})
		f.local.errs["GET "+objKey(rkeControlPlaneGVR, "fleet-default", "prod")] = clierr.New(clierr.CodeNotFound, "missing")
		_, _, err := f.savePlan(t, saveInput())
		ce := asCode(t, err)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.True(t, ce.Retryable)
		assert.Equal(t, "cluster prod has no RKEControlPlane yet; Rancher creates it while it provisions the cluster; try again later", ce.Message)
		assert.Empty(t, f.local.updates)
	})
	for name, readErr := range map[string]error{
		"forbidden": clierr.New(clierr.CodeRBAC, "forbidden"),
		"failed":    clierr.New(clierr.CodeGeneric, "boom"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := saveFixture(t, map[string]any{"rotateCertificates": map[string]any{"generation": int64(2)}})
			f.local.errs["GET "+objKey(rkeControlPlaneGVR, "fleet-default", "prod")] = readErr
			eff, exec, err := f.savePlan(t, saveInput())
			require.NoError(t, err, "an operation in progress cannot be seen; none is refused")
			require.Len(t, eff.Warnings, 4)
			assert.Equal(t, "cannot read the RKEControlPlane of cluster prod ("+clierr.From(readErr).Code+"); this command cannot see whether an operation is in progress, and if one is, Rancher runs the etcd snapshot after it ends", eff.Warnings[3])
			res, err := exec(t.Context())
			require.NoError(t, err)
			assert.Equal(t, "cluster/prod etcd snapshot requested\n", res.Text)
			assert.Equal(t, false, dataOf(t, res)["waited"])
			assert.Equal(t, []string{"cannot read the RKEControlPlane of cluster prod; the etcd snapshot was requested and this CLI cannot wait for it"}, res.Warnings)
		})
	}
}

// TestSaveReadiness asserts a management Cluster the Rancher UI does not call
// ready is refused with the condition the check used, a cluster with neither
// condition is refused, and a failed read of the management Cluster is a
// warning that skips the check.
func TestSaveReadiness(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name string
		mgmt *unstructured.Unstructured
		want string
	}{
		{"connected false", managementWith("rke2", condition("Connected", "False", "agent offline")),
			"cluster prod is not ready (condition Connected is False: agent offline); the Rancher UI offers etcd snapshots, etcd snapshot restores, certificate rotations and encryption key rotations only on a ready cluster"},
		{"connected unknown, no message", managementWith("rke2", condition("Connected", "Unknown", "")),
			"cluster prod is not ready (condition Connected is Unknown); the Rancher UI offers etcd snapshots, etcd snapshot restores, certificate rotations and encryption key rotations only on a ready cluster"},
		{"ready false", managementWith("rke2", condition("Ready", "False", "waiting")),
			"cluster prod is not ready (condition Ready is False: waiting); the Rancher UI offers etcd snapshots, etcd snapshot restores, certificate rotations and encryption key rotations only on a ready cluster"},
		{"connected false, ready true", managementWith("rke2", condition("Ready", "True", ""), condition("Connected", "False", "down")),
			"cluster prod is not ready (condition Connected is False: down); the Rancher UI offers etcd snapshots, etcd snapshot restores, certificate rotations and encryption key rotations only on a ready cluster"},
		{"neither condition", managementWith("rke2", condition("Updated", "True", "")),
			"cluster prod is not ready (it reports no Connected or Ready condition); the Rancher UI offers etcd snapshots, etcd snapshot restores, certificate rotations and encryption key rotations only on a ready cluster"},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := saveFixture(t, map[string]any{})
			f.steve.set(tc.mgmt)
			_, _, err := f.savePlan(t, saveInput())
			ce := asCode(t, err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.False(t, ce.Retryable)
			assert.Equal(t, tc.want, ce.Message)
			assert.Empty(t, f.local.updates)
		})
	}

	t.Run("ready by the Ready condition", func(t *testing.T) {
		t.Parallel()
		f := saveFixture(t, map[string]any{})
		f.steve.set(managementWith("rke2", condition("Ready", "True", "")))
		_, _, err := f.savePlan(t, saveInput())
		require.NoError(t, err)
	})
	t.Run("read fails", func(t *testing.T) {
		t.Parallel()
		f := saveFixture(t, map[string]any{})
		f.steve.mu.Lock()
		delete(f.steve.objs, "c-m-prod1")
		f.steve.mu.Unlock()
		eff, _, err := f.savePlan(t, saveInput())
		require.NoError(t, err)
		require.Len(t, eff.Warnings, 4)
		assert.Contains(t, eff.Warnings[3], "cannot read the management cluster of cluster prod (")
		assert.Contains(t, eff.Warnings[3], "the check that the cluster is ready was skipped")
		assert.Len(t, f.local.updates, 1)
	})
	t.Run("no link to a management cluster", func(t *testing.T) {
		t.Parallel()
		f := saveFixture(t, map[string]any{})
		obj := provisionedObject("v1.31.4+rke2r1", map[string]any{})
		unstructured.RemoveNestedField(obj.Object, "status", "clusterName")
		f.local.put(provisioningClusterGVR, "fleet-default", "prod", obj)
		_, _, err := f.savePlan(t, saveInput())
		ce := asCode(t, err)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.True(t, ce.Retryable)
		assert.Equal(t, "Rancher has not linked cluster prod to its management cluster yet; try again later", ce.Message)
		assert.Empty(t, f.local.updates)
	})
}

// TestSaveRefusesAClusterWithNoRKEConfig asserts every cluster with no
// spec.rkeConfig is refused with its own text, retryable only for the two
// states that end by themselves, and no dry-run sent; a custom cluster with
// an empty spec.rkeConfig is accepted.
func TestSaveRefusesAClusterWithNoRKEConfig(t *testing.T) {
	t.Parallel()
	// the text names the four kinds exactly when the kind table has a row for
	// each
	importedText := "cluster prod is an imported cluster; Rancher takes its etcd snapshots, restores and rotations through objects this CLI does not create yet"
	if allRegistered() {
		importedText = "cluster prod is an imported cluster; Rancher takes its etcd snapshots, restores and rotations through objects of the kinds ETCDSnapshotSave, ETCDSnapshotRestore, EncryptionKeyRotation and CertificateRotation"
	}
	label := func(driver, provider string) *unstructured.Unstructured {
		obj := managementWith(driver, condition("Connected", "True", ""))
		if provider != "" {
			obj.SetLabels(map[string]string{"provider.cattle.io": provider})
		}
		return obj
	}
	rows := []struct {
		name      string
		mgmt      *unstructured.Unstructured
		unlinked  bool
		localID   bool
		want      string
		retryable bool
		code      string
	}{
		{"imported rke2 by driver", label("rke2", ""), false, false, importedText, false, clierr.CodeValidation},
		{"imported k3s by driver", label("k3s", ""), false, false, importedText, false, clierr.CodeValidation},
		{"imported by the label only", label("", "rke2"), false, false, importedText, false, clierr.CodeValidation},
		{"driver imported", label("imported", ""), false, false, "Rancher takes etcd snapshots only for RKE2 and K3s clusters; cluster prod has driver imported", false, clierr.CodeValidation},
		{"EKS", label("EKS", ""), false, false, "Rancher takes etcd snapshots only for RKE2 and K3s clusters; cluster prod has driver EKS", false, clierr.CodeValidation},
		{"AKS", label("AKS", ""), false, false, "Rancher takes etcd snapshots only for RKE2 and K3s clusters; cluster prod has driver AKS", false, clierr.CodeValidation},
		{"GKE", label("GKE", ""), false, false, "Rancher takes etcd snapshots only for RKE2 and K3s clusters; cluster prod has driver GKE", false, clierr.CodeValidation},
		{"Alibaba", label("Alibaba", ""), false, false, "Rancher takes etcd snapshots only for RKE2 and K3s clusters; cluster prod has driver Alibaba", false, clierr.CodeValidation},
		{"empty driver", label("", ""), false, false, "Rancher has not detected the distribution of cluster prod yet; try again later", true, clierr.CodeGeneric},
		{"empty cluster name", nil, true, false, "Rancher has not linked cluster prod to its management cluster yet; try again later", true, clierr.CodeGeneric},
		{"local", label("rke2", ""), false, true, "this CLI does not take etcd snapshots of the local cluster", false, clierr.CodeValidation},
	}
	for _, tc := range rows {
		for shape, mutate := range map[string]func(*unstructured.Unstructured){
			"absent": func(o *unstructured.Unstructured) { unstructured.RemoveNestedField(o.Object, "spec", "rkeConfig") },
			"null":   func(o *unstructured.Unstructured) { o.Object["spec"].(map[string]any)["rkeConfig"] = nil },
		} {
			t.Run(tc.name+", rkeConfig "+shape, func(t *testing.T) {
				t.Parallel()
				f := newUpgradeFixture(t)
				prov := provisionedObject("v1.31.4+rke2r1", nil)
				mutate(prov)
				var mgmt *unstructured.Unstructured
				if tc.mgmt != nil {
					mgmt = tc.mgmt.DeepCopy()
				}
				switch {
				case tc.unlinked:
					unstructured.RemoveNestedField(prov.Object, "status", "clusterName")
				case tc.localID:
					_ = unstructured.SetNestedField(prov.Object, "local", "status", "clusterName")
					mgmt.SetName("local")
				}
				f.local.put(provisioningClusterGVR, "fleet-default", "prod", prov)
				if mgmt != nil {
					f.steve.set(mgmt)
				}
				_, _, err := f.savePlan(t, saveInput())
				ce := asCode(t, err)
				assert.Equal(t, tc.code, ce.Code)
				assert.Equal(t, tc.retryable, ce.Retryable)
				assert.Equal(t, tc.want, ce.Message)
				assert.Empty(t, ce.Hint, "no hint")
				assert.Empty(t, f.local.updates, "no dry-run")
				assert.Zero(t, f.local.requestsOf("GET "+objKey(rkeControlPlaneGVR, "fleet-default", "prod")), "the RKEControlPlane is not read")
			})
		}
	}

	t.Run("custom cluster with an empty rkeConfig is accepted", func(t *testing.T) {
		t.Parallel()
		f := saveFixture(t, map[string]any{})
		_, _, err := f.savePlan(t, saveInput())
		require.NoError(t, err)
	})
	t.Run("management cluster read fails", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		prov := provisionedObject("v1.31.4+rke2r1", nil)
		unstructured.RemoveNestedField(prov.Object, "spec", "rkeConfig")
		f.local.put(provisioningClusterGVR, "fleet-default", "prod", prov)
		_, _, err := f.savePlan(t, saveInput())
		ce := asCode(t, err)
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
		assert.Empty(t, f.local.updates)
	})
}

// TestClassifyNoRKEConfigNamesTheKindsOnlyWhenAllExist asserts the text of an
// imported cluster names the four kinds only when the kind table has a row for
// each, and says this CLI does not create the objects yet otherwise.
func TestClassifyNoRKEConfigNamesTheKindsOnlyWhenAllExist(t *testing.T) {
	t.Parallel()
	mgmt := managementWith("rke2").Object
	all := func(string) bool { return true }
	for name, hasKind := range map[string]func(string) bool{
		"all four rows": all,
		"three rows": func(key string) bool {
			return key != "certificaterotation"
		},
		"no rows": func(string) bool { return false },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := classifyNoRKEConfig("prod", "c-m-prod1", mgmt, hasKind)
			require.Error(t, err)
			assert.Equal(t, clierr.CodeValidation, err.Code)
			assert.Empty(t, err.Hint)
			if name == "all four rows" {
				assert.Equal(t, "cluster prod is an imported cluster; Rancher takes its etcd snapshots, restores and rotations through objects of the kinds ETCDSnapshotSave, ETCDSnapshotRestore, EncryptionKeyRotation and CertificateRotation", err.Message)
				return
			}
			assert.Equal(t, "cluster prod is an imported cluster; Rancher takes its etcd snapshots, restores and rotations through objects this CLI does not create yet", err.Message)
		})
	}
}

// allRegistered reports whether the kind table has a row for each of the four
// kinds.
func allRegistered() bool {
	for _, key := range importedDay2Rows {
		if !kindRegistered(key) {
			return false
		}
	}
	return true
}

// TestKindRegisteredFollowsTheKindTable asserts kindRegistered returns true
// for the keys of the kind table, and that the text of an imported cluster
// names the four kinds exactly when the table has all four rows.
func TestKindRegisteredFollowsTheKindTable(t *testing.T) {
	t.Parallel()
	assert.True(t, kindRegistered("etcdsnapshot"))
	assert.False(t, kindRegistered("no-such-kind"))
	err := classifyNoRKEConfig("prod", "c-m-prod1", managementWith("rke2").Object, kindRegistered)
	assert.Equal(t, allRegistered(), strings.Contains(err.Message, "of the kinds ETCDSnapshotSave"))
}

// TestSaveChecksTheOperationInProgressBeforeReadiness asserts the order of the
// checks: an operation in progress is refused before the readiness of the
// cluster is read.
func TestSaveChecksTheOperationInProgressBeforeReadiness(t *testing.T) {
	t.Parallel()
	f := saveFixture(t, map[string]any{"rotateCertificates": map[string]any{"generation": int64(2)}})
	f.steve.set(managementWith("rke2", condition("Connected", "False", "down")))
	_, _, err := f.savePlan(t, saveInput())
	ce := asCode(t, err)
	assert.Contains(t, ce.Message, "certificate rotation in progress")
	assert.NotContains(t, ce.Message, "not ready")
}

// TestSaveSendsEveryRequestToTheLocalCluster asserts every request of a Plan
// goes to the local cluster's client: the provisioning Cluster, the
// RKEControlPlane and the one dry-run.
func TestSaveSendsEveryRequestToTheLocalCluster(t *testing.T) {
	t.Parallel()
	f := saveFixture(t, map[string]any{})
	_, _, err := f.savePlan(t, saveInput())
	require.NoError(t, err)
	assert.Equal(t, []string{
		"GET clusters/fleet-default/prod",
		"GET rkecontrolplanes/fleet-default/prod",
		"PUT clusters/fleet-default/prod",
	}, f.local.requests)
	assert.Empty(t, f.downstream.requests)
}
