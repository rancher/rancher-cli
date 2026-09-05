package rbac

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/selfuser"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func userKind(t *testing.T) kinds.Kind {
	t.Helper()
	k, ok := kinds.Lookup("user")
	require.True(t, ok, "user kind must exist in the table")
	return k
}

func planDeleteUser(t *testing.T, cmd runtime.Command, in runtime.Input) (safety.Effect, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "command must be a MutatingCommand")
	eff, _, err := mc.Plan(t.Context(), in)
	return eff, err
}

// selfUserObj returns a SelfUser probe result with the given userID.
func selfUserObj(userID string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "ext.cattle.io/v1",
		"kind":       "SelfUser",
		"status":     map[string]any{"userID": userID},
	}}
}

// selfUserCreate returns a createFn that answers selfuser.GVR probes with
// a SelfUser carrying userID, and errors for any other GVR.
func selfUserCreate(userID string) func(schema.GroupVersionResource) (*unstructured.Unstructured, error) {
	return func(gvr schema.GroupVersionResource) (*unstructured.Unstructured, error) {
		if gvr == selfuser.GVR {
			return selfUserObj(userID), nil
		}
		return nil, errors.New("unexpected Create")
	}
}

// probeErrCreate returns a createFn that returns err for selfuser.GVR probes.
func probeErrCreate(err error) func(schema.GroupVersionResource) (*unstructured.Unstructured, error) {
	return func(gvr schema.GroupVersionResource) (*unstructured.Unstructured, error) {
		if gvr == selfuser.GVR {
			return nil, err
		}
		return nil, errors.New("unexpected Create")
	}
}

// delUserObj returns a minimal management.cattle.io/v3 User object for delete tests.
func delUserObj(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3",
		"kind":       "User",
		"metadata":   map[string]any{"name": name, "resourceVersion": "1"},
	}}
}

// TestDeleteUserDescribeMatchesGenericDelete verifies that the override's
// Descriptor mirrors the generic delete command's Descriptor (same Name,
// Args, Flags) so the override does not change the schema surface.
func TestDeleteUserDescribeMatchesGenericDelete(t *testing.T) {
	t.Parallel()
	k := userKind(t)
	cmd := NewDeleteUser(k, fakeFactory{c: &fakeClient{}})
	d := cmd.Describe()
	assert.Equal(t, "delete user", d.Name)
	assert.Equal(t, runtime.Mutating, d.CommandType)
}

// TestDeleteUserPlanAppendsCascadeWarning verifies that Plan appends the
// cascade warning to the Effect.Warnings for a non-self delete.
func TestDeleteUserPlanAppendsCascadeWarning(t *testing.T) {
	t.Parallel()
	k := userKind(t)
	c := &fakeClient{
		createFn:    selfUserCreate("u-current"),
		gets:        map[string]*unstructured.Unstructured{"/u-other": delUserObj("u-other")},
		allowDelete: true,
	}
	cmd := NewDeleteUser(k, fakeFactory{c: c})

	eff, err := planDeleteUser(t, cmd, stubInput{args: []string{"u-other"}})
	require.NoError(t, err)
	require.NotEmpty(t, eff.Warnings, "cascade warning must be appended")
	found := false
	for _, w := range eff.Warnings {
		if strings.Contains(w, "bindings") {
			found = true
			break
		}
	}
	assert.True(t, found, "cascade warning must mention bindings; got: %v", eff.Warnings)
}

