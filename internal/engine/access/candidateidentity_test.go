package access

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestResolveCandidateFixedTablesNeedNoServer asserts groups, service
// accounts, system:anonymous, and other system: usernames resolve from the
// identity tables alone: the management client is never read (it would
// fail if it were).
func TestResolveCandidateFixedTablesNeedNoServer(t *testing.T) {
	t.Parallel()
	local := &fakeClient{GetErr: errors.New("management cluster must not be read")}
	cases := []struct {
		name string
		c    candidate
		want candidateIdentity
	}{
		{"group", candidate{Kind: KindGroup, Name: "github_org://eng"},
			candidateIdentity{Subject: AccessSubject{Kind: KindGroup, Name: "github_org://eng"}, GroupsFrom: GroupsGroupOnly, Groups: []string{"github_org://eng"}}},
		{"masters", candidate{Kind: KindGroup, Name: "system:masters", Fixed: true},
			candidateIdentity{Subject: AccessSubject{Kind: KindGroup, Name: "system:masters"}, GroupsFrom: GroupsGroupOnly, Groups: []string{"system:masters"}}},
		{"service account", candidate{Kind: KindServiceAccount, Name: "system:serviceaccount:team-a:ci"},
			candidateIdentity{Subject: AccessSubject{Kind: KindServiceAccount, Name: "system:serviceaccount:team-a:ci"}, GroupsFrom: GroupsServiceAccountIdentity,
				Groups: []string{"system:serviceaccounts", "system:serviceaccounts:team-a", "system:authenticated"}}},
		{"anonymous", candidate{Kind: KindUser, Name: "system:anonymous"},
			candidateIdentity{Subject: AccessSubject{Kind: KindUser, Name: "system:anonymous"}, GroupsFrom: GroupsAnonymousIdentity, Groups: []string{"system:unauthenticated"}}},
		{"component user", candidate{Kind: KindUser, Name: "system:kube-scheduler"},
			candidateIdentity{Subject: AccessSubject{Kind: KindUser, Name: "system:kube-scheduler"}, GroupsFrom: GroupsAuthenticatedOnly, Groups: []string{"system:authenticated"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveCandidate(t.Context(), local, attrsFor(local), c.c)
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

// TestResolveCandidateRancherUserByExactGetOnly asserts a binding username
// that is a Rancher user id gets the display name and the UserAttribute
// groups, while a binding username equal to a Rancher user's username (not
// its id) is NOT matched: the exact GET misses and the arbitrary-user rule
// applies, so the SubjectAccessReview is asked about the name as bound.
func TestResolveCandidateRancherUserByExactGetOnly(t *testing.T) {
	t.Parallel()
	local := &fakeClient{Objects: map[string]*unstructured.Unstructured{
		"u-abc12":      userObj("u-abc12", "alice", "Alice", "local://u-abc12"),
		"u-abc12-attr": userAttrObj("u-abc12", "github_org://eng"),
		"u-nogroups":   userObj("u-nogroups", "bob", "Bob", "local://u-nogroups"),
	}}
	got, err := resolveCandidate(t.Context(), local, attrsFor(local), candidate{Kind: KindUser, Name: "u-abc12"})
	require.NoError(t, err)
	assert.Equal(t, candidateIdentity{
		Subject:    AccessSubject{Kind: KindUser, Name: "u-abc12", ID: "u-abc12", DisplayName: "Alice"},
		GroupsFrom: GroupsRancherUserAttribute,
		Groups:     []string{"github_org://eng", "system:authenticated", "system:cattle:authenticated"},
	}, got)

	got, err = resolveCandidate(t.Context(), local, attrsFor(local), candidate{Kind: KindUser, Name: "u-nogroups"})
	require.NoError(t, err)
	assert.Equal(t, GroupsRancherUserAttribute, got.GroupsFrom, "a user who never logged in has no UserAttribute and still is a Rancher user")
	assert.Equal(t, []string{"system:authenticated", "system:cattle:authenticated"}, got.Groups)

	got, err = resolveCandidate(t.Context(), local, attrsFor(local), candidate{Kind: KindUser, Name: "alice"})
	require.NoError(t, err)
	assert.Equal(t, candidateIdentity{
		Subject:    AccessSubject{Kind: KindUser, Name: "alice"},
		GroupsFrom: GroupsAuthenticatedOnly,
		Groups:     []string{"system:authenticated"},
	}, got, "a username is never matched against Rancher usernames or principals")
}

// TestResolveCandidateOtherReadErrorIsReturned asserts any failure other
// than NOT_FOUND on the User GET or the UserAttribute GET is returned as is
// (the caller turns it into the candidate's request issue).
func TestResolveCandidateOtherReadErrorIsReturned(t *testing.T) {
	t.Parallel()
	local := &fakeClient{GetErr: clierr.Retryable(clierr.CodeGeneric, "GET users: 503 Service Unavailable")}
	_, err := resolveCandidate(t.Context(), local, attrsFor(local), candidate{Kind: KindUser, Name: "u-abc12"})
	require.ErrorContains(t, err, "503 Service Unavailable")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.True(t, ce.Retryable)
}

// TestGroupMembersIndexesUserAttributes asserts one LIST of UserAttributes
// becomes a principal -> sorted, deduplicated user id index, and that a
// failed LIST is a warning carrying the mapped message, never an error.
func TestGroupMembersIndexesUserAttributes(t *testing.T) {
	t.Parallel()
	local := &fakeClient{Lists: map[string]*unstructured.UnstructuredList{
		"userattributes||": {Items: []unstructured.Unstructured{
			*userAttrObj("u-def34", "github_org://eng"),
			*userAttrObj("u-abc12", "github_org://eng", "okta_group://ops"),
			*userAttrObj("u-empty"),
		}},
	}}
	members, warning := groupMembers(t.Context(), attrsFor(local))
	assert.Empty(t, warning)
	assert.Equal(t, map[string][]string{
		"github_org://eng": {"u-abc12", "u-def34"},
		"okta_group://ops": {"u-abc12"},
	}, members)

	local = &fakeClient{ListErr: clierr.New(clierr.CodeRBAC, "userattributes is forbidden")}
	members, warning = groupMembers(t.Context(), attrsFor(local))
	assert.Nil(t, members)
	assert.Equal(t, "group members not expanded: you cannot list userattributes (GET /v1/management.cattle.io.userattributes answered 403)"+userAttributesRemedyText, warning)
}

// TestGroupsCacheWarningStatesTheCacheLimit asserts the warning every
// result with a bound group carries names the cache and its limit, so a
// reader never takes knownMembers for the whole membership.
func TestGroupsCacheWarningStatesTheCacheLimit(t *testing.T) {
	t.Parallel()
	assert.Contains(t, groupsCacheWarning, "only the members Rancher has cached")
	assert.Contains(t, groupsCacheWarning, "not expanded beyond that")
}
