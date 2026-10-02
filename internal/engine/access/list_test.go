package access

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
)

func rules(incomplete bool, res ...map[string]any) engine.ReviewResponse {
	rr := make([]any, 0, len(res))
	for _, r := range res {
		rr = append(rr, r)
	}
	return review(engine.ReviewSelfSubjectRules, map[string]any{"incomplete": incomplete, "resourceRules": rr,
		"nonResourceRules": []any{map[string]any{"verbs": []any{"get"}, "nonResourceURLs": []any{"/healthz", "/healthz"}}}})
}

func rule(verbs []any, groups []any, resources []any) map[string]any {
	return map[string]any{"verbs": verbs, "apiGroups": groups, "resources": resources}
}

func decodeList(t *testing.T, res runtime.Result) listResult {
	t.Helper()
	b, err := json.Marshal(res.Data)
	require.NoError(t, err)
	var out listResult
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

func listFactory(callerUser string, script ...scripted) *fakeFactory {
	full := append([]scripted{{resp: whoamiResp(callerUser, "system:authenticated")}}, script...)
	c := &fakeClient{script: full, Objects: map[string]*unstructured.Unstructured{
		"default": new(nsObj("default", nil)), "team-a": new(nsObj("team-a", nil)), "team-b": new(nsObj("team-b", nil)),
	}, Lists: map[string]*unstructured.UnstructuredList{
		"namespaces||": {Items: []unstructured.Unstructured{nsObj("team-b", nil), nsObj("team-a", nil)}},
	}}
	return &fakeFactory{caller: c, resolve: map[string]string{"prod": "c-phmj7"}}
}

// TestListDefaultNamespaceCanonicalRules asserts the implicit default
// namespace, canonical sorting and deduplication, and exit 0.
func TestListDefaultNamespaceCanonicalRules(t *testing.T) {
	t.Parallel()
	f := listFactory("user-vwq4j", scripted{resp: rules(false,
		rule([]any{"list", "get", "get"}, []any{""}, []any{"pods", "configmaps"}),
		rule([]any{"get", "list"}, []any{""}, []any{"configmaps", "pods"}),
	)})
	res, err := NewList(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(nil), nil)
	require.NoError(t, err)
	out := decodeList(t, res)
	require.Len(t, out.Namespaces, 1)
	ns := out.Namespaces[0]
	assert.Equal(t, "default", ns.Namespace)
	assert.True(t, ns.Evaluated)
	assert.Equal(t, MemberAuthorizer, ns.Source)
	require.Len(t, ns.ResourceRules, 1, "duplicates collapse")
	assert.Equal(t, Rule{Verbs: []string{"get", "list"}, APIGroups: []string{""}, Resources: []string{"configmaps", "pods"}}, ns.ResourceRules[0])
	assert.Equal(t, []string{"/healthz"}, ns.NonResourceRules[0].NonResourceURLs)
	assert.True(t, out.Rollup.Complete)
	assert.Equal(t, 0, res.ExitStatus)
	assert.Equal(t, IdentityCaller, out.Identity)
	assert.Contains(t, res.Text, "including every cluster-wide rule")
	spec, _, _ := unstructured.NestedString(f.caller.Reviews[1].Object.Object, "spec", "namespace")
	assert.Equal(t, "default", spec)
	assert.Equal(t, map[string]any{"cluster": "c-phmj7"}, scopeJSON(t, out.Scope), "the implicit default is a row, not wire scope")
	assert.Equal(t, 1, f.ResolveCalls)
}

// TestListIncompleteIsExit13 asserts a server-marked incomplete set keeps
// its rules but fails completeness.
func TestListIncompleteIsExit13(t *testing.T) {
	t.Parallel()
	f := listFactory("user-vwq4j", scripted{resp: rules(true, rule([]any{"get"}, []any{""}, []any{"pods"}))})
	res, err := NewList(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}), nil)
	require.NoError(t, err)
	out := decodeList(t, res)
	assert.True(t, out.Namespaces[0].Incomplete)
	assert.Len(t, out.Namespaces[0].ResourceRules, 1)
	assert.False(t, out.Rollup.Complete)
	assert.Equal(t, 13, res.ExitStatus)
}

