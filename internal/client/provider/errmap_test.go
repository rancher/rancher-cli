package provider

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestMapErrPassesThroughStructuredError asserts that an error already carrying
// a *clierr.Error (a refused cross-origin redirect arrives wrapped in a
// *url.Error) keeps its code, message, and retryability instead of being
// rebuilt as GENERIC from its text.
func TestMapErrPassesThroughStructuredError(t *testing.T) {
	t.Parallel()
	inner := clierr.New(clierr.CodeGeneric, "refused to follow a redirect from https://a to https://b: a redirect to another origin is not followed, to keep credentials from being sent there; check that https://a is the correct address")
	wrapped := &url.Error{Op: "Patch", URL: "https://a/x", Err: inner}
	got := mapErr(t.Context(), http.MethodPatch, config.Effective{}, wrapped)
	ce, ok := errors.AsType[*clierr.Error](got)
	require.True(t, ok)
	assert.Same(t, inner, ce)
	assert.False(t, ce.Retryable)
}

// localManagementRefusal is the note a management.cattle.io refusal
// through the local cluster's proxy carries.
const localManagementRefusal = "; through the local cluster's proxy, Rancher answers some management.cattle.io resources, clusters among them, only for users with access to the local cluster"

// TestMapRequestErrNamesTheRefusedRequest asserts a refusal that gave no
// reason names the call: the verb, resource.group (the core group left
// out) and the name; a management.cattle.io request through the local
// cluster's proxy adds why Rancher refuses it; a 401 names the call.
func TestMapRequestErrNamesTheRefusedRequest(t *testing.T) {
	t.Parallel()
	forbidden := &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: 403}}
	for _, tc := range []struct {
		q    clierr.Request
		want string
	}{
		{clierr.Request{Verb: "list", Group: "management.cattle.io", Resource: "clusters", Cluster: "local"},
			"the server refused list clusters.management.cattle.io (403) and gave no reason" + localManagementRefusal},
		{clierr.Request{Verb: "get", Group: "management.cattle.io", Resource: "clusters", Name: "c-abc", Cluster: "local"},
			"the server refused get clusters.management.cattle.io 'c-abc' (403) and gave no reason" + localManagementRefusal},
		{clierr.Request{Verb: "get", Group: "management.cattle.io", Resource: "clusters", Name: "c-abc", Cluster: "c-abc"},
			"the server refused get clusters.management.cattle.io 'c-abc' (403) and gave no reason"},
		{clierr.Request{Verb: "list", Resource: "namespaces", Cluster: "local"},
			"the server refused list namespaces (403) and gave no reason"},
	} {
		ce, ok := errors.AsType[*clierr.Error](mapRequestErr(t.Context(), http.MethodGet, forbidden, &tc.q))
		require.True(t, ok)
		assert.Equal(t, clierr.CodeRBAC, ce.Code)
		assert.Equal(t, tc.want, ce.Message)
		assert.Empty(t, ce.Hint)
	}
	unauthorized := &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonUnauthorized, Code: 401}}
	ce, ok := errors.AsType[*clierr.Error](mapRequestErr(t.Context(), http.MethodGet, unauthorized,
		&clierr.Request{Verb: "list", Group: "management.cattle.io", Resource: "clusters", Cluster: "local"}))
	require.True(t, ok)
	assert.Equal(t, "the server rejected the credential (401) for list clusters.management.cattle.io", ce.Message)
	assert.Equal(t, clierr.Rancher+" auth status", ce.Hint)
}

// TestMapRequestErrDeadlineExceeded asserts a request that ran out of time
// with no response is a retryable GENERIC error: through the local cluster
// or with no cluster named it says the server may be slow and names no
// command; through a downstream cluster it also names the agent and the
// command that inspects that cluster, with its id quoted.
func TestMapRequestErrDeadlineExceeded(t *testing.T) {
	t.Parallel()
	timeout := &url.Error{Op: "Get", URL: "https://rancher.example/k8s/clusters/c-m-x/api/v1/nodes", Err: context.DeadlineExceeded}
	const cause = `Get "https://rancher.example/k8s/clusters/c-m-x/api/v1/nodes": context deadline exceeded`
	for name, tc := range map[string]struct {
		q    *clierr.Request
		want string
	}{
		"local cluster":            {&clierr.Request{Cluster: "local"}, cause + " - the server may be slow; retry later"},
		"no request":               {nil, cause + " - the server may be slow; retry later"},
		"no cluster named":         {&clierr.Request{}, cause + " - the server may be slow; retry later"},
		"a downstream cluster":     {&clierr.Request{Cluster: "c-m-x"}, cause + " - the server may be slow, or its agent may be disconnected; check the cluster with: " + clierr.Rancher + " inspect cluster 'c-m-x'"},
		"a cluster id with a dash": {&clierr.Request{Cluster: "-c"}, cause + " - the server may be slow, or its agent may be disconnected; check the cluster with: " + clierr.Rancher + " inspect cluster -- '-c'"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var ce *clierr.Error
			require.ErrorAs(t, mapRequestErr(t.Context(), http.MethodGet, timeout, tc.q), &ce)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.True(t, ce.Retryable, "request timeout must be retryable")
			assert.Equal(t, tc.want, ce.Message)
			assert.Empty(t, ce.Hint)
			assert.NotContains(t, ce.Message, "<cluster-id>")
		})
	}

	t.Run("context cancellation is not given the timeout hint", func(t *testing.T) {
		got := mapErr(t.Context(), http.MethodGet, config.Effective{}, &url.Error{Op: "Get", URL: "https://rancher.example", Err: context.Canceled})
		var ce *clierr.Error
		require.ErrorAs(t, got, &ce)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.False(t, ce.Retryable)
		assert.Empty(t, ce.Hint)
	})
}

