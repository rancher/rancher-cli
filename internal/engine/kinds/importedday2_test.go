package kinds

import (
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const day2Group = "operation.cattle.io"

// day2Rows returns the rows whose resource is in the operation.cattle.io group
// and asserts the four rows for the objects of an imported cluster are among
// them. Every guard that ranges over the set starts here, so a row added to the
// group is subject to each of them.
func day2Rows(t *testing.T) []Kind {
	t.Helper()
	var rows []Kind
	for _, k := range All() {
		if k.GVR.Group == day2Group {
			rows = append(rows, k)
		}
	}
	keys := make([]string, 0, len(rows))
	for _, k := range rows {
		keys = append(keys, k.Key)
	}
	for _, want := range []string{"etcdsnapshotsave", "etcdsnapshotrestore", "encryptionkeyrotation", "certificaterotation"} {
		require.Contains(t, keys, want, "the day-2 operation rows are missing a row")
	}
	return rows
}

// day2Fixture is what a guard needs of a row that the table does not hold.
type day2Fixture struct {
	// needsSnapshotName is true for the row whose spec.args.name names an etcd
	// snapshot.
	needsSnapshotName bool
}

// day2Fixtures holds one fixture per row of the set. A guard that needs one
// asserts its keys equal the keys of the set (requireFixturesForEveryRow), so a
// new row of the group fails with "no fixture" and never passes unchecked.
var day2Fixtures = map[string]day2Fixture{
	"etcdsnapshotsave":      {},
	"etcdsnapshotrestore":   {needsSnapshotName: true},
	"encryptionkeyrotation": {},
	"certificaterotation":   {},
}

func requireFixturesForEveryRow(t *testing.T, rows []Kind) {
	t.Helper()
	have := slices.Sorted(maps.Keys(day2Fixtures))
	var want []string
	for _, k := range rows {
		want = append(want, k.Key)
	}
	slices.Sort(want)
	require.Equal(t, want, have, "no fixture for a row of the operation.cattle.io group, or a fixture for a row that is not one")
}

// day2Object is a manifest of the row that passes every object check: the
// namespace is the cluster's id, spec.ttl is 60 and a restore names its
// snapshot.
func day2Object(k Kind) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": day2Group + "/v1alpha1",
		"kind":       k.K8sKind,
		"metadata":   map[string]any{"namespace": "c-m-x", "generateName": "c-m-x-"},
		"spec": map[string]any{
			"ttl":        int64(60),
			"clusterRef": map[string]any{"apiVersion": "management.cattle.io/v3", "kind": "Cluster", "name": "c-m-x"},
		},
	}}
	if day2Fixtures[k.Key].needsSnapshotName {
		_ = unstructured.SetNestedField(obj.Object, "c-m-x-local-snapshot", "spec", "args", "name")
	}
	return obj
}

// TestImportedDay2RowsAreGatedByDiscovery asserts every row of the group is
// gated by discovery of its own group, version and resource, with an
// Unsupported text that names the version that added it.
func TestImportedDay2RowsAreGatedByDiscovery(t *testing.T) {
	t.Parallel()
	for _, k := range day2Rows(t) {
		c := k.Capability
		assert.Equal(t, runtime.CapabilityDiscovery, c.Tier, k.Key)
		assert.Equal(t, k.GVR.Group, c.Group, k.Key)
		assert.Equal(t, k.GVR.Version, c.Version, k.Key)
		assert.Equal(t, k.GVR.Resource, c.Feature, k.Key)
		assert.Contains(t, c.Unsupported, "which Rancher v2.1", k.Key)
		assert.Contains(t, c.Unsupported, k.K8sKind+" operation", k.Key)
	}
	byKey := map[string]string{}
	for _, k := range day2Rows(t) {
		byKey[k.Key] = k.Capability.Unsupported
	}
	assert.Equal(t, "the ETCDSnapshotSave operation, which Rancher v2.15 added", byKey["etcdsnapshotsave"])
	assert.Equal(t, "the ETCDSnapshotRestore operation, which Rancher v2.15 added", byKey["etcdsnapshotrestore"])
	assert.Equal(t, "the EncryptionKeyRotation operation, which Rancher v2.15 added", byKey["encryptionkeyrotation"])
	assert.Equal(t, "the CertificateRotation operation, which Rancher v2.16 added", byKey["certificaterotation"])
}

