package resources

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

// localGet is one Get the fake received.
type localGet struct {
	resource string
	ns       string
	name     string
}

// localWrite is one Create, Update or Delete the fake received.
type localWrite struct {
	op     string
	ns     string
	name   string
	dryRun bool
}

// localClient is a fake local cluster for a kind whose objects live in a
// project's backing namespace. It holds the project p-x of cluster c-x in
// the namespace c-x, with status.backingNamespace set to backing (absent
// when backing is empty), serves the bindings by namespace and records
// every request.
type localClient struct {
	backing  string
	resource string // the resource of the row under test, served from its namespaces
	bindings []unstructured.Unstructured

	gets   []localGet
	lists  []routeListCall
	writes []localWrite
}

var _ engine.ClusterClient = (*localClient)(nil)

func localBinding(k kinds.Kind, ns, name string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": k.GVR.Group + "/" + k.GVR.Version,
		"kind":       k.K8sKind,
		"metadata": map[string]any{
			"name": name, "namespace": ns, "resourceVersion": "5",
		},
		"projectName":      "c-x:p-x",
		"roleTemplateName": "project-member",
		"userName":         "u-1",
	}}
}

func (c *localClient) Get(_ context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	c.gets = append(c.gets, localGet{resource: gvr.Resource, ns: ns, name: name})
	switch gvr.Resource {
	case "projects":
		if ns != "c-x" || name != "p-x" {
			return nil, clierr.New(clierr.CodeNotFound, "project not found: "+name)
		}
		p := &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "p-x", "namespace": "c-x"},
		}}
		if c.backing != "" {
			p.Object["status"] = map[string]any{"backingNamespace": c.backing}
		}
		return p, nil
	case c.resource:
		for i := range c.bindings {
			if c.bindings[i].GetNamespace() == ns && c.bindings[i].GetName() == name {
				return c.bindings[i].DeepCopy(), nil
			}
		}
	}
	return nil, clierr.New(clierr.CodeNotFound, name+" not found")
}

func (c *localClient) List(_ context.Context, gvr schema.GroupVersionResource, ns, selector string) (*unstructured.UnstructuredList, error) {
	c.lists = append(c.lists, routeListCall{resource: gvr.Resource, ns: ns, selector: selector})
	out := &unstructured.UnstructuredList{}
	for _, b := range c.bindings {
		if ns == "" || b.GetNamespace() == ns {
			out.Items = append(out.Items, b)
		}
	}
	return out, nil
}

func (c *localClient) Create(_ context.Context, _ schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	c.writes = append(c.writes, localWrite{op: "create", ns: ns, name: obj.GetName(), dryRun: dryRun})
	return obj, nil
}

func (c *localClient) Update(_ context.Context, _ schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	c.writes = append(c.writes, localWrite{op: "update", ns: ns, name: obj.GetName(), dryRun: dryRun})
	return obj, nil
}

func (c *localClient) Delete(_ context.Context, _ schema.GroupVersionResource, ns, name, _ string, dryRun bool) error {
	c.writes = append(c.writes, localWrite{op: "delete", ns: ns, name: name, dryRun: dryRun})
	return nil
}

func (*localClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", nil
}

func (*localClient) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	return engine.ReviewResponse{}, errors.New("PostReview not scripted in this fake")
}

func (*localClient) ServerResources(context.Context) ([]engine.APIResource, error) {
	return nil, errors.New("ServerResources not scripted in this fake")
}

// localFixture is a fake local cluster whose project p-x of cluster c-x has
// the given backing namespace ("" leaves status.backingNamespace unset). It
// holds the binding b1 in the project's namespace (the project id when the
// backing namespace is empty) and the binding b2 in the namespace
// "elsewhere".
func localFixture(k kinds.Kind, backing string) (*localClient, *requestFactory) {
	ns := backing
	if ns == "" {
		ns = "p-x"
	}
	client := &localClient{backing: backing, resource: k.GVR.Resource,
		bindings: []unstructured.Unstructured{localBinding(k, ns, "b1"), localBinding(k, "elsewhere", "b2")}}
	fac := &requestFactory{}
	fac.client, fac.resolve = client, map[string]string{"c-x": "c-x"}
	return client, fac
}

