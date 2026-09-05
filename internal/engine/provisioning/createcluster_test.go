package provisioning

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/selfuser"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ccInput is a full runtime.Input fake (String + Bool + Args maps) - the
// cloudcred_test.go and versions_test.go `in`/noArgsInput fakes only ever
// need one axis, but create cluster's flag surface exercises all three.
type ccInput struct {
	s map[string]string
	b map[string]bool
	a []string
}

func (i ccInput) String(k string) string  { return i.s[k] }
func (i ccInput) Bool(k string) bool      { return i.b[k] }
func (i ccInput) Strings(string) []string { return nil }
func (i ccInput) IsSet(string) bool       { return false }
func (i ccInput) Args() []string          { return i.a }

var _ runtime.Input = ccInput{}

// ccRecClient is a recording engine.ClusterClient fake: Get/List serve fixed
// objects (credential secrets, Settings) while Create records the built
// object exactly as PlanCreate's dry-run/real-write sequence sends it -
// mirrors projects_test.go's recClient, extended with the Get/List a
// node-driver Plan also needs (cloud-credential resolution).
//
// probeErr, probeEmpty, and probeUserID control the ext.cattle.io SelfUser
// Create probe added by the creatorId stamping feature. Precedence:
// probeErr (checked first) > probeEmpty > probeUserID/default. Only one of
// these fields may be non-zero - misuse panics in Create. Calls with
// selfuser.GVR are NOT recorded into created/dryRuns - only
// provisioning-cluster creates are. When none is set, probeUserID defaults
// to "u-default": every test's created object then carries the creatorId
// annotation with that synthetic id (a deliberate side effect - a fixture
// asserting clean annotations must set probeErr or probeEmpty to opt out).
// probeEmpty returns a SelfUser WITHOUT status.userID (empty id, nil error).
//
// createErr, when non-nil, causes Create to return an error for the
// provisioning-cluster GVR (used to exercise the stamp-warning-fold-on-
// PlanCreate-error path). returnCreatorID, when non-empty, overrides the
// creatorId annotation in the object returned from a real (non-dry-run)
// provisioning-cluster create - used to test post-apply verification.
// returnNoAnnotations strips ALL annotations from that returned object
// (a trimmed create response) - post-apply verification must then skip,
// not report a false mismatch.
type ccRecClient struct {
	objects             map[string]*unstructured.Unstructured
	list                *unstructured.UnstructuredList
	created             *unstructured.Unstructured
	createdNS           string
	dryRuns             []bool
	probeErr            error
	probeEmpty          bool
	probeUserID         string
	createErr           error
	returnCreatorID     string
	returnNoAnnotations bool
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

func (r *ccRecClient) Create(_ context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	if gvr == selfuser.GVR {
		set := 0
		if r.probeErr != nil {
			set++
		}
		if r.probeEmpty {
			set++
		}
		if r.probeUserID != "" {
			set++
		}
		if set > 1 {
			panic("ccRecClient: only one of probeErr, probeEmpty, probeUserID may be set")
		}
		if r.probeErr != nil {
			return nil, r.probeErr
		}
		if r.probeEmpty {
			return obj.DeepCopy(), nil
		}
		uid := r.probeUserID
		if uid == "" {
			uid = "u-default"
		}
		result := obj.DeepCopy()
		_ = unstructured.SetNestedField(result.Object, uid, "status", "userID")
		return result, nil
	}
	if r.createErr != nil {
		return nil, r.createErr
	}
	r.created, r.createdNS = obj, ns
	r.dryRuns = append(r.dryRuns, dryRun)
	result := obj
	if !dryRun && r.returnNoAnnotations {
		result = obj.DeepCopy()
		result.SetAnnotations(nil)
	} else if !dryRun && r.returnCreatorID != "" {
		result = obj.DeepCopy()
		ann := result.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		ann[creatorIDAnnotation] = r.returnCreatorID
		result.SetAnnotations(ann)
	}
	return result, nil
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
	return mc.Plan(t.Context(), input)
}

func requireValidation(t *testing.T, err error) *clierr.Error {
	t.Helper()
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	return ce
}

// TestCreateClusterImportedOmitsRKEConfigEntirely asserts the nil-vs-{}
// discriminator (rancher cluster/controller.go:493,666 -
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

// TestCreateClusterCustomHasEmptyRKEConfigNoMachinePools asserts the other half
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

// TestCreateClusterNodeDriverBuildsFullPoolAndResolvesCredential asserts the
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

// TestMachineConfigKindsTracksKindsTable proves machineConfigKinds() returns
// exactly the five node-driver config kinds Task 3 specifies. The expected
// set is spelled out here as literal kind keys - deliberately NOT
// recomputed by filtering kinds.All() on kinds.MachineConfigGroup, which
// would just be the production code's own selection restated: a partial
// edit to only one of kinds.go's five machine-config rows (e.g. a typo'd
// GVR.Group on one row) would then change both the expectation and the
// result together, and this test would never notice. Listing the keys
// explicitly gives an independent source, so that kind of regression makes
// machineConfigKinds() drop or gain a key relative to this fixed list and
// the test fails.
func TestMachineConfigKindsTracksKindsTable(t *testing.T) {
	t.Parallel()
	want := map[string]bool{
		"amazonec2config":     true,
		"azureconfig":         true,
		"digitaloceanconfig":  true,
		"harvesterconfig":     true,
		"vmwarevsphereconfig": true,
	}

	got := machineConfigKinds()
	assert.Equal(t, want, got)
}

// TestMachineConfigFlagDescListsClosedKindSet asserts the schema-visible Desc of
// --machine-config to the closed kind set, spelled as a LITERAL - deliberately
// not recomputed via machineConfigKindsHint(), which would restate the
// production derivation (same independent-source doctrine as
// TestMachineConfigKindsTracksKindsTable). The Desc is derived from the same
// single source as the VALIDATION message, so schema and message can never
// drift; this literal is the tripwire that a kind-table edit reaches both.
func TestMachineConfigFlagDescListsClosedKindSet(t *testing.T) {
	t.Parallel()
	d := NewCreateCluster(fakeFactory{}, fakeRootFactory{}).Describe()
	var desc string
	for _, f := range d.Flags {
		if f.Name == "machine-config" {
			desc = f.Desc
		}
	}
	assert.Equal(t,
		"machine config `kind/name` for a node-driver pool; kinds: "+
			"amazonec2config, azureconfig, digitaloceanconfig, harvesterconfig, vmwarevsphereconfig",
		desc)
}

// TestMachineConfigFlagCarriesNoEnum asserts that the closed kind set is carried
// in Desc, never as ArgSpec.Enum: the flag's values are kind/name composites,
// and the runtime enum validator would reject every real value.
func TestMachineConfigFlagCarriesNoEnum(t *testing.T) {
	t.Parallel()
	d := NewCreateCluster(fakeFactory{}, fakeRootFactory{}).Describe()
	for _, f := range d.Flags {
		if f.Name == "machine-config" {
			assert.Empty(t, f.Enum, "kind/name composites must not be enum-validated")
		}
	}
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
// warning at all, silently - the server is the authority, discovery is
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
// server - FetchVersions must surface this to the caller (not panic), and
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

// TestNewCreateClusterDescribe asserts the Descriptor shape.
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

// TestCreateClusterCreatorID covers all creatorId stamping outcomes in one
// table, following the TestCreateClusterModeExclusivity convention.
//
// Cases: stamped-success (probe returns a valid id), error-degrade (probe
// errors), empty-id-degrade (probe returns no status.userID), malformed-id-
// degrade (probe returns an id failing the DNS-1123-ish pattern), and
// ordering (both a version warning and the stamp warning present - stamp
// comes last).
func TestCreateClusterCreatorID(t *testing.T) {
	t.Parallel()

	// Set up a version server reused by the ordering case only. t.Cleanup,
	// NOT defer: the subtests are parallel, so a defer would close the
	// server when this parent function returns - before the subtests run -
	// silently degrading versionWarnings to nil. t.Cleanup waits for
	// parallel subtests to finish.
	srv := httptest.NewServer(releaseHandler(t, map[string][]string{
		rke2ReleasePath: {"v1.30.5+rke2r1"},
		k3sReleasePath:  {},
	}))
	t.Cleanup(srv.Close)

	cases := []struct {
		name          string
		probeErr      error
		probeEmpty    bool
		probeUserID   string
		input         ccInput
		objects       map[string]*unstructured.Unstructured
		roots         engine.RootClientFactory
		wantStamped   string
		wantStampWarn bool
		wantStampLast bool // stamp warning must come after any other warnings
	}{
		{
			name:          "stamped-success",
			probeUserID:   "u-test",
			input:         ccInput{a: []string{"imported-1"}, b: map[string]bool{"import": true}},
			wantStamped:   "u-test",
			wantStampWarn: false,
		},
		{
			name:          "error-degrade",
			probeErr:      errors.New("rpc error"),
			input:         ccInput{a: []string{"imported-1"}, b: map[string]bool{"import": true}},
			wantStamped:   "",
			wantStampWarn: true,
		},
		{
			name:          "empty-id-degrade",
			probeEmpty:    true,
			input:         ccInput{a: []string{"imported-1"}, b: map[string]bool{"import": true}},
			wantStamped:   "",
			wantStampWarn: true,
		},
		{
			name:          "malformed-id-degrade",
			probeUserID:   "U-INVALID!",
			input:         ccInput{a: []string{"imported-1"}, b: map[string]bool{"import": true}},
			wantStamped:   "",
			wantStampWarn: true,
		},
		{
			// ordering: both a version warning (version not in discovery list)
			// and a stamp warning are present - stamp must come last.
			name:     "ordering-stamp-after-version",
			probeErr: errors.New("probe fail"),
			objects: map[string]*unstructured.Unstructured{
				rke2DefaultVersionSetting: settingObj(rke2DefaultVersionSetting, "v1.30.5+rke2r1"),
			},
			roots: rootsFor(srv),
			input: ccInput{
				a: []string{"imported-1"},
				b: map[string]bool{"import": true},
				s: map[string]string{"k8s-version": "v1.99.9+rke2r1"},
			},
			wantStampWarn: true,
			wantStampLast: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &ccRecClient{
				objects:     tc.objects,
				probeErr:    tc.probeErr,
				probeEmpty:  tc.probeEmpty,
				probeUserID: tc.probeUserID,
			}
			roots := tc.roots
			if roots == nil {
				roots = errRootFactory{}
			}
			cmd := NewCreateCluster(fakeFactory{c: client}, roots)

			eff, _, err := planCluster(t, cmd, tc.input)
			require.NoError(t, err)

			ann := client.created.GetAnnotations()
			if tc.wantStamped != "" {
				assert.Equal(t, tc.wantStamped, ann[creatorIDAnnotation])
			} else {
				assert.Empty(t, ann[creatorIDAnnotation])
			}

			count := stampWarningCount(eff.Warnings)
			if tc.wantStampWarn {
				assert.Equal(t, 1, count, "expected exactly one stamp warning")
			} else {
				assert.Equal(t, 0, count, "expected no stamp warning")
			}

			if tc.wantStampLast {
				// Hard requirements, not guarded: BOTH warnings must have
				// fired (a silent versionWarnings regression must fail this
				// case, not skip it), and the stamp warning must be last.
				require.Len(t, eff.Warnings, 2,
					"ordering case needs the version warning AND the stamp warning")
				assert.NotContains(t, eff.Warnings[0], stampWarningPrefix,
					"the first warning must be the version warning")
				assert.Contains(t, eff.Warnings[1], stampWarningPrefix,
					"stamp warning must be last among all warnings")
			}
		})
	}
}

// TestCreateClusterStampWarningFoldedIntoPlanCreateError proves that when
// the identity probe fails AND the server-side dry-run also errors, the
// stamp warning survives to what the pipeline actually renders. Real
// client errors are ALWAYS clierr-mapped, and the pipeline's toClierr
// unwraps to the innermost *clierr.Error, discarding any fmt.Errorf
// wrapper text - so for the production shape the context must ride the
// clierr Hint. The plain-error case keeps the string wrapper.
func TestCreateClusterStampWarningFoldedIntoPlanCreateError(t *testing.T) {
	t.Parallel()

	t.Run("clierr-shaped error carries the context in Hint", func(t *testing.T) {
		t.Parallel()
		client := &ccRecClient{
			probeErr:  errors.New("probe failed"),
			createErr: clierr.New(clierr.CodeValidation, "dry-run denied by webhook"),
		}
		cmd := NewCreateCluster(fakeFactory{c: client}, errRootFactory{})

		_, _, err := planCluster(t, cmd, ccInput{a: []string{"imported-1"}, b: map[string]bool{"import": true}})
		require.Error(t, err)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce,
			"the clierr must remain extractable - toClierr depends on it")
		assert.Equal(t, "dry-run denied by webhook", ce.Message)
		assert.Contains(t, ce.Hint, stampWarningPrefix,
			"the stamp context must live where the envelope renders it")
	})

	t.Run("plain error keeps the string wrapper", func(t *testing.T) {
		t.Parallel()
		client := &ccRecClient{
			probeErr:  errors.New("probe failed"),
			createErr: errors.New("dry-run denied by webhook"),
		}
		cmd := NewCreateCluster(fakeFactory{c: client}, errRootFactory{})

		_, _, err := planCluster(t, cmd, ccInput{a: []string{"imported-1"}, b: map[string]bool{"import": true}})
		require.Error(t, err)
		require.ErrorContains(t, err, "dry-run denied by webhook")
		require.ErrorContains(t, err, stampWarningPrefix)
	})
}

