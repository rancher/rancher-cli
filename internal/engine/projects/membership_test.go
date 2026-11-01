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
// exercise: Get (fetch the live namespace, counted), Create, and Update
// (recorded for assertion), and dry-run tracking on both. getErr, when
// set, overrides every namespace Get with the given error (used by
// curation tests).
type membershipClient struct {
	namespaces map[string]*unstructured.Unstructured
	getErr     error // if non-nil, Get returns this for all namespace lookups

	updated       *unstructured.Unstructured
	updatedNS     string
	updateDryRuns []bool
	created       *unstructured.Unstructured
	createdNS     string
	createDryRuns []bool
	gets          int
}

func (m *membershipClient) Get(_ context.Context, _ schema.GroupVersionResource, _, name string) (*unstructured.Unstructured, error) {
	m.gets++
	if m.getErr != nil {
		return nil, m.getErr
	}
	if ns, ok := m.namespaces[name]; ok {
		return ns, nil
	}
	return nil, clierr.New(clierr.CodeNotFound, "namespace not found: "+name)
}

func (m *membershipClient) List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error) {
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
		_ = unstructured.SetNestedField(obj.Object, projectID, "metadata", "labels", projectKey)
		_ = unstructured.SetNestedField(obj.Object, "c-x:"+projectID, "metadata", "annotations", projectKey)
	}
	return obj
}

func planMembership(t *testing.T, ctx context.Context, cmd runtime.Command, input runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "membership commands must be MutatingCommand")
	return mc.Plan(ctx, input)
}

// projectCtx carries project id on cluster c-x, as the pipeline resolves a
// typed --project.
func projectCtx(t *testing.T, id string) context.Context {
	t.Helper()
	return runtime.WithProject(t.Context(), runtime.Project{Cluster: "c-x", ID: id, DisplayName: "web"})
}

func TestAddNamespaceSetsLabelAndAnnotation(t *testing.T) {
	t.Parallel()
	client := &membershipClient{
		namespaces: map[string]*unstructured.Unstructured{"team-a": namespaceObj("team-a", "")},
	}
	cmd := NewAddNamespace(stubFactory{c: client})

	eff, exec, err := planMembership(t, projectCtx(t, "p-1"), cmd, in{s: map[string]string{"project": "web", "cluster": "c-x"}, a: []string{"team-a"}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, safety.ActionUpdate, eff.Action)
	assert.Equal(t, "namespaces/team-a", eff.Target)
	assert.False(t, eff.Destructive)

	label, _, _ := unstructured.NestedString(client.updated.Object, "metadata", "labels", projectKey)
	assert.Equal(t, "p-1", label)
	annotation, _, _ := unstructured.NestedString(client.updated.Object, "metadata", "annotations", projectKey)
	assert.Equal(t, "c-x:p-1", annotation)
	require.NotEmpty(t, eff.Diff)

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "namespace/team-a patched\n  added to project p-1\n", res.Text)
}

