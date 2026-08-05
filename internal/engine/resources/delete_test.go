package resources

import (
	"context"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type delCall struct {
	ns, name, rv string
	dryRun       bool
}

// delClient records Delete calls and serves a canned object (or error) from Get,
// with an optional dry-run failure and an optional exec-phase (real-delete) failure.
type delClient struct {
	obj       *unstructured.Unstructured
	getErr    error
	dryRunErr error
	execErr   error // returned only for the non-dry-run real delete
	deletes   []delCall
}

func (d *delClient) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	if d.getErr != nil {
		return nil, d.getErr
	}
	return d.obj, nil
}
func (d *delClient) List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error) {
	return &unstructured.UnstructuredList{}, nil
}
func (d *delClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", nil
}
func (d *delClient) Create(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (d *delClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (d *delClient) Delete(_ context.Context, _ schema.GroupVersionResource, ns, name, rv string, dryRun bool) error {
	d.deletes = append(d.deletes, delCall{ns: ns, name: name, rv: rv, dryRun: dryRun})
	if dryRun {
		return d.dryRunErr
	}
	return d.execErr
}

func objWithRV(name, rv string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
	}}
	o.SetResourceVersion(rv)
	return o
}

func planDelete(t *testing.T, cmd runtime.Command, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "delete must be a MutatingCommand")
	return mc.Plan(context.Background(), in)
}

func TestDeletePlanCapturesResourceVersionAndExecUsesIt(t *testing.T) {
	t.Parallel()
	client := &delClient{obj: objWithRV("web", "42")}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	cmd := NewDelete(mustKind(t, "pod"), fac)

	eff, exec, err := planDelete(t, cmd, input{args: []string{"web"}, s: map[string]string{"cluster": "prod", "namespace": "team-a"}})
	require.NoError(t, err)
	require.NotNil(t, exec)

	assert.Equal(t, safety.ActionDelete, eff.Action)
	assert.True(t, eff.Destructive)
	assert.True(t, eff.ConflictDetection)
	assert.False(t, eff.Reversible)
	assert.Equal(t, "42", eff.ResourceVersion)
	assert.Equal(t, "c-abc", eff.Cluster)
	assert.Equal(t, "pods/web", eff.Target)

	res, err := exec(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "PodDelete", res.Kind)

	last := client.deletes[len(client.deletes)-1]
	assert.Equal(t, delCall{ns: "team-a", name: "web", rv: "42", dryRun: false}, last,
		"the executor deletes with the captured RV precondition, for real")
}

// TestDeleteMultiWordKindUsesK8sKindCasing is the exercise bug 7 repro for
// the delete path: the envelope kind must echo k.K8sKind's casing
// ("StatefulSet"), not a naive first-rune capitalization ("Statefulset").
func TestDeleteMultiWordKindUsesK8sKindCasing(t *testing.T) {
	t.Parallel()
	client := &delClient{obj: objWithRV("web", "1")}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	cmd := NewDelete(mustKind(t, "statefulset"), fac)

	_, exec, err := planDelete(t, cmd, input{args: []string{"web"}, s: map[string]string{"cluster": "prod", "namespace": "team-a"}})
	require.NoError(t, err)

	res, err := exec(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "StatefulSetDelete", res.Kind)
}

