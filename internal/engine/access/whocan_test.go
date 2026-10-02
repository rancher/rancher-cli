package access

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
)

func decodeSubjects(t *testing.T, res runtime.Result) subjectsResult {
	t.Helper()
	b, err := json.Marshal(res.Data)
	require.NoError(t, err)
	var out subjectsResult
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

// serialFanOut pins the fan-out to one worker so the scripted review
// answers are consumed in sorted candidate order, row by row.
func serialFanOut(t *testing.T) {
	t.Helper()
	old := fanOutLimit
	fanOutLimit = 1
	t.Cleanup(func() { fanOutLimit = old })
}

// whoCanFactory serves everything a who-can run reads from one client
// (the caller's cluster client doubles as the management client): the
// caller whoami, discovery, namespaces, the bindings of
// bindingsClient, the Rancher Users and UserAttributes the candidates
// resolve against, and the UserAttribute LIST behind knownMembers. The
// scripted answers are the SubjectAccessReviews, in row order.
func whoCanFactory(script ...scripted) *fakeFactory {
	c := bindingsClient()
	c.script = append([]scripted{{resp: whoamiResp("user-vwq4j", "system:authenticated", "system:cattle:authenticated")}}, script...)
	c.Resources = discovery()
	c.Objects = map[string]*unstructured.Unstructured{
		"team-a":       new(nsObj("team-a", nil)),
		"team-b":       new(nsObj("team-b", nil)),
		"user-vwq4j":   userObj("user-vwq4j", "admin", "Admin", "local://user-vwq4j"),
		"u-abc12":      userObj("u-abc12", "alice", "Alice", "local://u-abc12"),
		"u-abc12-attr": userAttrObj("u-abc12", "github_org://eng"),
	}
	c.Lists["namespaces||"] = &unstructured.UnstructuredList{Items: []unstructured.Unstructured{nsObj("team-a", nil), nsObj("team-b", nil)}}
	c.Lists["namespaces||field.cattle.io/projectId=p-cvrp4"] = &unstructured.UnstructuredList{Items: []unstructured.Unstructured{nsObj("team-a", nil), nsObj("team-b", nil)}}
	// u-def34's cache also names system:masters: the fixed row must never
	// list it, while the bound group still does.
	c.Lists["userattributes||"] = &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*userAttrObj("u-abc12", "github_org://eng"), *userAttrObj("u-def34", "github_org://eng", mastersGroup),
	}}
	return &fakeFactory{caller: c, resolve: map[string]string{"prod": "c-phmj7"}}
}

func sarAnswer(allowed bool, reason string) scripted {
	return scripted{resp: review(engine.ReviewSubjectAccess, map[string]any{"allowed": allowed, "reason": reason})}
}

// The reasons below follow the authorizer's grammar exactly as recorded
// against the oracle; system:masters is allowed with no reason at all.
const (
	adminReason = `RBAC: allowed by ClusterRoleBinding "globaladmin-user-vwq4j" of ClusterRole "cluster-admin" to User "user-vwq4j"`
	aliceReason = `RBAC: allowed by RoleBinding "team-a/rb-1" of ClusterRole "view" to User "u-abc12"`
)

// mastersAnswer is what the authorizer says about system:masters: allowed
// before RBAC is consulted, so no reason.
func mastersAnswer() scripted { return sarAnswer(true, "") }

// assertDecisionColumn asserts every rendered row's decision starts at the
// column the DECISION header does, whatever the names in between are long.
func assertDecisionColumn(t *testing.T, text string) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	header := -1
	for i, l := range lines {
		if strings.Contains(l, "DECISION") {
			header = i
			break
		}
	}
	require.GreaterOrEqual(t, header, 0, text)
	col := strings.Index(lines[header], "DECISION")
	widths := map[int]bool{}
	rows := 0
	for _, l := range lines[header+1:] {
		fields := strings.Fields(l)
		if len(fields) == 0 || !slices.Contains([]string{"group", "user", "serviceaccount"}, fields[0]) {
			continue
		}
		at := -1
		for _, d := range []string{"allowed", "denied", "unknown"} {
			if i := strings.Index(l, " "+d); i >= 0 && (at < 0 || i < at) {
				at = i + 1
			}
		}
		require.GreaterOrEqual(t, at, 0, l)
		assert.Equal(t, col, at, l)
		widths[len(fields[1])] = true
		rows++
	}
	assert.GreaterOrEqual(t, rows, 2, text)
	assert.Greater(t, len(widths), 1, "the rows compared must have names of different lengths")
}

