package access

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

// TestClassifyAccessMapsPerReviewType asserts the SubjectAccessReview
// mapping: allowed stays allowed even with an evaluation error, denied
// with an evaluation error is unknown, denied without one is denied.
func TestClassifyAccessMapsPerReviewType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status map[string]any
		want   Decision
	}{
		{"allowed", map[string]any{"allowed": true, "reason": "RBAC: allowed by ClusterRoleBinding \"a\" of ClusterRole \"b\" to User \"c\""}, DecisionAllowed},
		{"allowed with evaluation error", map[string]any{"allowed": true, "evaluationError": "webhook slow"}, DecisionAllowed},
		{"denied with evaluation error", map[string]any{"allowed": false, "evaluationError": "webhook down"}, DecisionUnknown},
		{"denied", map[string]any{"allowed": false}, DecisionDenied},
		{"denied explicit", map[string]any{"allowed": false, "denied": true}, DecisionDenied},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			out := classifyAccess(review(engine.ReviewSelfSubjectAccess, c.status).Object)
			assert.Equal(t, c.want, out.Decision)
			if r, ok := c.status["reason"]; ok {
				assert.Equal(t, r, out.Reason)
			}
		})
	}
}

// TestPostReviewClassifiesTheThree403Shapes asserts Rancher's error
// object (or an empty body) means no cluster access, the Rancher message
// means impersonation refused, and an apiserver Status means the review
// endpoint refused (terminal).
func TestPostReviewClassifiesTheThree403Shapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		resp engine.ReviewResponse
		want ReviewOutcomeKind
	}{
		{"cluster-access refusal (Rancher error object)", http403(`{"Code":{"Code":"Forbidden","Status":403},"Message":"clusters.management.cattle.io \"c-phmj7\" is forbidden: User \"m-54pdw\" cannot get resource \"clusters\" in API group \"management.cattle.io\" at the cluster scope","Cause":null,"FieldName":""}`), outcomeNoClusterAccess},
		{"cluster-access refusal (empty body)", http403(""), outcomeNoClusterAccess},
		{"impersonate user", http403(`{"message":"not allowed to impersonate user"}`), outcomeImpersonationRefused},
		{"impersonate service account", http403(`{"message":"not allowed to impersonate service account"}`), outcomeImpersonationRefused},
		{"plain-text refusal", http403(`not allowed to impersonate user`), outcomeImpersonationRefused},
		{"Status body mentioning the refusal text", http403(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"users is forbidden: not allowed to impersonate user","reason":"Forbidden","code":403}`), outcomeTerminal},
		{"apiserver status", http403(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"cannot create resource \"selfsubjectaccessreviews\""}`), outcomeTerminal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fc := &fakeClient{script: []scripted{{resp: c.resp}}}
			got := postReview(t.Context(), fc, engine.ReviewSelfSubjectIdentity, identityReviewObject())
			assert.Equal(t, c.want, got.kind)
		})
	}
}

// TestPostReviewMatrixTerminalAndDegradable asserts 401 and any other 4xx
// are terminal, 5xx is degradable, and a transport error is degradable.
func TestPostReviewMatrixTerminalAndDegradable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		s    scripted
		want ReviewOutcomeKind
	}{
		{"401", scripted{resp: engine.ReviewResponse{Status: 401, Mapped: clierr.New(clierr.CodeAuth, "must authenticate")}}, outcomeTerminal},
		{"404", scripted{resp: engine.ReviewResponse{Status: 404, Mapped: clierr.New(clierr.CodeNotFound, "no such endpoint")}}, outcomeTerminal},
		{"405", scripted{resp: engine.ReviewResponse{Status: 405, Mapped: clierr.New(clierr.CodeValidation, "method not allowed")}}, outcomeTerminal},
		{"503", scripted{resp: engine.ReviewResponse{Status: 503, Mapped: clierr.Retryable(clierr.CodeGeneric, "unavailable")}}, outcomeDegradable},
		{"500", scripted{resp: engine.ReviewResponse{Status: 500, Mapped: clierr.New(clierr.CodeGeneric, "boom")}}, outcomeDegradable},
		{"500 impersonated service account not found", scripted{resp: saNotFound500("serviceaccounts \"ghost\" not found")}, outcomeSubjectNotFound},
		{"302 (outside 4xx/5xx is terminal)", scripted{resp: engine.ReviewResponse{Status: 302, Mapped: clierr.New(clierr.CodeGeneric, "unexpected redirect")}}, outcomeTerminal},
		{"transport (provider marks no-response retryable)", scripted{err: clierr.Retryable(clierr.CodeGeneric, "no response from the cluster: dial tcp: connection refused")}, outcomeDegradable},
		{"protocol", scripted{err: clierr.New(clierr.CodeGeneric, "review response has no status")}, outcomeTerminal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fc := &fakeClient{script: []scripted{c.s}}
			got := postReview(t.Context(), fc, engine.ReviewSelfSubjectAccess, accessReviewObject(Question{Verb: "get", Resource: "pods", Version: "v1"}, "default"))
			assert.Equal(t, c.want, got.kind, "%+v", got)
			if c.want == outcomeDegradable {
				require.NotNil(t, got.err)
				iss := issueFrom(got.err)
				assert.Equal(t, got.err.Code, iss.Code)
				assert.Equal(t, got.err.Retryable, iss.Retryable)
			}
			if c.want == outcomeSubjectNotFound {
				require.NotNil(t, got.err)
				assert.Equal(t, clierr.CodeNotFound, got.err.Code)
				assert.False(t, got.err.Retryable)
				assert.Equal(t, "can't create token: serviceaccounts \"ghost\" not found", got.err.Message)
			}
		})
	}
}