// typedProjectContext is the context the pipeline gives a command whose typed
// --project resolved to the project p-x of cluster c-x.
func typedProjectContext(t *testing.T) context.Context {
	t.Helper()
	return runtime.WithProject(t.Context(), runtime.Project{Cluster: "c-x", ID: "p-x"})
}

func projectBackingNamespaceRows(t *testing.T) []kinds.Kind {
	t.Helper()
	var rows []kinds.Kind
	for _, k := range kinds.All() {
		if k.ProjectBackingNamespace {
			rows = append(rows, k)
		}
	}
	require.NotEmpty(t, rows, "at least one kind has its objects in a project's backing namespace")
	return rows
}

// TestEveryManagementPlainRowSetsProjectBackingNamespaceOrIsListedWithAReason
// asserts each Management row with a plain namespace sets
// ProjectBackingNamespace or is listed here with the reason its namespace is
// not a project's backing namespace, and that a listed row does not set it.
func TestEveryManagementPlainRowSetsProjectBackingNamespaceOrIsListedWithAReason(t *testing.T) {
	t.Parallel()
	exempt := map[string]string{
		"clusterregistrationtoken": "its namespace is the cluster's management namespace, the cluster id; its get is a Steve read and its patch is refused",
	}
	found := map[string]bool{}
	for _, k := range kinds.All() {
		if k.Scope != kinds.Management || k.Policy != kinds.NSPlain {
			continue
		}
		found[k.Key] = true
		if reason, listed := exempt[k.Key]; listed {
			assert.NotEmpty(t, reason, k.Key)
			assert.False(t, k.ProjectBackingNamespace, "%s is listed as a kind whose namespace is not a project's backing namespace, but sets ProjectBackingNamespace", k.Key)
			continue
		}
		assert.True(t, k.ProjectBackingNamespace,
			"%s is Management with a plain namespace: set ProjectBackingNamespace, or list it here with the reason", k.Key)
	}
	for key := range exempt {
		assert.True(t, found[key], "%s is listed here but is not a Management row with a plain namespace", key)
	}
}

// TestProjectBackingNamespaceRowFlags asserts the get, patch and delete of a
// kind whose objects live in a project's backing namespace list --cluster,
// --namespace and --project first, in that order, none required, with a
// --namespace description written for this kind and a --project description
// that mentions --cluster and does not say the namespace must be in the
// project.
func TestProjectBackingNamespaceRowFlags(t *testing.T) {
	t.Parallel()
	for _, k := range projectBackingNamespaceRows(t) {
		for verb, flags := range map[string][]runtime.ArgSpec{
			"get":    getFlags(k),
			"patch":  patchFlags(k),
			"delete": deleteFlags(k),
		} {
			t.Run(verb+" "+k.Name, func(t *testing.T) {
				t.Parallel()
				require.GreaterOrEqual(t, len(flags), 3)
				assert.Equal(t, []string{FlagCluster, FlagNamespace, FlagProject},
					[]string{flags[0].Name, flags[1].Name, flags[2].Name})
				for _, f := range flags[:3] {
					assert.False(t, f.Required, "--%s", f.Name)
				}
				assert.NotEqual(t, engine.NamespaceFlag(false).Desc, flags[1].Desc, "the namespace description is not the generic one")
				assert.Contains(t, flags[2].Desc, "--cluster")
				assert.NotContains(t, flags[2].Desc, "must be in", "the project description is not a membership check")
			})
		}
	}
}

