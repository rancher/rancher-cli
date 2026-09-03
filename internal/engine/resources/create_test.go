package resources

import (
	"context"
	"fmt"
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

// mapFileSource is the engine.FileSource fake: it serves manifest bytes by
// path, so create/patch tests never touch the filesystem or os.Stdin.
type mapFileSource map[string][]byte

func (m mapFileSource) Read(path string) ([]byte, error) {
	b, ok := m[path]
	if !ok {
		return nil, fmt.Errorf("no such file: %s", path)
	}
	return b, nil
}

type createCall struct {
	gvr    schema.GroupVersionResource
	ns     string
	dryRun bool
	obj    *unstructured.Unstructured
}

// createClient records Create calls and echoes the submitted object back (as
// the API server would, with its name), so PlanCreate can stamp the target.
type createClient struct {
	creates   []createCall
	createErr error
}

func (c *createClient) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (c *createClient) List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error) {
	return &unstructured.UnstructuredList{}, nil
}
func (c *createClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", nil
}
func (c *createClient) Create(_ context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	c.creates = append(c.creates, createCall{gvr: gvr, ns: ns, dryRun: dryRun, obj: obj})
	if c.createErr != nil {
		return nil, c.createErr
	}
	return obj, nil
}
func (c *createClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (c *createClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}

func planCreate(t *testing.T, cmd runtime.Command, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "create must be a MutatingCommand")
	return mc.Plan(t.Context(), in)
}

