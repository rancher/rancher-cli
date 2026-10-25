package resources

import (
	"bytes"
	"context"
	"errors"
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

// TestCreateResourceWithoutClusterGetsTheTextThatFits asserts a manifest of a
// downstream kind with no --cluster is refused before any request, with the
// local-cluster advice only for a kind that lives in a namespace.
func TestCreateResourceWithoutClusterGetsTheTextThatFits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		manifest string
		want     string
	}{
		{"namespaced kind", deploymentManifest, engine.ClusterRequiredMessage},
		{"cluster-scoped kind", "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: team-a\n", engine.ClusterRequiredPlainMessage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fac := &recFactory{client: &createClient{}}
			_, _, err := planCreate(t, NewCreate(fac, mapFileSource{"m.yaml": []byte(tc.manifest)}),
				input{s: map[string]string{"filename": "m.yaml"}})
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, tc.want, ce.Message)
			assert.Equal(t, engine.ClusterRequiredHint, ce.Hint)
			assert.Empty(t, fac.clusterCalls)
		})
	}
}

// TestCreateResourceRejectsNoCreateViaManifestKind verifies that a manifest
// whose resolved kind has NoCreateViaManifest set is rejected with a
// VALIDATION error before any network call, and that the guidance is
// surfaced in the message (NoCreateViaManifest is prose, not a command, so
// it is folded into the message rather than carried as a hint). The three
// rows are the two ext kinds and the v3 token, each created by a command
// that shows its value once.
func TestCreateResourceRejectsNoCreateViaManifestKind(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		manifest string
		want     string
	}{
		"management token": {
			"apiVersion: management.cattle.io/v3\nkind: Token\nmetadata:\n  name: t\n",
			"cannot create a management-token from a manifest; create tokens with '" + clierr.Rancher + " create token' (it creates an ext.cattle.io Token)",
		},
		"token": {
			"apiVersion: ext.cattle.io/v1\nkind: Token\nmetadata:\n  name: t\nspec:\n  userID: u-1\n",
			"cannot create a token from a manifest; create tokens with '" + clierr.Rancher + " create token' (it shows the token's value once)",
		},
		"kubeconfig": {
			"apiVersion: ext.cattle.io/v1\nkind: Kubeconfig\nmetadata:\n  name: k\n",
			"cannot create a kubeconfig from a manifest; create kubeconfigs with '" + clierr.Rancher + " create kubeconfig' (it prints the kubeconfig once)",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := &createClient{}
			fac := &recFactory{client: client}
			_, _, err := planCreate(t, NewCreate(fac, mapFileSource{"m.yaml": []byte(tc.manifest)}),
				input{s: map[string]string{"filename": "m.yaml"}})
			requireValidation(t, err)
			// Verify no network call was made.
			assert.Empty(t, fac.clusterCalls, "no network call should be made when the kind is rejected")
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, tc.want, ce.Message)
			assert.Empty(t, ce.Hint)
		})
	}
}

// requestFactory is recFactory that also records every cluster
// resolution: resolving a cluster name reaches the server.
type requestFactory struct {
	recFactory
	resolveCalls []string
}

func (f *requestFactory) ResolveCluster(ctx context.Context, s string) (string, error) {
	f.resolveCalls = append(f.resolveCalls, s)
	return f.recFactory.ResolveCluster(ctx, s)
}

// countingChecker is a CapabilityChecker that permits every requirement
// and counts its calls: each one is a request to the server.
type countingChecker struct{ calls int }

func (c *countingChecker) Check(context.Context, runtime.CapabilityRequirement) runtime.CapabilityDecision {
	c.calls++
	return runtime.CapabilityDecision{Outcome: runtime.CapabilityProceed}
}

// countingProjects resolves every --project to project p-1 on cluster
// c-abc, whose one namespace is ns1, and counts its lookups.
type countingProjects struct{ calls int }

func (p *countingProjects) ResolveProject(context.Context, runtime.Input, string) (runtime.Project, string, error) {
	p.calls++
	return runtime.Project{Cluster: "c-abc", ID: "p-1", DisplayName: "web",
		Namespaces: func(context.Context) (map[string]bool, error) { return map[string]bool{"ns1": true}, nil }}, "", nil
}

const namespaceManifest = "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: ns1\n"

func configMapManifest(ns string) string {
	return "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n  namespace: " + ns + "\ndata:\n  k: v\n"
}

