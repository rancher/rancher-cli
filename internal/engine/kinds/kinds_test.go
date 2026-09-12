package kinds

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// cliOnlyKinds are table rows with no rancher-ai-mcp tool counterpart at
// the recorded vocabulary snapshot (mcp-kinds-050e375.json): the tranche-D
// provisioning surface predates MCP provisioning tools. Revisit when the
// MCP vocabulary grows provisioning kinds - a key appearing in BOTH this
// set and a future fixture must fail, so stale exemptions cannot linger.
var cliOnlyKinds = map[string]bool{
	"amazonec2config": true, "azureconfig": true, "digitaloceanconfig": true,
	"harvesterconfig": true, "vmwarevsphereconfig": true,
	"clusterregistrationtoken": true,
	"managementtoken":          true, // deprecated legacy v3 Token; no MCP tool counterpart
	"nodedriver":               true,
	"oidcclient":               true, // first Prime-capability kind row; no MCP tool counterpart yet
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
			t.Errorf("kind %q not in the MCP fixture - vocabulary drift", k.Key)
			continue
		}
		if k.GVR.Group != w.Group || k.GVR.Version != w.Version || k.GVR.Resource != w.Resource {
			t.Errorf("kind %q GVR %v diverges from MCP %+v", k.Key, k.GVR, w)
		}
	}
	// Bidirectional: every fixture key must exist in the table - a dropped
	// kind must fail conformance, not silently shrink the surface.
	for key := range want {
		if _, ok := have[key]; !ok {
			t.Errorf("fixture kind %q missing from the table", key)
		}
	}
	// Staleness guard: a cliOnlyKinds exemption must not linger once the MCP
	// vocabulary actually grows to cover that kind - if it appears in the
	// fixture, the exemption is stale and must be removed.
	for key := range cliOnlyKinds {
		if _, ok := want[key]; ok {
			t.Errorf("cliOnlyKinds %q now present in the MCP fixture - remove the stale exemption", key)
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
// with Path - used by the mixed-field rejection tests.
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

// TestValidateFirstDefectWinsPerKindTableOrder asserts the merged-loop's
// first-defect-wins ordering: a column-condition conflict on kind 1 is
// reported before an identifier collision on kind 2, because the loop
// visits each kind fully (identifier checks then column checks) in table
// order and returns on the first error it encounters.
func TestValidateFirstDefectWinsPerKindTableOrder(t *testing.T) {
	t.Parallel()
	// Kind 1: clean identifiers, but column mixes Condition with Path.
	// Kind 2: identifier "foo" collides with kind 1's Name - but the loop
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
	assert.False(t, ok, "clusters are not deletable - no delete binding")
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

// TestClusterRowsCarryWaitDefaultConditionReady asserts IC-10/IC-11 at the
// row level: BOTH cluster rows' bare---wait default is the LITERAL
// "condition=Ready". A literal expectation on purpose - a silent row edit
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

// TestBindingRowsCarryMemberColumns asserts the crtb/prtb table projection:
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
	assert.ErrorContains(t, err, "K8sKind")
}

// TestValidateKindsRejectsEmptyName asserts the generic-family half of the
// dry-run display-identity conformance guarantee: every kind's Name feeds
// straight through to its generated create/patch/delete Effect's
// DisplayKind, so a row with an empty Name must fail closed-world
// validation rather than surface later as a checkPlan command-bug error on
// the kind's first real mutation.
func TestValidateKindsRejectsEmptyName(t *testing.T) {
	t.Parallel()
	k := Kind{Key: "test", Name: "", K8sKind: "Test"}
	err := validateKinds([]Kind{k})
	require.Error(t, err)
	assert.ErrorContains(t, err, "Name")
}

// TestAllKindsHaveNonEmptyName walks the REAL, registered kind table (the
// generic-family closed-world conformance walk): every row's Name is
// non-empty. Combined with PlanCreate/PlanUpdate/PlanFromFetched always
// setting Effect.DisplayKind from a kind's Name, and checkPlan rejecting
// an empty DisplayKind/DisplayName as a command bug, this proves every
// generated create/patch/delete command has a valid dry-run and success
// display identity by construction - no per-command exercise needed for
// the ~130 generated commands, unlike the bespoke Plans, which each
// assert the same fact directly on their own Effect (grant, revoke,
// project add/remove-namespace, the OIDC client secret lifecycle, create
// token).
func TestAllKindsHaveNonEmptyName(t *testing.T) {
	t.Parallel()
	for _, k := range All() {
		assert.NotEmpty(t, k.Name, "kind %q must have a non-empty Name", k.Key)
	}
}

// TestUserRowDeletableAndCompletable asserts the user kind row's flags: the row
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

// TestValidateRejectsBadFirstAlias asserts the plural-first alias invariant: a row
// whose Aliases[0] is not plural(hyphen-stripped Name) must fail validateKinds,
// and a row with no aliases at all also fails - every row must register its plural.
func TestValidateRejectsBadFirstAlias(t *testing.T) {
	t.Parallel()
	bad := []Kind{
		{Key: "a", Name: "foo", K8sKind: "Foo", Aliases: []string{"wrongplural"}},
	}
	err := validateKinds(bad)
	require.Error(t, err)
	require.ErrorContains(t, err, "Aliases[0]")
	require.ErrorContains(t, err, "foos")

	// An alias-less row fails validation.
	noAlias := []Kind{
		{Key: "b", Name: "bar", K8sKind: "Bar"},
	}
	noAliasErr := validateKinds(noAlias)
	require.Error(t, noAliasErr)
	require.ErrorContains(t, noAliasErr, "bar")

	// The shipped table must pass the invariant.
	require.NoError(t, Validate())
}

// TestHintTokenResolvesForAllKinds proves that hintToken(k) is a token
// Lookup recognizes for every kind in the table - i.e. the plural token the
// not-found hint prints is a real command the CLI accepts. Hyphenated kinds
// ("fleet-cluster") register an unhyphenated plural alias ("fleetclusters")
// that hintToken must use instead of plural(k.Name).
func TestHintTokenResolvesForAllKinds(t *testing.T) {
	t.Parallel()
	for _, k := range All() {
		t.Run(k.Name, func(t *testing.T) {
			t.Parallel()
			token := hintToken(k)
			_, ok := Lookup(token)
			assert.True(t, ok, "hintToken(%q) = %q does not resolve via Lookup", k.Name, token)
		})
	}
}

// TestNotFoundCuratedShape asserts the curated NOT_FOUND output shape:
// "<kind> not found: <name>" with a hint naming the list command.
func TestNotFoundCuratedShape(t *testing.T) {
	t.Parallel()
	k, ok := Lookup("user")
	require.True(t, ok)
	err := NotFoundCurated(k, "u-xxx", "")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "user not found: u-xxx", ce.Message)
	assert.Equal(t, "list with: rancher get users", ce.Hint)
}

// TestNotFoundCuratedHyphenatedKind asserts the hyphenated-kind case: the hint
// token is the unhyphenated first alias, not plural(k.Name).
func TestNotFoundCuratedHyphenatedKind(t *testing.T) {
	t.Parallel()
	k, ok := Lookup("fleet-cluster")
	require.True(t, ok)
	err := NotFoundCurated(k, "local", "")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, "fleet-cluster not found: local", ce.Message)
	assert.Equal(t, "list with: rancher get fleetclusters", ce.Hint)
}

