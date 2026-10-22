package rbac

import (
	"errors"
	"fmt"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The seven-case grammar fixture set:
// local id, known external user principal, known group principal
// (rejected), unknown prefix (rejected), unowned external (NOT_FOUND),
// ambiguous display name, system principal (resolves).

func TestResolveInspectUserLocalID(t *testing.T) {
	c := &fakeClient{users: []map[string]any{userObj("u-abc", "Jane", "jane", "local://u-abc")}}
	got, err := ResolveInspectUser(t.Context(), fakeFactory{c: c}, "u-abc")
	require.NoError(t, err)
	assert.Equal(t, "u-abc", got.UserID)
	assert.Equal(t, "local://u-abc", got.Principal)
	assert.Zero(t, c.lists, "exact id hit must not list")
}

func TestResolveInspectUserKnownExternalPrincipal(t *testing.T) {
	principal := "okta_user://alice"
	c := &fakeClient{users: []map[string]any{userObj("u-alice", "Alice", "alice", principal)}}
	got, err := ResolveInspectUser(t.Context(), fakeFactory{c: c}, principal)
	require.NoError(t, err)
	assert.Equal(t, "u-alice", got.UserID)
	assert.Equal(t, principal, got.Principal)
}

func TestResolveInspectUserGroupPrincipalRejected(t *testing.T) {
	c := &fakeClient{}
	_, err := ResolveInspectUser(t.Context(), fakeFactory{c: c}, "okta_group://engineering")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "grant/revoke")
	assert.Zero(t, c.lists, "a group principal must never reach the ownership scan")
}

func TestResolveInspectUserUnknownPrefixRejected(t *testing.T) {
	c := &fakeClient{}
	_, err := ResolveInspectUser(t.Context(), fakeFactory{c: c}, "mystery://x")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.NotContains(t, ce.Message, "--type", "inspect has no --type flag to suggest")
	assert.Zero(t, c.lists)
}

func TestResolveInspectUserUnownedExternalNotFound(t *testing.T) {
	c := &fakeClient{users: []map[string]any{userObj("u-someone", "Someone", "someone", "okta_user://someone")}}
	_, err := ResolveInspectUser(t.Context(), fakeFactory{c: c}, "okta_user://nobody")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
}

func TestResolveInspectUserAmbiguousDisplayName(t *testing.T) {
	c := &fakeClient{users: []map[string]any{
		userObj("u-1", "Jane", "jane1", "okta_user://1"),
		userObj("u-2", "Jane", "jane2", "okta_user://2"),
	}}
	_, err := ResolveInspectUser(t.Context(), fakeFactory{c: c}, "Jane")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Len(t, ce.Candidates, 2)
}

func TestResolveInspectUserSystemPrincipalResolves(t *testing.T) {
	c := &fakeClient{users: []map[string]any{
		userObj("u-sys", "System account for Cluster c-x", "system-c-x", "system://c-x"),
	}}
	got, err := ResolveInspectUser(t.Context(), fakeFactory{c: c}, "system://c-x")
	require.NoError(t, err)
	assert.Equal(t, "u-sys", got.UserID)
	assert.Equal(t, "system://c-x", got.Principal)
}

func TestResolveInspectUserSystemUserByIDResolves(t *testing.T) {
	// Divergence from ResolveSubject: system accounts resolve by id, never
	// terminal.
	c := &fakeClient{users: []map[string]any{
		userObj("u-sys", "Deploy Bot", "bot", "system://c-x"),
	}}
	got, err := ResolveInspectUser(t.Context(), fakeFactory{c: c}, "u-sys")
	require.NoError(t, err)
	assert.Equal(t, "u-sys", got.UserID)
}

func TestResolveInspectUserSystemUserInDisplayNameScanResolves(t *testing.T) {
	// Divergence from ResolveSubject: system accounts are not excluded from
	// the name-match scan (named access is intent).
	c := &fakeClient{users: []map[string]any{
		userObj("u-sys", "Deploy Bot", "bot", "system://c-x"),
	}}
	got, err := ResolveInspectUser(t.Context(), fakeFactory{c: c}, "Deploy Bot")
	require.NoError(t, err)
	assert.Equal(t, "u-sys", got.UserID)
}

func TestResolveInspectUserDuplicateOwnerAmbiguous(t *testing.T) {
	principal := "okta_user://shared"
	c := &fakeClient{users: []map[string]any{
		userObj("u-1", "One", "one", principal),
		userObj("u-2", "Two", "two", principal),
	}}
	_, err := ResolveInspectUser(t.Context(), fakeFactory{c: c}, principal)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Len(t, ce.Candidates, 2)
}

