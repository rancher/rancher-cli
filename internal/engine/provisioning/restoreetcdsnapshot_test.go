package provisioning

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	restoreCreated = "2026-01-01T00:00:00Z"
	restoreTaken   = "2026-06-01T00:00:00Z"

	restoreSpec = `{"kubernetesVersion":"v1.31.4+rke2r1","rkeConfig":{}}`
)

// restoreInput is the input of cluster restore-etcd-snapshot prod: the etcd
// snapshot to restore ("" types none) and pairs of a flag name and its value,
// "wait" taking "true" or "false".
func restoreInput(snapshot string, extra ...string) mcInput {
	in := saveInput(extra...)
	if snapshot != "" {
		in.s[restoreSnapshotFlag] = snapshot
		in.is[restoreSnapshotFlag] = true
	}
	return in
}

// gzipBase64Text compresses raw and encodes it as Rancher stores the cluster
// spec in an etcd snapshot's metadata.
func gzipBase64Text(t *testing.T, raw string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write([]byte(raw))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// snapshotMetadata is snapshotFile.metadata for a snapshot of a cluster spec.
func snapshotMetadata(t *testing.T, spec string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"provisioning-cluster-spec": gzipBase64Text(t, spec)})
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(b)
}

// restorableSnapshot is an etcd snapshot object the restore accepts for
// cluster prod: its own, successful, taken after the cluster was created, on
// node n1, with a Kubernetes version of 1.25 or newer and an RKE
// configuration in its metadata.
func restorableSnapshot(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rke.cattle.io/v1", "kind": "ETCDSnapshot",
		"metadata": map[string]any{"name": name, "namespace": "fleet-default", "labels": map[string]any{"rke.cattle.io/cluster-name": "prod"}},
		"spec":     map[string]any{"clusterName": "prod"},
		"snapshotFile": map[string]any{
			"name": name + ".zip", "nodeName": "n1", "status": "successful", "createdAt": restoreTaken,
			"metadata": snapshotMetadata(t, restoreSpec),
		},
	}}
}

// restoreProvisioning is the provisioning Cluster prod, created at
// restoreCreated, with the given spec.rkeConfig.
func restoreProvisioning(rke map[string]any) *unstructured.Unstructured {
	prov := provisionedObject("v1.31.4+rke2r1", rke)
	prov.Object["metadata"].(map[string]any)["creationTimestamp"] = restoreCreated
	return prov
}

// restoreControlPlane is an RKEControlPlane whose status copy of the restore
// request, its phase and its Ready condition are the given ones.
func restoreControlPlane(request map[string]any, phase, reason, message string) *unstructured.Unstructured {
	status := map[string]any{}
	if request != nil {
		status["etcdSnapshotRestore"] = request
	}
	if phase != "" {
		status["etcdSnapshotRestorePhase"] = phase
	}
	if reason != "" || message != "" {
		status["conditions"] = []any{map[string]any{"type": "Ready", "status": "False", "reason": reason, "message": message}}
	}
	return &unstructured.Unstructured{Object: map[string]any{"status": status}}
}

// restoreFixture serves a provisioned cluster prod whose spec.rkeConfig is
// rke, the etcd snapshot objects s1 (restorable), the given RKEControlPlane
// states one after the other, and a ready management Cluster.
func restoreFixture(t *testing.T, rke map[string]any, states ...*unstructured.Unstructured) *upgradeFixture {
	t.Helper()
	f := newUpgradeFixture(t)
	if len(states) == 0 {
		states = []*unstructured.Unstructured{restoreControlPlane(nil, "", "", "")}
	}
	f.local.put(provisioningClusterGVR, "fleet-default", "prod", restoreProvisioning(rke))
	f.local.put(rkeControlPlaneGVR, "fleet-default", "prod", states...)
	f.local.put(etcdSnapshotRow.GVR, "fleet-default", "s1", restorableSnapshot(t, "s1"))
	f.steve.set(readyManagement())
	return f
}

func (f *upgradeFixture) restorePlan(t *testing.T, in mcInput) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	cmd := NewRestoreEtcdSnapshot(f.factory, fakeRootFactory{rc: classifiedRoot{srv: f.srv}}).(runtime.MutatingCommand)
	return cmd.Plan(t.Context(), in)
}

// restoreRequest returns spec.rkeConfig.etcdSnapshotRestore of an object.
func restoreRequest(obj *unstructured.Unstructured) (map[string]any, bool) {
	v, found, _ := unstructured.NestedMap(obj.Object, "spec", "rkeConfig", "etcdSnapshotRestore")
	return v, found
}

// restoreRecoveryOf is the preview of the restore command the recovery names.
func restoreRecoveryOf(snapshot, mode string) clierr.Command {
	cmd := clierr.NewCommand("cluster restore-etcd-snapshot").Arg("prod").FlagValue("--snapshot", snapshot)
	if mode != "" {
		cmd = cmd.FlagValue("--restore-mode", mode)
	}
	return cmd.Flag("--dry-run")
}

const (
	restoreDryRunWarning = "the server-side dry-run checks the request; it does not run the etcd snapshot restore"
	oneOperationWarning  = "Rancher runs one operation at a time on a cluster; while one is in progress, Rancher does not apply an upgrade or a configuration change"
)

// restoreEffectWarning is the warning of what a restore does to the
// cluster; when is the text of its parenthesis after the snapshot's name.
func restoreEffectWarning(snapshot, when string) string {
	return "Rancher stops Kubernetes on every node of cluster prod (its API server is down until the restore ends), removes the server certificate files on the etcd and control plane nodes, replaces the cluster's etcd data with etcd-snapshot " + snapshot + " (" + when + "), then starts the nodes again: every Kubernetes object created or changed after that time is lost, objects deleted since then come back, and Rancher deletes the Node objects of nodes that are no longer part of the cluster; a restore cannot be undone, only followed by the restore of another snapshot"
}

const (
	modeNoneWarning = "restore type Only etcd: the cluster's configuration and Kubernetes version stay as they are"
	modeVersionText = "restore type Kubernetes version and etcd: Rancher first sets spec.kubernetesVersion from v1.31.4+rke2r1 to v1.31.4+rke2r1, the version recorded in the snapshot"
	modeAllText     = "restore type Cluster config, Kubernetes version and etcd: Rancher first replaces spec.kubernetesVersion (v1.31.4+rke2r1 to v1.31.4+rke2r1) and, from the snapshot, spec.rkeConfig machineGlobalConfig, machineSelectorConfig, chartValues, registries, upgradeStrategy, additionalManifest and networking, spec.defaultPodSecurityAdmissionConfigurationTemplateName, and the cluster agent, Fleet agent and webhook deployment customizations; machine pools are not restored"
)

