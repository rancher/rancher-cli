package access

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// decodeCan round-trips Result.Data through JSON into canResult, the only
// way tests read output.
func decodeCan(t *testing.T, res runtime.Result) canResult {
	t.Helper()
	b, err := json.Marshal(res.Data)
	require.NoError(t, err)
	var out canResult
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

func canInput(strs map[string]string, args ...string) fakeInput {
	if strs == nil {
		strs = map[string]string{}
	}
	if _, ok := strs["cluster"]; !ok {
		strs["cluster"] = "prod"
	}
	return fakeInput{strs: strs, args: args}
}

func projectObj(cluster, id, display string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{"apiVersion": "management.cattle.io/v3", "kind": "Project",
		"metadata": map[string]any{"name": id, "namespace": cluster}, "spec": map[string]any{"displayName": display}}}
}

// canFactory: the caller client doubles as the management cluster client
// in these tests, so it also serves the Project objects ResolveProject
// lists in the cluster's namespace.
func canFactory(callerUser string, script ...scripted) *fakeFactory {
	full := append([]scripted{{resp: whoamiResp(callerUser, "system:authenticated")}}, script...)
	c := &fakeClient{script: full, Resources: discovery(), Objects: map[string]*unstructured.Unstructured{
		"team-a": new(nsObj("team-a", nil)), "team-b": new(nsObj("team-b", nil)),
	}, Lists: map[string]*unstructured.UnstructuredList{
		"namespaces||field.cattle.io/projectId=p-cvrp4": {Items: []unstructured.Unstructured{nsObj("team-a", nil), nsObj("team-b", nil)}},
		"projects|c-phmj7|":                             {Items: []unstructured.Unstructured{projectObj("c-phmj7", "p-cvrp4", "Team"), projectObj("c-phmj7", "p-empty", "Empty")}},
	}}
	return &fakeFactory{caller: c, resolve: map[string]string{"prod": "c-phmj7"}}
}

// TestCanCallerAllowed asserts the caller path: whoami then one
// SelfSubjectAccessReview, decision allowed, exit 0, version carried.
func TestCanCallerAllowed(t *testing.T) {
	t.Parallel()
	f := canFactory("user-vwq4j", scripted{resp: sar(true, `RBAC: allowed by ClusterRoleBinding "crb-1" of ClusterRole "cluster-owner-aggregator" to User "user-vwq4j"`)})
	cmd := NewCan(f, f.caller).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "delete", "pods"), nil)
	require.NoError(t, err)
	out := decodeCan(t, res)
	assert.Equal(t, DecisionAllowed, out.Decision)
	assert.Equal(t, 0, res.ExitStatus)
	assert.Equal(t, IdentityCaller, out.Identity)
	assert.Equal(t, SourceAuthorizer, out.ResultSource)
	assert.Equal(t, "user-vwq4j", out.Subject.Name)
	assert.Equal(t, "v1", out.Question.Version)
	assert.Contains(t, out.Reason, "cluster-owner-aggregator")
	require.Len(t, f.caller.Reviews, 2)
	spec, _, _ := unstructured.NestedMap(f.caller.Reviews[1].Object.Object, "spec", "resourceAttributes")
	assert.Equal(t, "team-a", spec["namespace"])
	assert.Equal(t, "v1", spec["version"])
	assert.True(t, strings.HasPrefix(res.Text, "allowed  you can delete pods in team-a on c-phmj7"), res.Text)
	assert.True(t, strings.HasSuffix(res.Text, "source: authorizer | identity: caller\n"), "text ends with the source line and a newline: %q", res.Text)
	assert.Equal(t, 1, f.ResolveCalls, "the cluster is resolved once per command")
	m := scopeJSON(t, out.Scope)
	assert.Equal(t, map[string]any{"cluster": "c-phmj7", "namespace": "team-a"}, m)
}