func rowByName(t *testing.T, out subjectsResult, name string) subjectRow {
	t.Helper()
	for _, r := range out.Subjects {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no row named %s in %+v", name, out.Subjects)
	return subjectRow{}
}

// TestWhoCanSingleNamespaceRowsAndRollup asserts the single-namespace
// shape: one SubjectAccessReview per candidate as the caller with the
// identity table's user and groups, rows in candidate order carrying
// display name, id, groupsFrom, via, and knownMembers, the rollup, exit
// 0, and the text view with denied rows hidden until --show-denied.
func TestWhoCanSingleNamespaceRowsAndRollup(t *testing.T) {
	serialFanOut(t)
	// candidates in team-a, sorted: group github_org://eng, group
	// system:masters, serviceaccount team-a:ci, user u-abc12, user user-vwq4j
	f := whoCanFactory(
		sarAnswer(true, `RBAC: allowed by ClusterRoleBinding "crb-1" of ClusterRole "view" to Group "github_org://eng"`),
		mastersAnswer(),
		sarAnswer(false, ""),
		sarAnswer(true, aliceReason),
		sarAnswer(true, adminReason),
	)
	cmd := NewWhoCan(f, f.caller).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "get", "pods"), nil)
	require.NoError(t, err)
	out := decodeSubjects(t, res)

	assert.Equal(t, 0, res.ExitStatus)
	assert.Equal(t, IdentitySAR, out.Identity)
	assert.Equal(t, SourceAuthorizer, out.ResultSource)
	assert.Equal(t, "rolebindings and clusterrolebindings on c-phmj7 (+ system:masters)", out.CandidateSource)
	assert.Equal(t, subjectsRollup{Candidates: 5, Rows: 5, Evaluated: 5, Allowed: 4, Denied: 1,
		OutsideCandidateSet: []string{"system:masters members", "non-RBAC authorizers"}}, out.Rollup)
	require.Len(t, out.Subjects, 5)

	eng := out.Subjects[0]
	assert.Equal(t, KindGroup, eng.Kind)
	assert.Equal(t, GroupsGroupOnly, eng.GroupsFrom)
	assert.Equal(t, []string{"u-abc12", "u-def34"}, eng.KnownMembers)
	assert.Equal(t, []string{"direct"}, eng.Via)
	assert.Empty(t, eng.Namespace, "a single namespace is scope, not a row field")

	masters := out.Subjects[1]
	assert.Equal(t, "system:masters", masters.Name)
	assert.Equal(t, DecisionAllowed, masters.Decision)
	assert.Equal(t, "", masters.Reason, "the privileged group is allowed without a reason")
	assert.Nil(t, masters.Via)
	assert.Contains(t, masters.Note, "fixed candidate")
	assert.Nil(t, masters.KnownMembers, "the fixed row never lists cached members, even when a cache names system:masters")
	assert.Equal(t, []string{"u-abc12", "u-def34"}, eng.KnownMembers, "the same cached user still counts for the bound group")

	alice := out.Subjects[3]
	assert.Equal(t, AccessSubject{Kind: KindUser, Name: "u-abc12", ID: "u-abc12", DisplayName: "Alice"},
		AccessSubject{Kind: alice.Kind, Name: alice.Name, ID: alice.ID, DisplayName: alice.DisplayName})
	assert.Equal(t, GroupsRancherUserAttribute, alice.GroupsFrom)
	assert.Equal(t, []string{"direct", "group github_org://eng"}, alice.Via, "a member of a bound group shows both sources")
	assert.Equal(t, aliceReason, alice.Reason)

	sa := out.Subjects[2]
	assert.Equal(t, DecisionDenied, sa.Decision)
	assert.Equal(t, "", sa.Reason)

	require.Len(t, f.caller.Reviews, 6, "one whoami and one SubjectAccessReview per candidate")
	for _, r := range f.caller.Reviews[1:] {
		assert.Equal(t, engine.ReviewSubjectAccess, r.Kind)
	}
	groupSpec, _, _ := unstructured.NestedMap(f.caller.Reviews[1].Object.Object, "spec")
	_, hasUser := groupSpec["user"]
	assert.False(t, hasUser, "a group is asked by groups alone")
	assert.Equal(t, []any{"github_org://eng"}, groupSpec["groups"])
	aliceSpec, _, _ := unstructured.NestedMap(f.caller.Reviews[4].Object.Object, "spec")
	assert.Equal(t, "u-abc12", aliceSpec["user"])
	assert.Equal(t, []any{"github_org://eng", "system:authenticated", "system:cattle:authenticated"}, aliceSpec["groups"])
	ra, _, _ := unstructured.NestedMap(f.caller.Reviews[4].Object.Object, "spec", "resourceAttributes")
	assert.Equal(t, "team-a", ra["namespace"])
	assert.Equal(t, "v1", ra["version"])

	assert.Contains(t, res.Warnings, groupsCacheWarning)
	assert.True(t, strings.HasPrefix(res.Text, "who can get pods in team-a on c-phmj7\n"), res.Text)
	assert.Contains(t, res.Text, "u-abc12")
	assert.Contains(t, res.Text, "known members: u-abc12, u-def34")
	assert.Contains(t, res.Text, "\n  fixed: the apiserver allows system:masters before RBAC is consulted", res.Text)
	assert.Equal(t, 1, strings.Count(res.Text, "the apiserver allows system:masters"), "the fixed candidate's note is a footnote, printed once")
	assertDecisionColumn(t, res.Text)
	assert.Contains(t, res.Text, "1 denied not shown; pass --show-denied to list them")
	assert.NotContains(t, res.Text, "system:serviceaccount:team-a:ci")
	assert.True(t, strings.HasSuffix(res.Text, "source: authorizer | identity: subjectAccessReview\n"), res.Text)

	f = whoCanFactory(
		sarAnswer(true, ""), mastersAnswer(), sarAnswer(false, ""), sarAnswer(true, aliceReason), sarAnswer(true, adminReason),
	)
	res, err = NewWhoCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(),
		fakeInput{strs: map[string]string{"cluster": "prod", "namespace": "team-a"}, bools: map[string]bool{"show-denied": true}, args: []string{"get", "pods"}}, nil)
	require.NoError(t, err)
	assert.Contains(t, res.Text, "system:serviceaccount:team-a:ci")
	assert.NotContains(t, res.Text, "denied not shown")
}

