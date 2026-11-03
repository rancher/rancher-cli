package access

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

func callerFactory(callerUser string, impersonated map[string]*fakeClient) *fakeFactory {
	return &fakeFactory{
		caller:       &fakeClient{script: []scripted{{resp: whoamiResp(callerUser, "system:authenticated", "system:cattle:authenticated")}}},
		impersonated: impersonated,
	}
}

func localWith(users ...string) *fakeClient {
	objs := map[string]*unstructured.Unstructured{}
	for _, u := range users {
		objs[u] = userObj(u, u+"-name", u+" display", "local://"+u)
	}
	return &fakeClient{Objects: objs, Lists: map[string]*unstructured.UnstructuredList{"users||": {}}}
}

// TestPlanIdentityCallerPaths asserts an omitted --as and an --as naming
// the caller both take the caller path with no impersonation.
func TestPlanIdentityCallerPaths(t *testing.T) {
	t.Parallel()
	for _, as := range []string{"", "user-vwq4j"} {
		f := callerFactory("user-vwq4j", nil)
		p, err := ParseSubject(as, "")
		require.NoError(t, err)
		caller, err := callerPreflight(t.Context(), f.caller, "c-phmj7")
		require.NoError(t, err)
		plan, err := planIdentityOver(t.Context(), f, f.caller, localWith("user-vwq4j"), "c-phmj7", caller, p, false)
		require.NoError(t, err)
		assert.Equal(t, IdentityCaller, plan.Identity)
		assert.True(t, plan.AsCaller)
		assert.Equal(t, AccessSubject{Kind: KindUser, Name: "user-vwq4j", ID: "user-vwq4j", DisplayName: "user-vwq4j display"}, plan.Subject, "caller enriched best-effort from its User object")
		assert.Empty(t, f.Impersonated)
	}
}

// TestPlanIdentityGroupNamedLikeCallerUsesSAR asserts a group whose name
// equals the caller's username never takes the caller shortcut: the
// question stays a SubjectAccessReview about that group. A service
// account caller naming itself does take the shortcut.
func TestPlanIdentityGroupNamedLikeCallerUsesSAR(t *testing.T) {
	t.Parallel()
	f := callerFactory("user-vwq4j", nil)
	admin := callerIdentity{Username: "user-vwq4j", Groups: []string{"system:authenticated"}}
	g, _ := ParseSubject("user-vwq4j", "group")
	plan, err := planIdentityOver(t.Context(), f, f.caller, localWith(), "c-phmj7", admin, g, false)
	require.NoError(t, err)
	assert.Equal(t, IdentitySAR, plan.Identity)
	assert.Equal(t, GroupsGroupOnly, plan.GroupsFrom)
	assert.Equal(t, []string{"user-vwq4j"}, plan.Groups)
	assert.False(t, plan.AsCaller)
	assert.Empty(t, f.Impersonated)

	sa := callerIdentity{Username: "system:serviceaccount:ci:runner", Groups: []string{"system:serviceaccounts"}}
	p, _ := ParseSubject("system:serviceaccount:ci:runner", "")
	plan, err = planIdentityOver(t.Context(), f, f.caller, localWith(), "c-phmj7", sa, p, false)
	require.NoError(t, err)
	assert.Equal(t, IdentityCaller, plan.Identity)
	assert.True(t, plan.AsCaller)
}

// TestPlanIdentityNonexistentServiceAccountIsNotFound asserts the proxy's
// plain-text 500 for an impersonated service account it cannot find is a
// NOT_FOUND refusal naming the subject and the cluster.
func TestPlanIdentityNonexistentServiceAccountIsNotFound(t *testing.T) {
	t.Parallel()
	ghost := &fakeClient{script: []scripted{{resp: saNotFound500("serviceaccounts \"ghost\" not found")}}}
	f := callerFactory("user-vwq4j", map[string]*fakeClient{"system:serviceaccount:team-a:ghost": ghost})
	p, _ := ParseSubject("system:serviceaccount:team-a:ghost", "")
	_, err := planIdentityOver(t.Context(), f, f.caller, localWith(), "c-phmj7", callerIdentity{Username: "user-vwq4j", Groups: []string{"system:authenticated"}}, p, false)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.False(t, ce.Retryable)
	require.ErrorContains(t, err, "system:serviceaccount:team-a:ghost not found on cluster c-phmj7")
	require.ErrorContains(t, err, "serviceaccounts \"ghost\" not found")
}

