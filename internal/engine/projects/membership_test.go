package projects

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

// membershipClient fakes the pieces add/remove-namespace and create namespace
// exercise: List (ResolveProject's project list), Get (fetch the live
// namespace), Create, and Update (recorded for assertion), and dry-run
// tracking on both. getErr, when set, overrides every namespace Get with
// the given error (used by curation tests).
type membershipClient struct {
	projects   *unstructured.UnstructuredList
	namespaces map[string]*unstructured.Unstructured
	getErr     error // if non-nil, Get returns this for all namespace lookups

	updated       *unstructured.Unstructured
	updatedNS     string
	updateDryRuns []bool
	created       *unstructured.Unstructured
	createdNS     string
	createDryRuns []bool
}

func (m *membershipClient) Get(_ context.Context, _ schema.GroupVersionResource, _, name string) (*unstructured.Unstructured, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	if ns, ok := m.namespaces[name]; ok {
		return ns, nil
	}
	return nil, clierr.New(clierr.CodeNotFound, "namespace not found: "+name)
}

func (m *membershipClient) List(_ context.Context, gvr schema.GroupVersionResource, _, _ string) (*unstructured.UnstructuredList, error) {
	if gvr.Resource == "projects" && m.projects != nil {
		return m.projects, nil
	}
	return &unstructured.UnstructuredList{}, nil
}

func (m *membershipClient) Create(_ context.Context, _ schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	m.created, m.createdNS = obj, ns
	m.createDryRuns = append(m.createDryRuns, dryRun)
	return obj, nil
}

func (m *membershipClient) Update(_ context.Context, _ schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	m.updated, m.updatedNS = obj, ns
	m.updateDryRuns = append(m.updateDryRuns, dryRun)
	return obj, nil
}

func (m *membershipClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}

func (m *membershipClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", nil
}

func namespaceObj(name, projectID string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "resourceVersion": "1"},
	}}
	if projectID != "" {
		_ = unstructured.SetNestedField(obj.Object, projectID, "metadata", "labels", projectLabel)
		_ = unstructured.SetNestedField(obj.Object, "c-x:"+projectID, "metadata", "annotations", projectLabel)
	}
	return obj
}

func planMembership(t *testing.T, cmd runtime.Command, input runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "membership commands must be MutatingCommand")
	return mc.Plan(context.Background(), input)
}

