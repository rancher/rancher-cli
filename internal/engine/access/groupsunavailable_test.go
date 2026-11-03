package access

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// groupsSetup is what steve says about the subject u-abc12's UserAttribute.
type groupsSetup int

const (
	groupsRead groupsSetup = iota
	groupsNone
	groupsRefused
)

func (g groupsSetup) String() string { return [...]string{"groups read", "no object", "refused"}[g] }

// subjectLocal is the management client for u-abc12 with its UserAttribute
// as set.
func subjectLocal(g groupsSetup) *fakeClient {
	local := localWith("u-abc12")
	switch g {
	case groupsRead:
		local.Objects["u-abc12-attr"] = userAttrObj("u-abc12", "okta_group://eng")
	case groupsRefused:
		local.AttrRefusal = attrNoRight
	}
	return local
}

// w1 is the warning for a review asked without the cached groups, with the
// refusal M1 of a 404 for u-abc12 spelled out.
var w1 = "the groups Rancher has cached for u-abc12 were not read: " + m1For404 +
	"; the review was asked with system:authenticated and system:cattle:authenticated only, and a denied answer is reported unknown"

var m1For404 = "you cannot read userattributes (GET /v1/management.cattle.io.userattributes/u-abc12 answered 404); " +
	"Rancher's API shows you no userattributes type" + "; a GlobalRole with get and list on userattributes.management.cattle.io allows it; see: " + clierr.Rancher + " get globalroles"

func sarSubject(allowed bool) scripted {
	return scripted{resp: review(engine.ReviewSubjectAccess, map[string]any{"allowed": allowed, "reason": ""})}
}

// refusedImpersonation is the client for u-abc12 whose proxy route refuses
// the impersonation, so the review is a SubjectAccessReview as the caller.
func refusedImpersonation() map[string]*fakeClient {
	return map[string]*fakeClient{"u-abc12": {script: []scripted{{resp: http403(`{"message":"not allowed to impersonate user"}`)}}}}
}

// droppedImpersonation is the client for u-abc12 whose route drops the
// header: its whoami answers as the caller.
func droppedImpersonation() map[string]*fakeClient {
	return map[string]*fakeClient{"u-abc12": {script: []scripted{{resp: whoamiResp("user-vwq4j")}}}}
}