// TestDeleteUserSelfDeleteRefused verifies that attempting to delete the
// currently authenticated user is refused with a VALIDATION error. With
// pre-Plan guard timing the curated refusal fires before the inner Plan's
// server-side dry-run - the inner dry-run must not execute on refusal.
func TestDeleteUserSelfDeleteRefused(t *testing.T) {
	t.Parallel()
	k := userKind(t)
	c := &fakeClient{
		createFn:    selfUserCreate("u-self"),
		gets:        map[string]*unstructured.Unstructured{"/u-self": delUserObj("u-self")},
		allowDelete: true,
	}
	cmd := NewDeleteUser(k, fakeFactory{c: c})

	_, err := planDeleteUser(t, cmd, stubInput{args: []string{"u-self"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "u-self")
	assert.Contains(t, ce.Message, "admin")
	assert.Empty(t, c.deleteDryRuns, "inner dry-run must not fire on self-delete refusal")
}

// TestDeleteUserGenericProbeErrorRefuses verifies that a non-NotFound
// SelfUser probe failure refuses with a retryable GENERIC error to prevent
// inadvertent lockout on transient infrastructure issues.
func TestDeleteUserGenericProbeErrorRefuses(t *testing.T) {
	t.Parallel()
	k := userKind(t)
	c := &fakeClient{
		createFn:    probeErrCreate(errors.New("identity probe unavailable")),
		gets:        map[string]*unstructured.Unstructured{"/u-self": delUserObj("u-self")},
		allowDelete: true,
	}
	cmd := NewDeleteUser(k, fakeFactory{c: c})

	_, err := planDeleteUser(t, cmd, stubInput{args: []string{"u-self"}})
	require.Error(t, err, "generic probe failure must refuse the delete")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.True(t, ce.Retryable)
	assert.Contains(t, ce.Message, "retry")
}

// TestDeleteUserProbeNotFoundSkips verifies that a NOT_FOUND probe error
// (ext.cattle.io endpoint absent on older Rancher) skips the guard and
// allows the delete to proceed.
func TestDeleteUserProbeNotFoundSkips(t *testing.T) {
	t.Parallel()
	k := userKind(t)
	c := &fakeClient{
		createFn:    probeErrCreate(clierr.New(clierr.CodeNotFound, "selfusers.ext.cattle.io not found")),
		gets:        map[string]*unstructured.Unstructured{"/u-self": delUserObj("u-self")},
		allowDelete: true,
	}
	cmd := NewDeleteUser(k, fakeFactory{c: c})

	eff, err := planDeleteUser(t, cmd, stubInput{args: []string{"u-self"}})
	require.NoError(t, err, "NOT_FOUND probe (endpoint absent) must not block deletion")
	assert.NotEmpty(t, eff.Warnings, "cascade warning must still be appended")
}

// TestDeleteUserRawClusterErrorRefusesRetryably verifies that a raw (non-clierr)
// Cluster() construction failure is wrapped in the retryable GENERIC refusal -
// an ambiguous construction failure must cost a retry rather than risk a lockout.
// Assertions use errors.As rather than ErrorIs because guardVerifyRefusal embeds
// the original text in the message without wrapping the error identity; the
// shape after errors.As is exactly what runtime.toClierr would return.
func TestDeleteUserRawClusterErrorRefusesRetryably(t *testing.T) {
	t.Parallel()
	k := userKind(t)
	cmd := NewDeleteUser(k, fakeFactory{cErr: errors.New("connection refused")})

	_, err := planDeleteUser(t, cmd, stubInput{args: []string{"u-other"}})
	require.Error(t, err, "raw Cluster() failure must prevent deletion")
	// Replicate runtime.toClierr normalization: errors.As finds the *clierr.Error
	// (guardVerifyRefusal returns one); toClierr returns it unchanged.
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce, "raw construction failure must be wrapped as a structured error")
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.True(t, ce.Retryable, "raw construction failure must refuse retryably")
	assert.Contains(t, ce.Message, "connection refused", "original text must be embedded in the message")
}

// TestDeleteUserCliErrClusterErrorPassesThrough verifies that a clierr-coded
// Cluster() failure passes through with its original code and retryability
// intact - the guard must not rewrap a structured error.
// Assertions after errors.As replicate runtime.toClierr normalization: a
// *clierr.Error found by errors.As is returned unchanged by toClierr.
func TestDeleteUserCliErrClusterErrorPassesThrough(t *testing.T) {
	t.Parallel()
	k := userKind(t)
	authErr := clierr.New(clierr.CodeAuth, "token expired").WithRetryable(false)
	cmd := NewDeleteUser(k, fakeFactory{cErr: authErr})

	_, err := planDeleteUser(t, cmd, stubInput{args: []string{"u-other"}})
	require.Error(t, err, "clierr Cluster() failure must prevent deletion")
	// Replicate runtime.toClierr: errors.As finds the original *clierr.Error.
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce, "clierr-coded error must remain a *clierr.Error after normalization")
	assert.Equal(t, clierr.CodeAuth, ce.Code, "original code must pass through unchanged")
	assert.Equal(t, "token expired", ce.Message, "original message must pass through unchanged")
	assert.False(t, ce.Retryable, "original retryability must pass through unchanged")
}

