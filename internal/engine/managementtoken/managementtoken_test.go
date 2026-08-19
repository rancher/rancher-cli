package managementtoken_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/managementtoken"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func managementTokenKind(t *testing.T) kinds.Kind {
	t.Helper()
	k, ok := kinds.Lookup("managementtoken")
	require.True(t, ok, "managementtoken kind must exist in the table")
	return k
}

func planMutating(t *testing.T, cmd runtime.Command, in runtime.Input) error {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "command must be a MutatingCommand")
	_, _, err := mc.Plan(t.Context(), in)
	return err
}

func requireValidation(t *testing.T, err error) {
	t.Helper()
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce), "want a *clierr.Error, got %v (%T)", err, err)
	assert.Equal(t, clierr.CodeValidation, ce.Code, "want VALIDATION error code")
}

// TestRefusePatchPlanReturnsValidationError verifies that NewRefusePatch
// declines every Plan call with a VALIDATION error before any network call.
func TestRefusePatchPlanReturnsValidationError(t *testing.T) {
	t.Parallel()
	k := managementTokenKind(t)
	cmd := managementtoken.NewRefusePatch(k)
	err := planMutating(t, cmd, stubInput{})
	require.Error(t, err)
	requireValidation(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Contains(t, ce.Message, "patch is not supported")
}

// TestRefusePatchDescribeMatchesGenericPatch verifies that NewRefusePatch's
// Descriptor is identical to the generic patch command's Descriptor (same
// Name, same Args, same Flags) so the override does not change the schema
// surface.
func TestRefusePatchDescribeMatchesGenericPatch(t *testing.T) {
	t.Parallel()
	k := managementTokenKind(t)
	refuse := managementtoken.NewRefusePatch(k)
	d := refuse.Describe()
	assert.Equal(t, "patch management-token", d.Name)
	assert.Equal(t, runtime.Mutating, d.CommandType)
}

// TestDeleteGuardRefusesSessionToken verifies that the delete guard fires when
// the target name matches the active session's token ID and the token is not
// derived (isDerived absent/false).
func TestDeleteGuardRefusesSessionToken(t *testing.T) {
	t.Parallel()
	k := managementTokenKind(t)
	client := &stubClient{obj: tokenObj("my-token-abc")}
	fac := &stubFactory{client: client}
	namer := &stubNamer{id: "my-token-abc"}
	cmd := managementtoken.NewDeleteGuard(k, fac, namer)

	err := planMutating(t, cmd, stubInput{args: []string{"my-token-abc"}})
	require.Error(t, err)
	requireValidation(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Contains(t, ce.Message, "backs the current CLI session")
	assert.NotContains(t, ce.Hint, "auth logout", "hint must not promise logout revokes the session")
	assert.Contains(t, ce.Hint, "RANCHER_CLI_TOKEN")
}

// TestDeleteGuardDerivedTokenPassesThrough verifies that a derived token whose
// name matches the active session ID is NOT refused — Rancher's /v3/tokens
// handler explicitly permits current-derived-token deletion.
func TestDeleteGuardDerivedTokenPassesThrough(t *testing.T) {
	t.Parallel()
	k := managementTokenKind(t)
	client := &stubClient{obj: tokenObjDerived("my-token-abc", true)}
	fac := &stubFactory{client: client}
	namer := &stubNamer{id: "my-token-abc"}
	cmd := managementtoken.NewDeleteGuard(k, fac, namer)

	err := planMutating(t, cmd, stubInput{args: []string{"my-token-abc"}})
	assert.NoError(t, err, "current derived token must pass through the guard")
}

// TestDeleteGuardNonDerivedTokenRefused verifies that a non-derived token with
// a name matching the active session ID is refused with VALIDATION.
func TestDeleteGuardNonDerivedTokenRefused(t *testing.T) {
	t.Parallel()
	k := managementTokenKind(t)
	client := &stubClient{obj: tokenObjDerived("my-token-abc", false)}
	fac := &stubFactory{client: client}
	namer := &stubNamer{id: "my-token-abc"}
	cmd := managementtoken.NewDeleteGuard(k, fac, namer)

	err := planMutating(t, cmd, stubInput{args: []string{"my-token-abc"}})
	require.Error(t, err)
	requireValidation(t, err)
}

// TestDeleteGuardIsDerivedAbsentRefused verifies that when the target token has
// no isDerived field (absent = false), the guard refuses deletion.
func TestDeleteGuardIsDerivedAbsentRefused(t *testing.T) {
	t.Parallel()
	k := managementTokenKind(t)
	// tokenObj produces an object with no isDerived field.
	client := &stubClient{obj: tokenObj("my-token-abc")}
	fac := &stubFactory{client: client}
	namer := &stubNamer{id: "my-token-abc"}
	cmd := managementtoken.NewDeleteGuard(k, fac, namer)

	err := planMutating(t, cmd, stubInput{args: []string{"my-token-abc"}})
	require.Error(t, err)
	requireValidation(t, err)
}

// TestDeleteGuardGetNotFoundPassesThrough verifies that when the GET for the
// target returns NOT_FOUND, the guard passes through to the generic delete
// (which produces its own NOT_FOUND) rather than refusing with VALIDATION.
func TestDeleteGuardGetNotFoundPassesThrough(t *testing.T) {
	t.Parallel()
	k := managementTokenKind(t)
	notFound := clierr.New(clierr.CodeNotFound, "management-token my-token-abc not found")
	client := &stubClient{getErr: notFound}
	fac := &stubFactory{client: client}
	namer := &stubNamer{id: "my-token-abc"}
	cmd := managementtoken.NewDeleteGuard(k, fac, namer)

	err := planMutating(t, cmd, stubInput{args: []string{"my-token-abc"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce), "want a *clierr.Error, got %v", err)
	assert.NotEqual(t, clierr.CodeValidation, ce.Code, "NOT_FOUND must not surface as VALIDATION")
}

// TestDeleteGuardGetErrorPropagates verifies that a GET error other than
// NOT_FOUND is propagated rather than silently swallowed.
func TestDeleteGuardGetErrorPropagates(t *testing.T) {
	t.Parallel()
	k := managementTokenKind(t)
	netErr := errors.New("network error")
	client := &stubClient{getErr: netErr}
	fac := &stubFactory{client: client}
	namer := &stubNamer{id: "my-token-abc"}
	cmd := managementtoken.NewDeleteGuard(k, fac, namer)

	err := planMutating(t, cmd, stubInput{args: []string{"my-token-abc"}})
	require.Error(t, err)
	assert.True(t, errors.Is(err, netErr), "original network error must propagate")
}

// TestDeleteGuardPassesThroughNonSessionToken verifies that a token whose
// name does NOT match the active session ID passes through to the inner
// generic delete (which will proceed with its own sequence).
func TestDeleteGuardPassesThroughNonSessionToken(t *testing.T) {
	t.Parallel()
	k := managementTokenKind(t)
	client := &stubClient{obj: tokenObj("other-token")}
	fac := &stubFactory{client: client}
	namer := &stubNamer{id: "my-token-abc"} // different from the target
	cmd := managementtoken.NewDeleteGuard(k, fac, namer)

	// The inner generic delete runs: it will Get → dry-run Delete → return an
	// Effect. A nil error means the guard passed through.
	err := planMutating(t, cmd, stubInput{args: []string{"other-token"}})
	assert.NoError(t, err, "a non-session token must pass through the guard")
}

// TestDeleteGuardWithNilNamerPassesThrough verifies that a nil
// SessionTokenNamer (zero runDeps) skips the guard entirely.
func TestDeleteGuardWithNilNamerPassesThrough(t *testing.T) {
	t.Parallel()
	k := managementTokenKind(t)
	client := &stubClient{obj: tokenObj("any-token")}
	fac := &stubFactory{client: client}
	cmd := managementtoken.NewDeleteGuard(k, fac, nil)

	err := planMutating(t, cmd, stubInput{args: []string{"any-token"}})
	assert.NoError(t, err, "nil namer must not block deletion")
}

// TestDeleteGuardDescribeMatchesGenericDelete verifies that the guard's
// Descriptor mirrors the generic delete command's Descriptor.
func TestDeleteGuardDescribeMatchesGenericDelete(t *testing.T) {
	t.Parallel()
	k := managementTokenKind(t)
	cmd := managementtoken.NewDeleteGuard(k, &stubFactory{}, nil)
	d := cmd.Describe()
	assert.Equal(t, "delete management-token", d.Name)
	assert.Equal(t, runtime.Mutating, d.CommandType)
}

// TestDeleteGuardDerivedTokenExecUsesGuardFetchRV verifies that the guard's
// name-match path issues exactly one GET and binds the delete's resourceVersion
// precondition to that fetch — not to a second read. If two GETs occurred, a
// concurrent isDerived flip could make the guard decide on a derived object
// while the execution deletes a non-derived (now live) session token.
func TestDeleteGuardDerivedTokenExecUsesGuardFetchRV(t *testing.T) {
	t.Parallel()
	k := managementTokenKind(t)

	// First GET: derived token, rv "999" — what the guard should decide from.
	first := tokenObjDerived("my-token-abc", true)
	first.SetResourceVersion("999")
	// Second GET: isDerived flipped to false, rv "9000" — what a second read would see.
	second := tokenObjDerived("my-token-abc", false)
	second.SetResourceVersion("9000")

	client := &trackingClient{objects: []*unstructured.Unstructured{first, second}}
	fac := &stubFactory{client: client}
	namer := &stubNamer{id: "my-token-abc"}
	cmd := managementtoken.NewDeleteGuard(k, fac, namer)

	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok)
	_, exec, err := mc.Plan(t.Context(), stubInput{args: []string{"my-token-abc"}})
	require.NoError(t, err, "derived token must pass through the guard without error")
	require.NotNil(t, exec)

	assert.Equal(t, 1, client.getCount, "guard must issue exactly one GET, not two")

	_, execErr := exec(t.Context())
	require.NoError(t, execErr)

	// The real delete (non-dry-run) must carry the rv from the guard's single GET.
	var realDel *trackingDelete
	for i := range client.deletes {
		if !client.deletes[i].dryRun {
			realDel = &client.deletes[i]
		}
	}
	require.NotNil(t, realDel, "exec must issue a real delete")
	assert.Equal(t, "999", realDel.rv,
		"delete must use the guard's fetched rv, not a second read's rv")
}

func tokenObj(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3",
		"kind":       "Token",
		"metadata":   map[string]any{"name": name, "resourceVersion": "1"},
	}}
}