// TestCanSubjectAccessReviewGroupsOutcomes asserts the answer, the exit,
// the note, groupsFrom, groupAliases, the warning and the text line for a
// Rancher user asked through a SubjectAccessReview, whatever steve says
// about the cached groups.
func TestCanSubjectAccessReviewGroupsOutcomes(t *testing.T) {
	tests := []struct {
		groups   groupsSetup
		allowed  bool
		decision Decision
		exit     int
		note     string
		status   string
		count    int
		warning  bool
	}{
		{groups: groupsRead, allowed: true, decision: DecisionAllowed, exit: 0, status: "available", count: 1},
		{groups: groupsRead, allowed: false, decision: DecisionDenied, exit: exitDenied, status: "available", count: 1},
		{groups: groupsNone, allowed: true, decision: DecisionAllowed, exit: 0, status: "available", count: 0},
		{groups: groupsNone, allowed: false, decision: DecisionDenied, exit: exitDenied, status: "available", count: 0},
		{groups: groupsRefused, allowed: true, decision: DecisionAllowed, exit: 0, status: "unavailable", warning: true},
		{groups: groupsRefused, allowed: false, decision: DecisionUnknown, exit: exitUnknown, note: groupsUnknownNote, status: "unavailable", warning: true},
	}
	for _, tc := range tests {
		name := tc.groups.String() + " allowed " + map[bool]string{true: "yes", false: "no"}[tc.allowed]
		t.Run(name, func(t *testing.T) {
			serialFanOut(t)
			f := canFactory("user-vwq4j", sarSubject(tc.allowed))
			f.impersonated = refusedImpersonation()
			local := subjectLocal(tc.groups)
			res, err := newCanOver(f, local).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a", "as": "u-abc12"}, "get", "pods"), nil)
			require.NoError(t, err)
			out := decodeCan(t, res)
			assert.Equal(t, tc.decision, out.Decision)
			assert.Equal(t, tc.exit, res.ExitStatus)
			assert.Equal(t, tc.note, out.Note)
			assert.Equal(t, GroupsRancherUserAttribute, out.GroupsFrom)
			require.NotNil(t, out.GroupAliases)
			assert.Equal(t, tc.status, out.GroupAliases.Status)
			assert.Equal(t, GroupsRancherUserAttribute, out.GroupAliases.Source)
			if tc.status == "unavailable" {
				assert.Nil(t, out.GroupAliases.Count)
			} else {
				require.NotNil(t, out.GroupAliases.Count)
				assert.Equal(t, tc.count, *out.GroupAliases.Count)
			}
			if tc.warning {
				assert.Equal(t, []string{w1}, res.Warnings)
				assert.Contains(t, res.Text, " | groups from rancherUserAttribute (unavailable)\n")
			} else {
				assert.Empty(t, res.Warnings)
				assert.Contains(t, res.Text, " | groups from rancherUserAttribute\n")
				assert.NotContains(t, res.Text, "(unavailable)")
			}
			review := f.caller.Reviews[1]
			groups, _, _ := unstructured.NestedStringSlice(review.Object.Object, "spec", "groups")
			if tc.groups == groupsRefused {
				assert.Equal(t, []string{"system:authenticated", "system:cattle:authenticated"}, groups)
			} else {
				assert.Contains(t, groups, "system:authenticated")
			}
		})
	}
}

// TestCanGroupAliasesAbsentWithoutAUserAttribute asserts groupAliases is
// left out of the output for a subject whose groups do not come from the
// UserAttribute.
func TestCanGroupAliasesAbsentWithoutAUserAttribute(t *testing.T) {
	t.Parallel()
	f := canFactory("user-vwq4j", scripted{resp: review(engine.ReviewSubjectAccess, map[string]any{"allowed": true, "reason": ""})})
	res, err := NewCan(f, attrRootsFor(f.caller), f.caller).(runtime.ReadCommand).Execute(t.Context(),
		canInput(map[string]string{"namespace": "team-a", "as": "github_org://eng"}, "get", "pods"), nil)
	require.NoError(t, err)
	assert.NotContains(t, string(mustJSON(t, res.Data)), "groupAliases")
}

// TestCanFallbackPathCarriesTheWarning asserts the warning also reaches the
// SubjectAccessReview taken when the route drops the impersonation header.
func TestCanFallbackPathCarriesTheWarning(t *testing.T) {
	serialFanOut(t)
	f := canFactory("user-vwq4j", sarSubject(false))
	f.impersonated = droppedImpersonation()
	res, err := newCanOver(f, subjectLocal(groupsRefused)).(runtime.ReadCommand).Execute(t.Context(),
		canInput(map[string]string{"namespace": "team-a", "as": "u-abc12"}, "get", "pods"), nil)
	require.NoError(t, err)
	out := decodeCan(t, res)
	assert.Equal(t, IdentitySAR, out.Identity)
	assert.Equal(t, DecisionUnknown, out.Decision)
	assert.Equal(t, []string{w1}, res.Warnings)
}

