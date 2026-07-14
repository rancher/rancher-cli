package provisioning

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
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

// ccInput is a full runtime.Input fake (String + Bool + Args maps) — the
// cloudcred_test.go and versions_test.go `in`/noArgsInput fakes only ever
// need one axis, but create cluster's flag surface exercises all three.
type ccInput struct {
	s map[string]string
	b map[string]bool
	a []string
}

func (i ccInput) String(k string) string { return i.s[k] }
func (i ccInput) Bool(k string) bool     { return i.b[k] }
func (i ccInput) Args() []string         { return i.a }

var _ runtime.Input = ccInput{}

// ccRecClient is a recording engine.ClusterClient fake: Get/List serve fixed
// objects (credential secrets, Settings) while Create records the built
// object exactly as PlanCreate's dry-run/real-write sequence sends it —
// mirrors projects_test.go's recClient, extended with the Get/List a
// node-driver Plan also needs (cloud-credential resolution).
type ccRecClient struct {
	objects   map[string]*unstructured.Unstructured
	list      *unstructured.UnstructuredList
	created   *unstructured.Unstructured
	createdNS string
	dryRuns   []bool
}

func (r *ccRecClient) Get(_ context.Context, _ schema.GroupVersionResource, _ string, name string) (*unstructured.Unstructured, error) {
	if obj, ok := r.objects[name]; ok {
		return obj, nil
	}
	return nil, fmt.Errorf("ccRecClient: no object named %q", name)
}

func (r *ccRecClient) List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error) {
	if r.list == nil {
		return &unstructured.UnstructuredList{}, nil
	}
	return r.list, nil
}

func (r *ccRecClient) Create(_ context.Context, _ schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	r.created, r.createdNS = obj, ns
	r.dryRuns = append(r.dryRuns, dryRun)
	return obj, nil
}

func (r *ccRecClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}

func (r *ccRecClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}

func (r *ccRecClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", nil
}

var _ engine.ClusterClient = (*ccRecClient)(nil)

func planCluster(t *testing.T, cmd runtime.Command, input runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "create cluster must be a MutatingCommand")
	return mc.Plan(context.Background(), input)
}

func requireValidation(t *testing.T, err error) *clierr.Error {
	t.Helper()
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	return ce
}