func tokenObjDerived(name string, isDerived bool) *unstructured.Unstructured {
	obj := tokenObj(name)
	obj.Object["isDerived"] = isDerived
	return obj
}

type stubInput struct {
	args []string
	s    map[string]string
	b    map[string]bool
}

func (i stubInput) String(k string) string  { return i.s[k] }
func (i stubInput) Bool(k string) bool      { return i.b[k] }
func (i stubInput) Strings(string) []string { return nil }
func (i stubInput) IsSet(string) bool       { return false }
func (i stubInput) Args() []string          { return i.args }

type stubNamer struct{ id string }

func (s *stubNamer) ActiveTokenID() (string, error) { return s.id, nil }

var _ engine.SessionTokenNamer = (*stubNamer)(nil)

type stubClient struct {
	obj    *unstructured.Unstructured
	getErr error
}

func (c *stubClient) Get(_ context.Context, _ schema.GroupVersionResource, _ string, _ string) (*unstructured.Unstructured, error) {
	if c.getErr != nil {
		return nil, c.getErr
	}
	return c.obj, nil
}
func (c *stubClient) List(_ context.Context, _ schema.GroupVersionResource, _ string, _ string) (*unstructured.UnstructuredList, error) {
	return &unstructured.UnstructuredList{}, nil
}
func (c *stubClient) Logs(_ context.Context, _ string, _ string, _ engine.LogOptions) (string, error) {
	return "", nil
}
func (c *stubClient) Create(_ context.Context, _ schema.GroupVersionResource, _ string, obj *unstructured.Unstructured, _ bool) (*unstructured.Unstructured, error) {
	return obj, nil
}
func (c *stubClient) Update(_ context.Context, _ schema.GroupVersionResource, _ string, obj *unstructured.Unstructured, _ bool) (*unstructured.Unstructured, error) {
	return obj, nil
}
func (c *stubClient) Delete(_ context.Context, _ schema.GroupVersionResource, _ string, _ string, _ string, _ bool) error {
	return nil
}