// TestCanVerifiedEmptyProjectWithUnavailableGroups asserts no review is
// asked and no warning is printed for a project with no namespaces, while
// groupAliases says the groups were unavailable.
func TestCanVerifiedEmptyProjectWithUnavailableGroups(t *testing.T) {
	t.Parallel()
	f := canFactory("user-vwq4j")
	f.caller.Lists["namespaces||field.cattle.io/projectId=p-empty"] = &unstructured.UnstructuredList{}
	f.impersonated = refusedImpersonation()
	res, err := newCanOver(f, subjectLocal(groupsRefused)).(runtime.ReadCommand).Execute(projectContext(t, f.caller, "p-empty", "Empty"),
		canInput(map[string]string{"project": "p-empty", "as": "u-abc12"}, "get", "pods"), nil)
	require.NoError(t, err)
	out := decodeCan(t, res)
	assert.Equal(t, DecisionUnknown, out.Decision)
	assert.Equal(t, exitUnknown, res.ExitStatus)
	assert.Empty(t, res.Warnings)
	require.NotNil(t, out.GroupAliases)
	assert.Equal(t, "unavailable", out.GroupAliases.Status)
	assert.Len(t, f.caller.Reviews, 1, "only the caller's identity review")
}

// TestCanImpersonatedPathIgnoresUnavailableGroups asserts the server owns
// the groups of an impersonated review: the answer is unchanged, with no
// warning and no groupAliases.
func TestCanImpersonatedPathIgnoresUnavailableGroups(t *testing.T) {
	serialFanOut(t)
	alice := &fakeClient{script: []scripted{{resp: whoamiResp("u-abc12", "system:authenticated")}, {resp: sar(false, "")}}}
	f := canFactory("user-vwq4j")
	f.impersonated = map[string]*fakeClient{"u-abc12": alice}
	res, err := newCanOver(f, subjectLocal(groupsRefused)).(runtime.ReadCommand).Execute(t.Context(),
		canInput(map[string]string{"namespace": "team-a", "as": "u-abc12"}, "get", "pods"), nil)
	require.NoError(t, err)
	out := decodeCan(t, res)
	assert.Equal(t, IdentityImpersonated, out.Identity)
	assert.Equal(t, DecisionDenied, out.Decision, "a denied answer stays denied")
	assert.Equal(t, exitDenied, res.ExitStatus)
	assert.Empty(t, res.Warnings)
	assert.Nil(t, out.GroupAliases)
	assert.NotContains(t, res.Text, "groups from")
}

// TestNoClusterAccessNoteClause asserts the "no binding grants it" clause
// is stated only with the evidence of the cached groups.
func TestNoClusterAccessNoteClause(t *testing.T) {
	const clause = " (no ClusterRoleTemplateBinding or ProjectRoleTemplateBinding grants it)"
	for _, g := range []groupsSetup{groupsRead, groupsNone, groupsRefused} {
		t.Run(g.String(), func(t *testing.T) {
			t.Parallel()
			f := callerFactory("user-vwq4j", map[string]*fakeClient{"u-abc12": {script: []scripted{{resp: http403("")}}}})
			p, _ := ParseSubject("u-abc12", "")
			plan, err := planIdentityOver(t.Context(), f, f.caller, subjectLocal(g), "c-phmj7", callerIdentity{Username: "user-vwq4j"}, p, false)
			require.NoError(t, err)
			require.NotNil(t, plan.Denied)
			assert.Equal(t, g != groupsRefused, strings.HasSuffix(plan.Denied.Note, clause), plan.Denied.Note)
			assert.Empty(t, plan.Warnings)
			assert.Equal(t, g == groupsRefused, plan.GroupsUnavailable != nil)
		})
	}
}

// TestListNoClusterAccessNoteClauseNeedsTheGroups asserts access list words
// the denial like the others.
func TestListNoClusterAccessNoteClauseNeedsTheGroups(t *testing.T) {
	t.Parallel()
	f := listFactory("user-vwq4j")
	f.impersonated = map[string]*fakeClient{"u-abc12": {script: []scripted{{resp: http403("")}}}}
	local := subjectLocal(groupsRefused)
	res, err := NewList(f, attrRootsFor(local), local).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a", "as": "u-abc12"}), nil)
	require.NoError(t, err)
	out := decodeList(t, res)
	assert.Equal(t, "u-abc12 has no access to cluster c-phmj7", out.Note)
	assert.Empty(t, res.Warnings)
}

