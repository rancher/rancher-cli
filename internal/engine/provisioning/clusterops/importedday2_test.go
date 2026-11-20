package clusterops

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestCommandDistribution asserts the distribution Rancher runs commands for
// is rke2 only when status.provider is rke2, and k3s for an empty or any other
// value, whatever status.driver says.
func TestCommandDistribution(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ provider, want string }{
		{"", "k3s"}, {"rke2", "rke2"}, {"k3s", "k3s"}, {"imported", "k3s"}, {"RKE2", "k3s"},
	} {
		status := map[string]any{"driver": "rke2"}
		if tc.provider != "" {
			status["provider"] = tc.provider
		}
		assert.Equal(t, tc.want, CommandDistribution(map[string]any{"status": status}), "provider %q", tc.provider)
	}
	assert.Equal(t, "k3s", CommandDistribution(map[string]any{}), "no status at all")
}

// TestCertificateServices asserts the services of a certificate rotation, in
// the Rancher UI's order: the services of every cluster, the distribution's
// controller and server, then supervisor; any value but k3s is read as rke2.
func TestCertificateServices(t *testing.T) {
	t.Parallel()
	common := []string{"admin", "api-server", "controller-manager", "scheduler", "cloud-controller", "etcd", "auth-proxy", "kubelet", "kube-proxy"}
	assert.Equal(t, append(append([]string{}, common...), "k3s-controller", "k3s-server", "supervisor"), CertificateServices("k3s"))
	assert.Equal(t, append(append([]string{}, common...), "rke2-controller", "rke2-server", "supervisor"), CertificateServices("rke2"))
	assert.Equal(t, CertificateServices("rke2"), CertificateServices(""))
}

// TestFirstUnknownService asserts the first service the distribution does not
// have is returned, in the order given, and that no list and an empty list name
// none.
func TestFirstUnknownService(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		distro   string
		services []string
		want     string
		found    bool
	}{
		{"empty list rotates every service", "k3s", []string{}, "", false},
		{"nil list", "rke2", nil, "", false},
		{"every service of k3s", "k3s", CertificateServices("k3s"), "", false},
		{"supervisor on k3s", "k3s", []string{"supervisor"}, "", false},
		{"rke2 server on k3s", "k3s", []string{"admin", "rke2-server"}, "rke2-server", true},
		{"k3s server on rke2", "rke2", []string{"k3s-server", "nosuch"}, "k3s-server", true},
		{"unknown after a known one", "rke2", []string{"etcd", "nosuch", "other"}, "nosuch", true},
	} {
		got, found := FirstUnknownService(tc.distro, tc.services)
		assert.Equal(t, tc.want, got, tc.name)
		assert.Equal(t, tc.found, found, tc.name)
	}
}

func clusterWithVersionManagement(annotation string, set bool) map[string]any {
	annotations := map[string]any{}
	if set {
		annotations["rancher.io/imported-cluster-version-management"] = annotation
	}
	return map[string]any{"metadata": map[string]any{"name": "c-m-x", "annotations": annotations}}
}

// TestVersionManagementOn asserts the annotation "true" turns version
// management on whatever the Setting says, "false" and any other value turn it
// off, and the Setting decides only for an absent, empty or system-default
// annotation.
func TestVersionManagementOn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		annotation string
		set        bool
		setting    string
		want       bool
	}{
		{"true, Setting false", "true", true, "false", true},
		{"false, Setting true", "false", true, "true", false},
		{"other value, Setting true", "maybe", true, "true", false},
		{"absent, Setting true", "", false, "true", true},
		{"absent, Setting false", "", false, "false", false},
		{"empty, Setting true", "", true, "true", true},
		{"system-default, Setting true", "system-default", true, "true", true},
		{"system-default, Setting false", "system-default", true, "false", false},
		{"system-default, no Setting value", "system-default", true, "", false},
	} {
		assert.Equal(t, tc.want, VersionManagementOn(clusterWithVersionManagement(tc.annotation, tc.set), tc.setting), tc.name)
	}
}

