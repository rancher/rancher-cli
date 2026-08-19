package provisioning

import (
	"context"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// delRecCall is one recorded Delete invocation on delRecClient.
type delRecCall struct {
	ns, name, rv string
	dryRun       bool
}

// delRecClient is a recording engine.ClusterClient fake for the delete
// sequence: Get is counted (so the local-refusal test can assert it was
// never called) and can fail; Delete records every call (dry-run and real)
// so tests can assert the rv precondition on both.
type delRecClient struct {
	obj       *unstructured.Unstructured
	getErr    error
	getCalls  int
	dryRunErr error
	deletes   []delRecCall
}

func (d *delRecClient) Get(_ context.Context, _ schema.GroupVersionResource, _, _ string) (*unstructured.Unstructured, error) {
	d.getCalls++
	if d.getErr != nil {
		return nil, d.getErr
	}
	return d.obj, nil
}
func (d *delRecClient) List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error) {
	return &unstructured.UnstructuredList{}, nil
}
func (d *delRecClient) Create(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (d *delRecClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (d *delRecClient) Delete(_ context.Context, _ schema.GroupVersionResource, ns, name, rv string, dryRun bool) error {
	d.deletes = append(d.deletes, delRecCall{ns: ns, name: name, rv: rv, dryRun: dryRun})
	if dryRun {
		return d.dryRunErr
	}
	return nil
}
func (d *delRecClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", nil
}

var _ engine.ClusterClient = (*delRecClient)(nil)

func mustProvisioningClusterKind(t *testing.T) kinds.Kind {
	t.Helper()
	k, ok := kinds.Lookup("provisioningcluster")
	require.True(t, ok, "provisioningcluster kind must exist")
	return k
}

func clusterObjWithRV(name, rv string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
	}}
	o.SetResourceVersion(rv)
	return o
}

func planDeleteCluster(t *testing.T, cmd runtime.Command, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "delete provisioning-cluster must be a MutatingCommand")
	return mc.Plan(t.Context(), in)
}

// TestDeleteProvisioningClusterRefusesLocal pins the D-6 guard's core
// property: the literal argument "local" is refused with CodeValidation.
// The guard runs after inner.Plan, so the object must exist for the refusal
// to fire — inner.Plan confirms existence and completes the dry-run before
// the guard checks the argument.
func TestDeleteProvisioningClusterRefusesLocal(t *testing.T) {
	t.Parallel()
	client := &delRecClient{obj: clusterObjWithRV("local", "1")}
	cmd := NewDeleteProvisioningCluster(mustProvisioningClusterKind(t), fakeFactory{c: client})

	_, exec, err := planDeleteCluster(t, cmd, ccInput{a: []string{"local"}})
	require.Error(t, err)
	assert.Nil(t, exec)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "refusing to delete the local cluster", ce.Message)

	assert.Greater(t, client.getCalls, 0, "inner Plan must Get before the guard check")
	assert.NotEmpty(t, client.deletes, "inner Plan's dry-run must have executed")
	assert.True(t, client.deletes[0].dryRun, "inner Plan issues a dry-run delete")
}

// TestDeleteProvisioningClusterCarriesCascadeWarning pins the second D-6
// addition: a normal (non-"local") delete's Effect carries the cascade
// statement in Warnings, never as a diff entry or a gate — Destructive is
// already true via the wrapped generic delete, so no separate flag is
// needed to require --yes.
func TestDeleteProvisioningClusterCarriesCascadeWarning(t *testing.T) {
	t.Parallel()
	client := &delRecClient{obj: clusterObjWithRV("imported-1", "5")}
	cmd := NewDeleteProvisioningCluster(mustProvisioningClusterKind(t), fakeFactory{c: client})

	eff, exec, err := planDeleteCluster(t, cmd, ccInput{a: []string{"imported-1"}})
	require.NoError(t, err)
	require.NotNil(t, exec)

	assert.Equal(t, safety.ActionDelete, eff.Action)
	assert.True(t, eff.Destructive)
	assert.True(t, eff.ConflictDetection)
	assert.Equal(t, "5", eff.ResourceVersion)
	assert.Equal(t, []string{cascadeWarning}, eff.Warnings)

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"deleted": "imported-1"}, res.Data)
}

// TestDeleteProvisioningClusterRVPreconditionMirrorsGeneric pins that the
// wrapped delete's rv-precondition behavior (dry-run preview, then a real
// delete gated on the SAME captured resourceVersion) is unchanged from
// resources.NewDelete's own sequence, and that a dry-run admission
// failure aborts Plan with the mapped error exactly as it does generically.
func TestDeleteProvisioningClusterRVPreconditionMirrorsGeneric(t *testing.T) {
	t.Parallel()
	k := mustProvisioningClusterKind(t)

	client := &delRecClient{obj: clusterObjWithRV("imported-1", "9")}
	_, _, err := planDeleteCluster(t, NewDeleteProvisioningCluster(k, fakeFactory{c: client}), ccInput{a: []string{"imported-1"}})
	require.NoError(t, err)
	require.Len(t, client.deletes, 1, "Plan issues exactly one dry-run delete (admission preview)")
	assert.True(t, client.deletes[0].dryRun)
	assert.Equal(t, "9", client.deletes[0].rv)
	assert.Equal(t, "fleet-default", client.deletes[0].ns, "NSWorkspace defaults the namespace")

	failClient := &delRecClient{obj: clusterObjWithRV("imported-1", "9"), dryRunErr: clierr.New(clierr.CodeRBAC, "forbidden")}
	_, _, err = planDeleteCluster(t, NewDeleteProvisioningCluster(k, fakeFactory{c: failClient}), ccInput{a: []string{"imported-1"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
}

// TestDeleteProvisioningClusterNotFoundSurfacesCleanly pins that a missing
// object's error still surfaces through unchanged (the wrapped Get's error
// is returned as-is).
func TestDeleteProvisioningClusterNotFoundSurfacesCleanly(t *testing.T) {
	t.Parallel()
	client := &delRecClient{getErr: clierr.New(clierr.CodeNotFound, `clusters "missing" not found`)}
	_, _, err := planDeleteCluster(t, NewDeleteProvisioningCluster(mustProvisioningClusterKind(t), fakeFactory{c: client}),
		ccInput{a: []string{"missing"}})
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
}

// TestDeleteProvisioningClusterDescribeMirrorsGeneric pins that the override
// changes ONLY Plan's behavior: its Descriptor (name, args, flags,
// destructive) is byte-identical to what resources.NewDelete(k, ...) would
// have generated for this row, so schema/completion/routing are unaffected.
func TestDeleteProvisioningClusterDescribeMirrorsGeneric(t *testing.T) {
	t.Parallel()
	k := mustProvisioningClusterKind(t)
	generic := resources.NewDelete(k, nil).Describe()
	override := NewDeleteProvisioningCluster(k, nil).Describe()
	assert.Equal(t, generic, override)
	assert.Equal(t, "delete provisioning-cluster", override.Name)
}