// TestCanDeniedAndUnknownExitCodes asserts 12 for denied and 13 for a
// negative answer with an evaluation error.
func TestCanDeniedAndUnknownExitCodes(t *testing.T) {
	t.Parallel()
	f := canFactory("m-54pdw", scripted{resp: sar(false, "")})
	res, err := NewCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "get", "pods"), nil)
	require.NoError(t, err)
	assert.Equal(t, DecisionDenied, decodeCan(t, res).Decision)
	assert.Equal(t, 12, res.ExitStatus)

	f = canFactory("m-54pdw", scripted{resp: review(engine.ReviewSelfSubjectAccess, map[string]any{"allowed": false, "evaluationError": "webhook unreachable"})})
	res, err = NewCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "get", "pods"), nil)
	require.NoError(t, err)
	assert.Equal(t, DecisionUnknown, decodeCan(t, res).Decision)
	assert.Equal(t, 13, res.ExitStatus)
}

// TestCanValidationBeforeAnyRequest asserts every validation rule fires
// with zero client calls.
func TestCanValidationBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   fakeInput
		want string
	}{
		{"verb missing", canInput(nil), "a verb is required"},
		{"resource missing", canInput(nil, "get"), "a resource is required"},
		{"verb shape", canInput(map[string]string{"namespace": "team-a"}, "get pods", "pods"), "is not a verb"},
		{"resource with non-resource-url", canInput(map[string]string{"non-resource-url": "/healthz"}, "get", "pods"), "--non-resource-url replaces the resource"},
		{"non-resource with namespace", canInput(map[string]string{"non-resource-url": "/healthz", "namespace": "x"}, "get"), "--non-resource-url cannot be combined"},
		{"non-resource verb grammar", canInput(map[string]string{"non-resource-url": "/healthz"}, "create"), "HTTP verb"},
		{"non-resource path shape", canInput(map[string]string{"non-resource-url": "healthz"}, "get"), "must begin with /"},
		{"non-resource with all-namespaces", fakeInput{strs: map[string]string{"cluster": "prod", "non-resource-url": "/healthz"},
			bools: map[string]bool{"all-namespaces": true}, args: []string{"get"}}, "cannot be combined with --all-namespaces"},
		{"type without as", canInput(map[string]string{"type": "group"}, "get", "pods"), "--type qualifies a named subject"},
		{"named resource without namespace", canInput(map[string]string{"namespace": "team-a"}, "get", "pods/web"), ""},
		{"get pods cluster scope", canInput(nil, "get", "pods"), "needs a namespace"},
		{"named resource cluster scope", canInput(nil, "get", "pods/web"), "needs a namespace"},
	}
	for _, c := range cases {
		if c.want == "" {
			continue // a valid line, kept in the table as the counter-example
		}
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := canFactory("user-vwq4j")
			f.caller.Resources = discovery()
			_, err := NewCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), c.in, nil)
			require.ErrorContains(t, err, c.want)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.LessOrEqual(t, len(f.caller.Reviews), 1, "at most the caller preflight ran; no question was asked")
		})
	}
}

// TestCanLocalConflictsMakeNoRequest asserts every conflict decidable
// from the command line alone is refused before cluster resolution, the
// preflight, or discovery.
func TestCanLocalConflictsMakeNoRequest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   fakeInput
		want string
	}{
		{"two selectors", canInput(map[string]string{"namespace": "team-a", "project": "p-cvrp4"}, "get", "pods"), "at most one of"},
		{"all-namespaces on can", fakeInput{strs: map[string]string{"cluster": "prod"}, bools: map[string]bool{"all-namespaces": true}, args: []string{"get", "pods"}}, "--all-namespaces is not accepted here"},
		{"empty name after slash", canInput(map[string]string{"namespace": "team-a"}, "get", "pods/"), "the name is one segment"},
		{"second slash", canInput(map[string]string{"namespace": "team-a"}, "get", "pods/web/extra"), "the name is one segment"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := canFactory("user-vwq4j")
			_, err := NewCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), c.in, nil)
			require.ErrorContains(t, err, c.want)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, 0, f.ResolveCalls, "no cluster resolution")
			assert.Empty(t, f.caller.Reviews, "no request of any kind")
		})
	}
}