func TestAddNamespaceToDifferentProjectIsValidation(t *testing.T) {
	t.Parallel()
	client := &membershipClient{
		namespaces: map[string]*unstructured.Unstructured{"team-a": namespaceObj("team-a", "p-2")},
	}
	cmd := NewAddNamespace(stubFactory{c: client})

	_, _, err := planMembership(t, projectCtx(t, "p-1"), cmd, in{s: map[string]string{"project": "web", "cluster": "c-x"}, a: []string{"team-a"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "p-2")
	assert.Equal(t, clierr.Rancher+" project remove-namespace 'team-a' --cluster 'c-x' --dry-run", ce.Hint)
}

// TestAddNamespaceWithoutProjectOnTheContextIsACommandBug asserts that
// add-namespace, whose --project is required, refuses to run when no
// project reached it rather than writing an empty project label.
func TestAddNamespaceWithoutProjectOnTheContextIsACommandBug(t *testing.T) {
	t.Parallel()
	client := &membershipClient{
		namespaces: map[string]*unstructured.Unstructured{"team-a": namespaceObj("team-a", "")},
	}
	cmd := NewAddNamespace(stubFactory{c: client})

	_, _, err := planMembership(t, t.Context(), cmd, in{s: map[string]string{"cluster": "c-x"}, a: []string{"team-a"}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Equal(t, "internal error: project add-namespace ran without --project", ce.Message)
	assert.Nil(t, client.updated, "nothing is previewed")
}

func TestAddNamespaceAlreadyMemberIsEmptyDiff(t *testing.T) {
	t.Parallel()
	client := &membershipClient{
		namespaces: map[string]*unstructured.Unstructured{"team-a": namespaceObj("team-a", "p-1")},
	}
	cmd := NewAddNamespace(stubFactory{c: client})

	eff, _, err := planMembership(t, projectCtx(t, "p-1"), cmd, in{s: map[string]string{"project": "web", "cluster": "c-x"}, a: []string{"team-a"}})
	require.NoError(t, err)
	assert.Empty(t, eff.Diff, "re-adding to the same project is a no-change Effect")
}

// TestRemoveNamespaceClearsLabelAndAnnotation asserts the namespace's own
// project annotation names the project it leaves: the preview shows the
// label and annotation removed and the result line names that project. The
// command never reads a project.
func TestRemoveNamespaceClearsLabelAndAnnotation(t *testing.T) {
	t.Parallel()
	client := &membershipClient{
		namespaces: map[string]*unstructured.Unstructured{"team-a": namespaceObj("team-a", "p-7")},
	}
	cmd := NewRemoveNamespace(stubFactory{c: client})
	ctx := runtime.WithProjectLookup(t.Context(), func(context.Context) (runtime.Project, error) {
		t.Error("the project was read")
		return runtime.Project{}, errors.New("unexpected project read")
	})

	eff, exec, err := planMembership(t, ctx, cmd, in{s: map[string]string{"cluster": "c-x"}, a: []string{"team-a"}})
	require.NoError(t, err)
	assert.Equal(t, safety.ActionUpdate, eff.Action)
	_, found, _ := unstructured.NestedString(client.updated.Object, "metadata", "labels", projectKey)
	assert.False(t, found)
	_, found, _ = unstructured.NestedString(client.updated.Object, "metadata", "annotations", projectKey)
	assert.False(t, found)
	assert.Equal(t, []safety.DiffEntry{
		{Path: "/metadata/annotations/field.cattle.io~1projectId", From: "c-x:p-7"},
		{Path: "/metadata/labels/field.cattle.io~1projectId", From: "p-7"},
	}, eff.Diff, "the preview names the project the label held")

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "namespace/team-a patched\n  removed from project p-7\n", res.Text)
}

// TestRemoveNamespaceInNoProjectIsValidation asserts a namespace with
// neither the project label nor the annotation is refused after its one
// GET, with nothing previewed.
func TestRemoveNamespaceInNoProjectIsValidation(t *testing.T) {
	t.Parallel()
	client := &membershipClient{
		namespaces: map[string]*unstructured.Unstructured{"ns": namespaceObj("ns", "")},
	}
	cmd := NewRemoveNamespace(stubFactory{c: client})

	_, _, err := planMembership(t, t.Context(), cmd, in{s: map[string]string{"cluster": "c-x"}, a: []string{"ns"}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "namespace 'ns' is in no project; there is nothing to remove", ce.Message)
	assert.Equal(t, 1, client.gets, "the namespace GET is the one request")
	assert.Nil(t, client.updated, "nothing is previewed")
}

// namespaceWithProjectLabelWithoutAnnotation is a namespace with the
// project label and no annotation.
func namespaceWithProjectLabelWithoutAnnotation(name, projectID string) *unstructured.Unstructured {
	obj := namespaceObj(name, "")
	_ = unstructured.SetNestedField(obj.Object, projectID, "metadata", "labels", projectKey)
	return obj
}

// TestRemoveNamespaceClearsALabelWithoutAnnotation asserts a namespace with
// the project label and no annotation has its label removed through the
// gated update: the preview carries the note and the removal, the result
// line says the label was removed.
func TestRemoveNamespaceClearsALabelWithoutAnnotation(t *testing.T) {
	t.Parallel()
	client := &membershipClient{
		namespaces: map[string]*unstructured.Unstructured{"team-a": namespaceWithProjectLabelWithoutAnnotation("team-a", "p-7")},
	}
	cmd := NewRemoveNamespace(stubFactory{c: client})

	eff, exec, err := planMembership(t, t.Context(), cmd, in{s: map[string]string{"cluster": "c-x"}, a: []string{"team-a"}})
	require.NoError(t, err)
	assert.Equal(t, safety.ActionUpdate, eff.Action)
	assert.Equal(t, []safety.DiffEntry{{Path: "/metadata/labels/field.cattle.io~1projectId", From: "p-7"}}, eff.Diff)
	assert.Equal(t, []string{"team-a is in no project; removing its project label"}, eff.Warnings)
	_, found, _ := unstructured.NestedString(client.updated.Object, "metadata", "labels", projectKey)
	assert.False(t, found)

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "namespace/team-a project label removed\n", res.Text)
}

// TestRemoveNamespaceKeepsLabelWhileAnAnnotationNamesAProject asserts the
// path for a namespace with only the project label is not taken for a namespace that has the annotation: it
// leaves its project and says which.
func TestRemoveNamespaceKeepsLabelWhileAnAnnotationNamesAProject(t *testing.T) {
	t.Parallel()
	client := &membershipClient{
		namespaces: map[string]*unstructured.Unstructured{"team-a": namespaceObj("team-a", "p-7")},
	}
	eff, exec, err := planMembership(t, t.Context(), NewRemoveNamespace(stubFactory{c: client}), in{s: map[string]string{"cluster": "c-x"}, a: []string{"team-a"}})
	require.NoError(t, err)
	assert.Empty(t, eff.Warnings)
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "namespace/team-a patched\n  removed from project p-7\n", res.Text)
}

// TestRemoveNamespaceAnnotatedForAnotherClusterIsInNoProject asserts a
// namespace whose annotation names another cluster is in no project here:
// it is refused, and its label is not cleared.
func TestRemoveNamespaceAnnotatedForAnotherClusterIsInNoProject(t *testing.T) {
	t.Parallel()
	ns := namespaceObj("team-a", "p-7")
	_ = unstructured.SetNestedField(ns.Object, "c-def:p-7", "metadata", "annotations", projectKey)
	client := &membershipClient{namespaces: map[string]*unstructured.Unstructured{"team-a": ns}}
	_, _, err := planMembership(t, t.Context(), NewRemoveNamespace(stubFactory{c: client}), in{s: map[string]string{"cluster": "c-x"}, a: []string{"team-a"}})
	require.ErrorContains(t, err, "is in no project; there is nothing to remove")
	assert.Nil(t, client.updated)
}

// TestAddNamespaceIgnoresALabelWithoutAnnotation asserts a namespace with a
// project label and no annotation, or an annotation naming another cluster,
// is in no project here: it is added with no conflict.
func TestAddNamespaceIgnoresALabelWithoutAnnotation(t *testing.T) {
	t.Parallel()
	otherCluster := namespaceObj("team-b", "p-2")
	_ = unstructured.SetNestedField(otherCluster.Object, "c-def:p-2", "metadata", "annotations", projectKey)
	for name, ns := range map[string]*unstructured.Unstructured{
		"team-a": namespaceWithProjectLabelWithoutAnnotation("team-a", "p-2"),
		"team-b": otherCluster,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := &membershipClient{namespaces: map[string]*unstructured.Unstructured{name: ns}}
			_, _, err := planMembership(t, projectCtx(t, "p-1"), NewAddNamespace(stubFactory{c: client}),
				in{s: map[string]string{"project": "web", "cluster": "c-x"}, a: []string{name}})
			require.NoError(t, err)
			annotation, _, _ := unstructured.NestedString(client.updated.Object, "metadata", "annotations", projectKey)
			assert.Equal(t, "c-x:p-1", annotation)
		})
	}
}

