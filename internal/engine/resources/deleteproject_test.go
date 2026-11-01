package resources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	textKeepWarning = "deleting this project keeps its namespaces and their workloads, except a namespace with the field.cattle.io/creatorId annotation (which Rancher writes on namespaces created through its v3 API): that namespace is deleted with everything in it. Rancher removes the project annotation from each kept namespace and deletes the project's role bindings and project-scoped secrets."
	textFlagWarning = `deleting this project and its namespaces deletes everything in the namespaces marked "will be deleted"; namespaces marked "not deleted" are kept; the project's role bindings and project-scoped secrets are deleted`
	textFlagHelp    = "also delete the project's namespaces and everything in them"
)

// projClient is a scripted cluster client for the project delete: Get
// answers per "namespace/name" key from a queue (the last answer repeats),
// List answers the namespace LIST, Delete and PostReview record into the
// shared log.
type projClient struct {
	log       *[]string
	objs      map[string][]*unstructured.Unstructured
	list      *unstructured.UnstructuredList
	listErr   error
	deleteErr func(ns, name, rv string, dryRun bool) error
	review    engine.ReviewResponse
	reviewErr error
	reviews   int
	selectors []string
	// lastReview is the object of the last PostReview.
	lastReview *unstructured.Unstructured
}

func (c *projClient) record(format string, args ...any) {
	*c.log = append(*c.log, fmt.Sprintf(format, args...))
}

func (c *projClient) Get(_ context.Context, _ schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	key := ns + "/" + name
	c.record("get %s", key)
	queue := c.objs[key]
	if len(queue) == 0 {
		return nil, clierr.New(clierr.CodeNotFound, "not found: "+key)
	}
	obj := queue[0]
	if len(queue) > 1 {
		c.objs[key] = queue[1:]
	}
	return obj.DeepCopy(), nil
}

func (c *projClient) List(_ context.Context, gvr schema.GroupVersionResource, _, selector string) (*unstructured.UnstructuredList, error) {
	c.record("list %s", gvr.Resource)
	c.selectors = append(c.selectors, selector)
	if c.listErr != nil {
		return nil, c.listErr
	}
	return c.list, nil
}

func (c *projClient) Create(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, errors.New("Create not scripted")
}

func (c *projClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, errors.New("Update not scripted")
}

func (c *projClient) Delete(_ context.Context, _ schema.GroupVersionResource, ns, name, rv string, dryRun bool) error {
	verb := "delete"
	if dryRun {
		verb = "dry-run delete"
	}
	c.record("%s %s rv=%s", verb, strings.TrimPrefix(ns+"/"+name, "/"), rv)
	if c.deleteErr != nil {
		return c.deleteErr(ns, name, rv, dryRun)
	}
	return nil
}

func (c *projClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", nil
}

func (c *projClient) PostReview(_ context.Context, req engine.ReviewRequest) (engine.ReviewResponse, error) {
	c.reviews++
	c.record("review %s", req.Kind)
	c.lastReview = req.Object
	return c.review, c.reviewErr
}

func (c *projClient) ServerResources(context.Context) ([]engine.APIResource, error) {
	return nil, errors.New("ServerResources not scripted")
}

// projFactory serves the management (local) client and the downstream one.
type projFactory struct {
	local, down engine.ClusterClient
}

func (f projFactory) Cluster(_ context.Context, id string) (engine.ClusterClient, error) {
	if id == engine.LocalCluster {
		return f.local, nil
	}
	return f.down, nil
}

func (f projFactory) ResolveCluster(_ context.Context, s string) (string, error) { return s, nil }

func (f projFactory) ImpersonatedCluster(ctx context.Context, id string, _ engine.Impersonation) (engine.ClusterClient, error) {
	return f.Cluster(ctx, id)
}

// steveRoots answers steve GETs from canned bodies by path.
type steveRoots struct {
	pages map[string][]byte
	err   error
	paths []string
}

func (r *steveRoots) Root() (engine.RootClient, error) { return r, nil }

func (r *steveRoots) GetJSON(_ context.Context, path string) ([]byte, error) {
	r.paths = append(r.paths, path)
	if r.err != nil {
		return nil, r.err
	}
	body, ok := r.pages[path]
	if !ok {
		return nil, clierr.New(clierr.CodeNotFound, "no page "+path)
	}
	return body, nil
}

func stevePage(t *testing.T, items ...map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"data": items, "count": len(items), "revision": "1"})
	require.NoError(t, err)
	return body
}

// stevePath is the steve namespace collection path of cluster.
func stevePath(cluster string) string { return "/k8s/clusters/" + cluster + "/v1/namespaces" }

// projectFixture is one project delete scenario on cluster c-abc: project
// p-1 at resourceVersion 5, and the namespace LIST the downstream answers.
type projectFixture struct {
	log   []string
	local *projClient
	down  *projClient
	roots *steveRoots
	cmd   runtime.MutatingCommand
}

func deleteTestProject(rv, usedLimit string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "Project",
		"metadata": map[string]any{"name": "p-1", "namespace": "c-abc", "resourceVersion": rv, "generation": int64(1)},
		"spec": map[string]any{
			"clusterName": "c-abc", "displayName": "web",
			"resourceQuota": map[string]any{"limit": map[string]any{"pods": "10"}, "usedLimit": map[string]any{"pods": usedLimit}},
		},
		"status": map[string]any{"backingNamespace": "c-abc-p-1"},
	}}
	return obj
}

type nsOption func(*unstructured.Unstructured)

func creator(ns *unstructured.Unstructured) {
	annotations := ns.GetAnnotations()
	annotations["field.cattle.io/creatorId"] = "u-abc"
	ns.SetAnnotations(annotations)
}