// TestListWideOutputHasNamespaceColumn asserts the default text groups
// identical complete namespaces while the wide projection lists every
// namespace on its own rows under a NAMESPACE column.
func TestListWideOutputHasNamespaceColumn(t *testing.T) {
	t.Parallel()
	same := rules(false, rule([]any{"get"}, []any{""}, []any{"pods"}))
	f := listFactory("user-vwq4j", scripted{resp: same}, scripted{resp: same})
	res, err := NewList(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), fakeInput{strs: map[string]string{"cluster": "prod"}, bools: map[string]bool{"all-namespaces": true}}, nil)
	require.NoError(t, err)
	assert.Contains(t, res.Text, "also in: team-b")
	assert.NotContains(t, res.Text, "NAMESPACE")
	require.NotEmpty(t, res.WideText)
	assert.Contains(t, res.WideText, "NAMESPACE")
	assert.NotContains(t, res.WideText, "also in")
	assert.Regexp(t, `(?m)^NAMESPACE\s+API GROUP\s+RESOURCE\s+VERBS$`, res.WideText)
	assert.Regexp(t, `(?m)^team-a\s+core\s+pods\s+get$`, res.WideText)
	assert.True(t, strings.HasSuffix(res.WideText, "\n"))
	resourceBlock, nonResourceBlock, ok := strings.Cut(res.WideText, "\nNAMESPACE ")
	require.True(t, ok, "the wide view carries a non-resource block of its own")
	assert.Equal(t, 1, strings.Count(resourceBlock, "\nteam-a "), "one resource row per namespace")
	assert.Equal(t, 1, strings.Count(resourceBlock, "\nteam-b "))
	assert.Equal(t, 1, strings.Count(nonResourceBlock, "\nteam-a "), "one non-resource row per namespace")
	assert.Equal(t, 1, strings.Count(nonResourceBlock, "\nteam-b "))
}

// TestListTextDoesNotGroupDifferentCompletenessStates asserts two
// namespaces with identical rules but different completeness never fold
// into one block, in either order, and the evaluation error stays with
// its own namespace.
func TestListTextDoesNotGroupDifferentCompletenessStates(t *testing.T) {
	t.Parallel()
	complete := rules(false, rule([]any{"get"}, []any{""}, []any{"pods"}))
	incomplete := review(engine.ReviewSelfSubjectRules, map[string]any{"incomplete": true, "evaluationError": "webhook timed out",
		"resourceRules": []any{rule([]any{"get"}, []any{""}, []any{"pods"})}})
	for name, script := range map[string][]scripted{"complete first": {{resp: complete}, {resp: incomplete}}, "incomplete first": {{resp: incomplete}, {resp: complete}}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := listFactory("user-vwq4j", script...)
			res, err := NewList(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), fakeInput{strs: map[string]string{"cluster": "prod"}, bools: map[string]bool{"all-namespaces": true}}, nil)
			require.NoError(t, err)
			assert.Contains(t, res.Text, "namespace team-a on")
			assert.Contains(t, res.Text, "namespace team-b on")
			assert.NotContains(t, res.Text, "also in")
			assert.Equal(t, 1, strings.Count(res.Text, "webhook timed out"))
		})
	}
}

