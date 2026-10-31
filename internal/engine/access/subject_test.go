package access

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func userObj(id, username, display string, principals ...string) *unstructured.Unstructured {
	ps := make([]any, 0, len(principals))
	for _, p := range principals {
		ps = append(ps, p)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "User",
		"metadata": map[string]any{"name": id}, "username": username, "displayName": display, "principalIds": ps,
	}}
}

func userAttrObj(id string, groups ...string) *unstructured.Unstructured {
	items := make([]any, 0, len(groups))
	for _, g := range groups {
		items = append(items, map[string]any{"metadata": map[string]any{"name": g}, "displayName": g})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "UserAttribute",
		"metadata":        map[string]any{"name": id},
		"GroupPrincipals": map[string]any{"okta": map[string]any{"Items": items}},
	}}
}

// TestParseSubjectKinds asserts the closed kind set and the validation
// rules for --type.
func TestParseSubjectKinds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, as, typ string
		want          SubjectKind
		errSubstr     string
	}{
		{"bare user", "alice", "", KindUser, ""},
		{"user id", "u-abc12", "", KindUser, ""},
		{"user principal", "github_user://12", "", KindUser, ""},
		{"group by type", "eng", "group", KindGroup, ""},
		{"group principal", "github_org://eng", "", KindGroup, ""},
		{"service account", "system:serviceaccount:team-a:ci", "", KindServiceAccount, ""},
		{"anonymous", "system:anonymous", "", KindUser, ""},
		{"type without as", "", "group", "", "--type qualifies a named subject"},
		{"bad type", "alice", "robot", "", "--type must be user or group"},
		{"bad sa", "system:serviceaccount:ci", "", "", "system:serviceaccount:<namespace>:<name>"},
		{"group principal with type user", "github_org://eng", "user", "", "does not match principal"},
		{"user principal with type group", "local://u-abc12", "group", "", "does not match principal"},
		{"unknown scheme needs a type", "custom://subject", "", "", "cannot infer subject type"},
		{"unknown scheme as user", "custom://subject", "user", KindUser, ""},
		{"unknown scheme as group", "custom://subject", "group", KindGroup, ""},
		{"github team is a group", "github_team://core", "", KindGroup, ""},
		{"sa with type user", "system:serviceaccount:team-a:ci", "user", "", "neither a user nor a group"},
		{"sa with type group", "system:serviceaccount:team-a:ci", "group", "", "neither a user nor a group"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseSubject(c.as, c.typ)
			if c.errSubstr != "" {
				require.ErrorContains(t, err, c.errSubstr)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, got.Kind)
			assert.Equal(t, c.as, got.Raw)
		})
	}
}

// TestResolveSubjectRancherUser asserts id, username, and principal
// lookups converge on the same user with UserAttribute groups.
func TestResolveSubjectRancherUser(t *testing.T) {
	t.Parallel()
	local := &fakeClient{
		Objects: map[string]*unstructured.Unstructured{
			"u-abc12": userObj("u-abc12", "alice", "Alice A", "local://u-abc12", "github_user://12"),
		},
		Lists: map[string]*unstructured.UnstructuredList{
			"users||": {Items: []unstructured.Unstructured{*userObj("u-abc12", "alice", "Alice A", "local://u-abc12", "github_user://12")}},
		},
	}
	local.Objects["u-abc12-attr"] = userAttrObj("u-abc12", "okta_group://eng")
	for _, as := range []string{"u-abc12", "alice", "github_user://12"} {
		t.Run(as, func(t *testing.T) {
			t.Parallel()
			p, err := ParseSubject(as, "")
			require.NoError(t, err)
			got, err := ResolveSubject(t.Context(), local, p)
			require.NoError(t, err)
			assert.True(t, got.IsRancherUser)
			assert.Equal(t, AccessSubject{Kind: KindUser, Name: "u-abc12", ID: "u-abc12", DisplayName: "Alice A"}, got.Subject)
			assert.Equal(t, GroupsRancherUserAttribute, got.GroupsFrom)
			assert.ElementsMatch(t, []string{"okta_group://eng", "system:authenticated", "system:cattle:authenticated"}, got.Groups)
		})
	}
}