func terminating(ns *unstructured.Unstructured) {
	_ = unstructured.SetNestedField(ns.Object, "Terminating", "status", "phase")
	now := metav1.Now()
	ns.SetDeletionTimestamp(&now)
}

// terminatingByPhase marks the namespace Terminating by status.phase only.
func terminatingByPhase(ns *unstructured.Unstructured) {
	_ = unstructured.SetNestedField(ns.Object, "Terminating", "status", "phase")
}

// terminatingByTimestamp gives the namespace a deletionTimestamp and leaves
// its phase Active.
func terminatingByTimestamp(ns *unstructured.Unstructured) {
	now := metav1.Now()
	ns.SetDeletionTimestamp(&now)
}

func inProject(project string) nsOption {
	return func(ns *unstructured.Unstructured) {
		ns.SetAnnotations(map[string]string{"field.cattle.io/projectId": "c-abc:" + project})
		ns.SetLabels(map[string]string{"kubernetes.io/metadata.name": ns.GetName(), "field.cattle.io/projectId": project})
	}
}

// member is a namespace of project p-1 on c-abc as the Kubernetes API
// returns it.
func member(name, rv string, opts ...nsOption) unstructured.Unstructured {
	ns := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": name, "resourceVersion": rv},
		"status":   map[string]any{"phase": "Active"},
	}}
	inProject("p-1")(&ns)
	for _, o := range opts {
		o(&ns)
	}
	return ns
}

func newProjectFixture(t *testing.T, items ...unstructured.Unstructured) *projectFixture {
	t.Helper()
	f := &projectFixture{}
	f.local = &projClient{log: &f.log, objs: map[string][]*unstructured.Unstructured{
		"c-abc/p-1": {deleteTestProject("5", "1")},
	}}
	f.down = &projClient{log: &f.log, list: &unstructured.UnstructuredList{Items: items},
		review: engine.ReviewResponse{Status: 201, Object: &unstructured.Unstructured{Object: map[string]any{
			"status": map[string]any{"allowed": true},
		}}},
	}
	f.roots = &steveRoots{pages: map[string][]byte{}}
	f.cmd = NewDeleteProject(mustKind(t, "project"), projectFactory(f), f.roots).(runtime.MutatingCommand)
	return f
}

func projectFactory(f *projectFixture) projFactory { return projFactory{local: f.local, down: f.down} }

func (f *projectFixture) plan(t *testing.T, withFlag bool) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	return f.cmd.Plan(t.Context(), input{
		args: []string{"p-1"},
		s:    map[string]string{"cluster": "c-abc"},
		b:    map[string]bool{FlagDeleteNamespaces: withFlag},
	})
}

func (f *projectFixture) mustPlan(t *testing.T, withFlag bool) (safety.Effect, runtime.ExecuteFunc) {
	t.Helper()
	eff, exec, err := f.plan(t, withFlag)
	require.NoError(t, err)
	require.NotNil(t, exec)
	return eff, exec
}

func objectsByName(objs []safety.Object) map[string]string {
	out := map[string]string{}
	for _, o := range objs {
		out[o.Kind+"/"+o.Name] = o.Outcome
	}
	return out
}

// resultData decodes a Result's data the way the JSON envelope carries it.
func resultData(t *testing.T, res runtime.Result) (deleted string, objects []safety.Object) {
	t.Helper()
	raw, err := json.Marshal(res.Data)
	require.NoError(t, err)
	var data struct {
		Deleted string          `json:"deleted"`
		Objects []safety.Object `json:"objects"`
	}
	require.NoError(t, json.Unmarshal(raw, &data))
	return data.Deleted, data.Objects
}

func detailsOf(t *testing.T, err error) DeleteProjectPartialFailure {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "a clierr error: %v", err)
	details, ok := ce.Details.(DeleteProjectPartialFailure)
	require.True(t, ok, "partial failure details: %#v", ce.Details)
	return details
}

func TestDeleteProjectDescribesTheFlag(t *testing.T) {
	t.Parallel()
	d := NewDeleteProject(mustKind(t, "project"), nil, nil).Describe()
	assert.Equal(t, "delete project", d.Name)
	var found bool
	for _, f := range d.Flags {
		if f.Name == FlagDeleteNamespaces {
			found = true
			assert.Equal(t, "bool", f.Type)
			assert.Equal(t, textFlagHelp, f.Desc)
		}
	}
	assert.True(t, found, "the descriptor lists --delete-namespaces")
}

func TestDeleteProjectKeepsTheGenericRowWarning(t *testing.T) {
	t.Parallel()
	assert.Equal(t, textKeepWarning, mustKind(t, "project").DeleteCascadeWarning)
}

