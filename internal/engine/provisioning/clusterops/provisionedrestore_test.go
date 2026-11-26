package clusterops

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	testClusterCreated = "2026-01-01T00:00:00Z"
	testSnapshotTaken  = "2026-06-01T00:00:00Z"
)

// restoreCandidate is a snapshot CheckProvisionedRestore accepts for cluster
// "prod": its own, successful, taken after the cluster, with a Kubernetes
// version at 1.25 or newer and an RKE configuration in its metadata.
func restoreCandidate(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{
		"metadata": map[string]any{"name": "snap", "labels": map[string]any{clusterNameLabel: "prod"}},
		"spec":     map[string]any{"clusterName": "prod"},
		"snapshotFile": map[string]any{
			"status":    "successful",
			"createdAt": testSnapshotTaken,
			"metadata":  metadataWithSpec(t, `{"kubernetesVersion":"v1.30.5+rke2r1","rkeConfig":{}}`),
		},
	}
}

func restoreCluster(version string) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": "prod", "creationTimestamp": testClusterCreated},
		"spec":     map[string]any{"kubernetesVersion": version, "rkeConfig": map[string]any{}},
	}
}

// TestCheckProvisionedRestoreBaseline asserts a restorable snapshot is
// accepted, with all three modes offered and all the default.
func TestCheckProvisionedRestoreBaseline(t *testing.T) {
	t.Parallel()
	got := CheckProvisionedRestore(restoreCandidate(t), restoreCluster("v1.30.5+rke2r1"), "")
	assert.Equal(t, ProvisionedRestoreAccepted, got.Problem)
	assert.Equal(t, []string{RestoreModeNone, RestoreModeKubernetesVersion, RestoreModeAll}, got.Modes)
	assert.Equal(t, RestoreModeAll, got.Mode)
	assert.Equal(t, SnapshotSpec{KubernetesVersion: "v1.30.5+rke2r1", HasRKEConfig: true}, got.Spec)
}

