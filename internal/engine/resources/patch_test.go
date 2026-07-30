package resources

import (
	"context"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
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
	getName []string
}

func (p *patchClient) Get(_ context.Context, _ schema.GroupVersionResource, _ string, name string) (*unstructured.Unstructured, error) {
	p.getName = append(p.getName, name)
	if p.getErr != nil {
		return nil, p.getErr
	}
	return p.obj, nil
}
func (p *patchClient) List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error) {
	return &unstructured.UnstructuredList{}, nil
}
func (p *patchClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
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

// TestPatchMultiWordKindUsesK8sKindCasing is the exercise bug 7 repro for
// the patch path: the envelope kind must echo k.K8sKind's casing
// ("StatefulSet"), not a naive first-rune capitalization ("Statefulset").
func TestPatchMultiWordKindUsesK8sKindCasing(t *testing.T) {
	t.Parallel()
	orig := objFromJSON(t, `{"apiVersion":"apps/v1","kind":"StatefulSet","metadata":{"name":"web","resourceVersion":"7"},"spec":{"replicas":1}}`)
	client := &patchClient{obj: orig}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	cmd := NewPatch(mustKind(t, "statefulset"), fac, nil)

	_, exec, err := planPatch(t, cmd, input{args: []string{"web"}, s: map[string]string{
		"cluster": "prod", "namespace": "ns1",
		"patch": `[{"op":"replace","path":"/spec/replicas","value":3}]`,
	}})
	require.NoError(t, err)

	res, err := exec(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "StatefulSet", res.Kind)
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

	// Round-2: redaction is now gated on Sensitive (same flag as the --yes
	// gate above), not the literal "secret" kind — but a Sensitive kind
	// whose payload isn't under /data or /stringData must see no behavior
	// change: the redaction condition fires, the path-shape check just never
	// matches, so the real values still reach the Effect.
	var found bool
	for _, d := range eff.Diff {
		if d.Path == "/value" {
			found = true
			assert.EqualValues(t, "old", d.From)
			assert.EqualValues(t, "new", d.To)
		}
	}
	assert.True(t, found, "diff must contain the value change, got %+v", eff.Diff)
}

func TestPatchSecretDiffIsRedacted(t *testing.T) {
	t.Parallel()
	orig := objFromJSON(t, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db-creds","namespace":"default","resourceVersion":"5"},"data":{"password":"b2xk"}}`)
	client := &patchClient{obj: orig}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	cmd := NewPatch(mustKind(t, "secret"), fac, nil)

	eff, _, err := planPatch(t, cmd, input{args: []string{"db-creds"}, s: map[string]string{
		"cluster": "prod", "namespace": "default",
		"patch": `[{"op":"replace","path":"/data/password","value":"bmV3"}]`,
	}})
	require.NoError(t, err)
	assert.True(t, eff.Destructive, "patching a secret must be destructive")

	var found bool
	for _, d := range eff.Diff {
		if d.Path == "/data/password" {
			found = true
			assert.Equal(t, "<redacted>", d.From)
			assert.Equal(t, "<redacted>", d.To)
		}
		assert.NotEqual(t, "b2xk", d.From, "raw secret value must never reach the Effect")
		assert.NotEqual(t, "bmV3", d.To, "raw secret value must never reach the Effect")
	}
	assert.True(t, found, "diff must contain the password path, got %+v", eff.Diff)
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

// TestPatchClusterResolvesDisplayName is the round-2 repro for finding 3:
// patch must resolve a cluster's positional name the same way get does —
// id hit, display hit, ambiguous, and miss — so the two commands agree on
// what a name means for the managementcluster kind.
func TestPatchClusterResolvesDisplayName(t *testing.T) {
	t.Parallel()
	orig := objFromJSON(t, `{"apiVersion":"management.cattle.io/v3","kind":"Cluster","metadata":{"name":"c-m-abc","resourceVersion":"1"},"spec":{"a":"x"}}`)

	t.Run("id hit passes through unchanged", func(t *testing.T) {
		t.Parallel()
		client := &patchClient{obj: orig}
		fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-m-abc"}}
		_, _, err := planPatch(t, NewPatch(mustKind(t, "cluster"), fac, nil),
			input{args: []string{"c-m-abc"}, s: map[string]string{"patch": `[{"op":"replace","path":"/spec/a","value":"y"}]`}})
		require.NoError(t, err)
		assert.Equal(t, []string{"c-m-abc"}, client.getName)
	})

	t.Run("display name resolves to the cluster id before Get", func(t *testing.T) {
		t.Parallel()
		client := &patchClient{obj: orig}
		fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-m-abc"}}
		_, _, err := planPatch(t, NewPatch(mustKind(t, "cluster"), fac, nil),
			input{args: []string{"prod"}, s: map[string]string{"patch": `[{"op":"replace","path":"/spec/a","value":"y"}]`}})
		require.NoError(t, err)
		assert.Equal(t, []string{"c-m-abc"}, client.getName)
	})

	t.Run("ambiguous display name fails before Get", func(t *testing.T) {
		t.Parallel()
		client := &patchClient{obj: orig}
		fac := &recFactory{client: client, resolveErr: map[string]error{
			"prod": clierr.New(clierr.CodeAmbiguousSelection, `2 clusters have display name "prod"`),
		}}
		_, _, err := planPatch(t, NewPatch(mustKind(t, "cluster"), fac, nil),
			input{args: []string{"prod"}, s: map[string]string{"patch": `[{"op":"replace","path":"/spec/a","value":"y"}]`}})
		var ce *clierr.Error
		require.True(t, errors.As(err, &ce))
		assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
		assert.Empty(t, client.getName, "Get must not be called on an ambiguous resolution")
	})

	t.Run("unresolvable name fails before Get", func(t *testing.T) {
		t.Parallel()
		client := &patchClient{obj: orig}
		fac := &recFactory{client: client, resolveErr: map[string]error{
			"missing": clierr.New(clierr.CodeNotFound, `cluster not found: "missing"`),
		}}
		_, _, err := planPatch(t, NewPatch(mustKind(t, "cluster"), fac, nil),
			input{args: []string{"missing"}, s: map[string]string{"patch": `[{"op":"replace","path":"/spec/a","value":"y"}]`}})
		var ce *clierr.Error
		require.True(t, errors.As(err, &ce))
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
		assert.Empty(t, client.getName, "Get must not be called on a failed resolution")
	})
}

// TestPatchEffectEnricherObservesObjectsAndWarns is Task 8's hook coverage:
// an enricher passed to NewPatch is invoked with the exact orig/patched
// objects Plan computed, and a warning it appends survives into the
// returned Effect. This is the seam the provisioningcluster quorum-preview
// override (surface/cli/root.go) is built on.
func TestPatchEffectEnricherObservesObjectsAndWarns(t *testing.T) {
	t.Parallel()
	orig := objFromJSON(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","resourceVersion":"7"},"spec":{"replicas":1}}`)
	client := &patchClient{obj: orig}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}

	var gotOrig, gotPatched *unstructured.Unstructured
	enrich := func(o, p *unstructured.Unstructured, eff *safety.Effect) {
		gotOrig, gotPatched = o, p
		eff.Warnings = append(eff.Warnings, "marker-warning")
	}
	cmd := NewPatch(mustKind(t, "deployment"), fac, nil, enrich)

	eff, _, err := planPatch(t, cmd, input{args: []string{"web"}, s: map[string]string{
		"cluster": "prod", "namespace": "ns1",
		"patch": `[{"op":"replace","path":"/spec/replicas","value":3}]`,
	}})
	require.NoError(t, err)
	assert.Contains(t, eff.Warnings, "marker-warning")

	require.NotNil(t, gotOrig, "enricher must receive the original object")
	require.NotNil(t, gotPatched, "enricher must receive the patched object")
	origReplicas, _, _ := unstructured.NestedInt64(gotOrig.Object, "spec", "replicas")
	patchedReplicas, _, _ := unstructured.NestedInt64(gotPatched.Object, "spec", "replicas")
	assert.EqualValues(t, 1, origReplicas, "enricher's orig must be the fetched object, unmodified")
	assert.EqualValues(t, 3, patchedReplicas, "enricher's patched must reflect the applied patch")
}

// TestPatchNoEnricherLeavesWarningsUnset is the regression proof the
// variadic enrich parameter doesn't alter default behavior: every existing
// call site passes zero enrichers, and the Effect it gets back must carry
// no warnings from this mechanism.
func TestPatchNoEnricherLeavesWarningsUnset(t *testing.T) {
	t.Parallel()
	orig := objFromJSON(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","resourceVersion":"7"},"spec":{"replicas":1}}`)
	client := &patchClient{obj: orig}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	cmd := NewPatch(mustKind(t, "deployment"), fac, nil)

	eff, _, err := planPatch(t, cmd, input{args: []string{"web"}, s: map[string]string{
		"cluster": "prod", "namespace": "ns1",
		"patch": `[{"op":"replace","path":"/spec/replicas","value":3}]`,
	}})
	require.NoError(t, err)
	assert.Empty(t, eff.Warnings)
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
