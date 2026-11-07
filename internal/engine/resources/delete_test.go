package resources

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

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
	return mc.Plan(t.Context(), in)
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

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "PodDelete", res.Kind)

	last := client.deletes[len(client.deletes)-1]
	assert.Equal(t, delCall{ns: "team-a", name: "web", rv: "42", dryRun: false}, last,
		"the executor deletes with the captured RV precondition, for real")
}

// TestDeleteMultiWordKindUsesK8sKindCasing asserts, for
// the delete path, that the envelope kind echoes k.K8sKind's casing
// ("StatefulSet"), not a naive first-rune capitalization ("Statefulset").
func TestDeleteMultiWordKindUsesK8sKindCasing(t *testing.T) {
	t.Parallel()
	client := &delClient{obj: objWithRV("web", "1")}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	cmd := NewDelete(mustKind(t, "statefulset"), fac)

	_, exec, err := planDelete(t, cmd, input{args: []string{"web"}, s: map[string]string{"cluster": "prod", "namespace": "team-a"}})
	require.NoError(t, err)

	res, err := exec(t.Context())
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
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
}

func TestDeleteNotFoundSurfacesCleanly(t *testing.T) {
	t.Parallel()
	client := &delClient{getErr: clierr.New(clierr.CodeNotFound, `pods "web" not found`)}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	_, _, err := planDelete(t, NewDelete(mustKind(t, "pod"), fac),
		input{args: []string{"web"}, s: map[string]string{"cluster": "prod", "namespace": "team-a"}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
}

// TestDeleteNotFoundCurated asserts the curated NOT_FOUND shape: the dynamic client
// now produces "<kind> not found: <name>"; the fake simulates that.
func TestDeleteNotFoundCurated(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "pod")
	client := &delClient{getErr: kinds.NotFoundCurated(k, "web", "c-abc")}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	_, _, err := planDelete(t, NewDelete(k, fac),
		input{args: []string{"web"}, s: map[string]string{"cluster": "prod", "namespace": "team-a"}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "pod not found: web", ce.Message)
	assert.Equal(t, clierr.Rancher+" get pods --cluster 'c-abc'", ce.Hint)
}

// TestDeleteNonNotFoundPassesThrough asserts that a non-NOT_FOUND error from the
// client (e.g. RBAC) reaches the caller unchanged without re-wrapping.
func TestDeleteNonNotFoundPassesThrough(t *testing.T) {
	t.Parallel()
	client := &delClient{getErr: clierr.New(clierr.CodeRBAC, "forbidden")}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	_, _, err := planDelete(t, NewDelete(mustKind(t, "pod"), fac),
		input{args: []string{"web"}, s: map[string]string{"cluster": "prod", "namespace": "team-a"}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, "forbidden", ce.Message)
}

// TestDeleteDryRunNotFoundCurated asserts the TOCTOU path: when Get succeeds but
// the dry-run delete returns a NOT_FOUND (deleted between Get and dry-run),
// the dynamic client curates it; the fake simulates that pre-curated shape.
func TestDeleteDryRunNotFoundCurated(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "pod")
	client := &delClient{
		obj:       objWithRV("web", "7"),
		dryRunErr: kinds.NotFoundCurated(k, "web", "c-abc"),
	}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	_, _, err := planDelete(t, NewDelete(k, fac),
		input{args: []string{"web"}, s: map[string]string{"cluster": "prod", "namespace": "team-a"}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "pod not found: web", ce.Message)
	assert.Equal(t, clierr.Rancher+" get pods --cluster 'c-abc'", ce.Hint)
}

// TestDeleteDryRunNotFoundWithHintPassesThrough asserts that an already-curated
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
	require.ErrorAs(t, err, &ce)
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
	require.ErrorAs(t, err, &ce)
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
	require.ErrorAs(t, err, &ce)
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

// TestDeleteExecPhaseNotFoundCurated asserts the post-confirmation TOCTOU path:
// the object is deleted between the user's confirmation and the real execute
// call; the dynamic client curates the NOT_FOUND; the fake simulates that.
func TestDeleteExecPhaseNotFoundCurated(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "pod")
	client := &delClient{
		obj:     objWithRV("web", "7"),
		execErr: kinds.NotFoundCurated(k, "web", "c-abc"),
	}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	_, exec, err := planDelete(t, NewDelete(k, fac),
		input{args: []string{"web"}, s: map[string]string{"cluster": "prod", "namespace": "team-a"}})
	require.NoError(t, err, "Plan must succeed (dry-run succeeds)")
	require.NotNil(t, exec)

	_, err = exec(t.Context())
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "pod not found: web", ce.Message)
	assert.Equal(t, clierr.Rancher+" get pods --cluster 'c-abc'", ce.Hint)
}

// TestDeleteExecPhaseNotFoundWithHintPassesThrough asserts that an already-curated
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

	_, err = exec(t.Context())
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Contains(t, ce.Message, "ext.cattle.io", "already-curated message must pass through")
	assert.Contains(t, ce.Hint, "auth list-tokens", "already-curated hint must pass through")
}