// TestCreateClusterImportedOmitsRKEConfigEntirely pins the nil-vs-{}
// discriminator (rancher cluster/controller.go:493,666 —
// NormalizeCluster(newCluster, cluster.Spec.RKEConfig == nil) treats a nil
// RKEConfig as "not managed by RKE"): an imported cluster's built object
// must carry NO spec.rkeConfig key at all, not an empty map.
func TestCreateClusterImportedOmitsRKEConfigEntirely(t *testing.T) {
	t.Parallel()
	client := &ccRecClient{}
	cmd := NewCreateCluster(fakeFactory{c: client}, fakeRootFactory{})

	eff, exec, err := planCluster(t, cmd, ccInput{a: []string{"imported-1"}, b: map[string]bool{"import": true}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, []bool{true}, client.dryRuns)
	assert.Equal(t, safety.ActionCreate, eff.Action)
	assert.Equal(t, "clusters/imported-1", eff.Target)
	assert.Equal(t, "local", eff.Cluster)

	assert.Equal(t, "fleet-default", client.createdNS)
	assert.Equal(t, "provisioning.cattle.io/v1", client.created.GetAPIVersion())
	assert.Equal(t, "Cluster", client.created.GetKind())
	assert.Equal(t, "imported-1", client.created.GetName())
	assert.Equal(t, "fleet-default", client.created.GetNamespace())

	spec, found, _ := unstructured.NestedMap(client.created.Object, "spec")
	require.True(t, found)
	_, present := spec["rkeConfig"]
	assert.False(t, present, "imported cluster must omit spec.rkeConfig entirely (nil, not {})")
}

// TestCreateClusterCustomRequiresK8sVersion proves --custom without
// --k8s-version is VALIDATION.
func TestCreateClusterCustomRequiresK8sVersion(t *testing.T) {
	t.Parallel()
	client := &ccRecClient{}
	cmd := NewCreateCluster(fakeFactory{c: client}, fakeRootFactory{})

	_, _, err := planCluster(t, cmd, ccInput{a: []string{"custom-1"}, b: map[string]bool{"custom": true}})
	ce := requireValidation(t, err)
	assert.Contains(t, ce.Message, "k8s-version")
}

// TestCreateClusterCustomHasEmptyRKEConfigNoMachinePools pins the other half
// of the nil-vs-{} discriminator: --custom's rkeConfig key IS present, as an
// empty map, and carries no machinePools.
func TestCreateClusterCustomHasEmptyRKEConfigNoMachinePools(t *testing.T) {
	t.Parallel()
	client := &ccRecClient{}
	// errRootFactory: --k8s-version is set, so Plan reaches versionWarnings;
	// this test only cares about the built object's shape, not the warning.
	cmd := NewCreateCluster(fakeFactory{c: client}, errRootFactory{})

	_, _, err := planCluster(t, cmd, ccInput{
		a: []string{"custom-1"},
		b: map[string]bool{"custom": true},
		s: map[string]string{"k8s-version": "v1.30.5+rke2r1"},
	})
	require.NoError(t, err)

	spec, found, _ := unstructured.NestedMap(client.created.Object, "spec")
	require.True(t, found)
	rkeConfig, present := spec["rkeConfig"]
	require.True(t, present, "custom cluster must carry spec.rkeConfig")
	rc, ok := rkeConfig.(map[string]any)
	require.True(t, ok)
	assert.Empty(t, rc, "custom cluster's rkeConfig must be empty (no machinePools)")
	kv, _, _ := unstructured.NestedString(client.created.Object, "spec", "kubernetesVersion")
	assert.Equal(t, "v1.30.5+rke2r1", kv)
}

// TestCreateClusterNodeDriverBuildsFullPoolAndResolvesCredential pins the
// node-driver object shape (rancher cluster_types.go:11-105,121-169,176+):
// a single machine pool with the resolved cloud-credential secret name and
// the Task 3 K8sKind casing on machineConfigRef.
func TestCreateClusterNodeDriverBuildsFullPoolAndResolvesCredential(t *testing.T) {
	t.Parallel()
	client := &ccRecClient{objects: map[string]*unstructured.Unstructured{
		"cc-abc12": secretObj("cc-abc12", map[string]string{displayNameAnnotation: "AWS Prod"}, nil),
	}}
	// errRootFactory: --k8s-version is set, so Plan reaches versionWarnings;
	// this test only cares about the built object's shape, not the warning.
	cmd := NewCreateCluster(fakeFactory{c: client}, errRootFactory{})

	eff, _, err := planCluster(t, cmd, ccInput{
		a: []string{"node-1"},
		s: map[string]string{
			"machine-config":   "amazonec2config/mycfg",
			"cloud-credential": "cc-abc12",
			"k8s-version":      "v1.30.5+rke2r1",
		},
		b: map[string]bool{"etcd": true, "controlplane": true, "worker": true},
	})
	require.NoError(t, err)
	assert.Equal(t, "clusters/node-1", eff.Target)

	credSecretName, _, _ := unstructured.NestedString(client.created.Object, "spec", "cloudCredentialSecretName")
	assert.Equal(t, "cattle-global-data:cc-abc12", credSecretName)

	pools, found, _ := unstructured.NestedSlice(client.created.Object, "spec", "rkeConfig", "machinePools")
	require.True(t, found)
	require.Len(t, pools, 1)
	pool, ok := pools[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "pool1", pool["name"], "default pool name")
	assert.Equal(t, true, pool["etcdRole"])
	assert.Equal(t, true, pool["controlPlaneRole"])
	assert.Equal(t, true, pool["workerRole"])
	assert.Equal(t, int64(1), pool["quantity"], "default quantity")
	ref, ok := pool["machineConfigRef"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Amazonec2Config", ref["kind"])
	assert.Equal(t, "mycfg", ref["name"])
}

// TestCreateClusterNodeDriverPoolNameAndQuantityFlags proves --pool-name and
// --quantity override their defaults.
func TestCreateClusterNodeDriverPoolNameAndQuantityFlags(t *testing.T) {
	t.Parallel()
	client := &ccRecClient{objects: map[string]*unstructured.Unstructured{
		"cc-abc12": secretObj("cc-abc12", nil, nil),
	}}
	// errRootFactory: --k8s-version is set, so Plan reaches versionWarnings;
	// this test only cares about the built object's shape, not the warning.
	cmd := NewCreateCluster(fakeFactory{c: client}, errRootFactory{})

	_, _, err := planCluster(t, cmd, ccInput{
		a: []string{"node-1"},
		s: map[string]string{
			"machine-config":   "azureconfig/mycfg",
			"cloud-credential": "cc-abc12",
			"k8s-version":      "v1.30.5+rke2r1",
			"pool-name":        "workers",
			"quantity":         "3",
		},
		b: map[string]bool{"worker": true},
	})
	require.NoError(t, err)

	pools, _, _ := unstructured.NestedSlice(client.created.Object, "spec", "rkeConfig", "machinePools")
	require.Len(t, pools, 1)
	pool := pools[0].(map[string]any)
	assert.Equal(t, "workers", pool["name"])
	assert.Equal(t, int64(3), pool["quantity"])
	ref := pool["machineConfigRef"].(map[string]any)
	assert.Equal(t, "AzureConfig", ref["kind"])
}

// TestCreateClusterNodeDriverPoolNoRoleIsValidation is the per-pool
// validation rule (rancher template.go:328-342): a pool with no role set is
// VALIDATION, in an "at least one role"-shaped message.
func TestCreateClusterNodeDriverPoolNoRoleIsValidation(t *testing.T) {
	t.Parallel()
	client := &ccRecClient{}
	cmd := NewCreateCluster(fakeFactory{c: client}, fakeRootFactory{})

	_, _, err := planCluster(t, cmd, ccInput{
		a: []string{"node-1"},
		s: map[string]string{
			"machine-config":   "amazonec2config/mycfg",
			"cloud-credential": "cc-abc12",
			"k8s-version":      "v1.30.5+rke2r1",
		},
	})
	ce := requireValidation(t, err)
	assert.Contains(t, ce.Message, "at least one role")
}

// TestCreateClusterNodeDriverMissingMachineConfigIsValidation.
func TestCreateClusterNodeDriverMissingMachineConfigIsValidation(t *testing.T) {
	t.Parallel()
	client := &ccRecClient{}
	cmd := NewCreateCluster(fakeFactory{c: client}, fakeRootFactory{})

	_, _, err := planCluster(t, cmd, ccInput{
		a: []string{"node-1"},
		s: map[string]string{"cloud-credential": "cc-abc12", "k8s-version": "v1.30.5+rke2r1"},
		b: map[string]bool{"worker": true},
	})
	ce := requireValidation(t, err)
	assert.Contains(t, ce.Message, "machine-config")
}

func TestCreateClusterNodeDriverMissingCloudCredentialIsValidation(t *testing.T) {
	t.Parallel()
	client := &ccRecClient{}
	cmd := NewCreateCluster(fakeFactory{c: client}, fakeRootFactory{})

	_, _, err := planCluster(t, cmd, ccInput{
		a: []string{"node-1"},
		s: map[string]string{"machine-config": "amazonec2config/mycfg", "k8s-version": "v1.30.5+rke2r1"},
		b: map[string]bool{"worker": true},
	})
	ce := requireValidation(t, err)
	assert.Contains(t, ce.Message, "cloud-credential")
}

func TestCreateClusterNodeDriverMissingK8sVersionIsValidation(t *testing.T) {
	t.Parallel()
	client := &ccRecClient{}
	cmd := NewCreateCluster(fakeFactory{c: client}, fakeRootFactory{})

	_, _, err := planCluster(t, cmd, ccInput{
		a: []string{"node-1"},
		s: map[string]string{"machine-config": "amazonec2config/mycfg", "cloud-credential": "cc-abc12"},
		b: map[string]bool{"worker": true},
	})
	ce := requireValidation(t, err)
	assert.Contains(t, ce.Message, "k8s-version")
}

func TestCreateClusterNodeDriverUnrecognizedMachineConfigKindIsValidation(t *testing.T) {
	t.Parallel()
	client := &ccRecClient{}
	cmd := NewCreateCluster(fakeFactory{c: client}, fakeRootFactory{})

	_, _, err := planCluster(t, cmd, ccInput{
		a: []string{"node-1"},
		s: map[string]string{
			"machine-config":   "pod/mycfg", // a real kind-table row, but not a machine-config
			"cloud-credential": "cc-abc12",
			"k8s-version":      "v1.30.5+rke2r1",
		},
		b: map[string]bool{"worker": true},
	})
	ce := requireValidation(t, err)
	assert.Contains(t, ce.Message, "pod")
}

func TestCreateClusterNodeDriverMalformedMachineConfigIsValidation(t *testing.T) {
	t.Parallel()
	client := &ccRecClient{}
	cmd := NewCreateCluster(fakeFactory{c: client}, fakeRootFactory{})

	_, _, err := planCluster(t, cmd, ccInput{
		a: []string{"node-1"},
		s: map[string]string{
			"machine-config":   "amazonec2config-no-slash",
			"cloud-credential": "cc-abc12",
			"k8s-version":      "v1.30.5+rke2r1",
		},
		b: map[string]bool{"worker": true},
	})
	_ = requireValidation(t, err)
}

// TestCreateClusterModeExclusivity covers every combination the brief names:
// --import + --custom, and either of those with the node-driver flags.
func TestCreateClusterModeExclusivity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		bools map[string]bool
		strs  map[string]string
	}{
		{"import-and-custom", map[string]bool{"import": true, "custom": true}, nil},
		{"import-and-machine-config", map[string]bool{"import": true}, map[string]string{"machine-config": "amazonec2config/mycfg"}},
		{"import-and-cloud-credential", map[string]bool{"import": true}, map[string]string{"cloud-credential": "cc-abc12"}},
		{"custom-and-machine-config", map[string]bool{"custom": true}, map[string]string{"machine-config": "amazonec2config/mycfg", "k8s-version": "v1.30.5+rke2r1"}},
		{"custom-and-cloud-credential", map[string]bool{"custom": true}, map[string]string{"cloud-credential": "cc-abc12", "k8s-version": "v1.30.5+rke2r1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &ccRecClient{}
			cmd := NewCreateCluster(fakeFactory{c: client}, fakeRootFactory{})
			_, _, err := planCluster(t, cmd, ccInput{a: []string{"x"}, b: tc.bools, s: tc.strs})
			_ = requireValidation(t, err)
		})
	}
}