// TestCanClusterScopedResourceRejectsProjectBeforeLookup asserts the
// discovery-decidable rule fires before the project is looked up: the
// project list is absent from the fake, so a lookup would have failed
// with a different error.
func TestCanClusterScopedResourceRejectsProjectBeforeLookup(t *testing.T) {
	t.Parallel()
	f := canFactory("user-vwq4j")
	delete(f.caller.Lists, "projects|c-phmj7|")
	_, err := NewCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"project": "p-cvrp4"}, "get", "nodes"), nil)
	require.ErrorContains(t, err, "resource nodes is cluster-scoped")
	assert.Len(t, f.caller.Reviews, 1, "preflight only")
}

// TestCanDescriptorClusterFlagCarriesTheStandardMessage asserts the
// required --cluster flag uses the repository's message and hint, as get
// does.
func TestCanDescriptorClusterFlagCarriesTheStandardMessage(t *testing.T) {
	t.Parallel()
	for _, cmd := range []runtime.Command{NewCan(nil, nil), NewList(nil, nil)} {
		d := cmd.Describe()
		i := slices.IndexFunc(d.Flags, func(f runtime.ArgSpec) bool { return f.Name == "cluster" })
		require.GreaterOrEqual(t, i, 0, d.Name)
		assert.True(t, d.Flags[i].Required)
		assert.Equal(t, engine.ClusterRequiredMessage, d.Flags[i].MissingMessage, d.Name)
		assert.Equal(t, engine.ClusterRequiredHint, d.Flags[i].MissingHint, d.Name)
	}
}

// TestCanListVerbWithoutNamespaceIsAllNamespaces asserts a collection verb
// on a namespaced resource with no selector asks with an empty namespace.
func TestCanListVerbWithoutNamespaceIsAllNamespaces(t *testing.T) {
	t.Parallel()
	f := canFactory("user-vwq4j", scripted{resp: sar(true, "RBAC: allowed by ClusterRoleBinding \"a\" of ClusterRole \"b\" to User \"c\"")})
	res, err := NewCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(nil, "list", "pods"), nil)
	require.NoError(t, err)
	spec, _, _ := unstructured.NestedMap(f.caller.Reviews[1].Object.Object, "spec", "resourceAttributes")
	_, hasNS := spec["namespace"]
	assert.False(t, hasNS)
	assert.Contains(t, res.Text, "all namespaces")
}

// TestCanNamedSubjectViaSAR asserts a group subject posts one
// SubjectAccessReview with spec.groups and reports groupsFrom.
func TestCanNamedSubjectViaSAR(t *testing.T) {
	t.Parallel()
	f := canFactory("user-vwq4j", scripted{resp: review(engine.ReviewSubjectAccess, map[string]any{"allowed": true, "reason": "RBAC: allowed by RoleBinding \"r\" of ClusterRole \"c\" to Group \"github_org://eng\""})})
	res, err := NewCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a", "as": "github_org://eng"}, "get", "pods"), nil)
	require.NoError(t, err)
	out := decodeCan(t, res)
	assert.Equal(t, IdentitySAR, out.Identity)
	assert.Equal(t, GroupsGroupOnly, out.GroupsFrom)
	assert.Equal(t, KindGroup, out.Subject.Kind)
	req := f.caller.Reviews[1]
	assert.Equal(t, engine.ReviewSubjectAccess, req.Kind)
	groups, _, _ := unstructured.NestedStringSlice(req.Object.Object, "spec", "groups")
	assert.Equal(t, []string{"github_org://eng"}, groups)
	_, hasUser := req.Object.Object["spec"].(map[string]any)["user"]
	assert.False(t, hasUser)
}