// TestRestoreFlagRefusalsMakeNoRequest asserts --wait=false with --timeout and
// a bad --timeout are refused before any request.
func TestRestoreFlagRefusalsMakeNoRequest(t *testing.T) {
	t.Parallel()
	rows := map[string]struct {
		in   mcInput
		want string
	}{
		"wait false with timeout": {restoreInput("s1", "wait", "false", "timeout", "5m"), "--wait=false conflicts with --timeout; remove one"},
		"bad duration":            {restoreInput("s1", "timeout", "soon"), `--timeout "soon" is not a valid duration, e.g. 30m`},
	}
	for name, tc := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := restoreFixture(t, map[string]any{})
			_, _, err := f.restorePlan(t, tc.in)
			assert.Equal(t, clierr.CodeValidation, asCode(t, err).Code)
			require.ErrorContains(t, err, tc.want)
			assert.Empty(t, f.local.requests)
			assert.Empty(t, *f.ids, "no client was asked for")
		})
	}
}

// TestRestoreNoProvisioningCluster asserts the NOT_FOUND refusal of the
// shared texts.
func TestRestoreNoProvisioningCluster(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	_, _, err := f.restorePlan(t, restoreInput("s1"))
	ce := asCode(t, err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "cluster prod has no provisioning cluster in namespace fleet-default; an admin or a user with the Create Clusters role can list them", ce.Message)
	assert.Equal(t, clierr.NewCommand("get provisioning-clusters").Field(), ce.Hint)
	assert.Empty(t, f.local.updates)
}

// TestRestorePreview asserts the preview of a request with no --restore-mode:
// a server dry-run, gated, not reversible, with the one field in its diff, the
// request written with the default mode, and the warnings in order.
func TestRestorePreview(t *testing.T) {
	t.Parallel()
	f := restoreFixture(t, map[string]any{})
	eff, exec, err := f.restorePlan(t, restoreInput("s1"))
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, safety.PreviewServerDryRun, eff.PreviewSource)
	assert.Equal(t, safety.ActionUpdate, eff.Action)
	assert.False(t, eff.Reversible)
	assert.True(t, eff.Destructive)
	assert.True(t, eff.ConflictDetection)
	assert.Equal(t, "7", eff.ResourceVersion)
	assert.Equal(t, "provisioning-cluster", eff.DisplayKind)
	require.NotEmpty(t, eff.Diff)
	assert.Contains(t, eff.Diff[0].Path, "etcdSnapshotRestore")
	assert.Equal(t, []string{
		restoreDryRunWarning,
		oneOperationWarning,
		restoreEffectWarning("s1", "taken "+restoreTaken+", on node n1"),
		modeAllText,
		"--restore-mode was not given: the Rancher UI's default for this snapshot, all",
	}, eff.Warnings)
	require.Len(t, f.local.updates, 1)
	assert.True(t, f.local.updates[0].dryRun)
	request, _ := restoreRequest(f.local.updates[0].obj)
	assert.Equal(t, map[string]any{"generation": int64(1), "name": "s1", "restoreRKEConfig": "all"}, request)
}

// TestRestoreWarningsNameWhatTheModeReplaces asserts each of the three modes
// typed, and the default, gives the warning of its mode with the fields it
// replaces and the Rancher UI's label, and the warning that no mode was given
// only without --restore-mode.
func TestRestoreWarningsNameWhatTheModeReplaces(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name  string
		typed string
		mode  string
		want  string
	}{
		{"none typed", "none", "none", modeNoneWarning},
		{"kubernetesVersion typed", "kubernetesVersion", "kubernetesVersion", modeVersionText},
		{"all typed", "all", "all", modeAllText},
		{"default", "", "all", modeAllText},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := restoreFixture(t, map[string]any{})
			in := restoreInput("s1")
			if tc.typed != "" {
				in = restoreInput("s1", restoreModeFlag, tc.typed)
			}
			eff, _, err := f.restorePlan(t, in)
			require.NoError(t, err)
			require.GreaterOrEqual(t, len(eff.Warnings), 4)
			assert.Equal(t, tc.want, eff.Warnings[3])
			request, _ := restoreRequest(f.local.updates[0].obj)
			assert.Equal(t, tc.mode, request["restoreRKEConfig"])
			notGiven := "--restore-mode was not given: the Rancher UI's default for this snapshot, " + tc.mode
			if tc.typed == "" {
				require.Len(t, eff.Warnings, 5)
				assert.Equal(t, notGiven, eff.Warnings[4])
			} else {
				assert.Len(t, eff.Warnings, 4)
			}
		})
	}
	for _, field := range []string{"machineGlobalConfig", "machineSelectorConfig", "chartValues", "registries", "upgradeStrategy", "additionalManifest", "networking", "defaultPodSecurityAdmissionConfigurationTemplateName"} {
		assert.Contains(t, modeAllText, field)
	}
}

// TestRestoreWarningLocation asserts the parenthesis of the restore warning
// names the S3 bucket, S3 alone, the node, or only the time.
func TestRestoreWarningLocation(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"node", func(map[string]any) {}, "taken " + restoreTaken + ", on node n1"},
		{"S3 bucket", func(sf map[string]any) { sf["s3"] = map[string]any{"bucket": "backups", "cloudCredentialName": "cc"} }, "taken " + restoreTaken + ", in S3 bucket backups"},
		{"S3, no bucket", func(sf map[string]any) { sf["s3"] = map[string]any{"cloudCredentialName": "cc"} }, "taken " + restoreTaken + ", in S3"},
		{"no node and no S3", func(sf map[string]any) { delete(sf, "nodeName") }, "taken " + restoreTaken},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := restoreFixture(t, map[string]any{})
			snap := restorableSnapshot(t, "s1")
			tc.mutate(snap.Object["snapshotFile"].(map[string]any))
			f.local.put(etcdSnapshotRow.GVR, "fleet-default", "s1", snap)
			eff, _, err := f.restorePlan(t, restoreInput("s1", restoreModeFlag, "none"))
			require.NoError(t, err)
			assert.Equal(t, restoreEffectWarning("s1", tc.want), eff.Warnings[2])
		})
	}
}