// TestListImpersonatedAdmittedIgnoresUnavailableGroups asserts access list
// is unchanged and prints no warning when the groups are unavailable.
func TestListImpersonatedAdmittedIgnoresUnavailableGroups(t *testing.T) {
	t.Parallel()
	f := listFactory("user-vwq4j")
	f.impersonated = map[string]*fakeClient{"u-abc12": {script: []scripted{
		{resp: whoamiResp("u-abc12", "system:authenticated")}, {resp: rules(false)},
	}}}
	local := subjectLocal(groupsRefused)
	res, err := NewList(f, attrRootsFor(local), local).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a", "as": "u-abc12"}), nil)
	require.NoError(t, err)
	assert.Empty(t, res.Warnings)
	assert.Equal(t, 0, res.ExitStatus)
}

// TestPlanIdentityCallerSubjectCarriesNoGroupsWhenUnavailable asserts that
// naming yourself with unavailable groups leaves the plan without groups,
// a marker or a warning: the review reports the caller's own groups.
func TestPlanIdentityCallerSubjectCarriesNoGroupsWhenUnavailable(t *testing.T) {
	t.Parallel()
	for _, g := range []groupsSetup{groupsRead, groupsRefused} {
		t.Run(g.String(), func(t *testing.T) {
			t.Parallel()
			f := callerFactory("u-abc12", nil)
			p, _ := ParseSubject("u-abc12", "")
			plan, err := planIdentityOver(t.Context(), f, f.caller, subjectLocal(g), "c-phmj7", callerIdentity{Username: "u-abc12"}, p, false)
			require.NoError(t, err)
			assert.True(t, plan.AsCaller)
			assert.Nil(t, plan.GroupsUnavailable)
			assert.Empty(t, plan.Warnings)
			if g == groupsRefused {
				assert.Empty(t, plan.Groups)
			} else {
				assert.NotEmpty(t, plan.Groups)
			}
		})
	}
}

// explainChainFixtures is explainFixtures for the subject u-abc12: the
// binding and the role the reason names, and the Rancher objects behind it.
func explainChainFixtures(script ...scripted) (*fakeFactory, *fakeClient) {
	f := canFactory("user-vwq4j", script...)
	f.caller.Objects["team-a/rb-1"] = chainObj("rbac.authorization.k8s.io/v1", "RoleBinding", "team-a", "rb-1",
		map[string]string{rtbOwnerLabel: "c-phmj7-p-cvrp4_prtb-1"}, nil,
		map[string]any{"roleRef": rbacRoleRef("ClusterRole", "project-member-promoted"), "subjects": []any{rbacSubject("User", "u-abc12", "")}})
	f.caller.Objects["project-member-promoted"] = chainObj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "project-member-promoted", nil, nil,
		map[string]any{"rules": podsRule("get", "list", "delete")})
	f.caller.Lists["rolebindings|team-a|"] = &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*f.caller.Objects["team-a/rb-1"]}}
	f.caller.Lists["clusterrolebindings||"] = &unstructured.UnstructuredList{}
	return f, f.caller
}

func seedSubjectChain(local *fakeClient) {
	local.Objects["c-phmj7-p-cvrp4/prtb-1"] = chainObj("management.cattle.io/v3", "ProjectRoleTemplateBinding", "c-phmj7-p-cvrp4", "prtb-1", nil, nil,
		map[string]any{"userName": "u-abc12", "roleTemplateName": "project-member", "projectName": "c-phmj7:p-cvrp4"})
	local.Objects["project-member"] = chainObj("management.cattle.io/v3", "RoleTemplate", "", "project-member", nil, nil, map[string]any{"roleTemplateNames": []any{"edit"}})
	local.Objects["edit"] = chainObj("management.cattle.io/v3", "RoleTemplate", "", "edit", nil, nil, map[string]any{"external": true})
}

const subjectReason = `RBAC: allowed by RoleBinding "rb-1/team-a" of ClusterRole "project-member-promoted" to User "u-abc12"`

