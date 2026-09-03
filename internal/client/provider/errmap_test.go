package provider

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestMapErrPassesThroughStructuredError asserts that an error already carrying
// a *clierr.Error (a refused cross-origin redirect arrives wrapped in a
// *url.Error) keeps its code, message, and retryability instead of being
// rebuilt as GENERIC from its text.
func TestMapErrPassesThroughStructuredError(t *testing.T) {
	t.Parallel()
	inner := clierr.New(clierr.CodeGeneric, "refused to follow a redirect from https://a to https://b: credentialed redirects are not followed across origins; check that https://a is the correct address")
	wrapped := &url.Error{Op: "Patch", URL: "https://a/x", Err: inner}
	got := mapErr(wrapped)
	ce, ok := errors.AsType[*clierr.Error](got)
	require.True(t, ok)
	assert.Same(t, inner, ce)
	assert.False(t, ce.Retryable)
}

func TestMapErr(t *testing.T) {
	gr := schema.GroupResource{Group: "management.cattle.io", Resource: "projects"}
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"nil", nil, ""},
		{"notfound", apierrors.NewNotFound(gr, "p-x"), clierr.CodeNotFound},
		{"forbidden", apierrors.NewForbidden(gr, "p-x", errors.New("nope")), clierr.CodeRBAC},
		{"unauthorized", apierrors.NewUnauthorized("bad token"), clierr.CodeAuth},
		{"conflict", apierrors.NewConflict(gr, "p-x", errors.New("rv")), clierr.CodeConflict},
		{"exists", apierrors.NewAlreadyExists(gr, "p-x"), clierr.CodeConflict},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Group: gr.Group, Kind: "Project"}, "p-x", nil), clierr.CodeValidation},
		{"badrequest", apierrors.NewBadRequest("bad"), clierr.CodeValidation},
		{"other", errors.New("boom"), clierr.CodeGeneric},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapErr(tc.err)
			if tc.code == "" {
				assert.NoError(t, got)
				return
			}
			var ce *clierr.Error
			require.ErrorAs(t, got, &ce)
			assert.Equal(t, tc.code, ce.Code)
		})
	}
}

// TestMapErrServiceUnavailable covers the Rancher proxy 503: a cluster whose
// agent is not yet registered returns a 503 ServiceUnavailable that should be
// classified as retryable so callers know to wait and retry, not give up.
func TestMapErrServiceUnavailable(t *testing.T) {
	t.Run("503 maps to GENERIC+retryable+message", func(t *testing.T) {
		err := apierrors.NewServiceUnavailable("ClusterUnavailable 503: ClusterUnavailable 503")
		got := mapErr(err)
		var ce *clierr.Error
		require.ErrorAs(t, got, &ce)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.True(t, ce.Retryable, "503 must be retryable")
		assert.Contains(t, ce.Message, "its agent may not be registered yet; retry once the cluster is active")
		assert.Empty(t, ce.Hint)
	})

	t.Run("403 still maps to RBAC (no regression)", func(t *testing.T) {
		gr := schema.GroupResource{Group: "management.cattle.io", Resource: "nodes"}
		err := apierrors.NewForbidden(gr, "local", errors.New("denied"))
		got := mapErr(err)
		var ce *clierr.Error
		require.ErrorAs(t, got, &ce)
		assert.Equal(t, clierr.CodeRBAC, ce.Code)
		assert.False(t, ce.Retryable, "403 must not be retryable")
	})
}

// TestMapErrConflictRetryable covers the precondition-conflict race: a
// resourceVersion mismatch from a concurrent writer (e.g. a reconciling
// controller) is retryable with plain wording, never the raw Kubernetes
// "the object has been modified" message. A duplicate-name AlreadyExists is
// the other HTTP 409 predicate and stays non-retryable — retrying it without
// changing the name never succeeds.
func TestMapErrConflictRetryable(t *testing.T) {
	gr := schema.GroupResource{Group: "provisioning.cattle.io", Resource: "clusters"}

	t.Run("conflict is retryable with plain wording", func(t *testing.T) {
		err := apierrors.NewConflict(gr, "c-x", errors.New("the object has been modified; please apply your changes to the latest version and try again"))
		got := mapErr(err)
		var ce *clierr.Error
		require.ErrorAs(t, got, &ce)
		assert.Equal(t, clierr.CodeConflict, ce.Code)
		assert.True(t, ce.Retryable, "a resourceVersion precondition conflict is retryable")
		assert.NotContains(t, ce.Message, "the object has been modified", "the raw Kubernetes message must not reach the caller")
	})

	t.Run("already exists is not retryable", func(t *testing.T) {
		err := apierrors.NewAlreadyExists(gr, "c-x")
		got := mapErr(err)
		var ce *clierr.Error
		require.ErrorAs(t, got, &ce)
		assert.Equal(t, clierr.CodeConflict, ce.Code)
		assert.False(t, ce.Retryable, "a duplicate name never succeeds on retry")
	})
}