// TestRestoreS3Warning asserts a snapshot stored in S3 warns when neither the
// etcd snapshot object nor the cluster names a cloud credential, and only
// then.
func TestRestoreS3Warning(t *testing.T) {
	t.Parallel()
	s3Warning := func(where string) string {
		return "etcd-snapshot s1 is " + where + "; cluster prod names no S3 cloud credential in spec.rkeConfig.etcd.s3: Rancher restores it with no S3 credentials"
	}
	rows := []struct {
		name string
		s3   map[string]any
		rke  map[string]any
		want string
	}{
		{"no credential, a bucket", map[string]any{"bucket": "backups"}, map[string]any{}, s3Warning("in S3 bucket backups")},
		{"no credential, no bucket", map[string]any{}, map[string]any{}, s3Warning("in S3")},
		{"the snapshot names one", map[string]any{"bucket": "backups", "cloudCredentialName": "cc"}, map[string]any{}, ""},
		{"the cluster names one", map[string]any{"bucket": "backups"}, map[string]any{"etcd": map[string]any{"s3": map[string]any{"cloudCredentialName": "cc"}}}, ""},
		{"not in S3", nil, map[string]any{}, ""},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := restoreFixture(t, tc.rke)
			snap := restorableSnapshot(t, "s1")
			if tc.s3 != nil {
				snap.Object["snapshotFile"].(map[string]any)["s3"] = tc.s3
			}
			f.local.put(etcdSnapshotRow.GVR, "fleet-default", "s1", snap)
			eff, _, err := f.restorePlan(t, restoreInput("s1", restoreModeFlag, "none"))
			require.NoError(t, err)
			if tc.want == "" {
				assert.Len(t, eff.Warnings, 4)
				return
			}
			require.Len(t, eff.Warnings, 5)
			assert.Equal(t, tc.want, eff.Warnings[4])
		})
	}
}

// TestRestoreEndedWarning asserts a restore of the same etcd snapshot object
// with the same mode that Rancher ended gives the warning that it restores it
// again, and another snapshot, another mode or a restore that did not finish
// does not.
func TestRestoreEndedWarning(t *testing.T) {
	t.Parallel()
	ended := "etcd-snapshot s1 is the last restore of cluster prod and it has ended; this restores it again"
	request := func(name, mode string) map[string]any {
		r := map[string]any{"name": name, "generation": int64(2)}
		if mode != "" {
			r["restoreRKEConfig"] = mode
		}
		return r
	}
	rows := []struct {
		name string
		spec map[string]any
		cp   *unstructured.Unstructured
		want bool
	}{
		{"same snapshot and mode, finished", request("s1", "none"), restoreControlPlane(request("s1", "none"), phaseFinished, "", ""), true},
		{"empty mode reads as none", request("s1", ""), restoreControlPlane(request("s1", ""), phaseFinished, "", ""), true},
		{"another snapshot", request("s0", "none"), restoreControlPlane(request("s0", "none"), phaseFinished, "", ""), false},
		{"another mode", request("s1", "all"), restoreControlPlane(request("s1", "all"), phaseFinished, "", ""), false},
		{"status copy of another generation", request("s1", "none"), restoreControlPlane(map[string]any{"name": "s1", "generation": int64(1), "restoreRKEConfig": "none"}, phaseFinished, "", ""), false},
		{"failed", request("s1", "none"), restoreControlPlane(request("s1", "none"), phaseFailed, "", ""), false},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := restoreFixture(t, map[string]any{"etcdSnapshotRestore": tc.spec}, tc.cp)
			eff, _, err := f.restorePlan(t, restoreInput("s1", restoreModeFlag, "none"))
			require.NoError(t, err)
			assert.Equal(t, tc.want, assertContains(eff.Warnings, ended))
		})
	}
}

func assertContains(warnings []string, want string) bool {
	return slices.Contains(warnings, want)
}

// TestRestoreBodyChangesOnlyTheOperationField asserts the dry-run and the
// real PUT have the object as read with only spec.rkeConfig.etcdSnapshotRestore
// set to the next generation, the name and the mode, whatever else the cluster
// carries, and that nothing of the snapshot's cluster spec is written.
func TestRestoreBodyChangesOnlyTheOperationField(t *testing.T) {
	t.Parallel()
	ended := map[string]any{"etcdSnapshotRestore": map[string]any{"name": "old", "generation": int64(2), "restoreRKEConfig": "none"}, "etcdSnapshotRestorePhase": phaseFinished}
	rows := map[string]struct {
		rke    map[string]any
		status map[string]any
	}{
		"empty rkeConfig": {map[string]any{}, ended},
		"with s3":         {map[string]any{"etcd": map[string]any{"s3": map[string]any{"bucket": "b", "cloudCredentialName": "cc"}}}, ended},
		"fields of other operations": {
			map[string]any{
				"rotateCertificates":  map[string]any{"generation": int64(2)},
				"etcdSnapshotCreate":  map[string]any{"generation": int64(1)},
				"etcdSnapshotRestore": map[string]any{"name": "old", "generation": int64(2), "restoreRKEConfig": "none"},
				"machinePools":        []any{map[string]any{"name": "pool1", "quantity": int64(3)}},
				"chartValues":         map[string]any{"rke2-calico": map[string]any{"installCRDs": true}},
			},
			map[string]any{
				"etcdSnapshotCreate": map[string]any{"generation": int64(1)}, "etcdSnapshotCreatePhase": phaseFinished,
				"certificateRotationGeneration": int64(2),
				"etcdSnapshotRestore":           map[string]any{"name": "old", "generation": int64(2), "restoreRKEConfig": "none"},
				"etcdSnapshotRestorePhase":      phaseFinished,
			},
		},
	}
	for name, tc := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := restoreFixture(t, tc.rke, &unstructured.Unstructured{Object: map[string]any{"status": tc.status}})
			read := restoreProvisioning(tc.rke)
			_, exec, err := f.restorePlan(t, restoreInput("s1", "wait", "false"))
			require.NoError(t, err)
			_, err = exec(t.Context())
			require.NoError(t, err)
			require.Len(t, f.local.updates, 2)
			for i, update := range f.local.updates {
				assert.Equal(t, i == 0, update.dryRun)
				request, found := restoreRequest(update.obj)
				require.True(t, found)
				assert.Equal(t, map[string]any{"generation": int64(3), "name": "s1", "restoreRKEConfig": "all"}, request)
				unstructured.RemoveNestedField(update.obj.Object, "spec", "rkeConfig", "etcdSnapshotRestore")
				want := read.DeepCopy()
				unstructured.RemoveNestedField(want.Object, "spec", "rkeConfig", "etcdSnapshotRestore")
				assert.Equal(t, want.Object, update.obj.Object, "everything else is as read, the Kubernetes version included")
			}
		})
	}
}

