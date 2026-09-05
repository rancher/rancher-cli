package access

import (
	"errors"
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
		{"group principal with type user", "github_org://eng", "user", "", "contradicts principal"},
		{"user principal with type group", "local://u-abc12", "group", "", "contradicts principal"},
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
		assert.Equal(t, "rancher get users", ce.Hint)
	}
}
