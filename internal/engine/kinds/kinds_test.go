package kinds

import (
	"encoding/json"
	"os"
	"strings"
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
	"managementtoken":          true, // deprecated legacy v3 Token; no MCP tool counterpart
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

// mixedConditionColumn returns a Column that illegally combines Condition
// with Path — used by the mixed-field rejection tests.
func mixedConditionColumn() Column {
	return Column{Header: "READY", Condition: "Ready", Path: []string{"status", "ready"}}
}

func TestValidateRejectsConditionMixedWithPathFields(t *testing.T) {
	t.Parallel()
	mixed := []Kind{
		{Key: "a", Name: "foo", Columns: []Column{mixedConditionColumn()}},
	}
	if err := validateKinds(mixed); err == nil {
		t.Error("Column mixing Condition with Path must fail validation")
	}
	// Real table must still pass.
	if err := Validate(); err != nil {
		t.Fatalf("shipped table must validate: %v", err)
	}
}

// TestValidateFirstDefectWinsPerKindTableOrder pins the merged-loop's
// first-defect-wins ordering: a column-condition conflict on kind 1 is
// reported before an identifier collision on kind 2, because the loop
// visits each kind fully (identifier checks then column checks) in table
// order and returns on the first error it encounters.
func TestValidateFirstDefectWinsPerKindTableOrder(t *testing.T) {
	t.Parallel()
	// Kind 1: clean identifiers, but column mixes Condition with Path.
	// Kind 2: identifier "foo" collides with kind 1's Name — but the loop
	// never reaches it because kind 1's column defect fires first.
	fixture := []Kind{
		{
			Key:     "a",
			Name:    "foo",
			K8sKind: "Foo",
			Columns: []Column{mixedConditionColumn()},
		},
		{Key: "b", Name: "bar", K8sKind: "Bar", Aliases: []string{"foo"}},
	}
	err := validateKinds(fixture)
	if err == nil {
		t.Fatal("expected a validation error but got nil")
	}
	if !strings.Contains(err.Error(), "column") {
		t.Errorf("expected column-conflict error (kind 1) to win over identifier collision (kind 2), got: %v", err)
	}
}

func TestValidateRejectsLowercaseCondition(t *testing.T) {
	t.Parallel()
	lower := []Kind{
		{Key: "a", Name: "foo", Columns: []Column{{Header: "STATUS", Condition: "diskPressure"}}},
	}
	if err := validateKinds(lower); err == nil {
		t.Error("lowercase Condition type must fail validation")
	}
	// Real table must still pass.
	if err := Validate(); err != nil {
		t.Fatalf("shipped table must validate: %v", err)
	}
}

func TestBindingsGeneratedPerKind(t *testing.T) {
	t.Parallel()
	b := Bindings()

	gc, ok := b["get cluster"]
	require.True(t, ok, "get cluster must have a generated binding")
	assert.Equal(t, safety.ContractInternal, gc.Contract)
	assert.Equal(t, safety.Beta, gc.Stability)

	dp, ok := b["delete project"]
	require.True(t, ok, "delete project must have a generated binding")
	assert.Equal(t, safety.ContractPublic, dp.Contract)
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

// TestBindingRowsCarryMemberColumns pins the crtb/prtb table projection:
// one MEMBER column with the userName → groupPrincipalName →
// userPrincipalName fallback, ROLE, and the row's scope column.
func TestBindingRowsCarryMemberColumns(t *testing.T) {
	t.Parallel()
	memberFallback := [][]string{{"groupPrincipalName"}, {"userPrincipalName"}}

	crtb, ok := Lookup("clusterroletemplatebinding")
	require.True(t, ok)
	require.Len(t, crtb.Columns, 3)
	assert.Equal(t, Column{Header: "MEMBER", Path: []string{"userName"}, AltPaths: memberFallback}, crtb.Columns[0])
	assert.Equal(t, Column{Header: "ROLE", Path: []string{"roleTemplateName"}}, crtb.Columns[1])
	assert.Equal(t, Column{Header: "CLUSTER", Path: []string{"clusterName"}}, crtb.Columns[2])

	prtb, ok := Lookup("projectroletemplatebinding")
	require.True(t, ok)
	require.Len(t, prtb.Columns, 3)
	assert.Equal(t, Column{Header: "MEMBER", Path: []string{"userName"}, AltPaths: memberFallback}, prtb.Columns[0])
	assert.Equal(t, Column{Header: "ROLE", Path: []string{"roleTemplateName"}}, prtb.Columns[1])
	assert.Equal(t, Column{Header: "PROJECT", Path: []string{"projectName"}}, prtb.Columns[2])
}

func TestValidateKindsRejectsEmptyK8sKind(t *testing.T) {
	t.Parallel()
	k := Kind{Key: "test", Name: "test", K8sKind: ""}
	err := validateKinds([]Kind{k})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "K8sKind")
}

// TestUserRowDeletableAndCompletable pins the user kind row's flags: the row
// must be Deletable and Completable after the grant-cleanup slice, and must
// carry no NoDeleteReason (that field belongs to non-deletable kinds only).
func TestUserRowDeletableAndCompletable(t *testing.T) {
	t.Parallel()
	k, ok := Lookup("user")
	require.True(t, ok)
	assert.True(t, k.Deletable, "user row must be Deletable")
	assert.True(t, k.Completable, "user row must be Completable")
	assert.Empty(t, k.NoDeleteReason, "Deletable row must not carry a NoDeleteReason")
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
