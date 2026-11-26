package complete

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	restoreClusterPath   = "/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters/prod"
	restoreSnapshotsPath = "/v1/rke.cattle.io.etcdsnapshots/fleet-default"
	restoreSnapshotPath  = "/k8s/clusters/local/apis/rke.cattle.io/v1/namespaces/fleet-default/etcdsnapshots/s1"

	restoreProvCluster = `{"metadata":{"name":"prod","creationTimestamp":"2026-01-01T00:00:00Z"},"spec":{"kubernetesVersion":"v1.31.4+rke2r1","rkeConfig":{}}}`
)

// restoreSnapshotBody is the JSON of an etcd snapshot object of cluster prod
// with the given name and snapshotFile.status, with no metadata: the checks
// accept it when the status is successful, and only the restore mode none is
// offered.
func restoreSnapshotBody(name, status string) string {
	return `{"metadata":{"name":"` + name + `"},"spec":{"clusterName":"prod"},"snapshotFile":{"status":"` + status + `","createdAt":"2026-06-01T00:00:00Z"}}`
}

// TestRestoreEtcdSnapshotSourceClaims asserts the source claims the first
// argument with one listing and the two flags with two listings, and declines
// every other site.
func TestRestoreEtcdSnapshotSourceClaims(t *testing.T) {
	t.Parallel()
	src := RestoreEtcdSnapshotSource()
	reqs, ok := src.Claim(completion.Request{Index: 0})
	require.True(t, ok)
	assert.Equal(t, completion.ManagementListing(), reqs)
	for _, flag := range []string{"snapshot", "restore-mode"} {
		reqs, ok = src.Claim(completion.Request{Flag: flag})
		require.True(t, ok, flag)
		assert.Equal(t, completion.ManagementTwoListings(), reqs, flag)
	}
	for name, req := range map[string]completion.Request{
		"second positional": {Index: 1},
		"namespace flag":    {Flag: "namespace"},
		"timeout flag":      {Flag: "timeout"},
	} {
		_, ok := src.Claim(req)
		assert.False(t, ok, name)
	}
}

// TestRestoreEtcdSnapshotSourceOffersTheClusters asserts the <cluster>
// argument is offered as the save's is: the clusters with spec.rkeConfig, from
// one request.
func TestRestoreEtcdSnapshotSourceOffersTheClusters(t *testing.T) {
	t.Parallel()
	s := completiontest.New(t, map[string]string{clustersPath: `{"data":[
		{"metadata":{"name":"provisioned"},"spec":{"rkeConfig":{"machinePools":[{"name":"p"}]}}},
		{"metadata":{"name":"imported"},"spec":{}}
	]}`})
	conn := s.Conn(1)
	got := RestoreEtcdSnapshotSource().Complete(t.Context(), conn, completion.Request{Index: 0})
	assert.Equal(t, []string{"provisioned"}, got.Names)
	assert.Equal(t, 1, conn.Used())
}

// TestRestoreEtcdSnapshotSourceOffersSnapshots asserts --snapshot offers the
// etcd snapshot objects the checks accept, from two requests: the provisioning
// Cluster and one Steve page, and with the restore mode typed on the line.
func TestRestoreEtcdSnapshotSourceOffersSnapshots(t *testing.T) {
	t.Parallel()
	page := `{"data":[` + restoreSnapshotBody("s1", "successful") + `,` + restoreSnapshotBody("s2", "failed") + `,` +
		`{"metadata":{"name":"s3"},"spec":{"clusterName":"other"},"snapshotFile":{"status":"successful","createdAt":"2026-06-01T00:00:00Z"}}]}`
	s := completiontest.New(t, map[string]string{restoreClusterPath: restoreProvCluster, restoreSnapshotsPath: page})
	conn := s.Conn(2)
	got := RestoreEtcdSnapshotSource().Complete(t.Context(), conn, completion.Request{Flag: "snapshot", Positionals: []string{"prod"}})
	assert.Equal(t, []string{"s1"}, got.Names)
	assert.Equal(t, 2, conn.Used())
	assert.Equal(t, []string{restoreClusterPath, restoreSnapshotsPath}, s.Requested())

	got = RestoreEtcdSnapshotSource().Complete(t.Context(), s.Conn(2), completion.Request{Flag: "snapshot", Positionals: []string{"prod"}, Tail: []string{"--restore-mode", "all"}})
	assert.Empty(t, got.Names, "a snapshot with no metadata does not offer the mode all")
}

// TestRestoreEtcdSnapshotSourceOffersTheModesOfASnapshot asserts
// --restore-mode offers the modes the checks accept for the snapshot typed on
// the line, from two requests: the provisioning Cluster and the etcd snapshot
// object.
func TestRestoreEtcdSnapshotSourceOffersTheModesOfASnapshot(t *testing.T) {
	t.Parallel()
	s := completiontest.New(t, map[string]string{restoreClusterPath: restoreProvCluster, restoreSnapshotPath: restoreSnapshotBody("s1", "successful")})
	conn := s.Conn(2)
	got := RestoreEtcdSnapshotSource().Complete(t.Context(), conn, completion.Request{Flag: "restore-mode", Positionals: []string{"prod"}, Tail: []string{"--snapshot", "s1"}})
	assert.Equal(t, []string{"none"}, got.Names)
	assert.Equal(t, 2, conn.Used())
	assert.Equal(t, []string{restoreClusterPath, restoreSnapshotPath}, s.Requested())
}

