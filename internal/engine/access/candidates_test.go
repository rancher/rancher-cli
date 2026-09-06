package access

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// rbacSubject builds one RBAC binding subject as the API serializes it.
func rbacSubject(kind, name, namespace string) map[string]any {
	s := map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": kind, "name": name}
	if namespace != "" {
		s["namespace"] = namespace
	}
	return s
}

// bindingObj builds a RoleBinding (namespace set) or ClusterRoleBinding
// (namespace empty) with the given subjects.
func bindingObj(kind, namespace, name string, subjects ...map[string]any) unstructured.Unstructured {
	subs := make([]any, 0, len(subjects))
	for _, s := range subjects {
		subs = append(subs, s)
	}
	meta := map[string]any{"name": name}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": kind, "metadata": meta,
		"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "view"},
		"subjects": subs,
	}}
}

// bindingsClient serves a cluster's bindings: Kubernetes' own
// cluster-admin binding of system:masters, two ClusterRoleBindings naming
// a user, a group, another user, and one subject of an unknown kind;
// RoleBindings in team-a (a user and a service account) and team-b (an
// arbitrary username), served both cluster-wide and per namespace the
// way the fake keys LIST calls.
func bindingsClient() *fakeClient {
	rbTeamA := bindingObj("RoleBinding", "team-a", "rb-1", rbacSubject("User", "u-abc12", ""), rbacSubject("ServiceAccount", "ci", "team-a"))
	rbTeamB := bindingObj("RoleBinding", "team-b", "rb-2", rbacSubject("User", "oidc-external@corp", ""))
	return &fakeClient{Lists: map[string]*unstructured.UnstructuredList{
		"clusterrolebindings||": {Items: []unstructured.Unstructured{
			bindingObj("ClusterRoleBinding", "", "cluster-admin", rbacSubject("Group", "system:masters", "")),
			bindingObj("ClusterRoleBinding", "", "globaladmin-user-vwq4j", rbacSubject("User", "user-vwq4j", "")),
			bindingObj("ClusterRoleBinding", "", "crb-1", rbacSubject("Group", "github_org://eng", ""), rbacSubject("User", "u-abc12", "")),
			bindingObj("ClusterRoleBinding", "", "odd", map[string]any{"kind": "Robot", "name": "r2"}),
		}},
		"rolebindings||":       {Items: []unstructured.Unstructured{rbTeamA, rbTeamB}},
		"rolebindings|team-a|": {Items: []unstructured.Unstructured{rbTeamA}},
		"rolebindings|team-b|": {Items: []unstructured.Unstructured{rbTeamB}},
	}}
}

func candidateNames(cands []candidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, string(c.Kind)+" "+c.Name)
	}
	return out
}

// TestListCandidatesUnscopedUnionSortedAndDeduplicated asserts the
// cluster-wide candidate set: every subject of every binding once, kinds
// converted to the output set, the service account as its username, the
// fixed system:masters row staying fixed although the cluster-admin
// binding names that group, sorted by kind then name, and one warning for
// the subject kind the set does not know.
func TestListCandidatesUnscopedUnionSortedAndDeduplicated(t *testing.T) {
	t.Parallel()
	cands, warnings, err := listCandidates(t.Context(), bindingsClient(), Scope{Cluster: "c-phmj7", mode: scopeCluster})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"group github_org://eng", "group system:masters",
		"serviceaccount system:serviceaccount:team-a:ci",
		"user oidc-external@corp", "user u-abc12", "user user-vwq4j",
	}, candidateNames(cands))
	masters := cands[1]
	assert.True(t, masters.Fixed, "the bootstrap cluster-admin binding names system:masters, yet the row stays the fixed one")
	assert.False(t, cands[0].Fixed)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], `kind "Robot"`)
}