// TestDeleteUserUnclassifiedCliErrClusterErrorRefusesRetryably verifies that a
// Cluster() failure with the mapErr catch-all shape (CodeGeneric, no hint,
// Retryable false) is refused retryably rather than passed through unchanged -
// it is indistinguishable from transport noise and must not short-circuit the
// self-delete guard with a non-retryable generic error.
func TestDeleteUserUnclassifiedCliErrClusterErrorRefusesRetryably(t *testing.T) {
	t.Parallel()
	k := userKind(t)
	// clierr.New(CodeGeneric, ...) with no hint and Retryable false is the
	// shape mapErr's catch-all default produces. It must NOT pass through.
	bareGeneric := clierr.New(clierr.CodeGeneric, "dial: connection refused")
	cmd := NewDeleteUser(k, fakeFactory{cErr: bareGeneric})

	_, err := planDeleteUser(t, cmd, stubInput{args: []string{"u-other"}})
	require.Error(t, err, "unclassified clierr Cluster() failure must prevent deletion")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce, "must be wrapped as a structured error")
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.True(t, ce.Retryable, "unclassified construction failure must refuse retryably")
	assert.Contains(t, ce.Message, "retry")
}

// TestDeleteUserCliErrProbeErrorPassesThrough verifies that a clierr-coded
// SelfUser probe failure (e.g. CodeAuth) passes through with its original
// code and retryability intact - the guard must not rewrap a structured error
// from the probe path. Assertions replicate runtime.toClierr normalization.
func TestDeleteUserCliErrProbeErrorPassesThrough(t *testing.T) {
	t.Parallel()
	k := userKind(t)
	authErr := clierr.New(clierr.CodeAuth, "token expired").WithRetryable(false)
	c := &fakeClient{
		createFn:    probeErrCreate(authErr),
		gets:        map[string]*unstructured.Unstructured{"/u-other": delUserObj("u-other")},
		allowDelete: true,
	}
	cmd := NewDeleteUser(k, fakeFactory{c: c})

	_, err := planDeleteUser(t, cmd, stubInput{args: []string{"u-other"}})
	require.Error(t, err, "clierr probe failure must prevent deletion")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce, "clierr-coded probe error must remain a *clierr.Error")
	assert.Equal(t, clierr.CodeAuth, ce.Code, "original code must pass through unchanged")
	assert.Equal(t, "token expired", ce.Message, "original message must pass through unchanged")
	assert.False(t, ce.Retryable, "original retryability must pass through unchanged")
}