// TestRestoreEtcdSnapshotSourceOffersNothing asserts each flag offers nothing
// when the line lacks the cluster or the snapshot, the cluster has no
// spec.rkeConfig, a read is refused, fails or finds nothing, and the snapshot
// is one the checks refuse.
func TestRestoreEtcdSnapshotSourceOffersNothing(t *testing.T) {
	t.Parallel()
	noRKE := `{"metadata":{"name":"prod"},"spec":{}}`
	nullRKE := `{"metadata":{"name":"prod"},"spec":{"rkeConfig":null}}`
	page := `{"data":[` + restoreSnapshotBody("s1", "successful") + `]}`
	rows := []struct {
		name   string
		server *completiontest.Server
		req    completion.Request
	}{
		{"snapshot, no cluster typed", completiontest.New(t, map[string]string{restoreClusterPath: restoreProvCluster, restoreSnapshotsPath: page}), completion.Request{Flag: "snapshot"}},
		{"snapshot, no rkeConfig", completiontest.New(t, map[string]string{restoreClusterPath: noRKE, restoreSnapshotsPath: page}), completion.Request{Flag: "snapshot", Positionals: []string{"prod"}}},
		{"snapshot, null rkeConfig", completiontest.New(t, map[string]string{restoreClusterPath: nullRKE, restoreSnapshotsPath: page}), completion.Request{Flag: "snapshot", Positionals: []string{"prod"}}},
		{"snapshot, cluster not found", completiontest.New(t, map[string]string{restoreSnapshotsPath: page}), completion.Request{Flag: "snapshot", Positionals: []string{"prod"}}},
		{"snapshot, cluster read refused", completiontest.NewStatus(t, 403), completion.Request{Flag: "snapshot", Positionals: []string{"prod"}}},
		{"snapshot, cluster read failed", completiontest.NewStatus(t, 500), completion.Request{Flag: "snapshot", Positionals: []string{"prod"}}},
		{"snapshot, page not found", completiontest.New(t, map[string]string{restoreClusterPath: restoreProvCluster}), completion.Request{Flag: "snapshot", Positionals: []string{"prod"}}},
		{"restore mode, no snapshot typed", completiontest.New(t, map[string]string{restoreClusterPath: restoreProvCluster, restoreSnapshotPath: restoreSnapshotBody("s1", "successful")}), completion.Request{Flag: "restore-mode", Positionals: []string{"prod"}}},
		{"restore mode, no cluster typed", completiontest.New(t, map[string]string{restoreClusterPath: restoreProvCluster, restoreSnapshotPath: restoreSnapshotBody("s1", "successful")}), completion.Request{Flag: "restore-mode", Tail: []string{"--snapshot", "s1"}}},
		{"restore mode, no rkeConfig", completiontest.New(t, map[string]string{restoreClusterPath: noRKE, restoreSnapshotPath: restoreSnapshotBody("s1", "successful")}), completion.Request{Flag: "restore-mode", Positionals: []string{"prod"}, Tail: []string{"--snapshot", "s1"}}},
		{"restore mode, snapshot read refused", completiontest.NewStatus(t, 403), completion.Request{Flag: "restore-mode", Positionals: []string{"prod"}, Tail: []string{"--snapshot", "s1"}}},
		{"restore mode, snapshot not found", completiontest.New(t, map[string]string{restoreClusterPath: restoreProvCluster}), completion.Request{Flag: "restore-mode", Positionals: []string{"prod"}, Tail: []string{"--snapshot", "s1"}}},
		{"restore mode, failed snapshot", completiontest.New(t, map[string]string{restoreClusterPath: restoreProvCluster, restoreSnapshotPath: restoreSnapshotBody("s1", "failed")}), completion.Request{Flag: "restore-mode", Positionals: []string{"prod"}, Tail: []string{"--snapshot", "s1"}}},
	}
	for _, tc := range rows {
		got := RestoreEtcdSnapshotSource().Complete(t.Context(), tc.server.Conn(2), tc.req)
		assert.Empty(t, got.Names, tc.name)
	}
}

// TestRestoreEtcdSnapshotSourceDoesNotFollowAContinuation asserts a Steve page
// with a continuation token offers its own snapshots and reads no second
// page.
func TestRestoreEtcdSnapshotSourceDoesNotFollowAContinuation(t *testing.T) {
	t.Parallel()
	page := `{"pagination":{"next":"` + restoreSnapshotsPath + `?page=2"},"data":[` + restoreSnapshotBody("s1", "successful") + `]}`
	s := completiontest.New(t, map[string]string{restoreClusterPath: restoreProvCluster, restoreSnapshotsPath: page})
	got := RestoreEtcdSnapshotSource().Complete(t.Context(), s.Conn(2), completion.Request{Flag: "snapshot", Positionals: []string{"prod"}})
	assert.Equal(t, []string{"s1"}, got.Names)
	assert.Equal(t, []string{restoreClusterPath, restoreSnapshotsPath}, s.Requested())
}
