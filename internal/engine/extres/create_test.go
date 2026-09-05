package extres

import (
	"context"
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

// fakeClient is a minimal engine.ClusterClient for the extres commands: it
// records every Create call (dry-run and real) and, on a real create, can
// assign a server-generated name and merge an injected status - standing in
// for the ext Token/Kubeconfig store's create-only response shape.
type fakeClient struct {
	dryRuns   []bool
	created   *unstructured.Unstructured
	createdNS string

	name   string         // assigned to the object on the real (non-dry-run) create
	status map[string]any // merged into status on the real (non-dry-run) create

	createErr error
}

func (f *fakeClient) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (f *fakeClient) List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error) {
	return nil, nil
}
func (f *fakeClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", nil
}
func (f *fakeClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (f *fakeClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}

func (f *fakeClient) Create(_ context.Context, _ schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	f.dryRuns = append(f.dryRuns, dryRun)
	f.created, f.createdNS = obj, ns
	if f.createErr != nil {
		return nil, f.createErr
	}
	out := obj.DeepCopy()
	if !dryRun {
		if f.name != "" {
			out.SetName(f.name)
		}
		if f.status != nil {
			_ = unstructured.SetNestedMap(out.Object, f.status, "status")
		}
	}
	return out, nil
}

// stubFactory hands out a fixed ClusterClient for any cluster id and resolves
// display names via an optional map (unmapped names pass through unchanged).
type stubFactory struct {
	c       *fakeClient
	resolve map[string]string
}

func (f stubFactory) Cluster(context.Context, string) (engine.ClusterClient, error) { return f.c, nil }
func (f stubFactory) ContextCluster() (string, error)                               { return "", nil }
func (f stubFactory) ContextProject() (string, error)                               { return "", nil }
func (f stubFactory) ResolveCluster(_ context.Context, s string) (string, error) {
	if id, ok := f.resolve[s]; ok {
		return id, nil
	}
	return s, nil
}

type in struct {
	s map[string]string
}

func (i in) String(k string) string  { return i.s[k] }
func (i in) Bool(string) bool        { return false }
func (i in) Strings(string) []string { return nil }
func (i in) IsSet(string) bool       { return false }
func (i in) Args() []string          { return nil }

func planToken(t *testing.T, cmd runtime.Command, input runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "create token must be a MutatingCommand")
	return mc.Plan(t.Context(), input)
}

func TestCreateTokenBuildsManifestWithMsTTLAndClusterName(t *testing.T) {
	t.Parallel()
	client := &fakeClient{}
	cmd := NewCreateToken(stubFactory{c: client, resolve: map[string]string{"testcluster": "c-x"}})

	_, _, err := planToken(t, cmd, in{s: map[string]string{
		"ttl": "12h", "description": "ci token", "cluster": "testcluster",
	}})
	require.NoError(t, err)

	require.NotNil(t, client.created)
	assert.Equal(t, "ext.cattle.io/v1", client.created.GetAPIVersion())
	assert.Equal(t, "Token", client.created.GetKind())
	assert.Empty(t, client.created.GetName(), "the store generates the name")
	assert.Empty(t, client.created.GetGenerateName(), "generateName is never set either")

	ttl, _, _ := unstructured.NestedInt64(client.created.Object, "spec", "ttl")
	assert.Equal(t, int64(43_200_000), ttl, "12h in milliseconds")
	desc, _, _ := unstructured.NestedString(client.created.Object, "spec", "description")
	assert.Equal(t, "ci token", desc)
	clusterName, _, _ := unstructured.NestedString(client.created.Object, "spec", "clusterName")
	assert.Equal(t, "c-x", clusterName, "--cluster is resolved via ResolveCluster")
}

func TestCreateTokenOmitsTTLAndClusterNameWhenAbsent(t *testing.T) {
	t.Parallel()
	client := &fakeClient{}
	cmd := NewCreateToken(stubFactory{c: client})

	_, _, err := planToken(t, cmd, in{s: map[string]string{}})
	require.NoError(t, err)

	_, ttlSet, _ := unstructured.NestedInt64(client.created.Object, "spec", "ttl")
	assert.False(t, ttlSet, "absent --ttl must not set spec.ttl (server default)")
	_, clusterSet, _ := unstructured.NestedString(client.created.Object, "spec", "clusterName")
	assert.False(t, clusterSet, "absent --cluster must not set spec.clusterName")
}

func TestCreateTokenInvalidTTLIsValidation(t *testing.T) {
	t.Parallel()
	cmd := NewCreateToken(stubFactory{c: &fakeClient{}})
	_, _, err := planToken(t, cmd, in{s: map[string]string{"ttl": "not-a-duration"}})
	require.Error(t, err)
}