// TestExplainChainStateWithUnavailableGroups asserts the chain is never
// complete when the cached groups were not read, per path: the issue is the
// refusal, the state is partial where the chain was walked or the proxy
// denied, and unknown where the review turned unknown.
func TestExplainChainStateWithUnavailableGroups(t *testing.T) {
	type row struct {
		name     string
		groups   groupsSetup
		setup    func() (*fakeFactory, *fakeClient)
		decision Decision
		exit     int
		chain    chainState
		issue    bool
		warning  bool
	}
	subjectSAR := func(s scripted) func() (*fakeFactory, *fakeClient) {
		return func() (*fakeFactory, *fakeClient) {
			f, local := explainChainFixtures(s)
			f.impersonated = refusedImpersonation()
			return f, local
		}
	}
	impersonated := func(answer scripted) func() (*fakeFactory, *fakeClient) {
		return func() (*fakeFactory, *fakeClient) {
			f, local := explainChainFixtures()
			f.impersonated = map[string]*fakeClient{"u-abc12": {script: []scripted{{resp: whoamiResp("u-abc12", "system:authenticated")}, answer}}}
			return f, local
		}
	}
	tests := []row{
		{name: "proxy denial", groups: groupsRefused, decision: DecisionDenied, exit: exitDenied, chain: chainPartial, issue: true,
			setup: func() (*fakeFactory, *fakeClient) {
				f, local := explainChainFixtures()
				f.impersonated = map[string]*fakeClient{"u-abc12": {script: []scripted{{resp: http403("")}}}}
				return f, local
			}},
		{name: "impersonated allowed", groups: groupsRefused, decision: DecisionAllowed, chain: chainPartial, issue: true, setup: impersonated(scripted{resp: sar(true, subjectReason)})},
		{name: "impersonated denied", groups: groupsRefused, decision: DecisionDenied, exit: exitDenied, chain: chainPartial, issue: true, setup: impersonated(scripted{resp: sar(false, "")})},
		{name: "review allowed", groups: groupsRefused, decision: DecisionAllowed, chain: chainPartial, issue: true, warning: true,
			setup: subjectSAR(scripted{resp: review(engine.ReviewSubjectAccess, map[string]any{"allowed": true, "reason": subjectReason})})},
		{name: "review denied becomes unknown", groups: groupsRefused, decision: DecisionUnknown, exit: exitUnknown, chain: chainUnknown, issue: true, warning: true,
			setup: subjectSAR(scripted{resp: review(engine.ReviewSubjectAccess, map[string]any{"allowed": false})})},
		{name: "groups read, review allowed", groups: groupsRead, decision: DecisionAllowed, chain: chainComplete,
			setup: subjectSAR(scripted{resp: review(engine.ReviewSubjectAccess, map[string]any{"allowed": true, "reason": subjectReason})})},
		{name: "no object, impersonated allowed", groups: groupsNone, decision: DecisionAllowed, chain: chainComplete, setup: impersonated(scripted{resp: sar(true, subjectReason)})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			serialFanOut(t)
			f, local := tc.setup()
			seedSubjectChain(local)
			local.Objects["u-abc12"] = userObj("u-abc12", "alice", "alice display", "local://u-abc12")
			switch tc.groups {
			case groupsRead:
				local.Objects["u-abc12-attr"] = userAttrObj("u-abc12", "okta_group://eng")
			case groupsRefused:
				local.AttrRefusal = attrNoRight
			}
			res, err := NewExplain(f, attrRootsFor(local), local).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a", "as": "u-abc12"}, "delete", "pods"), nil)
			require.NoError(t, err)
			out := decodeExplain(t, res)
			assert.Equal(t, tc.decision, out.Decision)
			assert.Equal(t, tc.exit, res.ExitStatus)
			assert.Equal(t, tc.chain, out.Chain)
			var issues []chainIssue
			for _, i := range out.ChainIssues {
				if i.Object == "userattribute u-abc12" {
					issues = append(issues, i)
				}
			}
			if !tc.issue {
				assert.Empty(t, issues)
				assert.Empty(t, out.ChainIssues)
				return
			}
			require.Len(t, issues, 1)
			assert.Equal(t, stageManagementRead, issues[0].Stage)
			assert.Equal(t, clierr.CodeRBAC, issues[0].Code)
			assert.Equal(t, m1For404, issues[0].Message)
			if tc.warning {
				assert.Equal(t, []string{w1}, res.Warnings)
				assert.Contains(t, res.Text, " | groups from rancherUserAttribute (unavailable)\n")
			} else {
				assert.Empty(t, res.Warnings)
			}
			assert.Contains(t, res.Text, "\nchain: "+string(tc.chain))
		})
	}
}

