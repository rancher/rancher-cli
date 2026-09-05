package provider

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

func reviewObj(kind string) *unstructured.Unstructured {
	group := "authorization.k8s.io"
	if kind == "SelfSubjectReview" {
		group = "authentication.k8s.io"
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": group + "/v1", "kind": kind, "spec": map[string]any{},
	}}
}

// newReviewServer serves one scripted answer for every POST and counts
// the requests it saw.
func newReviewServer(t *testing.T, status int, contentType, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		assert.Equal(t, http.MethodPost, r.Method)
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func clientFor(t *testing.T, srv *httptest.Server) engine.ClusterClient {
	t.Helper()
	c, err := NewClusterClient(&rest.Config{Host: srv.URL, BearerToken: "t"}, "local")
	require.NoError(t, err)
	return c
}

// TestPostReviewReturnsStatusAndBodyForAllowed asserts a 201 with a
// SubjectAccessReview status decodes into Object with Mapped nil.
func TestPostReviewReturnsStatusAndBodyForAllowed(t *testing.T) {
	t.Parallel()
	body := `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","status":{"allowed":true,"reason":"RBAC: allowed by ClusterRoleBinding \"x\" of ClusterRole \"y\" to User \"z\""}}`
	srv, n := newReviewServer(t, http.StatusCreated, "application/json", body)
	c := clientFor(t, srv)
	res, err := c.PostReview(t.Context(), engine.ReviewRequest{Kind: engine.ReviewSubjectAccess, Object: reviewObj("SubjectAccessReview")})
	require.NoError(t, err)
	assert.Equal(t, http.StatusCreated, res.Status)
	assert.Nil(t, res.Mapped)
	allowed, _, _ := unstructured.NestedBool(res.Object.Object, "status", "allowed")
	assert.True(t, allowed)
	assert.Equal(t, int32(1), n.Load())
}

// TestPostReviewKeepsEmpty403Body asserts the Rancher proxy's empty 403
// comes back as Status 403, empty Body, Mapped CodeRBAC, nil error.
func TestPostReviewKeepsEmpty403Body(t *testing.T) {
	t.Parallel()
	srv, _ := newReviewServer(t, http.StatusForbidden, "", "")
	c := clientFor(t, srv)
	res, err := c.PostReview(t.Context(), engine.ReviewRequest{Kind: engine.ReviewSelfSubjectIdentity, Object: reviewObj("SelfSubjectReview")})
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, res.Status)
	assert.Empty(t, res.Body)
	require.NotNil(t, res.Mapped)
	assert.Equal(t, clierr.CodeRBAC, res.Mapped.Code)
}

// TestPostReviewKeepsRancherRefusalBody asserts the proxy's impersonation
// refusal text survives in Body for the caller's classification.
func TestPostReviewKeepsRancherRefusalBody(t *testing.T) {
	t.Parallel()
	srv, _ := newReviewServer(t, http.StatusForbidden, "application/json", `{"message":"not allowed to impersonate user"}`)
	c := clientFor(t, srv)
	res, err := c.PostReview(t.Context(), engine.ReviewRequest{Kind: engine.ReviewSelfSubjectIdentity, Object: reviewObj("SelfSubjectReview")})
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, res.Status)
	assert.Contains(t, string(res.Body), "not allowed to impersonate user")
	assert.Equal(t, clierr.CodeRBAC, res.Mapped.Code)
}

// TestPostReviewMapsApiserverStatus asserts a Kubernetes Status body is
// mapped with its message and code.
func TestPostReviewMapsApiserverStatus(t *testing.T) {
	t.Parallel()
	body := `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"subjectaccessreviews.authorization.k8s.io is forbidden: User \"u\" cannot create resource \"subjectaccessreviews\"","reason":"Forbidden","code":403}`
	srv, _ := newReviewServer(t, http.StatusForbidden, "application/json", body)
	c := clientFor(t, srv)
	res, err := c.PostReview(t.Context(), engine.ReviewRequest{Kind: engine.ReviewSubjectAccess, Object: reviewObj("SubjectAccessReview")})
	require.NoError(t, err)
	require.NotNil(t, res.Mapped)
	assert.Equal(t, clierr.CodeRBAC, res.Mapped.Code)
	assert.Contains(t, res.Mapped.Message, "cannot create resource")
}

// TestPostReviewRetryabilityTable asserts 502, 503, 504 map retryable,
// a Status 500 does not, and any plain-text 500 (the proxy-path shape,
// classified by shape as mapErr does) does.
func TestPostReviewRetryabilityTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		status      int
		contentType string
		body        string
		retryable   bool
	}{
		{"502", http.StatusBadGateway, "text/plain", "bad gateway", true},
		{"503", http.StatusServiceUnavailable, "application/json", `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"unavailable","reason":"ServiceUnavailable","code":503}`, true},
		{"504", http.StatusGatewayTimeout, "text/plain", "gateway timeout", true},
		{"500 status", http.StatusInternalServerError, "application/json", `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"boom","reason":"InternalError","code":500}`, false},
		{"500 proxy text", http.StatusInternalServerError, "text/plain", "cluster agent disconnected", true},
		{"500 other text", http.StatusInternalServerError, "text/plain", "upstream broke", true},
		{"599", 599, "text/plain", "odd", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv, _ := newReviewServer(t, c.status, c.contentType, c.body)
			cl := clientFor(t, srv)
			res, err := cl.PostReview(t.Context(), engine.ReviewRequest{Kind: engine.ReviewSubjectAccess, Object: reviewObj("SubjectAccessReview")})
			require.NoError(t, err)
			require.NotNil(t, res.Mapped)
			assert.Equal(t, clierr.CodeGeneric, res.Mapped.Code)
			assert.Equal(t, c.retryable, res.Mapped.Retryable)
		})
	}
}