// TestWhoCanUnknownGroupsNeverDriveTheExit asserts a binding username with
// no Rancher User is asked with system:authenticated only: allowed is a
// definite answer, denied is unknown with the groups-unknown note, counted
// in groupsUnknown, folded into one text line, and exit stays 0.
func TestWhoCanUnknownGroupsNeverDriveTheExit(t *testing.T) {
	serialFanOut(t)
	// candidates in team-b, sorted: group github_org://eng, group
	// system:masters, user oidc-external@corp, user u-abc12, user user-vwq4j
	f := whoCanFactory(sarAnswer(false, ""), mastersAnswer(), sarAnswer(false, ""), sarAnswer(false, ""), sarAnswer(true, adminReason))
	res, err := NewWhoCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-b"}, "get", "pods"), nil)
	require.NoError(t, err)
	out := decodeSubjects(t, res)
	ext := rowByName(t, out, "oidc-external@corp")
	assert.Equal(t, DecisionUnknown, ext.Decision)
	assert.Equal(t, groupsUnknownNote, ext.Note)
	assert.Equal(t, GroupsAuthenticatedOnly, ext.GroupsFrom)
	assert.Empty(t, ext.ID)
	assert.Equal(t, MemberAuthorizer, ext.Source)
	assert.Equal(t, 1, out.Rollup.Unknown)
	assert.Equal(t, 1, out.Rollup.GroupsUnknown)
	assert.Equal(t, 0, out.Rollup.Issues)
	assert.Equal(t, 0, res.ExitStatus)
	assert.Contains(t, res.Text, "1 subject(s) with unknown groups could not be ruled out: oidc-external@corp")
	spec, _, _ := unstructured.NestedMap(f.caller.Reviews[3].Object.Object, "spec")
	assert.Equal(t, "oidc-external@corp", spec["user"])
	assert.Equal(t, []any{"system:authenticated"}, spec["groups"])

	f = whoCanFactory(sarAnswer(false, ""), mastersAnswer(),
		sarAnswer(true, `RBAC: allowed by RoleBinding "team-b/rb-2" of ClusterRole "view" to User "oidc-external@corp"`),
		sarAnswer(false, ""), sarAnswer(true, adminReason))
	res, err = NewWhoCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-b"}, "get", "pods"), nil)
	require.NoError(t, err)
	out = decodeSubjects(t, res)
	assert.Equal(t, DecisionAllowed, rowByName(t, out, "oidc-external@corp").Decision, "a positive answer needs no groups")
	assert.Equal(t, 0, out.Rollup.GroupsUnknown)
}