// TestRestoreGeneration asserts the generation written is one more than the
// higher of the request's and the status copy's, an absent one counting as 0,
// and the request's plus one when the RKEControlPlane cannot be read.
func TestRestoreGeneration(t *testing.T) {
	t.Parallel()
	req := func(n int64) map[string]any {
		return map[string]any{"name": "old", "generation": n, "restoreRKEConfig": "none"}
	}
	rows := []struct {
		name     string
		specGen  int64
		copyGen  int64
		readable bool
		want     int64
	}{
		{"nothing written, no status copy", 0, 0, true, 1},
		{"nothing written, status copy 2", 0, 2, true, 3},
		{"nothing written, status copy 3", 0, 3, true, 4},
		{"nothing written, status copy 5", 0, 5, true, 6},
		{"request 3 ended", 3, 3, true, 4},
		{"RKEControlPlane forbidden, nothing written", 0, 0, false, 1},
		{"RKEControlPlane forbidden, request 3", 3, 0, false, 4},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rke := map[string]any{}
			if tc.specGen > 0 {
				rke["etcdSnapshotRestore"] = req(tc.specGen)
			}
			cp := restoreControlPlane(nil, "", "", "")
			if tc.copyGen > 0 {
				cp = restoreControlPlane(req(tc.copyGen), phaseFinished, "", "")
			}
			f := restoreFixture(t, rke, cp)
			if !tc.readable {
				f.local.errs["GET "+objKey(rkeControlPlaneGVR, "fleet-default", "prod")] = clierr.New(clierr.CodeRBAC, "forbidden")
			}
			_, _, err := f.restorePlan(t, restoreInput("s1", restoreModeFlag, "none"))
			require.NoError(t, err)
			require.Len(t, f.local.updates, 1)
			request, _ := restoreRequest(f.local.updates[0].obj)
			assert.Equal(t, tc.want, request["generation"])
		})
	}
}

// TestRestoreSnapshotReads asserts the refusals of the read of the etcd
// snapshot object: a 403 is RBAC with its text, another error is its own code,
// and a missing object is NOT_FOUND with the etcd snapshot objects of the
// cluster that the command would restore, newest first, and a hint with the
// newest one; with the file name of an object it names that object, and with
// nothing restorable or a failed list it has no hint.
func TestRestoreSnapshotReads(t *testing.T) {
	t.Parallel()
	getKey := "GET " + objKey(etcdSnapshotRow.GVR, "fleet-default", "s1")
	listKey := "LIST " + objKey(etcdSnapshotRow.GVR, "fleet-default", "")
	object := func(t *testing.T, name, cluster, status, createdAt string) unstructured.Unstructured {
		t.Helper()
		o := restorableSnapshot(t, name)
		_ = unstructured.SetNestedField(o.Object, cluster, "spec", "clusterName")
		_ = unstructured.SetNestedField(o.Object, cluster, "metadata", "labels", "rke.cattle.io/cluster-name")
		_ = unstructured.SetNestedField(o.Object, status, "snapshotFile", "status")
		_ = unstructured.SetNestedField(o.Object, createdAt, "snapshotFile", "createdAt")
		return *o
	}
	t.Run("403", func(t *testing.T) {
		t.Parallel()
		f := restoreFixture(t, map[string]any{})
		f.local.errs[getKey] = snapshotListResponse(403)
		_, _, err := f.restorePlan(t, restoreInput("s1"))
		ce := asCode(t, err)
		assert.Equal(t, clierr.CodeRBAC, ce.Code)
		assert.Equal(t, "the etcd-snapshot s1 in namespace fleet-default could not be read (RBAC); Cluster Owner of the cluster grants get on its snapshots; an object Rancher created moments ago may not be readable yet", ce.Message)
		assert.Empty(t, f.local.updates)
		assert.Zero(t, f.local.requestsOf("LIST "), "no list")
	})
	t.Run("another error", func(t *testing.T) {
		t.Parallel()
		f := restoreFixture(t, map[string]any{})
		f.local.errs[getKey] = clierr.New(clierr.CodeGeneric, "boom")
		_, _, err := f.restorePlan(t, restoreInput("s1"))
		ce := asCode(t, err)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.Equal(t, "boom", ce.Message)
	})
	t.Run("404 with candidates", func(t *testing.T) {
		t.Parallel()
		f := restoreFixture(t, map[string]any{})
		delete(f.local.objects, objKey(etcdSnapshotRow.GVR, "fleet-default", "s1"))
		var items []unstructured.Unstructured
		for i := range 12 {
			items = append(items, object(t, fmt.Sprintf("snap-%02d", i), "prod", "successful", fmt.Sprintf("2026-07-%02dT10:00:00Z", i+1)))
		}
		items = append(items,
			object(t, "failed", "prod", "failed", "2026-08-01T10:00:00Z"),
			object(t, "other", "elsewhere", "successful", "2026-08-02T10:00:00Z"))
		f.local.putList(etcdSnapshotRow.GVR, "fleet-default", items)
		_, _, err := f.restorePlan(t, restoreInput("missing"))
		ce := asCode(t, err)
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
		assert.Equal(t, "no etcd-snapshot missing in namespace fleet-default", ce.Message)
		require.Len(t, ce.Candidates, 10)
		assert.Equal(t, clierr.Candidate{ID: "snap-11", Name: "snap-11.zip"}, ce.Candidates[0])
		assert.Equal(t, clierr.NewCommand("cluster restore-etcd-snapshot").Arg("prod").FlagValue("--snapshot", "snap-11").Flag("--dry-run").Field(), ce.Hint)
		assert.Zero(t, f.local.requestsOf("PUT "), "no dry-run")
	})
	t.Run("404 with the file name of an object", func(t *testing.T) {
		t.Parallel()
		f := restoreFixture(t, map[string]any{})
		delete(f.local.objects, objKey(etcdSnapshotRow.GVR, "fleet-default", "s1"))
		f.local.putList(etcdSnapshotRow.GVR, "fleet-default", []unstructured.Unstructured{object(t, "snap-07", "prod", "successful", "2026-07-01T10:00:00Z")})
		_, _, err := f.restorePlan(t, restoreInput("snap-07.zip"))
		ce := asCode(t, err)
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
		assert.Equal(t, "no etcd-snapshot snap-07.zip in namespace fleet-default; snap-07.zip is the snapshot file name of etcd-snapshot snap-07; --snapshot takes the etcd-snapshot name", ce.Message)
		assert.Empty(t, ce.Candidates)
		assert.Empty(t, ce.Hint)
	})
	t.Run("404 with nothing restorable", func(t *testing.T) {
		t.Parallel()
		f := restoreFixture(t, map[string]any{})
		delete(f.local.objects, objKey(etcdSnapshotRow.GVR, "fleet-default", "s1"))
		f.local.putList(etcdSnapshotRow.GVR, "fleet-default", []unstructured.Unstructured{object(t, "failed", "prod", "failed", "2026-08-01T10:00:00Z"), object(t, "other", "elsewhere", "successful", "2026-08-02T10:00:00Z")})
		_, _, err := f.restorePlan(t, restoreInput("missing"))
		ce := asCode(t, err)
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
		assert.Empty(t, ce.Candidates)
		assert.Empty(t, ce.Hint)
	})
	t.Run("404 with a failed list", func(t *testing.T) {
		t.Parallel()
		f := restoreFixture(t, map[string]any{})
		delete(f.local.objects, objKey(etcdSnapshotRow.GVR, "fleet-default", "s1"))
		f.local.errs[listKey] = clierr.New(clierr.CodeGeneric, "boom")
		_, _, err := f.restorePlan(t, restoreInput("missing"))
		ce := asCode(t, err)
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
		assert.Equal(t, "no etcd-snapshot missing in namespace fleet-default", ce.Message)
		assert.Empty(t, ce.Candidates)
		assert.Empty(t, ce.Hint)
	})
	t.Run("404 in another namespace", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		prov := restoreProvisioning(map[string]any{})
		prov.SetNamespace("ws-two")
		f.local.put(provisioningClusterGVR, "ws-two", "prod", prov)
		f.local.put(rkeControlPlaneGVR, "ws-two", "prod", restoreControlPlane(nil, "", "", ""))
		f.steve.set(readyManagement())
		f.local.putList(etcdSnapshotRow.GVR, "ws-two", []unstructured.Unstructured{object(t, "snap-01", "prod", "successful", "2026-07-01T10:00:00Z")})
		_, _, err := f.restorePlan(t, restoreInput("missing", "namespace", "ws-two"))
		ce := asCode(t, err)
		assert.Equal(t, "no etcd-snapshot missing in namespace ws-two", ce.Message)
		assert.Equal(t, clierr.NewCommand("cluster restore-etcd-snapshot").Arg("prod").FlagValue("--namespace", "ws-two").FlagValue("--snapshot", "snap-01").Flag("--dry-run").Field(), ce.Hint)
	})
}