// TestDeleteProjectPreviewClassifiesTheNamespaces asserts a run without the
// flag reads the members by their annotation and lists what Rancher does with
// each: deletes the one with creatorId, keeps the others, and a namespace
// already terminating is being deleted. The row warning is the only
// warning, and no namespace is deleted.
func TestDeleteProjectPreviewClassifiesTheNamespaces(t *testing.T) {
	t.Parallel()
	withProjectLabel := unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{
		"name": "with-project-label", "resourceVersion": "9", "labels": map[string]any{"field.cattle.io/projectId": "p-1"}}}}
	f := newProjectFixture(t,
		member("legacy", "2", creator),
		member("apps", "3"),
		member("old", "4", terminating),
		withProjectLabel,
	)

	eff, exec := f.mustPlan(t, false)
	assert.Equal(t, map[string]string{
		"namespace/apps":   safety.OutcomeRancherKeeps,
		"namespace/legacy": safety.OutcomeRancherDeletes,
		"namespace/old":    safety.OutcomeBeingDeleted,
	}, objectsByName(eff.Objects), "the namespace with only the project label is not a member")
	assert.Equal(t, []string{textKeepWarning}, eff.Warnings)
	assert.Equal(t, []string{"field.cattle.io/projectId=p-1"}, f.down.selectors, "the LIST is narrowed by the project label")
	assert.Equal(t, []string{
		"namespaces:",
		"  rancher keeps: apps",
		"  rancher deletes: legacy",
		"  being deleted: old",
	}, runtime.ObjectLines(eff.Objects, "project"))
	assert.Equal(t, []string{"get c-abc/p-1", "dry-run delete c-abc/p-1 rv=5", "list namespaces", "review selfSubjectAccess"}, f.log,
		"project GET, project dry-run delete, namespace LIST and the visibility review")

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "ProjectDelete", res.Kind)
	assert.Equal(t, "project/p-1 deleted\n  namespaces:\n    rancher keeps: apps\n    rancher deletes: legacy\n    being deleted: old\n", res.Text)
	deleted, objects := resultData(t, res)
	assert.Equal(t, "p-1", deleted)
	assert.Equal(t, eff.Objects, objects)
	for _, call := range f.log {
		if strings.HasPrefix(call, "delete ") {
			assert.Equal(t, "delete c-abc/p-1 rv=5", call, "only the project is deleted, with its resourceVersion")
		}
	}
	assert.Equal(t, "delete c-abc/p-1 rv=5", f.log[len(f.log)-1])
}

// TestDeleteProjectPreviewListsAtMostFiveNamesPerOutcome asserts the text
// lines show five names and a count of the rest.
func TestDeleteProjectPreviewListsAtMostFiveNamesPerOutcome(t *testing.T) {
	t.Parallel()
	var items []unstructured.Unstructured
	for _, n := range []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8"} {
		items = append(items, member(n, "1"))
	}
	f := newProjectFixture(t, items...)
	eff, _ := f.mustPlan(t, false)
	assert.Equal(t, []string{
		"namespaces:",
		"  rancher keeps: a1, a2, a3, a4, a5 and 3 more",
	}, runtime.ObjectLines(eff.Objects, "project"))
	assert.Len(t, eff.Objects, 8, "JSON carries every object")
}

// TestDeleteProjectPreviewSteveItemWithoutGetIsUnknown asserts that when the
// LIST is refused and steve answers, a namespace steve serves without get (no
// creatorId visible) is unknown, a full one is classified, and a namespace
// of another project is left out.
func TestDeleteProjectPreviewSteveItemWithoutGetIsUnknown(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t)
	f.down.listErr = clierr.New(clierr.CodeRBAC, "namespaces is forbidden")
	f.roots.pages[stevePath("c-abc")] = stevePage(t,
		map[string]any{"id": "without-get", "metadata": map[string]any{"name": "without-get", "labels": map[string]any{},
			"annotations": map[string]any{"field.cattle.io/projectId": "c-abc:p-1"}}},
		map[string]any{"id": "full", "metadata": map[string]any{"name": "full", "labels": map[string]any{"kubernetes.io/metadata.name": "full"},
			"annotations": map[string]any{"field.cattle.io/projectId": "c-abc:p-1", "field.cattle.io/creatorId": "u-abc"}}},
		map[string]any{"id": "other", "metadata": map[string]any{"name": "other", "labels": map[string]any{"kubernetes.io/metadata.name": "other"},
			"annotations": map[string]any{"field.cattle.io/projectId": "c-abc:p-2"}}},
	)
	eff, _ := f.mustPlan(t, false)
	assert.Equal(t, map[string]string{
		"namespace/full":        safety.OutcomeRancherDeletes,
		"namespace/without-get": safety.OutcomeUnknown,
	}, objectsByName(eff.Objects))
	assert.Equal(t, []string{stevePath("c-abc")}, f.roots.paths)
}

// TestDeleteProjectProtectedNamesAreKeptInAPlainRun asserts a namespace
// nobody may delete is listed as kept even when it carries the creatorId
// annotation, while any other namespace with it is deleted.
func TestDeleteProjectProtectedNamesAreKeptInAPlainRun(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t,
		member("default", "1", creator), member("kube-system", "2", creator), member("kube-public", "3", creator),
		member("legacy", "4", creator),
	)
	eff, _ := f.mustPlan(t, false)
	assert.Equal(t, map[string]string{
		"namespace/default":     safety.OutcomeRancherKeeps,
		"namespace/kube-public": safety.OutcomeRancherKeeps,
		"namespace/kube-system": safety.OutcomeRancherKeeps,
		"namespace/legacy":      safety.OutcomeRancherDeletes,
	}, objectsByName(eff.Objects))
	assert.Equal(t, 1, f.down.reviews, "a kept namespace is what the visibility review is for")
}