// TestWhoCanEvaluationErrorAndRequestIssueExitThirteen asserts the two
// unknowns that DO mean the answer is incomplete: an authorizer evaluation
// error (unknown, not groupsUnknown) and a request failure (a row with an
// issue, source request, resultSource mixed). Both exit 13.
func TestWhoCanEvaluationErrorAndRequestIssueExitThirteen(t *testing.T) {
	serialFanOut(t)
	f := whoCanFactory(sarAnswer(false, ""), mastersAnswer(), sarAnswer(false, ""),
		scripted{resp: review(engine.ReviewSubjectAccess, map[string]any{"allowed": false, "evaluationError": "webhook unreachable"})},
		sarAnswer(true, adminReason))
	res, err := NewWhoCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "get", "pods"), nil)
	require.NoError(t, err)
	out := decodeSubjects(t, res)
	alice := rowByName(t, out, "u-abc12")
	assert.Equal(t, DecisionUnknown, alice.Decision)
	assert.Equal(t, "webhook unreachable", alice.EvaluationError)
	assert.Equal(t, 1, out.Rollup.Unknown)
	assert.Equal(t, 0, out.Rollup.GroupsUnknown)
	assert.Equal(t, exitUnknown, res.ExitStatus)
	assert.Contains(t, res.Text, "evaluation error: webhook unreachable")

	f = whoCanFactory(sarAnswer(false, ""), mastersAnswer(),
		scripted{err: clierr.Retryable(clierr.CodeGeneric, "POST subjectaccessreviews: 503 Service Unavailable")},
		sarAnswer(true, aliceReason), sarAnswer(true, adminReason))
	res, err = NewWhoCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "get", "pods"), nil)
	require.NoError(t, err)
	out = decodeSubjects(t, res)
	sa := rowByName(t, out, "system:serviceaccount:team-a:ci")
	assert.Equal(t, MemberRequest, sa.Source)
	assert.Equal(t, DecisionUnknown, sa.Decision)
	require.NotNil(t, sa.Issue)
	assert.True(t, sa.Issue.Retryable)
	assert.Equal(t, GroupsServiceAccountIdentity, sa.GroupsFrom, "the identity was known; the review failed")
	assert.Equal(t, 1, out.Rollup.Issues)
	assert.Equal(t, 4, out.Rollup.Evaluated)
	assert.Equal(t, SourceMixed, out.ResultSource)
	assert.Equal(t, exitUnknown, res.ExitStatus)
}

// TestWhoCanAllowedWithEvaluationErrorWarnsAndKeepsRows asserts an allowed
// answer that also carries an evaluation error stays allowed (the decision
// mapping), exits 0, emits one warning naming the subject and namespace,
// and never collapses with the subject's other rows, so the error stays
// attributable to its namespace.
func TestWhoCanAllowedWithEvaluationErrorWarnsAndKeepsRows(t *testing.T) {
	serialFanOut(t)
	withErr := scripted{resp: review(engine.ReviewSubjectAccess, map[string]any{"allowed": true, "reason": adminReason, "evaluationError": "webhook slow"})}
	f := whoCanFactory(
		sarAnswer(false, ""), sarAnswer(false, ""),
		mastersAnswer(), mastersAnswer(),
		sarAnswer(false, ""), sarAnswer(false, ""),
		sarAnswer(false, ""), sarAnswer(false, ""),
		sarAnswer(false, ""), sarAnswer(false, ""),
		sarAnswer(true, adminReason), withErr,
	)
	res, err := NewWhoCan(f, f.caller).(runtime.ReadCommand).Execute(projectContext(t, f.caller, "p-cvrp4", "Team"), canInput(map[string]string{"project": "p-cvrp4"}, "get", "pods"), nil)
	require.NoError(t, err)
	out := decodeSubjects(t, res)
	assert.Equal(t, 0, res.ExitStatus)
	var admin []subjectRow
	for _, r := range out.Subjects {
		if r.Name == "user-vwq4j" {
			admin = append(admin, r)
		}
	}
	require.Len(t, admin, 2, "no collapse while a row carries an evaluation error")
	assert.Equal(t, "team-b", admin[1].Namespace)
	assert.Equal(t, DecisionAllowed, admin[1].Decision)
	assert.Equal(t, "webhook slow", admin[1].EvaluationError)
	assert.Equal(t, out.Rollup.GroupsUnknown, out.Rollup.Unknown, "the error on an allowed row is not an unknown row")
	assert.Contains(t, res.Warnings, "the authorizer reported an evaluation error for user-vwq4j in team-b: webhook slow")
	assert.Contains(t, res.Text, "evaluation error: webhook slow")
}

