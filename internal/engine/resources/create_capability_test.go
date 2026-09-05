package resources

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// capabilityKind returns a synthetic Deployment-shaped kind row carrying a
// capability requirement, injected via lookupGVK: the kinds table is
// closed-world, with no way to register a Capability-bearing row for a
// test, so createCmd's kind lookup is test-only DI instead.
func capabilityKind() kinds.Kind {
	return kinds.Kind{
		Key: "widget", Name: "widget",
		GVR:    schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		Policy: kinds.NSPlain, K8sKind: "Deployment", Scope: kinds.Downstream,
		Capability: runtime.CapabilityRequirement{Tier: runtime.CapabilityPrime, Feature: "widget-feature"},
	}
}

// countingFileSource wraps mapFileSource, counting Read calls so memo tests
// can prove exactly one read regardless of which caller (ResolveCapability,
// Plan) runs first.
type countingFileSource struct {
	mapFileSource
	reads int
}

func (c *countingFileSource) Read(path string) ([]byte, error) {
	c.reads++
	return c.mapFileSource.Read(path)
}

func TestResolveCapabilityReturnsRowRequirement(t *testing.T) {
	t.Parallel()
	k := capabilityKind()
	cmd := &createCmd{clients: &recFactory{client: &createClient{}}, src: mapFileSource{"m.yaml": []byte(deploymentManifest)},
		lookupGVK: func(string, string) (kinds.Kind, bool) { return k, true }}
	req, err := cmd.ResolveCapability(t.Context(), input{s: map[string]string{"filename": "m.yaml"}})
	require.NoError(t, err)
	assert.Equal(t, k.Capability, req)
}

func TestResolveCapabilityNoCreateViaManifestReturnsZero(t *testing.T) {
	t.Parallel()
	k := capabilityKind()
	k.NoCreateViaManifest = "use rancher create widget"
	cmd := &createCmd{clients: &recFactory{client: &createClient{}}, src: mapFileSource{"m.yaml": []byte(deploymentManifest)},
		lookupGVK: func(string, string) (kinds.Kind, bool) { return k, true }}
	req, err := cmd.ResolveCapability(t.Context(), input{s: map[string]string{"filename": "m.yaml"}})
	require.NoError(t, err, "a NoCreateViaManifest row must not fail ResolveCapability - Plan's own refusal handles it")
	assert.Equal(t, runtime.CapabilityRequirement{}, req)
}

// TestResolveCapabilityNoRequirementTouchesNoNetwork asserts the acceptance
// bullet directly: a manifest naming a kind without a requirement produces
// zero capability traffic. ResolveCapability's only I/O is the memoized
// manifest read; it must never touch the cluster client factory.
func TestResolveCapabilityNoRequirementTouchesNoNetwork(t *testing.T) {
	t.Parallel()
	fac := &recFactory{client: &createClient{}, resolve: map[string]string{"prod": "c-abc"}}
	cmd := NewCreate(fac, mapFileSource{"m.yaml": []byte(deploymentManifest)}).(*createCmd)
	req, err := cmd.ResolveCapability(t.Context(), input{s: map[string]string{"filename": "m.yaml", "cluster": "prod"}})
	require.NoError(t, err)
	assert.Equal(t, runtime.CapabilityRequirement{}, req)
	assert.Empty(t, fac.clusterCalls, "resolving a non-Capability kind must touch no network")
}

