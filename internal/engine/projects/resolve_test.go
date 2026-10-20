package projects

import (
	"context"
	"errors"
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

func namespaceWith(labels, annotations map[string]string) *unstructured.Unstructured {
	ns := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "team-a"}}}
	if labels != nil {
		ns.SetLabels(labels)
	}
	if annotations != nil {
		ns.SetAnnotations(annotations)
	}
	return ns
}

// TestProjectOfReadsTheAnnotationOnly asserts the project comes from the
// <cluster>:<project> annotation: a label alone, a malformed annotation and
// a missing one give no project.
func TestProjectOfReadsTheAnnotationOnly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		ns          *unstructured.Unstructured
		wantCluster string
		wantProject string
		wantOK      bool
	}{
		{"annotation", namespaceWith(nil, map[string]string{projectKey: "c-abc:p-1"}), "c-abc", "p-1", true},
		{"annotation and label", namespaceWith(map[string]string{projectKey: "p-1"}, map[string]string{projectKey: "c-abc:p-1"}), "c-abc", "p-1", true},
		{"label without annotation", namespaceWith(map[string]string{projectKey: "p-1"}, nil), "", "", false},
		{"no project", namespaceWith(nil, nil), "", "", false},
		{"no colon", namespaceWith(nil, map[string]string{projectKey: "p-1"}), "", "", false},
		{"empty cluster", namespaceWith(nil, map[string]string{projectKey: ":p-1"}), "", "", false},
		{"empty project", namespaceWith(nil, map[string]string{projectKey: "c-abc:"}), "", "", false},
		{"two colons", namespaceWith(nil, map[string]string{projectKey: "c-abc:p-1:x"}), "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, p, ok := ProjectOf(tt.ns)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantCluster, c)
			assert.Equal(t, tt.wantProject, p)
		})
	}
}

// TestIsMemberNeedsBothTheClusterAndTheProject asserts a namespace is a
// member only of the project its annotation names on the cluster it names;
// the label never makes one.
func TestIsMemberNeedsBothTheClusterAndTheProject(t *testing.T) {
	t.Parallel()
	annotated := namespaceWith(map[string]string{projectKey: "p-1"}, map[string]string{projectKey: "c-abc:p-1"})
	assert.True(t, IsMember(annotated, "c-abc", "p-1"))
	assert.False(t, IsMember(annotated, "c-def", "p-1"), "another cluster")
	assert.False(t, IsMember(annotated, "c-abc", "p-2"), "another project")
	assert.False(t, IsMember(namespaceWith(map[string]string{projectKey: "p-1"}, nil), "c-abc", "p-1"), "a label alone")
	assert.True(t, IsMember(namespaceWith(nil, map[string]string{projectKey: "c-abc:p-1"}), "c-abc", "p-1"), "an annotation alone")
}

func TestMemberSelectorIsTheLabelEquality(t *testing.T) {
	t.Parallel()
	got, err := MemberSelector("p-1")
	require.NoError(t, err)
	assert.Equal(t, "field.cattle.io/projectId=p-1", got)
	_, err = MemberSelector("bad id!")
	require.ErrorContains(t, err, "invalid project id")
}