// TestCheckProvisionedRestoreProblems asserts the one problem each change to
// the baseline gives.
func TestCheckProvisionedRestoreProblems(t *testing.T) {
	t.Parallel()
	set := func(snapshot map[string]any, value any, path ...string) map[string]any {
		m := snapshot
		for _, key := range path[:len(path)-1] {
			m = m[key].(map[string]any)
		}
		if value == nil {
			delete(m, path[len(path)-1])
		} else {
			m[path[len(path)-1]] = value
		}
		return snapshot
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any) map[string]any
		mode   string
		want   ProvisionedRestoreProblem
	}{
		{"no cluster name", func(s map[string]any) map[string]any { return set(s, "", "spec", "clusterName") }, "", ProvisionedRestoreNoClusterName},
		{"another cluster", func(s map[string]any) map[string]any { return set(s, "other", "spec", "clusterName") }, "", ProvisionedRestoreOtherCluster},
		{"label another cluster", func(s map[string]any) map[string]any { return set(s, "other", "metadata", "labels", clusterNameLabel) }, "", ProvisionedRestoreOtherLabel},
		{"label empty", func(s map[string]any) map[string]any { return set(s, "", "metadata", "labels", clusterNameLabel) }, "", ProvisionedRestoreOtherLabel},
		{"label absent", func(s map[string]any) map[string]any { return set(s, nil, "metadata", "labels", clusterNameLabel) }, "", ProvisionedRestoreAccepted},
		{"status failed", func(s map[string]any) map[string]any { return set(s, "failed", "snapshotFile", "status") }, "", ProvisionedRestoreNotSuccessful},
		{"status empty", func(s map[string]any) map[string]any { return set(s, nil, "snapshotFile", "status") }, "", ProvisionedRestoreNotSuccessful},
		{"no createdAt", func(s map[string]any) map[string]any { return set(s, nil, "snapshotFile", "createdAt") }, "", ProvisionedRestoreNoCreatedAt},
		{"before the cluster", func(s map[string]any) map[string]any {
			return set(s, "2025-01-01T00:00:00Z", "snapshotFile", "createdAt")
		}, "", ProvisionedRestoreBeforeCluster},
		{"at the cluster creation", func(s map[string]any) map[string]any { return set(s, testClusterCreated, "snapshotFile", "createdAt") }, "", ProvisionedRestoreBeforeCluster},
		{"another cluster and failed", func(s map[string]any) map[string]any {
			return set(set(s, "other", "spec", "clusterName"), "failed", "snapshotFile", "status")
		}, "", ProvisionedRestoreOtherCluster},
		{"label another cluster and before", func(s map[string]any) map[string]any {
			return set(set(s, "other", "metadata", "labels", clusterNameLabel), "2025-01-01T00:00:00Z", "snapshotFile", "createdAt")
		}, "", ProvisionedRestoreOtherLabel},
		{"failed and before", func(s map[string]any) map[string]any {
			return set(set(s, "failed", "snapshotFile", "status"), "2025-01-01T00:00:00Z", "snapshotFile", "createdAt")
		}, "", ProvisionedRestoreNotSuccessful},
		{"malformed version", func(s map[string]any) map[string]any {
			return set(s, metadataWithSpec(t, `{"kubernetesVersion":"1.2.3.4","rkeConfig":{}}`), "snapshotFile", "metadata")
		}, "none", ProvisionedRestoreBadVersion},
		{"empty version", func(s map[string]any) map[string]any {
			return set(s, metadataWithSpec(t, `{"rkeConfig":{}}`), "snapshotFile", "metadata")
		}, "all", ProvisionedRestoreBadVersion},
		{"mode not offered", func(s map[string]any) map[string]any {
			return set(s, metadataWithSpec(t, `{"kubernetesVersion":"v1.30.5"}`), "snapshotFile", "metadata")
		}, "all", ProvisionedRestoreModeNotOffered},
		{"below 1.25 with none", func(s map[string]any) map[string]any {
			return set(s, metadataWithSpec(t, `{"kubernetesVersion":"v1.24.9+rke2r1","rkeConfig":{}}`), "snapshotFile", "metadata")
		}, "none", ProvisionedRestoreBelow125},
		{"below 1.25 with all", func(s map[string]any) map[string]any {
			return set(s, metadataWithSpec(t, `{"kubernetesVersion":"v1.24.9+rke2r1","rkeConfig":{}}`), "snapshotFile", "metadata")
		}, "all", ProvisionedRestoreAccepted},
	} {
		got := CheckProvisionedRestore(tc.mutate(restoreCandidate(t)), restoreCluster("v1.30.5+rke2r1"), tc.mode)
		assert.Equal(t, tc.want, got.Problem, tc.name)
	}
}