// TestCanSARRefusedIsRBACRefusal asserts the review endpoint's own 403 on
// the SAR path is a refusal with the globalroles hint.
func TestCanSARRefusedIsRBACRefusal(t *testing.T) {
	t.Parallel()
	f := canFactory("m-54pdw", scripted{resp: http403(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"subjectaccessreviews.authorization.k8s.io is forbidden"}`)})
	_, err := NewCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a", "as": "github_org://eng"}, "get", "pods"), nil)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, "rancher get globalroles", ce.Hint)
}

// TestCanSingleScopeTerminalPassesThroughUnchanged asserts the refusal
// remap belongs to the SubjectAccessReview path alone: a Status-shaped
// 403 the caller's own review earned keeps the server's mapped error and
// gains no hint.
func TestCanSingleScopeTerminalPassesThroughUnchanged(t *testing.T) {
	t.Parallel()
	body := `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"selfsubjectaccessreviews.authorization.k8s.io is forbidden"}`
	f := canFactory("user-vwq4j", scripted{resp: http403(body)})
	_, err := NewCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "get", "pods"), nil)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, body, ce.Message)
	assert.Empty(t, ce.Hint)
	assert.NotContains(t, ce.Error(), "you lack")
}

// TestCanAllowedWithEvaluationErrorWarns asserts an allowed answer that
// also carries an evaluation error stays allowed and reports the error as
// a warning, so a partial authorization chain is never silent.
func TestCanAllowedWithEvaluationErrorWarns(t *testing.T) {
	t.Parallel()
	f := canFactory("user-vwq4j", scripted{resp: review(engine.ReviewSelfSubjectAccess, map[string]any{"allowed": true, "evaluationError": "webhook unreachable"})})
	res, err := NewCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "get", "pods"), nil)
	require.NoError(t, err)
	out := decodeCan(t, res)
	assert.Equal(t, DecisionAllowed, out.Decision)
	assert.Equal(t, "webhook unreachable", out.EvaluationError)
	assert.Equal(t, []string{"the authorizer reported an evaluation error: webhook unreachable"}, res.Warnings)
	assert.Equal(t, 0, res.ExitStatus)
	assert.Contains(t, res.Text, "evaluation error: webhook unreachable")
}

// TestCanSingleReviewRequestFailureIsTerminal asserts a 503 on a single
// review returns the mapped error (degradation is fan-out only).
func TestCanSingleReviewRequestFailureIsTerminal(t *testing.T) {
	t.Parallel()
	f := canFactory("user-vwq4j", scripted{resp: engine.ReviewResponse{Status: 503, Mapped: clierr.Retryable(clierr.CodeGeneric, "unavailable")}})
	_, err := NewCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "get", "pods"), nil)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.True(t, ce.Retryable)
}

// TestCanProjectRollup asserts per-namespace rows, the mixed source, the
// issue on a failed member, and exit 13. fanOutLimit is set to 1 so the
// scripted answers land on the sorted namespaces in order (a seam
// mutation, hence no t.Parallel).
func TestCanProjectRollup(t *testing.T) {
	old := fanOutLimit
	fanOutLimit = 1
	t.Cleanup(func() { fanOutLimit = old })
	f := canFactory("user-vwq4j",
		scripted{resp: sar(true, "RBAC: allowed by RoleBinding \"a\" of ClusterRole \"b\" to User \"c\"")},
		scripted{resp: engine.ReviewResponse{Status: 503, Mapped: clierr.Retryable(clierr.CodeGeneric, "POST selfsubjectaccessreviews: 503 Service Unavailable")}},
	)
	res, err := NewCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"project": "p-cvrp4"}, "get", "pods"), nil)
	require.NoError(t, err)
	out := decodeCan(t, res)
	require.Len(t, out.Namespaces, 2)
	assert.Equal(t, "team-a", out.Namespaces[0].Namespace)
	assert.Equal(t, MemberAuthorizer, out.Namespaces[0].Source)
	assert.Equal(t, DecisionAllowed, out.Namespaces[0].Decision)
	assert.Equal(t, MemberRequest, out.Namespaces[1].Source)
	assert.Equal(t, DecisionUnknown, out.Namespaces[1].Decision)
	require.NotNil(t, out.Namespaces[1].Issue)
	assert.Equal(t, "GENERIC", out.Namespaces[1].Issue.Code)
	assert.Equal(t, canRollup{Namespaces: 2, Evaluated: 1, Allowed: 1, Unknown: 1, Issues: 1}, *out.Rollup)
	assert.Equal(t, SourceMixed, out.ResultSource)
	assert.Equal(t, DecisionUnknown, out.Decision)
	assert.Equal(t, 13, res.ExitStatus)
}

// TestCanEmptyProjectIsUnknown asserts zero namespaces never answers
// allowed.
func TestCanEmptyProjectIsUnknown(t *testing.T) {
	t.Parallel()
	f := canFactory("user-vwq4j")
	f.caller.Lists["namespaces||field.cattle.io/projectId=p-empty"] = &unstructured.UnstructuredList{}
	res, err := NewCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"project": "p-empty"}, "get", "pods"), nil)
	require.NoError(t, err)
	out := decodeCan(t, res)
	assert.Equal(t, DecisionUnknown, out.Decision)
	assert.Equal(t, SourceScopeDiscovery, out.ResultSource)
	assert.Equal(t, 13, res.ExitStatus)
	assert.Contains(t, out.Note, "project p-empty has no namespaces on cluster c-phmj7")
	assert.Len(t, f.caller.Reviews, 1, "only the whoami ran")
}

// TestCanEmptyProjectWithProxyDeniedSubjectIsUnknown asserts the
// verified-empty scope wins over the proxy's cluster-wide refusal: no
// question was evaluated, so the answer is unknown, exit 13,
// scopeDiscovery, with the resolved subject kept.
func TestCanEmptyProjectWithProxyDeniedSubjectIsUnknown(t *testing.T) {
	t.Parallel()
	f := canFactory("user-vwq4j")
	f.caller.Lists["namespaces||field.cattle.io/projectId=p-empty"] = &unstructured.UnstructuredList{}
	f.impersonated = map[string]*fakeClient{"u-abc12": {script: []scripted{{resp: http403("")}}}}
	local := localWith("u-abc12")
	local.Lists["clusterroletemplatebindings|c-phmj7|"] = &unstructured.UnstructuredList{}
	local.Lists["projectroletemplatebindings||"] = &unstructured.UnstructuredList{}
	res, err := NewCan(f, local).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"project": "p-empty", "as": "u-abc12"}, "get", "pods"), nil)
	require.NoError(t, err)
	out := decodeCan(t, res)
	assert.Equal(t, DecisionUnknown, out.Decision)
	assert.Equal(t, SourceScopeDiscovery, out.ResultSource)
	assert.Equal(t, "u-abc12", out.Subject.Name)
	assert.Contains(t, out.Note, "has no namespaces")
	assert.Equal(t, 13, res.ExitStatus)
	assert.Empty(t, out.Namespaces)
}

// blockUntilDone is a fakeClient.Before hook that waits for the context,
// standing in for a slow server so a test can spend the fan-out budget
// in one stage.
func blockUntilDone(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestCanBudgetCoversTargetDiscovery asserts the fan-out budget is
// already running while the project's namespaces are listed: a slow LIST
// spends it and no review is posted.
func TestCanBudgetCoversTargetDiscovery(t *testing.T) {
	old := fanOutBudget
	fanOutBudget = 20 * time.Millisecond
	t.Cleanup(func() { fanOutBudget = old })
	f := canFactory("user-vwq4j")
	preflightDone := false
	f.caller.Before = func(ctx context.Context) error {
		if !preflightDone { // let the whoami through; block the namespace LIST
			preflightDone = true
			return nil
		}
		return blockUntilDone(ctx)
	}
	_, err := NewCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"project": "p-cvrp4"}, "get", "pods"), nil)
	require.ErrorContains(t, err, "fan-out budget of 20ms exhausted during")
	assert.Len(t, f.caller.Reviews, 1, "the preflight ran; no question was asked")
}

// TestCanBudgetCoversSubjectResolution asserts a slow management-cluster
// read (the subject's User or UserAttribute) spends the same budget.
func TestCanBudgetCoversSubjectResolution(t *testing.T) {
	old := fanOutBudget
	fanOutBudget = 20 * time.Millisecond
	t.Cleanup(func() { fanOutBudget = old })
	f := canFactory("user-vwq4j")
	local := localWith("u-abc12")
	local.Before = blockUntilDone
	_, err := NewCan(f, local).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"project": "p-cvrp4", "as": "u-abc12"}, "get", "pods"), nil)
	require.ErrorContains(t, err, "fan-out budget of 20ms exhausted during subject resolution")
	assert.Empty(t, f.Impersonated, "nothing was impersonated")
}

// TestCanProjectSARClusterAccessLostMidRun asserts a mid-run
// 403 on the SubjectAccessReview path keeps its own error and is never
// remapped to the missing-right refusal.
func TestCanProjectSARClusterAccessLostMidRun(t *testing.T) {
	old := fanOutLimit
	fanOutLimit = 1
	t.Cleanup(func() { fanOutLimit = old })
	f := canFactory("user-vwq4j",
		scripted{resp: sar(false, "")},
		scripted{resp: http403("")},
	)
	f.impersonated = map[string]*fakeClient{"u-abc12": {script: []scripted{{resp: http403(`{"message":"not allowed to impersonate user"}`)}}}}
	res, err := NewCan(f, localWith("u-abc12")).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"project": "p-cvrp4", "as": "u-abc12"}, "get", "pods"), nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "cluster access changed during the run; evaluated 1 of 2")
	assert.NotContains(t, err.Error(), "impersonate")
	assert.Zero(t, res)
}

// TestCanProjectSARRefusedIsRBACRefusal asserts the positive half of the
// fan-out remap: a Status-shaped 403 from a member on the
// SubjectAccessReview path is the caller's missing right, named with the
// globalroles hint.
func TestCanProjectSARRefusedIsRBACRefusal(t *testing.T) {
	old := fanOutLimit
	fanOutLimit = 1
	t.Cleanup(func() { fanOutLimit = old })
	f := canFactory("user-vwq4j",
		scripted{resp: sar(true, "RBAC: allowed by RoleBinding \"a\" of ClusterRole \"b\" to User \"c\"")},
		scripted{resp: http403(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"subjectaccessreviews.authorization.k8s.io is forbidden"}`)},
	)
	f.impersonated = map[string]*fakeClient{"u-abc12": {script: []scripted{{resp: http403(`{"message":"not allowed to impersonate user"}`)}}}}
	_, err := NewCan(f, localWith("u-abc12")).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"project": "p-cvrp4", "as": "u-abc12"}, "get", "pods"), nil)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, "cannot ask about u-abc12 on cluster c-phmj7: you lack create subjectaccessreviews on cluster c-phmj7, or impersonate on users on the management cluster", ce.Message)
	assert.Equal(t, "rancher get globalroles", ce.Hint)
}