func TestDeletePlanDryRunsDeletion(t *testing.T) {
	t.Parallel()
	client := &delClient{obj: objWithRV("web", "7")}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	_, _, err := planDelete(t, NewDelete(mustKind(t, "pod"), fac),
		input{args: []string{"web"}, s: map[string]string{"cluster": "prod", "namespace": "team-a"}})
	require.NoError(t, err)
	require.Len(t, client.deletes, 1, "Plan issues one dry-run delete (admission preview)")
	assert.True(t, client.deletes[0].dryRun)
	assert.Equal(t, "7", client.deletes[0].rv)

	// A dry-run failure aborts Plan with the mapped error.
	failClient := &delClient{obj: objWithRV("web", "7"), dryRunErr: clierr.New(clierr.CodeRBAC, "forbidden")}
	_, _, err = planDelete(t, NewDelete(mustKind(t, "pod"), &recFactory{client: failClient, resolve: map[string]string{"prod": "c-abc"}}),
		input{args: []string{"web"}, s: map[string]string{"cluster": "prod", "namespace": "team-a"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
}

func TestDeleteNotFoundSurfacesCleanly(t *testing.T) {
	t.Parallel()
	client := &delClient{getErr: clierr.New(clierr.CodeNotFound, `pods "web" not found`)}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	_, _, err := planDelete(t, NewDelete(mustKind(t, "pod"), fac),
		input{args: []string{"web"}, s: map[string]string{"cluster": "prod", "namespace": "team-a"}})
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
}

// TestDeleteNotFoundCurated pins the curated NOT_FOUND shape: the dynamic client
// now produces "<kind> not found: <name>"; the fake simulates that.
func TestDeleteNotFoundCurated(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "pod")
	client := &delClient{getErr: kinds.NotFoundCurated(k, "web")}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	_, _, err := planDelete(t, NewDelete(k, fac),
		input{args: []string{"web"}, s: map[string]string{"cluster": "prod", "namespace": "team-a"}})
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "pod not found: web", ce.Message)
	assert.Equal(t, "list with: rancher get pods", ce.Hint)
}

// TestDeleteNonNotFoundPassesThrough pins that a non-NOT_FOUND error from the
// client (e.g. RBAC) reaches the caller unchanged without re-wrapping.
func TestDeleteNonNotFoundPassesThrough(t *testing.T) {
	t.Parallel()
	client := &delClient{getErr: clierr.New(clierr.CodeRBAC, "forbidden")}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	_, _, err := planDelete(t, NewDelete(mustKind(t, "pod"), fac),
		input{args: []string{"web"}, s: map[string]string{"cluster": "prod", "namespace": "team-a"}})
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, "forbidden", ce.Message)
}

// TestDeleteDryRunNotFoundCurated pins the TOCTOU path: when Get succeeds but
// the dry-run delete returns a NOT_FOUND (deleted between Get and dry-run),
// the dynamic client curates it; the fake simulates that pre-curated shape.
func TestDeleteDryRunNotFoundCurated(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "pod")
	client := &delClient{
		obj:       objWithRV("web", "7"),
		dryRunErr: kinds.NotFoundCurated(k, "web"),
	}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	_, _, err := planDelete(t, NewDelete(k, fac),
		input{args: []string{"web"}, s: map[string]string{"cluster": "prod", "namespace": "team-a"}})
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "pod not found: web", ce.Message)
	assert.Equal(t, "list with: rancher get pods", ce.Hint)
}

// TestDeleteDryRunNotFoundWithHintPassesThrough pins that an already-curated
// dry-run NOT_FOUND (one that already carries a hint) passes through verbatim.
func TestDeleteDryRunNotFoundWithHintPassesThrough(t *testing.T) {
	t.Parallel()
	alreadyCurated := clierr.New(clierr.CodeNotFound,
		"this Rancher does not serve ext.cattle.io (requires Rancher >= 2.14, the supported minimum)").
		WithHint("token management: auth list-tokens; kubeconfig: the default exec-credential mode")
	client := &delClient{
		obj:       objWithRV("t-xxx", "1"),
		dryRunErr: alreadyCurated,
	}
	fac := &recFactory{client: client}
	_, _, err := planDelete(t, NewDelete(mustKind(t, "token"), fac),
		input{args: []string{"t-xxx"}})
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Contains(t, ce.Message, "ext.cattle.io", "already-curated message must pass through")
	assert.Contains(t, ce.Hint, "auth list-tokens", "already-curated hint must pass through")
}

