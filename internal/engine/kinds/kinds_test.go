package kinds

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cliOnlyKinds are table rows with no rancher-ai-mcp tool counterpart at
// the pinned vocabulary snapshot (mcp-kinds-050e375.json): the tranche-D
// provisioning surface predates MCP provisioning tools. Revisit when the
// MCP vocabulary grows provisioning kinds — a key appearing in BOTH this
// set and a future fixture must fail, so stale exemptions cannot linger.
var cliOnlyKinds = map[string]bool{
	"amazonec2config": true, "azureconfig": true, "digitaloceanconfig": true,
	"harvesterconfig": true, "vmwarevsphereconfig": true,
	"clusterregistrationtoken": true,
	"nodedriver":               true,
}

func TestConformsToMCPVocabulary(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("testdata/mcp-kinds-050e375.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]struct{ Group, Version, Resource string }
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	have := map[string]Kind{}
	for _, k := range All() {
		have[k.Key] = k
		if cliOnlyKinds[k.Key] {
			continue
		}
		w, ok := want[k.Key]
		if !ok {
			t.Errorf("kind %q not in the MCP fixture — vocabulary drift", k.Key)
			continue
		}
		if k.GVR.Group != w.Group || k.GVR.Version != w.Version || k.GVR.Resource != w.Resource {
			t.Errorf("kind %q GVR %v diverges from MCP %+v", k.Key, k.GVR, w)
		}
	}
	// Bidirectional: every fixture key must exist in the table — a dropped
	// kind must fail conformance, not silently shrink the surface.
	for key := range want {
		if _, ok := have[key]; !ok {
			t.Errorf("fixture kind %q missing from the table", key)
		}
	}
	// Staleness guard: a cliOnlyKinds exemption must not linger once the MCP
	// vocabulary actually grows to cover that kind — if it appears in the
	// fixture, the exemption is stale and must be removed.
	for key := range cliOnlyKinds {
		if _, ok := want[key]; ok {
			t.Errorf("cliOnlyKinds %q now present in the MCP fixture — remove the stale exemption", key)
		}
	}
}

func TestValidateRejectsCollidingAliases(t *testing.T) {
	t.Parallel()
	if err := Validate(); err != nil {
		t.Fatalf("shipped table must validate: %v", err)
	}
	// A distinct-Key table whose alias collides with another kind's name.
	collide := []Kind{
		{Key: "a", Name: "foo"},
		{Key: "b", Name: "bar", Aliases: []string{"foo"}},
	}
	if err := validateKinds(collide); err == nil {
		t.Error("colliding alias must fail validation")
	}
}

func TestValidateRejectsDuplicateKey(t *testing.T) {
	t.Parallel()
	dup := []Kind{
		{Key: "same", Name: "one"},
		{Key: "same", Name: "two"},
	}
	if err := validateKinds(dup); err == nil {
		t.Error("duplicate Key must fail validation")
	}
}

func TestBindingsGeneratedPerKind(t *testing.T) {
	t.Parallel()
	b := Bindings()

	gc, ok := b["get cluster"]
	require.True(t, ok, "get cluster must have a generated binding")
	assert.Equal(t, safety.TierCRD, gc.Tier)
	assert.Equal(t, safety.Beta, gc.Stability)

	dp, ok := b["delete project"]
	require.True(t, ok, "delete project must have a generated binding")
	assert.Equal(t, safety.TierPublic, dp.Tier)
	assert.Equal(t, safety.Stable, dp.Stability)

	_, ok = b["delete cluster"]
	assert.False(t, ok, "clusters are not deletable — no delete binding")
}

func TestLookupByNameAliasAndKey(t *testing.T) {
	t.Parallel()
	for _, q := range []string{"cluster", "clusters", "managementcluster"} {
		if k, ok := Lookup(q); !ok || k.Key != "managementcluster" {
			t.Errorf("Lookup(%q) = %+v, %v", q, k, ok)
		}
	}
	for q, wantKey := range map[string]string{
		"sts":                         "statefulset",
		"pv":                          "persistentvolume",
		"ing":                         "ingress",
		"crtb":                        "clusterroletemplatebinding",
		"clusterroletemplatebindings": "clusterroletemplatebinding",
		"prtb":                        "projectroletemplatebinding",
		"projectroletemplatebindings": "projectroletemplatebinding",
		"machine":                     "capimachine",
		"fleet-cluster":               "fleetcluster",
		"nodedrivers":                 "nodedriver",
	} {
		k, ok := Lookup(q)
		require.True(t, ok, q)
		assert.Equal(t, wantKey, k.Key)
	}
	if _, ok := Lookup("nonesuch"); ok {
		t.Error("unknown kind must not resolve")
	}
}

func TestProjectScopable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		k    Kind
		want bool
	}{
		{"NSPlain + Downstream", Kind{Name: "pod", Policy: NSPlain, Scope: Downstream}, true},
		{"NSPlain + Management", Kind{Name: "projectroletemplatebinding", Policy: NSPlain, Scope: Management}, false},
		{"namespace carve-out (NSNone + Downstream)", Kind{Name: "namespace", Policy: NSNone, Scope: Downstream}, true},
		{"NSCluster", Kind{Name: "project", Policy: NSCluster, Scope: Management}, false},
		{"NSWorkspace", Kind{Name: "gitrepo", Policy: NSWorkspace, Scope: Management}, false},
		{"NSNone, not the namespace kind", Kind{Name: "node", Policy: NSNone, Scope: Downstream}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ProjectScopable(tt.k))
		})
	}
}

// TestClusterRowsCarryWaitDefaultConditionReady is IC-10/IC-11's row-level
// pin: BOTH cluster rows' bare---wait default is the LITERAL
// "condition=Ready". A literal expectation on purpose — a silent row edit
// must fail here, not round-trip.
func TestClusterRowsCarryWaitDefaultConditionReady(t *testing.T) {
	t.Parallel()
	mgmt, ok := Lookup("managementcluster")
	require.True(t, ok)
	assert.Equal(t, "condition=Ready", mgmt.WaitDefault)

	prov, ok := Lookup("provisioningcluster")
	require.True(t, ok)
	assert.Equal(t, "condition=Ready", prov.WaitDefault)
}

func TestLookupGVK(t *testing.T) {
	t.Parallel()
	tests := []struct {
		apiVersion, kind string
		wantKey          string
		ok               bool
	}{
		{"apps/v1", "Deployment", "deployment", true},
		{"management.cattle.io/v3", "Cluster", "managementcluster", true},
		{"provisioning.cattle.io/v1", "Cluster", "provisioningcluster", true},
		{"fleet.cattle.io/v1alpha1", "Cluster", "fleetcluster", true},
		{"cluster.x-k8s.io/v1beta1", "Cluster", "capicluster", true}, // empty-version row matches any served version of its group
		{"v1", "Pod", "pod", true},                                   // core group apiVersion has no slash
		{"apps/v1", "Widget", "", false},
		{"unknown.io/v1", "Deployment", "", false},
		{"apps/v1beta1", "Deployment", "", false}, // version MUST match for fixed-version rows — never silently dispatch a v1beta1 manifest on the v1 GVR
		{"batch/v2", "Job", "", false},
	}
	for _, tt := range tests {
		k, ok := LookupGVK(tt.apiVersion, tt.kind)
		assert.Equal(t, tt.ok, ok, "%s %s", tt.apiVersion, tt.kind)
		if ok {
			assert.Equal(t, tt.wantKey, k.Key)
		}
	}
}