func TestCreateTokenIsDestructive(t *testing.T) {
	t.Parallel()
	cmd := NewCreateToken(stubFactory{c: &fakeClient{}})
	eff, exec, err := planToken(t, cmd, in{s: map[string]string{}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.True(t, eff.Destructive, "the token kind is Sensitive - create must gate on --yes")
}

// TestCreateTokenDryRunEffectDoesNotLeak asserts the shown-once discipline at
// the Plan level: safety.Effect has no field that could carry spec/status
// payload, so the dry-run preview structurally cannot surface a bearer
// token even though PlanCreate's internal dry-run Create call round-trips
// the full object through the fake client.
func TestCreateTokenDryRunEffectDoesNotLeak(t *testing.T) {
	t.Parallel()
	client := &fakeClient{status: map[string]any{"bearerToken": "ext/token-abc:should-not-leak"}}
	cmd := NewCreateToken(stubFactory{c: client})

	eff, _, err := planToken(t, cmd, in{s: map[string]string{"description": "leak-check"}})
	require.NoError(t, err)
	assert.Equal(t, safety.ActionCreate, eff.Action)
	assert.Equal(t, "tokens/", eff.Target, "no name assigned pre-create")
	assert.NotContains(t, eff.Target, "should-not-leak")
	assert.True(t, len(client.dryRuns) >= 1 && client.dryRuns[0], "PlanCreate previews via dry-run")
}

func TestCreateTokenExecMapsBearerTokenAndWarns(t *testing.T) {
	t.Parallel()
	client := &fakeClient{
		name:   "token-xyz",
		status: map[string]any{"bearerToken": "ext/token-xyz:secretvalue", "expiresAt": "2026-08-01T00:00:00Z"},
	}
	cmd := NewCreateToken(stubFactory{c: client})

	_, exec, err := planToken(t, cmd, in{s: map[string]string{}})
	require.NoError(t, err)
	require.NotNil(t, exec)

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "TokenCreated", res.Kind)
	data, ok := res.Data.(map[string]string)
	require.True(t, ok)
	assert.Equal(t, "token-xyz", data["name"])
	assert.Equal(t, "ext/token-xyz:secretvalue", data["bearerToken"])
	assert.Equal(t, "2026-08-01T00:00:00Z", data["expiresAt"])
	// The shown-once note rides BOTH Text (below) and OnceOnlyNotice; the
	// pipeline prepends OnceOnlyNotice to Warnings before rendering, so it
	// still reaches -o json without also mirroring to stderr under
	// table/wide.
	assert.Empty(t, res.Warnings)
	assert.Contains(t, res.OnceOnlyNotice, "shown exactly once")
	assert.Equal(t, "token/token-xyz created\n  bearer token: ext/token-xyz:secretvalue   (shown once - store it now)\n  expires: 2026-08-01T00:00:00Z\n", res.Text)

	require.Len(t, client.dryRuns, 2, "one dry-run from Plan, one real create from exec")
	assert.False(t, client.dryRuns[1])

	require.NotNil(t, res.CommittedContext, "a DeliveryOnceOnly Result must carry CommittedContext")
	assert.Equal(t, "Token", res.CommittedContext.ResourceKind)
	assert.Equal(t, "token-xyz", res.CommittedContext.ResourceName)
	assert.Contains(t, res.CommittedContext.Recovery, "delete token token-xyz")
}

// TestCreateTokenDescriptorDeclaresOnceOnlySeam asserts that create token
// declares the same OutputPolicy/Delivery classification as the oidc-client
// create and add-secret verbs, excluding "name" and requiring committed context.
func TestCreateTokenDescriptorDeclaresOnceOnlySeam(t *testing.T) {
	t.Parallel()
	d := NewCreateToken(stubFactory{c: &fakeClient{}}).Describe()
	assert.Equal(t, runtime.DeliveryOnceOnly, d.Delivery)
	assert.NotContains(t, d.OutputPolicy.AllowedFormats, "name")
	for _, f := range []string{"table", "wide", "json", "yaml", "template"} {
		assert.Contains(t, d.OutputPolicy.AllowedFormats, f)
	}
}

// TestCreateTokenExecEmptyBearerTokenIsCommittedPartialSuccess asserts 3e's
// missing-value contract: an empty status.bearerToken after a committed
// Create is a non-retryable error naming the token and the delete-then-
// recreate recovery - never a bare generic failure that could read as
// retryable (retrying would create a second token).
func TestCreateTokenExecEmptyBearerTokenIsCommittedPartialSuccess(t *testing.T) {
	t.Parallel()
	client := &fakeClient{name: "token-empty", status: map[string]any{"bearerToken": ""}}
	cmd := NewCreateToken(stubFactory{c: client})

	_, exec, err := planToken(t, cmd, in{s: map[string]string{}})
	require.NoError(t, err)
	require.NotNil(t, exec)

	_, err = exec(t.Context())
	require.Error(t, err)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok, "exec error must be a structured *clierr.Error")
	assert.False(t, ce.Retryable)
	details, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok)
	assert.True(t, details.Committed)
	assert.Equal(t, "Token", details.ResourceKind)
	assert.Equal(t, "token-empty", details.ResourceName)
	assert.Contains(t, details.Recovery, "delete token token-empty")
}