// snapshotChange is one change to the baseline snapshot of a row of restoreRows.
type snapshotChange func(t *testing.T, snap *unstructured.Unstructured)

func setSnapshotField(value any, path ...string) snapshotChange {
	return func(_ *testing.T, snap *unstructured.Unstructured) {
		if value == nil {
			unstructured.RemoveNestedField(snap.Object, path...)
			return
		}
		_ = unstructured.SetNestedField(snap.Object, value, path...)
	}
}

func setSpec(spec string) snapshotChange {
	return func(t *testing.T, snap *unstructured.Unstructured) {
		t.Helper()
		_ = unstructured.SetNestedField(snap.Object, snapshotMetadata(t, spec), "snapshotFile", "metadata")
	}
}

func setAnnotation(modes string) snapshotChange {
	return setSnapshotField(map[string]any{"etcdsnapshot.rke.io/restore-mode-options": modes}, "metadata", "annotations")
}

func both(a, b snapshotChange) snapshotChange {
	return func(t *testing.T, snap *unstructured.Unstructured) {
		t.Helper()
		a(t, snap)
		b(t, snap)
	}
}

// restoreRow is one row of the table of every factor of the snapshot checks.
type restoreRow struct {
	name   string
	change snapshotChange
	mode   string
	// refusal is the text the refusal has, "" when the Plan accepts the row.
	refusal string
	// written is the mode written when the row is accepted.
	written string
	// large says the cluster spec in the metadata is larger than the bound.
	large bool
	// modeWarning is the warning about the restore mode the accepted row has,
	// "" when the row does not assert it.
	modeWarning string
}

func largeSpec(version string) snapshotChange {
	return func(t *testing.T, snap *unstructured.Unstructured) {
		t.Helper()
		padding := strings.Repeat("a", 16<<20+1)
		setSpec(`{"padding":"`+padding+`","kubernetesVersion":"`+version+`","rkeConfig":{}}`)(t, snap)
	}
}

// longVersion is a version of 300 bytes, over the 256 bytes Rancher parses.
var longVersion = "v1.2.3-" + strings.Repeat("a", 293)

