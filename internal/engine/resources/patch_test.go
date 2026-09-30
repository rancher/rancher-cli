package resources

import (
	"context"
	"errors"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
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

type updateCall struct {
	ns     string
	dryRun bool
	obj    *unstructured.Unstructured
}

// patchClient serves a canned object from Get and records Update calls (dry-run
// preview then real), echoing the submitted object back. execUpdateErr is
// returned only on the real (non-dry-run) Update, simulating a post-confirmation
// TOCTOU race.
type patchClient struct {
	obj           *unstructured.Unstructured
	getErr        error
	execUpdateErr error
	updates       []updateCall
	getName       []string
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
	if !dryRun && p.execUpdateErr != nil {
		return nil, p.execUpdateErr
	}
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
	return mc.Plan(t.Context(), in)
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

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "Deployment", res.Kind)
	require.Len(t, client.updates, 2, "exec performs the real update")
	assert.False(t, client.updates[1].dryRun)
	assert.Equal(t, "7", client.updates[1].obj.GetResourceVersion(), "the update carries the fetched RV precondition")
}

// TestPatchMultiWordKindUsesK8sKindCasing asserts, for
// the patch path, that the envelope kind echoes k.K8sKind's casing
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

	res, err := exec(t.Context())
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

	// Redaction is gated on Sensitive (same flag as the --yes
	// gate above), not the literal "secret" kind - but a Sensitive kind
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

// TestMatchesRedactPathEscapesSlashKey asserts a RedactPaths key containing
// "/" matches only the pointer segment it produced, not a path through a
// nested key of the same name split on "/".
func TestMatchesRedactPathEscapesSlashKey(t *testing.T) {
	t.Parallel()
	keys := []string{"a/b"}
	assert.True(t, matchesRedactPath("/a~1b", keys))
	assert.True(t, matchesRedactPath("/a~1b/nested", keys))
	assert.False(t, matchesRedactPath("/a/b", keys), "must not match the two-segment path a -> b")
}

// TestValueCarriesEscapesSlashKey asserts valueCarries and matchesRedactPath
// build the same pointer for a key containing "/": a replace value carrying
// the literal key "a/b" carries the field, a value nesting "b" under "a"
// (a distinct pointer) does not.
func TestValueCarriesEscapesSlashKey(t *testing.T) {
	t.Parallel()
	keys := []string{"a/b"}

	carrying, err := jsonpatch.DecodePatch([]byte(`[{"op":"replace","path":"","value":{"a/b":"x"}}]`))
	require.NoError(t, err)
	assert.True(t, valueCarries(carrying[0], "", keys))

	nested, err := jsonpatch.DecodePatch([]byte(`[{"op":"replace","path":"","value":{"a":{"b":"x"}}}]`))
	require.NoError(t, err)
	assert.False(t, valueCarries(nested[0], "", keys), "must not match the two-segment field a -> b")
}

// TestPatchRedactsUserPasswordFromResult asserts the patch-response redaction
// seam: a User's password reaches the server in the patch (the fake client
// echoes back exactly what was submitted, as a real server does), but the
// committed Result never carries it - mirroring create's redaction of the
// same field.
func TestPatchRedactsUserPasswordFromResult(t *testing.T) {
	t.Parallel()
	orig := objFromJSON(t, `{"apiVersion":"management.cattle.io/v3","kind":"User","metadata":{"name":"u-test","resourceVersion":"1"},"username":"alice","password":"hunter2"}`)
	client := &patchClient{obj: orig}
	fac := &recFactory{client: client}
	cmd := NewPatch(mustKind(t, "user"), fac, nil)

	_, exec, err := planPatch(t, cmd, input{args: []string{"u-test"}, s: map[string]string{
		"patch": `[{"op":"replace","path":"/username","value":"alice2"}]`,
	}})
	require.NoError(t, err)
	require.NotNil(t, exec)

	res, err := exec(t.Context())
	require.NoError(t, err)
	obj, ok := res.Data.(map[string]any)
	require.True(t, ok)
	_, present := obj["password"]
	assert.False(t, present, "password must be scrubbed from the patch response")
	assert.Equal(t, "alice2", obj["username"], "unrelated fields survive redaction")
}