// TestCreateResourceNamespaceWithProjectPointsAtCreateNamespace asserts a
// Namespace manifest with --project is VALIDATION from the capability
// stage, and from Route's refusal on a direct call to Plan, before any
// request or project read, with the hint naming create namespace with the
// typed values.
func TestCreateResourceNamespaceWithProjectPointsAtCreateNamespace(t *testing.T) {
	t.Parallel()
	in := input{s: map[string]string{"filename": "m.yaml", "cluster": "prod", "project": "web"}}
	wantHint := clierr.Rancher + " create namespace 'ns1' --cluster 'prod' --project 'web' --dry-run"

	client := &createClient{}
	fac := &requestFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	cmd := NewCreate(fac, mapFileSource{"m.yaml": []byte(namespaceManifest)}).(*createCmd)

	_, err := cmd.ResolveCapability(unreadProjectContext(t), in)
	requireValidation(t, err)
	ce, _ := errors.AsType[*clierr.Error](err)
	assert.Equal(t, wantHint, ce.Hint)

	_, _, err = cmd.Plan(unreadProjectContext(t), in)
	requireValidation(t, err)
	ce, _ = errors.AsType[*clierr.Error](err)
	assert.Equal(t, "--project does not place a namespace created from a manifest in a project; create it with create namespace --project", ce.Message)
	assert.Equal(t, wantHint, ce.Hint)

	assert.Empty(t, fac.clusterCalls)
	assert.Empty(t, fac.resolveCalls)
	assert.Empty(t, client.creates)
}