func restoreRows() []restoreRow {
	notOffered := func(mode, offered string) string {
		return "--restore-mode " + mode + " is not offered for etcd-snapshot s1; the modes offered are: " + offered
	}
	rows := []restoreRow{
		{name: "baseline", change: func(*testing.T, *unstructured.Unstructured) {}, written: "all"},
		{name: "cluster name empty", change: setSnapshotField("", "spec", "clusterName"), refusal: "etcd-snapshot s1 has no spec.clusterName; the Rancher UI restores only a snapshot of cluster prod"},
		{name: "another cluster", change: setSnapshotField("other", "spec", "clusterName"), refusal: "etcd-snapshot s1 belongs to cluster other (spec.clusterName); restore a snapshot of cluster prod"},
		{name: "label another cluster", change: setSnapshotField("other", "metadata", "labels", "rke.cattle.io/cluster-name"), refusal: "etcd-snapshot s1 has the label rke.cattle.io/cluster-name other and spec.clusterName prod; the Rancher UI lists it under cluster other, not under cluster prod"},
		{name: "label absent", change: setSnapshotField(nil, "metadata", "labels"), written: "all"},
		{name: "status failed", change: setSnapshotField("failed", "snapshotFile", "status"), refusal: "etcd-snapshot s1 has status failed; the Rancher UI restores only a successful snapshot"},
		{name: "status empty", change: setSnapshotField(nil, "snapshotFile", "status"), refusal: "etcd-snapshot s1 has no snapshotFile.status; the Rancher UI restores only a successful snapshot"},
		{name: "createdAt absent", change: setSnapshotField(nil, "snapshotFile", "createdAt"), refusal: "etcd-snapshot s1 has no snapshotFile.createdAt; the Rancher UI does not restore it"},
		{name: "createdAt before the cluster", change: setSnapshotField("2025-01-01T00:00:00Z", "snapshotFile", "createdAt"), refusal: "etcd-snapshot s1 was taken at 2025-01-01T00:00:00Z, at or before the time cluster prod was created (" + restoreCreated + "); the Rancher UI does not restore a snapshot taken before the cluster was created"},
		{name: "createdAt equal to the cluster's", change: setSnapshotField(restoreCreated, "snapshotFile", "createdAt"), refusal: "at or before the time cluster prod was created"},
		{name: "metadata absent", change: setSnapshotField(nil, "snapshotFile", "metadata"), written: "none"},
		{name: "metadata absent, all typed", change: setSnapshotField(nil, "snapshotFile", "metadata"), mode: "all", refusal: notOffered("all", "none")},
		{name: "metadata absent, kubernetesVersion typed", change: setSnapshotField(nil, "snapshotFile", "metadata"), mode: "kubernetesVersion", refusal: notOffered("kubernetesVersion", "none")},
		{name: "metadata absent, none typed", change: setSnapshotField(nil, "snapshotFile", "metadata"), mode: "none", written: "none"},
		{name: "metadata bad base64", change: setSnapshotField("!!!", "snapshotFile", "metadata"), written: "none"},
		{name: "metadata with no entry", change: setSnapshotField(base64.StdEncoding.EncodeToString([]byte(`{}`)), "snapshotFile", "metadata"), written: "none"},
		{name: "metadata with a bad gzip entry", change: setSnapshotField(base64.StdEncoding.EncodeToString([]byte(`{"provisioning-cluster-spec":"bm90IGd6aXA="}`)), "snapshotFile", "metadata"), written: "none"},
		{name: "version only", change: setSpec(`{"kubernetesVersion":"v1.31.4+rke2r1"}`), written: "kubernetesVersion"},
		{name: "version only, all typed", change: setSpec(`{"kubernetesVersion":"v1.31.4+rke2r1"}`), mode: "all", refusal: notOffered("all", "none, kubernetesVersion")},
		{name: "version empty", change: setSpec(`{"rkeConfig":{}}`), refusal: "the metadata of etcd-snapshot s1 has no Kubernetes version; Rancher would fail the restore on every attempt"},
		{name: "version malformed, 300 bytes", change: setSpec(`{"kubernetesVersion":"` + longVersion + `","rkeConfig":{}}`), refusal: `has Kubernetes version "` + longVersion[:256] + `...", which Rancher cannot parse`},
		{name: "version malformed", change: setSpec(`{"kubernetesVersion":"1.2.3.4","rkeConfig":{}}`), refusal: `the metadata of etcd-snapshot s1 has Kubernetes version "1.2.3.4", which Rancher cannot parse; Rancher would fail the restore on every attempt`},
		{name: "version malformed, none typed", change: setSpec(`{"kubernetesVersion":"1.2.3.4","rkeConfig":{}}`), mode: "none", refusal: "which Rancher cannot parse"},
		{name: "version malformed, kubernetesVersion typed", change: setSpec(`{"kubernetesVersion":"1.2.3.4","rkeConfig":{}}`), mode: "kubernetesVersion", refusal: "which Rancher cannot parse"},
		{name: "version malformed, all typed", change: setSpec(`{"kubernetesVersion":"1.2.3.4","rkeConfig":{}}`), mode: "all", refusal: "which Rancher cannot parse"},
		{name: "version Rancher parses and the CLI's own parser refuses", change: setSpec(`{"kubernetesVersion":"v1.30","rkeConfig":{}}`), written: "all"},
		{name: "too large", change: largeSpec("v1.31.4+rke2r1"), written: "all", large: true,
			modeWarning: "restore type Cluster config, Kubernetes version and etcd: Rancher first replaces spec.kubernetesVersion (v1.31.4+rke2r1 to the version recorded in the snapshot) and, from the snapshot, spec.rkeConfig machineGlobalConfig, machineSelectorConfig, chartValues, registries, upgradeStrategy, additionalManifest and networking, spec.defaultPodSecurityAdmissionConfigurationTemplateName, and the cluster agent, Fleet agent and webhook deployment customizations; machine pools are not restored"},
		{name: "too large, kubernetesVersion typed", change: largeSpec("v1.31.4+rke2r1"), mode: "kubernetesVersion", written: "kubernetesVersion", large: true,
			modeWarning: "restore type Kubernetes version and etcd: Rancher first sets spec.kubernetesVersion from v1.31.4+rke2r1 to the version recorded in the snapshot"},
		{name: "too large, below 1.25, none typed", change: largeSpec("v1.24.0+rke2r1"), mode: "none", written: "none", large: true,
			modeWarning: "restore type Only etcd: the cluster's configuration and Kubernetes version stay as they are"},
		{name: "annotation none only", change: setAnnotation("none"), written: "none"},
		{name: "annotation none only, all typed", change: setAnnotation("none"), mode: "all", refusal: notOffered("all", "none")},
		{name: "annotation without none", change: setAnnotation("kubernetesVersion"), written: "kubernetesVersion"},
		{name: "annotation none and kubernetesVersion", change: setAnnotation("none,kubernetesVersion"), written: "kubernetesVersion"},
		{name: "annotation all three", change: setAnnotation("none,kubernetesVersion,all"), written: "all"},
		{name: "none typed", change: func(*testing.T, *unstructured.Unstructured) {}, mode: "none", written: "none"},
		{name: "kubernetesVersion typed", change: func(*testing.T, *unstructured.Unstructured) {}, mode: "kubernetesVersion", written: "kubernetesVersion"},
		{name: "all typed", change: func(*testing.T, *unstructured.Unstructured) {}, mode: "all", written: "all"},
		{name: "another cluster and a failed status", change: both(setSnapshotField("other", "spec", "clusterName"), setSnapshotField("failed", "snapshotFile", "status")), refusal: "belongs to cluster other"},
		{name: "label another cluster and createdAt before", change: both(setSnapshotField("other", "metadata", "labels", "rke.cattle.io/cluster-name"), setSnapshotField("2025-01-01T00:00:00Z", "snapshotFile", "createdAt")), refusal: "has the label rke.cattle.io/cluster-name other"},
		{name: "failed status and createdAt before", change: both(setSnapshotField("failed", "snapshotFile", "status"), setSnapshotField("2025-01-01T00:00:00Z", "snapshotFile", "createdAt")), refusal: "has status failed"},
		{name: "below 1.25, none typed", change: setSpec(`{"kubernetesVersion":"v1.24.9+rke2r1","rkeConfig":{}}`), mode: "none", refusal: "Rancher does not restore a snapshot taken before Kubernetes 1.25 onto a cluster at 1.25 or newer with restore type Only etcd (--restore-mode none); etcd-snapshot s1 has Kubernetes version v1.24.9+rke2r1 and cluster prod is at v1.31.4+rke2r1; the --restore-mode values that restore it are: kubernetesVersion, all"},
		{name: "below 1.25, all typed", change: setSpec(`{"kubernetesVersion":"v1.24.9+rke2r1","rkeConfig":{}}`), mode: "all", written: "all"},
		{name: "below 1.25, no rkeConfig, no mode typed", change: setSpec(`{"kubernetesVersion":"v1.24.9+rke2r1"}`), written: "kubernetesVersion"},
		{name: "metadata unreadable, no mode typed", change: setSnapshotField("!!!", "snapshotFile", "metadata"), written: "none"},
		{name: "annotation none only with full metadata", change: setAnnotation("none"), written: "none"},
	}
	return rows
}

