package kinds

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// cliOnlyKinds are table rows with no rancher-ai-mcp counterpart in the
// fixture testdata/mcp-kinds.json. TestConformsToMCPVocabulary fails when a
// key in this set is in the fixture or is not a row of the table. An
// exemption cannot outlive the kind it covers.
var cliOnlyKinds = map[string]bool{
	"amazonec2config": true, "azureconfig": true, "digitaloceanconfig": true,
	"harvesterconfig": true, "vmwarevsphereconfig": true,
	"clusterregistrationtoken": true,
	"managementtoken":          true, // deprecated legacy v3 Token; no MCP tool counterpart
	"managementnode":           true, // no rancher-ai-mcp counterpart
	"token":                    true, // no rancher-ai-mcp counterpart

	"etcdsnapshotsave":      true, // no rancher-ai-mcp counterpart
	"etcdsnapshotrestore":   true, // no rancher-ai-mcp counterpart
	"encryptionkeyrotation": true, // no rancher-ai-mcp counterpart
	"certificaterotation":   true, // no rancher-ai-mcp counterpart
}

// mcpNotYetKinds are the keys of the fixture testdata/mcp-kinds.json that
// have no row in the kind table: kinds rancher-ai-mcp knows that the CLI has
// no row for yet. TestConformsToMCPVocabulary skips them and fails when one
// gains a row or leaves the fixture. The list shrinks as rows are added.
var mcpNotYetKinds = map[string]bool{
	"aiagentconfig": true, "auditpolicy": true, "bundlenamespacemapping": true,
	"clusterproxyconfig": true, "crd": true, "csidriver": true, "csinode": true,
	"customresourcedefinition": true, "endpointslice": true, "flowschema": true,
	"gitreporestriction": true, "group": true, "groupmember": true,
	"imagescan": true, "ingressclass": true, "kontainerdriver": true,
	"mutatingwebhookconfiguration": true, "node.metrics.k8s.io": true,
	"nodepool": true, "nodetemplate": true, "pod.metrics.k8s.io": true,
	"podsecurityadmissionconfigurationtemplate": true, "priorityclass": true,
	"prioritylevelconfiguration": true, "projectnetworkpolicy": true,
	"proxyendpoint": true, "rancherusernotification": true,
	"runtimeclass": true, "userattribute": true,
	"validatingadmissionpolicy": true, "validatingadmissionpolicybinding": true,
	"validatingwebhookconfiguration": true, "virtualmachine": true,
	"virtualmachineclusterpreference": true, "virtualmachineinstance": true,
	"virtualmachinepreference": true, "volumeattachment": true, "vpa": true,
}

// mcpKindsFixture is the file testdata/mcp-kinds.json: the map of kind keys
// of rancher-ai-mcp at one upstream commit.
type mcpKindsFixture struct {
	Commit string `json:"commit"`
	Kinds  map[string]struct {
		Group    string `json:"group"`
		Version  string `json:"version"`
		Resource string `json:"resource"`
	} `json:"kinds"`
}

var fortyHexDigits = regexp.MustCompile(`^[0-9a-f]{40}$`)