// TestDeleteProjectTerminatingNamespaceIsBeingDeleted asserts a namespace is
// being deleted by status.phase alone, by deletionTimestamp alone, or by the
// phase of an item steve serves without get (which has no
// kubernetes.io/metadata.name label), in a plain run and in a run with the
// flag, where it is also left alone.
func TestDeleteProjectTerminatingNamespaceIsBeingDeleted(t *testing.T) {
	t.Parallel()
	steveItem := map[string]any{"id": "ending", "metadata": map[string]any{"name": "ending", "labels": map[string]any{},
		"annotations": map[string]any{"field.cattle.io/projectId": "c-abc:p-1"}},
		"status": map[string]any{"phase": "Terminating"}}
	rows := map[string]func(t *testing.T) *projectFixture{
		"phase only": func(t *testing.T) *projectFixture {
			t.Helper()
			return newProjectFixture(t, member("ending", "3", terminatingByPhase))
		},
		"deletion timestamp only": func(t *testing.T) *projectFixture {
			t.Helper()
			return newProjectFixture(t, member("ending", "3", terminatingByTimestamp))
		},
		"steve item without get": func(t *testing.T) *projectFixture {
			t.Helper()
			f := newProjectFixture(t)
			f.down.listErr = clierr.New(clierr.CodeRBAC, "namespaces is forbidden")
			f.roots.pages[stevePath("c-abc")] = stevePage(t, steveItem)
			return f
		},
	}
	for name, setup := range rows {
		for _, withFlag := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s flag=%v", name, withFlag), func(t *testing.T) {
				t.Parallel()
				f := setup(t)
				eff, _ := f.mustPlan(t, withFlag)
				assert.Equal(t, safety.OutcomeBeingDeleted, objectsByName(eff.Objects)["namespace/ending"])
				for _, call := range f.log {
					assert.NotContains(t, call, "delete ending", "a namespace being deleted is not deleted again")
				}
			})
		}
	}
}

// TestDeleteProjectPreviewFailedReadWarnsAndDeletes asserts a failed
// namespace read is a warning with the reason, the delete still runs, and
// the result lists no namespace.
func TestDeleteProjectPreviewFailedReadWarnsAndDeletes(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t)
	f.down.listErr = clierr.New(clierr.CodeGeneric, "the proxy failed")

	eff, exec := f.mustPlan(t, false)
	assert.Equal(t, []string{
		textKeepWarning,
		"could not read the project's namespaces: the proxy failed; Rancher deletes those with the field.cattle.io/creatorId annotation and keeps the rest",
	}, eff.Warnings)
	assert.Empty(t, eff.Objects)
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "project/p-1 deleted\n", res.Text)
	_, objects := resultData(t, res)
	assert.NotNil(t, objects, "the objects key is an empty list")
	assert.Equal(t, "delete c-abc/p-1 rv=5", f.log[len(f.log)-1])
}

// TestDeleteProjectPreviewWarnsWhenTheCallerCannotSeeKeptNamespaces asserts
// the visibility warning appears only when the access review denies, the
// read succeeded and a namespace is kept.
func TestDeleteProjectPreviewWarnsWhenTheCallerCannotSeeKeptNamespaces(t *testing.T) {
	t.Parallel()
	denied := engine.ReviewResponse{Status: 201, Object: &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"allowed": false}}}}
	allowed := engine.ReviewResponse{Status: 201, Object: &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"allowed": true}}}}
	const warning = "you cannot get namespaces outside projects; after this delete you will not be able to get the namespaces Rancher keeps: apps, tools"
	tests := []struct {
		name      string
		review    engine.ReviewResponse
		reviewErr error
		items     []unstructured.Unstructured
		listErr   error
		wantWarn  bool
		wantAsked bool
	}{
		{"denied", denied, nil, []unstructured.Unstructured{member("apps", "1"), member("tools", "1"), member("legacy", "1", creator)}, nil, true, true},
		{"allowed", allowed, nil, []unstructured.Unstructured{member("apps", "1"), member("tools", "1")}, nil, false, true},
		{"request failed", engine.ReviewResponse{}, errors.New("transport"), []unstructured.Unstructured{member("apps", "1"), member("tools", "1")}, nil, false, true},
		{"refused with a status", engine.ReviewResponse{Status: 403}, nil, []unstructured.Unstructured{member("apps", "1"), member("tools", "1")}, nil, false, true},
		{"nothing kept", denied, nil, []unstructured.Unstructured{member("legacy", "1", creator)}, nil, false, false},
		{"read failed", denied, nil, nil, clierr.New(clierr.CodeGeneric, "boom"), false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newProjectFixture(t, tt.items...)
			f.down.review, f.down.reviewErr, f.down.listErr = tt.review, tt.reviewErr, tt.listErr
			eff, _ := f.mustPlan(t, false)
			if tt.wantWarn {
				assert.Contains(t, eff.Warnings, warning)
			} else {
				for _, w := range eff.Warnings {
					assert.NotContains(t, w, "you cannot get namespaces")
				}
			}
			assert.Equal(t, tt.wantAsked, f.down.reviews == 1)
			if tt.wantAsked {
				spec, _, _ := unstructured.NestedMap(f.down.lastReview.Object, "spec", "resourceAttributes")
				assert.Equal(t, map[string]any{"verb": "get", "group": "", "resource": "namespaces"}, spec,
					"the review asks about get on namespaces with no name and no namespace")
			}
		})
	}
}