// TestResolveSubjectSpecialIdentities asserts anonymous, service account,
// other system users, and groups need no server round trip.
func TestResolveSubjectSpecialIdentities(t *testing.T) {
	t.Parallel()
	local := &fakeClient{GetErr: clierr.New(clierr.CodeGeneric, "must not be called")}
	cases := []struct {
		as, typ    string
		kind       SubjectKind
		groupsFrom GroupsFrom
		groups     []string
	}{
		{"system:anonymous", "", KindUser, GroupsAnonymousIdentity, []string{"system:unauthenticated"}},
		{"system:kube-controller-manager", "", KindUser, GroupsAuthenticatedOnly, []string{"system:authenticated"}},
		{"system:serviceaccount:team-a:ci", "", KindServiceAccount, GroupsServiceAccountIdentity, []string{"system:serviceaccounts", "system:serviceaccounts:team-a", "system:authenticated"}},
		{"github_org://eng", "", KindGroup, GroupsGroupOnly, []string{"github_org://eng"}},
		{"eng", "group", KindGroup, GroupsGroupOnly, []string{"eng"}},
	}
	for _, c := range cases {
		t.Run(c.as, func(t *testing.T) {
			t.Parallel()
			p, err := ParseSubject(c.as, c.typ)
			require.NoError(t, err)
			got, err := ResolveSubject(t.Context(), local, p)
			require.NoError(t, err)
			assert.False(t, got.IsRancherUser)
			assert.Equal(t, c.kind, got.Subject.Kind)
			assert.Equal(t, c.as, got.Subject.Name)
			assert.Equal(t, c.groupsFrom, got.GroupsFrom)
			assert.Equal(t, c.groups, got.Groups)
		})
	}
}

// TestResolveSubjectUnknownUserIsNotFound asserts an unknown user or
// external principal is NOT_FOUND with a paste-valid hint, never
// provisioned or guessed.
func TestResolveSubjectUnknownUserIsNotFound(t *testing.T) {
	t.Parallel()
	local := &fakeClient{Lists: map[string]*unstructured.UnstructuredList{"users||": {}}}
	for _, as := range []string{"nobody", "github_user://404"} {
		p, err := ParseSubject(as, "")
		require.NoError(t, err)
		_, err = ResolveSubject(t.Context(), local, p)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok)
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
		assert.Equal(t, clierr.Rancher+" get users", ce.Hint)
	}
}

// usersClient answers the user LIST with the given users and the user GET
// with not found.
func usersClient(users ...*unstructured.Unstructured) *fakeClient {
	list := &unstructured.UnstructuredList{}
	for _, u := range users {
		list.Items = append(list.Items, *u)
	}
	return &fakeClient{Lists: map[string]*unstructured.UnstructuredList{"users||": list}}
}

// numberedUsers returns n users u-01..u-NN, each with username and display
// name set from name and a principal of its own.
func numberedUsers(n int, name string) []*unstructured.Unstructured {
	users := make([]*unstructured.Unstructured, 0, n)
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("u-%02d", i)
		users = append(users, userObj(id, name, "", "local://"+id))
	}
	return users
}

func candidateIDs(cands []clierr.Candidate) []string {
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.ID)
	}
	return ids
}

func findUserError(t *testing.T, local *fakeClient, raw string) *clierr.Error {
	t.Helper()
	_, err := findRancherUser(t.Context(), local, raw)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "want a *clierr.Error, got %v", err)
	assert.Equal(t, clierr.Rancher+" get users", ce.Hint)
	return ce
}

// TestFindRancherUserBareNameMissListsNearMatches asserts a bare name that
// matches no user lists the users it near-matches, by user id, and leaves
// system users out.
func TestFindRancherUserBareNameMissListsNearMatches(t *testing.T) {
	t.Parallel()
	local := usersClient(
		userObj("u-1", "alice", "Alice A", "local://u-1"),
		userObj("u-2", "alicia", "", "local://u-2"),
		userObj("u-3", "bob", "Bob B", "local://u-3"),
		userObj("u-sys", "alice-system", "", "system://alice-system"),
	)
	ce := findUserError(t, local, "alic")
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, []clierr.Candidate{{ID: "u-1", Name: "Alice A"}, {ID: "u-2", Name: "alicia"}}, ce.Candidates)
}

// TestFindRancherUserBareNameMissWholePoolIsCutAndCounted asserts a bare
// name with no near match lists the first 10 of 12 users in list order and
// counts all twelve.
func TestFindRancherUserBareNameMissWholePoolIsCutAndCounted(t *testing.T) {
	t.Parallel()
	local := usersClient(numberedUsers(12, "member")...)
	ce := findUserError(t, local, "zzz")
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, []string{"u-01", "u-02", "u-03", "u-04", "u-05", "u-06", "u-07", "u-08", "u-09", "u-10"}, candidateIDs(ce.Candidates))
	assert.Equal(t, 12, ce.CandidatesTotal)
}