// TestRemoveNamespaceHelpSaysItRemovesTheProjectLabel asserts the
// command's one-line help.
func TestRemoveNamespaceHelpSaysItRemovesTheProjectLabel(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "Remove a namespace from its project and remove its project label", NewRemoveNamespace(stubFactory{}).Describe().Usage)
}

// TestRemoveNamespaceProjectRefusalHint asserts the command a typed
// --project is refused with: the same removal without --project, as a
// preview, built from the typed namespace and --cluster; no hint when
// either is missing.
func TestRemoveNamespaceProjectRefusalHint(t *testing.T) {
	t.Parallel()
	hinter, ok := NewRemoveNamespace(stubFactory{}).(runtime.ProjectRefusalHinter)
	require.True(t, ok, "remove-namespace must implement runtime.ProjectRefusalHinter")

	tests := []struct {
		name string
		in   in
		want string
	}{
		{"namespace and cluster", in{s: map[string]string{"cluster": "c", "project": "p"}, a: []string{"ns"}},
			clierr.Rancher + " project remove-namespace 'ns' --cluster 'c' --dry-run"},
		{"values are shell-quoted", in{s: map[string]string{"cluster": "my cluster"}, a: []string{"it's"}},
			clierr.Rancher + " project remove-namespace " + clierr.ShellQuote("it's") + " --cluster 'my cluster' --dry-run"},
		{"no namespace", in{s: map[string]string{"cluster": "c"}}, ""},
		{"empty namespace", in{s: map[string]string{"cluster": "c"}, a: []string{""}}, ""},
		{"no cluster", in{a: []string{"ns"}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, hinter.ProjectRefusalHint(tt.in))
		})
	}
}