// TestConformsToMCPVocabulary asserts the kind table agrees with the fixture
// of rancher-ai-mcp kind keys. Each row not in cliOnlyKinds has a fixture
// entry with the same group and resource, and the same version when the row
// names one (a row with an empty version, such as kubeconfig, is resolved
// through API discovery at call time). Each fixture key has a row, unless it
// is in mcpNotYetKinds, and neither list holds a stale key.
func TestConformsToMCPVocabulary(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("testdata/mcp-kinds.json")
	require.NoError(t, err)
	var fixture mcpKindsFixture
	require.NoError(t, json.Unmarshal(b, &fixture))
	require.Regexp(t, fortyHexDigits, fixture.Commit, "the fixture's commit is the 40 lowercase hex digits of an upstream commit")
	want := fixture.Kinds
	require.NotEmpty(t, want)
	have := map[string]Kind{}
	for _, k := range All() {
		have[k.Key] = k
		if cliOnlyKinds[k.Key] {
			continue
		}
		w, ok := want[k.Key]
		if !ok {
			t.Errorf("kind %q not in the fixture of rancher-ai-mcp kind keys - vocabulary drift", k.Key)
			continue
		}
		if k.GVR.Group != w.Group || k.GVR.Resource != w.Resource || (k.GVR.Version != "" && k.GVR.Version != w.Version) {
			t.Errorf("kind %q GVR %v diverges from MCP %+v", k.Key, k.GVR, w)
		}
	}
	// Bidirectional: every fixture key must exist in the table - a dropped
	// kind must fail conformance, not silently shrink the surface. A key
	// rancher-ai-mcp has and the CLI has no row for is listed in
	// mcpNotYetKinds.
	for key := range want {
		if _, ok := have[key]; !ok && !mcpNotYetKinds[key] {
			t.Errorf("fixture kind %q missing from the table - add a row, or list it in mcpNotYetKinds", key)
		}
	}
	// Staleness guards: an exemption must not outlive its reason.
	for key := range cliOnlyKinds {
		if _, ok := want[key]; ok {
			t.Errorf("cliOnlyKinds %q is in the fixture of rancher-ai-mcp kind keys - remove the stale exemption", key)
		}
		if _, ok := have[key]; !ok {
			t.Errorf("cliOnlyKinds %q has no row in the table - remove it", key)
		}
	}
	for key := range mcpNotYetKinds {
		if _, ok := have[key]; ok {
			t.Errorf("mcpNotYetKinds %q has a row in the table now - remove it from the list", key)
		}
		if _, ok := want[key]; !ok {
			t.Errorf("mcpNotYetKinds %q is not in the fixture of rancher-ai-mcp kind keys - remove it from the list", key)
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
		{"NSPlain + Management", Kind{Name: "clusterregistrationtoken", Policy: NSPlain, Scope: Management}, false},
		{"NSPlain + Management with a project backing namespace", Kind{Name: "projectroletemplatebinding", Policy: NSPlain, Scope: Management, ProjectBackingNamespace: true}, true},
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

// TestClusterRowsCarryWaitDefaultConditionReady asserts at the
// row level that BOTH cluster rows' bare---wait default is the LITERAL
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
// must be Deletable and Completable, and must
// carry no NoDeleteReason (that field belongs to non-deletable kinds only).
func TestUserRowDeletableAndCompletable(t *testing.T) {
	t.Parallel()
	k, ok := Lookup("user")
	require.True(t, ok)
	assert.True(t, k.Deletable, "user row must be Deletable")
	assert.True(t, k.Completable, "user row must be Completable")
	assert.Empty(t, k.NoDeleteReason, "Deletable row must not carry a NoDeleteReason")
}

// TestValidateRejectsProjectBackingNamespaceOutsideManagementPlainRows
// asserts ProjectBackingNamespace is accepted on a Management row with a
// plain namespace and refused on every other row.
func TestValidateRejectsProjectBackingNamespaceOutsideManagementPlainRows(t *testing.T) {
	t.Parallel()
	row := func(policy NamespacePolicy, scope Scope) []Kind {
		return []Kind{{Key: "foo", Name: "foo", K8sKind: "Foo", Aliases: []string{"foos"}, Policy: policy, Scope: scope, ProjectBackingNamespace: true}}
	}
	require.NoError(t, validateKinds(row(NSPlain, Management)))
	for name, bad := range map[string][]Kind{
		"downstream plain": row(NSPlain, Downstream),
		"management none":  row(NSNone, Management),
		"cluster id":       row(NSCluster, Management),
		"workspace":        row(NSWorkspace, Management),
	} {
		err := validateKinds(bad)
		require.Error(t, err, name)
		require.ErrorContains(t, err, "ProjectBackingNamespace", name)
	}
}

// TestValidateAcceptsSteveReadOnForbiddenOnlyWhereThereIsASteveRead asserts
// SteveReadOnForbidden is accepted on a Management row and on the namespace
// row, refused on any other downstream row, and that a row that sets it
// together with ServerTable needs a table builder.
func TestValidateAcceptsSteveReadOnForbiddenOnlyWhereThereIsASteveRead(t *testing.T) {
	t.Parallel()
	row := func(key string, scope Scope, serverTable bool) []Kind {
		return []Kind{{Key: key, Name: key, K8sKind: "Foo", Aliases: []string{key + "s"}, Policy: NSNone, Scope: scope, SteveReadOnForbidden: true, ServerTable: serverTable}}
	}
	require.NoError(t, validateKinds(row("foo", Management, false)))
	require.NoError(t, validateKinds(row("namespace", Downstream, true)), "the namespace row has a Steve read and a table builder")

	for _, key := range []string{"foo", "node", "pod"} {
		err := validateKinds(row(key, Downstream, false))
		require.ErrorContains(t, err, `kind "`+key+`" sets SteveReadOnForbidden but is a downstream kind with no Steve read`, key)
	}

	err := validateKinds(row("foo", Management, true))
	require.ErrorContains(t, err, `kind "foo" sets ServerTable together with SteveReadOnForbidden and has no table builder`)
}

// TestValidateRejectsNamespaceDescOutsideManagementPlainAndWorkspaceRows
// asserts NamespaceDesc is accepted on a Management row with a plain or
// workspace namespace and refused on every other row, and that its text
// carries exactly one backtick pair.
func TestValidateRejectsNamespaceDescOutsideManagementPlainAndWorkspaceRows(t *testing.T) {
	t.Parallel()
	const desc = "cluster `id` of the token"
	row := func(policy NamespacePolicy, scope Scope, text string) []Kind {
		return []Kind{{Key: "foo", Name: "foo", K8sKind: "Foo", Aliases: []string{"foos"}, Policy: policy, Scope: scope, NamespaceDesc: text}}
	}
	require.NoError(t, validateKinds(row(NSPlain, Management, desc)))
	require.NoError(t, validateKinds(row(NSWorkspace, Management, desc)))
	for name, bad := range map[string][]Kind{
		"downstream plain":       row(NSPlain, Downstream, desc),
		"downstream workspace":   row(NSWorkspace, Downstream, desc),
		"management none":        row(NSNone, Management, desc),
		"cluster id":             row(NSCluster, Management, desc),
		"no backticks":           row(NSPlain, Management, "cluster id"),
		"one backtick":           row(NSPlain, Management, "cluster `id"),
		"two backtick pairs":     row(NSWorkspace, Management, "cluster `id` or `name`"),
		"no backticks workspace": row(NSWorkspace, Management, "Fleet workspace"),
	} {
		err := validateKinds(bad)
		require.Error(t, err, name)
		require.ErrorContains(t, err, "NamespaceDesc", name)
	}
}

// TestNamespaceDescOfManagementRows asserts the --namespace description of
// the Management rows whose value is not an ordinary namespace of the
// target cluster and not always a Fleet workspace.
func TestNamespaceDescOfManagementRows(t *testing.T) {
	t.Parallel()
	for key, want := range map[string]string{
		"projectroletemplatebinding": "the project's backing `namespace` on the local cluster; or pass --project with --cluster",
		"clusterregistrationtoken":   "cluster `id` (c-...), the namespace of its cluster registration tokens",
		"bundledeployment":           "cluster namespace of a Fleet cluster (`namespace`): its status.namespace, such as cluster-fleet-...",
		"etcdsnapshot":               "`namespace` of the cluster's snapshots: for a cluster Rancher provisions, the namespace of its provisioning cluster (default: fleet-default); otherwise the cluster's id (c-...)",
		"capicluster":                "`namespace` of the CAPI cluster: for a cluster Rancher provisions, the namespace of its provisioning cluster (default: fleet-default); otherwise the namespace it was created in",
		"capimachine":                "`namespace` of the CAPI object: for a cluster Rancher provisions, the namespace of its provisioning cluster (default: fleet-default); otherwise the namespace the CAPI cluster was created in",
		"capimachineset":             "`namespace` of the CAPI object: for a cluster Rancher provisions, the namespace of its provisioning cluster (default: fleet-default); otherwise the namespace the CAPI cluster was created in",
		"capimachinedeployment":      "`namespace` of the CAPI object: for a cluster Rancher provisions, the namespace of its provisioning cluster (default: fleet-default); otherwise the namespace the CAPI cluster was created in",
	} {
		k, ok := Lookup(key)
		require.True(t, ok, key)
		assert.Equal(t, want, k.NamespaceDesc, key)
	}
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
	assert.Equal(t, clierr.Rancher+" get users", ce.Hint)
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
	assert.Equal(t, clierr.Rancher+" get fleetclusters", ce.Hint)
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
	assert.Equal(t, clierr.Rancher+" get deployments --cluster 'c-abc123'", ce.Hint)
}

// TestNotFoundCuratedLeavesOutTheListCommandForAnUnprintableCluster asserts a
// Downstream-scoped kind's NOT_FOUND with a --cluster value that cannot be
// printed as one shell word, or with none, has no hint and carries the note,
// and that a Management-scoped kind ignores the value.
func TestNotFoundCuratedLeavesOutTheListCommandForAnUnprintableCluster(t *testing.T) {
	t.Parallel()
	k, ok := Lookup("deployment")
	require.True(t, ok)
	for name, cluster := range map[string]string{
		"apostrophe":       "c'x",
		"right quote":      "c\u2019x",
		"zero width":       "c\u200bx",
		"double backslash": "c\\\\x",
		"empty":            "",
	} {
		t.Run(name, func(t *testing.T) {
			var ce *clierr.Error
			require.ErrorAs(t, NotFoundCurated(k, "web", cluster), &ce)
			assert.Empty(t, ce.Hint)
			assert.Equal(t, "deployment not found: web; "+clierr.LeftOutNote, ce.Message)
		})
	}
	user, ok := Lookup("user")
	require.True(t, ok)
	require.Equal(t, Management, user.Scope)
	var ce *clierr.Error
	require.ErrorAs(t, NotFoundCurated(user, "bob", "c'x"), &ce)
	assert.Equal(t, clierr.Rancher+" get users", ce.Hint)
}

// TestForbiddenCommandIsLeftOutForAnUnprintableValue asserts the field of an
// RBAC refusal's command is empty when the name or namespace cannot be printed
// as one shell word, and that ForbiddenCommand reports the command as left out.
func TestForbiddenCommandIsLeftOutForAnUnprintableValue(t *testing.T) {
	t.Parallel()
	cluster, _ := ByKey("managementcluster")
	project, _ := ByKey("project")
	for _, v := range []string{"x'y", "x\u2019y", "x\u200by", "x\\\\y"} {
		c, ok := ForbiddenCommand(cluster, "get", "", v)
		require.True(t, ok)
		assert.False(t, c.OK())
		assertForbiddenLeftOut(t, cluster, "get", "", v)
		c, ok = ForbiddenCommand(project, "list", v, "")
		require.True(t, ok)
		assert.False(t, c.OK())
		assertForbiddenLeftOut(t, project, "list", v, "")
	}
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
		want string
	}{
		{"unknown tier", runtime.CapabilityRequirement{Tier: "bogus", Feature: "widget-feature"},
			`foo: unknown capability tier "bogus"`},
		{"known tier empty feature", runtime.CapabilityRequirement{Tier: runtime.CapabilityPrime},
			`foo: capability tier "prime" requires a Feature name`},
		{"feature no tier", runtime.CapabilityRequirement{Feature: "widget-feature"},
			"foo: capability requirement has Feature/Unsupported set with no Tier"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bad := []Kind{{Key: "a", Name: "foo", K8sKind: "Foo", Aliases: []string{"foos"}, Capability: tt.cap}}
			assert.EqualError(t, validateKinds(bad), tt.want)
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

// TestValidateRejectsServerTableMisuse asserts a ServerTable row is refused
// when it also carries Columns, a SystemFilter or RedactPaths, and accepted
// on its own: the server's table has its own columns and is shown as it is.
func TestValidateRejectsServerTableMisuse(t *testing.T) {
	t.Parallel()
	base := Kind{Key: "a", Name: "foo", K8sKind: "Foo", Aliases: []string{"foos"}, ServerTable: true}
	require.NoError(t, validateKinds([]Kind{base}))
	for _, tt := range []struct {
		name string
		edit func(k *Kind)
		want string
	}{
		{"Columns", func(k *Kind) { k.Columns = []Column{{Header: "X", Path: []string{"x"}}} }, "Columns"},
		{"SystemFilter", func(k *Kind) {
			k.SystemFilter = &PrefixFilter{Path: []string{"p"}, Prefix: "s://", Label: "system account"}
		}, "SystemFilter"},
		{"RedactPaths", func(k *Kind) { k.RedactPaths = [][]string{{"password"}} }, "RedactPaths"},
		{"RedactReferences", func(k *Kind) { k.RedactReferences = true }, "RedactReferences"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bad := base
			tt.edit(&bad)
			require.ErrorContains(t, validateKinds([]Kind{bad}), tt.want)
		})
	}
}

// TestValidateRejectsCreateWarningWithNoCreateViaManifest asserts a row that
// sets both CreateWarning and NoCreateViaManifest is refused (a refused
// create shows no warning), and that each field alone is accepted.
func TestValidateRejectsCreateWarningWithNoCreateViaManifest(t *testing.T) {
	t.Parallel()
	base := Kind{Key: "a", Name: "foo", K8sKind: "Foo", Aliases: []string{"foos"}}
	warned := base
	warned.CreateWarning = "the controller installs it"
	require.NoError(t, validateKinds([]Kind{warned}))
	refused := base
	refused.NoCreateViaManifest = "use create foo"
	require.NoError(t, validateKinds([]Kind{refused}))
	both := warned
	both.NoCreateViaManifest = "use create foo"
	require.ErrorContains(t, validateKinds([]Kind{both}), "CreateWarning together with NoCreateViaManifest")
}

// TestManagedChartRow asserts the managedchart row: identity, routing, the
// five columns, the hidden Helm values, the gates, the default wait, the
// delete and create warnings, and that its manifest apiVersion and kind
// resolve to it.
func TestManagedChartRow(t *testing.T) {
	t.Parallel()
	mc, ok := Lookup("managedcharts")
	require.True(t, ok)
	assert.Equal(t, "managedchart", mc.Key)
	assert.Equal(t, "managedchart", mc.Name)
	assert.Equal(t, []string{"managedcharts"}, mc.Aliases)
	assert.Equal(t, schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "managedcharts"}, mc.GVR)
	assert.Equal(t, "ManagedChart", mc.K8sKind)
	assert.Equal(t, Management, mc.Scope)
	assert.Equal(t, NSWorkspace, mc.Policy)
	assert.Equal(t, "fleet-default", mc.DefaultNamespace)
	assert.True(t, mc.Deletable)
	assert.True(t, mc.Sensitive)
	assert.True(t, mc.Completable)
	assert.Equal(t, "condition=Ready", mc.WaitDefault)
	assert.False(t, mc.WaitFailsOnStalled)
	assert.Equal(t, safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta}, mc.Binding)
	assert.Equal(t, []Column{
		{Header: "CHART", Path: []string{"spec", "chart"}},
		{Header: "VERSION", Path: []string{"spec", "version"}},
		{Header: "REPO", Path: []string{"spec", "repoName"}},
		{Header: "BUNDLEDEPLOYMENTS-READY", Path: []string{"status", "display", "readyClusters"}},
		{Header: "READY", Condition: "Ready"},
	}, mc.Columns)
	assert.Equal(t, [][]string{
		{"spec", "values"},
		{"spec", "targets", "*", "helm", "values"}, {"spec", "targets", "*", "helm", "templateValues"},
	}, mc.RedactPaths)
	assert.Equal(t, "Rancher serves ManagedCharts when its fleet and provisioningv2 features are both on; when either is off, get returns NOT_FOUND.", mc.GetDesc)
	assert.Equal(t, "deleting this ManagedChart makes Rancher delete its Fleet Bundle, which makes Fleet uninstall the chart from every target cluster, except a cluster whose first matching target has keepResources: true", mc.DeleteCascadeWarning)
	assert.Equal(t, "Rancher installs the chart from the ClusterRepo whose name is in spec.repoName, through Fleet, on every target cluster: a cluster that spec.targets matches, except one whose first matching target has doNotDeploy: true", mc.CreateWarning)
	assert.Empty(t, mc.NoCreateViaManifest)

	byGVK, ok := LookupGVK("management.cattle.io/v3", "ManagedChart")
	require.True(t, ok)
	assert.Equal(t, "managedchart", byGVK.Key)
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

// TestForbiddenCommandField asserts the caller-scoped command for each kind
// verb the generated commands send through the local cluster's proxy, and none
// for every other kind and verb.
func TestForbiddenCommandField(t *testing.T) {
	t.Parallel()
	cluster, _ := ByKey("managementcluster")
	project, _ := ByKey("project")
	user, _ := ByKey("user")
	for _, tc := range []struct {
		k                     Kind
		verb, namespace, name string
		want                  string
	}{
		{cluster, "list", "", "", clierr.Rancher + " access clusters"},
		{cluster, "get", "", "c-abc", clierr.Rancher + " inspect cluster 'c-abc'"},
		{cluster, "get", "", "my cluster", clierr.Rancher + " inspect cluster 'my cluster'"},
		{cluster, "get", "", "it's", ""},
		{cluster, "update", "", "c-abc", ""},
		{project, "list", "c-abc", "", clierr.Rancher + " access projects --cluster 'c-abc'"},
		{project, "list", "", "", clierr.Rancher + " access projects"},
		{project, "get", "c-abc", "p-1", ""},
		{user, "list", "", "", ""},
	} {
		c, _ := ForbiddenCommand(tc.k, tc.verb, tc.namespace, tc.name)
		assert.Equal(t, tc.want, c.Field(), "%s %s", tc.k.Key, tc.verb)
	}
}

// TestDisplayNameColumnsAreHeadedDisplayName asserts every column of every
// kind row that reads a displayName field, from its path or any alternate
// path, has the header DISPLAY NAME.
func TestDisplayNameColumnsAreHeadedDisplayName(t *testing.T) {
	t.Parallel()
	endsInDisplayName := func(path []string) bool {
		return len(path) > 0 && path[len(path)-1] == "displayName"
	}
	var seen int
	for _, k := range All() {
		for _, c := range k.Columns {
			if !endsInDisplayName(c.Path) && !slices.ContainsFunc(c.AltPaths, endsInDisplayName) {
				continue
			}
			seen++
			assert.Equal(t, "DISPLAY NAME", c.Header, "column of %s", k.Key)
		}
	}
	assert.Positive(t, seen, "no kind row has a display name column")
}

// TestOIDCClientRowTexts asserts the oidcclient row's column headers and
// its delete warning word for word; no table or golden test shows them.
func TestOIDCClientRowTexts(t *testing.T) {
	t.Parallel()
	k, ok := ByKey("oidcclient")
	require.True(t, ok)
	var headers []string
	for _, c := range k.Columns {
		headers = append(headers, c.Header)
	}
	assert.Equal(t, []string{"CLIENT-ID", "SECRETS", "TOKEN-EXPIRATION", "REFRESH-EXPIRATION"}, headers)
	assert.Equal(t, "deleting this OIDC client also deletes its Secret, which invalidates every client secret; every application that uses one loses access", k.DeleteCascadeWarning)
}

// TestFleetHelmOpAndWorkspaceRows asserts the helmop row waits on Ready and
// fails on Stalled, warns about the uninstall on delete and hides the Helm
// values and template values of the HelmOp and of each target, and that the
// fleet-workspace row is cluster-scoped, cannot be deleted, has no default
// wait condition and requires --yes on patch.
func TestFleetHelmOpAndWorkspaceRows(t *testing.T) {
	t.Parallel()
	op, ok := Lookup("helmops")
	require.True(t, ok)
	assert.Equal(t, "helmop", op.Key)
	assert.Equal(t, "helmop", op.Name)
	assert.Equal(t, []string{"helmops"}, op.Aliases)
	assert.Equal(t, "HelmOp", op.K8sKind)
	assert.Equal(t, Management, op.Scope)
	assert.Equal(t, safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta}, op.Binding)
	assert.Equal(t, NSWorkspace, op.Policy)
	assert.Equal(t, "fleet-default", op.DefaultNamespace)
	assert.Equal(t, "condition=Ready", op.WaitDefault)
	assert.True(t, op.WaitFailsOnStalled)
	assert.True(t, op.Deletable)
	assert.False(t, op.Sensitive)
	assert.Equal(t, [][]string{
		{"spec", "helm", "values"}, {"spec", "helm", "templateValues"},
		{"spec", "targets", "*", "helm", "values"}, {"spec", "targets", "*", "helm", "templateValues"},
	}, op.RedactPaths)
	assert.Contains(t, op.DeleteCascadeWarning, "keepResources")

	ws, ok := Lookup("fleet-workspaces")
	require.True(t, ok)
	assert.Equal(t, "fleetworkspace", ws.Key)
	assert.Equal(t, "fleet-workspace", ws.Name)
	assert.Equal(t, []string{"fleetworkspaces", "fleet-workspaces"}, ws.Aliases)
	assert.Equal(t, "FleetWorkspace", ws.K8sKind)
	assert.Equal(t, Management, ws.Scope)
	assert.Equal(t, safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta}, ws.Binding)
	assert.Contains(t, ws.NoDeleteReason, "provisioning.cattle.io/managed")
	assert.Equal(t, NSNone, ws.Policy)
	assert.False(t, ws.Deletable)
	assert.NotEmpty(t, ws.NoDeleteReason)
	assert.True(t, ws.Sensitive)
	assert.Empty(t, ws.WaitDefault)
	assert.False(t, ws.WaitFailsOnStalled)
}

// TestBuiltInKubernetesKindRows asserts, for each of the six kinds, that its
// name, plural, kubectl short name and key all resolve to the same row, and
// that the row has the group, version, resource, namespace policy, sensitivity
// and server-table setting listed in this test. It also asserts that a
// manifest with the kind's apiVersion and kind resolves to the row and that a
// manifest with another apiVersion of the kind does not.
func TestBuiltInKubernetesKindRows(t *testing.T) {
	t.Parallel()
	binding := safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta}
	tests := []struct {
		name       string
		plural     string
		short      string
		gvr        schema.GroupVersionResource
		k8sKind    string
		policy     NamespacePolicy
		sensitive  bool
		apiVersion string
		otherAPI   string
	}{
		{"horizontalpodautoscaler", "horizontalpodautoscalers", "hpa",
			schema.GroupVersionResource{Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"},
			"HorizontalPodAutoscaler", NSPlain, false, "autoscaling/v2", "autoscaling/v1"},
		{"limitrange", "limitranges", "limits",
			schema.GroupVersionResource{Group: "", Version: "v1", Resource: "limitranges"},
			"LimitRange", NSPlain, false, "v1", "v2"},
		{"networkpolicy", "networkpolicies", "netpol",
			schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"},
			"NetworkPolicy", NSPlain, true, "networking.k8s.io/v1", "networking.k8s.io/v1beta1"},
		{"poddisruptionbudget", "poddisruptionbudgets", "pdb",
			schema.GroupVersionResource{Group: "policy", Version: "v1", Resource: "poddisruptionbudgets"},
			"PodDisruptionBudget", NSPlain, false, "policy/v1", "policy/v1beta1"},
		{"resourcequota", "resourcequotas", "quota",
			schema.GroupVersionResource{Group: "", Version: "v1", Resource: "resourcequotas"},
			"ResourceQuota", NSPlain, true, "v1", "v2"},
		{"storageclass", "storageclasses", "sc",
			schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"},
			"StorageClass", NSNone, true, "storage.k8s.io/v1", "storage.k8s.io/v1beta1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			row, ok := Lookup(tt.name)
			require.True(t, ok)
			for _, q := range []string{tt.plural, tt.short, tt.name} {
				got, ok := Lookup(q)
				require.True(t, ok, q)
				assert.Equal(t, row, got, q)
			}
			assert.Equal(t, tt.name, row.Key)
			assert.Equal(t, tt.name, row.Name)
			assert.Equal(t, []string{tt.plural, tt.short}, row.Aliases)
			assert.Equal(t, tt.gvr, row.GVR)
			assert.Equal(t, tt.k8sKind, row.K8sKind)
			assert.Equal(t, tt.policy, row.Policy)
			assert.Equal(t, Downstream, row.Scope)
			assert.True(t, row.Deletable)
			assert.Empty(t, row.NoDeleteReason)
			assert.Equal(t, tt.sensitive, row.Sensitive)
			assert.True(t, row.ServerTable)
			assert.Equal(t, binding, row.Binding)
			assert.Empty(t, row.WaitDefault)
			assert.False(t, row.Completable)

			byGVR, ok := ByGVR(tt.gvr)
			require.True(t, ok)
			assert.Equal(t, row, byGVR)
			byGVK, ok := LookupGVK(tt.apiVersion, tt.k8sKind)
			require.True(t, ok)
			assert.Equal(t, row, byGVK)
			_, ok = LookupGVK(tt.otherAPI, tt.k8sKind)
			assert.False(t, ok, "a manifest with another apiVersion of the kind finds no row")
		})
	}
}

