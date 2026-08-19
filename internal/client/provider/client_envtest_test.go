//go:build envtest

package provider

import (
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var widgetGVR = schema.GroupVersionResource{Group: "test.rancher.io", Version: "v1", Resource: "widgets"}

func widgetCRD() *apiextv1.CustomResourceDefinition {
	scope := apiextv1.NamespaceScoped
	preserve := true
	return &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets.test.rancher.io"},
		Spec: apiextv1.CustomResourceDefinitionSpec{
			Group: "test.rancher.io", Scope: scope,
			Names: apiextv1.CustomResourceDefinitionNames{Plural: "widgets", Kind: "Widget", ListKind: "WidgetList", Singular: "widget"},
			Versions: []apiextv1.CustomResourceDefinitionVersion{{
				Name: "v1", Served: true, Storage: true,
				Schema: &apiextv1.CustomResourceValidation{OpenAPIV3Schema: &apiextv1.JSONSchemaProps{
					Type: "object", XPreserveUnknownFields: &preserve,
				}},
			}},
		},
	}
}

func TestRepoContractAgainstEnvtest(t *testing.T) {
	t.Parallel()
	env := &envtest.Environment{CRDs: []*apiextv1.CustomResourceDefinition{widgetCRD()}}
	rc, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.Stop() })

	repo, err := NewClusterClient(rc)
	require.NoError(t, err)
	ctx := t.Context()
	w := func(name string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "test.rancher.io/v1", "kind": "Widget",
			"metadata": map[string]any{"name": name, "namespace": "default"},
		}}
	}

	// dry-run create must NOT persist.
	_, err = repo.Create(ctx, widgetGVR, "default", w("dry"), true)
	require.NoError(t, err)
	_, err = repo.Get(ctx, widgetGVR, "default", "dry")
	require.Error(t, err, "dry-run create must not persist")

	// real create persists.
	created, err := repo.Create(ctx, widgetGVR, "default", w("real"), false)
	require.NoError(t, err)
	assert.NotEmpty(t, created.GetResourceVersion())

	// delete with a stale resourceVersion precondition → conflict (409).
	err = repo.Delete(ctx, widgetGVR, "default", "real", "1", false)
	require.Error(t, err, "stale resourceVersion precondition must 409")

	// delete with the correct resourceVersion succeeds.
	require.NoError(t, repo.Delete(ctx, widgetGVR, "default", "real", created.GetResourceVersion(), false))
}

// TestUpdateStaleResourceVersionConflicts is the patch precondition end to end:
// an update carrying a resourceVersion the server has since advanced past must
// 409, and the provider must map that to CONFLICT — the guarantee patch relies
// on so a concurrent modification is never a lost write.
func TestUpdateStaleResourceVersionConflicts(t *testing.T) {
	t.Parallel()
	env := &envtest.Environment{CRDs: []*apiextv1.CustomResourceDefinition{widgetCRD()}}
	rc, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.Stop() })

	repo, err := NewClusterClient(rc)
	require.NoError(t, err)
	ctx := t.Context()

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "test.rancher.io/v1", "kind": "Widget",
		"metadata": map[string]any{"name": "conflict", "namespace": "default"},
		"spec":     map[string]any{"replicas": int64(1)},
	}}
	created, err := repo.Create(ctx, widgetGVR, "default", obj, false)
	require.NoError(t, err)
	staleRV := created.GetResourceVersion()

	// An out-of-band update advances the stored resourceVersion.
	bumped := created.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(bumped.Object, int64(2), "spec", "replicas"))
	updated, err := repo.Update(ctx, widgetGVR, "default", bumped, false)
	require.NoError(t, err)
	require.NotEqual(t, staleRV, updated.GetResourceVersion(), "the out-of-band update must bump the RV")

	// A second update carrying the now-stale RV must 409 → CONFLICT.
	stale := created.DeepCopy()
	stale.SetResourceVersion(staleRV)
	require.NoError(t, unstructured.SetNestedField(stale.Object, int64(3), "spec", "replicas"))
	_, err = repo.Update(ctx, widgetGVR, "default", stale, false)
	require.Error(t, err, "a stale resourceVersion must be rejected")
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce), "want a clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
}