// TestPostReviewKeepsRancherErrorObject403 asserts the proxy's
// cluster-access refusal (403, Rancher's error object as text/plain) is
// preserved with its body and mapped to CodeRBAC with a non-empty
// message, so the engine can classify it by shape.
func TestPostReviewKeepsRancherErrorObject403(t *testing.T) {
	t.Parallel()
	body := `{"Code":{"Code":"Forbidden","Status":403},"Message":"clusters.management.cattle.io \"c-1\" is forbidden: User \"u-x\" cannot get resource \"clusters\" in API group \"management.cattle.io\" at the cluster scope","Cause":null,"FieldName":""}`
	srv, _ := newReviewServer(t, http.StatusForbidden, "text/plain; charset=utf-8", body)
	c := clientFor(t, srv)
	res, err := c.PostReview(t.Context(), engine.ReviewRequest{Kind: engine.ReviewSelfSubjectIdentity, Object: reviewObj("SelfSubjectReview")})
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, res.Status)
	assert.Equal(t, body, string(res.Body))
	require.NotNil(t, res.Mapped)
	assert.Equal(t, clierr.CodeRBAC, res.Mapped.Code)
	assert.Contains(t, res.Mapped.Message, "is forbidden")
}

// TestPostReviewMaps401 asserts an unauthenticated answer keeps its status
// and maps to CodeAuth with the denied fallback message.
func TestPostReviewMaps401(t *testing.T) {
	t.Parallel()
	srv, _ := newReviewServer(t, http.StatusUnauthorized, "", "")
	c := clientFor(t, srv)
	res, err := c.PostReview(t.Context(), engine.ReviewRequest{Kind: engine.ReviewSelfSubjectAccess, Object: reviewObj("SelfSubjectAccessReview")})
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, res.Status)
	require.NotNil(t, res.Mapped)
	assert.Equal(t, clierr.CodeAuth, res.Mapped.Code)
	assert.NotEmpty(t, res.Mapped.Message)
}

// TestPostReviewNoResponseIsRetryableError asserts a closed listener
// yields a non-nil retryable error and a zero response.
func TestPostReviewNoResponseIsRetryableError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c, err := NewClusterClient(&rest.Config{Host: url, BearerToken: "t"}, "local")
	require.NoError(t, err)
	res, err := c.PostReview(t.Context(), engine.ReviewRequest{Kind: engine.ReviewSelfSubjectAccess, Object: reviewObj("SelfSubjectAccessReview")})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.True(t, ce.Retryable)
	assert.Equal(t, engine.ReviewResponse{}, res)
}

// TestPostReviewTimeoutIsRetryableError asserts a hung server trips the
// per-request timeout seam and surfaces as a retryable error.
func TestPostReviewTimeoutIsRetryableError(t *testing.T) {
	// Not t.Parallel(): shrinks the package-global requestTimeout.
	restore := SetRequestTimeoutForTest(100 * time.Millisecond)
	defer restore()
	// The handler blocks on a channel closed before the server shuts down,
	// never on the request context: a client-side timeout leaves the
	// connection open, so a handler waiting on the request context would
	// hold Close forever.
	block := make(chan struct{})
	defer close(block)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
		w.WriteHeader(http.StatusCreated)
	}))
	// t.Cleanup, not defer: cleanups run after every defer, so the handler
	// has already been released by the time the server shuts down.
	t.Cleanup(srv.Close)
	c := clientFor(t, srv)
	_, err := c.PostReview(t.Context(), engine.ReviewRequest{Kind: engine.ReviewSelfSubjectAccess, Object: reviewObj("SelfSubjectAccessReview")})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.True(t, ce.Retryable)
}

