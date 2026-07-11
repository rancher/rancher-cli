package provider

import (
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

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
		assert.Equal(t, deniedFallbackMessage, ce.Message)
		assert.Equal(t, "rancher auth check", ce.Hint)
	})

	t.Run("empty unauthorized message gets a substituted message and hint", func(t *testing.T) {
		got := mapErr(emptyUnauthorized)
		var ce *clierr.Error
		require.ErrorAs(t, got, &ce)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.Equal(t, deniedFallbackMessage, ce.Message)
		assert.Equal(t, "rancher auth check", ce.Hint)
	})

	t.Run("whitespace-only forbidden message counts as empty", func(t *testing.T) {
		got := mapErr(whitespaceForbidden)
		var ce *clierr.Error
		require.ErrorAs(t, got, &ce)
		assert.Equal(t, deniedFallbackMessage, ce.Message)
	})

	t.Run("a populated forbidden message passes through untouched", func(t *testing.T) {
		gr := schema.GroupResource{Group: "management.cattle.io", Resource: "projects"}
		got := mapErr(apierrors.NewForbidden(gr, "p-x", errors.New("nope")))
		var ce *clierr.Error
		require.ErrorAs(t, got, &ce)
		assert.NotEqual(t, deniedFallbackMessage, ce.Message)
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