// TestPatchRedactsManagementTokenFromResult mirrors the User case for
// management-token's root-level token field.
func TestPatchRedactsManagementTokenFromResult(t *testing.T) {
	t.Parallel()
	orig := objFromJSON(t, `{"apiVersion":"management.cattle.io/v3","kind":"Token","metadata":{"name":"t-test","resourceVersion":"1"},"userId":"u-1","token":"secretvalue"}`)
	client := &patchClient{obj: orig}
	fac := &recFactory{client: client}
	cmd := NewPatch(mustKind(t, "managementtoken"), fac, nil)

	_, exec, err := planPatch(t, cmd, input{args: []string{"t-test"}, s: map[string]string{
		"patch": `[{"op":"replace","path":"/userId","value":"u-2"}]`,
	}})
	require.NoError(t, err)
	require.NotNil(t, exec)

	res, err := exec(t.Context())
	require.NoError(t, err)
	obj, ok := res.Data.(map[string]any)
	require.True(t, ok)
	_, present := obj["token"]
	assert.False(t, present, "token must be scrubbed from the patch response")
	assert.Equal(t, "u-2", obj["userId"], "unrelated fields survive redaction")
}

// TestPatchRedactsSecretPayloadFromResult proves the Sensitive-gated
// data/stringData guard applies to a patch's committed result, the same way
// it already applies to the patch's preview diff and to create's response.
func TestPatchRedactsSecretPayloadFromResult(t *testing.T) {
	t.Parallel()
	orig := objFromJSON(t, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db-creds","namespace":"default","resourceVersion":"5"},"data":{"password":"b2xk"}}`)
	client := &patchClient{obj: orig}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	cmd := NewPatch(mustKind(t, "secret"), fac, nil)

	_, exec, err := planPatch(t, cmd, input{args: []string{"db-creds"}, s: map[string]string{
		"cluster": "prod", "namespace": "default",
		"patch": `[{"op":"replace","path":"/data/password","value":"bmV3"}]`,
	}})
	require.NoError(t, err)
	require.NotNil(t, exec)

	res, err := exec(t.Context())
	require.NoError(t, err)
	obj, ok := res.Data.(map[string]any)
	require.True(t, ok)
	_, present := obj["data"]
	assert.False(t, present, "data must be scrubbed from the patch response")
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

// TestPatchNotFoundCurated asserts the curated NOT_FOUND shape for a named patch:
// the dynamic client now produces "<kind> not found: <name>"; the fake
// simulates that pre-curated shape.
func TestPatchNotFoundCurated(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "deployment")
	client := &patchClient{getErr: kinds.NotFoundCurated(k, "web", "c-abc")}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	_, _, err := planPatch(t, NewPatch(k, fac, nil),
		input{args: []string{"web"}, s: map[string]string{
			"cluster": "prod", "namespace": "ns1",
			"patch": `[{"op":"replace","path":"/spec/replicas","value":3}]`,
		}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "deployment not found: web", ce.Message)
	assert.Equal(t, "list with: rancher get deployments --cluster 'c-abc'", ce.Hint)
}

// TestPatchNonNotFoundPassesThrough asserts that a non-NOT_FOUND error from the
// client (e.g. RBAC) reaches the caller unchanged without re-wrapping.
func TestPatchNonNotFoundPassesThrough(t *testing.T) {
	t.Parallel()
	client := &patchClient{getErr: clierr.New(clierr.CodeRBAC, "forbidden")}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	_, _, err := planPatch(t, NewPatch(mustKind(t, "deployment"), fac, nil),
		input{args: []string{"web"}, s: map[string]string{
			"cluster": "prod", "namespace": "ns1",
			"patch": `[{"op":"replace","path":"/spec/replicas","value":3}]`,
		}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, "forbidden", ce.Message)
}

// TestPatchClusterResolvesDisplayName asserts that
// patch resolves a cluster's positional name the same way get does -
// id hit, display hit, ambiguous, and miss - so the two commands agree on
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
		require.ErrorAs(t, err, &ce)
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
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
		assert.Empty(t, client.getName, "Get must not be called on a failed resolution")
	})
}

// TestPatchEffectEnricherObservesObjectsAndWarns covers the hook:
// an enricher passed to NewPatch is invoked with the exact orig/patched
// objects Plan computed, and a warning it appends survives into the
// returned Effect. This is the seam the provisioningcluster quorum-preview
// override (internal/cli/root.go) is built on.
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

