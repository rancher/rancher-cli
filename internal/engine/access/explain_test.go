package access

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

func decodeExplain(t *testing.T, res runtime.Result) explainResult {
	t.Helper()
	b, err := json.Marshal(res.Data)
	require.NoError(t, err)
	var out explainResult
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

const explainReason = `RBAC: allowed by RoleBinding "rb-1/team-a" of ClusterRole "project-member-promoted" to User "user-vwq4j"`

// explainFixtures returns the caller's downstream client (whoami then the
// scripted review) with the binding and role, and the management client
// with the caller's User, the PRTB and RoleTemplates.
func explainFixtures(review scripted) (*fakeFactory, *fakeClient) {
	f := canFactory("user-vwq4j", review)
	f.caller.Objects["team-a/rb-1"] = chainObj("rbac.authorization.k8s.io/v1", "RoleBinding", "team-a", "rb-1",
		map[string]string{rtbOwnerLabel: "c-phmj7-p-cvrp4_prtb-1"}, nil,
		map[string]any{"roleRef": rbacRoleRef("ClusterRole", "project-member-promoted"), "subjects": []any{rbacSubject("User", "user-vwq4j", "")}})
	f.caller.Objects["project-member-promoted"] = chainObj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "project-member-promoted", nil, nil,
		map[string]any{"rules": podsRule("get", "list", "delete")})
	f.caller.Lists["rolebindings|team-a|"] = &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*f.caller.Objects["team-a/rb-1"]}}
	f.caller.Lists["clusterrolebindings||"] = &unstructured.UnstructuredList{}
	local := localWith("user-vwq4j")
	local.Objects["c-phmj7-p-cvrp4/prtb-1"] = chainObj("management.cattle.io/v3", "ProjectRoleTemplateBinding", "c-phmj7-p-cvrp4", "prtb-1", nil, nil,
		map[string]any{"userName": "user-vwq4j", "roleTemplateName": "project-member", "projectName": "c-phmj7:p-cvrp4"})
	local.Objects["project-member"] = chainObj("management.cattle.io/v3", "RoleTemplate", "", "project-member", nil, nil, map[string]any{"roleTemplateNames": []any{"edit"}})
	local.Objects["edit"] = chainObj("management.cattle.io/v3", "RoleTemplate", "", "edit", nil, nil, map[string]any{"external": true})
	return f, local
}

// TestExplainAllowedCallerBuildsTheTree asserts the caller path: whoami,
// one SelfSubjectAccessReview, the binding the reason names as the root
// with its role and Rancher parent, chain complete, exit 0, and the text
// header, tree and source line.
func TestExplainAllowedCallerBuildsTheTree(t *testing.T) {
	t.Parallel()
	f, local := explainFixtures(scripted{resp: sar(true, explainReason)})
	res, err := NewExplain(f, local).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "delete", "pods"), nil)
	require.NoError(t, err)
	out := decodeExplain(t, res)
	assert.Equal(t, 0, res.ExitStatus)
	assert.Equal(t, DecisionAllowed, out.Decision)
	assert.Equal(t, explainReason, out.Reason)
	assert.Equal(t, chainComplete, out.Chain)
	assert.Empty(t, out.ChainIssues)
	assert.NotNil(t, out.ChainIssues, "an empty list, never null")
	require.NotNil(t, out.Tree)
	assert.Equal(t, "RoleBinding", out.Tree.Kind)
	assert.Equal(t, "rb-1", out.Tree.Name)
	assert.Equal(t, "team-a", out.Tree.Namespace)
	require.Len(t, out.Tree.Children, 2)
	assert.Equal(t, "ProjectRoleTemplateBinding", out.Tree.Children[1].Kind)
	assert.Equal(t, SourceAuthorizer, out.ResultSource)
	assert.Equal(t, IdentityCaller, out.Identity)
	assert.Len(t, f.caller.Reviews, 2, "whoami and one SelfSubjectAccessReview")
	lines := strings.Split(strings.TrimSuffix(res.Text, "\n"), "\n")
	assert.Equal(t, "allowed  you can delete pods in team-a on c-phmj7", lines[0])
	assert.Equal(t, "  reason: "+explainReason, lines[1])
	assert.Equal(t, "[allowed] RoleBinding team-a/rb-1 - one binding the server accepted", lines[2])
	assert.Equal(t, "+-- ClusterRole project-member-promoted - rules covering the request: core/pods [get list delete]", lines[3])
	assert.Equal(t, "+-- ProjectRoleTemplateBinding c-phmj7-p-cvrp4/prtb-1 - project c-phmj7:p-cvrp4, user user-vwq4j, RoleTemplate project-member", lines[4])
	assert.Equal(t, "source: authorizer | identity: caller", lines[len(lines)-1])
	assert.NotContains(t, res.Text, "chain:", "a complete chain prints no chain block")
}

// TestExplainDeniedListsTheSubjectsBindings asserts a denied answer exits
// 12 and roots the tree at the bindings naming the subject, marked denied.
func TestExplainDeniedListsTheSubjectsBindings(t *testing.T) {
	t.Parallel()
	f, local := explainFixtures(scripted{resp: sar(false, "")})
	res, err := NewExplain(f, local).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "delete", "pods"), nil)
	require.NoError(t, err)
	out := decodeExplain(t, res)
	assert.Equal(t, exitDenied, res.ExitStatus)
	assert.Equal(t, DecisionDenied, out.Decision)
	assert.Equal(t, "the CLI found no binding above that the server accepted for this request", out.Note)
	require.NotNil(t, out.Tree)
	assert.Equal(t, nodeBindings, out.Tree.Kind)
	require.Len(t, out.Tree.Children, 1)
	assert.Equal(t, "rb-1", out.Tree.Children[0].Name)
	assert.Equal(t, chainComplete, out.Chain)
	assert.Contains(t, res.Text, "denied  you cannot delete pods in team-a on c-phmj7\n")
	assert.Contains(t, res.Text, "\n[denied] Bindings - "+bindingsTitle+"\n")
}

