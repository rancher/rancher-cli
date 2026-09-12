package complete

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	machineConfigsPath = "/k8s/clusters/local/apis/rke-machine-config.cattle.io/v1/namespaces/fleet-default/amazonec2configs"
	cloudCredsPath     = "/k8s/clusters/local/api/v1/namespaces/cattle-global-data/secrets"
	prodClusterPath    = "/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters/prod"
	teamClusterPath    = "/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/team-a/clusters/prod"
)

// TestCreateClusterSourceClaims asserts the two flag values create
// cluster's source owns: --cloud-credential always; --machine-config only
// once the typed value carries a `<kind>/` prefix naming a machine-config
// kind (the bare prefixes are the surface's static projection); every
// other site of the command is declined.
func TestCreateClusterSourceClaims(t *testing.T) {
	t.Parallel()
	src := CreateClusterSource()
	tests := []struct {
		name string
		req  completion.Request
		ok   bool
	}{
		{name: "cloud credential", req: completion.Request{Flag: "cloud-credential"}, ok: true},
		{name: "machine config kind prefix", req: completion.Request{Flag: "machine-config", Partial: "amazonec2config/"}, ok: true},
		{name: "machine config kind prefix and a partial name", req: completion.Request{Flag: "machine-config", Partial: "amazonec2config/cf"}, ok: true},
		{name: "machine config kind alias prefix", req: completion.Request{Flag: "machine-config", Partial: "amazonec2configs/"}, ok: true},
		{name: "machine config without a slash", req: completion.Request{Flag: "machine-config", Partial: "amazonec2"}},
		{name: "machine config unknown kind", req: completion.Request{Flag: "machine-config", Partial: "nonesuch/x"}},
		{name: "machine config kind outside the group", req: completion.Request{Flag: "machine-config", Partial: "pod/x"}},
		{name: "another flag", req: completion.Request{Flag: "k8s-version"}},
		{name: "the name positional", req: completion.Request{Index: 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reqs, ok := src.Claim(tt.req)
			assert.Equal(t, tt.ok, ok)
			if ok {
				assert.Equal(t, completion.ManagementListing(), reqs)
			}
		})
	}
}

// TestCompleteMachineConfigs asserts the live half of --machine-config:
// the typed kind's objects listed in its default workspace namespace, the
// names raw, the kind's `<kind>/` prefix answered for the surface to
// recompose, an alias prefix answered as the kind's name; an unknown kind
// answers nothing without a request.
func TestCompleteMachineConfigs(t *testing.T) {
	t.Parallel()
	s := completiontest.New(t, map[string]string{machineConfigsPath: `{"items":[{"metadata":{"name":"cfg-b"}},{"metadata":{"name":"cfg-a"}},{"metadata":{"name":"evil;rm -rf"}}]}`})
	src := CreateClusterSource()
	got := src.Complete(t.Context(), s.Conn(1), completion.Request{Flag: "machine-config", Partial: "amazonec2config/cf"})
	assert.Equal(t, completion.Result{Names: []string{"cfg-b", "cfg-a", "evil;rm -rf"}, Prefix: "amazonec2config/"}, got)
	assert.Equal(t, []string{machineConfigsPath}, s.Requested())

	got = src.Complete(t.Context(), s.Conn(1), completion.Request{Flag: "machine-config", Partial: "amazonec2configs/"})
	assert.Equal(t, "amazonec2config/", got.Prefix, "an alias prefix is answered as the kind's name, the spelling the command documents")

	conn := s.Conn(1)
	got = src.Complete(t.Context(), conn, completion.Request{Flag: "machine-config", Partial: "nonesuch/x"})
	assert.Empty(t, got.Names)
	assert.Empty(t, got.Prefix)
	assert.Zero(t, conn.Used())
}