func TestAddNamespaceSetsLabelAndAnnotation(t *testing.T) {
	t.Parallel()
	client := &membershipClient{
		projects:   &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web")}},
		namespaces: map[string]*unstructured.Unstructured{"team-a": namespaceObj("team-a", "")},
	}
	cmd := NewAddNamespace(stubFactory{c: client})

	eff, exec, err := planMembership(t, cmd, in{s: map[string]string{"project": "p-1", "cluster": "c-x"}, a: []string{"team-a"}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, safety.ActionUpdate, eff.Action)
	assert.Equal(t, "namespaces/team-a", eff.Target)
	assert.False(t, eff.Destructive)

	label, _, _ := unstructured.NestedString(client.updated.Object, "metadata", "labels", projectLabel)
	assert.Equal(t, "p-1", label)
	annotation, _, _ := unstructured.NestedString(client.updated.Object, "metadata", "annotations", projectLabel)
	assert.Equal(t, "c-x:p-1", annotation)
	require.NotEmpty(t, eff.Diff)
}

func TestAddNamespaceToDifferentProjectIsValidation(t *testing.T) {
	t.Parallel()
	client := &membershipClient{
		projects:   &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web"), project("p-2", "db")}},
		namespaces: map[string]*unstructured.Unstructured{"team-a": namespaceObj("team-a", "p-2")},
	}
	cmd := NewAddNamespace(stubFactory{c: client})

	_, _, err := planMembership(t, cmd, in{s: map[string]string{"project": "p-1", "cluster": "c-x"}, a: []string{"team-a"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "p-2")
	assert.Contains(t, ce.Hint, "remove-namespace")
}

func TestAddNamespaceAlreadyMemberIsEmptyDiff(t *testing.T) {
	t.Parallel()
	client := &membershipClient{
		projects:   &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web")}},
		namespaces: map[string]*unstructured.Unstructured{"team-a": namespaceObj("team-a", "p-1")},
	}
	cmd := NewAddNamespace(stubFactory{c: client})

	eff, _, err := planMembership(t, cmd, in{s: map[string]string{"project": "p-1", "cluster": "c-x"}, a: []string{"team-a"}})
	require.NoError(t, err)
	assert.Empty(t, eff.Diff, "re-adding to the same project is a no-change Effect")
}

func TestRemoveNamespaceClearsLabelAndAnnotation(t *testing.T) {
	t.Parallel()
	client := &membershipClient{
		projects:   &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web")}},
		namespaces: map[string]*unstructured.Unstructured{"team-a": namespaceObj("team-a", "p-1")},
	}
	cmd := NewRemoveNamespace(stubFactory{c: client})

	eff, _, err := planMembership(t, cmd, in{s: map[string]string{"project": "p-1", "cluster": "c-x"}, a: []string{"team-a"}})
	require.NoError(t, err)
	assert.Equal(t, safety.ActionUpdate, eff.Action)
	_, found, _ := unstructured.NestedString(client.updated.Object, "metadata", "labels", projectLabel)
	assert.False(t, found)
	_, found, _ = unstructured.NestedString(client.updated.Object, "metadata", "annotations", projectLabel)
	assert.False(t, found)
}

func TestRemoveNamespaceFromDifferentProjectIsValidation(t *testing.T) {
	t.Parallel()
	client := &membershipClient{
		projects:   &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web"), project("p-2", "db")}},
		namespaces: map[string]*unstructured.Unstructured{"team-a": namespaceObj("team-a", "p-2")},
	}
	cmd := NewRemoveNamespace(stubFactory{c: client})

	_, _, err := planMembership(t, cmd, in{s: map[string]string{"project": "p-1", "cluster": "c-x"}, a: []string{"team-a"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestCreateNamespaceWithProjectCarriesMembershipFromBirth(t *testing.T) {
	t.Parallel()
	client := &membershipClient{
		projects: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web")}},
	}
	cmd := NewCreateNamespace(stubFactory{c: client})

	eff, exec, err := planMembership(t, cmd, in{s: map[string]string{"project": "p-1", "cluster": "c-x"}, a: []string{"team-b"}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, safety.ActionCreate, eff.Action)
	label, _, _ := unstructured.NestedString(client.created.Object, "metadata", "labels", projectLabel)
	assert.Equal(t, "p-1", label)
	annotation, _, _ := unstructured.NestedString(client.created.Object, "metadata", "annotations", projectLabel)
	assert.Equal(t, "c-x:p-1", annotation)
}

func TestCreateNamespaceWithoutProjectHasNoMembership(t *testing.T) {
	t.Parallel()
	client := &membershipClient{}
	cmd := NewCreateNamespace(stubFactory{c: client})

	_, _, err := planMembership(t, cmd, in{s: map[string]string{"cluster": "c-x"}, a: []string{"team-c"}})
	require.NoError(t, err)
	_, found, _ := unstructured.NestedString(client.created.Object, "metadata", "labels", projectLabel)
	assert.False(t, found)
}

var _ engine.ClusterClient = (*membershipClient)(nil)

// TestLoadMembershipTargetNotFoundCurated verifies that a hint-less NOT_FOUND
// from the namespace Get is curated to "namespace not found: <name>" and that
// a hint-bearing NOT_FOUND passes through unchanged.
func TestLoadMembershipTargetNotFoundCurated(t *testing.T) {
	t.Parallel()

	// hint-less 404 → curated shape (produced by dynamic client; fake simulates it)
	nsKind, _ := kinds.Lookup("namespace")
	client := &membershipClient{
		projects: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web")}},
		getErr:   kinds.NotFoundCurated(nsKind, "no-ns"),
	}
	cmd := NewAddNamespace(stubFactory{c: client})
	_, _, err := planMembership(t, cmd, in{s: map[string]string{"project": "p-1", "cluster": "c-x"}, a: []string{"no-ns"}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, "namespace not found: no-ns", ce.Message)
	assert.Equal(t, "list with: rancher get namespaces", ce.Hint)

	// hint-bearing 404 passes through unchanged
	hinted := clierr.New(clierr.CodeNotFound, "unsupported group").WithHint("try another group")
	client2 := &membershipClient{
		projects: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web")}},
		getErr:   hinted,
	}
	cmd2 := NewAddNamespace(stubFactory{c: client2})
	_, _, err2 := planMembership(t, cmd2, in{s: map[string]string{"project": "p-1", "cluster": "c-x"}, a: []string{"no-ns"}})
	var ce2 *clierr.Error
	require.ErrorAs(t, err2, &ce2)
	assert.Equal(t, "try another group", ce2.Hint, "hint-bearing error must pass through unchanged")
}
