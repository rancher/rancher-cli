package projects

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// listClient is a minimal engine.ClusterClient fake that answers Get by
// "namespace/name" key; List always returns an empty list.
type listClient struct {
	// gets answers Get by "namespace/name" key; unset keys fall through to
	// a nil, nil no-op.
	gets map[string]*unstructured.Unstructured
	// getErrs, when set, returns the error for "namespace/name" key.
	getErrs map[string]error
}

func (l *listClient) Get(_ context.Context, _ schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	k := ns + "/" + name
	if err, ok := l.getErrs[k]; ok {
		return nil, err
	}
	if obj, ok := l.gets[k]; ok {
		return obj, nil
	}
	return nil, nil
}

func (l *listClient) List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error) {
	return &unstructured.UnstructuredList{}, nil
}

func (l *listClient) Create(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}

func (l *listClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}

func (l *listClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}

func (l *listClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", nil
}

// TestMatchOneOrError exercises the shared List->match->switch assembly in
// isolation from any particular caller's List/display shape, asserting the
// three outcomes (single match, NOT_FOUND, AMBIGUOUS_SELECTION) plus the
// idFound-vs-displayMatchCount wording split each caller (the CLI cluster
// resolver, the token cluster lookup and provisioning.resolveCredentialObj)
// relies on.
func TestMatchOneOrError(t *testing.T) {
	t.Parallel()
	ids := []string{"a-1", "a-2"}
	displayByID := map[string]string{"a-1": "Alpha", "a-2": "Beta"}
	displayOf := func(id string) string { return displayByID[id] }
	cands := []clierr.Candidate{{ID: "a-1", Name: "Alpha"}, {ID: "a-2", Name: "Beta"}}
	ambiguousMsg := func(idFound bool, displayMatchCount int) string {
		if idFound {
			return "id-collision"
		}
		return fmt.Sprintf("display-collision:%d", displayMatchCount)
	}

	t.Run("single match by id", func(t *testing.T) {
		t.Parallel()
		id, err := MatchOneOrError("a-1", ids, displayOf, cands, "not found", "hint-nf", ambiguousMsg, "hint-amb")
		require.NoError(t, err)
		assert.Equal(t, "a-1", id)
	})

	t.Run("single match by display name", func(t *testing.T) {
		t.Parallel()
		id, err := MatchOneOrError("Alpha", ids, displayOf, cands, "not found", "hint-nf", ambiguousMsg, "hint-amb")
		require.NoError(t, err)
		assert.Equal(t, "a-1", id)
	})

	t.Run("no match uses caller's not-found message and hint verbatim", func(t *testing.T) {
		t.Parallel()
		_, err := MatchOneOrError("nope", ids, displayOf, cands, "not found: nope", "hint-nf", ambiguousMsg, "hint-amb")
		require.Error(t, err)
		cerr, ok := err.(*clierr.Error)
		require.True(t, ok)
		assert.Equal(t, clierr.CodeNotFound, cerr.Code)
		assert.Equal(t, "not found: nope", cerr.Message)
		assert.Equal(t, "hint-nf", cerr.Hint)
		assert.Equal(t, cands, cerr.Candidates)
	})

	t.Run("id collides with a different item's display name calls ambiguousMsg with idFound=true", func(t *testing.T) {
		t.Parallel()
		collidingIDs := []string{"Beta", "a-2"}
		collidingDisplay := map[string]string{"Beta": "x", "a-2": "Beta"}
		_, err := MatchOneOrError("Beta", collidingIDs, func(id string) string { return collidingDisplay[id] }, cands,
			"not found", "hint-nf", ambiguousMsg, "hint-amb")
		require.Error(t, err)
		cerr, ok := err.(*clierr.Error)
		require.True(t, ok)
		assert.Equal(t, clierr.CodeAmbiguousSelection, cerr.Code)
		assert.Equal(t, "id-collision", cerr.Message)
		assert.Equal(t, "hint-amb", cerr.Hint)
	})

	t.Run("two items share a display name calls ambiguousMsg with the match count", func(t *testing.T) {
		t.Parallel()
		sharedIDs := []string{"a-1", "a-2"}
		sharedDisplay := map[string]string{"a-1": "Shared", "a-2": "Shared"}
		_, err := MatchOneOrError("Shared", sharedIDs, func(id string) string { return sharedDisplay[id] }, cands,
			"not found", "hint-nf", ambiguousMsg, "hint-amb")
		require.Error(t, err)
		cerr, ok := err.(*clierr.Error)
		require.True(t, ok)
		assert.Equal(t, clierr.CodeAmbiguousSelection, cerr.Code)
		assert.Equal(t, "display-collision:2", cerr.Message)
	})

	t.Run("ambiguity narrows candidates to the colliders only", func(t *testing.T) {
		t.Parallel()
		// Three items, two colliding on the display name: the error's
		// candidates must exclude the non-collider - asserted at the helper
		// so every caller inherits the narrowing.
		threeIDs := []string{"a-1", "a-2", "a-3"}
		threeDisplay := map[string]string{"a-1": "Shared", "a-2": "Shared", "a-3": "Solo"}
		threeCands := []clierr.Candidate{
			{ID: "a-1", Name: "Shared"}, {ID: "a-2", Name: "Shared"}, {ID: "a-3", Name: "Solo"},
		}
		_, err := MatchOneOrError("Shared", threeIDs, func(id string) string { return threeDisplay[id] }, threeCands,
			"not found", "hint-nf", ambiguousMsg, "hint-amb")
		require.Error(t, err)
		cerr, ok := err.(*clierr.Error)
		require.True(t, ok)
		assert.Equal(t, clierr.CodeAmbiguousSelection, cerr.Code)
		assert.Equal(t, []clierr.Candidate{
			{ID: "a-1", Name: "Shared"}, {ID: "a-2", Name: "Shared"},
		}, cerr.Candidates, "the non-colliding candidate must be excluded")
	})
}