// TestPostReviewProtocolErrors asserts malformed or incomplete 2xx bodies
// are terminal, non-retryable GENERIC errors and never decode.
func TestPostReviewProtocolErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		kind engine.ReviewKind
		obj  string
		body string
	}{
		{"malformed json", engine.ReviewSubjectAccess, "SubjectAccessReview", `{"status":`},
		{"wrong kind", engine.ReviewSubjectAccess, "SubjectAccessReview", `{"apiVersion":"v1","kind":"Pod","status":{"allowed":true}}`},
		{"status empty", engine.ReviewSubjectAccess, "SubjectAccessReview", `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","status":{}}`},
		{"allowed wrong type", engine.ReviewSubjectAccess, "SubjectAccessReview", `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","status":{"allowed":"yes"}}`},
		{"rules incomplete missing", engine.ReviewSelfSubjectRules, "SelfSubjectRulesReview", `{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectRulesReview","status":{"resourceRules":[]}}`},
		{"rules wrong array type", engine.ReviewSelfSubjectRules, "SelfSubjectRulesReview", `{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectRulesReview","status":{"incomplete":false,"resourceRules":"x"}}`},
		{"identity empty username", engine.ReviewSelfSubjectIdentity, "SelfSubjectReview", `{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview","status":{"userInfo":{"username":""}}}`},
		{"identity groups not strings", engine.ReviewSelfSubjectIdentity, "SelfSubjectReview", `{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview","status":{"userInfo":{"username":"u","groups":[1]}}}`},
		{"rules entry not an object", engine.ReviewSelfSubjectRules, "SelfSubjectRulesReview", `{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectRulesReview","status":{"incomplete":false,"resourceRules":[1]}}`},
		{"rules verbs not strings", engine.ReviewSelfSubjectRules, "SelfSubjectRulesReview", `{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectRulesReview","status":{"incomplete":false,"resourceRules":[{"verbs":[1],"resources":["pods"]}]}}`},
		{"non-resource urls not strings", engine.ReviewSelfSubjectRules, "SelfSubjectRulesReview", `{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectRulesReview","status":{"incomplete":false,"nonResourceRules":[{"verbs":["get"],"nonResourceURLs":[true]}]}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv, _ := newReviewServer(t, http.StatusCreated, "application/json", c.body)
			cl := clientFor(t, srv)
			_, err := cl.PostReview(t.Context(), engine.ReviewRequest{Kind: c.kind, Object: reviewObj(c.obj)})
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "expected a clierr, got %v", err)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.False(t, ce.Retryable)
		})
	}
}

// TestPostReviewTransportStatusWins asserts the HTTP status, not a Status
// body's code field, decides the public error code.
func TestPostReviewTransportStatusWins(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{"401 carrying code 403", http.StatusUnauthorized, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"x","reason":"Forbidden","code":403}`, clierr.CodeAuth},
		{"403 carrying code 404", http.StatusForbidden, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"x","reason":"NotFound","code":404}`, clierr.CodeRBAC},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv, _ := newReviewServer(t, c.status, "application/json", c.body)
			cl := clientFor(t, srv)
			res, err := cl.PostReview(t.Context(), engine.ReviewRequest{Kind: engine.ReviewSubjectAccess, Object: reviewObj("SubjectAccessReview")})
			require.NoError(t, err)
			assert.Equal(t, c.status, res.Status)
			require.NotNil(t, res.Mapped)
			assert.Equal(t, c.code, res.Mapped.Code)
		})
	}
}

// TestPostReviewRejectsUnknownKindBeforeTransport asserts a non-review
// kind never produces a request.
func TestPostReviewRejectsUnknownKindBeforeTransport(t *testing.T) {
	t.Parallel()
	srv, n := newReviewServer(t, http.StatusCreated, "application/json", `{}`)
	c := clientFor(t, srv)
	_, err := c.PostReview(t.Context(), engine.ReviewRequest{Kind: engine.ReviewKind("secrets"), Object: reviewObj("Secret")})
	require.ErrorContains(t, err, "not a review kind")
	assert.Equal(t, int32(0), n.Load())
}

// TestPostReviewRejectsMismatchedGVKBeforeTransport asserts an object whose
// kind does not match the review kind never produces a request.
func TestPostReviewRejectsMismatchedGVKBeforeTransport(t *testing.T) {
	t.Parallel()
	srv, n := newReviewServer(t, http.StatusCreated, "application/json", `{}`)
	c := clientFor(t, srv)
	_, err := c.PostReview(t.Context(), engine.ReviewRequest{Kind: engine.ReviewSubjectAccess, Object: reviewObj("SelfSubjectRulesReview")})
	require.ErrorContains(t, err, "does not match")
	assert.Equal(t, int32(0), n.Load())
}

// TestPostReviewSendsImpersonateUserOnly asserts the impersonation
// config yields exactly one Impersonate-User header and no group header.
func TestPostReviewSendsImpersonateUserOnly(t *testing.T) {
	t.Parallel()
	var seenUser, seenGroup string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUser = r.Header.Get("Impersonate-User")
		seenGroup = r.Header.Get("Impersonate-Group")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview",
			"status": map[string]any{"userInfo": map[string]any{"username": "u-abc12"}}})
	}))
	t.Cleanup(srv.Close)
	rc := &rest.Config{Host: srv.URL, BearerToken: "t", Impersonate: rest.ImpersonationConfig{UserName: "u-abc12"}}
	c, err := NewClusterClient(rc, "c-1")
	require.NoError(t, err)
	_, err = c.PostReview(t.Context(), engine.ReviewRequest{Kind: engine.ReviewSelfSubjectIdentity, Object: reviewObj("SelfSubjectReview")})
	require.NoError(t, err)
	assert.Equal(t, "u-abc12", seenUser)
	assert.Empty(t, seenGroup)
}