// TestMapErrDeniedFallback covers exercise bug 5: Rancher's proxy paths
// reject an invalid/insufficient token with an empty-body 403, which
// apierrors passes through as an empty Error() string. mapErr must never
// hand the pipeline an empty message for a denied request.
func TestMapErrDeniedFallback(t *testing.T) {
	emptyForbidden := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: 403,
	}}
	emptyUnauthorized := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Reason: metav1.StatusReasonUnauthorized, Code: 401,
	}}
	whitespaceForbidden := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: 403, Message: "   ",
	}}

	t.Run("empty forbidden message gets a substituted message and hint", func(t *testing.T) {
		got := mapErr(emptyForbidden)
		var ce *clierr.Error
		require.ErrorAs(t, got, &ce)
		assert.Equal(t, clierr.CodeRBAC, ce.Code)
		assert.Equal(t, deniedFallbackMessage(403), ce.Message)
		assert.Equal(t, "rancher auth status", ce.Hint)
	})

	t.Run("empty unauthorized message gets a substituted message and hint naming 401, not 403", func(t *testing.T) {
		got := mapErr(emptyUnauthorized)
		var ce *clierr.Error
		require.ErrorAs(t, got, &ce)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.Equal(t, deniedFallbackMessage(401), ce.Message)
		assert.Contains(t, ce.Message, "(401)")
		assert.NotContains(t, ce.Message, "(403)")
		assert.Equal(t, "rancher auth status", ce.Hint)
	})

	t.Run("whitespace-only forbidden message counts as empty", func(t *testing.T) {
		got := mapErr(whitespaceForbidden)
		var ce *clierr.Error
		require.ErrorAs(t, got, &ce)
		assert.Equal(t, deniedFallbackMessage(403), ce.Message)
	})

	t.Run("a populated forbidden message passes through untouched", func(t *testing.T) {
		gr := schema.GroupResource{Group: "management.cattle.io", Resource: "projects"}
		got := mapErr(apierrors.NewForbidden(gr, "p-x", errors.New("nope")))
		var ce *clierr.Error
		require.ErrorAs(t, got, &ce)
		assert.NotEqual(t, deniedFallbackMessage(403), ce.Message)
		assert.Empty(t, ce.Hint, "an already-populated message must not gain the denied-fallback hint")
	})

	t.Run("a populated unauthorized message passes through untouched", func(t *testing.T) {
		got := mapErr(apierrors.NewUnauthorized("bad token"))
		var ce *clierr.Error
		require.ErrorAs(t, got, &ce)
		assert.Equal(t, "bad token", ce.Message)
		assert.Empty(t, ce.Hint)
	})
}

// TestMapErrAgentDisconnected covers the Rancher proxy's post-registration
// agent drop: the proxy answers with a plain-text 5xx body ("error trying
// to reach service: cluster agent disconnected") that client-go wraps as a
// generic InternalError StatusError with an UnexpectedServerResponse cause.
// Same underlying condition as the pre-registration 503 — classified
// retryable with a matching hint. A genuine k8s-Status InternalError (e.g.
// Rancher's stale-precondition delete, fidelity/probes.go) must NOT match.
func TestMapErrAgentDisconnected(t *testing.T) {
	gr := schema.GroupResource{Group: "", Resource: "nodes"}

	t.Run("post-registration disconnect maps to GENERIC+retryable+message", func(t *testing.T) {
		err := apierrors.NewGenericServerResponse(http.StatusInternalServerError, "get", gr, "",
			"error trying to reach service: cluster agent disconnected", 0, true)
		got := mapErr(err)
		var ce *clierr.Error
		require.ErrorAs(t, got, &ce)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.True(t, ce.Retryable, "disconnected agent must be retryable")
		assert.Contains(t, ce.Message, "retry once the cluster reconnects")
		assert.Empty(t, ce.Hint)
	})

	t.Run("genuine k8s InternalError stays non-retryable", func(t *testing.T) {
		got := mapErr(apierrors.NewInternalError(errors.New("etcdserver: request timed out")))
		var ce *clierr.Error
		require.ErrorAs(t, got, &ce)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.False(t, ce.Retryable)
		assert.Empty(t, ce.Hint)
	})

	t.Run("unexpected 500 without the disconnect text stays retryable with the proxy message", func(t *testing.T) {
		err := apierrors.NewGenericServerResponse(http.StatusInternalServerError, "get", gr, "",
			"upstream connect error", 0, true)
		got := mapErr(err)
		var ce *clierr.Error
		require.ErrorAs(t, got, &ce)
		assert.True(t, ce.Retryable, "unexpected 5xx via proxy must be retryable")
		assert.Contains(t, ce.Message, "often a proxy-path failure such as an unreachable downstream cluster; retrying may succeed")
		assert.Empty(t, ce.Hint)
	})

	t.Run("503 keeps its registration-window classification (no regression)", func(t *testing.T) {
		got := mapErr(apierrors.NewServiceUnavailable("ClusterUnavailable 503"))
		var ce *clierr.Error
		require.ErrorAs(t, got, &ce)
		assert.True(t, ce.Retryable)
		assert.Contains(t, ce.Message, "may not be registered yet")
	})
}

// TestMapErrDeadlineExceeded covers the hung agent-tunnel request: client-go
// hands back a *url.Error wrapping context.DeadlineExceeded with no HTTP
// status at all, which must classify retryable with a hint pointing at the
// likely cause instead of falling through to the uncurated default.
func TestMapErrDeadlineExceeded(t *testing.T) {
	t.Run("wrapped deadline maps to GENERIC+retryable+hint", func(t *testing.T) {
		err := &url.Error{Op: "Get", URL: "https://rancher.example/k8s/clusters/c-m-x/api/v1/nodes", Err: context.DeadlineExceeded}
		got := mapErr(err)
		var ce *clierr.Error
		require.ErrorAs(t, got, &ce)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.True(t, ce.Retryable, "request timeout must be retryable")
		assert.Contains(t, ce.Message, "rancher inspect cluster")
		assert.Empty(t, ce.Hint)
	})

	t.Run("context cancellation is not given the timeout hint", func(t *testing.T) {
		got := mapErr(&url.Error{Op: "Get", URL: "https://rancher.example", Err: context.Canceled})
		var ce *clierr.Error
		require.ErrorAs(t, got, &ce)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.False(t, ce.Retryable)
		assert.Empty(t, ce.Hint)
	})
}