// TestPlanIdentityCallerWithoutClusterAccess asserts the caller's own
// cluster-access refusal is a refusal, not a decision.
func TestPlanIdentityCallerWithoutClusterAccess(t *testing.T) {
	t.Parallel()
	f := &fakeFactory{caller: &fakeClient{script: []scripted{{resp: http403("")}}}}
	_, err := callerPreflight(t.Context(), f.caller, "c-phmj7")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	require.ErrorContains(t, err, "you have no access to cluster c-phmj7; a cluster owner grants it with a ClusterRoleTemplateBinding or ProjectRoleTemplateBinding")
	assert.Empty(t, ce.Hint)
}

// TestPlanIdentityImpersonationSucceeds asserts a named user runs on the
// impersonated client once whoami confirms the identity.
func TestPlanIdentityImpersonationSucceeds(t *testing.T) {
	t.Parallel()
	alice := &fakeClient{script: []scripted{{resp: whoamiResp("u-abc12", "system:authenticated")}}}
	f := callerFactory("user-vwq4j", map[string]*fakeClient{"u-abc12": alice})
	p, _ := ParseSubject("u-abc12", "")
	plan, err := planIdentityOver(t.Context(), f, f.caller, localWith("u-abc12"), "c-phmj7", callerIdentity{Username: "user-vwq4j", Groups: []string{"system:authenticated"}}, p, false)
	require.NoError(t, err)
	assert.Equal(t, IdentityImpersonated, plan.Identity)
	assert.Equal(t, []string{"u-abc12"}, f.Impersonated)
	assert.Same(t, alice, plan.Client.(*fakeClient))
}

// TestPlanIdentityHeaderDropFallsBackToSAR asserts a whoami that still
// reports the caller (the local route drops the header) yields the SAR
// path with the subject's groups.
func TestPlanIdentityHeaderDropFallsBackToSAR(t *testing.T) {
	t.Parallel()
	dropped := &fakeClient{script: []scripted{{resp: whoamiResp("user-vwq4j")}}}
	f := callerFactory("user-vwq4j", map[string]*fakeClient{"u-abc12": dropped})
	p, _ := ParseSubject("u-abc12", "")
	plan, err := planIdentityOver(t.Context(), f, f.caller, localWith("u-abc12"), "c-phmj7", callerIdentity{Username: "user-vwq4j", Groups: []string{"system:authenticated"}}, p, false)
	require.NoError(t, err)
	assert.Equal(t, IdentitySAR, plan.Identity)
	assert.Equal(t, GroupsRancherUserAttribute, plan.GroupsFrom)
	assert.Contains(t, plan.Groups, "system:cattle:authenticated")
	assert.Same(t, f.caller, plan.Client.(*fakeClient), "SAR is posted as the caller")
}

// TestPlanIdentityImpersonationRefusedFallsBackToSAR asserts the Rancher
// refusal message routes to the SAR path.
func TestPlanIdentityImpersonationRefusedFallsBackToSAR(t *testing.T) {
	t.Parallel()
	refused := &fakeClient{script: []scripted{{resp: http403(`{"message":"not allowed to impersonate user"}`)}}}
	f := callerFactory("m-54pdw", map[string]*fakeClient{"u-abc12": refused})
	p, _ := ParseSubject("u-abc12", "")
	plan, err := planIdentityOver(t.Context(), f, f.caller, localWith("u-abc12"), "c-phmj7", callerIdentity{Username: "m-54pdw"}, p, false)
	require.NoError(t, err)
	assert.Equal(t, IdentitySAR, plan.Identity)
}