// TestDeleteProjectFlagListsAndDryRunsEveryNamespace asserts the flag's
// preview: the namespaces the CLI will delete, the protected ones kept with
// their reason, terminating ones listed and untouched, the project last, the
// flag's warning in place of the row's, and one dry-run delete per deletable
// namespace carrying that namespace's resourceVersion.
func TestDeleteProjectFlagListsAndDryRunsEveryNamespace(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t,
		member("tools", "12"), member("apps", "11"), member("default", "1"), member("kube-system", "2"),
		member("kube-public", "3"), member("old", "4", terminating), member("legacy", "13", creator),
	)

	eff, _ := f.mustPlan(t, true)
	assert.Equal(t, []string{textFlagWarning}, eff.Warnings)
	assert.Equal(t, []safety.Object{
		{Kind: "namespace", Name: "apps", Outcome: "will be deleted"},
		{Kind: "namespace", Name: "default", Outcome: "not deleted", Message: "Kubernetes never deletes it"},
		{Kind: "namespace", Name: "kube-public", Outcome: "not deleted", Message: "Kubernetes never deletes it"},
		{Kind: "namespace", Name: "kube-system", Outcome: "not deleted", Message: "Kubernetes never deletes it"},
		{Kind: "namespace", Name: "legacy", Outcome: "will be deleted"},
		{Kind: "namespace", Name: "old", Outcome: "being deleted"},
		{Kind: "namespace", Name: "tools", Outcome: "will be deleted"},
		{Kind: "project", Name: "p-1", Outcome: "will be deleted"},
	}, eff.Objects)
	assert.Equal(t, []string{
		"namespaces:",
		"  will be deleted: apps, legacy, tools",
		"  not deleted: default (Kubernetes never deletes it), kube-public (Kubernetes never deletes it), kube-system (Kubernetes never deletes it)",
		"  being deleted: old",
	}, runtime.ObjectLines(eff.Objects, "project"))
	assert.Equal(t, []string{
		"get c-abc/p-1", "dry-run delete c-abc/p-1 rv=5", "list namespaces",
		"dry-run delete apps rv=11", "dry-run delete legacy rv=13", "dry-run delete tools rv=12",
	}, f.log, "no review, and no namespace is written before the apply")
	assert.Equal(t, "5", eff.ResourceVersion)
	assert.True(t, eff.ConflictDetection)
	assert.True(t, eff.Destructive)
	assert.Equal(t, "projects/p-1", eff.Target)
}

// TestDeleteProjectFlagProtectedNamespacesOnTheLocalCluster asserts local
// and fleet-local are skipped only on the local cluster, with their own
// reason, and deleted on a downstream cluster.
func TestDeleteProjectFlagProtectedNamespacesOnTheLocalCluster(t *testing.T) {
	t.Parallel()
	t.Run("local cluster", func(t *testing.T) {
		t.Parallel()
		var log []string
		client := &projClient{log: &log,
			objs: map[string][]*unstructured.Unstructured{"local/p-1": {deleteTestProject("5", "1")}},
			list: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
				localMember("local"), localMember("fleet-local"), localMember("apps"),
			}},
		}
		cmd := NewDeleteProject(mustKind(t, "project"), projFactory{local: client, down: client}, &steveRoots{}).(runtime.MutatingCommand)
		eff, _, err := cmd.Plan(t.Context(), input{args: []string{"p-1"}, s: map[string]string{"cluster": "local"}, b: map[string]bool{FlagDeleteNamespaces: true}})
		require.NoError(t, err)
		assert.Equal(t, []safety.Object{
			{Kind: "namespace", Name: "apps", Outcome: "will be deleted"},
			{Kind: "namespace", Name: "fleet-local", Outcome: "not deleted", Message: "Rancher never deletes it on the local cluster"},
			{Kind: "namespace", Name: "local", Outcome: "not deleted", Message: "Rancher never deletes it on the local cluster"},
			{Kind: "project", Name: "p-1", Outcome: "will be deleted"},
		}, eff.Objects)
	})
	t.Run("downstream cluster", func(t *testing.T) {
		t.Parallel()
		f := newProjectFixture(t, member("local", "1"), member("fleet-local", "2"))
		eff, _ := f.mustPlan(t, true)
		assert.Equal(t, map[string]string{
			"namespace/fleet-local": "will be deleted", "namespace/local": "will be deleted", "project/p-1": "will be deleted",
		}, objectsByName(eff.Objects))
	})
}

// localMember is a namespace of project p-1 on the local cluster.
func localMember(name string) unstructured.Unstructured {
	ns := member(name, "1")
	ns.SetAnnotations(map[string]string{"field.cattle.io/projectId": "local:p-1"})
	return ns
}

// TestDeleteProjectFlagLeavesANamespaceWithProjectLabelWithoutAnnotationAlone
// asserts a namespace with the project label and no annotation is not a
// member: it is neither listed nor deleted.
func TestDeleteProjectFlagLeavesANamespaceWithProjectLabelWithoutAnnotationAlone(t *testing.T) {
	t.Parallel()
	withProjectLabel := unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{
		"name": "with-project-label", "resourceVersion": "9", "labels": map[string]any{"field.cattle.io/projectId": "p-1"}}}}
	f := newProjectFixture(t, member("apps", "1"), withProjectLabel)
	eff, exec := f.mustPlan(t, true)
	assert.Equal(t, map[string]string{"namespace/apps": "will be deleted", "project/p-1": "will be deleted"}, objectsByName(eff.Objects))
	_, err := exec(t.Context())
	require.NoError(t, err)
	for _, call := range f.log {
		assert.NotContains(t, call, "with-project-label")
	}
}

// TestDeleteProjectFlagRefusesAnUnknownNamespaceBeforeAnyWrite asserts a
// namespace the caller cannot read in full refuses the command with the
// concrete ids in the hint, and nothing is dry-run or deleted.
func TestDeleteProjectFlagRefusesAnUnknownNamespaceBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t)
	f.down.listErr = clierr.New(clierr.CodeRBAC, "namespaces is forbidden")
	f.roots.pages[stevePath("c-abc")] = stevePage(t,
		map[string]any{"id": "tools", "metadata": map[string]any{"name": "tools", "labels": map[string]any{"kubernetes.io/metadata.name": "tools"},
			"annotations": map[string]any{"field.cattle.io/projectId": "c-abc:p-1"}}},
		map[string]any{"id": "hidden", "metadata": map[string]any{"name": "hidden", "labels": map[string]any{},
			"annotations": map[string]any{"field.cattle.io/projectId": "c-abc:p-1"}}},
	)
	_, _, err := f.plan(t, true)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "cannot read namespace hidden in full; --delete-namespaces needs to read each namespace it deletes in full; the hint previews deleting the project without its namespaces: add --apply --yes after reading the preview", ce.Message)
	assert.Equal(t, clierr.Rancher+" delete project 'p-1' --cluster 'c-abc' --dry-run", ce.Hint)
	for _, call := range f.log {
		assert.False(t, strings.HasPrefix(call, "dry-run delete hidden") || strings.HasPrefix(call, "dry-run delete tools"), call)
	}
}

