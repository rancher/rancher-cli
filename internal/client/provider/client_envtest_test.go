//go:build envtest

package provider

import (
	"context"
	"testing"

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
	ctx := context.Background()
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