// assertForbiddenLeftOut asserts the caller-scoped command for the value is
// left out: its field is empty.
func assertForbiddenLeftOut(t *testing.T, k Kind, verb, namespace, name string) {
	t.Helper()
	c, ok := ForbiddenCommand(k, verb, namespace, name)
	require.True(t, ok)
	assert.False(t, c.OK())
	assert.Empty(t, c.Field())
}

// TestValidateRedactReferencesNeedsBothWriteRefusals asserts a row that sets
// RedactReferences is refused unless it sets both NoPatchReason and
// NoCreateViaManifest, because the diff of a write preview does not hide
// references. A row that sets both is accepted. A row that sets ServerTable and
// RedactReferences is refused by the ServerTable check, which runs first
// (TestValidateRejectsServerTableMisuse).
func TestValidateRedactReferencesNeedsBothWriteRefusals(t *testing.T) {
	t.Parallel()
	base := Kind{Key: "a", Name: "foo", K8sKind: "Foo", Aliases: []string{"foos"}, RedactReferences: true}
	for _, tt := range []struct {
		name string
		edit func(k *Kind)
	}{
		{"neither reason", func(k *Kind) {}},
		{"only NoPatchReason", func(k *Kind) { k.NoPatchReason = "read-only" }},
		{"only NoCreateViaManifest", func(k *Kind) { k.NoCreateViaManifest = "not supported" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bad := base
			tt.edit(&bad)
			require.ErrorContains(t, validateKinds([]Kind{bad}), "sets RedactReferences and must also set NoPatchReason and NoCreateViaManifest")
		})
	}
	ok := base
	ok.NoPatchReason = "read-only"
	ok.NoCreateViaManifest = "not supported"
	require.NoError(t, validateKinds([]Kind{ok}))
}