func TestResolveInspectUserOccupiedNonOwnerNotFound(t *testing.T) {
	// A user object happens to exist at the deterministic predicted id for
	// this principal, but does not itself own the principal (its
	// principalIds carry a different value). Ownership is proven by the
	// scan alone, never assumed from the occupied id: NOT_FOUND, not a
	// false resolve.
	principal := "okta_user://alice"
	predictedID := ProvisionedUserID(principal)
	c := &fakeClient{users: []map[string]any{
		userObj(predictedID, "Someone Else", "someoneelse", "okta_user://renamed"),
	}}
	_, err := ResolveInspectUser(t.Context(), fakeFactory{c: c}, principal)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
}

func TestResolveInspectUserMalformedEmptyPrincipalIDsResolves(t *testing.T) {
	// A malformed user (empty principalIds) must be inspection-safe, never
	// a panic or an error: the row-principal fallback is
	// local:// -> first principalIds element -> user id.
	c := &fakeClient{users: []map[string]any{userObj("u-broken", "Broken", "broken")}}
	got, err := ResolveInspectUser(t.Context(), fakeFactory{c: c}, "u-broken")
	require.NoError(t, err)
	assert.Equal(t, "u-broken", got.UserID)
	assert.Equal(t, "u-broken", got.Principal, "empty principalIds falls back to the user id")
	assert.Empty(t, got.PrincipalIDs)
}

func TestResolveInspectUserEmptyPrincipalRemainderRejected(t *testing.T) {
	_, err := ResolveInspectUser(t.Context(), fakeFactory{c: &fakeClient{}}, "local://")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func inspectUserNotFound(t *testing.T, users []map[string]any, bare string) *clierr.Error {
	t.Helper()
	_, err := ResolveInspectUser(t.Context(), fakeFactory{c: &fakeClient{users: users}}, bare)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "want a *clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	return ce
}

// TestResolveInspectUserBareNotFoundWholePoolIsCutInListOrder asserts a bare
// name that near-matches none of twelve users lists the first 10 in list
// order and counts all twelve.
func TestResolveInspectUserBareNotFoundWholePoolIsCutInListOrder(t *testing.T) {
	var users []map[string]any
	for i := 1; i <= 12; i++ {
		id := fmt.Sprintf("u-%02d", i)
		users = append(users, userObj(id, "", "member", "local://"+id))
	}
	ce := inspectUserNotFound(t, users, "lx-no-such-user")
	assert.Equal(t, []string{"u-01", "u-02", "u-03", "u-04", "u-05", "u-06", "u-07", "u-08", "u-09", "u-10"}, candidateIDs(ce))
	assert.Equal(t, 12, ce.CandidatesTotal)
}

// TestResolveInspectUserBareNotFoundNearMatchesAreCutAndCounted asserts
// fifteen users that contain the typed name list the first 10 in list order
// and count all fifteen.
func TestResolveInspectUserBareNotFoundNearMatchesAreCutAndCounted(t *testing.T) {
	var users []map[string]any
	for i := 1; i <= 15; i++ {
		id := fmt.Sprintf("u-jan%02d", i)
		users = append(users, userObj(id, fmt.Sprintf("Jan %d", i), fmt.Sprintf("jan%d", i), "local://"+id))
	}
	users = append(users, userObj("u-zzz", "Zed", "zed", "local://u-zzz"))
	ce := inspectUserNotFound(t, users, "jan")
	require.Len(t, ce.Candidates, CandidateLimit)
	assert.Equal(t, "u-jan01", ce.Candidates[0].ID)
	assert.Equal(t, 15, ce.CandidatesTotal)
}

// TestResolveInspectUserBareNotFoundAtTheLimitHasNoTotal asserts a pool of
// exactly 10 users is listed whole with no total.
func TestResolveInspectUserBareNotFoundAtTheLimitHasNoTotal(t *testing.T) {
	var users []map[string]any
	for i := range CandidateLimit {
		id := fmt.Sprintf("u-%02d", i)
		users = append(users, userObj(id, "", "member", "local://"+id))
	}
	ce := inspectUserNotFound(t, users, "lx-no-such-user")
	assert.Len(t, ce.Candidates, CandidateLimit)
	assert.Zero(t, ce.CandidatesTotal)
}
