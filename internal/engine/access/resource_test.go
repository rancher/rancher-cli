package access

import (
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

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
		got, _, err := ResolveResource(t.Context(), c, in, "")
		require.NoError(t, err, in)
		assert.Equal(t, ResourceRef{Group: "apps", Version: "v1", Resource: "deployments", Kind: "Deployment", Namespaced: true, Subresources: []string{"scale"}}, got)
	}
	got, _, err := ResolveResource(t.Context(), c, "nodes", "")
	require.NoError(t, err)
	assert.False(t, got.Namespaced)
	assert.Equal(t, "v1", got.Version)
	_, _, err = ResolveResource(t.Context(), c, "pods/log", "")
	require.ErrorContains(t, err, "unknown resource")
}

// TestResolveResourceKindNameIsUnknown asserts a Kind spelling is outside
// the grammar: plural, singular, short name, and name.group resolve, the
// Kind does not.
func TestResolveResourceKindNameIsUnknown(t *testing.T) {
	t.Parallel()
	c := &fakeClient{Resources: discovery()}
	for _, in := range []string{"deployments", "deployment", "deploy", "deployments.apps"} {
		_, _, err := ResolveResource(t.Context(), c, in, "")
		require.NoError(t, err, in)
	}
	_, _, err := ResolveResource(t.Context(), c, "Deployment", "")
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
	got, _, err := ResolveResource(t.Context(), c, "pods", "")
	require.NoError(t, err)
	assert.Equal(t, "", got.Group)
	assert.Equal(t, "v1", got.Version)
	assert.Equal(t, []string{"log", "status"}, got.Subresources)
	got, _, err = ResolveResource(t.Context(), c, "pods", "metrics.k8s.io")
	require.NoError(t, err)
	assert.Equal(t, "v1beta1", got.Version)
	got, _, err = ResolveResource(t.Context(), c, "pods.metrics.k8s.io", "")
	require.NoError(t, err)
	assert.Equal(t, "metrics.k8s.io", got.Group)
}

// TestResolveResourceNonCoreTieNeedsGroup asserts two non-core groups
// serving the same plural is the one ambiguity that needs --group.
func TestResolveResourceNonCoreTieNeedsGroup(t *testing.T) {
	t.Parallel()
	res := append(discovery(), engine.APIResource{Group: "extensions", Version: "v1beta1", Name: "deployments", SingularName: "deployment", Kind: "Deployment", Namespaced: true})
	c := &fakeClient{Resources: res}
	_, _, err := ResolveResource(t.Context(), c, "deployments", "")
	require.ErrorContains(t, err, "--group")
	require.ErrorContains(t, err, "exists in groups apps, extensions;")
	slices.Reverse(res)
	_, _, errRev := ResolveResource(t.Context(), &fakeClient{Resources: res}, "deployments", "")
	assert.Equal(t, err.Error(), errRev.Error(), "the message does not depend on discovery order")
	got, _, err := ResolveResource(t.Context(), c, "deployments", "apps")
	require.NoError(t, err)
	assert.Equal(t, "apps", got.Group)
}

// TestResolveResourceUnknownNamesClosest asserts the validation message
// offers the closest names.
func TestResolveResourceUnknownNamesClosest(t *testing.T) {
	t.Parallel()
	c := &fakeClient{Resources: discovery()}
	_, _, err := ResolveResource(t.Context(), c, "deploymnts", "")
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

func incompleteFor(groups ...string) *engine.DiscoveryIncomplete {
	failed := map[schema.GroupVersion]error{}
	for _, g := range groups {
		failed[schema.GroupVersion{Group: g, Version: "v1"}] = errors.New("service unavailable")
	}
	return &engine.DiscoveryIncomplete{Failed: failed}
}

// TestResolveResourceIncompleteBareNameResolvesWithWarning asserts a bare
// name with one healthy match resolves under incomplete discovery and the
// warning names the failed groups and the assumption made.
func TestResolveResourceIncompleteBareNameResolvesWithWarning(t *testing.T) {
	t.Parallel()
	c := &fakeClient{Resources: discovery(), ResourcesErr: incompleteFor("dead.example")}
	ref, warn, err := ResolveResource(t.Context(), c, "deployments", "")
	require.NoError(t, err)
	assert.Equal(t, "apps", ref.Group)
	assert.Equal(t, "discovery for API group(s) dead.example failed; deployments resolved to deployments.apps assuming the failed group(s) serve no such resource", warn)
}

// TestResolveResourceIncompleteCoreAndQualifiedCarryNoWarning asserts the
// two exact cases: a core-group match wins by rule, and a qualified name
// is exact, so neither carries the caveat.
func TestResolveResourceIncompleteCoreAndQualifiedCarryNoWarning(t *testing.T) {
	t.Parallel()
	c := &fakeClient{Resources: discovery(), ResourcesErr: incompleteFor("dead.example")}
	ref, warn, err := ResolveResource(t.Context(), c, "pods", "")
	require.NoError(t, err)
	assert.Equal(t, "", ref.Group)
	assert.Empty(t, warn)
	ref, warn, err = ResolveResource(t.Context(), c, "deployments.apps", "")
	require.NoError(t, err)
	assert.Equal(t, "apps", ref.Group)
	assert.Empty(t, warn)
}

// TestResolveResourceIncompleteUnknownNameIsRetryable asserts that a name
// no healthy group serves is undetermined, not unknown: a retryable error
// naming the failed groups, for a bare name and for a name qualified with
// the failed group alike.
func TestResolveResourceIncompleteUnknownNameIsRetryable(t *testing.T) {
	t.Parallel()
	c := &fakeClient{Resources: discovery(), ResourcesErr: incompleteFor("dead.example")}
	for _, arg := range []string{"widgets", "widgets.dead.example"} {
		_, _, err := ResolveResource(t.Context(), c, arg, "")
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, arg)
		assert.Equal(t, clierr.CodeGeneric, ce.Code, arg)
		assert.True(t, ce.Retryable, arg)
		require.ErrorContains(t, err, "discovery for dead.example failed")
		assert.NotContains(t, err.Error(), "unknown resource")
	}
	// A name qualified with a HEALTHY group that does not serve it is
	// unknown as before: the failed groups cannot be where it lives.
	_, _, err := ResolveResource(t.Context(), c, "widgets.apps", "")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, "unknown resource")
}

// TestResolveResourceIncompleteEmptyListIsRetryable asserts that when the
// server described nothing at all, every name is undetermined: a retryable
// error naming the failed groups.
func TestResolveResourceIncompleteEmptyListIsRetryable(t *testing.T) {
	t.Parallel()
	c := &fakeClient{Resources: nil, ResourcesErr: incompleteFor("", "apps")}
	_, _, err := ResolveResource(t.Context(), c, "pods", "")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.True(t, ce.Retryable)
	require.ErrorContains(t, err, "discovery for apps, core failed")
}

// TestResolveResourceOtherErrorsPassThrough asserts a total discovery
// failure is returned as is.
func TestResolveResourceOtherErrorsPassThrough(t *testing.T) {
	t.Parallel()
	boom := clierr.Retryable(clierr.CodeGeneric, "discovery unavailable")
	c := &fakeClient{ResourcesErr: boom}
	_, _, err := ResolveResource(t.Context(), c, "pods", "")
	assert.Equal(t, boom, err)
}