// TestRestoreStatusRebuildsFromTheAnswersBody asserts that a raw request's
// error is rebuilt from the answer's body with the one decoder the other
// hand-read answers use: a Status body's or a JSON error object's message
// reaches the caller, a text body is the message as client-go reads it,
// and an empty body is rebuilt as an answer with no reason, without the
// request suffix client-go adds; an error with no status is left as it was.
func TestRestoreStatusRebuildsFromTheAnswersBody(t *testing.T) {
	t.Parallel()
	generic := apierrors.NewGenericServerResponse(http.StatusForbidden, "get", schema.GroupResource{Resource: "pods"}, "web", "unknown", 0, true)
	status := `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"pods \"web\" is forbidden: no pods/log","reason":"Forbidden","code":403}`

	got := restoreStatus(generic, []byte(status))
	require.True(t, apierrors.IsForbidden(got))
	require.ErrorContains(t, got, `pods "web" is forbidden: no pods/log`)

	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"rancher proxy object": {`{"type":"error","status":"403","message":"clusters.management.cattle.io \"c-1\" is forbidden"}`, `clusters.management.cattle.io "c-1" is forbidden`},
		"status without code":  {`{"kind":"Status","message":"x"}`, "x"},
		"log text":             {"2026-09-08T10:00:00Z started", "2026-09-08T10:00:00Z started"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := restoreStatus(generic, []byte(tc.body))
			require.True(t, apierrors.IsForbidden(got))
			code, ok := clierr.HTTPStatus(got)
			require.True(t, ok)
			assert.Equal(t, http.StatusForbidden, code)
			assert.EqualError(t, got, tc.want)
		})
	}

	t.Run("empty body", func(t *testing.T) {
		t.Parallel()
		got := restoreStatus(generic, nil)
		require.True(t, apierrors.IsForbidden(got))
		code, ok := clierr.HTTPStatus(got)
		require.True(t, ok)
		assert.Equal(t, http.StatusForbidden, code)
		assert.EqualError(t, got, "")
	})
	t.Run("no status", func(t *testing.T) {
		t.Parallel()
		plain := errors.New("connection refused")
		assert.Same(t, plain, restoreStatus(plain, []byte(status)))
	})
	assert.NoError(t, restoreStatus(nil, []byte(status)))
}

// TestLogsWordsAnEmptyRefusalByItsRequest asserts a 401 or 403 with no
// body on a pod log read, the one typed request, reads as any answer with
// no reason does: the 403 names the request and says the server gave no
// reason, the 401 names the request and the credential with the
// rancher auth status hint, and neither keeps the request client-go adds
// to its own text.
func TestLogsWordsAnEmptyRefusalByItsRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		status      int
		contentType string
		code        string
		message     string
		hint        string
	}{
		{"403", http.StatusForbidden, "", clierr.CodeRBAC, "the server refused get pods/log 'x' (403) and gave no reason", ""},
		{"403 application/json", http.StatusForbidden, "application/json", clierr.CodeRBAC, "the server refused get pods/log 'x' (403) and gave no reason", ""},
		{"401", http.StatusUnauthorized, "", clierr.CodeAuth, "the server rejected the credential (401) for get pods/log", clierr.AuthStatusRemedy},
		{"401 application/json", http.StatusUnauthorized, "application/json", clierr.CodeAuth, "the server rejected the credential (401) for get pods/log", clierr.AuthStatusRemedy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := refusingServer(t, refusal{status: tc.status, contentType: tc.contentType})
			c, err := NewClusterClient(&rest.Config{Host: srv.URL, BearerToken: "t", Transport: srv.Client().Transport}, "local")
			require.NoError(t, err)
			_, err = c.Logs(t.Context(), "default", "x", engine.LogOptions{})
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, tc.message, ce.Message)
			assert.Equal(t, tc.hint, ce.Hint)
			status, ok := clierr.HTTPStatus(err)
			require.True(t, ok)
			assert.Equal(t, tc.status, status)
		})
	}
}
