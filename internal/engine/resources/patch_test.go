package resources

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type updateCall struct {
	ns     string
	dryRun bool
	obj    *unstructured.Unstructured
}

// patchClient serves a canned object from Get and records Update calls (dry-run
// preview then real), echoing the submitted object back.
type patchClient struct {
	obj     *unstructured.Unstructured
	getErr  error
	updates []updateCall
}

func (p *patchClient) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	if p.getErr != nil {
		return nil, p.getErr
	}
	return p.obj, nil
}
func (p *patchClient) List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error) {
	return &unstructured.UnstructuredList{}, nil
}
func (p *patchClient) Logs(context.Context, string, string, string, int) (string, error) {
	return "", nil
}
func (p *patchClient) Create(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (p *patchClient) Update(_ context.Context, _ schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	p.updates = append(p.updates, updateCall{ns: ns, dryRun: dryRun, obj: obj})
	return obj, nil
}
func (p *patchClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}

func objFromJSON(t *testing.T, j string) *unstructured.Unstructured {
	t.Helper()
	o := &unstructured.Unstructured{}
	require.NoError(t, o.UnmarshalJSON([]byte(j)))
	return o
}

func planPatch(t *testing.T, cmd runtime.Command, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "patch must be a MutatingCommand")
	return mc.Plan(context.Background(), in)
}

func TestPatchPlanDiffsAndExecUsesRV(t *testing.T) {
	t.Parallel()
	orig := objFromJSON(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","resourceVersion":"7"},"spec":{"replicas":1}}`)
	client := &patchClient{obj: orig}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	cmd := NewPatch(mustKind(t, "deployment"), fac, nil)

	eff, exec, err := planPatch(t, cmd, input{args: []string{"web"}, s: map[string]string{
		"cluster": "prod", "namespace": "ns1",
		"patch": `[{"op":"replace","path":"/spec/replicas","value":3}]`,
	}})
	require.NoError(t, err)
	require.NotNil(t, exec)

	assert.Equal(t, safety.ActionUpdate, eff.Action)
	assert.True(t, eff.ConflictDetection)
	assert.False(t, eff.Destructive, "deployment is not a sensitive kind")
	assert.Equal(t, "7", eff.ResourceVersion)
	assert.Equal(t, "c-abc", eff.Cluster)
	assert.Equal(t, "deployments/web", eff.Target)

	var found bool
	for _, d := range eff.Diff {
		if d.Path == "/spec/replicas" {
			found = true
			assert.EqualValues(t, 1, d.From)
			assert.EqualValues(t, 3, d.To)
		}
	}
	assert.True(t, found, "diff must contain the replicas change, got %+v", eff.Diff)

	require.Len(t, client.updates, 1, "Plan issues one dry-run update")
	assert.True(t, client.updates[0].dryRun)

	res, err := exec(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "Deployment", res.Kind)
	require.Len(t, client.updates, 2, "exec performs the real update")
	assert.False(t, client.updates[1].dryRun)
	assert.Equal(t, "7", client.updates[1].obj.GetResourceVersion(), "the update carries the fetched RV precondition")
}

func TestPatchSensitiveKindIsDestructive(t *testing.T) {
	t.Parallel()
	orig := objFromJSON(t, `{"apiVersion":"management.cattle.io/v3","kind":"Setting","metadata":{"name":"server-url","resourceVersion":"3"},"value":"old"}`)
	client := &patchClient{obj: orig}
	fac := &recFactory{client: client}
	cmd := NewPatch(mustKind(t, "setting"), fac, nil)

	eff, _, err := planPatch(t, cmd, input{args: []string{"server-url"}, s: map[string]string{
		"patch": `[{"op":"replace","path":"/value","value":"new"}]`,
	}})
	require.NoError(t, err)
	assert.True(t, eff.Destructive, "patching a sensitive kind must be destructive")
}

func TestPatchDiffFromObjectComparisonNotOps(t *testing.T) {
	t.Parallel()
	orig := objFromJSON(t, `{"apiVersion":"management.cattle.io/v3","kind":"Cluster","metadata":{"name":"c1","resourceVersion":"1"},"spec":{"a":"x"}}`)
	client := &patchClient{obj: orig}
	fac := &recFactory{client: client}
	cmd := NewPatch(mustKind(t, "cluster"), fac, nil)

	eff, _, err := planPatch(t, cmd, input{args: []string{"c1"}, s: map[string]string{
		"patch": `[{"op":"move","from":"/spec/a","path":"/spec/b"}]`,
	}})
	require.NoError(t, err)

	var sawRemove, sawAdd bool
	for _, d := range eff.Diff {
		switch d.Path {
		case "/spec/a":
			sawRemove = true
			assert.EqualValues(t, "x", d.From)
			assert.Nil(t, d.To)
		case "/spec/b":
			sawAdd = true
			assert.Nil(t, d.From)
			assert.EqualValues(t, "x", d.To)
		}
	}
	assert.True(t, sawRemove, "a move must show the source cleared, got %+v", eff.Diff)
	assert.True(t, sawAdd, "a move must show the destination added, got %+v", eff.Diff)
}

func TestPatchExactlyOneInputSource(t *testing.T) {
	t.Parallel()
	fac := &recFactory{client: &patchClient{obj: objFromJSON(t, `{"apiVersion":"management.cattle.io/v3","kind":"Cluster","metadata":{"name":"c1"}}`)}}

	// both --patch and -f → VALIDATION.
	_, _, err := planPatch(t, NewPatch(mustKind(t, "cluster"), fac, mapFileSource{"p.json": []byte("[]")}),
		input{args: []string{"c1"}, s: map[string]string{"patch": "[]", "filename": "p.json"}})
	requireValidation(t, err)

	// neither → VALIDATION.
	_, _, err = planPatch(t, NewPatch(mustKind(t, "cluster"), fac, nil),
		input{args: []string{"c1"}, s: map[string]string{}})
	requireValidation(t, err)
}

func TestPatchInvalidOpsRejected(t *testing.T) {
	t.Parallel()

	// malformed JSON → VALIDATION at decode.
	fac := &recFactory{client: &patchClient{obj: objFromJSON(t, `{"apiVersion":"management.cattle.io/v3","kind":"Cluster","metadata":{"name":"c1"}}`)}}
	_, _, err := planPatch(t, NewPatch(mustKind(t, "cluster"), fac, nil),
		input{args: []string{"c1"}, s: map[string]string{"patch": "not-json"}})
	requireValidation(t, err)

	// a failing "test" op → VALIDATION at apply.
	orig := objFromJSON(t, `{"apiVersion":"management.cattle.io/v3","kind":"Cluster","metadata":{"name":"c1","resourceVersion":"1"},"spec":{"x":"y"}}`)
	fac2 := &recFactory{client: &patchClient{obj: orig}}
	_, _, err = planPatch(t, NewPatch(mustKind(t, "cluster"), fac2, nil),
		input{args: []string{"c1"}, s: map[string]string{"patch": `[{"op":"test","path":"/spec/x","value":"wrong"}]`}})
	requireValidation(t, err)
}