// TestImportedDay2RowsHaveACreateWarning asserts every row of the group has a
// CreateWarning, ends a wait on Failed and Canceled, reports a create as not
// reversible, is Deletable and sets no DeleteCascadeWarning.
func TestImportedDay2RowsHaveACreateWarning(t *testing.T) {
	t.Parallel()
	for _, k := range day2Rows(t) {
		assert.NotEmpty(t, k.CreateWarning, k.Key)
		assert.Equal(t, []string{"Failed", "Canceled"}, k.WaitFailsOn, k.Key)
		assert.True(t, k.CreateNotReversible, k.Key)
		assert.True(t, k.Deletable, k.Key)
		assert.Empty(t, k.DeleteCascadeWarning, k.Key)
		assert.NotNil(t, k.ObjectChecks, k.Key)
		assert.Equal(t, k.Key != "etcdsnapshotsave", k.Sensitive, "only the snapshot is not Sensitive: %s", k.Key)
	}
}

// TestImportedDay2CreateObjectChecks asserts the checks of the manifest alone
// for every row: spec.clusterRef names a management Cluster, the namespace is
// its id, a restore names its snapshot, a paused object is refused, and a ttl of
// 0 or none is a warning only.
func TestImportedDay2CreateObjectChecks(t *testing.T) {
	t.Parallel()
	rows := day2Rows(t)
	requireFixturesForEveryRow(t, rows)
	const badRef = "spec.clusterRef must name a management cluster: apiVersion management.cattle.io/v3, kind Cluster, as the Rancher UI sets it"
	for _, k := range rows {
		t.Run(k.Key, func(t *testing.T) {
			t.Parallel()
			check := k.ObjectChecks.Create
			require.NotNil(t, check)
			assert.Equal(t, Verdict{}, check(day2Object(k)), "the object the Rancher UI creates passes")

			for name, mutate := range map[string]func(o *unstructured.Unstructured){
				"another kind": func(o *unstructured.Unstructured) {
					_ = unstructured.SetNestedField(o.Object, "Secret", "spec", "clusterRef", "kind")
				},
				"another apiVersion": func(o *unstructured.Unstructured) {
					_ = unstructured.SetNestedField(o.Object, "provisioning.cattle.io/v1", "spec", "clusterRef", "apiVersion")
				},
				"a namespace": func(o *unstructured.Unstructured) {
					_ = unstructured.SetNestedField(o.Object, "c-m-x", "spec", "clusterRef", "namespace")
				},
				"an empty name": func(o *unstructured.Unstructured) {
					_ = unstructured.SetNestedField(o.Object, "", "spec", "clusterRef", "name")
				},
				"no clusterRef": func(o *unstructured.Unstructured) { unstructured.RemoveNestedField(o.Object, "spec", "clusterRef") },
			} {
				o := day2Object(k)
				mutate(o)
				assert.Equal(t, Verdict{Refusal: badRef}, check(o), "%s: %s", k.Key, name)
			}

			o := day2Object(k)
			o.SetNamespace("c-m-other")
			assert.Equal(t, Verdict{Refusal: "the namespace c-m-other must be the cluster's id c-m-x from spec.clusterRef.name, as the Rancher UI creates it"}, check(o))

			o = day2Object(k)
			unstructured.RemoveNestedField(o.Object, "spec", "args", "name")
			if day2Fixtures[k.Key].needsSnapshotName {
				assert.Equal(t, Verdict{Refusal: "spec.args.name must name an etcd-snapshot of the cluster; without it Rancher stops the cluster before it fails"}, check(o))
			} else {
				assert.Equal(t, Verdict{}, check(o), "only a restore needs a snapshot name")
			}

			o = day2Object(k)
			_ = unstructured.SetNestedField(o.Object, true, "spec", "paused")
			assert.Equal(t, Verdict{Refusal: fmt.Sprintf("create the %s without spec.paused: true; Rancher does not start a paused day-2 operation, and this CLI does not un-pause one that has not started", k.Name)}, check(o))
			_ = unstructured.SetNestedField(o.Object, false, "spec", "paused")
			assert.Equal(t, Verdict{}, check(o), "paused: false is accepted")

			const ttlWarning = "spec.ttl is 0: Rancher deletes this object a few seconds after the day-2 operation ends, and a wait can then answer NOT_FOUND; the Rancher UI sets spec.ttl: 60"
			for name, ttl := range map[string]any{"int64 zero": int64(0), "float zero": float64(0), "int zero": 0} {
				o = day2Object(k)
				o.Object["spec"].(map[string]any)["ttl"] = ttl
				assert.Equal(t, Verdict{Warnings: []string{ttlWarning}}, check(o), name)
			}
			o = day2Object(k)
			unstructured.RemoveNestedField(o.Object, "spec", "ttl")
			assert.Equal(t, Verdict{Warnings: []string{ttlWarning}}, check(o), "no ttl")
			for name, ttl := range map[string]any{"negative is infinite": int64(-1), "positive": float64(60), "text is left to the server": "soon"} {
				o = day2Object(k)
				_ = unstructured.SetNestedField(o.Object, ttl, "spec", "ttl")
				assert.Equal(t, Verdict{}, check(o), name)
			}
		})
	}
}