// TestDeleteCarriesRowCascadeWarning asserts, in table-row form, that a kind
// whose row declares DeleteCascadeWarning gets that advisory in its generic
// delete Effect (gitrepo is the first user), and rows without one stay
// warning-free.
func TestDeleteCarriesRowCascadeWarning(t *testing.T) {
	t.Parallel()
	gitrepo := mustKind(t, "gitrepo")
	require.NotEmpty(t, gitrepo.DeleteCascadeWarning, "gitrepo row must declare the cascade warning")
	require.NotEmpty(t, mustKind(t, "project").DeleteCascadeWarning, "project row must declare the cascade warning")

	client := &delClient{obj: objWithRV("repo-1", "7")}
	fac := &recFactory{client: client}
	eff, exec, err := planDelete(t, NewDelete(gitrepo, fac), input{args: []string{"repo-1"}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, []string{gitrepo.DeleteCascadeWarning}, eff.Warnings)
	assert.Contains(t, gitrepo.DeleteCascadeWarning, "makes Fleet delete")

	helmop := mustKind(t, "helmop")
	require.NotEmpty(t, helmop.DeleteCascadeWarning, "helmop row must declare the cascade warning")
	hclient := &delClient{obj: objWithRV("chart-1", "3")}
	heff, hexec, err := planDelete(t, NewDelete(helmop, &recFactory{client: hclient}), input{args: []string{"chart-1"}})
	require.NoError(t, err)
	require.NotNil(t, hexec)
	assert.Equal(t, []string{helmop.DeleteCascadeWarning}, heff.Warnings)
	assert.Contains(t, helmop.DeleteCascadeWarning, "uninstalls the chart from every target cluster")

	plain := &delClient{obj: objWithRV("web", "1")}
	eff2, _, err := planDelete(t, NewDelete(mustKind(t, "pod"), &recFactory{client: plain, resolve: map[string]string{"prod": "c-abc"}}),
		input{args: []string{"web"}, s: map[string]string{"cluster": "prod", "namespace": "team-a"}})
	require.NoError(t, err)
	assert.Empty(t, eff2.Warnings, "a row without DeleteCascadeWarning must not grow one")
}

// TestWaitStalledScopedToOptInKinds asserts that the
// Stalled=True fast-fail fires only for rows with WaitFailsOnStalled
// (fleet's kstatus kinds), never for kinds whose controllers may use
// Stalled transiently.
func TestWaitStalledScopedToOptInKinds(t *testing.T) {
	withFastPoll(t)
	synctest.Test(t, func(t *testing.T) {
		stalledReady := multiConditionObj(
			[3]string{"Ready", "False", ""},
			[3]string{"Stalled", "True", "boom"},
		)
		cond := waitCond{kind: "condition", name: "Ready", status: "True"}

		// Opt-in kind: aborts immediately.
		client := &seqClient{objs: []*unstructured.Unstructured{stalledReady}}
		_, err := waitFor(t.Context(), client, schema.GroupVersionResource{}, "", "x", cond, time.Second, true)
		require.Error(t, err)
		require.ErrorContains(t, err, "stalled")

		// Non-opt-in kind: keeps polling to the eventual Ready.
		ready := multiConditionObj([3]string{"Ready", "True", ""}, [3]string{"Stalled", "True", "boom"})
		client2 := &seqClient{objs: []*unstructured.Unstructured{stalledReady, ready}}
		got, err := waitFor(t.Context(), client2, schema.GroupVersionResource{}, "", "x", cond, time.Second, false)
		require.NoError(t, err, "without the opt-in, Stalled=True must not abort the wait")
		require.NotNil(t, got)
	})
}

// TestWaitFlagWiredFromKindRow asserts the row->waitFor plumbing: gitrepo,
// bundle, and fleet-cluster opt in; the common Ready-waiting kinds do not.
func TestWaitFlagWiredFromKindRow(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"gitrepo", "bundle", "fleet-cluster"} {
		assert.True(t, mustKind(t, name).WaitFailsOnStalled, name)
	}
	for _, name := range []string{"pod", "deployment", "node", "cluster"} {
		assert.False(t, mustKind(t, name).WaitFailsOnStalled, name)
	}
}