// TestCanUnknownGroupsNegativeIsUnknown asserts a system: user without a
// Rancher User is never reported denied: its groups are unknown. The
// impersonated whoami answers as the caller (the header was dropped), so
// the SubjectAccessReview path is taken explicitly.
func TestCanUnknownGroupsNegativeIsUnknown(t *testing.T) {
	t.Parallel()
	f := canFactory("user-vwq4j", scripted{resp: review(engine.ReviewSubjectAccess, map[string]any{"allowed": false})})
	f.impersonated = map[string]*fakeClient{"system:kube-controller-manager": {script: []scripted{{resp: whoamiResp("user-vwq4j")}}}}
	res, err := NewCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a", "as": "system:kube-controller-manager"}, "get", "pods"), nil)
	require.NoError(t, err)
	out := decodeCan(t, res)
	assert.Equal(t, DecisionUnknown, out.Decision)
	assert.Equal(t, "groups unknown", out.Note)
	assert.Equal(t, GroupsAuthenticatedOnly, out.GroupsFrom)
	assert.Equal(t, 13, res.ExitStatus)
}

// TestCanAnonymousViaSAR asserts system:anonymous is never impersonated:
// one SubjectAccessReview with user system:anonymous and groups exactly
// [system:unauthenticated], and a negative answer is a definite denial.
func TestCanAnonymousViaSAR(t *testing.T) {
	t.Parallel()
	f := canFactory("user-vwq4j", scripted{resp: review(engine.ReviewSubjectAccess, map[string]any{"allowed": false})})
	res, err := NewCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a", "as": "system:anonymous"}, "get", "pods"), nil)
	require.NoError(t, err)
	out := decodeCan(t, res)
	assert.Equal(t, DecisionDenied, out.Decision)
	assert.Equal(t, GroupsAnonymousIdentity, out.GroupsFrom)
	assert.Empty(t, f.Impersonated)
	spec := f.caller.Reviews[1].Object.Object["spec"].(map[string]any)
	assert.Equal(t, "system:anonymous", spec["user"])
	assert.Equal(t, []any{"system:unauthenticated"}, spec["groups"])
	assert.Equal(t, 12, res.ExitStatus)
}