// TestCheckProvisionedRestoreModes asserts the modes offered and the mode to
// write for each state of the metadata and the annotation.
func TestCheckProvisionedRestoreModes(t *testing.T) {
	t.Parallel()
	annotate := func(s map[string]any, value string) map[string]any {
		s["metadata"].(map[string]any)["annotations"] = map[string]any{restoreModeOptionsAnnotation: value}
		return s
	}
	withMetadata := func(s map[string]any, metadata string) map[string]any {
		s["snapshotFile"].(map[string]any)["metadata"] = metadata
		return s
	}
	none, kube, all := RestoreModeNone, RestoreModeKubernetesVersion, RestoreModeAll
	for _, tc := range []struct {
		name     string
		snapshot map[string]any
		cluster  string
		typed    string
		modes    []string
		mode     string
		problem  ProvisionedRestoreProblem
	}{
		{"annotation absent", restoreCandidate(t), "v1.30.5", "", []string{none, kube, all}, all, ProvisionedRestoreAccepted},
		{"annotation none", annotate(restoreCandidate(t), "none"), "v1.30.5", "", []string{none}, none, ProvisionedRestoreAccepted},
		{"annotation none and kubernetesVersion", annotate(restoreCandidate(t), "none,kubernetesVersion"), "v1.30.5", "", []string{none, kube}, kube, ProvisionedRestoreAccepted},
		{"annotation without none", annotate(restoreCandidate(t), "all"), "v1.30.5", "", []string{none, all}, all, ProvisionedRestoreAccepted},
		{"no rkeConfig in the metadata", withMetadata(restoreCandidate(t), metadataWithSpec(t, `{"kubernetesVersion":"v1.30.5"}`)), "v1.30.5", "", []string{none, kube}, kube, ProvisionedRestoreAccepted},
		{"no rkeConfig and annotation all", annotate(withMetadata(restoreCandidate(t), metadataWithSpec(t, `{"kubernetesVersion":"v1.30.5"}`)), "all"), "v1.30.5", "", []string{none}, none, ProvisionedRestoreAccepted},
		{"metadata absent", withMetadata(restoreCandidate(t), ""), "v1.30.5", "", []string{none}, none, ProvisionedRestoreAccepted},
		{"metadata absent with kubernetesVersion typed", withMetadata(restoreCandidate(t), ""), "v1.30.5", kube, []string{none}, "", ProvisionedRestoreModeNotOffered},
		{"metadata unreadable below 1.25 cluster at 1.30", withMetadata(restoreCandidate(t), "!!"), "v1.30.5", "none", []string{none}, none, ProvisionedRestoreAccepted},
		{"cluster version does not parse", withMetadata(restoreCandidate(t), metadataWithSpec(t, `{"kubernetesVersion":"v1.24.1","rkeConfig":{}}`)), "x", "none", []string{none, kube, all}, none, ProvisionedRestoreAccepted},
		{"cluster below 1.25 too", withMetadata(restoreCandidate(t), metadataWithSpec(t, `{"kubernetesVersion":"v1.24.1","rkeConfig":{}}`)), "v1.24.9", "none", []string{none, kube, all}, none, ProvisionedRestoreAccepted},
		{"typed kubernetesVersion", restoreCandidate(t), "v1.30.5", kube, []string{none, kube, all}, kube, ProvisionedRestoreAccepted},
		{"typed none", restoreCandidate(t), "v1.30.5", none, []string{none, kube, all}, none, ProvisionedRestoreAccepted},
		{"typed value outside the three", restoreCandidate(t), "v1.30.5", "bogus", []string{none, kube, all}, "", ProvisionedRestoreModeNotOffered},
	} {
		got := CheckProvisionedRestore(tc.snapshot, restoreCluster(tc.cluster), tc.typed)
		assert.Equal(t, tc.problem, got.Problem, tc.name)
		assert.Equal(t, tc.modes, got.Modes, tc.name)
		assert.Equal(t, tc.mode, got.Mode, tc.name)
	}
}

// TestCheckProvisionedRestoreTooLarge asserts that with a spec too large to read
// the modes follow the annotation alone, the check for a snapshot taken before
// Kubernetes 1.25 is not made, and the check reports that.
func TestCheckProvisionedRestoreTooLarge(t *testing.T) {
	t.Parallel()
	snapshot := restoreCandidate(t)
	snapshot["snapshotFile"].(map[string]any)["metadata"] = metadataOf(t, map[string]any{snapshotSpecKey: gzipBase64(t, specOfSize(16<<20+1))})
	got := CheckProvisionedRestore(snapshot, restoreCluster("v1.30.5"), "none")
	assert.Equal(t, ProvisionedRestoreAccepted, got.Problem)
	assert.Equal(t, SnapshotSpecTooLarge, got.Spec.State)
	assert.Equal(t, []string{RestoreModeNone, RestoreModeKubernetesVersion, RestoreModeAll}, got.Modes)
	snapshot["metadata"].(map[string]any)["annotations"] = map[string]any{restoreModeOptionsAnnotation: "none"}
	got = CheckProvisionedRestore(snapshot, restoreCluster("v1.30.5"), "all")
	assert.Equal(t, ProvisionedRestoreModeNotOffered, got.Problem)
}