func snapshotWithOptions(options string) map[string]any {
	annotations := map[string]any{}
	if options != "" {
		annotations["etcdsnapshot.rke.io/restore-mode-options"] = options
	}
	return map[string]any{"metadata": map[string]any{"annotations": annotations}}
}

// TestRestoreModes asserts the modes offered: none always; the other two only
// with version management; filtered by the snapshot's annotation when it lists
// any (spaces and empty entries ignored); none when nothing is left.
func TestRestoreModes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		vm      bool
		options string
		want    []string
	}{
		{"version management off, no annotation", false, "", []string{"none"}},
		{"version management on, no annotation", true, "", []string{"none", "kubernetesVersion", "all"}},
		{"version management off, annotation lists all", false, "none,all", []string{"none"}},
		{"version management on, annotation lists two", true, "none,all", []string{"none", "all"}},
		{"version management on, annotation lists one", true, "kubernetesVersion", []string{"kubernetesVersion"}},
		{"spaces and empty entries", true, " all , ,none,", []string{"none", "all"}},
		{"annotation lists only a mode that is off", false, "all", []string{"none"}},
		{"annotation lists an unknown mode", true, "bogus", []string{"none"}},
		{"annotation of only commas lists none", true, ",,", []string{"none", "kubernetesVersion", "all"}},
	} {
		assert.Equal(t, tc.want, RestoreModes(tc.vm, snapshotWithOptions(tc.options)), tc.name)
	}
}

// TestDefaultRestoreMode asserts the default is the most complete mode offered.
func TestDefaultRestoreMode(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "all", DefaultRestoreMode([]string{"none", "kubernetesVersion", "all"}))
	assert.Equal(t, "kubernetesVersion", DefaultRestoreMode([]string{"none", "kubernetesVersion"}))
	assert.Equal(t, "none", DefaultRestoreMode([]string{"none"}))
	assert.Equal(t, "none", DefaultRestoreMode(nil))
}

func restorableSnapshot(cluster, status, createdAt string) map[string]any {
	snapshotFile := map[string]any{"status": status}
	if createdAt != "" {
		snapshotFile["createdAt"] = createdAt
	}
	return map[string]any{
		"metadata":     map[string]any{"name": "snap"},
		"spec":         map[string]any{"clusterName": cluster},
		"snapshotFile": snapshotFile,
	}
}

func clusterCreatedAt(ts string) map[string]any {
	meta := map[string]any{"name": "c-m-x"}
	if ts != "" {
		meta["creationTimestamp"] = ts
	}
	return map[string]any{"metadata": meta}
}