// TestNotFoundCuratedDownstreamCarriesCluster asserts that a Downstream-scoped
// kind's list hint carries the invocation's own --cluster value, shell-quoted -
// the list command a Downstream kind's hint names is never valid without it.
func TestNotFoundCuratedDownstreamCarriesCluster(t *testing.T) {
	t.Parallel()
	k, ok := Lookup("deployment")
	require.True(t, ok)
	require.Equal(t, Downstream, k.Scope)
	err := NotFoundCurated(k, "web", "c-abc123")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, "list with: rancher get deployments --cluster 'c-abc123'", ce.Hint)
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
		{"apps/v1beta1", "Deployment", "", false}, // version MUST match for fixed-version rows - never silently dispatch a v1beta1 manifest on the v1 GVR
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

// TestByGVR asserts ByGVR's Group+Resource matching, including the empty-Version
// CAPI case (any versioned incoming GVR matches the empty-Version row), exact
// version rows (version must match), and the miss cases (unknown group/resource,
// wrong version on a fixed-version row).
func TestByGVR(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		gvr     schema.GroupVersionResource
		wantKey string
		ok      bool
	}{
		{
			name:    "exact-version match: management cluster",
			gvr:     schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "clusters"},
			wantKey: "managementcluster",
			ok:      true,
		},
		{
			name:    "exact-version match: pod",
			gvr:     schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			wantKey: "pod",
			ok:      true,
		},
		{
			name:    "exact-version match: deployment",
			gvr:     schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			wantKey: "deployment",
			ok:      true,
		},
		{
			name:    "empty-Version row: CAPI cluster matches any incoming version (v1beta1)",
			gvr:     schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "v1beta1", Resource: "clusters"},
			wantKey: "capicluster",
			ok:      true,
		},
		{
			name:    "empty-Version row: CAPI machine matches any version",
			gvr:     schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "v1alpha4", Resource: "machines"},
			wantKey: "capimachine",
			ok:      true,
		},
		{
			name:    "empty-Version row: ext.cattle.io token matches any version",
			gvr:     schema.GroupVersionResource{Group: "ext.cattle.io", Version: "v1", Resource: "tokens"},
			wantKey: "token",
			ok:      true,
		},
		{
			name: "wrong version for fixed-version row: miss",
			gvr:  schema.GroupVersionResource{Group: "apps", Version: "v1beta1", Resource: "deployments"},
			ok:   false,
		},
		{
			name: "unknown group: miss",
			gvr:  schema.GroupVersionResource{Group: "custom.example.io", Version: "v1", Resource: "widgets"},
			ok:   false,
		},
		{
			name: "unknown resource in known group: miss",
			gvr:  schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "unknowns"},
			ok:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			k, ok := ByGVR(tt.gvr)
			assert.Equal(t, tt.ok, ok, "ByGVR(%v)", tt.gvr)
			if tt.ok {
				assert.Equal(t, tt.wantKey, k.Key)
			}
		})
	}
}