func TestCreateNamespaceWithProjectCarriesMembershipFromBirth(t *testing.T) {
	t.Parallel()
	client := &membershipClient{}
	cmd := NewCreateNamespace(stubFactory{c: client})

	eff, exec, err := planMembership(t, projectCtx(t, "p-1"), cmd, in{s: map[string]string{"project": "web", "cluster": "c-x"}, a: []string{"team-b"}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, safety.ActionCreate, eff.Action)
	label, _, _ := unstructured.NestedString(client.created.Object, "metadata", "labels", projectKey)
	assert.Equal(t, "p-1", label)
	annotation, _, _ := unstructured.NestedString(client.created.Object, "metadata", "annotations", projectKey)
	assert.Equal(t, "c-x:p-1", annotation)
}

func TestCreateNamespaceWithoutProjectHasNoMembership(t *testing.T) {
	t.Parallel()
	client := &membershipClient{}
	cmd := NewCreateNamespace(stubFactory{c: client})

	_, _, err := planMembership(t, t.Context(), cmd, in{s: map[string]string{"cluster": "c-x"}, a: []string{"team-c"}})
	require.NoError(t, err)
	_, found, _ := unstructured.NestedString(client.created.Object, "metadata", "labels", projectKey)
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
		getErr: kinds.NotFoundCurated(nsKind, "no-ns", "c-x"),
	}
	cmd := NewAddNamespace(stubFactory{c: client})
	_, _, err := planMembership(t, projectCtx(t, "p-1"), cmd, in{s: map[string]string{"project": "web", "cluster": "c-x"}, a: []string{"no-ns"}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, "namespace not found: no-ns", ce.Message)
	assert.Equal(t, clierr.Rancher+" get namespaces --cluster 'c-x'", ce.Hint)

	// hint-bearing 404 passes through unchanged
	hinted := clierr.New(clierr.CodeNotFound, "unsupported group").WithHint("try another group")
	client2 := &membershipClient{
		getErr: hinted,
	}
	cmd2 := NewAddNamespace(stubFactory{c: client2})
	_, _, err2 := planMembership(t, projectCtx(t, "p-1"), cmd2, in{s: map[string]string{"project": "web", "cluster": "c-x"}, a: []string{"no-ns"}})
	var ce2 *clierr.Error
	require.ErrorAs(t, err2, &ce2)
	assert.Equal(t, "try another group", ce2.Hint, "hint-bearing error must pass through unchanged")
}

func (*membershipClient) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	return engine.ReviewResponse{}, errors.New("PostReview not scripted in this fake")
}

func (*membershipClient) ServerResources(context.Context) ([]engine.APIResource, error) {
	return nil, errors.New("ServerResources not scripted in this fake")
}
