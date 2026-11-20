package complete

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
)

const (
	strategyProdPath = upgradeProvBase + "/prod"
	strategyEdgePath = upgradeProvBase + "/edge"
	strategyTeamPath = "/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/team-a/clusters/prod"
)

var (
	graceValues   = []string{"30", "60", "120", "300"}
	timeoutValues = []string{"30s", "1m", "2m", "5m", "10m"}
)

// strategyFlagCases are the four flags that complete from a live source, with
// their group and values.
var strategyFlagCases = []struct {
	flag, group string
	values      []string
}{
	{"control-plane-grace-period", "control-plane", graceValues},
	{"worker-grace-period", "worker", graceValues},
	{"control-plane-drain-timeout", "control-plane", timeoutValues},
	{"worker-drain-timeout", "worker", timeoutValues},
}

// TestUpgradeStrategySourceClaims asserts the source owns the <cluster>
// positional with two requests and the four grace period and drain timeout flags
// after a typed cluster with one, and declines every other site.
func TestUpgradeStrategySourceClaims(t *testing.T) {
	t.Parallel()
	src := UpgradeStrategySource()
	reqs, ok := src.Claim(completion.Request{Index: 0})
	assert.True(t, ok)
	assert.Equal(t, completion.Requirements{Cluster: completion.ClusterNone, Credential: completion.CredentialManagement, Allowance: completion.AllowanceTwo}, reqs)
	for _, c := range strategyFlagCases {
		reqs, ok = src.Claim(completion.Request{Flag: c.flag, Positionals: []string{"prod"}})
		assert.True(t, ok, c.flag)
		assert.Equal(t, completion.Requirements{Cluster: completion.ClusterNone, Credential: completion.CredentialManagement, Allowance: completion.AllowanceOne}, reqs, c.flag)
		_, ok = src.Claim(completion.Request{Flag: c.flag})
		assert.False(t, ok, "%s with no cluster typed", c.flag)
	}
	for _, req := range []completion.Request{
		{Flag: "control-plane-concurrency", Positionals: []string{"prod"}}, {Flag: "worker-concurrency", Positionals: []string{"prod"}},
		{Flag: "worker-drain", Positionals: []string{"prod"}}, {Flag: "worker-drain-force", Positionals: []string{"prod"}},
		{Flag: "namespace"}, {Index: 1}, {Flag: "wait"}, {Flag: "timeout", Positionals: []string{"prod"}},
	} {
		_, ok := src.Claim(req)
		assert.False(t, ok, "%+v", req)
	}
}

// TestUpgradeStrategySourceValues asserts each of the four flags offers its list
// for a provisioned cluster with one request, and nothing for an imported
// cluster, a line that turns the flag's own group's drain off with the exact
// word, a failed read and a typed namespace without the cluster. The other
// group's drain=false and a bare drain change nothing.
func TestUpgradeStrategySourceValues(t *testing.T) {
	t.Parallel()
	bodies := map[string]string{
		strategyProdPath: `{"metadata":{"name":"prod"},"spec":{"rkeConfig":{}},"status":{"clusterName":"c-m-prod1"}}`,
		strategyEdgePath: `{"metadata":{"name":"edge"},"spec":{},"status":{"clusterName":"c-m-edge1"}}`,
		strategyTeamPath: `{"metadata":{"name":"prod"},"spec":{"rkeConfig":null}}`,
	}
	other := map[string]string{"control-plane": "worker", "worker": "control-plane"}
	for _, c := range strategyFlagCases {
		offer := func(positional string, tail ...string) ([]string, *completion.Connection) {
			conn := completiontest.New(t, bodies).Conn(1)
			req := completion.Request{Flag: c.flag, Tail: tail}
			if positional != "" {
				req.Positionals = []string{positional}
			}
			return UpgradeStrategySource().Complete(t.Context(), conn, req).Names, conn
		}
		got, conn := offer("prod")
		assert.Equal(t, c.values, got, c.flag)
		assert.Equal(t, 1, conn.Used(), c.flag)
		assert.False(t, conn.Exceeded(), c.flag)

		got, _ = offer("edge")
		assert.Empty(t, got, "%s: an imported cluster refuses the flag", c.flag)
		got, conn = offer("prod", "--"+c.group+"-drain=false")
		assert.Empty(t, got, "%s: its own group's drain is off", c.flag)
		assert.Zero(t, conn.Used(), "%s: no request after the line refuses it", c.flag)
		got, _ = offer("prod", "--"+other[c.group]+"-drain=false")
		assert.Equal(t, c.values, got, "%s: the other group's drain=false", c.flag)
		got, _ = offer("prod", "--"+c.group+"-drain")
		assert.Equal(t, c.values, got, "%s: a bare drain", c.flag)
		got, _ = offer("prod", "--"+c.group+"-drain", "false")
		assert.Equal(t, c.values, got, "%s: a bare drain followed by the word false is not drain=false", c.flag)
		got, _ = offer("prod", "--"+c.group+"-drain=true")
		assert.Equal(t, c.values, got, "%s: drain=true", c.flag)
		got, _ = offer("missing")
		assert.Empty(t, got, "%s: a failed read", c.flag)
		got, _ = offer("prod", "--namespace", "team-a")
		assert.Empty(t, got, "%s: the cluster of the typed namespace has no rkeConfig", c.flag)
		got, _ = offer("")
		assert.Empty(t, got, "%s: no cluster typed", c.flag)
	}
}