// TestCreateResourceConfigMapInProjectNamespacePreviews asserts a
// namespaced manifest whose namespace is one of the project's previews
// through the one dry-run create.
func TestCreateResourceConfigMapInProjectNamespacePreviews(t *testing.T) {
	t.Parallel()
	client := &createClient{}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	ctx, reads := projectContext(t, "ns1")
	cmd := NewCreate(fac, mapFileSource{"m.yaml": []byte(configMapManifest("ns1"))})

	eff, exec, err := cmd.(runtime.MutatingCommand).Plan(ctx, input{s: map[string]string{"filename": "m.yaml", "cluster": "prod", "project": "web"}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, "configmaps/cm", eff.Target)
	assert.Equal(t, "c-abc", eff.Cluster)
	assert.Equal(t, int32(1), reads.Load(), "the namespace is checked against the project's namespaces")
	require.Len(t, client.creates, 1)
	assert.True(t, client.creates[0].dryRun)
	assert.Equal(t, "ns1", client.creates[0].ns)
}

// TestCreateResourceConfigMapOutsideProjectIsValidation asserts a
// namespaced manifest whose namespace is not one of the project's is
// refused before the dry-run create.
func TestCreateResourceConfigMapOutsideProjectIsValidation(t *testing.T) {
	t.Parallel()
	client := &createClient{}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	ctx, _ := projectContext(t, "ns1")
	cmd := NewCreate(fac, mapFileSource{"m.yaml": []byte(configMapManifest("ns-other"))})

	_, _, err := cmd.(runtime.MutatingCommand).Plan(ctx, input{s: map[string]string{"filename": "m.yaml", "cluster": "prod", "project": "web"}})
	requireValidation(t, err)
	require.ErrorContains(t, err, "ns-other")
	require.ErrorContains(t, err, "p-1")
	assert.Empty(t, client.creates)
}

// TestCreateResourceProjectRefusalPrecedesEveryRequest asserts, through
// the pipeline, that create resource refuses --project for every manifest
// family it does not apply to before any request: no cluster resolution,
// no cluster client, no capability check, no project lookup, no dry-run
// create. The last row is the control: an allowed create through the same
// harness reaches the project lookup and the dry-run create.
func TestCreateResourceProjectRefusalPrecedesEveryRequest(t *testing.T) {
	t.Parallel()
	const prtbManifest = "apiVersion: management.cattle.io/v3\nkind: ProjectRoleTemplateBinding\n" +
		"metadata:\n  name: b1\n  namespace: p-1\nroleTemplateName: project-member\n"
	const gitRepoManifest = "apiVersion: fleet.cattle.io/v1alpha1\nkind: GitRepo\n" +
		"metadata:\n  name: g1\n  namespace: fleet-default\nspec:\n  repo: https://example.com/r.git\n"
	const oidcClientManifest = "apiVersion: management.cattle.io/v3\nkind: OIDCClient\nmetadata:\n  name: o1\n"
	for _, tc := range []struct {
		name     string
		manifest string
		cluster  string
		wantMsg  string
		wantHint string
	}{
		{name: "NSPlain kind that is not project scopable", manifest: prtbManifest, cluster: "prod",
			wantMsg: "--project does not apply to projectroletemplatebinding"},
		{name: "Namespace", manifest: namespaceManifest, cluster: "prod",
			wantMsg:  "--project does not place a namespace created from a manifest in a project; create it with create namespace --project",
			wantHint: "rancher create namespace 'ns1' --cluster 'prod' --project 'web' --dry-run"},
		{name: "NSCluster with --cluster and metadata.namespace", manifest: projectManifest, cluster: "prod",
			wantMsg: "--project does not apply to project"},
		{name: "NSWorkspace", manifest: gitRepoManifest,
			wantMsg: "--project does not apply to gitrepo"},
		{name: "capability row", manifest: oidcClientManifest,
			wantMsg: "--project does not apply to oidcclient"},
		{name: "control: namespaced manifest in the project", manifest: configMapManifest("ns1"), cluster: "prod"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &createClient{}
			fac := &requestFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
			checker := &countingChecker{}
			projects := &countingProjects{}
			flags := map[string]string{"filename": "m.yaml", "project": "web"}
			set := map[string]bool{"filename": true, "project": true}
			if tc.cluster != "" {
				flags["cluster"], set["cluster"] = tc.cluster, true
			}
			var out, errOut bytes.Buffer
			_, ce := runtime.Run(t.Context(), NewCreate(fac, mapFileSource{"m.yaml": []byte(tc.manifest)}),
				input{s: flags, isSet: set},
				runtime.IO{Out: &out, Err: &errOut, Format: "json", DryRun: true, Capability: checker, Projects: projects},
				safety.Binding{})

			if tc.wantMsg == "" {
				require.Nil(t, ce, "%v", ce)
				assert.Equal(t, 1, projects.calls, "the allowed create reads the project")
				require.Len(t, client.creates, 1)
				assert.True(t, client.creates[0].dryRun)
				return
			}
			require.NotNil(t, ce)
			var env struct {
				Kind string `json:"kind"`
				Data struct {
					Error clierr.Error `json:"error"`
				} `json:"data"`
			}
			decodeEnvelope(t, "json", errOut.Bytes(), &env)
			assert.Equal(t, "Error", env.Kind)
			assert.Equal(t, clierr.CodeValidation, env.Data.Error.Code)
			assert.Equal(t, tc.wantMsg, env.Data.Error.Message)
			assert.Equal(t, tc.wantHint, env.Data.Error.Hint)
			assert.Empty(t, out.String(), "nothing is previewed")

			assert.Empty(t, fac.resolveCalls, "no cluster resolution")
			assert.Empty(t, fac.clusterCalls, "no cluster client")
			assert.Zero(t, checker.calls, "no capability check")
			assert.Zero(t, projects.calls, "no project lookup")
			assert.Empty(t, client.creates, "no dry-run create")
		})
	}
}

func (*createClient) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	return engine.ReviewResponse{}, errors.New("PostReview not scripted in this fake")
}

func (*createClient) ServerResources(context.Context) ([]engine.APIResource, error) {
	return nil, errors.New("ServerResources not scripted in this fake")
}