// TestWhoCanIdentityReadFailureIsTheRowsIssue asserts a management-plane
// read failure private to a candidate (its User GET) is that row's issue:
// no review is posted for it, the row is unknown with source request and
// no groupsFrom, and the other candidates are answered.
func TestWhoCanIdentityReadFailureIsTheRowsIssue(t *testing.T) {
	serialFanOut(t)
	f := whoCanFactory(sarAnswer(false, ""), mastersAnswer(), sarAnswer(false, ""))
	local := &fakeClient{GetErr: clierr.Retryable(clierr.CodeGeneric, "GET users: 503 Service Unavailable")}
	res, err := NewWhoCan(f, local).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "get", "pods"), nil)
	require.NoError(t, err)
	out := decodeSubjects(t, res)
	require.Len(t, f.caller.Reviews, 4, "whoami plus one review per candidate whose identity resolved")
	for _, name := range []string{"u-abc12", "user-vwq4j"} {
		row := rowByName(t, out, name)
		assert.Equal(t, MemberRequest, row.Source, name)
		assert.Equal(t, DecisionUnknown, row.Decision, name)
		require.NotNil(t, row.Issue, name)
		assert.Contains(t, row.Issue.Message, "503 Service Unavailable")
		assert.Empty(t, row.GroupsFrom, name)
	}
	assert.Equal(t, 2, out.Rollup.Issues)
	assert.Equal(t, 3, out.Rollup.Evaluated)
	assert.Equal(t, exitUnknown, res.ExitStatus)
	assert.Contains(t, res.Warnings, groupsCacheWarning, "a bound group is a candidate")
	for _, w := range res.Warnings {
		assert.False(t, strings.HasPrefix(w, "group members not expanded"), "the management LIST succeeded (empty): %s", w)
	}
}

// TestWhoCanGroupMembersFailureIsAWarning asserts a failed UserAttribute
// LIST leaves every row and the exit code alone and adds one warning.
func TestWhoCanGroupMembersFailureIsAWarning(t *testing.T) {
	serialFanOut(t)
	f := whoCanFactory(sarAnswer(true, ""), mastersAnswer(), sarAnswer(false, ""), sarAnswer(true, aliceReason), sarAnswer(true, adminReason))
	local := &fakeClient{Objects: f.caller.Objects, ListErr: clierr.New(clierr.CodeRBAC, "userattributes is forbidden")}
	res, err := NewWhoCan(f, local).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "get", "pods"), nil)
	require.NoError(t, err)
	out := decodeSubjects(t, res)
	assert.Equal(t, 0, res.ExitStatus)
	assert.Nil(t, rowByName(t, out, "github_org://eng").KnownMembers)
	assert.Equal(t, []string{"direct"}, rowByName(t, out, "u-abc12").Via)
	assert.Contains(t, res.Warnings, "group members not expanded: userattributes is forbidden")
}

// TestWhoCanNoBoundGroupSkipsTheMembershipCache asserts that when no
// binding names a group (users and the fixed system:masters row only), the
// management UserAttribute LIST is not made and neither membership warning
// is raised, even though that LIST would fail.
func TestWhoCanNoBoundGroupSkipsTheMembershipCache(t *testing.T) {
	serialFanOut(t)
	f := whoCanFactory(sarAnswer(true, ""), sarAnswer(true, `RBAC: allowed by ClusterRoleBinding "system:kube-scheduler" of ClusterRole "system:kube-scheduler" to User "system:kube-scheduler"`))
	f.caller.Lists["clusterrolebindings||"] = &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		bindingObj("ClusterRoleBinding", "", "system:kube-scheduler", rbacSubject("User", "system:kube-scheduler", "")),
	}}
	f.caller.Lists["rolebindings|team-a|"] = &unstructured.UnstructuredList{}
	local := &fakeClient{ListErr: clierr.New(clierr.CodeRBAC, "userattributes is forbidden")}
	res, err := NewWhoCan(f, local).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "get", "pods"), nil)
	require.NoError(t, err)
	out := decodeSubjects(t, res)
	assert.Equal(t, 2, out.Rollup.Candidates, "system:masters and the scheduler")
	assert.Empty(t, res.Warnings, "no bound group: no membership LIST, no cache warning")
	assert.Equal(t, 0, res.ExitStatus)
}

