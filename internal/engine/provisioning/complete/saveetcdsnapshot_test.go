package complete

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	clustersPath     = "/v1/provisioning.cattle.io.clusters/fleet-default"
	teamClustersPath = "/v1/provisioning.cattle.io.clusters/team-a"

	kubernetesClustersPath = "/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters"
)

// TestSaveEtcdSnapshotSourceClaims asserts the source claims only the first
// argument, with the requirement of a management token and one listing, and
// declines every flag value and later argument.
func TestSaveEtcdSnapshotSourceClaims(t *testing.T) {
	t.Parallel()
	src := SaveEtcdSnapshotSource()
	reqs, ok := src.Claim(completion.Request{Index: 0})
	require.True(t, ok)
	assert.Equal(t, completion.ManagementListing(), reqs)
	for name, req := range map[string]completion.Request{
		"second positional": {Index: 1},
		"namespace flag":    {Flag: "namespace"},
		"timeout flag":      {Flag: "timeout"},
	} {
		_, ok := src.Claim(req)
		assert.False(t, ok, name)
	}
}

// TestSaveEtcdSnapshotSourceOffersClustersWithRKEConfig asserts a provisioned
// cluster, a custom cluster with an empty spec.rkeConfig, and no imported
// cluster (spec.rkeConfig absent or null) is offered, from one request, in the
// default namespace and in the one typed on the line.
func TestSaveEtcdSnapshotSourceOffersClustersWithRKEConfig(t *testing.T) {
	t.Parallel()
	body := `{"data":[
		{"metadata":{"name":"provisioned"},"spec":{"rkeConfig":{"machinePools":[{"name":"p"}]}}},
		{"metadata":{"name":"custom"},"spec":{"rkeConfig":{}}},
		{"metadata":{"name":"imported-absent"},"spec":{}},
		{"metadata":{"name":"imported-null"},"spec":{"rkeConfig":null}},
		{"metadata":{"name":"no-spec"}},
		{"spec":{"rkeConfig":{}}}
	]}`
	s := completiontest.New(t, map[string]string{
		clustersPath:     body,
		teamClustersPath: `{"data":[{"metadata":{"name":"team-custom"},"spec":{"rkeConfig":{}}}]}`,
	})
	src := SaveEtcdSnapshotSource()
	conn := s.Conn(1)
	got := src.Complete(t.Context(), conn, completion.Request{Index: 0})
	assert.Equal(t, []string{"provisioned", "custom"}, got.Names)
	assert.Equal(t, 1, conn.Used())
	assert.Equal(t, []string{clustersPath}, s.Requested())

	got = src.Complete(t.Context(), s.Conn(1), completion.Request{Index: 0, Tail: []string{"--namespace", "team-a"}})
	assert.Equal(t, []string{"team-custom"}, got.Names)
	assert.Equal(t, []string{clustersPath, teamClustersPath}, s.Requested())
}

// TestSaveEtcdSnapshotSourceOffersNothingOnAnError asserts the source offers
// nothing when the listing is refused or fails, and when the server serves the
// clusters only through the Kubernetes API: the source reads Steve.
func TestSaveEtcdSnapshotSourceOffersNothingOnAnError(t *testing.T) {
	t.Parallel()
	for name, s := range map[string]*completiontest.Server{
		"not found": completiontest.New(t, nil),
		"forbidden": completiontest.NewStatus(t, 403),
		"failed":    completiontest.NewStatus(t, 500),
		"Kubernetes API only": completiontest.New(t, map[string]string{
			kubernetesClustersPath: `{"items":[{"metadata":{"name":"prod"},"spec":{"rkeConfig":{}}}]}`,
		}),
	} {
		got := SaveEtcdSnapshotSource().Complete(t.Context(), s.Conn(1), completion.Request{Index: 0})
		assert.Empty(t, got.Names, name)
	}
}