// TestAuthConfigRow asserts the authconfig row: its names, scope, refusal
// texts, hidden paths, RedactReferences, columns and description.
func TestAuthConfigRow(t *testing.T) {
	t.Parallel()
	k, ok := Lookup("authconfigs")
	require.True(t, ok)
	byGVK, ok := LookupGVK("management.cattle.io/v3", "AuthConfig")
	require.True(t, ok)
	assert.Equal(t, k.Key, byGVK.Key)

	assert.Equal(t, "authconfig", k.Key)
	assert.Equal(t, "authconfig", k.Name)
	assert.Equal(t, []string{"authconfigs"}, k.Aliases)
	assert.Equal(t, schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "authconfigs"}, k.GVR)
	assert.Equal(t, "AuthConfig", k.K8sKind)
	assert.Equal(t, Management, k.Scope)
	assert.Equal(t, NSNone, k.Policy)
	assert.False(t, k.Deletable)
	assert.True(t, k.Sensitive)
	assert.True(t, k.Completable)
	assert.Empty(t, k.WaitDefault)
	assert.Equal(t, safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta}, k.Binding)

	assert.Equal(t, "auth configs are read-only in this CLI for now: Rancher configures, enables and disables auth providers through its own test-and-apply flow, and changing them from the CLI is planned separately", k.NoPatchReason)
	assert.Equal(t, "deleting an auth config is not supported: in Rancher, deleting one removes none of its users, tokens or role bindings, and disabling an auth provider is planned separately", k.NoDeleteReason)
	assert.Equal(t, "adding an auth config is not supported: Rancher adds and enables auth providers through its test-and-apply flow, which the CLI does not run yet", k.NoCreateViaManifest)
	assert.Equal(t, "Auth configs of the Rancher server. Before v2.16, Rancher keeps one for every auth provider it supports, most of them disabled and never configured. From v2.16 it keeps the local auth config and the ones that were configured. ENABLED says whether users can log in with an auth config; ACCESS-MODE and its Authorized Users & Groups say who (an empty mode means any user the provider authenticates). CLEANUP is Rancher's management.cattle.io/auth-provider-cleanup annotation: when a disabled auth config's annotation is unlocked, Rancher deletes its users, tokens and role bindings. Fields that hold credentials are never shown.", k.GetDesc)

	assert.Equal(t, [][]string{
		{"clientSecret"}, {"privateKey"}, {"applicationSecret"}, {"serviceAccountPassword"},
		{"spKey"}, {"oauthCredential"}, {"serviceAccountCredential"}, {"additionalClientIds"},
		{"openLdapConfig", "serviceAccountPassword"},
	}, k.RedactPaths)
	assert.True(t, k.RedactReferences)

	require.Len(t, k.Columns, 7)
	want := []struct {
		header string
		wide   bool
	}{
		{"TYPE", false}, {"ENABLED", false}, {"ACCESS-MODE", false}, {"ALLOWED-PRINCIPALS", false},
		{"CLEANUP", false}, {"DISPLAY NAME", true}, {"DESCRIPTION", true},
	}
	for i, w := range want {
		assert.Equal(t, w.header, k.Columns[i].Header, "column %d", i)
		assert.Equal(t, w.wide, k.Columns[i].Wide, w.header)
	}
	assert.Equal(t, []string{"type"}, k.Columns[0].Path)
	assert.Equal(t, []string{"enabled"}, k.Columns[1].Path)
	assert.True(t, k.Columns[1].Bool)
	assert.Equal(t, []string{"accessMode"}, k.Columns[2].Path)
	assert.Equal(t, []string{"allowedPrincipalIds"}, k.Columns[3].Count)
	assert.Equal(t, []string{"metadata", "annotations", "management.cattle.io/auth-provider-cleanup"}, k.Columns[4].Path)
	assert.Equal(t, []string{"displayName"}, k.Columns[5].Path)
	assert.Equal(t, []string{"metadata", "annotations", "field.cattle.io/description"}, k.Columns[6].Path)
}