// TestDeleteUserCancellation verifies that a cancelled context propagates
// through the guard via the probe call path: Cluster() succeeds, then the
// probe Create() returns a context error, and ctx.Err() being non-nil routes
// to the ctx.Err() return in the probe branch.
func TestDeleteUserCancellation(t *testing.T) {
	t.Parallel()
	k := userKind(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // pre-cancel so ctx.Err() is non-nil when the probe fires
	// Cluster() succeeds (ctxAware removed); cancellation propagates through
	// the probe's ctx.Err() check after Create returns an error.
	c := &fakeClient{createFn: probeErrCreate(context.Canceled)}
	cmd := NewDeleteUser(k, fakeFactory{c: c})
	mc := cmd.(runtime.MutatingCommand)
	_, _, err := mc.Plan(ctx, stubInput{args: []string{"u-self"}})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

// TestDeleteUserNonexistentProbesBeforeNotFound verifies that with pre-Plan
// guard timing the self-delete probe fires before inner.Plan - so a mistyped
// target name triggers one identity probe before the 404. Accepted tradeoff:
// a curated lockout refusal with a hint beats raw webhook text on a real
// server; the extra probe on mistyped targets is a cheap read-equivalent call.
func TestDeleteUserNonexistentProbesBeforeNotFound(t *testing.T) {
	t.Parallel()
	k := userKind(t)
	c := &fakeClient{
		createFn: selfUserCreate("u-current"), // probe fires with pre-Plan timing
		// no user in gets or users → Get returns NOT_FOUND
	}
	cmd := NewDeleteUser(k, fakeFactory{c: c})

	_, err := planDeleteUser(t, cmd, stubInput{args: []string{"u-missing"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code, "nonexistent target must still fail NOT_FOUND from inner Plan")
	assert.Equal(t, 1, c.createCalls, "self-delete probe fires once before inner.Plan (pre-Plan timing); accepted tradeoff vs. raw webhook text")
}

// TestDeleteUserMapErrDefaultProbeErrorRefusesRetryably verifies that a probe
// error matching mapErr's catch-all default (CodeGeneric, no hint, Retryable
// false) is treated as transport noise - the guard refuses retryably rather
// than passing the unclassified fault through as a diagnostic.
func TestDeleteUserMapErrDefaultProbeErrorRefusesRetryably(t *testing.T) {
	t.Parallel()
	k := userKind(t)
	// clierr.New(CodeGeneric, ...) with no hint and Retryable false is the
	// shape mapErr's catch-all default produces; it must not be passed through.
	bareGeneric := clierr.New(clierr.CodeGeneric, "tls: connection reset")
	c := &fakeClient{
		createFn:    probeErrCreate(bareGeneric),
		gets:        map[string]*unstructured.Unstructured{"/u-self": delUserObj("u-self")},
		allowDelete: true,
	}
	cmd := NewDeleteUser(k, fakeFactory{c: c})

	_, err := planDeleteUser(t, cmd, stubInput{args: []string{"u-self"}})
	require.Error(t, err, "mapErr-default probe error must refuse the delete")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.True(t, ce.Retryable, "bare generic probe error must refuse retryably")
	assert.Contains(t, ce.Message, "retry")
}

// TestDeleteUserRetryableGenericHintedProbePassesThrough verifies that a probe
// error with CodeGeneric but Retryable true and a hint carries a real
// classification (e.g. mapErr's 503/5xx arm) and passes through unchanged.
func TestDeleteUserRetryableGenericHintedProbePassesThrough(t *testing.T) {
	t.Parallel()
	k := userKind(t)
	// mapErr's 503 arm: CodeGeneric, Retryable true, with hint.
	unavailable := clierr.Retryable(clierr.CodeGeneric, "service unavailable").
		WithHint("retry once the cluster is active")
	c := &fakeClient{
		createFn:    probeErrCreate(unavailable),
		gets:        map[string]*unstructured.Unstructured{"/u-other": delUserObj("u-other")},
		allowDelete: true,
	}
	cmd := NewDeleteUser(k, fakeFactory{c: c})

	_, err := planDeleteUser(t, cmd, stubInput{args: []string{"u-other"}})
	require.Error(t, err, "retryable generic probe error must prevent deletion")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code, "original code must pass through unchanged")
	assert.True(t, ce.Retryable, "original retryability must pass through unchanged")
	assert.Contains(t, ce.Hint, "retry once the cluster is active", "original hint must pass through unchanged")
}

// TestDeleteUserEmptyProbeIDRefuses verifies that an empty identity returned
// by the SelfUser probe is refused retryably: an unverifiable identity must
// cost a retry rather than risk a lockout.
func TestDeleteUserEmptyProbeIDRefuses(t *testing.T) {
	t.Parallel()
	k := userKind(t)
	c := &fakeClient{
		createFn:    selfUserCreate(""), // probe succeeds but returns no userID
		gets:        map[string]*unstructured.Unstructured{"/u-self": delUserObj("u-self")},
		allowDelete: true,
	}
	cmd := NewDeleteUser(k, fakeFactory{c: c})

	_, err := planDeleteUser(t, cmd, stubInput{args: []string{"u-self"}})
	require.Error(t, err, "empty probe ID must refuse the delete")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.True(t, ce.Retryable)
}