// patchedCopy returns a copy of orig with mutate applied.
func patchedCopy(orig *unstructured.Unstructured, mutate func(o *unstructured.Unstructured)) *unstructured.Unstructured {
	out := orig.DeepCopy()
	mutate(out)
	return out
}

// withPhase returns the object with status.phase set, or without one for "".
func withPhase(obj *unstructured.Unstructured, phase string) *unstructured.Unstructured {
	return patchedCopy(obj, func(o *unstructured.Unstructured) {
		if phase == "" {
			unstructured.RemoveNestedField(o.Object, "status", "phase")
			return
		}
		_ = unstructured.SetNestedField(o.Object, phase, "status", "phase")
	})
}

// TestImportedDay2PatchCheck asserts, for every row, that a change under spec
// other than spec.paused and spec.ttl is refused with the path, that a ttl
// change and pausing are accepted, and that an un-pause is refused before the
// start, warns while the operation is in progress, and is accepted with no
// warning after the end.
func TestImportedDay2PatchCheck(t *testing.T) {
	t.Parallel()
	rows := day2Rows(t)
	requireFixturesForEveryRow(t, rows)
	for _, k := range rows {
		t.Run(k.Key, func(t *testing.T) {
			t.Parallel()
			check := k.ObjectChecks.Patch
			require.NotNil(t, check)
			base := day2Object(k)
			_ = unstructured.SetNestedField(base.Object, "Rancher says", "status", "message")
			refusal := func(path string) Verdict {
				return Verdict{Refusal: fmt.Sprintf("only spec.paused and spec.ttl of a %s object can change; create a new %s to change spec.%s", k.Name, k.Name, path)}
			}

			for path, mutate := range map[string]func(o *unstructured.Unstructured){
				"clusterRef.name": func(o *unstructured.Unstructured) {
					_ = unstructured.SetNestedField(o.Object, "c-m-y", "spec", "clusterRef", "name")
				},
				"args.name": func(o *unstructured.Unstructured) {
					_ = unstructured.SetNestedField(o.Object, "other", "spec", "args", "name")
				},
				"args.services": func(o *unstructured.Unstructured) {
					_ = unstructured.SetNestedStringSlice(o.Object, []string{"etcd"}, "spec", "args", "services")
				},
				"cancel": func(o *unstructured.Unstructured) {
					_ = unstructured.SetNestedField(o.Object, true, "spec", "cancel")
				},
			} {
				assert.Equal(t, refusal(path), check(base, patchedCopy(base, mutate)), "%s: %s", k.Key, path)
			}
			assert.Equal(t, refusal("clusterRef.apiVersion"), check(base, patchedCopy(base, func(o *unstructured.Unstructured) {
				unstructured.RemoveNestedField(o.Object, "spec", "clusterRef", "apiVersion")
			})), "a removed field")
			firstPath := "clusterRef.apiVersion"
			if day2Fixtures[k.Key].needsSnapshotName {
				firstPath = "args.name"
			}
			assert.Equal(t, refusal(firstPath), check(base, patchedCopy(base, func(o *unstructured.Unstructured) {
				unstructured.RemoveNestedField(o.Object, "spec")
			})), "a removed spec names the first path, in order")

			assert.Equal(t, Verdict{}, check(base, patchedCopy(base, func(o *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(o.Object, int64(0), "spec", "ttl")
			})), "ttl")
			assert.Equal(t, Verdict{}, check(base, patchedCopy(base, func(o *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(o.Object, float64(60), "spec", "ttl")
			})), "ttl as a float of the same value is no change")
			assert.Equal(t, Verdict{}, check(base, patchedCopy(base, func(o *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(o.Object, true, "spec", "paused")
			})), "pausing")
			assert.Equal(t, Verdict{}, check(base, patchedCopy(base, func(o *unstructured.Unstructured) {
				o.SetLabels(map[string]string{"a": "b"})
				_ = unstructured.SetNestedField(o.Object, "changed", "status", "message")
			})), "metadata and status changes are not limited")

			paused := patchedCopy(base, func(o *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(o.Object, true, "spec", "paused")
			})
			unpaused := patchedCopy(paused, func(o *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(o.Object, false, "spec", "paused")
			})
			for _, phase := range []string{"", "Pending"} {
				assert.Equal(t, Verdict{Refusal: fmt.Sprintf("this %s has not started; delete it and create a new %s from a manifest, which checks the cluster again", k.Name, k.Name)},
					check(withPhase(paused, phase), withPhase(unpaused, phase)), "phase %q", phase)
			}
			assert.Equal(t, Verdict{Warnings: []string{"resuming: " + k.CreateWarning}}, check(withPhase(paused, "InProgress"), withPhase(unpaused, "InProgress")))
			for _, phase := range []string{"Succeeded", "Failed", "Canceled"} {
				assert.Equal(t, Verdict{}, check(withPhase(paused, phase), withPhase(unpaused, phase)), "phase %q", phase)
			}
			assert.Equal(t, Verdict{}, check(withPhase(unpaused, ""), withPhase(unpaused, "")), "an object that was not paused is no un-pause")
		})
	}
}