// TestAuthConfigRedactionHidesReferences asserts the row's preview and read
// redactions both hide references.
func TestAuthConfigRedactionHidesReferences(t *testing.T) {
	t.Parallel()
	k, ok := ByKey("authconfig")
	require.True(t, ok)
	assert.True(t, k.Redaction().HidesReferences())
	assert.True(t, k.ReadRedaction().HidesReferences())
	other, ok := ByKey("user")
	require.True(t, ok)
	assert.False(t, other.ReadRedaction().HidesReferences())
}

// TestIsReference asserts a reference is a string that starts with exactly
// "cattle-global-data:".
func TestIsReference(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		v    any
		want bool
	}{
		{"exact prefix", "cattle-global-data:github-clientsecret", true},
		{"prefix only", "cattle-global-data:", true},
		{"empty", "", false},
		{"no colon", "cattle-global-data", false},
		{"other case", "Cattle-Global-Data:github-clientsecret", false},
		{"inside longer text", "see cattle-global-data:github-clientsecret", false},
		{"number", 42, false},
		{"bool", true, false},
		{"nil", nil, false},
		{"list", []any{"cattle-global-data:x"}, false},
		{"map", map[string]any{"a": "cattle-global-data:x"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, IsReference(tt.v))
		})
	}
}

// TestValidateRejectsDeleteDescOnARowWithoutTheGeneratedDelete asserts a
// row that sets DeleteDesc must be Deletable: the text describes the
// generated delete, which a row with Deletable false does not have.
func TestValidateRejectsDeleteDescOnARowWithoutTheGeneratedDelete(t *testing.T) {
	t.Parallel()
	row := func(deletable bool) []Kind {
		return []Kind{{Key: "foo", Name: "foo", K8sKind: "Foo", Aliases: []string{"foos"}, Deletable: deletable, DeleteDesc: "more about this delete"}}
	}
	require.NoError(t, validateKinds(row(true)))
	err := validateKinds(row(false))
	require.Error(t, err)
	require.ErrorContains(t, err, "DeleteDesc")
}