// TestResolveCapabilityErrorParity asserts that every manifest-shape error
// ResolveCapability can surface is exactly the VALIDATION error Plan has
// always produced - same shared parse, same envelope, one stage earlier.
func TestResolveCapabilityErrorParity(t *testing.T) {
	t.Parallel()
	t.Run("unreadable file", func(t *testing.T) {
		t.Parallel()
		cmd := NewCreate(&recFactory{client: &createClient{}}, mapFileSource{}).(*createCmd)
		_, err := cmd.ResolveCapability(t.Context(), input{s: map[string]string{"filename": "missing.yaml"}})
		requireValidation(t, err)
	})
	t.Run("multi-doc", func(t *testing.T) {
		t.Parallel()
		multi := deploymentManifest + "---\n" + projectManifest
		cmd := NewCreate(&recFactory{client: &createClient{}}, mapFileSource{"m.yaml": []byte(multi)}).(*createCmd)
		_, err := cmd.ResolveCapability(t.Context(), input{s: map[string]string{"filename": "m.yaml"}})
		requireValidation(t, err)
		require.ErrorContains(t, err, "2", "the error names the document count")
	})
	t.Run("invalid yaml", func(t *testing.T) {
		t.Parallel()
		cmd := NewCreate(&recFactory{client: &createClient{}}, mapFileSource{"m.yaml": []byte("not: [valid")}).(*createCmd)
		_, err := cmd.ResolveCapability(t.Context(), input{s: map[string]string{"filename": "m.yaml"}})
		requireValidation(t, err)
	})
	t.Run("un-tabled kind", func(t *testing.T) {
		t.Parallel()
		const widget = "apiVersion: x/v1\nkind: Widget\nmetadata:\n  name: w\n"
		cmd := NewCreate(&recFactory{client: &createClient{}}, mapFileSource{"m.yaml": []byte(widget)}).(*createCmd)
		_, err := cmd.ResolveCapability(t.Context(), input{s: map[string]string{"filename": "m.yaml"}})
		requireValidation(t, err)
		require.ErrorContains(t, err, "Widget")
	})
}

func TestParseManifestMemoizedResolveCapabilityFirst(t *testing.T) {
	t.Parallel()
	src := &countingFileSource{mapFileSource: mapFileSource{"m.yaml": []byte(deploymentManifest)}}
	fac := &recFactory{client: &createClient{}, resolve: map[string]string{"prod": "c-abc"}}
	cmd := NewCreate(fac, src).(*createCmd)
	in := input{s: map[string]string{"filename": "m.yaml", "cluster": "prod"}}

	_, err := cmd.ResolveCapability(t.Context(), in)
	require.NoError(t, err)
	_, _, err = cmd.Plan(t.Context(), in)
	require.NoError(t, err)
	assert.Equal(t, 1, src.reads, "the manifest must be read exactly once regardless of caller order")
}

func TestParseManifestMemoizedPlanFirst(t *testing.T) {
	t.Parallel()
	src := &countingFileSource{mapFileSource: mapFileSource{"m.yaml": []byte(deploymentManifest)}}
	fac := &recFactory{client: &createClient{}, resolve: map[string]string{"prod": "c-abc"}}
	cmd := NewCreate(fac, src).(*createCmd)
	in := input{s: map[string]string{"filename": "m.yaml", "cluster": "prod"}}

	_, _, err := cmd.Plan(t.Context(), in)
	require.NoError(t, err)
	_, err = cmd.ResolveCapability(t.Context(), in)
	require.NoError(t, err)
	assert.Equal(t, 1, src.reads, "Plan populating the memo first must not trigger a second read")
}

// TestParseManifestSingleReadStdin covers the memo's motivating case
// explicitly: -f - reads stdin, which is single-shot, so a second read
// would return nothing.
func TestParseManifestSingleReadStdin(t *testing.T) {
	t.Parallel()
	src := &countingFileSource{mapFileSource: mapFileSource{"-": []byte(deploymentManifest)}}
	fac := &recFactory{client: &createClient{}, resolve: map[string]string{"prod": "c-abc"}}
	cmd := NewCreate(fac, src).(*createCmd)
	in := input{s: map[string]string{"filename": "-", "cluster": "prod"}}

	_, err := cmd.ResolveCapability(t.Context(), in)
	require.NoError(t, err)
	_, _, err = cmd.Plan(t.Context(), in)
	require.NoError(t, err)
	assert.Equal(t, 1, src.reads, "stdin must be read exactly once across ResolveCapability and Plan")
}

// TestParseManifestPathMismatchIsCommandBug asserts the memo's defensive
// assertion: both callers must read the same Input's filename in the same
// invocation, so a differing path is a command bug, never a re-read.
func TestParseManifestPathMismatchIsCommandBug(t *testing.T) {
	t.Parallel()
	cmd := NewCreate(&recFactory{client: &createClient{}},
		mapFileSource{"a.yaml": []byte(deploymentManifest), "b.yaml": []byte(deploymentManifest)}).(*createCmd)
	_, err := cmd.ResolveCapability(t.Context(), input{s: map[string]string{"filename": "a.yaml"}})
	require.NoError(t, err)
	_, _, err = cmd.Plan(t.Context(), input{s: map[string]string{"filename": "b.yaml"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Contains(t, ce.Message, "command bug")
}
