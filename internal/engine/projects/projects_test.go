package projects

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type recRepo struct {
	created   *unstructured.Unstructured
	createdNS string
	dryRuns   []bool
}

func (r *recRepo) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (r *recRepo) List(context.Context, schema.GroupVersionResource, string) (*unstructured.UnstructuredList, error) {
	return nil, nil
}
func (r *recRepo) Create(_ context.Context, _ schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	r.created, r.createdNS = obj, ns
	r.dryRuns = append(r.dryRuns, dryRun)
	return obj, nil
}
func (r *recRepo) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (r *recRepo) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}

func factory(r engine.Repo) engine.RepoFactory {
	return func(string) (engine.Repo, error) { return r, nil }
}

type in struct {
	s map[string]string
	a []string
}

func (i in) String(k string) string { return i.s[k] }
func (i in) Bool(string) bool       { return false }
func (i in) Args() []string         { return i.a }

func TestCreateProjectPlanBuildsObjectAndDryRuns(t *testing.T) {
	repo := &recRepo{}
	cmd := projectCmdFor(t, repo)

	eff, err := cmd.Plan(context.Background(), in{s: map[string]string{"cluster": "c-x"}, a: []string{"web"}})
	require.NoError(t, err)
	assert.Equal(t, []bool{true}, repo.dryRuns, "Plan dry-runs")
	assert.Equal(t, "create", eff.Action)
	assert.Equal(t, "projects/web", eff.Target)
	assert.Equal(t, "public", eff.Tier)

	// object shape: management.cattle.io/v3 Project in the cluster-id namespace.
	assert.Equal(t, "c-x", repo.createdNS)
	assert.Equal(t, "web", repo.created.GetName())
	cn, _, _ := unstructured.NestedString(repo.created.Object, "spec", "clusterName")
	assert.Equal(t, "c-x", cn)
	dn, _, _ := unstructured.NestedString(repo.created.Object, "spec", "displayName")
	assert.Equal(t, "web", dn)
	assert.Equal(t, "management.cattle.io/v3", repo.created.GetAPIVersion())
	assert.Equal(t, "Project", repo.created.GetKind())
}

func TestCreateProjectExecuteWrites(t *testing.T) {
	repo := &recRepo{}
	cmd := projectCmdFor(t, repo)
	res, err := cmd.Execute(context.Background(),
		in{s: map[string]string{"cluster": "c-x"}, a: []string{"web"}}, safety.Effect{})
	require.NoError(t, err)
	assert.Equal(t, "Project", res.Kind)
	assert.Equal(t, "public", res.Tier)
	assert.Equal(t, []bool{false}, repo.dryRuns, "Execute writes for real (no dry-run)")
}

func TestCreateProjectRequiresClusterAndName(t *testing.T) {
	repo := &recRepo{}
	cmd := projectCmdFor(t, repo)
	// missing cluster
	_, err := cmd.Plan(context.Background(), in{a: []string{"web"}})
	require.Error(t, err)
	// missing name
	_, err = cmd.Plan(context.Background(), in{s: map[string]string{"cluster": "c-x"}})
	require.Error(t, err)
}

func projectCmdFor(t *testing.T, r engine.Repo) runtime.Command {
	t.Helper()
	return NewCreate(factory(r), "public", "stable")
}