// TestDeleteProjectFlagRefusesAFailedRead asserts a failed namespace read
// refuses the command with the failing call's code and the reason, before
// any namespace is written.
func TestDeleteProjectFlagRefusesAFailedRead(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t)
	f.down.listErr = clierr.New(clierr.CodeAuth, "the token expired")
	_, _, err := f.plan(t, true)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeAuth, ce.Code)
	assert.Equal(t, "could not read the project's namespaces: the token expired; --delete-namespaces needs the full list before it deletes anything; the hint previews deleting the project without its namespaces: add --apply --yes after reading the preview", ce.Message)
	assert.Equal(t, clierr.Rancher+" delete project 'p-1' --cluster 'c-abc' --dry-run", ce.Hint)
	assert.Equal(t, []string{"get c-abc/p-1", "dry-run delete c-abc/p-1 rv=5", "list namespaces"}, f.log)
}

// TestDeleteProjectFlagRefusedNamespaceDryRunRefusesBeforeAnyWrite asserts a
// namespace delete the server refuses at the preview refuses the command with
// the server's message and code, and the executor does not exist.
func TestDeleteProjectFlagRefusedNamespaceDryRunRefusesBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t, member("apps", "1"), member("tools", "2"))
	f.down.deleteErr = func(_, name, _ string, dryRun bool) error {
		if dryRun && name == "tools" {
			return clierr.New(clierr.CodeRBAC, "admission webhook \"validator\" denied the request")
		}
		return nil
	}
	eff, exec, err := f.plan(t, true)
	require.Error(t, err)
	assert.Nil(t, exec)
	assert.Empty(t, eff.Objects)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, "deleting namespace tools was refused: admission webhook \"validator\" denied the request; nothing was deleted", ce.Message)
	for _, call := range f.log {
		assert.False(t, strings.HasPrefix(call, "delete "), "nothing is deleted: %s", call)
	}
}

// TestDeleteProjectFlagDeletesNamespacesInNameOrderThenTheProject asserts
// the executor deletes the namespaces one at a time sorted by name, each
// with its own resourceVersion, treats NOT_FOUND as already gone, and
// deletes the project last with its resourceVersion.
func TestDeleteProjectFlagDeletesNamespacesInNameOrderThenTheProject(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t,
		member("tools", "12"), member("apps", "11"), member("default", "1"), member("old", "4", terminating), member("vanished", "14"),
	)
	f.down.deleteErr = func(_, name, _ string, dryRun bool) error {
		if !dryRun && name == "vanished" {
			return clierr.New(clierr.CodeNotFound, "namespaces \"vanished\" not found")
		}
		return nil
	}
	eff, exec := f.mustPlan(t, true)
	planned := len(f.log)

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{
		"delete apps rv=11", "delete tools rv=12", "delete vanished rv=14", "delete c-abc/p-1 rv=5",
	}, f.log[planned:], "name order, each with its resourceVersion, the project last")

	assert.Equal(t, "ProjectDelete", res.Kind)
	assert.Equal(t, "project/p-1 deleted\n  namespaces:\n    not deleted: default (Kubernetes never deletes it)\n    being deleted: old\n    deletion started: apps, tools\n    already gone: vanished\n", res.Text)
	deleted, objects := resultData(t, res)
	assert.Equal(t, "p-1", deleted)
	assert.Equal(t, []safety.Object{
		{Kind: "namespace", Name: "apps", Outcome: "deletion started"},
		{Kind: "namespace", Name: "default", Outcome: "not deleted", Message: "Kubernetes never deletes it"},
		{Kind: "namespace", Name: "old", Outcome: "being deleted"},
		{Kind: "namespace", Name: "tools", Outcome: "deletion started"},
		{Kind: "namespace", Name: "vanished", Outcome: "already gone"},
		{Kind: "project", Name: "p-1", Outcome: "deletion started"},
	}, objects)
	assert.Equal(t, "will be deleted", eff.Objects[len(eff.Objects)-1].Outcome, "the planned list is not changed by the run")
}