// TestWhoCanKubernetesGroupSkipsTheMembershipCache asserts a binding that
// names one of Kubernetes' own system: groups is answered like any other
// candidate while costing no membership request: Rancher caches the
// principals of identity-provider groups only, so such a row can carry no
// members and its absence is not a degradation.
func TestWhoCanKubernetesGroupSkipsTheMembershipCache(t *testing.T) {
	serialFanOut(t)
	f := whoCanFactory(sarAnswer(true, ""), sarAnswer(true, `RBAC: allowed by ClusterRoleBinding "authenticated-view" of ClusterRole "view" to Group "system:authenticated"`))
	f.caller.Lists["clusterrolebindings||"] = &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		bindingObj("ClusterRoleBinding", "", "authenticated-view", rbacSubject("Group", "system:authenticated", "")),
	}}
	f.caller.Lists["rolebindings|team-a|"] = &unstructured.UnstructuredList{}
	local := &fakeClient{ListErr: clierr.New(clierr.CodeRBAC, "userattributes is forbidden")}
	res, err := NewWhoCan(f, local).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "get", "pods"), nil)
	require.NoError(t, err)
	out := decodeSubjects(t, res)
	assert.Equal(t, 2, out.Rollup.Candidates, "system:masters and the authenticated group")
	row := rowByName(t, out, "system:authenticated")
	assert.Equal(t, DecisionAllowed, row.Decision, "the group still gets its own decision")
	assert.Nil(t, row.KnownMembers, "Rancher caches no members for a Kubernetes group")
	assert.Empty(t, res.Warnings, "no identity-provider group: no membership LIST, no cache warning")
	assert.Equal(t, 0, res.ExitStatus)
}

// TestWhoCanProjectRowsAndClusterWideCollapse asserts the several-namespace
// shape: one row per subject per namespace in namespace order, rollup
// counts over every pair, and a subject whose allowed reason names the
// same ClusterRoleBinding in every namespace collapsing to namespace "*",
// while a per-namespace grant, a mixed answer, and the fixed system:masters
// row (allowed with no reason) keep their rows. --project is typed as the
// display name; the scope and the text name the project's id.
func TestWhoCanProjectRowsAndClusterWideCollapse(t *testing.T) {
	serialFanOut(t)
	// candidates for p-cvrp4 (team-a, team-b), sorted: group eng, group
	// masters, serviceaccount team-a:ci, user oidc-external@corp, user
	// u-abc12, user user-vwq4j; two rows each, team-a then team-b
	f := whoCanFactory(
		sarAnswer(false, ""), sarAnswer(false, ""),
		mastersAnswer(), mastersAnswer(),
		sarAnswer(false, ""), sarAnswer(false, ""),
		sarAnswer(false, ""), sarAnswer(false, ""),
		sarAnswer(true, aliceReason), sarAnswer(false, ""),
		sarAnswer(true, adminReason), sarAnswer(true, adminReason),
	)
	res, err := NewWhoCan(f, f.caller).(runtime.ReadCommand).Execute(projectContext(t, f.caller, "p-cvrp4", "Team"), canInput(map[string]string{"project": "Team"}, "get", "pods"), nil)
	require.NoError(t, err)
	out := decodeSubjects(t, res)
	assert.Equal(t, subjectsRollup{Candidates: 6, Rows: 12, Evaluated: 12, Allowed: 5, Denied: 5, Unknown: 2, GroupsUnknown: 2,
		OutsideCandidateSet: []string{"system:masters members", "non-RBAC authorizers"}}, out.Rollup)
	assert.Equal(t, 0, res.ExitStatus)
	assert.Equal(t, map[string]any{"cluster": "c-phmj7", "project": "p-cvrp4", "namespaces": []any{"team-a", "team-b"}}, scopeJSON(t, out.Scope))

	var names []string
	for _, r := range out.Subjects {
		names = append(names, r.Name+"@"+r.Namespace)
	}
	assert.Equal(t, []string{
		"github_org://eng@team-a", "github_org://eng@team-b",
		"system:masters@team-a", "system:masters@team-b",
		"system:serviceaccount:team-a:ci@team-a", "system:serviceaccount:team-a:ci@team-b",
		"oidc-external@corp@team-a", "oidc-external@corp@team-b",
		"u-abc12@team-a", "u-abc12@team-b",
		"user-vwq4j@*",
	}, names)
	assert.Equal(t, DecisionAllowed, out.Subjects[8].Decision)
	assert.Equal(t, DecisionDenied, out.Subjects[9].Decision)
	assert.Contains(t, res.Text, "who can get pods in project p-cvrp4 on c-phmj7")
	assert.Contains(t, res.Text, "NAMESPACE")
	assert.Contains(t, res.Text, "1 subject(s) with unknown groups could not be ruled out: oidc-external@corp", "one name per subject, not per namespace")
	assertDecisionColumn(t, res.Text)
}