// TestCanNonResourceURLBody asserts a non-resource question sends
// nonResourceAttributes only, with the HTTP verb folded to lowercase.
func TestCanNonResourceURLBody(t *testing.T) {
	t.Parallel()
	f := canFactory("user-vwq4j", scripted{resp: sar(true, "RBAC: allowed by ClusterRoleBinding \"a\" of ClusterRole \"b\" to User \"c\"")})
	res, err := NewCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"non-resource-url": "/healthz"}, "POST"), nil)
	require.NoError(t, err)
	spec := f.caller.Reviews[1].Object.Object["spec"].(map[string]any)
	nra, ok := spec["nonResourceAttributes"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "post", nra["verb"])
	assert.Equal(t, "/healthz", nra["path"])
	_, hasRA := spec["resourceAttributes"]
	assert.False(t, hasRA)
	assert.Equal(t, "post", decodeCan(t, res).Question.Verb)
}

// TestCanCarriesResolvedVersion asserts the review body carries the API
// version discovery resolved (an authorizer may depend on it).
func TestCanCarriesResolvedVersion(t *testing.T) {
	t.Parallel()
	f := canFactory("user-vwq4j", scripted{resp: sar(true, "RBAC: allowed by ClusterRoleBinding \"a\" of ClusterRole \"b\" to User \"c\"")})
	f.caller.Resources = append(f.caller.Resources, engine.APIResource{Group: "apps", Version: "v1", Name: "statefulsets", Kind: "StatefulSet", Namespaced: true})
	_, err := NewCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "get", "statefulsets"), nil)
	require.NoError(t, err)
	spec, _, _ := unstructured.NestedMap(f.caller.Reviews[1].Object.Object, "spec", "resourceAttributes")
	assert.Equal(t, "apps", spec["group"])
	assert.Equal(t, "v1", spec["version"])
}