// TestCreateClusterVersionWarningPresentWhenNotDiscovered proves the
// warn-only version check (spec D-7 / review R1-2): a --k8s-version absent
// from FetchVersions' merged list appends an Effect warning naming the
// distro defaults, PRE-confirmation.
func TestCreateClusterVersionWarningPresentWhenNotDiscovered(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(releaseHandler(t, map[string][]string{
		rke2ReleasePath: {"v1.30.5+rke2r1"},
		k3sReleasePath:  {},
	}))
	defer srv.Close()

	client := &ccRecClient{objects: map[string]*unstructured.Unstructured{
		"cc-abc12":                secretObj("cc-abc12", nil, nil),
		rke2DefaultVersionSetting: settingObj(rke2DefaultVersionSetting, "v1.30.5+rke2r1"),
	}}
	cmd := NewCreateCluster(fakeFactory{c: client}, rootsFor(srv))

	eff, _, err := planCluster(t, cmd, ccInput{
		a: []string{"node-1"},
		s: map[string]string{
			"machine-config":   "amazonec2config/mycfg",
			"cloud-credential": "cc-abc12",
			"k8s-version":      "v1.99.9+rke2r1", // not among the discovered rke2 releases
		},
		b: map[string]bool{"worker": true},
	})
	require.NoError(t, err)
	require.NotEmpty(t, eff.Warnings)
	assert.Contains(t, eff.Warnings[0], "v1.99.9+rke2r1")
}