// TestDeleteDescriptorOfBuiltInKubernetesKindsIsDestructive asserts that each
// of the six kinds is deletable and that its generated delete command is
// destructive. A destructive command requires --yes.
func TestDeleteDescriptorOfBuiltInKubernetesKindsIsDestructive(t *testing.T) {
	t.Parallel()
	for _, name := range builtInKubernetesKinds {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			k := mustKind(t, name)
			require.True(t, k.Deletable)
			assert.Equal(t, "true", NewDelete(k, nil).Describe().Destructive)
		})
	}
}

// TestDeleteDescribeInheritsCapability asserts the requirement-inheritance
// rule: a kind row's Capability flows unchanged into the generated delete
// command's Descriptor.
func TestDeleteDescribeInheritsCapability(t *testing.T) {
	t.Parallel()
	req := runtime.CapabilityRequirement{Tier: runtime.CapabilityPrime, Feature: "widget-feature"}
	k := kinds.Kind{Name: "widget", Aliases: []string{"widgets"}, Deletable: true, Capability: req}
	d := NewDelete(k, nil).Describe()
	assert.Equal(t, req, d.Capability)
}

func (*delClient) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	return engine.ReviewResponse{}, errors.New("PostReview not scripted in this fake")
}

func (*delClient) ServerResources(context.Context) ([]engine.APIResource, error) {
	return nil, errors.New("ServerResources not scripted in this fake")
}

// TestDeleteAndPatchRequiredClusterDescDropsTheListAdvice asserts a kind that
// lists across clusters on get describes its --cluster, once delete and
// patch require it, as the cluster of the object, with no advice to omit it.
func TestDeleteAndPatchRequiredClusterDescDropsTheListAdvice(t *testing.T) {
	t.Parallel()
	for name, by := range map[string]string{"project": "id", "clusterroletemplatebinding": "name"} {
		k, ok := kinds.Lookup(name)
		require.True(t, ok, name)
		require.Equal(t, kinds.NSCluster, k.Policy, name)
		for _, flags := range [][]runtime.ArgSpec{getFlags(k), deleteFlags(k)} {
			i := slices.IndexFunc(flags, func(f runtime.ArgSpec) bool { return f.Name == FlagCluster })
			require.GreaterOrEqual(t, i, 0, name)
			if flags[i].Required {
				assert.Equal(t, "cluster `id|name` of the "+name, flags[i].Desc)
			} else {
				assert.Equal(t, "cluster `id|name`; required to get one "+name+" by "+by+"; leave it out to list "+kinds.Plural(name)+" in every cluster", flags[i].Desc)
			}
		}
	}
}