// TestValidateGroupResourceUniqueness asserts that the table validates correctly
// when two rows share a Group+Resource (which would make ByGVR ambiguous).
func TestValidateGroupResourceUniqueness(t *testing.T) {
	t.Parallel()
	// Two rows with identical Group+Resource but different Keys:
	dup := []Kind{
		{Key: "a", Name: "foo", K8sKind: "Foo", Aliases: []string{"foos"},
			GVR: schema.GroupVersionResource{Group: "example.io", Version: "v1", Resource: "foos"}},
		{Key: "b", Name: "bar", K8sKind: "Bar", Aliases: []string{"bars"},
			GVR: schema.GroupVersionResource{Group: "example.io", Version: "v1", Resource: "foos"}},
	}
	err := validateKinds(dup)
	require.Error(t, err, "duplicate Group+Resource must fail validation")
	require.ErrorContains(t, err, "ByGVR cannot distinguish them")

	// The real table must still pass - no current Group+Resource collisions.
	require.NoError(t, Validate(), "shipped table must validate")
}

func TestValidateRejectsJoinMisuse(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		col  Column
	}{
		{"Join with Bool", Column{Header: "X", Path: []string{"a"}, Join: true, Bool: true}},
		{"Join with Condition", Column{Header: "X", Condition: "Ready", Join: true, Path: []string{"a"}}},
		{"Join with AltPaths", Column{Header: "X", Path: []string{"a"}, Join: true, AltPaths: [][]string{{"b"}}}},
		{"Join without Path", Column{Header: "X", Join: true}},
		{"Default with Bool", Column{Header: "X", Path: []string{"a"}, Bool: true, Default: "true"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bad := []Kind{{Key: "a", Name: "foo", Aliases: []string{"foos"}, Columns: []Column{tt.col}}}
			assert.Error(t, validateKinds(bad))
		})
	}
	if err := Validate(); err != nil {
		t.Fatalf("shipped table must validate: %v", err)
	}
}

// TestValidateRejectsMalformedCapability asserts the shared full-shape
// validator at the kind-table boundary - the counterpart of
// runtime's Registry.Validate tests - for all three malformations: set but
// unknown tier, a known tier with an empty Feature, and Feature set with no
// Tier (the half-set value that would otherwise read as absent under
// IsSet and vanish from every gate).
func TestValidateRejectsMalformedCapability(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		cap  runtime.CapabilityRequirement
	}{
		{"unknown tier", runtime.CapabilityRequirement{Tier: "bogus", Feature: "widget-feature"}},
		{"known tier empty feature", runtime.CapabilityRequirement{Tier: runtime.CapabilityPrime}},
		{"feature no tier", runtime.CapabilityRequirement{Feature: "widget-feature"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bad := []Kind{{Key: "a", Name: "foo", K8sKind: "Foo", Aliases: []string{"foos"}, Capability: tt.cap}}
			assert.Error(t, validateKinds(bad))
		})
	}
	if err := Validate(); err != nil {
		t.Fatalf("shipped table must validate: %v", err)
	}
}