// TestFindRancherUserBareNameMissNearMatchesAreCutAndCounted asserts a bare
// name that 15 users contain lists the first 10 of them in list order and
// counts all fifteen.
func TestFindRancherUserBareNameMissNearMatchesAreCutAndCounted(t *testing.T) {
	t.Parallel()
	users := numberedUsers(15, "member")
	users = append(users, userObj("u-other", "other", "", "local://u-other"))
	ce := findUserError(t, usersClient(users...), "memb")
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, []string{"u-01", "u-02", "u-03", "u-04", "u-05", "u-06", "u-07", "u-08", "u-09", "u-10"}, candidateIDs(ce.Candidates))
	assert.Equal(t, 15, ce.CandidatesTotal)
}

// TestFindRancherUserBareNameMissAtTheLimitHasNoTotal asserts a pool of
// exactly 10 users, and exactly 10 near matches, are listed whole with no
// total.
func TestFindRancherUserBareNameMissAtTheLimitHasNoTotal(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{"whole pool": "zzz", "near matches": "memb"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ce := findUserError(t, usersClient(numberedUsers(10, "member")...), raw)
			assert.Len(t, ce.Candidates, 10)
			assert.Zero(t, ce.CandidatesTotal)
		})
	}
}

// TestResolveSubjectBareNameMissListsCandidates asserts the candidates reach
// the caller of ResolveSubject, the path every --as value takes.
func TestResolveSubjectBareNameMissListsCandidates(t *testing.T) {
	t.Parallel()
	local := usersClient(userObj("u-1", "alice", "Alice A", "local://u-1"))
	p, err := ParseSubject("alic", "")
	require.NoError(t, err)
	_, err = ResolveSubject(t.Context(), local, p)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, []clierr.Candidate{{ID: "u-1", Name: "Alice A"}}, ce.Candidates)
	assert.Zero(t, ce.CandidatesTotal)
}

// TestFindRancherUserSharedUsernameListsEveryMatch asserts a username that
// 12 users share is AMBIGUOUS_SELECTION and lists all 12 by user id.
func TestFindRancherUserSharedUsernameListsEveryMatch(t *testing.T) {
	t.Parallel()
	local := usersClient(numberedUsers(12, "dup")...)
	ce := findUserError(t, local, "dup")
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	require.ErrorContains(t, ce, "12 users match dup")
	require.Len(t, ce.Candidates, 12)
	for i, c := range ce.Candidates {
		assert.Equal(t, fmt.Sprintf("u-%02d", i+1), c.ID)
		assert.Equal(t, "dup", c.Name)
	}
}

// TestFindRancherUserSharedPrincipalListsEveryMatch asserts a principal that
// several users hold is AMBIGUOUS_SELECTION and lists every one of them.
func TestFindRancherUserSharedPrincipalListsEveryMatch(t *testing.T) {
	t.Parallel()
	local := usersClient(
		userObj("u-1", "alice", "Alice A", "local://u-1", "github_user://7"),
		userObj("u-2", "alice2", "", "local://u-2", "github_user://7"),
		userObj("u-3", "bob", "Bob B", "local://u-3"),
	)
	ce := findUserError(t, local, "github_user://7")
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Equal(t, []clierr.Candidate{{ID: "u-1", Name: "Alice A"}, {ID: "u-2", Name: "alice2"}}, ce.Candidates)
}

// TestFindRancherUserAmbiguityListsSystemUsers asserts a match that is a
// system user is listed: its id resolves to exactly that user.
func TestFindRancherUserAmbiguityListsSystemUsers(t *testing.T) {
	t.Parallel()
	local := usersClient(
		userObj("u-1", "dup", "", "local://u-1"),
		userObj("u-sys", "dup", "", "system://dup"),
	)
	ce := findUserError(t, local, "dup")
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Equal(t, []string{"u-1", "u-sys"}, candidateIDs(ce.Candidates))
}

// TestFindRancherUserPrincipalMissHasNoCandidates asserts a principal that
// no user holds is NOT_FOUND with no candidates: it has no name to match.
func TestFindRancherUserPrincipalMissHasNoCandidates(t *testing.T) {
	t.Parallel()
	local := usersClient(numberedUsers(3, "member")...)
	ce := findUserError(t, local, "github_user://404")
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Empty(t, ce.Candidates)
}

// TestFindRancherUserSystemValueMissHasNoCandidates asserts a system: value
// that no user holds is NOT_FOUND with no candidates.
func TestFindRancherUserSystemValueMissHasNoCandidates(t *testing.T) {
	t.Parallel()
	local := usersClient(numberedUsers(3, "member")...)
	ce := findUserError(t, local, "system:nobody")
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Empty(t, ce.Candidates)
}