// TestListAllNamespacesMemberIssueAndAlsoIn asserts listed names are
// targets without a GET, a failed member carries an issue, identical sets
// print once, and the source is mixed. fanOutLimit is set to 1 so the
// scripted answers land on the sorted namespaces in order (no t.Parallel).
func TestListAllNamespacesMemberIssueAndAlsoIn(t *testing.T) {
	old := fanOutLimit
	fanOutLimit = 1
	t.Cleanup(func() { fanOutLimit = old })
	f := listFactory("user-vwq4j",
		scripted{resp: rules(false, rule([]any{"get"}, []any{""}, []any{"pods"}))},
		scripted{resp: engine.ReviewResponse{Status: 503, Mapped: clierr.Retryable(clierr.CodeGeneric, "503")}},
	)
	f.caller.Objects = map[string]*unstructured.Unstructured{} // no GET may be needed for discovered names
	res, err := NewList(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), fakeInput{strs: map[string]string{"cluster": "prod"}, bools: map[string]bool{"all-namespaces": true}}, nil)
	require.NoError(t, err)
	out := decodeList(t, res)
	require.Len(t, out.Namespaces, 2)
	assert.Equal(t, "team-a", out.Namespaces[0].Namespace, "sorted")
	assert.Equal(t, MemberAuthorizer, out.Namespaces[0].Source)
	assert.False(t, out.Namespaces[1].Evaluated)
	require.NotNil(t, out.Namespaces[1].Issue)
	assert.Equal(t, listRollup{Namespaces: 2, Evaluated: 1, Incomplete: 0, Issues: 1, Complete: false}, out.Rollup)
	assert.Equal(t, SourceMixed, out.ResultSource)
	assert.Equal(t, 13, res.ExitStatus)
}

// TestListSubjectWithoutClusterAccess asserts a subject the proxy refuses
// yields an AccessRules result with nothing evaluated, the proxy as the
// source, and exit 13.
func TestListSubjectWithoutClusterAccess(t *testing.T) {
	t.Parallel()
	noAccess := &fakeClient{script: []scripted{{resp: http403("")}}}
	f := listFactory("user-vwq4j")
	f.impersonated = map[string]*fakeClient{"u-abc12": noAccess}
	local := localWith("u-abc12")
	local.Lists["clusterroletemplatebindings|c-phmj7|"] = &unstructured.UnstructuredList{}
	local.Lists["projectroletemplatebindings||"] = &unstructured.UnstructuredList{}
	res, err := NewList(f, local).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a", "as": "u-abc12"}), nil)
	require.NoError(t, err)
	out := decodeList(t, res)
	assert.Equal(t, SourceRancherProxy, out.ResultSource)
	assert.Equal(t, 0, out.Rollup.Evaluated)
	assert.False(t, out.Rollup.Complete)
	assert.Contains(t, out.Note, "has no access to cluster c-phmj7")
	assert.Equal(t, 13, res.ExitStatus)
	assert.Equal(t, map[string]any{"cluster": "c-phmj7", "namespace": "team-a"}, scopeJSON(t, out.Scope))
}

// TestListEmptyProjectIsCompleteScopeDiscovery asserts a verified-empty
// project lists nothing, is complete, names scopeDiscovery as its source,
// and exits 0; the same holds when the proxy would have refused the
// subject, because nothing was evaluated.
func TestListEmptyProjectIsCompleteScopeDiscovery(t *testing.T) {
	t.Parallel()
	f := listFactory("user-vwq4j")
	f.caller.Lists["namespaces||field.cattle.io/projectId=p-empty"] = &unstructured.UnstructuredList{}
	res, err := NewList(f, f.caller).(runtime.ReadCommand).Execute(projectContext(t, f.caller, "p-empty", "Empty"), canInput(map[string]string{"project": "p-empty"}), nil)
	require.NoError(t, err)
	out := decodeList(t, res)
	assert.Empty(t, out.Namespaces)
	assert.Equal(t, listRollup{Complete: true}, out.Rollup)
	assert.Equal(t, SourceScopeDiscovery, out.ResultSource)
	assert.Contains(t, out.Note, "project p-empty has no namespaces on cluster c-phmj7")
	assert.Equal(t, 0, res.ExitStatus)
	assert.Equal(t, map[string]any{"cluster": "c-phmj7", "project": "p-empty"}, scopeJSON(t, out.Scope))

	f = listFactory("user-vwq4j")
	f.caller.Lists["namespaces||field.cattle.io/projectId=p-empty"] = &unstructured.UnstructuredList{}
	f.impersonated = map[string]*fakeClient{"u-abc12": {script: []scripted{{resp: http403("")}}}}
	local := localWith("u-abc12")
	local.Lists["clusterroletemplatebindings|c-phmj7|"] = &unstructured.UnstructuredList{}
	local.Lists["projectroletemplatebindings||"] = &unstructured.UnstructuredList{}
	res, err = NewList(f, local).(runtime.ReadCommand).Execute(projectContext(t, f.caller, "p-empty", "Empty"), canInput(map[string]string{"project": "p-empty", "as": "u-abc12"}), nil)
	require.NoError(t, err)
	out = decodeList(t, res)
	assert.Equal(t, SourceScopeDiscovery, out.ResultSource)
	assert.True(t, out.Rollup.Complete)
	assert.Equal(t, 0, res.ExitStatus)
}