// TestCheckRestorableSnapshot asserts each reason a snapshot is not restored to
// the cluster, in the order of the rule, and that a snapshot taken at the
// cluster's creation time is refused.
func TestCheckRestorableSnapshot(t *testing.T) {
	t.Parallel()
	const created = "2026-11-01T10:00:00Z"
	for _, tc := range []struct {
		name     string
		snapshot map[string]any
		cluster  map[string]any
		want     SnapshotProblem
	}{
		{"restorable", restorableSnapshot("c-m-x", "successful", "2026-11-02T10:00:00Z"), clusterCreatedAt(created), SnapshotRestorable},
		{"no cluster name", restorableSnapshot("", "successful", "2026-11-02T10:00:00Z"), clusterCreatedAt(created), SnapshotNoClusterName},
		{"another cluster", restorableSnapshot("c-m-y", "successful", "2026-11-02T10:00:00Z"), clusterCreatedAt(created), SnapshotOtherCluster},
		{"failed", restorableSnapshot("c-m-x", "failed", "2026-11-02T10:00:00Z"), clusterCreatedAt(created), SnapshotNotSuccessful},
		{"no status", restorableSnapshot("c-m-x", "", "2026-11-02T10:00:00Z"), clusterCreatedAt(created), SnapshotNotSuccessful},
		{"no createdAt", restorableSnapshot("c-m-x", "successful", ""), clusterCreatedAt(created), SnapshotNoCreatedAt},
		{"unparseable createdAt", restorableSnapshot("c-m-x", "successful", "yesterday"), clusterCreatedAt(created), SnapshotNoCreatedAt},
		{"createdAt equals the creation time", restorableSnapshot("c-m-x", "successful", created), clusterCreatedAt(created), SnapshotBeforeImport},
		{"createdAt before the creation time", restorableSnapshot("c-m-x", "successful", "2026-10-31T10:00:00Z"), clusterCreatedAt(created), SnapshotBeforeImport},
		{"one second after the creation time", restorableSnapshot("c-m-x", "successful", "2026-11-01T10:00:01Z"), clusterCreatedAt(created), SnapshotRestorable},
		{"no creation time leaves the comparison out", restorableSnapshot("c-m-x", "successful", "2026-10-31T10:00:00Z"), clusterCreatedAt(""), SnapshotRestorable},
		{"unparseable creation time leaves the comparison out", restorableSnapshot("c-m-x", "successful", "2026-10-31T10:00:00Z"), clusterCreatedAt("soon"), SnapshotRestorable},
	} {
		assert.Equal(t, tc.want, CheckRestorableSnapshot(tc.snapshot, tc.cluster, "c-m-x"), tc.name)
	}
}

// TestNewestSnapshotAge asserts the age is of the newest snapshot of the
// cluster of any status, in whole days, 23h59m is 0 days, 24h and 47h are 1, 48h
// is 2, and a snapshot of another cluster does not count.
func TestNewestSnapshotAge(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	for _, tc := range []struct {
		name  string
		items []map[string]any
		want  SnapshotAge
	}{
		{"none", nil, SnapshotAge{}},
		{"only another cluster", []map[string]any{restorableSnapshot("c-m-y", "successful", ago(time.Hour))}, SnapshotAge{}},
		{"23h59m", []map[string]any{restorableSnapshot("c-m-x", "successful", ago(23*time.Hour+59*time.Minute))}, SnapshotAge{Count: 1, Days: 0, Dated: true}},
		{"24h", []map[string]any{restorableSnapshot("c-m-x", "successful", ago(24*time.Hour))}, SnapshotAge{Count: 1, Days: 1, Dated: true}},
		{"47h", []map[string]any{restorableSnapshot("c-m-x", "successful", ago(47*time.Hour))}, SnapshotAge{Count: 1, Days: 1, Dated: true}},
		{"48h", []map[string]any{restorableSnapshot("c-m-x", "successful", ago(48*time.Hour))}, SnapshotAge{Count: 1, Days: 2, Dated: true}},
		{"a failed snapshot counts", []map[string]any{restorableSnapshot("c-m-x", "failed", ago(72*time.Hour))}, SnapshotAge{Count: 1, Days: 3, Dated: true}},
		{"the newest of several", []map[string]any{
			restorableSnapshot("c-m-x", "successful", ago(100*time.Hour)),
			restorableSnapshot("c-m-x", "successful", ago(30*time.Hour)),
			restorableSnapshot("c-m-y", "successful", ago(time.Hour)),
		}, SnapshotAge{Count: 2, Days: 1, Dated: true}},
		{"a time in the future", []map[string]any{restorableSnapshot("c-m-x", "successful", now.Add(time.Hour).Format(time.RFC3339))}, SnapshotAge{Count: 1, Days: 0, Dated: true}},
		{"no time", []map[string]any{restorableSnapshot("c-m-x", "successful", "")}, SnapshotAge{Count: 1}},
	} {
		assert.Equal(t, tc.want, NewestSnapshotAge(tc.items, "c-m-x", now), tc.name)
	}
}