// TestCreateClusterVersionWarningAbsentWhenDiscovered proves the converse:
// a version present in the merged list carries no warning.
func TestCreateClusterVersionWarningAbsentWhenDiscovered(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(releaseHandler(t, map[string][]string{
		rke2ReleasePath: {"v1.30.5+rke2r1"},
		k3sReleasePath:  {},
	}))
	defer srv.Close()

	client := &ccRecClient{objects: map[string]*unstructured.Unstructured{
		"cc-abc12": secretObj("cc-abc12", nil, nil),
	}}
	cmd := NewCreateCluster(fakeFactory{c: client}, rootsFor(srv))

	eff, _, err := planCluster(t, cmd, ccInput{
		a: []string{"node-1"},
		s: map[string]string{
			"machine-config":   "amazonec2config/mycfg",
			"cloud-credential": "cc-abc12",
			"k8s-version":      "v1.30.5+rke2r1",
		},
		b: map[string]bool{"worker": true},
	})
	require.NoError(t, err)
	assert.Empty(t, eff.Warnings)
}

// TestCreateClusterVersionWarningSkippedOnFetchVersionsError proves
// FetchVersions failing (an unreachable/erroring root client) degrades to no
// warning at all, silently — the server is the authority, discovery is
// advisory (never a hard failure baked into a warn-only check).
func TestCreateClusterVersionWarningSkippedOnFetchVersionsError(t *testing.T) {
	t.Parallel()
	client := &ccRecClient{objects: map[string]*unstructured.Unstructured{
		"cc-abc12": secretObj("cc-abc12", nil, nil),
	}}
	cmd := NewCreateCluster(fakeFactory{c: client}, errRootFactory{})

	eff, _, err := planCluster(t, cmd, ccInput{
		a: []string{"node-1"},
		s: map[string]string{
			"machine-config":   "amazonec2config/mycfg",
			"cloud-credential": "cc-abc12",
			"k8s-version":      "v1.30.5+rke2r1",
		},
		b: map[string]bool{"worker": true},
	})
	require.NoError(t, err, "a FetchVersions error must never fail the create")
	assert.Empty(t, eff.Warnings)
}

// errRootFactory's Root() always errors, standing in for an unreachable
// server — FetchVersions must surface this to the caller (not panic), and
// versionWarnings must degrade it to "no warning" rather than propagate it.
type errRootFactory struct{}

func (errRootFactory) Root() (engine.RootClient, error) { return nil, errors.New("no root client") }

// TestCreateClusterMissingNameIsValidation.
func TestCreateClusterMissingNameIsValidation(t *testing.T) {
	t.Parallel()
	client := &ccRecClient{}
	cmd := NewCreateCluster(fakeFactory{c: client}, fakeRootFactory{})

	_, _, err := planCluster(t, cmd, ccInput{b: map[string]bool{"import": true}})
	_ = requireValidation(t, err)
}

// TestNewCreateClusterDescribe pins the Descriptor shape.
func TestNewCreateClusterDescribe(t *testing.T) {
	t.Parallel()
	d := NewCreateCluster(fakeFactory{}, fakeRootFactory{}).Describe()
	assert.Equal(t, "create cluster", d.Name)
	assert.Equal(t, runtime.Mutating, d.CommandType)
	assert.True(t, d.Network)
	assert.True(t, d.Auth)
	require.Len(t, d.Args, 1)
	assert.True(t, d.Args[0].Required)
}