type stubFactory struct {
	client engine.ClusterClient
}

func (f *stubFactory) Cluster(_ context.Context, _ string) (engine.ClusterClient, error) {
	if f.client == nil {
		return &stubClient{obj: &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "x", "resourceVersion": "1"},
		}}}, nil
	}
	return f.client, nil
}
func (f *stubFactory) ContextCluster() (string, error)                            { return "", nil }
func (f *stubFactory) ContextProject() (string, error)                            { return "", nil }
func (f *stubFactory) ResolveCluster(_ context.Context, s string) (string, error) { return s, nil }

var _ engine.ClusterClientFactory = (*stubFactory)(nil)

type trackingDelete struct {
	rv     string
	dryRun bool
}

// trackingClient serves GET objects in sequence (last one repeated on overflow)
// and records every Delete call. Used to assert one-GET + correct-rv invariants.
type trackingClient struct {
	objects  []*unstructured.Unstructured
	getCount int
	deletes  []trackingDelete
}

func (c *trackingClient) Get(_ context.Context, _ schema.GroupVersionResource, _ string, _ string) (*unstructured.Unstructured, error) {
	idx := c.getCount
	if idx >= len(c.objects) {
		idx = len(c.objects) - 1
	}
	c.getCount++
	return c.objects[idx], nil
}
func (c *trackingClient) List(_ context.Context, _ schema.GroupVersionResource, _ string, _ string) (*unstructured.UnstructuredList, error) {
	return &unstructured.UnstructuredList{}, nil
}
func (c *trackingClient) Logs(_ context.Context, _ string, _ string, _ engine.LogOptions) (string, error) {
	return "", nil
}
func (c *trackingClient) Create(_ context.Context, _ schema.GroupVersionResource, _ string, obj *unstructured.Unstructured, _ bool) (*unstructured.Unstructured, error) {
	return obj, nil
}
func (c *trackingClient) Update(_ context.Context, _ schema.GroupVersionResource, _ string, obj *unstructured.Unstructured, _ bool) (*unstructured.Unstructured, error) {
	return obj, nil
}
func (c *trackingClient) Delete(_ context.Context, _ schema.GroupVersionResource, _ string, _ string, rv string, dryRun bool) error {
	c.deletes = append(c.deletes, trackingDelete{rv: rv, dryRun: dryRun})
	return nil
}

var _ engine.ClusterClient = (*trackingClient)(nil)
