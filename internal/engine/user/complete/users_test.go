package complete

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUserIDSourceClaim asserts the source claims any site with a
// management need, no cluster need and one listing.
func TestUserIDSourceClaim(t *testing.T) {
	t.Parallel()
	reqs, ok := UserIDSource().Claim(completion.Request{Index: 0, Cluster: "prod cluster"})
	require.True(t, ok)
	assert.Equal(t, completion.ManagementListing(), reqs)
}

// TestUserIDSourceCompletes asserts one listing of the management Users
// answers the ids of every non-system user in list order, a repeated row
// included (the surface dedupes), usernames never offered, and a refused
// listing answering nothing.
func TestUserIDSourceCompletes(t *testing.T) {
	t.Parallel()
	s := completiontest.NewFixed(t, `{"items":[
		{"metadata":{"name":"u-alice"},"username":"alice","principalIds":["local://u-alice"]},
		{"metadata":{"name":"u-bob"},"username":"bob","principalIds":["local://u-bob"]},
		{"metadata":{"name":"u-bob"},"username":"bob","principalIds":["local://u-bob"]},
		{"metadata":{"name":"u-sys"},"username":"system-svc","principalIds":["system://u-sys"]},
		{"metadata":{"name":"u-badname"},"username":"bad name!","principalIds":["local://u-badname"]},
		{"metadata":{"name":"u-nopids"},"username":"nopids"},
		{"metadata":{"name":"sameid"},"username":"sameid","principalIds":["local://sameid"]}
	]}`)
	got := UserIDSource().Complete(t.Context(), s.Conn(1), completion.Request{})
	assert.Equal(t, []string{"u-alice", "u-bob", "u-bob", "u-badname", "u-nopids", "sameid"}, got.Names)
	assert.Equal(t, []string{"/k8s/clusters/local/apis/management.cattle.io/v3/users"}, s.Requested())

	refused := completiontest.NewStatus(t, 401)
	assert.Empty(t, UserIDSource().Complete(t.Context(), refused.Conn(1), completion.Request{}).Names)
}

// TestSystemRuleAgreesWithTheEnginePredicateOnRancherPrincipals asserts
// the kind table's system-account prefix ("system://", the form Rancher
// stamps on system users' principal ids) and the engine's broader
// principal rule ("system:", which grant and revoke apply to a typed
// subject) decide alike for every principal shape a User object carries,
// and names the one shape they differ on: "system:" without "//", which
// no User object carries.
func TestSystemRuleAgreesWithTheEnginePredicateOnRancherPrincipals(t *testing.T) {
	t.Parallel()
	k, ok := kinds.Lookup("user")
	require.True(t, ok)
	assert.Equal(t, "system://", k.SystemFilter.Prefix)
	for _, pid := range []string{"system://u-sys", "system://cattle-agent", "local://u-alice", "github_user://12345", "okta_user://jane"} {
		item := map[string]any{"principalIds": []any{pid}}
		assert.Equal(t, engine.IsSystemPrincipal(pid), k.SystemFilter.Matches(item), pid)
	}
	bare := "system:legacy"
	assert.True(t, engine.IsSystemPrincipal(bare))
	assert.False(t, k.SystemFilter.Matches(map[string]any{"principalIds": []any{bare}}),
		"a bare system: principal is a shape no User object carries; the engine's typed-subject rule treats it as system, the kind table's filter does not")
}

// TestUserIDSourceDescribesEveryId asserts each id carries its username
// and display name as the raw description: both (display name in
// parentheses), either alone, none, the same text once, a username equal
// to the id, and text with control characters kept raw for the surface to
// sanitize. Only ids are values: a username or display name never is, and
// spec.description-style fields are not read.
func TestUserIDSourceDescribesEveryId(t *testing.T) {
	t.Parallel()
	s := completiontest.NewFixed(t, `{"items":[
		{"metadata":{"name":"u-both"},"username":"alice","displayName":"Alice Smith","description":"ignored"},
		{"metadata":{"name":"u-user"},"username":"bob"},
		{"metadata":{"name":"u-display"},"displayName":"Carol"},
		{"metadata":{"name":"u-none"}},
		{"metadata":{"name":"u-same"},"username":"dave","displayName":"dave"},
		{"metadata":{"name":"sameid"},"username":"sameid","displayName":"Same Id"},
		{"metadata":{"name":"u-hostile"},"username":"x","displayName":"a\nb\u001b[31m"},
		{"metadata":{"name":"u-sys"},"username":"system-svc","principalIds":["system://u-sys"]}
	]}`)
	got := UserIDSource().Complete(t.Context(), s.Conn(1), completion.Request{})
	assert.Equal(t, []string{"u-both", "u-user", "u-display", "u-none", "u-same", "sameid", "u-hostile"}, got.Names)
	assert.Equal(t, map[string]string{
		"u-both":    "alice (Alice Smith)",
		"u-user":    "bob",
		"u-display": "Carol",
		"u-same":    "dave",
		"sameid":    "sameid (Same Id)",
		"u-hostile": "x (a\nb\u001b[31m)",
	}, got.Descriptions)
}

// TestUserIDSourceDescribesWithTheLookupFieldsOnly asserts the description
// is built from the fields of the user row's Handle.Lookup and from no
// other field the object carries.
func TestUserIDSourceDescribesWithTheLookupFieldsOnly(t *testing.T) {
	t.Parallel()
	s := completiontest.NewFixed(t, `{"items":[
		{"metadata":{"name":"u-1"},"username":"alice","displayName":"Alice Smith","description":"decoy description","spec":{"name":"decoy spec","displayName":"decoy display"},"status":{"displayName":"decoy status"}},
		{"metadata":{"name":"u-2"},"description":"decoy description","spec":{"displayName":"decoy display"}}
	]}`)
	got := UserIDSource().Complete(t.Context(), s.Conn(1), completion.Request{})
	assert.Equal(t, map[string]string{"u-1": "alice (Alice Smith)"}, got.Descriptions)
}