// TestListProjectScopeCarriesMembers asserts the --project wire scope
// names the project by its id, also when --project was typed as the
// display name, and its member namespaces, and -A marks allNamespaces.
func TestListProjectScopeCarriesMembers(t *testing.T) {
	t.Parallel()
	f := listFactory("user-vwq4j", scripted{resp: rules(false)}, scripted{resp: rules(false)})
	f.caller.Lists["namespaces||field.cattle.io/projectId=p-cvrp4"] = &unstructured.UnstructuredList{Items: []unstructured.Unstructured{nsObj("team-b", nil), nsObj("team-a", nil)}}
	res, err := NewList(f, f.caller).(runtime.ReadCommand).Execute(projectContext(t, f.caller, "p-cvrp4", "Team"), canInput(map[string]string{"project": "Team"}), nil)
	require.NoError(t, err)
	out := decodeList(t, res)
	assert.Equal(t, map[string]any{"cluster": "c-phmj7", "project": "p-cvrp4", "namespaces": []any{"team-a", "team-b"}}, scopeJSON(t, out.Scope))

	f = listFactory("user-vwq4j", scripted{resp: rules(false)}, scripted{resp: rules(false)})
	res, err = NewList(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), fakeInput{strs: map[string]string{"cluster": "prod"}, bools: map[string]bool{"all-namespaces": true}}, nil)
	require.NoError(t, err)
	out = decodeList(t, res)
	assert.Equal(t, map[string]any{"cluster": "c-phmj7", "allNamespaces": true}, scopeJSON(t, out.Scope))
}

// TestListGroupIsValidationAndLocalNamedSubjectRefuses asserts the two
// list-specific refusals.
func TestListGroupIsValidationAndLocalNamedSubjectRefuses(t *testing.T) {
	t.Parallel()
	f := listFactory("user-vwq4j")
	_, err := NewList(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"as": "github_org://eng"}), nil)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Empty(t, f.caller.Reviews, "rejected before any request")

	_, err = NewList(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"as": "system:anonymous"}), nil)
	ce, ok = errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Empty(t, f.caller.Reviews, "anonymous is rejected before any request")

	f = listFactory("user-vwq4j")
	f.resolve["prod"] = engine.LocalCluster
	f.caller.Objects["u-abc12"] = userObj("u-abc12", "alice", "Alice", "local://u-abc12")
	_, err = NewList(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"as": "u-abc12"}), nil)
	ce, ok = errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	require.ErrorContains(t, err, "cannot impersonate")
}