// TestExplainPartialChainKeepsTheExit asserts a caller refused the RBAC
// objects still gets the decision and exit 0, a partial chain with the
// typed issues, and the chain block after the tree.
func TestExplainPartialChainKeepsTheExit(t *testing.T) {
	t.Parallel()
	f, local := explainFixtures(scripted{resp: sar(true, explainReason)})
	refused := clierr.New(clierr.CodeRBAC, `rolebindings.rbac.authorization.k8s.io "rb-1" is forbidden`)
	f.caller.GetErrFor = map[string]error{"team-a/rb-1": refused, "project-member-promoted": refused}
	local.Lists["clusterroletemplatebindings||"] = &unstructured.UnstructuredList{}
	local.Lists["projectroletemplatebindings||"] = &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*local.Objects["c-phmj7-p-cvrp4/prtb-1"]}}
	res, err := NewExplain(f, local).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "delete", "pods"), nil)
	require.NoError(t, err)
	out := decodeExplain(t, res)
	assert.Equal(t, 0, res.ExitStatus, "the chain is provenance; the decision decides the exit")
	assert.Equal(t, DecisionAllowed, out.Decision)
	assert.Equal(t, chainPartial, out.Chain)
	require.Len(t, out.ChainIssues, 3)
	assert.Equal(t, stageBindingRead, out.ChainIssues[0].Stage)
	assert.Equal(t, clierr.CodeRBAC, out.ChainIssues[0].Code)
	assert.Contains(t, res.Text, "\nchain: partial (3 issues)\n")
	assert.Contains(t, res.Text, "\n  bindingRead RoleBinding team-a/rb-1: RBAC - rolebindings.rbac.authorization.k8s.io \"rb-1\" is forbidden\n")
	assert.Contains(t, res.Text, "+-- HeldBindings - "+heldBindingsTitle)
	assert.Empty(t, res.Warnings, "chain issues are the answer, never warnings")
}

// TestExplainUnparseableReasonIsAnUnknownChain asserts an allowed answer
// whose reason is not the authorizer's sentence keeps the decision and
// exit 0 with an empty tree and chain unknown.
func TestExplainUnparseableReasonIsAnUnknownChain(t *testing.T) {
	t.Parallel()
	f, local := explainFixtures(scripted{resp: sar(true, "allowed by a webhook authorizer")})
	res, err := NewExplain(f, local).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "delete", "pods"), nil)
	require.NoError(t, err)
	out := decodeExplain(t, res)
	assert.Equal(t, 0, res.ExitStatus)
	assert.Equal(t, chainUnknown, out.Chain)
	assert.Nil(t, out.Tree)
	require.Len(t, out.ChainIssues, 1)
	assert.Equal(t, codeUnparseable, out.ChainIssues[0].Code)
	assert.Contains(t, res.Text, "\nchain: unknown (1 issue)\n")
}

// TestExplainProxyDenialIsDeniedWithoutAChain asserts a subject the proxy
// refuses cluster access exits 12 with the proxy as source and no tree.
func TestExplainProxyDenialIsDeniedWithoutAChain(t *testing.T) {
	t.Parallel()
	noAccess := &fakeClient{script: []scripted{{resp: http403("")}}}
	f := canFactory("user-vwq4j")
	f.impersonated = map[string]*fakeClient{"u-abc12": noAccess}
	local := localWith("u-abc12")
	local.Lists["clusterroletemplatebindings|c-phmj7|"] = &unstructured.UnstructuredList{}
	local.Lists["projectroletemplatebindings||"] = &unstructured.UnstructuredList{}
	res, err := NewExplain(f, local).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a", "as": "u-abc12"}, "get", "pods"), nil)
	require.NoError(t, err)
	out := decodeExplain(t, res)
	assert.Equal(t, exitDenied, res.ExitStatus)
	assert.Equal(t, DecisionDenied, out.Decision)
	assert.Equal(t, SourceRancherProxy, out.ResultSource)
	assert.Equal(t, IdentityImpersonated, out.Identity)
	assert.Nil(t, out.Tree)
	assert.Equal(t, chainComplete, out.Chain, "nothing was left to establish")
	assert.Contains(t, out.Note, "has no access to cluster c-phmj7")
	assert.Len(t, f.caller.Reviews, 1, "no authorization review as the caller")
}

// TestExplainNeverReadsTheProjectFlag asserts access explain's parse leaves
// --project unread: explain does not take it (the pipeline refuses it), so
// no value can reach its scope, while can and who-can read it into their
// selector check.
func TestExplainNeverReadsTheProjectFlag(t *testing.T) {
	t.Parallel()
	in := canInput(map[string]string{"namespace": "team-a", "project": "web"}, "get", "pods")
	p, err := parseQuestion(in, false, false)
	require.NoError(t, err)
	assert.Empty(t, p.scope.project)
	assert.Equal(t, "team-a", p.scope.namespace)
	_, err = parseQuestion(in, false, true)
	require.ErrorContains(t, err, "pass at most one of --namespace, --project, --all-namespaces")
}