// TestProjectBackingNamespaceRowRoute asserts, for every kind whose objects
// live in a project's backing namespace, that --project with --cluster
// routes to the local cluster in the namespace the project's
// status.backingNamespace names (the project id when it is empty), with no
// project scope, and that the refusals send no request.
func TestProjectBackingNamespaceRowRoute(t *testing.T) {
	t.Parallel()
	for _, k := range projectBackingNamespaceRows(t) {
		both := map[string]string{"cluster": "c-x", "project": "p-x"}

		for _, op := range []struct {
			name string
			op   Operation
		}{{"list", OpList}, {"named", OpNamed}} {
			for _, tc := range []struct {
				name, backing, wantNS string
			}{
				{"backing namespace", "c-x-p-x", "c-x-p-x"},
				{"empty backing namespace", "", "p-x"},
			} {
				t.Run(k.Name+" "+op.name+" "+tc.name, func(t *testing.T) {
					t.Parallel()
					client, fac := localFixture(k, tc.backing)
					clusterID, ns, effect, scope, err := Route(typedProjectContext(t), fac, k, input{s: both}, op.op, "b1")
					require.NoError(t, err)
					assert.Equal(t, engine.LocalCluster, clusterID)
					assert.Equal(t, tc.wantNS, ns)
					assert.Equal(t, engine.LocalCluster, effect)
					assert.Nil(t, scope)
					assert.Equal(t, []localGet{{resource: "projects", ns: "c-x", name: "p-x"}}, client.gets,
						"the only read is the project, through the local cluster")
					assert.Empty(t, client.writes)
				})
			}
		}

		t.Run(k.Name+" a namespace that is the backing namespace", func(t *testing.T) {
			t.Parallel()
			_, fac := localFixture(k, "c-x-p-x")
			_, ns, _, _, err := Route(typedProjectContext(t), fac, k,
				input{s: map[string]string{"cluster": "c-x", "project": "p-x", "namespace": "c-x-p-x"}}, OpNamed, "b1")
			require.NoError(t, err)
			assert.Equal(t, "c-x-p-x", ns)
		})

		t.Run(k.Name+" a namespace that is not the backing namespace", func(t *testing.T) {
			t.Parallel()
			client, fac := localFixture(k, "c-x-p-x")
			_, _, _, _, err := Route(typedProjectContext(t), fac, k,
				input{s: map[string]string{"cluster": "c-x", "project": "p-x", "namespace": "other"}}, OpNamed, "b1")
			requireValidation(t, err)
			require.ErrorContains(t, err, "--namespace 'other' is not the backing namespace of project 'p-x' in cluster 'c-x'; the backing namespace is 'c-x-p-x'")
			assert.Empty(t, client.writes)
		})

		t.Run(k.Name+" --cluster without --project", func(t *testing.T) {
			t.Parallel()
			client, fac := localFixture(k, "c-x-p-x")
			_, _, _, _, err := Route(t.Context(), fac, k, input{s: map[string]string{"cluster": "c-x"}}, OpList, "")
			requireValidation(t, err)
			require.ErrorContains(t, err, "--cluster applies to a "+k.Name+" only with --project")
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, clierr.Rancher+" access projects --cluster 'c-x'", ce.Hint)
			assert.Empty(t, fac.resolveCalls)
			assert.Empty(t, fac.clusterCalls)
			assert.Empty(t, client.gets)
			assert.Empty(t, client.lists)
		})

		t.Run(k.Name+" a named object with no namespace and no project", func(t *testing.T) {
			t.Parallel()
			client, fac := localFixture(k, "c-x-p-x")
			_, _, _, _, err := Route(t.Context(), fac, k, input{}, OpNamed, "b1")
			requireValidation(t, err)
			require.ErrorContains(t, err, "--namespace, or --project with --cluster, is required when you give the name of one "+k.Name)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, clierr.Rancher+" access projects", ce.Hint)
			assert.Empty(t, fac.clusterCalls)
			assert.Empty(t, client.gets)
		})

		t.Run(k.Name+" a list with no namespace and no project", func(t *testing.T) {
			t.Parallel()
			_, fac := localFixture(k, "c-x-p-x")
			clusterID, ns, _, scope, err := Route(t.Context(), fac, k, input{}, OpList, "")
			require.NoError(t, err)
			assert.Equal(t, engine.LocalCluster, clusterID)
			assert.Empty(t, ns, "a nameless list reads every namespace")
			assert.Nil(t, scope)
		})

		t.Run(k.Name+" a namespace with no project", func(t *testing.T) {
			t.Parallel()
			client, fac := localFixture(k, "c-x-p-x")
			clusterID, ns, _, scope, err := Route(t.Context(), fac, k, input{s: map[string]string{"namespace": "c-x-p-x"}}, OpNamed, "b1")
			require.NoError(t, err)
			assert.Equal(t, engine.LocalCluster, clusterID)
			assert.Equal(t, "c-x-p-x", ns)
			assert.Nil(t, scope)
			assert.Empty(t, client.gets, "no project is read")
		})

		t.Run(k.Name+" a create with --project", func(t *testing.T) {
			t.Parallel()
			client, fac := localFixture(k, "c-x-p-x")
			_, _, _, _, err := Route(unreadProjectContext(t), fac, k, input{s: both}, OpCreate, "b1")
			requireValidation(t, err)
			require.ErrorContains(t, err, "--project does not apply to "+k.Name)
			assert.Empty(t, fac.clusterCalls)
			assert.Empty(t, client.gets)
			assert.Empty(t, client.writes)
		})

		t.Run(k.Name+" a create with --cluster and no --project", func(t *testing.T) {
			t.Parallel()
			_, fac := localFixture(k, "c-x-p-x")
			clusterID, ns, _, _, err := Route(t.Context(), fac, k,
				input{s: map[string]string{"cluster": "c-x", "namespace": "c-x-p-x"}}, OpCreate, "b1")
			require.NoError(t, err)
			assert.Equal(t, engine.LocalCluster, clusterID)
			assert.Equal(t, "c-x-p-x", ns)
		})

	}
}