// crtbFor is a ClusterRoleTemplateBinding naming a user id, the shape
// noRancherBindingsFor matches on.
func crtbFor(userID string) *unstructured.UnstructuredList {
	return &unstructured.UnstructuredList{Items: []unstructured.Unstructured{{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "ClusterRoleTemplateBinding",
		"metadata": map[string]any{"name": "crtb-1", "namespace": "c-phmj7"}, "userName": userID,
	}}}}
}

// TestPlanIdentitySubjectWithoutClusterAccess asserts the impersonated
// whoami's empty 403 is one cluster-wide denial with a note, and that the
// note's parenthetical is added only with evidence for it: neither a
// management plane that could not be read nor a binding that does name the
// subject earns the clause.
func TestPlanIdentitySubjectWithoutClusterAccess(t *testing.T) {
	t.Parallel()
	const bare = "u-abc12 has no access to cluster c-phmj7"
	for _, tc := range []struct {
		name  string
		local func() *fakeClient
		note  string
	}{
		{
			name: "no binding names the subject",
			local: func() *fakeClient {
				l := localWith("u-abc12")
				l.Lists["clusterroletemplatebindings|c-phmj7|"] = &unstructured.UnstructuredList{}
				l.Lists["projectroletemplatebindings||"] = &unstructured.UnstructuredList{}
				return l
			},
			note: bare + " (no ClusterRoleTemplateBinding or ProjectRoleTemplateBinding grants it)",
		},
		{
			name: "the management list fails",
			local: func() *fakeClient {
				l := localWith("u-abc12")
				l.ListErr = errors.New("management plane unreachable")
				return l
			},
			note: bare,
		},
		{
			name: "a binding names the subject",
			local: func() *fakeClient {
				l := localWith("u-abc12")
				l.Lists["clusterroletemplatebindings|c-phmj7|"] = crtbFor("u-abc12")
				l.Lists["projectroletemplatebindings||"] = &unstructured.UnstructuredList{}
				return l
			},
			note: bare,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			noAccess := &fakeClient{script: []scripted{{resp: http403("")}}}
			f := callerFactory("user-vwq4j", map[string]*fakeClient{"u-abc12": noAccess})
			p, _ := ParseSubject("u-abc12", "")
			plan, err := planIdentityOver(t.Context(), f, f.caller, tc.local(), "c-phmj7",
				callerIdentity{Username: "user-vwq4j", Groups: []string{"system:authenticated"}}, p, false)
			require.NoError(t, err)
			require.NotNil(t, plan.Denied)
			assert.Equal(t, IdentityImpersonated, plan.Identity)
			assert.Empty(t, plan.GroupsFrom)
			assert.Equal(t, tc.note, plan.Denied.Note)
		})
	}
}

// TestPlanIdentityImpersonationRefusedRefusesList asserts list has no
// SubjectAccessReview to fall back to: the same Rancher refusal that routes
// can to the SAR path ends list with the impersonation refusal.
func TestPlanIdentityImpersonationRefusedRefusesList(t *testing.T) {
	t.Parallel()
	refused := &fakeClient{script: []scripted{{resp: http403(`{"message":"not allowed to impersonate user"}`)}}}
	f := callerFactory("m-54pdw", map[string]*fakeClient{"u-abc12": refused})
	p, _ := ParseSubject("u-abc12", "")
	_, err := planIdentityOver(t.Context(), f, f.caller, localWith("u-abc12"), "c-phmj7", callerIdentity{Username: "m-54pdw"}, p, true)
	require.ErrorContains(t, err, "cannot act as u-abc12 on cluster c-phmj7: your GlobalRoles lack impersonate on users on the management cluster")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, clierr.Rancher+" get globalroles", ce.Hint)
}