// TestImportedDay2DeleteWarning asserts, for every row, that a delete warns
// while the object has not ended (no phase, Pending, InProgress), does not warn
// after it ended, and that the warning is the row's own and not a cascade note.
func TestImportedDay2DeleteWarning(t *testing.T) {
	t.Parallel()
	rows := day2Rows(t)
	requireFixturesForEveryRow(t, rows)
	for _, k := range rows {
		t.Run(k.Key, func(t *testing.T) {
			t.Parallel()
			check := k.ObjectChecks.Delete
			require.NotNil(t, check)
			assert.Empty(t, k.DeleteCascadeWarning)
			warn := Verdict{Warnings: []string{fmt.Sprintf("deleting this %s before it ends stops Rancher's work on it part way, which can leave the cluster in an unstable state", k.Name)}}
			for _, phase := range []string{"", "Pending", "InProgress"} {
				assert.Equal(t, warn, check(withPhase(day2Object(k), phase)), "phase %q", phase)
			}
			for _, phase := range []string{"Succeeded", "Failed", "Canceled"} {
				assert.Equal(t, Verdict{}, check(withPhase(day2Object(k), phase)), "phase %q", phase)
			}
		})
	}
}

// validRow is a row that validateKinds accepts, to be spoiled one rule at a
// time.
func validRow() Kind {
	return Kind{
		Key: "a", Name: "foo", K8sKind: "Foo", Aliases: []string{"foos"},
		GVR: schema.GroupVersionResource{Group: "example.io", Version: "v1", Resource: "foos"},
	}
}

// TestValidateRejectsMisusedRowFields asserts each rule the row fields of the
// day-2 operation rows carry: ObjectChecks.Delete needs a Deletable row,
// CreateNotReversible and ObjectChecks.Create refuse NoCreateViaManifest, an
// empty WaitFailsOn entry is refused, and a discovery capability must name the
// row's own group, version and resource.
func TestValidateRejectsMisusedRowFields(t *testing.T) {
	t.Parallel()
	noop := func(*unstructured.Unstructured) Verdict { return Verdict{} }
	discovery := func(group, version, feature string) runtime.CapabilityRequirement {
		return runtime.CapabilityRequirement{Tier: runtime.CapabilityDiscovery, Group: group, Version: version, Feature: feature, Unsupported: "the foo operation"}
	}
	for _, tc := range []struct {
		name   string
		mutate func(k *Kind)
		want   string
	}{
		{"delete check without a delete", func(k *Kind) { k.ObjectChecks = &ObjectChecks{Delete: noop} }, "ObjectChecks.Delete"},
		{"delete check on a Deletable row", func(k *Kind) { k.Deletable = true; k.ObjectChecks = &ObjectChecks{Delete: noop} }, ""},
		{"not reversible with NoCreateViaManifest", func(k *Kind) { k.CreateNotReversible = true; k.NoCreateViaManifest = "use a command" }, "CreateNotReversible"},
		{"create check with NoCreateViaManifest", func(k *Kind) { k.ObjectChecks = &ObjectChecks{Create: noop}; k.NoCreateViaManifest = "use a command" }, "ObjectChecks.Create"},
		{"not reversible alone", func(k *Kind) { k.CreateNotReversible = true }, ""},
		{"empty WaitFailsOn entry", func(k *Kind) { k.WaitFailsOn = []string{"Failed", ""} }, "WaitFailsOn"},
		{"WaitFailsOn entries", func(k *Kind) { k.WaitFailsOn = []string{"Failed", "Canceled"} }, ""},
		{"discovery group differs", func(k *Kind) { k.Capability = discovery("other.io", "v1", "foos") }, "discovery capability"},
		{"discovery version differs", func(k *Kind) { k.Capability = discovery("example.io", "v2", "foos") }, "discovery capability"},
		{"discovery resource differs", func(k *Kind) { k.Capability = discovery("example.io", "v1", "bars") }, "discovery capability"},
		{"discovery names the row's resource", func(k *Kind) { k.Capability = discovery("example.io", "v1", "foos") }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			k := validRow()
			tc.mutate(&k)
			err := validateKinds([]Kind{k})
			if tc.want == "" {
				assert.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
}