// TestCompleteCloudCredentials asserts the --cloud-credential projection:
// every credential-shaped Secret's id, in list order, followed by its
// display name when that name is on exactly one credential and is not
// itself an id; a Secret that is no credential is left out; names are
// answered raw for the surface to sanitize.
func TestCompleteCloudCredentials(t *testing.T) {
	t.Parallel()
	s := completiontest.New(t, map[string]string{cloudCredsPath: completiontest.CloudCredentialsBody})
	got := CreateClusterSource().Complete(t.Context(), s.Conn(1), completion.Request{Flag: "cloud-credential"})
	assert.Equal(t, []string{"cc-abc12", "aws-prod", "cc-dup1", "cc-dup2", "cc-collide", "cc-hostile", "bad name!", "cc-noannotation", "marker-only"}, got.Names)
	assert.Empty(t, got.Prefix)
	assert.Equal(t, []string{cloudCredsPath}, s.Requested())
}

// TestCompleteCreateClusterFailsEmpty asserts a failed listing answers
// nothing.
func TestCompleteCreateClusterFailsEmpty(t *testing.T) {
	t.Parallel()
	s := completiontest.New(t, nil)
	for _, req := range []completion.Request{{Flag: "cloud-credential"}, {Flag: "machine-config", Partial: "amazonec2config/"}} {
		got := CreateClusterSource().Complete(t.Context(), s.Conn(1), req)
		assert.Empty(t, got.Names, req.Flag)
		assert.Empty(t, got.Prefix, req.Flag)
	}
}

// TestPoolSourceClaims asserts --pool is claimed only with a first
// positional to read the cluster from; another flag or a positional site
// is declined.
func TestPoolSourceClaims(t *testing.T) {
	t.Parallel()
	src := PoolSource()
	reqs, ok := src.Claim(completion.Request{Flag: "pool", Positionals: []string{"prod"}})
	require.True(t, ok)
	assert.Equal(t, completion.ManagementListing(), reqs)
	_, ok = src.Claim(completion.Request{Flag: "pool"})
	assert.False(t, ok, "no cluster typed yet")
	_, ok = src.Claim(completion.Request{Flag: "nodes", Positionals: []string{"prod"}})
	assert.False(t, ok)
	_, ok = src.Claim(completion.Request{Index: 0})
	assert.False(t, ok)
}

// TestCompletePools asserts the machine pool names of the typed cluster
// read from the one object in the default workspace, in the typed
// --namespace, and nothing without a request for a cluster name that is
// not a path segment or a namespace that is not one.
func TestCompletePools(t *testing.T) {
	t.Parallel()
	s := completiontest.New(t, map[string]string{
		prodClusterPath: `{"spec":{"rkeConfig":{"machinePools":[{"name":"pool-b"},{"name":"pool-a"},{"name":"evil;rm -rf"}]}}}`,
		teamClusterPath: `{"spec":{"rkeConfig":{"machinePools":[{"name":"team-pool"}]}}}`,
	})
	src := PoolSource()
	got := src.Complete(t.Context(), s.Conn(1), completion.Request{Flag: "pool", Positionals: []string{"prod"}})
	assert.Equal(t, []string{"pool-b", "pool-a", "evil;rm -rf"}, got.Names)
	got = src.Complete(t.Context(), s.Conn(1), completion.Request{Flag: "pool", Positionals: []string{"prod"}, Tail: []string{"prod", "--namespace", "team-a", "--pool"}})
	assert.Equal(t, []string{"team-pool"}, got.Names)
	assert.Equal(t, []string{prodClusterPath, teamClusterPath}, s.Requested())

	for _, req := range []completion.Request{
		{Flag: "pool", Positionals: []string{"a/b"}},
		{Flag: "pool", Positionals: []string{".."}},
		{Flag: "pool", Positionals: []string{"prod"}, Tail: []string{"--namespace", "x/y"}},
		{Flag: "pool", Positionals: []string{"nonesuch"}},
	} {
		conn := s.Conn(1)
		got := src.Complete(t.Context(), conn, req)
		assert.Empty(t, got.Names, "%v", req)
		if req.Positionals[0] != "nonesuch" {
			assert.Zero(t, conn.Used(), "%v", req)
		}
	}
}