// TestCreateClusterPostApplyCreatorIDVerification proves that after the real
// create, the exec closure compares the server's returned creatorId against
// the value stamped before the dry-run:
//   - mismatched id → a warning naming both values is appended to Result.Warnings
//   - matching id → no warning
func TestCreateClusterPostApplyCreatorIDVerification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name            string
		probeUserID     string
		returnCreatorID string
		noAnnotations   bool
		wantWarning     bool
	}{
		{
			name:            "mismatched-creator-id-warns",
			probeUserID:     "u-stamped",
			returnCreatorID: "u-wrong",
			wantWarning:     true,
		},
		{
			name:        "matching-creator-id-no-warning",
			probeUserID: "u-stamped",
			wantWarning: false,
		},
		{
			// A trimmed create response without annotations is "can't
			// verify", never a false mismatch.
			name:          "trimmed-response-skips-verification",
			probeUserID:   "u-stamped",
			noAnnotations: true,
			wantWarning:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &ccRecClient{
				probeUserID:         tc.probeUserID,
				returnCreatorID:     tc.returnCreatorID,
				returnNoAnnotations: tc.noAnnotations,
			}
			cmd := NewCreateCluster(fakeFactory{c: client}, errRootFactory{})

			_, exec, err := planCluster(t, cmd, ccInput{a: []string{"imported-1"}, b: map[string]bool{"import": true}})
			require.NoError(t, err)
			require.NotNil(t, exec)

			result, err := exec(t.Context())
			require.NoError(t, err)

			if tc.wantWarning {
				require.NotEmpty(t, result.Warnings, "expected a creatorId mismatch warning")
				assert.Contains(t, result.Warnings[0], "creatorId on the created cluster")
				assert.Contains(t, result.Warnings[0], "u-stamped")
				assert.Contains(t, result.Warnings[0], "u-wrong")
			} else {
				for _, w := range result.Warnings {
					assert.NotContains(t, w, "creatorId on the created cluster")
				}
			}
		})
	}
}

// stampWarningCount counts warnings from the creatorId stamping degrade
// path, so tests don't couple to the total warning count (version warnings
// may legitimately appear alongside).
func stampWarningCount(warnings []string) int {
	n := 0
	for _, w := range warnings {
		if strings.Contains(w, stampWarningPrefix) {
			n++
		}
	}
	return n
}

func (*ccRecClient) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	return engine.ReviewResponse{}, errors.New("PostReview not scripted in this fake")
}

func (*ccRecClient) ServerResources(context.Context) ([]engine.APIResource, error) {
	return nil, errors.New("ServerResources not scripted in this fake")
}
