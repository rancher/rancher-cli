package access

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

// TestReviewNamespacesAsksGetNamespaceByNameAndKeepsTheReason asserts one
// SelfSubjectAccessReview per namespace, naming the namespace as the
// resource name and as the request's namespace attribute (as the apiserver
// derives it for a GET of a Namespace object), and the server's reason
// kept verbatim.
func TestReviewNamespacesAsksGetNamespaceByNameAndKeepsTheReason(t *testing.T) {
	old := fanOutLimit
	fanOutLimit = 1
	t.Cleanup(func() { fanOutLimit = old })
	c := &fakeClient{script: []scripted{
		{resp: sar(true, `RBAC: allowed by ClusterRoleBinding "crb-1" of ClusterRole "p-1-namespaces-edit" to User "u-1"`)},
		{resp: sar(false, "")},
	}}
	rows, failure := reviewNamespaces(t.Context(), c, []string{"default", "kube-system"})
	require.Nil(t, failure)
	assert.Equal(t, []namespaceRow{
		{Namespace: "default", Access: nsAllowed, Source: RowAuthorizer, Reason: `RBAC: allowed by ClusterRoleBinding "crb-1" of ClusterRole "p-1-namespaces-edit" to User "u-1"`},
		{Namespace: "kube-system", Access: nsDenied, Source: RowAuthorizer},
	}, rows)
	require.Len(t, c.Reviews, 2)
	for i, ns := range []string{"default", "kube-system"} {
		assert.Equal(t, engine.ReviewSelfSubjectAccess, c.Reviews[i].Kind)
		ra, _, _ := unstructured.NestedMap(c.Reviews[i].Object.Object, "spec", "resourceAttributes")
		assert.Equal(t, "get", ra["verb"])
		assert.Equal(t, "namespaces", ra["resource"])
		assert.Equal(t, ns, ra["name"])
		assert.Equal(t, ns, ra["namespace"], "a RoleBinding inside the namespace must count, as it does for the real GET")
	}
}

// TestReviewNamespacesEvaluationErrorIsUnknownFromTheAuthorizer asserts a
// not-allowed answer carrying an evaluationError is unknown with source
// authorizer, and the error text is kept.
func TestReviewNamespacesEvaluationErrorIsUnknownFromTheAuthorizer(t *testing.T) {
	c := scriptedClient(review(engine.ReviewSelfSubjectAccess, map[string]any{"allowed": false, "evaluationError": "webhook timed out"}))
	rows, failure := reviewNamespaces(t.Context(), c, []string{"a"})
	require.Nil(t, failure)
	assert.Equal(t, namespaceRow{Namespace: "a", Access: nsUnknown, Source: RowAuthorizer, EvaluationError: "webhook timed out"}, rows[0])
}

// TestReviewNamespacesRequestFailureIsUnknownWithAnIssue asserts a
// degradable failure (a retryable transport error) is the namespace's issue.
func TestReviewNamespacesRequestFailureIsUnknownWithAnIssue(t *testing.T) {
	old := fanOutLimit
	fanOutLimit = 1
	t.Cleanup(func() { fanOutLimit = old })
	c := &fakeClient{script: []scripted{{resp: sar(true, "")}, {err: clierr.Retryable(clierr.CodeGeneric, "connection reset")}}}
	rows, failure := reviewNamespaces(t.Context(), c, []string{"a", "b"})
	require.Nil(t, failure)
	assert.Equal(t, nsAllowed, rows[0].Access)
	assert.Equal(t, nsUnknown, rows[1].Access)
	assert.Equal(t, RowRequest, rows[1].Source)
	require.NotNil(t, rows[1].Issue)
	assert.True(t, rows[1].Issue.Retryable)
	assert.Equal(t, "connection reset", rows[1].Issue.Message)
}

// TestReviewNamespacesTerminalFailureAbortsTheFanOut asserts a non-retryable
// request failure aborts the whole namespace fan-out.
func TestReviewNamespacesTerminalFailureAbortsTheFanOut(t *testing.T) {
	c := &fakeClient{script: []scripted{{err: clierr.New(clierr.CodeAuth, "401")}}}
	rows, failure := reviewNamespaces(t.Context(), c, []string{"a", "b"})
	assert.Nil(t, rows)
	require.NotNil(t, failure)
	assert.Equal(t, clierr.CodeAuth, failure.Err.Code)
}

func TestReviewNamespacesNoneToAskIsNoRows(t *testing.T) {
	t.Parallel()
	c := &fakeClient{}
	rows, failure := reviewNamespaces(t.Context(), c, nil)
	require.Nil(t, failure)
	assert.Empty(t, rows)
	assert.Empty(t, c.Reviews)
}

func TestRollupVisibility(t *testing.T) {
	t.Parallel()
	allowed := func(ns string) namespaceRow {
		return namespaceRow{Namespace: ns, Access: nsAllowed, Source: RowAuthorizer}
	}
	denied := func(ns string) namespaceRow {
		return namespaceRow{Namespace: ns, Access: nsDenied, Source: RowAuthorizer}
	}
	unknownReq := func(ns string) namespaceRow {
		return namespaceRow{Namespace: ns, Access: nsUnknown, Source: RowRequest, Issue: &Issue{Code: "GENERIC", Message: "x"}}
	}
	cases := map[string]struct {
		rows   []namespaceRow
		failed bool
		want   visibility
		cell   string
	}{
		"all":                  {[]namespaceRow{allowed("a"), allowed("b")}, false, visibility{Visibility: visibilityAll, Source: SourceAuthorizer, AllowedIn: 2, Complete: true}, "all"},
		"partial":              {[]namespaceRow{allowed("a"), denied("b"), denied("c")}, false, visibility{Visibility: visibilityPartial, Source: SourceAuthorizer, AllowedIn: 1, Complete: true}, "partial (1 of 3)"},
		"partial with unknown": {[]namespaceRow{allowed("a"), allowed("b"), unknownReq("c")}, false, visibility{Visibility: visibilityPartial, Source: SourceMixed, AllowedIn: 2, UnknownIn: 1}, "partial (2 of 3, 1 unknown)"},
		"none":                 {[]namespaceRow{denied("a"), denied("b")}, false, visibility{Visibility: visibilityNone, Source: SourceAuthorizer, Complete: true}, "none"},
		"unknown":              {[]namespaceRow{denied("a"), unknownReq("b")}, false, visibility{Visibility: visibilityUnknown, Source: SourceMixed, UnknownIn: 1}, "unknown"},
		"all unknown requests": {[]namespaceRow{unknownReq("a")}, false, visibility{Visibility: visibilityUnknown, Source: ResultSource(RowRequest), UnknownIn: 1}, "unknown"},
		"empty project":        {nil, false, visibility{Visibility: visibilityUnknown, Source: SourceScopeDiscovery, Note: emptyProjectNote}, "unknown"},
		"discovery failed":     {[]namespaceRow{allowed("a")}, true, visibility{Visibility: visibilityUnknown, Source: SourceScopeDiscovery, Note: discoveryFailedNote}, "unknown"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := rollupVisibility(tc.rows, tc.failed)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.cell, visibilityCell(got, len(tc.rows)))
		})
	}
}