// TestExplainSelfWithUnavailableGroups asserts naming yourself needs no
// groups: no warning, no chain issue, and no groupAliases.
func TestExplainSelfWithUnavailableGroups(t *testing.T) {
	t.Parallel()
	f, local := explainChainFixtures(scripted{resp: sar(true, subjectReason)})
	f.caller.script = []scripted{{resp: whoamiResp("u-abc12", "system:authenticated")}, {resp: sar(true, subjectReason)}}
	seedSubjectChain(local)
	local.Objects["u-abc12"] = userObj("u-abc12", "alice", "alice display", "local://u-abc12")
	local.AttrRefusal = attrNoRight
	res, err := NewExplain(f, attrRootsFor(local), local).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a", "as": "u-abc12"}, "delete", "pods"), nil)
	require.NoError(t, err)
	out := decodeExplain(t, res)
	assert.Equal(t, IdentityCaller, out.Identity)
	assert.Empty(t, res.Warnings)
	assert.Empty(t, out.ChainIssues)
	assert.Nil(t, out.GroupAliases)
	assert.Equal(t, chainComplete, out.Chain)
}

// TestReadProvenanceUserAttributeOutcomes asserts a refused UserAttribute is
// an issue with the refusal, and a 401 ends the run.
func TestReadProvenanceUserAttributeOutcomes(t *testing.T) {
	t.Parallel()
	in := provenanceInput{Subject: AccessSubject{Kind: KindUser, Name: "u-alice"}}
	t.Run("refused is an issue", func(t *testing.T) {
		t.Parallel()
		fc := provenanceFixture()
		fc.AttrRefusal = attrNoRight
		p, err := readProvenanceOver(t.Context(), fc, in)
		require.NoError(t, err)
		assert.Equal(t, "unavailable", p.GroupAliases.Status)
		assert.Equal(t, "partial", p.Discovery.Status)
		require.NotEmpty(t, p.Discovery.Issues)
		var found bool
		for _, i := range p.Discovery.Issues {
			if i.Stage == stageUserAttribute {
				found = true
				assert.Equal(t, clierr.CodeRBAC, i.Issue.Code)
				assert.Equal(t, "you cannot read userattributes (GET /v1/management.cattle.io.userattributes/u-alice answered 404); Rancher's API shows you no userattributes type"+userAttributesRemedyText, i.Issue.Message)
			}
		}
		assert.True(t, found)
	})
	t.Run("401 is terminal", func(t *testing.T) {
		t.Parallel()
		fc := provenanceFixture()
		fc.GetErrFor = map[string]error{"u-alice-attr": clierr.New(clierr.CodeAuth, "expired")}
		_, err := readProvenanceOver(t.Context(), fc, in)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
	})
	t.Run("a steve 401 is terminal", func(t *testing.T) {
		t.Parallel()
		fc := provenanceFixture()
		attrs := steve.NewUserAttributes(func(context.Context, steve.Request) ([]byte, error) {
			return nil, clierr.ResponseError("GET", 401, nil)
		}, fc)
		_, err := readProvenance(t.Context(), fc, attrs, in)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
	})
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