// TestWhoCanAllNamespacesAndEmptyProject asserts -A lists the namespaces
// and marks the scope, and that an empty project evaluates nothing: no
// binding is read, no review is posted, exit 0 with resultSource
// scopeDiscovery.
func TestWhoCanAllNamespacesAndEmptyProject(t *testing.T) {
	serialFanOut(t)
	f := whoCanFactory(sarAnswer(false, ""))
	res, err := NewWhoCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(),
		fakeInput{strs: map[string]string{"cluster": "prod"}, bools: map[string]bool{"all-namespaces": true}, args: []string{"get", "pods"}}, nil)
	require.NoError(t, err)
	out := decodeSubjects(t, res)
	assert.Equal(t, map[string]any{"cluster": "c-phmj7", "allNamespaces": true}, scopeJSON(t, out.Scope))
	assert.Equal(t, 12, out.Rollup.Rows)
	assert.Equal(t, "team-a", out.Subjects[0].Namespace)

	f = whoCanFactory()
	res, err = NewWhoCan(f, f.caller).(runtime.ReadCommand).Execute(projectContext(t, f.caller, "p-empty", "Empty"), canInput(map[string]string{"project": "p-empty"}, "get", "pods"), nil)
	require.NoError(t, err)
	out = decodeSubjects(t, res)
	assert.Equal(t, 0, res.ExitStatus)
	assert.Empty(t, out.Subjects)
	assert.Equal(t, SourceScopeDiscovery, out.ResultSource)
	assert.Equal(t, 0, out.Rollup.Candidates)
	assert.Contains(t, out.Note, "has no namespaces")
	require.Len(t, f.caller.Reviews, 1, "the caller whoami only")
}

// TestWhoCanClusterScopeAndNonResource asserts a cluster-scoped resource
// and a non-resource URL ask with no namespace, every RoleBinding subject
// still a candidate, and the non-resource spec shape.
func TestWhoCanClusterScopeAndNonResource(t *testing.T) {
	serialFanOut(t)
	f := whoCanFactory(sarAnswer(false, ""))
	res, err := NewWhoCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(),
		canInput(map[string]string{"non-resource-url": "/healthz"}, "post"), nil)
	require.NoError(t, err)
	out := decodeSubjects(t, res)
	assert.Equal(t, 6, out.Rollup.Candidates, "cluster-wide RoleBindings included")
	assert.Equal(t, "/healthz", out.Question.NonResourceURL)
	assert.True(t, strings.HasPrefix(res.Text, "who can post /healthz on c-phmj7\n"), res.Text)
	spec, _, _ := unstructured.NestedMap(f.caller.Reviews[1].Object.Object, "spec")
	_, hasResource := spec["resourceAttributes"]
	assert.False(t, hasResource)
	nra, _ := spec["nonResourceAttributes"].(map[string]any)
	assert.Equal(t, "post", nra["verb"])
	assert.Equal(t, "/healthz", nra["path"])

	f = whoCanFactory(sarAnswer(false, ""))
	res, err = NewWhoCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(nil, "list", "pods"), nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(res.Text, "who can list pods in all namespaces on c-phmj7\n"), res.Text)
	ra, _, _ := unstructured.NestedMap(f.caller.Reviews[1].Object.Object, "spec", "resourceAttributes")
	_, hasNamespace := ra["namespace"]
	assert.False(t, hasNamespace, "a collection verb without a namespace asks cluster-wide")
}

