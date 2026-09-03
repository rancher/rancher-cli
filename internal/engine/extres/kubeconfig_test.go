package extres

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"
)

func planKubeconfig(t *testing.T, cmd runtime.Command, input runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "create kubeconfig must be a MutatingCommand")
	return mc.Plan(t.Context(), input)
}

func TestCreateKubeconfigClustersSplitAndResolution(t *testing.T) {
	t.Parallel()
	client := &fakeClient{}
	cmd := NewCreateKubeconfig(stubFactory{c: client, resolve: map[string]string{"prod": "c-prod", "staging": "c-staging"}})

	_, _, err := planKubeconfig(t, cmd, in{s: map[string]string{"clusters": "prod, staging,*,local"}})
	require.NoError(t, err)

	got, _, _ := unstructured.NestedStringSlice(client.created.Object, "spec", "clusters")
	assert.Equal(t, []string{"c-prod", "c-staging", "*", "local"}, got)
}

func TestCreateKubeconfigOmittedClustersLeavesSpecFieldAbsent(t *testing.T) {
	t.Parallel()
	client := &fakeClient{}
	cmd := NewCreateKubeconfig(stubFactory{c: client})

	_, _, err := planKubeconfig(t, cmd, in{s: map[string]string{}})
	require.NoError(t, err)

	_, ok, _ := unstructured.NestedStringSlice(client.created.Object, "spec", "clusters")
	assert.False(t, ok, "no --clusters means no spec.clusters (server default)")
}

func TestCreateKubeconfigTTLSecondsConversion(t *testing.T) {
	t.Parallel()
	client := &fakeClient{}
	cmd := NewCreateKubeconfig(stubFactory{c: client})

	_, _, err := planKubeconfig(t, cmd, in{s: map[string]string{"ttl": "30d"}})
	require.NoError(t, err)

	ttl, _, _ := unstructured.NestedInt64(client.created.Object, "spec", "ttl")
	assert.Equal(t, int64(2_592_000), ttl, "30d in seconds")
}

func TestCreateKubeconfigCurrentContextMustBeARequestedCluster(t *testing.T) {
	t.Parallel()
	client := &fakeClient{}
	cmd := NewCreateKubeconfig(stubFactory{c: client, resolve: map[string]string{"prod": "c-prod", "staging": "c-staging"}})

	_, _, err := planKubeconfig(t, cmd, in{s: map[string]string{
		"clusters": "prod", "current-context": "staging",
	}})
	require.Error(t, err)
}

func TestCreateKubeconfigCurrentContextResolvedAndAccepted(t *testing.T) {
	t.Parallel()
	client := &fakeClient{}
	cmd := NewCreateKubeconfig(stubFactory{c: client, resolve: map[string]string{"prod": "c-prod", "staging": "c-staging"}})

	_, _, err := planKubeconfig(t, cmd, in{s: map[string]string{
		"clusters": "prod,staging", "current-context": "staging",
	}})
	require.NoError(t, err)
	cc, _, _ := unstructured.NestedString(client.created.Object, "spec", "currentContext")
	assert.Equal(t, "c-staging", cc)
}

func TestCreateKubeconfigCurrentContextAllowedUnderWildcard(t *testing.T) {
	t.Parallel()
	client := &fakeClient{}
	cmd := NewCreateKubeconfig(stubFactory{c: client, resolve: map[string]string{"prod": "c-prod"}})

	_, _, err := planKubeconfig(t, cmd, in{s: map[string]string{
		"clusters": "*", "current-context": "prod",
	}})
	require.NoError(t, err, "a wildcard cluster set cannot be membership-checked client-side")
}

func TestCreateKubeconfigIncludeDefaultEntryTriState(t *testing.T) {
	t.Parallel()

	t.Run("omitted", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{}
		cmd := NewCreateKubeconfig(stubFactory{c: client})
		_, _, err := planKubeconfig(t, cmd, in{s: map[string]string{}})
		require.NoError(t, err)
		_, ok, _ := unstructured.NestedBool(client.created.Object, "spec", "includeDefaultEntry")
		assert.False(t, ok, "omitted means absent from the manifest — server default")
	})

	t.Run("true", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{}
		cmd := NewCreateKubeconfig(stubFactory{c: client})
		_, _, err := planKubeconfig(t, cmd, in{s: map[string]string{"include-default-entry": "true"}})
		require.NoError(t, err)
		v, ok, _ := unstructured.NestedBool(client.created.Object, "spec", "includeDefaultEntry")
		require.True(t, ok)
		assert.True(t, v)
	})

	t.Run("false with clusters", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{}
		cmd := NewCreateKubeconfig(stubFactory{c: client})
		_, _, err := planKubeconfig(t, cmd, in{s: map[string]string{
			"clusters": "local", "include-default-entry": "false",
		}})
		require.NoError(t, err)
		v, ok, _ := unstructured.NestedBool(client.created.Object, "spec", "includeDefaultEntry")
		require.True(t, ok)
		assert.False(t, v)
	})

	t.Run("false without clusters is VALIDATION", func(t *testing.T) {
		t.Parallel()
		cmd := NewCreateKubeconfig(stubFactory{c: &fakeClient{}})
		_, _, err := planKubeconfig(t, cmd, in{s: map[string]string{"include-default-entry": "false"}})
		require.Error(t, err, "mirrors the server rule: false with no clusters is rejected")
	})
}

func TestCreateKubeconfigDescribeAdvertisesEnum(t *testing.T) {
	t.Parallel()
	cmd := NewCreateKubeconfig(stubFactory{c: &fakeClient{}})
	d := cmd.Describe()
	for _, f := range d.Flags {
		if f.Name == "include-default-entry" {
			assert.Equal(t, []string{"true", "false"}, f.Enum,
				"the pipeline's validateInput rejects any other value before Plan ever runs")
			return
		}
	}
	t.Fatal("include-default-entry flag not declared")
}