// TestRestoreSnapshotChecksEveryFactor asserts each factor of the snapshot
// checks, changed one at a time from a restorable snapshot, gives its one
// outcome in the Plan: the refusal with its text and no dry-run, or the mode
// written and no refusal; a spec over the bound adds the warning and no other.
func TestRestoreSnapshotChecksEveryFactor(t *testing.T) {
	t.Parallel()
	large := "the cluster spec recorded in etcd-snapshot s1 is larger than 16 MiB and was not read: the restore modes offered are those the annotation etcdsnapshot.rke.io/restore-mode-options lists (all three when the snapshot has no such annotation), and the check for a snapshot taken before Kubernetes 1.25 was not made"
	for _, tc := range restoreRows() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := restoreFixture(t, map[string]any{})
			snap := restorableSnapshot(t, "s1")
			tc.change(t, snap)
			f.local.put(etcdSnapshotRow.GVR, "fleet-default", "s1", snap)
			in := restoreInput("s1")
			if tc.mode != "" {
				in = restoreInput("s1", restoreModeFlag, tc.mode)
			}
			eff, _, err := f.restorePlan(t, in)
			if tc.refusal != "" {
				ce := asCode(t, err)
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				assert.False(t, ce.Retryable)
				require.ErrorContains(t, err, tc.refusal)
				assert.Empty(t, f.local.updates, "no dry-run")
				return
			}
			require.NoError(t, err)
			request, _ := restoreRequest(f.local.updates[0].obj)
			assert.Equal(t, tc.written, request["restoreRKEConfig"])
			assert.Equal(t, tc.large, assertContains(eff.Warnings, large))
			if tc.modeWarning != "" {
				assert.Contains(t, eff.Warnings, tc.modeWarning)
			}
			for _, w := range eff.Warnings {
				assert.NotContains(t, w, " to ,", "no warning names an empty version")
			}
		})
	}
}

// TestRestoreBelow125NamesTheModesThatRestoreIt asserts the refusal of a
// snapshot below Kubernetes 1.25 with restore mode none names both versions and
// the modes the snapshot offers that Rancher restores, and no mode when the
// snapshot offers only none.
func TestRestoreBelow125NamesTheModesThatRestoreIt(t *testing.T) {
	t.Parallel()
	const head = "Rancher does not restore a snapshot taken before Kubernetes 1.25 onto a cluster at 1.25 or newer with restore type Only etcd (--restore-mode none); etcd-snapshot s1 has Kubernetes version v1.24.9+rke2r1 and cluster prod is at v1.31.4+rke2r1"
	rows := []struct {
		name   string
		change snapshotChange
		want   string
	}{
		{"with an rkeConfig", setSpec(`{"kubernetesVersion":"v1.24.9+rke2r1","rkeConfig":{}}`), head + "; the --restore-mode values that restore it are: kubernetesVersion, all"},
		{"without an rkeConfig", setSpec(`{"kubernetesVersion":"v1.24.9+rke2r1"}`), head + "; the --restore-mode values that restore it are: kubernetesVersion"},
		{"annotation none only", both(setSpec(`{"kubernetesVersion":"v1.24.9+rke2r1","rkeConfig":{}}`), setAnnotation("none")), head},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := restoreFixture(t, map[string]any{})
			snap := restorableSnapshot(t, "s1")
			tc.change(t, snap)
			f.local.put(etcdSnapshotRow.GVR, "fleet-default", "s1", snap)
			_, _, err := f.restorePlan(t, restoreInput("s1", restoreModeFlag, "none"))
			ce := asCode(t, err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, tc.want, ce.Message)
		})
	}
}

// TestCutVersion asserts a version longer than the 256 bytes Rancher parses is
// cut at a character boundary and ends in "...", and a version of at most 256
// bytes stays whole.
func TestCutVersion(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "v1.2.3", cutVersion("v1.2.3"))
	assert.Equal(t, strings.Repeat("a", 256), cutVersion(strings.Repeat("a", 256)))
	assert.Equal(t, strings.Repeat("a", 256)+"...", cutVersion(strings.Repeat("a", 257)))
	assert.Equal(t, strings.Repeat("a", 255)+"...", cutVersion(strings.Repeat("a", 255)+"\u00e9"), "a two-byte character that crosses the limit is dropped whole")
	assert.Equal(t, strings.Repeat("a", 254)+"\u00e9"+"...", cutVersion(strings.Repeat("a", 254)+"\u00e9"+"b"), "a character that ends at the limit is kept")
}