// TestPatchExecPhaseNotFoundCurated asserts the post-confirmation TOCTOU path:
// the object is deleted between the dry-run and the real update; the dynamic
// client curates the NOT_FOUND (client-level curation covers this). The fake
// simulates the pre-curated shape the real client now produces.
func TestPatchExecPhaseNotFoundCurated(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "deployment")
	orig := objFromJSON(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","resourceVersion":"3"},"spec":{"replicas":1}}`)
	client := &patchClient{
		obj:           orig,
		execUpdateErr: kinds.NotFoundCurated(k, "web", "c-abc"),
	}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	_, exec, err := planPatch(t, NewPatch(k, fac, nil),
		input{args: []string{"web"}, s: map[string]string{
			"cluster": "prod", "namespace": "ns1",
			"patch": `[{"op":"replace","path":"/spec/replicas","value":3}]`,
		}})
	require.NoError(t, err, "Plan must succeed (dry-run succeeds)")
	require.NotNil(t, exec)

	_, err = exec(t.Context())
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "deployment not found: web", ce.Message)
	assert.NotEmpty(t, ce.Hint, "curated exec-phase NOT_FOUND must carry a list hint")
}

// TestPatchDescribeInheritsCapability asserts the requirement-inheritance rule:
// a kind row's Capability flows unchanged into the generated patch command's
// Descriptor.
func TestPatchDescribeInheritsCapability(t *testing.T) {
	t.Parallel()
	req := runtime.CapabilityRequirement{Tier: runtime.CapabilityPrime, Feature: "widget-feature"}
	k := kinds.Kind{Name: "widget", Aliases: []string{"widgets"}, Capability: req}
	d := NewPatch(k, nil, nil).Describe()
	assert.Equal(t, req, d.Capability)
}