// TestProjectBackingNamespaceRowCommands asserts get, patch and delete of a
// kind whose objects live in a project's backing namespace read and write
// the object in the namespace the project names.
func TestProjectBackingNamespaceRowCommands(t *testing.T) {
	t.Parallel()
	for _, k := range projectBackingNamespaceRows(t) {
		for _, tc := range []struct {
			name, backing, wantNS string
		}{
			{"backing namespace", "c-x-p-x", "c-x-p-x"},
			{"empty backing namespace", "", "p-x"},
		} {
			flags := map[string]string{"cluster": "c-x", "project": "p-x"}

			t.Run(k.Name+" get list "+tc.name, func(t *testing.T) {
				t.Parallel()
				client, fac := localFixture(k, tc.backing)
				res, err := NewGet(k, fac).(runtime.ReadCommand).Execute(typedProjectContext(t), input{s: flags}, nil)
				require.NoError(t, err)
				assert.Equal(t, k.K8sKind+"List", res.Kind)
				assert.Equal(t, []routeListCall{{resource: k.GVR.Resource, ns: tc.wantNS}}, client.lists)
			})

			t.Run(k.Name+" get named "+tc.name, func(t *testing.T) {
				t.Parallel()
				client, fac := localFixture(k, tc.backing)
				res, err := NewGet(k, fac).(runtime.ReadCommand).Execute(typedProjectContext(t), input{args: []string{"b1"}, s: flags}, nil)
				require.NoError(t, err)
				assert.Equal(t, k.K8sKind, res.Kind)
				assert.Contains(t, client.gets, localGet{resource: k.GVR.Resource, ns: tc.wantNS, name: "b1"})
			})

			t.Run(k.Name+" patch "+tc.name, func(t *testing.T) {
				t.Parallel()
				client, fac := localFixture(k, tc.backing)
				patchArgs := map[string]string{"cluster": "c-x", "project": "p-x",
					"patch": `[{"op":"replace","path":"/roleTemplateName","value":"project-owner"}]`}
				_, exec, err := NewPatch(k, fac, nil).(runtime.MutatingCommand).Plan(typedProjectContext(t), input{args: []string{"b1"}, s: patchArgs})
				require.NoError(t, err)
				_, err = exec(t.Context())
				require.NoError(t, err)
				assert.Equal(t, []localWrite{
					{op: "update", ns: tc.wantNS, name: "b1", dryRun: true},
					{op: "update", ns: tc.wantNS, name: "b1"},
				}, client.writes, "the preview and the write go to the project's namespace")
			})

			t.Run(k.Name+" delete "+tc.name, func(t *testing.T) {
				t.Parallel()
				client, fac := localFixture(k, tc.backing)
				eff, exec, err := NewDelete(k, fac).(runtime.MutatingCommand).Plan(typedProjectContext(t), input{args: []string{"b1"}, s: flags})
				require.NoError(t, err)
				assert.Equal(t, safety.ActionDelete, eff.Action)
				assert.Equal(t, engine.LocalCluster, eff.Cluster)
				_, err = exec(t.Context())
				require.NoError(t, err)
				assert.Equal(t, []localWrite{
					{op: "delete", ns: tc.wantNS, name: "b1", dryRun: true},
					{op: "delete", ns: tc.wantNS, name: "b1"},
				}, client.writes, "the preview and the delete go to the project's namespace")
			})
		}
	}
}