// TestListCandidatesNamespaceScopeReadsThatNamespaceOnly asserts -n lists
// RoleBindings in the one namespace: team-b's arbitrary user is absent.
func TestListCandidatesNamespaceScopeReadsThatNamespaceOnly(t *testing.T) {
	t.Parallel()
	cands, _, err := listCandidates(t.Context(), bindingsClient(), Scope{Cluster: "c-phmj7", mode: scopeNamespace, Namespace: "team-a", targets: []string{"team-a"}})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"group github_org://eng", "group system:masters",
		"serviceaccount system:serviceaccount:team-a:ci",
		"user u-abc12", "user user-vwq4j",
	}, candidateNames(cands))
}

// TestListCandidatesProjectKeepsMemberNamespaces asserts --project lists
// RoleBindings cluster-wide and keeps the project's namespaces only.
func TestListCandidatesProjectKeepsMemberNamespaces(t *testing.T) {
	t.Parallel()
	cands, _, err := listCandidates(t.Context(), bindingsClient(), Scope{Cluster: "c-phmj7", mode: scopeProject, Project: "p-cvrp4", targets: []string{"team-b"}})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"group github_org://eng", "group system:masters",
		"user oidc-external@corp", "user u-abc12", "user user-vwq4j",
	}, candidateNames(cands))
}

// TestListCandidatesForbiddenNamesTheRights asserts a forbidden LIST is a
// CodeRBAC refusal naming both rights, and any other failure keeps its
// mapped error.
func TestListCandidatesForbiddenNamesTheRights(t *testing.T) {
	t.Parallel()
	c := bindingsClient()
	c.ListErr = clierr.New(clierr.CodeRBAC, "clusterrolebindings is forbidden")
	_, _, err := listCandidates(t.Context(), c, Scope{Cluster: "c-phmj7", mode: scopeCluster})
	require.ErrorContains(t, err, "who-can needs list on rolebindings and clusterrolebindings on cluster c-phmj7")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)

	c.ListErr = clierr.Retryable(clierr.CodeGeneric, "503 Service Unavailable")
	_, _, err = listCandidates(t.Context(), c, Scope{Cluster: "c-phmj7", mode: scopeCluster})
	require.ErrorContains(t, err, "503 Service Unavailable")
	ce, ok = errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.True(t, ce.Retryable)
}

// TestCandidateSourceForNamesBothBindingsAndTheFixedGroup asserts the
// candidateSource states the two listed resources, the cluster they were
// read on, and the group added independently of them.
func TestCandidateSourceForNamesBothBindingsAndTheFixedGroup(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "rolebindings and clusterrolebindings on c-phmj7 (+ system:masters)", candidateSourceFor("c-phmj7"))
}

// TestCandidateFromSubjectKeepsNamesByteForByte asserts the one kind
// conversion and that names are never normalized: a username that looks
// like a service account stays a user, a ServiceAccount subject becomes
// its username, and a subject without a name or (for a service account)
// without a namespace is not a candidate.
func TestCandidateFromSubjectKeepsNamesByteForByte(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind, name, namespace string
		want                  candidate
		ok                    bool
	}{
		{"User", "u-abc12", "", candidate{Kind: KindUser, Name: "u-abc12"}, true},
		{"User", "Alice Smith", "", candidate{Kind: KindUser, Name: "Alice Smith"}, true},
		{"User", "system:serviceaccount:ns:x", "", candidate{Kind: KindUser, Name: "system:serviceaccount:ns:x"}, true},
		{"Group", "github_org://eng", "", candidate{Kind: KindGroup, Name: "github_org://eng"}, true},
		{"ServiceAccount", "ci", "team-a", candidate{Kind: KindServiceAccount, Name: "system:serviceaccount:team-a:ci"}, true},
		{"ServiceAccount", "ci", "", candidate{}, false},
		{"User", "", "", candidate{}, false},
		{"Robot", "r2", "", candidate{}, false},
	}
	for _, c := range cases {
		got, ok := candidateFromSubject(c.kind, c.name, c.namespace)
		assert.Equal(t, c.ok, ok, c.kind+" "+c.name)
		if ok {
			assert.Equal(t, c.want, got)
		}
	}
}