// TestListTextIsOneRowPerGroupAndResource asserts the text expands rules
// to one row per API group and resource, sorted by group then resource,
// merging the verbs of rules about the same resource, keeping a rule with
// resource names on its own row (and showing NAMES only then), printing
// the core group as "core", fitting the columns to their content, and
// listing non-resource URLs one per row.
func TestListTextIsOneRowPerGroupAndResource(t *testing.T) {
	t.Parallel()
	named := map[string]any{"verbs": []any{"get"}, "apiGroups": []any{""}, "resources": []any{"configmaps"}, "resourceNames": []any{"my-config"}}
	f := listFactory("user-vwq4j", scripted{resp: rules(false,
		rule([]any{"get", "list", "watch"}, []any{"apps"}, []any{"deployments", "daemonsets"}),
		rule([]any{"create", "delete"}, []any{"apps"}, []any{"deployments"}),
		rule([]any{"get"}, []any{""}, []any{"pods"}),
		named,
	)})
	res, err := NewList(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(nil), nil)
	require.NoError(t, err)
	text := res.Text
	for _, want := range []string{
		`(?m)^  API GROUP\s+RESOURCE\s+VERBS\s+NAMES$`,
		`(?m)^  apps\s+daemonsets\s+get,list,watch$`,
		`(?m)^  apps\s+deployments\s+create,delete,get,list,watch$`,
		`(?m)^  core\s+configmaps\s+get\s+my-config$`,
		`(?m)^  core\s+pods\s+get$`,
		`(?m)^  NON-RESOURCE URL\s+VERBS$`,
		`(?m)^  /healthz\s+get$`,
	} {
		assert.Regexp(t, want, text)
	}
	assert.NotContains(t, text, "RESOURCES", "no per-rule column")
	assert.Less(t, strings.Index(text, "\n  apps "), strings.Index(text, "\n  core "), "sorted by the printed group, then resource")
	// Columns are fitted: every row's VERBS cell starts at the same column as the header's.
	headerCol := strings.Index(lineWith(t, text, "API GROUP"), "VERBS")
	assert.Equal(t, headerCol, strings.Index(lineWith(t, text, "  apps"), "get,list,watch"))
	assert.Equal(t, headerCol, strings.Index(lineWith(t, text, "  core"), "get"))
	assert.True(t, strings.HasSuffix(text, "\n"))

	// Without a named rule the NAMES column is absent.
	f = listFactory("user-vwq4j", scripted{resp: rules(false, rule([]any{"get"}, []any{""}, []any{"pods"}))})
	res, err = NewList(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(nil), nil)
	require.NoError(t, err)
	assert.Regexp(t, `(?m)^  API GROUP\s+RESOURCE\s+VERBS$`, res.Text)
	assert.NotContains(t, res.Text, "NAMES")
}

// lineWith returns the first line of text containing needle.
func lineWith(t *testing.T, text, needle string) string {
	t.Helper()
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	t.Fatalf("no line contains %q in:\n%s", needle, text)
	return ""
}

// TestListTimeoutBoundsTheSingleNamespace asserts --timeout is the budget
// of a plain list of one namespace as well (no --project, no -A): the
// first read after the preflight, the namespace GET, runs under it and
// its exhaustion is the staged, non-retryable budget error.
func TestListTimeoutBoundsTheSingleNamespace(t *testing.T) {
	t.Parallel()
	f := listFactory("user-vwq4j")
	f.caller.Before = blockAfterFirstCall()
	_, err := NewList(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{flagTimeout: "20ms"}), nil)
	require.ErrorContains(t, err, "fan-out budget of 20ms exhausted during namespace discovery")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.False(t, ce.Retryable)
	assert.Len(t, f.caller.Reviews, 1)
}

// TestListBudgetCoversProjectRead asserts --timeout is already running
// when the project is read: a lookup that waits for its context ends as
// the staged budget error, before any rules review.
func TestListBudgetCoversProjectRead(t *testing.T) {
	t.Parallel()
	f := listFactory("user-vwq4j")
	_, err := NewList(f, f.caller).(runtime.ReadCommand).Execute(blockingProjectContext(t), canInput(map[string]string{"project": "Team", flagTimeout: "20ms"}), nil)
	require.ErrorContains(t, err, "fan-out budget of 20ms exhausted during project resolution")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.False(t, ce.Retryable)
	assert.Len(t, f.caller.Reviews, 1, "the preflight ran; no rules were asked")
}
