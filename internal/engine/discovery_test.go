package engine

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apidiscoveryv2 "k8s.io/api/apidiscovery/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestResourcesFromAggregatedPicksPreferredAndMarksStale asserts the
// projection: the first non-stale version contributes resources and
// subresources; a stale version is a failed group version and contributes
// nothing, even when a later version of the same group is current.
func TestResourcesFromAggregatedPicksPreferredAndMarksStale(t *testing.T) {
	t.Parallel()
	doc := &apidiscoveryv2.APIGroupDiscoveryList{Items: []apidiscoveryv2.APIGroupDiscovery{
		{Name: "apps", Versions: []apidiscoveryv2.APIVersionDiscovery{{
			Version: "v1", Freshness: apidiscoveryv2.DiscoveryFreshnessCurrent,
			Resources: []apidiscoveryv2.APIResourceDiscovery{{
				Resource: "deployments", SingularResource: "deployment", Scope: apidiscoveryv2.ScopeNamespace,
				ResponseKind: &metav1.GroupVersionKind{Kind: "Deployment"}, ShortNames: []string{"deploy"}, Verbs: []string{"get", "list"},
				Subresources: []apidiscoveryv2.APISubresourceDiscovery{{Subresource: "scale", ResponseKind: &metav1.GroupVersionKind{Kind: "Scale"}, Verbs: []string{"get", "update"}}},
			}},
		}}},
		{Name: "dead.example", Versions: []apidiscoveryv2.APIVersionDiscovery{
			{Version: "v1beta1", Freshness: apidiscoveryv2.DiscoveryFreshnessStale},
			{Version: "v1alpha1", Freshness: apidiscoveryv2.DiscoveryFreshnessCurrent, Resources: []apidiscoveryv2.APIResourceDiscovery{{Resource: "widgets", Scope: apidiscoveryv2.ScopeCluster}}},
		}},
	}}
	res, failed := ResourcesFromAggregated(doc)
	assert.Equal(t, []APIResource{
		{Group: "apps", Version: "v1", Name: "deployments", SingularName: "deployment", Kind: "Deployment", ShortNames: []string{"deploy"}, Namespaced: true, Verbs: []string{"get", "list"}},
		{Group: "apps", Version: "v1", Name: "deployments/scale", Kind: "Scale", Namespaced: true, Verbs: []string{"get", "update"}},
		{Group: "dead.example", Version: "v1alpha1", Name: "widgets", Namespaced: false},
	}, res)
	require.Len(t, failed, 1)
	assert.Contains(t, failed, schema.GroupVersion{Group: "dead.example", Version: "v1beta1"})
}

// TestDiscoveryIncompleteGroups asserts the error names groups once, sorted,
// with the empty group spelled "core", and that GroupFailed accepts both
// spellings of the core group.
func TestDiscoveryIncompleteGroups(t *testing.T) {
	t.Parallel()
	e := &DiscoveryIncomplete{Failed: map[schema.GroupVersion]error{
		{Group: "metrics.k8s.io", Version: "v1beta1"}: errors.New("503"),
		{Group: "dead.example", Version: "v1beta1"}:   errors.New("503"),
		{Group: "dead.example", Version: "v1alpha1"}:  errors.New("503"),
		{Group: "", Version: "v1"}:                    errors.New("503"),
	}}
	assert.Equal(t, []string{"core", "dead.example", "metrics.k8s.io"}, e.Groups())
	assert.True(t, e.GroupFailed("dead.example"))
	assert.True(t, e.GroupFailed("core"))
	assert.True(t, e.GroupFailed(""))
	assert.False(t, e.GroupFailed("apps"))
	require.ErrorContains(t, e, "core, dead.example, metrics.k8s.io")
	assert.Nil(t, ErrDiscoveryIncompleteFor(nil))
	assert.True(t, IsAggregatedDiscovery("application/json;g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList"))
	assert.False(t, IsAggregatedDiscovery("application/json"))
}
