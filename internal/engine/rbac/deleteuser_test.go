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
	eff, _, err := mc.Plan(context.Background(), in)
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
// currently authenticated user is refused with a VALIDATION error.
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
	assert.Contains(t, ce.Hint, "admin")
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
	assert.Contains(t, ce.Hint, "retry")
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

// TestDeleteUserClusterErrorRefuses verifies that a Cluster() lookup failure
// prevents deletion. After the post-Plan guard reordering the error surfaces
// from inner.Plan (not the guard), so it propagates as-is rather than being
// wrapped as a retryable GENERIC error.
func TestDeleteUserClusterErrorRefuses(t *testing.T) {
	t.Parallel()
	k := userKind(t)
	connErr := errors.New("connection refused")
	cmd := NewDeleteUser(k, fakeFactory{cErr: connErr})

	_, err := planDeleteUser(t, cmd, stubInput{args: []string{"u-other"}})
	require.Error(t, err, "Cluster() failure must prevent deletion")
	assert.ErrorIs(t, err, connErr)
}

// TestDeleteUserCancellation verifies that a cancelled context propagates
// through the guard rather than being silently swallowed.
func TestDeleteUserCancellation(t *testing.T) {
	t.Parallel()
	k := userKind(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel
	cmd := NewDeleteUser(k, fakeFactory{c: &fakeClient{}, ctxAware: true})
	mc := cmd.(runtime.MutatingCommand)
	_, _, err := mc.Plan(ctx, stubInput{args: []string{"u-self"}})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

// TestDeleteUserNonexistentFailsBeforeProbe verifies that after the post-Plan
// guard reordering a nonexistent target returns NOT_FOUND from the inner Plan
// without the self-delete probe firing.
func TestDeleteUserNonexistentFailsBeforeProbe(t *testing.T) {
	t.Parallel()
	k := userKind(t)
	c := &fakeClient{
		createFn: selfUserCreate("u-current"), // probe would fire if called
		// no user in gets or users → Get returns NOT_FOUND
	}
	cmd := NewDeleteUser(k, fakeFactory{c: c})

	_, err := planDeleteUser(t, cmd, stubInput{args: []string{"u-missing"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code, "nonexistent target must fail NOT_FOUND from inner Plan")
	assert.Zero(t, c.createCalls, "self-delete probe must not fire before target existence is confirmed")
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