// TestDeleteProjectFlagStopsAtTheFirstFailedNamespace asserts a failed
// namespace delete stops the run, keeps the project, reports every object
// and carries the failing call's code and message.
func TestDeleteProjectFlagStopsAtTheFirstFailedNamespace(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t, member("apps", "1"), member("tools", "2"), member("zeta", "3"), member("old", "4", terminating))
	f.down.deleteErr = func(_, name, _ string, dryRun bool) error {
		if !dryRun && name == "tools" {
			return clierr.New(clierr.CodeRBAC, "namespaces \"tools\" is forbidden")
		}
		return nil
	}
	_, exec := f.mustPlan(t, true)
	planned := len(f.log)
	_, err := exec(t.Context())
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Empty(t, ce.Hint)
	assert.Equal(t, "delete project partially applied: deletion started for 1 namespaces, failed on tools, 1 not attempted; the project was kept; re-run the same command; it reads the project's namespaces again", ce.Message)
	assert.Equal(t, []safety.Object{
		{Kind: "namespace", Name: "apps", Outcome: "deletion started"},
		{Kind: "namespace", Name: "old", Outcome: "being deleted"},
		{Kind: "namespace", Name: "tools", Outcome: "failed", Code: clierr.CodeRBAC, Message: "namespaces \"tools\" is forbidden"},
		{Kind: "namespace", Name: "zeta", Outcome: "not attempted"},
		{Kind: "project", Name: "p-1", Outcome: "not attempted"},
	}, detailsOf(t, err).Objects)
	assert.Equal(t, []string{"delete apps rv=1", "delete tools rv=2"}, f.log[planned:], "no further namespace and not the project")
	assert.Equal(t, []string{
		"namespaces:",
		"  being deleted: old",
		"  deletion started: apps",
		"  failed: tools (namespaces \"tools\" is forbidden)",
		"  not attempted: zeta",
		"projects:",
		"  not attempted: p-1",
	}, detailsOf(t, err).TextDetails())
}

// TestDeleteProjectFlagFailureCounts asserts the counts in the partial
// failure message: a first namespace that fails starts none, and a namespace
// that was already gone before the failure counts as neither started nor not
// attempted. The error has no hint.
func TestDeleteProjectFlagFailureCounts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		gone    string
		failing string
		message string
		want    []safety.Object
	}{
		{
			name: "the first namespace fails", failing: "apps",
			message: "delete project partially applied: deletion started for 0 namespaces, failed on apps, 2 not attempted; the project was kept; re-run the same command; it reads the project's namespaces again",
			want: []safety.Object{
				{Kind: "namespace", Name: "apps", Outcome: "failed", Code: clierr.CodeRBAC, Message: "forbidden"},
				{Kind: "namespace", Name: "tools", Outcome: "not attempted"},
				{Kind: "namespace", Name: "zeta", Outcome: "not attempted"},
				{Kind: "project", Name: "p-1", Outcome: "not attempted"},
			},
		},
		{
			name: "an already gone namespace before the failure", gone: "apps", failing: "tools",
			message: "delete project partially applied: deletion started for 0 namespaces, failed on tools, 1 not attempted; the project was kept; re-run the same command; it reads the project's namespaces again",
			want: []safety.Object{
				{Kind: "namespace", Name: "apps", Outcome: "already gone"},
				{Kind: "namespace", Name: "tools", Outcome: "failed", Code: clierr.CodeRBAC, Message: "forbidden"},
				{Kind: "namespace", Name: "zeta", Outcome: "not attempted"},
				{Kind: "project", Name: "p-1", Outcome: "not attempted"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newProjectFixture(t, member("apps", "1"), member("tools", "2"), member("zeta", "3"))
			f.down.deleteErr = func(_, name, _ string, dryRun bool) error {
				switch {
				case dryRun:
					return nil
				case name == tt.gone:
					return clierr.New(clierr.CodeNotFound, "not found")
				case name == tt.failing:
					return clierr.New(clierr.CodeRBAC, "forbidden")
				}
				return nil
			}
			_, exec := f.mustPlan(t, true)
			_, err := exec(t.Context())
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, tt.message, ce.Message)
			assert.Empty(t, ce.Hint)
			assert.Equal(t, tt.want, detailsOf(t, err).Objects)
		})
	}
}

// withProjectAnswers sets the project GET answers: the first is the Plan
// read, the next ones the re-reads after a conflict.
func (f *projectFixture) withProjectAnswers(answers ...*unstructured.Unstructured) {
	f.local.objs["c-abc/p-1"] = answers
}

func conflictOnProject(rvs ...string) func(ns, name, rv string, dryRun bool) error {
	return func(ns, name, rv string, dryRun bool) error {
		if dryRun || name != "p-1" {
			return nil
		}
		if slices.Contains(rvs, rv) {
			return clierr.Retryable(clierr.CodeConflict, "the resource changed on the server since it was last read; retry the command")
		}
		return nil
	}
}

// TestDeleteProjectFlagRetriesAConflictThatChangedOnlyTheUsedLimit asserts
// the project delete after its namespaces meets a 409 caused by Rancher
// updating the used quota: it re-reads the project and deletes with the new
// resourceVersion.
func TestDeleteProjectFlagRetriesAConflictThatChangedOnlyTheUsedLimit(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t, member("apps", "1"))
	moved := deleteTestProject("7", "0")
	_ = unstructured.SetNestedField(moved.Object, "Active", "status", "phase")
	moved.SetGeneration(2)
	f.withProjectAnswers(deleteTestProject("5", "1"), moved)
	f.local.deleteErr = conflictOnProject("5")
	_, exec := f.mustPlan(t, true)
	planned := len(f.log)

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{
		"delete c-abc/p-1 rv=5", "get c-abc/p-1", "delete c-abc/p-1 rv=7",
	}, f.log[planned+1:])
	_, objects := resultData(t, res)
	assert.Equal(t, safety.Object{Kind: "project", Name: "p-1", Outcome: "deletion started"}, objects[len(objects)-1])
}

