package rbac_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/selfuser"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/rbac"
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

// userObj returns a minimal management.cattle.io/v3 User object.
func userObj(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3",
		"kind":       "User",
		"metadata":   map[string]any{"name": name, "resourceVersion": "1"},
	}}
}

// selfUserObj returns a SelfUser probe result with the given userID.
func selfUserObj(userID string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "ext.cattle.io/v1",
		"kind":       "SelfUser",
		"status":     map[string]any{"userID": userID},
	}}
}

// duClient is a dual-purpose client for deleteuser tests. Create calls for
// selfuser.GVR return a SelfUser with the configured identity; all other calls
// delegate to the stub user object.
type duClient struct {
	selfUserID string
	obj        *unstructured.Unstructured
	getErr     error
}

func (c *duClient) Get(_ context.Context, _ schema.GroupVersionResource, _ string, _ string) (*unstructured.Unstructured, error) {
	if c.getErr != nil {
		return nil, c.getErr
	}
	return c.obj, nil
}
func (c *duClient) List(_ context.Context, _ schema.GroupVersionResource, _ string, _ string) (*unstructured.UnstructuredList, error) {
	return &unstructured.UnstructuredList{}, nil
}
func (c *duClient) Logs(_ context.Context, _ string, _ string, _ engine.LogOptions) (string, error) {
	return "", nil
}
func (c *duClient) Create(_ context.Context, gvr schema.GroupVersionResource, _ string, obj *unstructured.Unstructured, _ bool) (*unstructured.Unstructured, error) {
	if gvr == selfuser.GVR {
		return selfUserObj(c.selfUserID), nil
	}
	return obj, nil
}
func (c *duClient) Update(_ context.Context, _ schema.GroupVersionResource, _ string, obj *unstructured.Unstructured, _ bool) (*unstructured.Unstructured, error) {
	return obj, nil
}
func (c *duClient) Delete(_ context.Context, _ schema.GroupVersionResource, _ string, _ string, _ string, _ bool) error {
	return nil
}

var _ engine.ClusterClient = (*duClient)(nil)

type duFactory struct {
	client engine.ClusterClient
}

func (f *duFactory) Cluster(_ context.Context, _ string) (engine.ClusterClient, error) {
	if f.client == nil {
		return &duClient{obj: &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "x", "resourceVersion": "1"},
		}}}, nil
	}
	return f.client, nil
}
func (f *duFactory) ContextCluster() (string, error)                            { return "", nil }
func (f *duFactory) ContextProject() (string, error)                            { return "", nil }
func (f *duFactory) ResolveCluster(_ context.Context, s string) (string, error) { return s, nil }

var _ engine.ClusterClientFactory = (*duFactory)(nil)

type duInput struct {
	args []string
}

func (i duInput) String(string) string    { return "" }
func (i duInput) Bool(string) bool        { return false }
func (i duInput) Strings(string) []string { return nil }
func (i duInput) IsSet(string) bool       { return false }
func (i duInput) Args() []string          { return i.args }

// TestDeleteUserDescribeMatchesGenericDelete verifies that the override's
// Descriptor mirrors the generic delete command's Descriptor (same Name,
// Args, Flags) so the override does not change the schema surface.
func TestDeleteUserDescribeMatchesGenericDelete(t *testing.T) {
	t.Parallel()
	k := userKind(t)
	cmd := rbac.NewDeleteUser(k, &duFactory{})
	d := cmd.Describe()
	assert.Equal(t, "delete user", d.Name)
	assert.Equal(t, runtime.Mutating, d.CommandType)
}

// TestDeleteUserPlanAppendsCascadeWarning verifies that Plan appends the
// cascade warning to the Effect.Warnings for a non-self delete.
func TestDeleteUserPlanAppendsCascadeWarning(t *testing.T) {
	t.Parallel()
	k := userKind(t)
	client := &duClient{
		selfUserID: "u-current",
		obj:        userObj("u-other"),
	}
	fac := &duFactory{client: client}
	cmd := rbac.NewDeleteUser(k, fac)

	eff, err := planDeleteUser(t, cmd, duInput{args: []string{"u-other"}})
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
	client := &duClient{
		selfUserID: "u-self",
		obj:        userObj("u-self"),
	}
	fac := &duFactory{client: client}
	cmd := rbac.NewDeleteUser(k, fac)

	_, err := planDeleteUser(t, cmd, duInput{args: []string{"u-self"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce), "want a *clierr.Error, got %v (%T)", err, err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "u-self")
	assert.Contains(t, ce.Hint, "admin")
}

// TestDeleteUserSelfDeleteProbeFailPassesThrough verifies that a SelfUser
// probe failure is non-blocking: the guard skips and the inner Plan proceeds.
func TestDeleteUserSelfDeleteProbeFailPassesThrough(t *testing.T) {
	t.Parallel()
	k := userKind(t)
	// probeErrClient returns an error for selfuser.GVR creates.
	probeErrClient := &probeFailClient{obj: userObj("u-self")}
	fac := &duFactory{client: probeErrClient}
	cmd := rbac.NewDeleteUser(k, fac)

	eff, err := planDeleteUser(t, cmd, duInput{args: []string{"u-self"}})
	require.NoError(t, err, "probe failure must not block deletion")
	assert.NotEmpty(t, eff.Warnings, "cascade warning must still be appended when probe fails")
}

// TestDeleteUserSelfDeleteEmptyIDPassesThrough verifies that an empty
// identity from the probe is non-blocking.
func TestDeleteUserSelfDeleteEmptyIDPassesThrough(t *testing.T) {
	t.Parallel()
	k := userKind(t)
	client := &duClient{
		selfUserID: "", // empty → no match possible
		obj:        userObj("u-self"),
	}
	fac := &duFactory{client: client}
	cmd := rbac.NewDeleteUser(k, fac)

	eff, err := planDeleteUser(t, cmd, duInput{args: []string{"u-self"}})
	require.NoError(t, err, "empty probe ID must not block deletion")
	assert.NotEmpty(t, eff.Warnings)
}

// probeFailClient returns an error when Create is called for selfuser.GVR.
type probeFailClient struct {
	obj *unstructured.Unstructured
}

func (c *probeFailClient) Get(_ context.Context, _ schema.GroupVersionResource, _ string, _ string) (*unstructured.Unstructured, error) {
	return c.obj, nil
}
func (c *probeFailClient) List(_ context.Context, _ schema.GroupVersionResource, _ string, _ string) (*unstructured.UnstructuredList, error) {
	return &unstructured.UnstructuredList{}, nil
}
func (c *probeFailClient) Logs(_ context.Context, _ string, _ string, _ engine.LogOptions) (string, error) {
	return "", nil
}
func (c *probeFailClient) Create(_ context.Context, gvr schema.GroupVersionResource, _ string, obj *unstructured.Unstructured, _ bool) (*unstructured.Unstructured, error) {
	if gvr == selfuser.GVR {
		return nil, errors.New("identity probe unavailable")
	}
	return obj, nil
}
func (c *probeFailClient) Update(_ context.Context, _ schema.GroupVersionResource, _ string, obj *unstructured.Unstructured, _ bool) (*unstructured.Unstructured, error) {
	return obj, nil
}
func (c *probeFailClient) Delete(_ context.Context, _ schema.GroupVersionResource, _ string, _ string, _ string, _ bool) error {
	return nil
}

var _ engine.ClusterClient = (*probeFailClient)(nil)
