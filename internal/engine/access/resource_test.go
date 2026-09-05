package access

import (
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

func discovery() []engine.APIResource {
	return []engine.APIResource{
		{Group: "", Version: "v1", Name: "pods", SingularName: "pod", Kind: "Pod", ShortNames: []string{"po"}, Namespaced: true},
		{Group: "", Version: "v1", Name: "pods/log", Kind: "Pod", Namespaced: true},
		{Group: "", Version: "v1", Name: "pods/status", Kind: "Pod", Namespaced: true},
		{Group: "", Version: "v1", Name: "nodes", SingularName: "node", Kind: "Node", ShortNames: []string{"no"}, Namespaced: false},
		{Group: "apps", Version: "v1", Name: "deployments", SingularName: "deployment", Kind: "Deployment", ShortNames: []string{"deploy"}, Namespaced: true},
		{Group: "apps", Version: "v1", Name: "deployments/scale", Kind: "Scale", Namespaced: true},
		{Group: "metrics.k8s.io", Version: "v1beta1", Name: "pods", Kind: "PodMetrics", Namespaced: true},
	}
}

// TestResolveResourceAcceptsEveryAliasAndKeepsVersion asserts plural,
// singular, short name, and resource.group all resolve to the preferred
// version, and that subresource entries are attached, never candidates.
func TestResolveResourceAcceptsEveryAliasAndKeepsVersion(t *testing.T) {
	t.Parallel()
	c := &fakeClient{Resources: discovery()}
	for _, in := range []string{"deployments", "deployment", "deploy", "deployments.apps"} {
		got, err := ResolveResource(t.Context(), c, in, "")
		require.NoError(t, err, in)
		assert.Equal(t, ResourceRef{Group: "apps", Version: "v1", Resource: "deployments", Kind: "Deployment", Namespaced: true, Subresources: []string{"scale"}}, got)
	}
	got, err := ResolveResource(t.Context(), c, "nodes", "")
	require.NoError(t, err)
	assert.False(t, got.Namespaced)
	assert.Equal(t, "v1", got.Version)
	_, err = ResolveResource(t.Context(), c, "pods/log", "")
	require.ErrorContains(t, err, "unknown resource")
}

// TestResolveResourceKindNameIsUnknown asserts a Kind spelling is outside
// the grammar: plural, singular, short name, and name.group resolve, the
// Kind does not.
func TestResolveResourceKindNameIsUnknown(t *testing.T) {
	t.Parallel()
	c := &fakeClient{Resources: discovery()}
	for _, in := range []string{"deployments", "deployment", "deploy", "deployments.apps"} {
		_, err := ResolveResource(t.Context(), c, in, "")
		require.NoError(t, err, in)
	}
	_, err := ResolveResource(t.Context(), c, "Deployment", "")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, "unknown resource")
}

// TestResolveResourceCoreGroupWinsBareName asserts a bare name served by
// the core group and another group resolves to core (metrics-server
// serves pods too on every Rancher-provisioned cluster), while --group
// or name.group selects the non-core one.
func TestResolveResourceCoreGroupWinsBareName(t *testing.T) {
	t.Parallel()
	c := &fakeClient{Resources: discovery()}
	got, err := ResolveResource(t.Context(), c, "pods", "")
	require.NoError(t, err)
	assert.Equal(t, "", got.Group)
	assert.Equal(t, "v1", got.Version)
	assert.Equal(t, []string{"log", "status"}, got.Subresources)
	got, err = ResolveResource(t.Context(), c, "pods", "metrics.k8s.io")
	require.NoError(t, err)
	assert.Equal(t, "v1beta1", got.Version)
	got, err = ResolveResource(t.Context(), c, "pods.metrics.k8s.io", "")
	require.NoError(t, err)
	assert.Equal(t, "metrics.k8s.io", got.Group)
}

// TestResolveResourceNonCoreTieNeedsGroup asserts two non-core groups
// serving the same plural is the one ambiguity that needs --group.
func TestResolveResourceNonCoreTieNeedsGroup(t *testing.T) {
	t.Parallel()
	res := append(discovery(), engine.APIResource{Group: "extensions", Version: "v1beta1", Name: "deployments", SingularName: "deployment", Kind: "Deployment", Namespaced: true})
	c := &fakeClient{Resources: res}
	_, err := ResolveResource(t.Context(), c, "deployments", "")
	require.ErrorContains(t, err, "--group")
	require.ErrorContains(t, err, "exists in groups apps, extensions;")
	slices.Reverse(res)
	_, errRev := ResolveResource(t.Context(), &fakeClient{Resources: res}, "deployments", "")
	assert.Equal(t, err.Error(), errRev.Error(), "the message does not depend on discovery order")
	got, err := ResolveResource(t.Context(), c, "deployments", "apps")
	require.NoError(t, err)
	assert.Equal(t, "apps", got.Group)
}

// TestResolveResourceUnknownNamesClosest asserts the validation message
// offers the closest names.
func TestResolveResourceUnknownNamesClosest(t *testing.T) {
	t.Parallel()
	c := &fakeClient{Resources: discovery()}
	_, err := ResolveResource(t.Context(), c, "deploymnts", "")
	require.ErrorContains(t, err, "unknown resource")
	require.ErrorContains(t, err, "deployments")
}

// TestNormalizeVerbGrammars asserts lowercase folding, the closed HTTP set
// for non-resource URLs, and shape-only validation for resource verbs.
func TestNormalizeVerbGrammars(t *testing.T) {
	t.Parallel()
	v, err := NormalizeVerb("GET", true)
	require.NoError(t, err)
	assert.Equal(t, "get", v)
	_, err = NormalizeVerb("create", true)
	require.ErrorContains(t, err, "HTTP verb")
	v, err = NormalizeVerb("Deletecollection", false)
	require.NoError(t, err)
	assert.Equal(t, "deletecollection", v)
	v, err = NormalizeVerb("updatepsa", false)
	require.NoError(t, err)
	assert.Equal(t, "updatepsa", v)
	_, err = NormalizeVerb("get pods", false)
	require.ErrorContains(t, err, "verb")
}