// TestValidateRejectsCountMisuse asserts Count's exclusivity: it is mutually
// exclusive with every other projection axis (Path, AltPaths, Bool,
// Default, Join, Condition).
func TestValidateRejectsCountMisuse(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		col  Column
	}{
		{"Count with Path", Column{Header: "X", Count: []string{"data"}, Path: []string{"a"}}},
		{"Count with AltPaths", Column{Header: "X", Count: []string{"data"}, AltPaths: [][]string{{"a"}}}},
		{"Count with Bool", Column{Header: "X", Count: []string{"data"}, Bool: true}},
		{"Count with Default", Column{Header: "X", Count: []string{"data"}, Default: "0"}},
		{"Count with Join", Column{Header: "X", Count: []string{"data"}, Join: true, Path: []string{"a"}}},
		{"Count with Condition", Column{Header: "X", Count: []string{"data"}, Condition: "Ready"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bad := []Kind{{Key: "a", Name: "foo", Aliases: []string{"foos"}, Columns: []Column{tt.col}}}
			assert.Error(t, validateKinds(bad))
		})
	}
	if err := Validate(); err != nil {
		t.Fatalf("shipped table must validate: %v", err)
	}
}

// TestValidateRejectsSystemFilterMisuse asserts SystemFilter's three required
// fields: a non-empty Path with no empty segment, a non-empty Prefix, and a
// non-empty Label - independent of the Columns axes.
func TestValidateRejectsSystemFilterMisuse(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		sf   PrefixFilter
	}{
		{"empty Path", PrefixFilter{Prefix: "system://", Label: "system account"}},
		{"empty Path segment", PrefixFilter{Path: []string{""}, Prefix: "system://", Label: "system account"}},
		{"empty Prefix", PrefixFilter{Path: []string{"principalIds"}, Label: "system account"}},
		{"empty Label", PrefixFilter{Path: []string{"principalIds"}, Prefix: "system://"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sf := tt.sf
			bad := []Kind{{Key: "a", Name: "foo", Aliases: []string{"foos"}, SystemFilter: &sf}}
			assert.Error(t, validateKinds(bad))
		})
	}
	if err := Validate(); err != nil {
		t.Fatalf("shipped table must validate: %v", err)
	}
}

// TestByKeyMatchesTheKeyAlone asserts ByKey resolves a canonical key and
// declines the CLI name and aliases Lookup accepts.
func TestByKeyMatchesTheKeyAlone(t *testing.T) {
	t.Parallel()
	k, ok := ByKey("managementcluster")
	require.True(t, ok)
	assert.Equal(t, "cluster", k.Name)
	for _, q := range []string{"cluster", "clusters", "provisioning-cluster", "no-such-kind", ""} {
		_, ok := ByKey(q)
		assert.False(t, ok, q)
	}
}

// TestPrefixFilterMatches asserts the system-account rule as applied to
// one object: any element with the prefix matches; a missing field, a
// field that is not an array, a non-string element, and a nil filter
// never match.
func TestPrefixFilterMatches(t *testing.T) {
	t.Parallel()
	f := &PrefixFilter{Path: []string{"principalIds"}, Prefix: "system://"}
	assert.True(t, f.Matches(map[string]any{"principalIds": []any{"local://x", "system://x"}}))
	assert.False(t, f.Matches(map[string]any{"principalIds": []any{"local://x"}}))
	assert.False(t, f.Matches(map[string]any{"principalIds": "system://x"}))
	assert.False(t, f.Matches(map[string]any{"principalIds": []any{7, "local://x"}}))
	assert.False(t, f.Matches(map[string]any{}))
	nested := &PrefixFilter{Path: []string{"spec", "ids"}, Prefix: "system:"}
	assert.True(t, nested.Matches(map[string]any{"spec": map[string]any{"ids": []any{"system://x"}}}))
	var none *PrefixFilter
	assert.False(t, none.Matches(map[string]any{"principalIds": []any{"system://x"}}))
}