// TestDeleteProjectFlagProjectChangedIsAConflict asserts a conflict after a
// change to the project's spec or labels keeps the project, reports the
// namespaces and is CONFLICT with the approved message.
func TestDeleteProjectFlagProjectChangedIsAConflict(t *testing.T) {
	t.Parallel()
	changes := map[string]func(*unstructured.Unstructured){
		"a spec field": func(p *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(p.Object, "11", "spec", "resourceQuota", "limit", "pods")
		},
		"a label": func(p *unstructured.Unstructured) { p.SetLabels(map[string]string{"team": "ops"}) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newProjectFixture(t, member("apps", "1"))
			changed := deleteTestProject("7", "1")
			change(changed)
			f.withProjectAnswers(deleteTestProject("5", "1"), changed)
			f.local.deleteErr = conflictOnProject("5")
			_, exec := f.mustPlan(t, true)
			planned := len(f.log)

			_, err := exec(t.Context())
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeConflict, ce.Code)
			assert.Equal(t, "project p-1 changed after it was read; its namespaces are being deleted and the project was kept; re-run the same command", ce.Message)
			objs := detailsOf(t, err).Objects
			assert.Equal(t, "deletion started", objectsByName(objs)["namespace/apps"])
			assert.Equal(t, "failed", objectsByName(objs)["project/p-1"])
			assert.Equal(t, []string{"delete apps rv=1", "delete c-abc/p-1 rv=5", "get c-abc/p-1"}, f.log[planned:], "the project is not deleted with the changed object")
		})
	}
}

// TestProjectSpecChanged asserts which differences between the project as
// read and the project after a conflict count as a change: the status, the
// used quota and the bookkeeping the server rewrites on every update do not;
// a label, an annotation, a finalizer, a deletion timestamp and a spec field
// do.
func TestProjectSpecChanged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(*unstructured.Unstructured)
		want   bool
	}{
		{"nothing", func(*unstructured.Unstructured) {}, false},
		{"managedFields only", func(p *unstructured.Unstructured) {
			p.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "rancher", Operation: metav1.ManagedFieldsOperationUpdate}})
		}, false},
		{"resourceVersion only", func(p *unstructured.Unstructured) { p.SetResourceVersion("99") }, false},
		{"generation only", func(p *unstructured.Unstructured) { p.SetGeneration(7) }, false},
		{"status only", func(p *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(p.Object, "other", "status", "backingNamespace")
		}, false},
		{"usedLimit only", func(p *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(p.Object, "9", "spec", "resourceQuota", "usedLimit", "pods")
		}, false},
		{"a label", func(p *unstructured.Unstructured) { p.SetLabels(map[string]string{"team": "ops"}) }, true},
		{"an annotation", func(p *unstructured.Unstructured) { p.SetAnnotations(map[string]string{"note": "x"}) }, true},
		{"a finalizer", func(p *unstructured.Unstructured) { p.SetFinalizers([]string{"controller.cattle.io/project"}) }, true},
		{"the deletion timestamp", func(p *unstructured.Unstructured) {
			now := metav1.Now()
			p.SetDeletionTimestamp(&now)
		}, true},
		{"a spec field", func(p *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(p.Object, "renamed", "spec", "displayName")
		}, true},
		{"the quota limit", func(p *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(p.Object, "11", "spec", "resourceQuota", "limit", "pods")
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			before := deleteTestProject("5", "1")
			after := before.DeepCopy()
			tt.change(after)
			assert.Equal(t, tt.want, projectSpecChanged(before, after))
		})
	}
}

// TestDeleteProjectFlagGivesUpAfterThreeRetries asserts a project that keeps
// moving its resourceVersion is deleted at most four times, then reported as
// changed.
func TestDeleteProjectFlagGivesUpAfterThreeRetries(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t)
	var answers []*unstructured.Unstructured
	var rvs []string
	for i := 5; i <= 12; i++ {
		answers = append(answers, deleteTestProject(fmt.Sprint(i), fmt.Sprint(i)))
		rvs = append(rvs, fmt.Sprint(i))
	}
	f.withProjectAnswers(answers...)
	f.local.deleteErr = conflictOnProject(rvs...)
	_, exec := f.mustPlan(t, true)
	planned := len(f.log)

	_, err := exec(t.Context())
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Equal(t, "project p-1 changed after it was read; its namespaces are being deleted and the project was kept; re-run the same command", ce.Message)
	var deletes int
	for _, call := range f.log[planned:] {
		if strings.HasPrefix(call, "delete c-abc/p-1") {
			deletes++
		}
	}
	assert.Equal(t, 4, deletes, "the first delete and three retries")
}

// TestDeleteProjectFlagProjectDeleteFailureKeepsItsCode asserts a project
// delete refused for another reason than a conflict returns that error with
// every object's outcome.
func TestDeleteProjectFlagProjectDeleteFailureKeepsItsCode(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t, member("apps", "1"))
	f.local.deleteErr = func(_, name, _ string, dryRun bool) error {
		if !dryRun && name == "p-1" {
			return clierr.New(clierr.CodeRBAC, "projects is forbidden")
		}
		return nil
	}
	_, exec := f.mustPlan(t, true)
	_, err := exec(t.Context())
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, "projects is forbidden", ce.Message)
	assert.Equal(t, map[string]string{"namespace/apps": "deletion started", "project/p-1": "failed"}, objectsByName(detailsOf(t, err).Objects))
}

// TestDeleteProjectRefusesBeforeReadingNamespaces asserts a project the
// caller may not delete is refused at the project's own dry-run, before any
// namespace is read.
func TestDeleteProjectRefusesBeforeReadingNamespaces(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t, member("apps", "1"))
	f.local.deleteErr = func(_, _, _ string, dryRun bool) error {
		return clierr.New(clierr.CodeRBAC, "projects \"p-1\" is forbidden")
	}
	_, _, err := f.plan(t, true)
	require.ErrorContains(t, err, "forbidden")
	assert.Equal(t, []string{"get c-abc/p-1", "dry-run delete c-abc/p-1 rv=5"}, f.log)
}