// TestRefusalMessagesNameTheMissingRight asserts the impersonation and
// SubjectAccessReview refusals name the right the caller lacks for the
// subject's kind, and that the two list refusals point at the point question
// instead.
func TestRefusalMessagesNameTheMissingRight(t *testing.T) {
	t.Parallel()
	user := ResolvedSubject{Subject: AccessSubject{Kind: KindUser, Name: "u-abc12"}}
	sa := ResolvedSubject{Subject: AccessSubject{Kind: KindServiceAccount, Name: "system:serviceaccount:ci:runner"}}
	group := ResolvedSubject{Subject: AccessSubject{Kind: KindGroup, Name: "eng"}}

	err := impersonationRefusal(user, "c-phmj7")
	assert.Equal(t, clierr.CodeRBAC, err.Code)
	assert.Equal(t, "cannot act as u-abc12 on cluster c-phmj7: your GlobalRoles lack impersonate on users on the management cluster", err.Message)
	assert.Equal(t, clierr.Rancher+" get globalroles", err.Hint)
	require.ErrorContains(t, impersonationRefusal(sa, "c-phmj7"), "lack impersonate on serviceaccounts on the management cluster")

	err = sarRefusal(user, "c-phmj7")
	assert.Equal(t, clierr.CodeRBAC, err.Code)
	assert.Equal(t, "cannot ask about u-abc12 on cluster c-phmj7: you lack create subjectaccessreviews on cluster c-phmj7, or impersonate on users on the management cluster", err.Message)
	assert.Equal(t, clierr.Rancher+" get globalroles", err.Hint)
	require.ErrorContains(t, sarRefusal(sa, "c-phmj7"), "or impersonate on serviceaccounts on the management cluster")
	// A group has nothing to impersonate, and local never impersonates:
	// only the review right is named.
	assert.Equal(t, "cannot ask about eng on cluster c-phmj7: you lack create subjectaccessreviews on cluster c-phmj7", sarRefusal(group, "c-phmj7").Message)
	assert.Equal(t, "cannot ask about u-abc12 on cluster local: you lack create subjectaccessreviews on cluster local", sarRefusal(user, engine.LocalCluster).Message)

	assert.Equal(t, "no server operation lists a group's rules; <verb> and <resource> are what to ask and <cluster> is the cluster; ask a point question with: "+
		clierr.Rancher+" access can <verb> <resource> --cluster <cluster> --as 'github_org://eng' --type group",
		groupListMessage("github_org://eng"))
	assert.Equal(t, "no server operation lists the anonymous identity's rules; <verb> and <resource> are what to ask and <cluster> is the cluster; ask a point question with: "+
		clierr.Rancher+" access can <verb> <resource> --cluster <cluster> --as system:anonymous", anonymousListMessage)
}

// TestPlanIdentityLocalAndGroupSkipImpersonation asserts groups and the
// local cluster go straight to SAR, and that list refuses a named subject
// on local. (A group is rejected by list before planIdentity runs.)
func TestPlanIdentityLocalAndGroupSkipImpersonation(t *testing.T) {
	t.Parallel()
	f := callerFactory("user-vwq4j", nil)
	admin := callerIdentity{Username: "user-vwq4j", Groups: []string{"system:authenticated"}}
	g, _ := ParseSubject("github_org://eng", "")
	plan, err := planIdentityOver(t.Context(), f, f.caller, localWith(), "c-phmj7", admin, g, false)
	require.NoError(t, err)
	assert.Equal(t, IdentitySAR, plan.Identity)
	assert.Equal(t, GroupsGroupOnly, plan.GroupsFrom)
	assert.Empty(t, f.Impersonated)

	anon, _ := ParseSubject("system:anonymous", "")
	plan, err = planIdentityOver(t.Context(), f, f.caller, localWith(), "c-phmj7", admin, anon, false)
	require.NoError(t, err)
	assert.Equal(t, IdentitySAR, plan.Identity)
	assert.Equal(t, GroupsAnonymousIdentity, plan.GroupsFrom)
	assert.Empty(t, f.Impersonated, "anonymous is never impersonated")

	f = callerFactory("user-vwq4j", nil)
	u, _ := ParseSubject("u-abc12", "")
	plan, err = planIdentityOver(t.Context(), f, f.caller, localWith("u-abc12"), engine.LocalCluster, admin, u, false)
	require.NoError(t, err)
	assert.Equal(t, IdentitySAR, plan.Identity)
	assert.Empty(t, f.Impersonated, "local never impersonates")
	_, err = planIdentityOver(t.Context(), f, f.caller, localWith("u-abc12"), engine.LocalCluster, admin, u, true)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	require.ErrorContains(t, err, "cannot impersonate")
}