// TestCanSubjectWithoutClusterAccessIsOneDenial asserts the proxy's
// empty 403 for the subject yields rows synthesized from one fact.
func TestCanSubjectWithoutClusterAccessIsOneDenial(t *testing.T) {
	t.Parallel()
	noAccess := &fakeClient{script: []scripted{{resp: http403("")}}}
	f := canFactory("user-vwq4j")
	f.impersonated = map[string]*fakeClient{"u-abc12": noAccess}
	local := localWith("u-abc12")
	local.Lists["clusterroletemplatebindings|c-phmj7|"] = &unstructured.UnstructuredList{}
	local.Lists["projectroletemplatebindings||"] = &unstructured.UnstructuredList{}
	res, err := NewCan(f, local).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"project": "p-cvrp4", "as": "u-abc12"}, "get", "pods"), nil)
	require.NoError(t, err)
	out := decodeCan(t, res)
	assert.Equal(t, DecisionDenied, out.Decision)
	assert.Equal(t, SourceRancherProxy, out.ResultSource)
	assert.Equal(t, IdentityImpersonated, out.Identity)
	assert.Empty(t, out.GroupsFrom)
	assert.Contains(t, out.Note, "one cluster-access denial applies to all listed namespaces")
	require.Len(t, out.Namespaces, 2)
	assert.Equal(t, MemberRancherProxy, out.Namespaces[1].Source)
	assert.Equal(t, 12, res.ExitStatus)
	assert.Len(t, noAccess.Reviews, 1, "exactly one impersonated SelfSubjectReview")
	assert.Len(t, f.caller.Reviews, 1, "no authorization review as the caller")
}