// TestRestoreRefusesAClusterWithNoRKEConfig asserts every cluster with no
// spec.rkeConfig is refused with the restore's texts, the same classification
// as the save's, and no dry-run is sent; a custom cluster with an empty
// spec.rkeConfig is accepted.
func TestRestoreRefusesAClusterWithNoRKEConfig(t *testing.T) {
	t.Parallel()
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
		{"driver imported", label("imported", ""), false, false, "Rancher restores etcd snapshots only on RKE2 and K3s clusters; cluster prod has driver imported", false, clierr.CodeValidation},
		{"EKS", label("EKS", ""), false, false, "Rancher restores etcd snapshots only on RKE2 and K3s clusters; cluster prod has driver EKS", false, clierr.CodeValidation},
		{"empty driver", label("", ""), false, false, "Rancher has not detected the distribution of cluster prod yet; try again later", true, clierr.CodeGeneric},
		{"empty cluster name", nil, true, false, "Rancher has not linked cluster prod to its management cluster yet; try again later", true, clierr.CodeGeneric},
		{"local", label("rke2", ""), false, true, "this CLI does not restore etcd snapshots of the local cluster", false, clierr.CodeValidation},
	}
	for _, tc := range rows {
		for shape, mutate := range map[string]func(*unstructured.Unstructured){
			"absent": func(o *unstructured.Unstructured) { unstructured.RemoveNestedField(o.Object, "spec", "rkeConfig") },
			"null":   func(o *unstructured.Unstructured) { o.Object["spec"].(map[string]any)["rkeConfig"] = nil },
		} {
			t.Run(tc.name+", rkeConfig "+shape, func(t *testing.T) {
				t.Parallel()
				f := newUpgradeFixture(t)
				prov := restoreProvisioning(nil)
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
				_, _, err := f.restorePlan(t, restoreInput("s1"))
				ce := asCode(t, err)
				assert.Equal(t, tc.code, ce.Code)
				assert.Equal(t, tc.retryable, ce.Retryable)
				assert.Equal(t, tc.want, ce.Message)
				assert.Empty(t, ce.Hint)
				assert.Empty(t, f.local.updates, "no dry-run")
				assert.Zero(t, f.local.requestsOf("GET "+objKey(etcdSnapshotRow.GVR, "fleet-default", "s1")), "the snapshot is not read")
			})
		}
	}
	t.Run("custom cluster with an empty rkeConfig is accepted", func(t *testing.T) {
		t.Parallel()
		f := restoreFixture(t, map[string]any{})
		_, _, err := f.restorePlan(t, restoreInput("s1"))
		require.NoError(t, err)
	})
}

// TestRestoreChecksReadinessBeforeTheSnapshot asserts a cluster that is not
// ready is refused before the etcd snapshot object is read.
func TestRestoreChecksReadinessBeforeTheSnapshot(t *testing.T) {
	t.Parallel()
	f := restoreFixture(t, map[string]any{})
	f.steve.set(managementWith("rke2", condition("Connected", "False", "agent offline")))
	_, _, err := f.restorePlan(t, restoreInput("s1"))
	ce := asCode(t, err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "cluster prod is not ready (condition Connected is False: agent offline)")
	assert.Zero(t, f.local.requestsOf("GET "+objKey(etcdSnapshotRow.GVR, "fleet-default", "s1")))
	assert.Empty(t, f.local.updates)
}

// TestRestoreSendsEveryRequestToTheLocalCluster asserts the requests of a Plan
// go to the local cluster's client in order: the provisioning Cluster, the
// RKEControlPlane, the etcd snapshot object and the one dry-run; none names a
// Secret.
func TestRestoreSendsEveryRequestToTheLocalCluster(t *testing.T) {
	t.Parallel()
	f := restoreFixture(t, map[string]any{"etcd": map[string]any{"s3": map[string]any{"cloudCredentialName": "cattle-global-data:cc-1"}}})
	snap := restorableSnapshot(t, "s1")
	snap.Object["snapshotFile"].(map[string]any)["s3"] = map[string]any{"bucket": "backups", "cloudCredentialName": "cattle-global-data:cc-1"}
	f.local.put(etcdSnapshotRow.GVR, "fleet-default", "s1", snap)
	_, exec, err := f.restorePlan(t, restoreInput("s1", "wait", "false"))
	require.NoError(t, err)
	_, err = exec(t.Context())
	require.NoError(t, err)
	require.Len(t, f.local.requests, 5)
	assert.Equal(t, []string{
		"GET clusters/fleet-default/prod",
		"GET rkecontrolplanes/fleet-default/prod",
		"GET etcdsnapshots/fleet-default/s1",
		"PUT clusters/fleet-default/prod",
		"PUT clusters/fleet-default/prod",
	}, f.local.requests[:5])
	for _, request := range f.local.requests {
		assert.NotContains(t, request, "secrets")
	}
	assert.Empty(t, f.downstream.requests)
}

// TestRestoreRecoveryNamesTheModeAsTyped asserts the recovery of a committed
// error is the restore with --dry-run, with --restore-mode only when the run
// typed one.
func TestRestoreRecoveryNamesTheModeAsTyped(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"", "none", "all"} {
		t.Run("mode "+mode, func(t *testing.T) {
			t.Parallel()
			f := restoreFixture(t, map[string]any{}, restoreControlPlane(nil, "", "", ""), restoreControlPlane(map[string]any{"name": "s1", "generation": int64(1)}, "Started", "", ""))
			in := restoreInput("s1", "timeout", "1s")
			if mode != "" {
				in = restoreInput("s1", "timeout", "1s", restoreModeFlag, mode)
			}
			_, exec, err := f.restorePlan(t, in)
			require.NoError(t, err)
			_, err = exec(t.Context())
			ce := asCode(t, err)
			assert.Equal(t, restoreRecoveryOf("s1", mode).Field(), ce.Hint)
		})
	}
}

// TestRestoreNeverShowsASecretValueOfTheSnapshotSpec asserts nothing of the
// snapshot's cluster spec but its Kubernetes version and whether it has an RKE
// configuration reaches the Effect, the diff, the result or an error, for each
// mode.
func TestRestoreNeverShowsASecretValueOfTheSnapshotSpec(t *testing.T) {
	t.Parallel()
	const marker = "MARKER-SECRET-VALUE"
	spec := `{"kubernetesVersion":"v1.31.4+rke2r1","rkeConfig":{"chartValues":{"rke2-calico":{"token":"` + marker + `"}},"registries":{"configs":{"r":{"auth":{"password":"` + marker + `"}}}},"additionalManifest":"` + marker + `"}}`
	for _, mode := range []string{"", "none", "kubernetesVersion", "all"} {
		t.Run("mode "+mode, func(t *testing.T) {
			t.Parallel()
			f := restoreFixture(t, map[string]any{})
			snap := restorableSnapshot(t, "s1")
			setSpec(spec)(t, snap)
			f.local.put(etcdSnapshotRow.GVR, "fleet-default", "s1", snap)
			in := restoreInput("s1", "wait", "false")
			if mode != "" {
				in = restoreInput("s1", "wait", "false", restoreModeFlag, mode)
			}
			eff, exec, err := f.restorePlan(t, in)
			require.NoError(t, err)
			res, err := exec(t.Context())
			require.NoError(t, err)
			shown, err := json.Marshal([]any{eff.Warnings, eff.Diff, res.Text, res.Data, res.Warnings})
			require.NoError(t, err)
			assert.NotContains(t, string(shown), marker)
		})
	}
}