// TestValidateRejectsDeleteNeedsGetOnARowWithoutTheGeneratedDelete asserts
// a row that sets DeleteNeedsGet must be Deletable: the reason is about the
// generated delete, which a row with Deletable false does not have.
func TestValidateRejectsDeleteNeedsGetOnARowWithoutTheGeneratedDelete(t *testing.T) {
	t.Parallel()
	row := func(deletable bool) []Kind {
		return []Kind{{Key: "foo", Name: "foo", K8sKind: "Foo", Aliases: []string{"foos"}, Deletable: deletable, DeleteNeedsGet: "a reason"}}
	}
	require.NoError(t, validateKinds(row(true)))
	err := validateKinds(row(false))
	require.ErrorContains(t, err, "DeleteNeedsGet")
}

// TestDeleteDescRowsAreTheProjectAndManagementToken asserts which rows add
// text to their generated delete's description.
func TestDeleteDescRowsAreTheProjectAndManagementToken(t *testing.T) {
	t.Parallel()
	var got []string
	for _, k := range All() {
		if k.DeleteDesc != "" {
			got = append(got, k.Key)
		}
	}
	assert.ElementsMatch(t, []string{"project", "managementtoken"}, got)
}

// TestRBACRows asserts each Kubernetes RBAC row field by field: the names, the
// group and version, the scope, the ServerTable and Completable fields, the
// three refusal texts and the description, that the Columns,
// DeleteCascadeWarning, DeleteDesc, CreateWarning, WaitDefault, RedactPaths,
// SystemFilter, DefaultNamespace, ArgDesc and Handle fields are empty, and
// that the row resolves by alias, by kind and by GVR.
func TestRBACRows(t *testing.T) {
	t.Parallel()
	const group = "rbac.authorization.k8s.io"
	patchReason := "this CLI does not change Kubernetes RBAC objects yet; use kubectl with a kubeconfig from `" + clierr.Rancher + " kubeconfig --cluster <cluster>` (<cluster> is the cluster's id or name), or the grant and revoke commands for access that Rancher grants"
	deleteReason := "this CLI does not delete Kubernetes RBAC objects yet; use kubectl with a kubeconfig from `" + clierr.Rancher + " kubeconfig --cluster <cluster>` (<cluster> is the cluster's id or name), or the revoke command for access that Rancher grants"
	createReason := "this CLI does not create Kubernetes RBAC objects yet; use kubectl with a kubeconfig from `" + clierr.Rancher + " kubeconfig --cluster <cluster>` (<cluster> is the cluster's id or name), or the grant command for access that Rancher grants"
	getDesc := "Kubernetes RBAC objects of one cluster. Rancher creates many of them from its role templates, global roles and Rancher role bindings. This CLI shows them and does not change them yet. To see what a subject may do, run `" + clierr.Rancher + " access can <verb> <resource> --cluster <cluster> --as <subject>`."

	tests := []struct {
		key      string
		plural   string
		k8sKind  string
		policy   NamespacePolicy
		resource string
	}{
		{"role", "roles", "Role", NSPlain, "roles"},
		{"rolebinding", "rolebindings", "RoleBinding", NSPlain, "rolebindings"},
		{"clusterrole", "clusterroles", "ClusterRole", NSNone, "clusterroles"},
		{"clusterrolebinding", "clusterrolebindings", "ClusterRoleBinding", NSNone, "clusterrolebindings"},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			t.Parallel()
			k, ok := ByKey(tt.key)
			require.True(t, ok)
			assert.Equal(t, tt.key, k.Key)
			assert.Equal(t, tt.key, k.Name)
			assert.Equal(t, []string{tt.plural}, k.Aliases)
			assert.Equal(t, schema.GroupVersionResource{Group: group, Version: "v1", Resource: tt.resource}, k.GVR)
			assert.Equal(t, tt.policy, k.Policy)
			assert.Equal(t, tt.k8sKind, k.K8sKind)
			assert.Equal(t, Downstream, k.Scope)
			assert.True(t, k.ServerTable)
			assert.True(t, k.Sensitive)
			assert.True(t, k.Completable)
			assert.Equal(t, safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta}, k.Binding)
			assert.False(t, k.Deletable)
			assert.Empty(t, k.Columns)
			assert.Empty(t, k.DeleteCascadeWarning)
			assert.Empty(t, k.DeleteDesc)
			assert.Empty(t, k.CreateWarning)
			assert.Empty(t, k.WaitDefault)
			assert.Empty(t, k.ArgDesc)
			assert.Empty(t, k.Handle)
			assert.Empty(t, k.RedactPaths)
			assert.Empty(t, k.SystemFilter)
			assert.Empty(t, k.DefaultNamespace)
			assert.Equal(t, patchReason, k.NoPatchReason)
			assert.Equal(t, deleteReason, k.NoDeleteReason)
			assert.Equal(t, createReason, k.NoCreateViaManifest)
			assert.Equal(t, getDesc, k.GetDesc)

			byAlias, ok := Lookup(tt.plural)
			require.True(t, ok)
			assert.Equal(t, tt.key, byAlias.Key)
			byKind, ok := LookupGVK(group+"/v1", tt.k8sKind)
			require.True(t, ok)
			assert.Equal(t, tt.key, byKind.Key)
			byGVR, ok := ByGVR(k.GVR)
			require.True(t, ok)
			assert.Equal(t, tt.key, byGVR.Key)
		})
	}
}

// TestEveryRBACRowIsReadOnly asserts every Kubernetes RBAC row is read-only:
// patch, create and delete are refused, and the set of such rows is the four
// known ones.
func TestEveryRBACRowIsReadOnly(t *testing.T) {
	t.Parallel()
	var keys []string
	for _, k := range All() {
		if k.GVR.Group != "rbac.authorization.k8s.io" {
			continue
		}
		keys = append(keys, k.Key)
		assert.False(t, k.Deletable, k.Key)
		assert.NotEmpty(t, k.NoDeleteReason, k.Key)
		assert.NotEmpty(t, k.NoPatchReason, k.Key)
		assert.NotEmpty(t, k.NoCreateViaManifest, k.Key)
	}
	slices.Sort(keys)
	assert.Equal(t, []string{"clusterrole", "clusterrolebinding", "role", "rolebinding"}, keys)
}