// TestCanIncompleteDiscoveryWarns asserts the resolver's caveat reaches the
// result: under incomplete discovery a bare non-core name still answers,
// and the warning names the groups that were not consulted. It rides on
// the verified-empty project answer too, which asks no authorizer.
func TestCanIncompleteDiscoveryWarns(t *testing.T) {
	t.Parallel()
	f := canFactory("user-vwq4j", scripted{resp: sar(true, "RBAC: allowed by ClusterRoleBinding \"crb-1\" of ClusterRole \"admin\" to User \"user-vwq4j\"")})
	f.caller.ResourcesErr = incompleteFor("dead.example")
	cmd := NewCan(f, f.caller).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "get", "deployments"), nil)
	require.NoError(t, err)
	out := decodeCan(t, res)
	assert.Equal(t, DecisionAllowed, out.Decision)
	assert.Equal(t, "apps", out.Question.Group)
	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "discovery for API group(s) dead.example failed")

	f = canFactory("user-vwq4j")
	f.caller.ResourcesErr = incompleteFor("dead.example")
	f.caller.Lists["namespaces||field.cattle.io/projectId=p-empty"] = &unstructured.UnstructuredList{}
	res, err = NewCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"project": "p-empty"}, "get", "deployments"), nil)
	require.NoError(t, err)
	assert.Equal(t, DecisionUnknown, decodeCan(t, res).Decision)
	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "discovery for API group(s) dead.example failed")
}

// blockAfterFirstCall is a fakeClient.Before hook that lets the first call
// (the caller's whoami) through and holds every later one until the
// context ends; a context that never ends is reported as an error, so a
// call outside any budget fails the test instead of hanging it.
func blockAfterFirstCall() func(ctx context.Context) error {
	var calls atomic.Int32
	return func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
			return errors.New("no budget bounded the request")
		}
	}
}

// TestCanTimeoutBoundsTheSingleReview asserts --timeout is the budget of a
// plain can as well (no --project, no fan-out): the one review runs under
// it and its exhaustion is the staged, non-retryable budget error.
func TestCanTimeoutBoundsTheSingleReview(t *testing.T) {
	t.Parallel()
	f := canFactory("user-vwq4j")
	f.caller.Before = blockAfterFirstCall()
	_, err := NewCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"non-resource-url": "/healthz", flagTimeout: "20ms"}, "get"), nil)
	require.ErrorContains(t, err, "fan-out budget of 20ms exhausted during the review")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.False(t, ce.Retryable, "an exhausted budget is not retried with the same budget")
	assert.Len(t, f.caller.Reviews, 1, "the whoami ran; the review never reached the server")
}