func TestDeleteDownstreamNamespacedRequiresNamespace(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "pod")

	// The Descriptor marks --namespace Required so the pipeline rejects pre-Plan.
	var nsFlag runtime.ArgSpec
	for _, f := range NewDelete(k, nil).Describe().Flags {
		if f.Name == "namespace" {
			nsFlag = f
		}
	}
	assert.True(t, nsFlag.Required, "--namespace must be Required for a downstream namespaced delete")

	// Route independently rejects an empty --namespace (delete is always named).
	cmd := NewDelete(k, &recFactory{client: &delClient{obj: objWithRV("web", "1")}, resolve: map[string]string{"prod": "c-abc"}})
	_, _, err := planDelete(t, cmd, input{args: []string{"web"}, s: map[string]string{"cluster": "prod"}})
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestDeleteManagementNamespacedRequiresCluster(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "project")

	// The Descriptor marks --cluster Required so the pipeline rejects before Plan.
	var clusterFlag runtime.ArgSpec
	for _, f := range NewDelete(k, nil).Describe().Flags {
		if f.Name == "cluster" {
			clusterFlag = f
		}
	}
	assert.True(t, clusterFlag.Required, "--cluster must be Required for a management namespaced delete")

	// Route independently rejects an empty --cluster (belt and suspenders): a
	// bare `delete project web --apply --yes` must never hit the local cluster.
	cmd := NewDelete(k, &recFactory{client: &delClient{obj: objWithRV("p1", "1")}})
	_, _, err := planDelete(t, cmd, input{args: []string{"p1"}})
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeValidation, ce.Code)

	// The explicit --cluster path works: client is the local proxy, namespace is
	// the resolved id, and the Effect targets the downstream cluster.
	okClient := &delClient{obj: objWithRV("p1", "9")}
	okFac := &recFactory{client: okClient, resolve: map[string]string{"prod": "c-abc"}}
	eff, _, err := planDelete(t, NewDelete(k, okFac), input{args: []string{"p1"}, s: map[string]string{"cluster": "prod"}})
	require.NoError(t, err)
	assert.Equal(t, "c-abc", eff.Cluster, "Effect targets the downstream cluster, not the local proxy")
	assert.Equal(t, []string{engine.LocalCluster}, okFac.clusterCalls, "client is the local proxy")
	assert.Equal(t, "c-abc", okClient.deletes[0].ns, "dry-run delete uses the resolved namespace")
}

// TestDeleteExecPhaseNotFoundCurated pins the post-confirmation TOCTOU path:
// the object is deleted between the user's confirmation and the real execute
// call; the dynamic client curates the NOT_FOUND; the fake simulates that.
func TestDeleteExecPhaseNotFoundCurated(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "pod")
	client := &delClient{
		obj:     objWithRV("web", "7"),
		execErr: kinds.NotFoundCurated(k, "web"),
	}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	_, exec, err := planDelete(t, NewDelete(k, fac),
		input{args: []string{"web"}, s: map[string]string{"cluster": "prod", "namespace": "team-a"}})
	require.NoError(t, err, "Plan must succeed (dry-run succeeds)")
	require.NotNil(t, exec)

	_, err = exec(context.Background())
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "pod not found: web", ce.Message)
	assert.Equal(t, "list with: rancher get pods", ce.Hint)
}

// TestDeleteExecPhaseNotFoundWithHintPassesThrough pins that an already-curated
// exec-phase NOT_FOUND (one that already carries a hint) passes through unchanged.
func TestDeleteExecPhaseNotFoundWithHintPassesThrough(t *testing.T) {
	t.Parallel()
	alreadyCurated := clierr.New(clierr.CodeNotFound,
		"this Rancher does not serve ext.cattle.io (requires Rancher >= 2.14, the supported minimum)").
		WithHint("token management: auth list-tokens; kubeconfig: the default exec-credential mode")
	client := &delClient{
		obj:     objWithRV("t-xxx", "1"),
		execErr: alreadyCurated,
	}
	fac := &recFactory{client: client}
	_, exec, err := planDelete(t, NewDelete(mustKind(t, "token"), fac),
		input{args: []string{"t-xxx"}})
	require.NoError(t, err)

	_, err = exec(context.Background())
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Contains(t, ce.Message, "ext.cattle.io", "already-curated message must pass through")
	assert.Contains(t, ce.Hint, "auth list-tokens", "already-curated hint must pass through")
}