// TestWhoCanRefusals asserts the refusals: a binding LIST the caller may
// not make, a SubjectAccessReview the authorizer refuses (a Status 403),
// a caller without cluster access, and --as.
func TestWhoCanRefusals(t *testing.T) {
	serialFanOut(t)
	f := whoCanFactory()
	f.caller.ListErr = clierr.New(clierr.CodeRBAC, "clusterrolebindings.rbac.authorization.k8s.io is forbidden")
	_, err := NewWhoCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "get", "pods"), nil)
	require.ErrorContains(t, err, "who-can needs list on rolebindings and clusterrolebindings on cluster c-phmj7")

	f = whoCanFactory(scripted{resp: http403(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"subjectaccessreviews.authorization.k8s.io is forbidden","reason":"Forbidden","code":403}`)})
	_, err = NewWhoCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "get", "pods"), nil)
	require.ErrorContains(t, err, "you lack create subjectaccessreviews")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, "rancher get globalroles", ce.Hint)
	require.Len(t, f.caller.Reviews, 2, "the first refused review ends the fan-out")

	f = whoCanFactory()
	f.caller.script = []scripted{{resp: http403("")}}
	_, err = NewWhoCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "get", "pods"), nil)
	require.ErrorContains(t, err, "you have no access to cluster c-phmj7")

	f = whoCanFactory()
	_, err = NewWhoCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a", "as": "alice"}, "get", "pods"), nil)
	require.ErrorContains(t, err, "who-can takes no --as")
	assert.Empty(t, f.caller.Reviews, "validation fires before any request")
}

// TestWhoCanValidation asserts the shared question rules and the who-can
// specific ones: a namespaced resource with a point verb needs a
// namespace (the message names all three selectors), a cluster-scoped
// resource rejects every selector, and at most one selector is accepted.
func TestWhoCanValidation(t *testing.T) {
	serialFanOut(t)
	cases := []struct {
		name string
		in   fakeInput
		want string
	}{
		{"needs namespace", canInput(nil, "get", "pods"), "add -n, --project, or --all-namespaces"},
		{"cluster scoped with -n", canInput(map[string]string{"namespace": "team-a"}, "get", "nodes"), "cluster-scoped"},
		{"two selectors", fakeInput{strs: map[string]string{"cluster": "prod", "namespace": "team-a"}, bools: map[string]bool{"all-namespaces": true}, args: []string{"get", "pods"}}, "at most one of"},
		{"http verb grammar", canInput(map[string]string{"non-resource-url": "/healthz"}, "create"), "HTTP verb"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := whoCanFactory()
			_, err := NewWhoCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), c.in, nil)
			require.ErrorContains(t, err, c.want)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
		})
	}
}

// TestWhoCanIncompleteDiscoveryWarns asserts the resolver's caveat reaches
// who-can: ahead of the candidate warnings on an evaluated answer, and on
// the verified-empty project answer, which asks no authorizer at all.
func TestWhoCanIncompleteDiscoveryWarns(t *testing.T) {
	serialFanOut(t)
	f := whoCanFactory(sarAnswer(true, ""), mastersAnswer(), sarAnswer(false, ""), sarAnswer(true, aliceReason), sarAnswer(true, adminReason))
	f.caller.ResourcesErr = incompleteFor("dead.example")
	res, err := NewWhoCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(),
		canInput(map[string]string{"namespace": "team-a"}, "get", "deployments"), nil)
	require.NoError(t, err)
	assert.Equal(t, "apps", decodeSubjects(t, res).Question.Group)
	require.NotEmpty(t, res.Warnings)
	assert.Contains(t, res.Warnings[0], "discovery for API group(s) dead.example failed",
		"the caveat about the question itself comes before the candidate warnings")

	f = whoCanFactory()
	f.caller.ResourcesErr = incompleteFor("dead.example")
	res, err = NewWhoCan(f, f.caller).(runtime.ReadCommand).Execute(projectContext(t, f.caller, "p-empty", "Empty"),
		canInput(map[string]string{"project": "p-empty"}, "get", "deployments"), nil)
	require.NoError(t, err)
	assert.Equal(t, SourceScopeDiscovery, decodeSubjects(t, res).ResultSource)
	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "discovery for API group(s) dead.example failed")
}

// TestWhoCanBudgetCoversProjectRead asserts --timeout is already running
// when the project is read: a lookup that waits for its context ends as
// the staged budget error, before any candidate is read or question asked.
func TestWhoCanBudgetCoversProjectRead(t *testing.T) {
	t.Parallel()
	f := whoCanFactory()
	_, err := NewWhoCan(f, f.caller).(runtime.ReadCommand).Execute(blockingProjectContext(t), canInput(map[string]string{"project": "Team", flagTimeout: "20ms"}, "get", "pods"), nil)
	require.ErrorContains(t, err, "fan-out budget of 20ms exhausted during project resolution")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.False(t, ce.Retryable)
	assert.Len(t, f.caller.Reviews, 1, "the preflight ran; no question was asked")
}

// TestWhoCanDescriptor asserts the descriptor contract: read-only,
// network, auth, exit set {0, 13}, text shape, no --as or --type, and a
// boolean --show-denied.
func TestWhoCanDescriptor(t *testing.T) {
	t.Parallel()
	d := NewWhoCan(nil, nil).Describe()
	assert.Equal(t, "access who-can", d.Name)
	assert.Equal(t, runtime.ReadOnly, d.CommandType)
	assert.True(t, d.Network)
	assert.True(t, d.Auth)
	assert.Equal(t, []int{0, 13}, d.SuccessExitCodes)
	assert.Equal(t, runtime.ShapeText, d.Shape)
	assert.Contains(t, d.Desc, "Exit 13 when a row has a request issue or is unknown because the authorizer returned an evaluation error",
		"the published exit rule names exactly the rows that exit 13")
	assert.Contains(t, d.Desc, "an allowed answer with an evaluation error stays allowed and warns")
	names := map[string]string{}
	for _, f := range d.Flags {
		names[f.Name] = f.Type
	}
	assert.Equal(t, "bool", names["show-denied"])
	assert.Equal(t, "bool", names["all-namespaces"])
	_, hasAs := names["as"]
	assert.False(t, hasAs)
	_, hasType := names["type"]
	assert.False(t, hasType)
}