func requireValidation(t *testing.T, err error) {
	t.Helper()
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce, "want a clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

const deploymentManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: ns1
spec:
  replicas: 1
`

const projectManifest = `apiVersion: management.cattle.io/v3
kind: Project
metadata:
  name: p1
  namespace: c-abc
spec:
  displayName: p1
`

func TestCreateResourcePlansFromManifest(t *testing.T) {
	t.Parallel()
	client := &createClient{}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	cmd := NewCreate(fac, mapFileSource{"m.yaml": []byte(deploymentManifest)})

	eff, exec, err := planCreate(t, cmd, input{s: map[string]string{"filename": "m.yaml", "cluster": "prod"}})
	require.NoError(t, err)
	require.NotNil(t, exec)

	assert.Equal(t, []string{"c-abc"}, fac.clusterCalls, "client requested for the resolved cluster id")
	require.Len(t, client.creates, 1, "Plan issues one dry-run create")
	assert.True(t, client.creates[0].dryRun)
	assert.Equal(t, "ns1", client.creates[0].ns, "manifest namespace reaches the client")
	assert.Equal(t, schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, client.creates[0].gvr)

	assert.Equal(t, safety.ActionCreate, eff.Action)
	assert.Equal(t, "c-abc", eff.Cluster)
	assert.Equal(t, "deployments/web", eff.Target)
	assert.False(t, eff.Destructive)

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "Deployment", res.Kind)
	require.Len(t, client.creates, 2, "exec performs the real create")
	assert.False(t, client.creates[1].dryRun)
	assert.Equal(t, "ns1", client.creates[1].ns)
}

func TestCreateResourceUnknownKindRejected(t *testing.T) {
	t.Parallel()
	const widget = "apiVersion: x/v1\nkind: Widget\nmetadata:\n  name: w\n"
	fac := &recFactory{client: &createClient{}}
	cmd := NewCreate(fac, mapFileSource{"m.yaml": []byte(widget)})

	_, _, err := planCreate(t, cmd, input{s: map[string]string{"filename": "m.yaml"}})
	requireValidation(t, err)
	require.ErrorContains(t, err, "Widget")
	assert.Empty(t, fac.clusterCalls, "no client is requested for an unsupported kind")
}

func TestCreateResourceMultiDocRejected(t *testing.T) {
	t.Parallel()
	multi := deploymentManifest + "---\n" + projectManifest
	fac := &recFactory{client: &createClient{}}
	cmd := NewCreate(fac, mapFileSource{"m.yaml": []byte(multi)})

	_, _, err := planCreate(t, cmd, input{s: map[string]string{"filename": "m.yaml", "cluster": "prod"}})
	requireValidation(t, err)
	require.ErrorContains(t, err, "2", "the error names the document count")
	assert.Empty(t, fac.clusterCalls)
}

func TestCreateResourceFlagManifestConflict(t *testing.T) {
	t.Parallel()

	// manifest namespace "ns1" + --namespace "other" → VALIDATION (no silent precedence).
	conflictFac := &recFactory{client: &createClient{}, resolve: map[string]string{"prod": "c-abc"}}
	_, _, err := planCreate(t, NewCreate(conflictFac, mapFileSource{"m.yaml": []byte(deploymentManifest)}),
		input{s: map[string]string{"filename": "m.yaml", "cluster": "prod", "namespace": "other"}})
	requireValidation(t, err)

	// manifest WITHOUT namespace + --namespace "team-b" → the flag fills the gap.
	const noNS = "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\nspec:\n  replicas: 1\n"
	client := &createClient{}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	_, _, err = planCreate(t, NewCreate(fac, mapFileSource{"m.yaml": []byte(noNS)}),
		input{s: map[string]string{"filename": "m.yaml", "cluster": "prod", "namespace": "team-b"}})
	require.NoError(t, err)
	require.Len(t, client.creates, 1)
	assert.Equal(t, "team-b", client.creates[0].ns, "the flag namespace fills the manifest gap")
}

func TestCreateResourceBlockScalarSeparatorNotMultiDoc(t *testing.T) {
	t.Parallel()
	const manifest = `apiVersion: v1
kind: ConfigMap
metadata:
  name: cm
  namespace: ns1
data:
  script: |
    line one
    ---
    line three
`
	client := &createClient{}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	_, _, err := planCreate(t, NewCreate(fac, mapFileSource{"m.yaml": []byte(manifest)}),
		input{s: map[string]string{"filename": "m.yaml", "cluster": "prod"}})
	require.NoError(t, err, "a --- inside a block scalar must not count as a document separator")
	require.Len(t, client.creates, 1)
}

func TestCreateResourceStdin(t *testing.T) {
	t.Parallel()
	client := &createClient{}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	cmd := NewCreate(fac, mapFileSource{"-": []byte(deploymentManifest)})

	eff, _, err := planCreate(t, cmd, input{s: map[string]string{"filename": "-", "cluster": "prod"}})
	require.NoError(t, err)
	assert.Equal(t, "deployments/web", eff.Target)
	require.Len(t, client.creates, 1)
	assert.Equal(t, "ns1", client.creates[0].ns)
}

func TestCreateResourceNSClusterManifestAsClusterID(t *testing.T) {
	t.Parallel()

	// A fully-specified project manifest (metadata.namespace c-abc) with NO
	// --cluster: the management client serves it, namespace is the cluster id.
	client := &createClient{}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	eff, _, err := planCreate(t, NewCreate(fac, mapFileSource{"m.yaml": []byte(projectManifest)}),
		input{s: map[string]string{"filename": "m.yaml"}})
	require.NoError(t, err)
	assert.Equal(t, []string{engine.LocalCluster}, fac.clusterCalls, "management client serves the create")
	require.Len(t, client.creates, 1)
	assert.Equal(t, "c-abc", client.creates[0].ns, "the manifest namespace (a cluster id) reaches the client")
	assert.Equal(t, "c-abc", eff.Cluster)

	// --cluster prod resolving to the manifest namespace c-abc → proceeds.
	okFac := &recFactory{client: &createClient{}, resolve: map[string]string{"prod": "c-abc"}}
	_, _, err = planCreate(t, NewCreate(okFac, mapFileSource{"m.yaml": []byte(projectManifest)}),
		input{s: map[string]string{"filename": "m.yaml", "cluster": "prod"}})
	require.NoError(t, err)

	// --cluster resolving to a DIFFERENT id than the manifest namespace → VALIDATION.
	badFac := &recFactory{client: &createClient{}, resolve: map[string]string{"other": "c-xyz"}}
	_, _, err = planCreate(t, NewCreate(badFac, mapFileSource{"m.yaml": []byte(projectManifest)}),
		input{s: map[string]string{"filename": "m.yaml", "cluster": "other"}})
	requireValidation(t, err)
}

func TestCreateResourceSensitiveKindIsDestructive(t *testing.T) {
	t.Parallel()
	const setting = "apiVersion: management.cattle.io/v3\nkind: Setting\nmetadata:\n  name: server-url\nvalue: https://x\n"
	client := &createClient{}
	fac := &recFactory{client: client}
	cmd := NewCreate(fac, mapFileSource{"m.yaml": []byte(setting)})

	eff, _, err := planCreate(t, cmd, input{s: map[string]string{"filename": "m.yaml"}})
	require.NoError(t, err)
	assert.True(t, eff.Destructive, "creating a sensitive kind must be destructive")
	assert.Equal(t, "settings/server-url", eff.Target)

	// A non-sensitive kind stays non-destructive.
	client2 := &createClient{}
	fac2 := &recFactory{client: client2, resolve: map[string]string{"prod": "c-abc"}}
	eff2, _, err := planCreate(t, NewCreate(fac2, mapFileSource{"m.yaml": []byte(deploymentManifest)}),
		input{s: map[string]string{"filename": "m.yaml", "cluster": "prod"}})
	require.NoError(t, err)
	assert.False(t, eff2.Destructive)
}

// TestCreateResourceRedactsUserPassword asserts the create-response redaction
// seam: a User manifest's password reaches the server (the create client
// echoes back exactly what it submitted, as a real server does), but the
// rendered Result never carries it.
func TestCreateResourceRedactsUserPassword(t *testing.T) {
	t.Parallel()
	const userManifest = "apiVersion: management.cattle.io/v3\nkind: User\n" +
		"metadata:\n  name: u-test\n" +
		"username: alice\npassword: hunter2\n"
	client := &createClient{}
	fac := &recFactory{client: client}
	cmd := NewCreate(fac, mapFileSource{"m.yaml": []byte(userManifest)})

	_, exec, err := planCreate(t, cmd, input{s: map[string]string{"filename": "m.yaml"}})
	require.NoError(t, err)

	res, err := exec(t.Context())
	require.NoError(t, err)
	obj, ok := res.Data.(map[string]any)
	require.True(t, ok)
	_, present := obj["password"]
	assert.False(t, present, "password must be scrubbed from the create response")
	assert.Equal(t, "alice", obj["username"], "unrelated fields survive redaction")
}

// TestCreateResourceRedactsSecretPayload proves the Sensitive-gated
// data/stringData guard applies to a create response the same way it
// already applies to patch diffs.
func TestCreateResourceRedactsSecretPayload(t *testing.T) {
	t.Parallel()
	const secretManifest = "apiVersion: v1\nkind: Secret\n" +
		"metadata:\n  name: s1\n  namespace: ns1\n" +
		"stringData:\n  k: v\n"
	client := &createClient{}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	cmd := NewCreate(fac, mapFileSource{"m.yaml": []byte(secretManifest)})

	_, exec, err := planCreate(t, cmd, input{s: map[string]string{"filename": "m.yaml", "cluster": "prod"}})
	require.NoError(t, err)

	res, err := exec(t.Context())
	require.NoError(t, err)
	obj, ok := res.Data.(map[string]any)
	require.True(t, ok)
	_, present := obj["stringData"]
	assert.False(t, present, "stringData must be scrubbed from the create response")
}

func TestCreateResourceClusterScopedRejectsNamespace(t *testing.T) {
	t.Parallel()
	const setting = "apiVersion: management.cattle.io/v3\nkind: Setting\nmetadata:\n  name: s\n  namespace: oops\n"
	fac := &recFactory{client: &createClient{}}
	_, _, err := planCreate(t, NewCreate(fac, mapFileSource{"m.yaml": []byte(setting)}),
		input{s: map[string]string{"filename": "m.yaml"}})
	requireValidation(t, err)
	assert.Empty(t, fac.clusterCalls)
}

// TestCreateResourceRejectsNoCreateViaManifestKind verifies that a manifest
// whose resolved kind has NoCreateViaManifest set is rejected with a
// VALIDATION error before any network call, and that the guidance is
// surfaced in the message (NoCreateViaManifest is prose, not a command, so
// it is folded into the message rather than carried as a hint).
func TestCreateResourceRejectsNoCreateViaManifestKind(t *testing.T) {
	t.Parallel()
	const manifest = "apiVersion: management.cattle.io/v3\nkind: Token\nmetadata:\n  name: t\n"
	fac := &recFactory{client: &createClient{}}
	_, _, err := planCreate(t, NewCreate(fac, mapFileSource{"m.yaml": []byte(manifest)}),
		input{s: map[string]string{"filename": "m.yaml"}})
	requireValidation(t, err)
	// Verify no network call was made.
	assert.Empty(t, fac.clusterCalls, "no network call should be made when the kind is rejected")
	// Verify the message references the ext kind path.
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Contains(t, ce.Message, "create token")
	assert.Empty(t, ce.Hint)
}