// TestPostReviewReturnsTheAnswerFor2xx asserts a decoded 2xx answer of
// either shape is an answer outcome carrying the object.
func TestPostReviewReturnsTheAnswerFor2xx(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		kind engine.ReviewKind
		obj  *unstructured.Unstructured
		resp engine.ReviewResponse
	}{
		{"identity", engine.ReviewSelfSubjectIdentity, identityReviewObject(), whoamiResp("user-vwq4j", "system:authenticated")},
		{"access", engine.ReviewSelfSubjectAccess, accessReviewObject(Question{Verb: "get", Resource: "pods", Version: "v1"}, "default"), sar(true, "RBAC: allowed by ClusterRoleBinding \"a\" of ClusterRole \"b\" to User \"c\"")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fc := &fakeClient{script: []scripted{{resp: c.resp}}}
			got := postReview(t.Context(), fc, c.kind, c.obj)
			require.Equal(t, outcomeAnswer, got.kind)
			require.NotNil(t, got.resp.Object)
			assert.Nil(t, got.err)
			require.Len(t, fc.Reviews, 1)
			assert.Equal(t, c.kind, fc.Reviews[0].Kind)
		})
	}
}

// TestReviewObjectsCarryTheResolvedVersionAndGroups asserts the spec each
// builder sends: a SubjectAccessReview names the user and the groups it
// was given, a rules review names its namespace, and a resource question
// carries the version discovery resolved.
func TestReviewObjectsCarryTheResolvedVersionAndGroups(t *testing.T) {
	t.Parallel()
	q := Question{Verb: "list", Resource: "deployments", Group: "apps", Version: "v1", Name: "web", Subresource: "scale"}

	sarObj := subjectAccessReviewObject(q, "prod", "user-vwq4j", []string{"system:authenticated", "team-a"})
	gvk, _ := engine.ReviewGVK(engine.ReviewSubjectAccess)
	assert.Equal(t, gvk, sarObj.GroupVersionKind())
	user, _, _ := unstructured.NestedString(sarObj.Object, "spec", "user")
	assert.Equal(t, "user-vwq4j", user)
	groups, _, _ := unstructured.NestedStringSlice(sarObj.Object, "spec", "groups")
	assert.Equal(t, []string{"system:authenticated", "team-a"}, groups)
	ra, _, _ := unstructured.NestedStringMap(sarObj.Object, "spec", "resourceAttributes")
	assert.Equal(t, map[string]string{
		"verb": "list", "resource": "deployments", "group": "apps", "version": "v1",
		"namespace": "prod", "name": "web", "subresource": "scale",
	}, ra)

	// A group-only subject sends no user field at all, so the authorizer
	// answers about the groups alone.
	groupOnly := subjectAccessReviewObject(q, "prod", "", []string{"team-a"})
	_, hasUser, _ := unstructured.NestedString(groupOnly.Object, "spec", "user")
	assert.False(t, hasUser)

	rules := rulesReviewObject("prod")
	rgvk, _ := engine.ReviewGVK(engine.ReviewSelfSubjectRules)
	assert.Equal(t, rgvk, rules.GroupVersionKind())
	ns, _, _ := unstructured.NestedString(rules.Object, "spec", "namespace")
	assert.Equal(t, "prod", ns)

	nonRes := accessReviewObject(Question{Verb: "get", NonResourceURL: "/healthz"}, "")
	nra, _, _ := unstructured.NestedStringMap(nonRes.Object, "spec", "nonResourceAttributes")
	assert.Equal(t, map[string]string{"verb": "get", "path": "/healthz"}, nra)
	_, hasRes, _ := unstructured.NestedMap(nonRes.Object, "spec", "resourceAttributes")
	assert.False(t, hasRes)
}

// TestPostReviewCancellationIsTerminal asserts a cancelled context never
// degrades to an issue.
func TestPostReviewCancellationIsTerminal(t *testing.T) {
	t.Parallel()
	fc := &fakeClient{script: []scripted{{err: errors.Join(context.Canceled)}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got := postReview(ctx, fc, engine.ReviewSelfSubjectAccess, accessReviewObject(Question{Verb: "get", Resource: "pods", Version: "v1"}, "default"))
	assert.Equal(t, outcomeTerminal, got.kind)
}