func TestCreateKubeconfigIsDestructive(t *testing.T) {
	t.Parallel()
	cmd := NewCreateKubeconfig(stubFactory{c: &fakeClient{}})
	eff, exec, err := planKubeconfig(t, cmd, in{s: map[string]string{}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.True(t, eff.Destructive, "the kubeconfig kind is Sensitive — create must gate on --yes")
}

func TestCreateKubeconfigExecEmitsHeaderThenValueVerbatim(t *testing.T) {
	t.Parallel()
	rawKC := "apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\ncurrent-context: \"\"\n"
	client := &fakeClient{name: "kubeconfig-abc", status: map[string]any{"value": rawKC}}
	cmd := NewCreateKubeconfig(stubFactory{c: client})

	_, exec, err := planKubeconfig(t, cmd, in{s: map[string]string{}})
	require.NoError(t, err)
	require.NotNil(t, exec)

	res, err := exec(t.Context())
	require.NoError(t, err)
	require.NotNil(t, res.RawBytes)

	wantHeader := `# ext kubeconfig "kubeconfig-abc" — shown exactly once; revoke with: rancher delete kubeconfig kubeconfig-abc` + "\n"
	assert.True(t, strings.HasPrefix(string(res.RawBytes), wantHeader))
	assert.Equal(t, wantHeader+rawKC, string(res.RawBytes), "status.value must travel verbatim after the header")

	cfg, err := clientcmd.Load(res.RawBytes) // NEVER string-match the rendered YAML for correctness
	require.NoError(t, err)
	assert.NotNil(t, cfg)

	require.NotNil(t, res.CommittedContext, "a DeliveryOnceOnly Result must carry CommittedContext")
	assert.Equal(t, "Kubeconfig", res.CommittedContext.ResourceKind)
	assert.Equal(t, "kubeconfig-abc", res.CommittedContext.ResourceName)
	assert.Contains(t, res.CommittedContext.Recovery, "delete kubeconfig kubeconfig-abc")
}

// TestCreateKubeconfigDescriptorDeclaresOnceOnlySeam asserts that create
// kubeconfig opts into Delivery: DeliveryOnceOnly and restricts OutputPolicy
// to "table" — the one format its RawBytes payload actually honors, since
// renderResult writes it verbatim regardless of -o.
func TestCreateKubeconfigDescriptorDeclaresOnceOnlySeam(t *testing.T) {
	t.Parallel()
	d := NewCreateKubeconfig(stubFactory{c: &fakeClient{}}).Describe()
	assert.Equal(t, runtime.DeliveryOnceOnly, d.Delivery)
	assert.Equal(t, []string{"table"}, d.OutputPolicy.AllowedFormats)
	assert.True(t, d.OutputPolicy.Allows(""), "the bare default is honored")
	assert.True(t, d.OutputPolicy.Allows("table"), "an explicit table request is honored")
	assert.False(t, d.OutputPolicy.Allows("name"), "RawBytes never varies with -o, so name must be rejected pre-Plan")
	assert.False(t, d.OutputPolicy.Allows("json"), "RawBytes never varies with -o, so json must be rejected pre-Plan")
}

// TestCreateKubeconfigOutputPolicyRejectsJSONWithZeroRequests asserts the
// pipeline-level enforcement: `create kubeconfig -o json` is rejected
// before Plan ever runs — zero total requests, not merely zero mutation
// requests — rather than silently accepting the flag and returning raw
// kubeconfig bytes anyway.
func TestCreateKubeconfigOutputPolicyRejectsJSONWithZeroRequests(t *testing.T) {
	t.Parallel()
	client := &fakeClient{}
	cmd := NewCreateKubeconfig(stubFactory{c: client})
	var out, errBuf bytes.Buffer
	_, ce := runtime.Run(t.Context(), cmd, in{s: map[string]string{}},
		runtime.IO{Out: &out, Err: &errBuf, Format: "json", Apply: true, Yes: true}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Empty(t, client.dryRuns, "zero TOTAL requests: not even the dry-run Plan Create may fire")
	assert.Empty(t, out.String())
}

// TestCreateKubeconfigExecEmptyValueIsError mirrors createDerivedToken's
// empty-bearer guard: an absent or empty status.value must be a real error,
// never exit-0 with a header-only file that fails to parse downstream.
func TestCreateKubeconfigExecEmptyValueIsError(t *testing.T) {
	t.Parallel()

	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{name: "kubeconfig-abc"}
		cmd := NewCreateKubeconfig(stubFactory{c: client})
		_, exec, err := planKubeconfig(t, cmd, in{s: map[string]string{}})
		require.NoError(t, err)
		_, err = exec(t.Context())
		require.Error(t, err)
		ce, ok := err.(*clierr.Error)
		require.True(t, ok, "exec error must be a structured *clierr.Error")
		assert.False(t, ce.Retryable)
		details, ok := ce.Details.(clierr.CommittedContextDetails)
		require.True(t, ok)
		assert.True(t, details.Committed)
		assert.Equal(t, "Kubeconfig", details.ResourceKind)
		assert.Equal(t, "kubeconfig-abc", details.ResourceName)
		assert.Contains(t, details.Recovery, "delete kubeconfig kubeconfig-abc")
	})

	t.Run("empty string", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{name: "kubeconfig-abc", status: map[string]any{"value": ""}}
		cmd := NewCreateKubeconfig(stubFactory{c: client})
		_, exec, err := planKubeconfig(t, cmd, in{s: map[string]string{}})
		require.NoError(t, err)
		_, err = exec(t.Context())
		require.Error(t, err)
	})
}
