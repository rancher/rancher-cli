package complete

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
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
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		_, _ = w.Write([]byte(`{"items":[
			{"metadata":{"name":"u-alice"},"username":"alice","principalIds":["local://u-alice"]},
			{"metadata":{"name":"u-bob"},"username":"bob","principalIds":["local://u-bob"]},
			{"metadata":{"name":"u-bob"},"username":"bob","principalIds":["local://u-bob"]},
			{"metadata":{"name":"u-sys"},"username":"system-svc","principalIds":["system://u-sys"]},
			{"metadata":{"name":"u-badname"},"username":"bad name!","principalIds":["local://u-badname"]},
			{"metadata":{"name":"u-nopids"},"username":"nopids"},
			{"metadata":{"name":"sameid"},"username":"sameid","principalIds":["local://sameid"]}
		]}`))
	}))
	t.Cleanup(srv.Close)
	got := UserIDSource().Complete(t.Context(), completion.NewConnection(srv.Client(), "tok", srv.URL, false, 1), completion.Request{})
	assert.Equal(t, []string{"u-alice", "u-bob", "u-bob", "u-badname", "u-nopids", "sameid"}, got.Names)
	assert.Equal(t, []string{"/k8s/clusters/local/apis/management.cattle.io/v3/users"}, paths)

	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	t.Cleanup(refused.Close)
	assert.Empty(t, UserIDSource().Complete(t.Context(), completion.NewConnection(refused.Client(), "tok", refused.URL, false, 1), completion.Request{}).Names)
}

// TestIsSystem asserts the kind table's rule as applied: any principal
// with the prefix marks the user; a missing or malformed field, or a kind
// without a filter, does not.
func TestIsSystem(t *testing.T) {
	t.Parallel()
	k, ok := kinds.Lookup("user")
	require.True(t, ok)
	require.NotNil(t, k.SystemFilter)
	assert.True(t, isSystem(map[string]any{"principalIds": []any{"local://x", "system://x"}}, k.SystemFilter))
	assert.False(t, isSystem(map[string]any{"principalIds": []any{"local://x"}}, k.SystemFilter))
	assert.False(t, isSystem(map[string]any{"principalIds": "system://x"}, k.SystemFilter))
	assert.False(t, isSystem(map[string]any{}, k.SystemFilter))
	assert.False(t, isSystem(map[string]any{"principalIds": []any{"system://x"}}, nil))
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
		assert.Equal(t, engine.IsSystemPrincipal(pid), isSystem(item, k.SystemFilter), pid)
	}
	bare := "system:legacy"
	assert.True(t, engine.IsSystemPrincipal(bare))
	assert.False(t, isSystem(map[string]any{"principalIds": []any{bare}}, k.SystemFilter),
		"a bare system: principal is a shape no User object carries; the engine's typed-subject rule treats it as system, the kind table's filter does not")
}