func projectWithBackingNS(id, backingNS string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": id}}}
	if backingNS != "" {
		_ = unstructured.SetNestedField(obj.Object, backingNS, "status", "backingNamespace")
	}
	return obj
}

func TestBackingNamespaceUsesStatusField(t *testing.T) {
	t.Parallel()
	client := &listClient{gets: map[string]*unstructured.Unstructured{
		"c-x/p-1": projectWithBackingNS("p-1", "p-abc123"),
	}}
	factory := stubFactory{c: client}

	ns, statusEmpty, err := BackingNamespace(t.Context(), factory, "c-x", "p-1")
	require.NoError(t, err)
	assert.Equal(t, "p-abc123", ns)
	assert.False(t, statusEmpty)
}

func TestBackingNamespaceFallsBackToProjectID(t *testing.T) {
	t.Parallel()
	client := &listClient{gets: map[string]*unstructured.Unstructured{
		"c-x/p-1": projectWithBackingNS("p-1", ""),
	}}
	factory := stubFactory{c: client}

	ns, statusEmpty, err := BackingNamespace(t.Context(), factory, "c-x", "p-1")
	require.NoError(t, err)
	assert.Equal(t, "p-1", ns)
	assert.True(t, statusEmpty)
}

func TestBackingNamespaceNotFoundCurated(t *testing.T) {
	t.Parallel()
	projectKind, _ := kinds.Lookup("project")
	client := &listClient{getErrs: map[string]error{"c-x/p-gone": kinds.NotFoundCurated(projectKind, "p-gone", "")}}
	factory := stubFactory{c: client}

	_, _, err := BackingNamespace(t.Context(), factory, "c-x", "p-gone")
	require.Error(t, err)

	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "project not found: p-gone", ce.Message)
	assert.NotEmpty(t, ce.Hint, "curated error must carry a hint")
}

func TestBackingNamespaceNotFoundWithHintPassesThrough(t *testing.T) {
	t.Parallel()
	alreadyCurated := clierr.New(clierr.CodeNotFound, "project not found: p-gone").
		WithHint("list with: rancher get projects")
	client := &listClient{getErrs: map[string]error{"c-x/p-gone": alreadyCurated}}
	factory := stubFactory{c: client}

	_, _, err := BackingNamespace(t.Context(), factory, "c-x", "p-gone")
	require.Error(t, err)
	assert.Equal(t, alreadyCurated, err, "already-curated error must pass through unchanged")
}

var _ engine.ClusterClient = (*listClient)(nil)

func (*listClient) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	return engine.ReviewResponse{}, errors.New("PostReview not scripted in this fake")
}

func (*listClient) ServerResources(context.Context) ([]engine.APIResource, error) {
	return nil, errors.New("ServerResources not scripted in this fake")
}