// TestCreateResourceManifestErrorsCarryNoContent asserts that a manifest the
// YAML decoders refuse with a text that quotes the document (a scalar that
// does not fit its explicit tag, an unknown alias, a map key that is a
// collection, a repeated key) is refused as VALIDATION with one fixed text
// that holds none of the document, and that a syntax error keeps the
// decoder's text with its line number. The manifest comes in through a file
// and through stdin.
func TestCreateResourceManifestErrorsCarryNoContent(t *testing.T) {
	t.Parallel()
	const canary = "s3cretcanary9"
	head := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: x\nstringData:\n"
	for _, tc := range []struct{ name, manifest string }{
		{"a scalar that does not fit !!int", head + "  password: !!int " + canary + "\n"},
		{"a scalar that does not fit !!float", head + "  password: !!float " + canary + "\n"},
		{"a scalar that does not fit !!timestamp", head + "  password: !!timestamp " + canary + "\n"},
		{"an unknown alias", head + "  password: *" + canary + "\n"},
		{"a map key that is a map", head + "  ? {a: " + canary + "}\n  : c\n"},
		{"a map key that is a list", head + "  ? [" + canary + "]\n  : c\n"},
		{"a repeated key", head + "  " + canary + ": a\n  " + canary + ": b\n"},
		{"a map key that is a list of numbers", head + "  ? [12345678]\n  : c\n"},
		{"a document that is one scalar", canary + "\n"},
		{"a document that is a list", "- " + canary + "\n"},
	} {
		for _, path := range []string{"m.yaml", "-"} {
			t.Run(tc.name+" from "+path, func(t *testing.T) {
				t.Parallel()
				cmd := NewCreate(&recFactory{client: &createClient{}}, mapFileSource{path: []byte(tc.manifest)})
				_, _, err := planCreate(t, cmd, input{s: map[string]string{"filename": path, "cluster": "prod"}})
				requireValidation(t, err)
				ce, _ := errors.AsType[*clierr.Error](err)
				assert.Equal(t, "invalid manifest: the YAML could not be decoded", ce.Message)
				assert.NotContains(t, err.Error(), canary)
			})
		}
	}

	for _, tc := range []struct{ name, manifest, want string }{
		{"a syntax error on the first line, with no line number", "a: b: c\n", "invalid manifest: yaml: mapping values are not allowed in this context"},
		{"a character that cannot start a token on the first line", "a: @\n", "invalid manifest: yaml: found character that cannot start any token"},
		{"a syntax error on a later line", head + "  password: [x\n", "invalid manifest: yaml: line "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := NewCreate(&recFactory{client: &createClient{}}, mapFileSource{"m.yaml": []byte(tc.manifest)})
			_, _, err := planCreate(t, cmd, input{s: map[string]string{"filename": "m.yaml", "cluster": "prod"}})
			requireValidation(t, err)
			require.ErrorContains(t, err, tc.want)
		})
	}

	t.Run("a syntax error keeps the line", func(t *testing.T) {
		t.Parallel()
		cmd := NewCreate(&recFactory{client: &createClient{}}, mapFileSource{"m.yaml": []byte(head + "  password: [" + canary + "\n")})
		_, _, err := planCreate(t, cmd, input{s: map[string]string{"filename": "m.yaml", "cluster": "prod"}})
		requireValidation(t, err)
		require.ErrorContains(t, err, "invalid manifest: yaml: line ")
		assert.NotContains(t, err.Error(), canary)
	})
}

// TestInvalidManifestMessageStripsTheConversionPrefix asserts that a syntax
// error from the YAML to JSON conversion keeps its text without the
// conversion prefix, and any other conversion error gets the fixed text.
func TestInvalidManifestMessageStripsTheConversionPrefix(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "invalid manifest: yaml: line 6: did not find expected ',' or ']'",
		invalidManifestMessage(errors.New("error converting YAML to JSON: yaml: line 6: did not find expected ',' or ']'")))
	assert.Equal(t, "invalid manifest: the YAML could not be decoded",
		invalidManifestMessage(errors.New("error converting YAML to JSON: yaml: cannot decode !!str `x` as a !!int")))
	assert.Equal(t, "invalid manifest: yaml: mapping values are not allowed in this context",
		invalidManifestMessage(errors.New("error converting YAML to JSON: yaml: mapping values are not allowed in this context")))
	for _, problem := range []string{
		"did not find expected '!'", "did not find expected tag URI", "did not find URI escaped octet",
		"found an incorrect leading UTF-8 octet", "found an incorrect trailing UTF-8 octet",
	} {
		assert.Equal(t, "invalid manifest: yaml: "+problem, invalidManifestMessage(errors.New("yaml: "+problem)), problem)
	}
	for _, text := range []string{
		"yaml: invalid map key: []interface {}{12345678}",
		"yaml: unknown anchor 'x' referenced",
		"yaml: unmarshal errors:\n  line 7: mapping key \"password\" already defined at line 6",
		"yaml: mapping values are not allowed in this context; and 12345678",
		"mapping values are not allowed in this context",
		"yaml: ",
		"",
	} {
		assert.Equal(t, "invalid manifest: the YAML could not be decoded", invalidManifestMessage(errors.New(text)), text)
	}
	assert.Equal(t, "invalid manifest: the YAML could not be decoded",
		invalidManifestMessage(errors.New("error unmarshaling JSON: while decoding JSON: json: cannot unmarshal string into Go value of type map[string]interface {}")))
}