// TestPatchRefusesManagedFieldsOps asserts that no RFC 6902 op may target the
// object's managed fields: field ownership is recorded by the server, and a
// patch that rewrites it would forge that record. The check covers the
// path of every op and the from of a move or copy, the exact path and
// anything under it, and leaves a sibling field with a similar name alone.
func TestPatchRefusesManagedFieldsOps(t *testing.T) {
	t.Parallel()
	orig := objFromJSON(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","resourceVersion":"7","managedFieldsX":"keep"},"spec":{"replicas":1}}`)
	cases := []struct {
		name    string
		ops     string
		refused bool
	}{
		{"replace the whole list", `[{"op":"replace","path":"/metadata/managedFields","value":[]}]`, true},
		{"remove the whole list", `[{"op":"remove","path":"/metadata/managedFields"}]`, true},
		{"add under it", `[{"op":"add","path":"/metadata/managedFields/0/manager","value":"x"}]`, true},
		{"move from under it", `[{"op":"move","from":"/metadata/managedFields/0","path":"/spec/x"}]`, true},
		{"copy from it", `[{"op":"copy","from":"/metadata/managedFields","path":"/spec/x"}]`, true},
		{"second op offends", `[{"op":"replace","path":"/spec/replicas","value":2},{"op":"remove","path":"/metadata/managedFields"}]`, true},
		{"sibling with a similar name", `[{"op":"replace","path":"/metadata/managedFieldsX","value":"other"}]`, false},
		{"ordinary spec change", `[{"op":"replace","path":"/spec/replicas","value":3}]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &patchClient{obj: orig}
			fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
			cmd := NewPatch(mustKind(t, "deployment"), fac, nil)
			_, _, err := planPatch(t, cmd, input{args: []string{"web"}, s: map[string]string{
				"cluster": "prod", "namespace": "ns1", "patch": tc.ops,
			}})
			if !tc.refused {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			require.ErrorContains(t, err, "/metadata/managedFields")
			require.ErrorContains(t, err, "the server sets this field and a patch cannot change it")
			assert.Empty(t, client.getName, "refused before any request is made")
		})
	}
}

// TestPatchRefusesServerOwnedMetadataOps asserts the refusal of ops on the
// fields the server owns and the diff already skips: managed fields and
// the resource version, exact or under, and an add or replace of an
// ancestor whose value smuggles managed fields in.
func TestPatchRefusesServerOwnedMetadataOps(t *testing.T) {
	t.Parallel()
	orig := objFromJSON(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","resourceVersion":"7"},"spec":{"replicas":1}}`)
	cases := []struct {
		name string
		ops  string
		want string
	}{
		{"resourceVersion", `[{"op":"replace","path":"/metadata/resourceVersion","value":"1"}]`, "/metadata/resourceVersion"},
		{"replace metadata carrying managedFields", `[{"op":"replace","path":"/metadata","value":{"name":"web","managedFields":[]}}]`, "/metadata/managedFields"},
		{"replace whole object carrying managedFields", `[{"op":"replace","path":"","value":{"metadata":{"name":"web","managedFields":[]}}}]`, "/metadata/managedFields"},
		{"add metadata carrying managedFields", `[{"op":"add","path":"/metadata","value":{"managedFields":[{"manager":"x"}]}}]`, "/metadata/managedFields"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &patchClient{obj: orig}
			fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
			cmd := NewPatch(mustKind(t, "deployment"), fac, nil)
			_, _, err := planPatch(t, cmd, input{args: []string{"web"}, s: map[string]string{
				"cluster": "prod", "namespace": "ns1", "patch": tc.ops,
			}})
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			require.ErrorContains(t, err, tc.want)
			assert.Empty(t, client.getName, "refused before any request is made")
		})
	}
}

// TestPatchSendsTheFetchedManagedFieldsUnchanged asserts that whatever a patch
// does to metadata, the object sent to the server carries the managed
// fields exactly as fetched: a replace of /metadata that omits them would
// otherwise reset field ownership on the server.
func TestPatchSendsTheFetchedManagedFieldsUnchanged(t *testing.T) {
	t.Parallel()
	orig := objFromJSON(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","resourceVersion":"7","labels":{"a":"b"},"managedFields":[{"manager":"kubectl","operation":"Update","fieldsType":"FieldsV1","fieldsV1":{"f:spec":{}}}]},"spec":{"replicas":1}}`)
	client := &patchClient{obj: orig}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	cmd := NewPatch(mustKind(t, "deployment"), fac, nil)
	_, exec, err := planPatch(t, cmd, input{args: []string{"web"}, s: map[string]string{
		"cluster": "prod", "namespace": "ns1",
		"patch": `[{"op":"replace","path":"/metadata","value":{"name":"web","labels":{"a":"c"}}}]`,
	}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	require.NotEmpty(t, client.updates, "the dry run sent the patched object")
	sent, _, err := unstructured.NestedSlice(client.updates[0].obj.Object, "metadata", "managedFields")
	require.NoError(t, err)
	want, _, _ := unstructured.NestedSlice(orig.Object, "metadata", "managedFields")
	assert.Equal(t, want, sent, "managed fields travel exactly as fetched")
	labels, _, _ := unstructured.NestedStringMap(client.updates[0].obj.Object, "metadata", "labels")
	assert.Equal(t, map[string]string{"a": "c"}, labels, "the rest of the metadata change still applies")
}

// TestPatchMoveIntoMetadataCannotForgeManagedFields asserts that a move or
// copy whose destination is /metadata, carrying a managedFields value from
// elsewhere in the object, is not caught by the path check (nothing static
// says what the moved value holds) but cannot reach the server: the fetched
// managed fields are restored after the local apply.
func TestPatchMoveIntoMetadataCannotForgeManagedFields(t *testing.T) {
	t.Parallel()
	orig := objFromJSON(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","resourceVersion":"7","managedFields":[{"manager":"kubectl","operation":"Update","fieldsType":"FieldsV1","fieldsV1":{"f:spec":{}}}]},"spec":{"replicas":1,"fake":{"name":"web","managedFields":[{"manager":"forged","operation":"Apply"}]}}}`)
	client := &patchClient{obj: orig}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	cmd := NewPatch(mustKind(t, "deployment"), fac, nil)
	_, _, err := planPatch(t, cmd, input{args: []string{"web"}, s: map[string]string{
		"cluster": "prod", "namespace": "ns1",
		"patch": `[{"op":"move","from":"/spec/fake","path":"/metadata"}]`,
	}})
	require.NoError(t, err)
	require.NotEmpty(t, client.updates)
	sent, _, _ := unstructured.NestedSlice(client.updates[0].obj.Object, "metadata", "managedFields")
	want, _, _ := unstructured.NestedSlice(orig.Object, "metadata", "managedFields")
	assert.Equal(t, want, sent, "the forged value never reaches the server")
}

func (*patchClient) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	return engine.ReviewResponse{}, errors.New("PostReview not scripted in this fake")
}

func (*patchClient) ServerResources(context.Context) ([]engine.APIResource, error) {
	return nil, errors.New("ServerResources not scripted in this fake")
}
