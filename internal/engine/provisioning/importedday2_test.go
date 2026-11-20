package provisioning

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	day2GroupVersionPath = "/k8s/clusters/local/apis/operation.cattle.io/v1alpha1"

	// day2ResourceList is the resource list of a server that has all four
	// day-2 operation resources: Rancher v2.16 and later.
	day2ResourceList = `{"kind":"APIResourceList","groupVersion":"operation.cattle.io/v1alpha1","resources":[` +
		`{"name":"etcdsnapshotsaves"},{"name":"etcdsnapshotrestores"},{"name":"encryptionkeyrotations"},{"name":"certificaterotations"}]}`

	// day2ResourceListV215 lacks certificaterotations, as Rancher v2.15's does.
	day2ResourceListV215 = `{"kind":"APIResourceList","groupVersion":"operation.cattle.io/v1alpha1","resources":[` +
		`{"name":"etcdsnapshotsaves"},{"name":"etcdsnapshotrestores"},{"name":"encryptionkeyrotations"}]}`
)

// day2Fixture serves what the create checks of the day-2 operation kinds read:
// a ready imported K3s management Cluster through Steve, both features on, the
// two Settings, and the resource list of a v2.16 server.
type day2Fixture struct{ *upgradeFixture }

func featureObject(name string, on bool) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"value": on},
	}}
}

// day2Management is the management Cluster c-m-x: imported, K3s, connected,
// created on 2026-11-01. edit changes its object before it is stored.
func day2Management(edit ...func(m map[string]any)) *unstructured.Unstructured {
	m := map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "Cluster",
		"metadata": map[string]any{"name": "c-m-x", "creationTimestamp": "2026-11-01T10:00:00Z", "annotations": map[string]any{}, "labels": map[string]any{}},
		"status": map[string]any{
			"driver": "k3s", "provider": "k3s",
			"conditions": []any{map[string]any{"type": "Connected", "status": "True"}},
		},
	}
	for _, e := range edit {
		e(m)
	}
	return &unstructured.Unstructured{Object: m}
}

func newDay2Fixture(t *testing.T) *day2Fixture {
	t.Helper()
	f := newUpgradeFixture(t)
	featureGVR := kindRow("feature").GVR
	f.local.put(featureGVR, "", "imported-day-2-ops", featureObject("imported-day-2-ops", true))
	f.local.put(featureGVR, "", "provisioningv2", featureObject("provisioningv2", true))
	f.local.put(settingsGVR, "", "imported-cluster-day2-ops-enabled", settingObj("imported-cluster-day2-ops-enabled", "true"))
	f.local.put(settingsGVR, "", "imported-cluster-version-management", settingObj("imported-cluster-version-management", "true"))
	f.steve.set(day2Management())
	f.steve.setGroupVersion(day2GroupVersionPath, http.StatusOK, day2ResourceList)
	return &day2Fixture{f}
}

// annotate sets annotations of the management Cluster, and label sets labels.
func annotate(kv ...string) func(m map[string]any) {
	return func(m map[string]any) {
		a := m["metadata"].(map[string]any)["annotations"].(map[string]any)
		for i := 0; i+1 < len(kv); i += 2 {
			a[kv[i]] = kv[i+1]
		}
	}
}

func label(kv ...string) func(m map[string]any) {
	return func(m map[string]any) {
		l := m["metadata"].(map[string]any)["labels"].(map[string]any)
		for i := 0; i+1 < len(kv); i += 2 {
			l[kv[i]] = kv[i+1]
		}
	}
}

func status(key string, value any) func(m map[string]any) {
	return func(m map[string]any) { m["status"].(map[string]any)[key] = value }
}

func conditions(c ...map[string]any) func(m map[string]any) {
	list := make([]any, 0, len(c))
	for _, cond := range c {
		list = append(list, cond)
	}
	return status("conditions", list)
}

// day2Snapshot is the etcd snapshot object name of cluster c-m-x.
func day2Snapshot(name, cluster, snapshotStatus, createdAt string, annotations map[string]any) *unstructured.Unstructured {
	file := map[string]any{"status": snapshotStatus, "nodeName": "node1"}
	if createdAt != "" {
		file["createdAt"] = createdAt
	}
	meta := map[string]any{"name": name, "namespace": "c-m-x"}
	if annotations != nil {
		meta["annotations"] = annotations
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rke.cattle.io/v1", "kind": "ETCDSnapshot",
		"metadata": meta, "spec": map[string]any{"clusterName": cluster}, "snapshotFile": file,
	}}
}

// putSnapshot stores the snapshot for the Get of a restore.
func (f *day2Fixture) putSnapshot(snap *unstructured.Unstructured) {
	f.local.put(kindRow("etcdsnapshot").GVR, "c-m-x", snap.GetName(), snap)
}

// day2Manifest is the manifest of a day-2 operation of kind kind for cluster
// c-m-x; edit changes its spec.
func day2Manifest(kind string, edit ...func(spec map[string]any)) *unstructured.Unstructured {
	spec := map[string]any{
		"ttl":        int64(60),
		"clusterRef": map[string]any{"apiVersion": "management.cattle.io/v3", "kind": "Cluster", "name": "c-m-x"},
	}
	if kind == "ETCDSnapshotRestore" {
		spec["args"] = map[string]any{"name": "snap"}
	}
	for _, e := range edit {
		e(spec)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "operation.cattle.io/v1alpha1", "kind": kind,
		"metadata": map[string]any{"namespace": "c-m-x", "generateName": "c-m-x-"},
		"spec":     spec,
	}}
}

func withArgs(kv ...any) func(spec map[string]any) {
	return func(spec map[string]any) {
		args, _ := spec["args"].(map[string]any)
		if args == nil {
			args = map[string]any{}
			spec["args"] = args
		}
		for i := 0; i+1 < len(kv); i += 2 {
			args[kv[i].(string)] = kv[i+1]
		}
	}
}

func (f *day2Fixture) roots() fakeRootFactory {
	return fakeRootFactory{rc: classifiedRoot{srv: f.srv}}
}

// create runs the checks of the manifest that read the server.
func (f *day2Fixture) create(t *testing.T, obj *unstructured.Unstructured) ([]string, error) {
	t.Helper()
	return checkImportedDay2Create(t.Context(), resources.CreateCheckInput{Local: f.local, Roots: f.roots(), Obj: obj})
}

// statusError is the error a client returns for a response with the status.
func statusError(code int) error {
	return clierr.FromResponse("GET the object", http.MethodGet, code, []byte(`{"message":"refused"}`), nil)
}

// TestImportedDay2RowKeysResolveThroughTheKindTable asserts every key of
// importedDay2Rows is a row of the kind table, in the group of the day-2
// operations.
func TestImportedDay2RowKeysResolveThroughTheKindTable(t *testing.T) {
	t.Parallel()
	require.Len(t, importedDay2Rows, 4)
	for _, key := range importedDay2Rows {
		k, ok := kinds.ByKey(key)
		require.True(t, ok, "importedDay2Rows names %q, which is no kind row", key)
		assert.Equal(t, "operation.cattle.io", k.GVR.Group, key)
		assert.True(t, kindRegistered(key), key)
	}
}

// day2Refusal is one case of the refusal table.
type day2Refusal struct {
	name  string
	kind  string
	setup func(f *day2Fixture)
	edit  []func(spec map[string]any)
	// object, when set, changes the manifest after it is built.
	object func(obj *unstructured.Unstructured)
	code   string
	want   string
}

const (
	notReadySuffix  = "; the Rancher UI offers day-2 operations only on a ready cluster"
	k3sServiceList  = "admin, api-server, controller-manager, scheduler, cloud-controller, etcd, auth-proxy, kubelet, kube-proxy, k3s-controller, k3s-server, supervisor"
	snapshotCreated = "2026-11-02T10:00:00Z"
)

// TestImportedDay2CreateRefusals asserts each refusal of the checks that read
// the server: its code and the message that names the annotation, Setting,
// feature, condition, snapshot or service that is missing or wrong.
func TestImportedDay2CreateRefusals(t *testing.T) {
	t.Parallel()
	steveCluster := func(edit ...func(m map[string]any)) func(f *day2Fixture) {
		return func(f *day2Fixture) { f.steve.set(day2Management(edit...)) }
	}
	snapshot := func(cluster, snapshotStatus, createdAt string, annotations map[string]any) func(f *day2Fixture) {
		return func(f *day2Fixture) {
			f.putSnapshot(day2Snapshot("snap", cluster, snapshotStatus, createdAt, annotations))
		}
	}
	disabled := func(name string) func(f *day2Fixture) {
		return func(f *day2Fixture) { f.local.put(kindRow("feature").GVR, "", name, featureObject(name, false)) }
	}
	setting := func(name, value string) func(f *day2Fixture) {
		return func(f *day2Fixture) { f.local.put(settingsGVR, "", name, settingObj(name, value)) }
	}
	restore := "ETCDSnapshotRestore"

	rows := []day2Refusal{
		{name: "imported-day-2-ops off", kind: "ETCDSnapshotSave", setup: disabled("imported-day-2-ops"), code: clierr.CodeValidation,
			want: "the imported-day-2-ops feature is off on the connected server; Rancher runs no day-2 operations on imported clusters while it is off"},
		{name: "provisioningv2 off", kind: "ETCDSnapshotSave", setup: disabled("provisioningv2"), code: clierr.CodeValidation,
			want: "the provisioningv2 feature is off on the connected server; no Rancher controller acts on day-2 operation objects while it is off"},
		{name: "annotation false", kind: "ETCDSnapshotSave", setup: steveCluster(annotate("rancher.io/operations-enabled", "false")), code: clierr.CodeValidation,
			want: `day-2 operations are off for cluster c-m-x: its annotation rancher.io/operations-enabled is "false"; Rancher runs them once the annotation is "true", and then installs its system agent on the cluster's nodes`},
		{name: "annotation absent, Setting false", kind: "ETCDSnapshotSave", setup: setting("imported-cluster-day2-ops-enabled", "false"), code: clierr.CodeValidation,
			want: `day-2 operations are off for cluster c-m-x: the setting imported-cluster-day2-ops-enabled is "false", and the cluster's annotation rancher.io/operations-enabled follows it`},
		{name: "annotation system-default, Setting false", kind: "ETCDSnapshotSave", code: clierr.CodeValidation,
			setup: func(f *day2Fixture) {
				steveCluster(annotate("rancher.io/operations-enabled", "system-default"))(f)
				setting("imported-cluster-day2-ops-enabled", "false")(f)
			},
			want: `day-2 operations are off for cluster c-m-x: the setting imported-cluster-day2-ops-enabled is "false", and the cluster's annotation rancher.io/operations-enabled follows it`},
		{name: "Connected False", kind: "ETCDSnapshotSave", code: clierr.CodeValidation,
			setup: steveCluster(conditions(map[string]any{"type": "Connected", "status": "False", "message": "agent is down"}, map[string]any{"type": "Ready", "status": "True"})),
			want:  "cluster c-m-x is not ready (condition Connected is False: agent is down)" + notReadySuffix},
		{name: "no Connected, Ready False", kind: "ETCDSnapshotSave", code: clierr.CodeValidation,
			setup: steveCluster(conditions(map[string]any{"type": "Ready", "status": "False"})),
			want:  "cluster c-m-x is not ready (condition Ready is False)" + notReadySuffix},
		{name: "neither condition", kind: "ETCDSnapshotSave", code: clierr.CodeValidation,
			setup: steveCluster(conditions()),
			want:  "cluster c-m-x is not ready (it reports no Connected or Ready condition)" + notReadySuffix},
		{name: "administrated", kind: "ETCDSnapshotSave", code: clierr.CodeValidation,
			setup: steveCluster(annotate("provisioning.cattle.io/administrated", "true")),
			want:  "cluster c-m-x is a provisioned cluster; Rancher takes its etcd snapshots and rotations through spec.rkeConfig of its provisioning cluster"},
		{name: "Turtles owner label", kind: "ETCDSnapshotSave", code: clierr.CodeValidation,
			setup: steveCluster(label("cluster-api.cattle.io/capi-cluster-owner", "x", "cluster-api.cattle.io/capi-cluster-owner-ns", "y")),
			want:  "cluster c-m-x carries the Rancher Turtles label cluster-api.cattle.io/capi-cluster-owner; this CLI does not create day-2 operations for it"},
		{name: "Turtles owner-ns label only", kind: "ETCDSnapshotSave", code: clierr.CodeValidation,
			setup: steveCluster(label("cluster-api.cattle.io/capi-cluster-owner-ns", "y")),
			want:  "cluster c-m-x carries the Rancher Turtles label cluster-api.cattle.io/capi-cluster-owner-ns; this CLI does not create day-2 operations for it"},
		{name: "the local cluster", kind: "ETCDSnapshotSave", code: clierr.CodeValidation,
			setup: func(f *day2Fixture) {
				f.steve.set(day2Management(func(m map[string]any) { m["metadata"].(map[string]any)["name"] = "local" }))
			},
			object: func(obj *unstructured.Unstructured) {
				obj.SetNamespace("local")
				_ = unstructured.SetNestedField(obj.Object, "local", "spec", "clusterRef", "name")
			},
			want: "Rancher runs day-2 operations on imported clusters, not on the local cluster"},
		{name: "Harvester", kind: "ETCDSnapshotSave", code: clierr.CodeValidation,
			setup: steveCluster(status("info", map[string]any{"machineProvider": "harvester"})),
			want:  "cluster c-m-x is a Harvester cluster; Rancher turns day-2 operations off for it"},
		{name: "driver eks with no provider label", kind: "ETCDSnapshotSave", code: clierr.CodeValidation,
			setup: steveCluster(status("driver", "EKS"), status("provider", "")),
			want:  "cluster c-m-x is not an imported RKE2 or K3s cluster; Rancher runs day-2 operations only on those"},
		{name: "restore of a snapshot of another cluster", kind: restore, code: clierr.CodeValidation,
			setup: snapshot("c-m-y", "successful", snapshotCreated, nil),
			want:  "etcd-snapshot snap belongs to cluster c-m-y, not to cluster c-m-x"},
		{name: "restore of a snapshot with no spec.clusterName", kind: restore, code: clierr.CodeValidation,
			setup: snapshot("", "successful", snapshotCreated, nil),
			want:  "etcd-snapshot snap has no spec.clusterName; the Rancher UI restores only a snapshot of cluster c-m-x"},
		{name: "restore of a failed snapshot", kind: restore, code: clierr.CodeValidation,
			setup: snapshot("c-m-x", "failed", snapshotCreated, nil),
			want:  "etcd-snapshot snap has status failed; the Rancher UI restores only a successful snapshot"},
		{name: "restore of a snapshot older than the cluster", kind: restore, code: clierr.CodeValidation,
			setup: snapshot("c-m-x", "successful", "2026-10-31T10:00:00Z", nil),
			want:  "etcd-snapshot snap was taken at 2026-10-31T10:00:00Z, at or before the time cluster c-m-x was imported into Rancher (2026-11-01T10:00:00Z); the Rancher UI does not restore a snapshot taken before the import"},
		{name: "restore of a snapshot taken when the cluster was created", kind: restore, code: clierr.CodeValidation,
			setup: snapshot("c-m-x", "successful", "2026-11-01T10:00:00Z", nil),
			want:  "etcd-snapshot snap was taken at 2026-11-01T10:00:00Z, at or before the time cluster c-m-x was imported into Rancher (2026-11-01T10:00:00Z); the Rancher UI does not restore a snapshot taken before the import"},
		{name: "restore of a snapshot with no createdAt", kind: restore, code: clierr.CodeValidation,
			setup: snapshot("c-m-x", "successful", "", nil),
			want:  "etcd-snapshot snap has no snapshotFile.createdAt; the Rancher UI does not restore it"},
		{name: "restoreMode all with version management off", kind: restore, code: clierr.CodeValidation,
			setup: func(f *day2Fixture) {
				snapshot("c-m-x", "successful", snapshotCreated, nil)(f)
				setting("imported-cluster-version-management", "false")(f)
			},
			edit: []func(spec map[string]any){withArgs("restoreMode", "all")},
			want: "spec.args.restoreMode all is not offered for etcd-snapshot snap on cluster c-m-x; the modes offered are: none"},
		{name: "a mode the snapshot's annotation does not list", kind: restore, code: clierr.CodeValidation,
			setup: snapshot("c-m-x", "successful", snapshotCreated, map[string]any{"etcdsnapshot.rke.io/restore-mode-options": "none,all"}),
			edit:  []func(spec map[string]any){withArgs("restoreMode", "kubernetesVersion")},
			want:  "spec.args.restoreMode kubernetesVersion is not offered for etcd-snapshot snap on cluster c-m-x; the modes offered are: none, all"},
		{name: "encryption key rotation with no snapshot", kind: "EncryptionKeyRotation", code: clierr.CodeValidation,
			want: "cluster c-m-x has no etcd snapshot; take one with an etcd-snapshot-save and rotate the keys once it has completed, to be able to restore the cluster if rotation fails"},
		{name: "rke2-server on a K3s cluster", kind: "CertificateRotation", code: clierr.CodeValidation,
			edit: []func(spec map[string]any){withArgs("services", []any{"etcd", "rke2-server"})},
			want: "cluster c-m-x runs K3s; it has no service rke2-server; its services are: " + k3sServiceList},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newDay2Fixture(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			if tc.kind == restore {
				if _, err := f.local.Get(t.Context(), kindRow("etcdsnapshot").GVR, "c-m-x", "snap"); err != nil {
					f.putSnapshot(day2Snapshot("snap", "c-m-x", "successful", snapshotCreated, nil))
				}
			}
			obj := day2Manifest(tc.kind, tc.edit...)
			if tc.object != nil {
				tc.object(obj)
			}
			_, err := f.create(t, obj)
			ce := asCode(t, err)
			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, tc.want, ce.Message)
		})
	}
}

// TestImportedDay2CreateOrderOfChecks asserts a management Cluster that does not
// exist is NOT_FOUND with the hint that lists clusters and sends no read of a
// feature, and that a refusal of the cluster comes before the features.
func TestImportedDay2CreateOrderOfChecks(t *testing.T) {
	t.Parallel()
	f := newDay2Fixture(t)
	obj := day2Manifest("ETCDSnapshotSave", func(spec map[string]any) {
		spec["clusterRef"].(map[string]any)["name"] = "c-m-none"
	})
	obj.SetNamespace("c-m-none")
	_, err := f.create(t, obj)
	ce := asCode(t, err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "cluster c-m-none not found", ce.Message)
	assert.Equal(t, clierr.Rancher+" access clusters", ce.Hint)
	require.Error(t, ce.Unwrap(), "the Steve 404 is the cause")
	assert.Zero(t, f.local.requestsOf("GET features"), "a cluster that does not exist sends no read of a feature")

	f = newDay2Fixture(t)
	f.steve.set(day2Management(annotate("rancher.io/operations-enabled", "false")))
	f.local.put(kindRow("feature").GVR, "", "imported-day-2-ops", featureObject("imported-day-2-ops", false))
	_, err = f.create(t, day2Manifest("ETCDSnapshotSave"))
	assert.Contains(t, asCode(t, err).Message, "day-2 operations are off for cluster c-m-x", "the cluster is checked before the features")
	assert.Zero(t, f.local.requestsOf("GET features"))
}

// TestImportedDay2CreateAccepts asserts the cases the checks accept, and the
// warnings of the ones that carry one.
func TestImportedDay2CreateAccepts(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ago := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339) }
	for _, tc := range []struct {
		name  string
		kind  string
		setup func(f *day2Fixture)
		edit  []func(spec map[string]any)
		want  []string
	}{
		{name: "an imported cluster by its label, whatever the driver", kind: "ETCDSnapshotSave",
			setup: func(f *day2Fixture) {
				f.steve.set(day2Management(status("driver", "imported"), label("provider.cattle.io", "k3s")))
			}},
		{name: "provisioningv2 is no Feature of the server", kind: "ETCDSnapshotSave",
			setup: func(f *day2Fixture) { delete(f.local.objects, "features//provisioningv2") }},
		{name: "a feature with no value warns", kind: "ETCDSnapshotSave",
			setup: func(f *day2Fixture) {
				f.local.put(kindRow("feature").GVR, "", "imported-day-2-ops", &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "imported-day-2-ops"}}})
			},
			want: []string{"could not check that the feature imported-day-2-ops is on: it has neither spec.value nor status.default"}},
		{name: "Connected True with Ready False", kind: "ETCDSnapshotSave",
			setup: func(f *day2Fixture) {
				f.steve.set(day2Management(conditions(map[string]any{"type": "Connected", "status": "True"}, map[string]any{"type": "Ready", "status": "False"})))
			}},
		{name: "annotation true with Setting false", kind: "ETCDSnapshotSave",
			setup: func(f *day2Fixture) {
				f.steve.set(day2Management(annotate("rancher.io/operations-enabled", "true")))
				f.local.put(settingsGVR, "", "imported-cluster-day2-ops-enabled", settingObj("imported-cluster-day2-ops-enabled", "false"))
			}},
		{name: "annotation system-default with Setting true", kind: "ETCDSnapshotSave",
			setup: func(f *day2Fixture) {
				f.steve.set(day2Management(annotate("rancher.io/operations-enabled", "system-default")))
			}},
		{name: "supervisor on a K3s cluster", kind: "CertificateRotation", edit: []func(spec map[string]any){withArgs("services", []any{"supervisor", "k3s-server"})}},
		{name: "an empty services list rotates every service", kind: "CertificateRotation", edit: []func(spec map[string]any){withArgs("services", []any{})}},
		{name: "rke2-server on an RKE2 cluster", kind: "CertificateRotation",
			setup: func(f *day2Fixture) { f.steve.set(day2Management(status("provider", "rke2"))) },
			edit:  []func(spec map[string]any){withArgs("services", []any{"rke2-server"})}},
		{name: "key rotation whose only snapshot failed", kind: "EncryptionKeyRotation",
			setup: func(f *day2Fixture) {
				f.local.putList(kindRow("etcdsnapshot").GVR, "c-m-x", []unstructured.Unstructured{*day2Snapshot("s1", "c-m-x", "failed", ago(time.Hour), nil)})
			}},
		{name: "key rotation with a snapshot of 23 hours", kind: "EncryptionKeyRotation",
			setup: func(f *day2Fixture) {
				f.local.putList(kindRow("etcdsnapshot").GVR, "c-m-x", []unstructured.Unstructured{*day2Snapshot("s1", "c-m-x", "successful", ago(23*time.Hour), nil)})
			}},
		{name: "key rotation with a two day old snapshot warns", kind: "EncryptionKeyRotation",
			setup: func(f *day2Fixture) {
				f.local.putList(kindRow("etcdsnapshot").GVR, "c-m-x", []unstructured.Unstructured{
					*day2Snapshot("s1", "c-m-x", "successful", ago(96*time.Hour), nil), *day2Snapshot("s2", "c-m-x", "successful", ago(49*time.Hour), nil)})
			},
			want: []string{"the newest etcd snapshot of cluster c-m-x is 2 days old; changes made since then cannot be restored if rotation fails"}},
		{name: "key rotation with a one day old snapshot warns", kind: "EncryptionKeyRotation",
			setup: func(f *day2Fixture) {
				f.local.putList(kindRow("etcdsnapshot").GVR, "c-m-x", []unstructured.Unstructured{*day2Snapshot("s1", "c-m-x", "successful", ago(30*time.Hour), nil)})
			},
			want: []string{"the newest etcd snapshot of cluster c-m-x is 1 day old; changes made since then cannot be restored if rotation fails"}},
		{name: "restore with no mode on a server with version management", kind: "ETCDSnapshotRestore",
			want: []string{
				"spec.args.restoreMode is empty: Rancher restores etcd only; the Rancher UI's default for this snapshot is all, which also writes the Kubernetes version and agent settings back",
				"Rancher restores etcd of cluster c-m-x to the snapshot taken at " + snapshotCreated + " on node node1; every change after that time is lost",
			}},
		{name: "restore with mode all", kind: "ETCDSnapshotRestore", edit: []func(spec map[string]any){withArgs("restoreMode", "all")},
			want: []string{"Rancher restores etcd of cluster c-m-x to the snapshot taken at " + snapshotCreated + " on node node1 and writes the Kubernetes version and agent settings recorded in the snapshot back onto the cluster; every change after that time is lost"}},
		{name: "restore with mode none", kind: "ETCDSnapshotRestore", edit: []func(spec map[string]any){withArgs("restoreMode", "none")},
			want: []string{"Rancher restores etcd of cluster c-m-x to the snapshot taken at " + snapshotCreated + " on node node1; every change after that time is lost"}},
		{name: "restore of a snapshot in S3", kind: "ETCDSnapshotRestore",
			setup: func(f *day2Fixture) {
				snap := day2Snapshot("snap", "c-m-x", "successful", snapshotCreated, map[string]any{"etcdsnapshot.rke.io/storage": "s3"})
				f.putSnapshot(snap)
			},
			edit: []func(spec map[string]any){withArgs("restoreMode", "none")},
			want: []string{"Rancher restores etcd of cluster c-m-x to the snapshot taken at " + snapshotCreated + " in S3; every change after that time is lost"}},
		{name: "restore on a server without certificaterotations skips the mode rules", kind: "ETCDSnapshotRestore",
			setup: func(f *day2Fixture) {
				f.steve.setGroupVersion(day2GroupVersionPath, http.StatusOK, day2ResourceListV215)
				f.local.put(settingsGVR, "", "imported-cluster-version-management", settingObj("imported-cluster-version-management", "false"))
			},
			edit: []func(spec map[string]any){withArgs("restoreMode", "all")},
			want: []string{"Rancher restores etcd of cluster c-m-x to the snapshot taken at " + snapshotCreated + " on node node1 and writes the Kubernetes version and agent settings recorded in the snapshot back onto the cluster; every change after that time is lost"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newDay2Fixture(t)
			f.putSnapshot(day2Snapshot("snap", "c-m-x", "successful", snapshotCreated, nil))
			if tc.setup != nil {
				tc.setup(f)
			}
			warnings, err := f.create(t, day2Manifest(tc.kind, tc.edit...))
			require.NoError(t, err)
			assert.Equal(t, tc.want, warnings)
		})
	}
}

// TestImportedDay2CreateReadFailuresWarn asserts every read the create checks
// make that fails is a warning with its own text and never a refusal, and that
// the checks that needed the read are left out: the management Cluster through
// Steve, each Setting, each feature, the etcd snapshot, the list of etcd
// snapshots (through Steve after a Kubernetes 403) and the group version
// document. The two 404s, of the management Cluster and of the etcd snapshot,
// are errors.
func TestImportedDay2CreateReadFailuresWarn(t *testing.T) {
	t.Parallel()
	const textPrefixCluster = "could not read cluster c-m-x to check that Rancher runs day-2 operations on it: "
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		code := fmt.Sprint(status)
		t.Run("management Cluster "+code, func(t *testing.T) {
			t.Parallel()
			f := newDay2Fixture(t)
			f.steve.set(day2Management(annotate("rancher.io/operations-enabled", "false")))
			f.steve.failCluster("c-m-x", status)
			warnings, err := f.create(t, day2Manifest("ETCDSnapshotRestore"))
			require.NoError(t, err, "the checks that need the cluster are left out")
			require.Len(t, warnings, 1)
			assert.True(t, strings.HasPrefix(warnings[0], textPrefixCluster), warnings[0])
			assert.Zero(t, f.local.requestsOf("GET etcdsnapshots"), "the snapshot is not read without the cluster")
			assert.Positive(t, f.local.requestsOf("GET features"), "the features do not need the cluster")
		})
		t.Run("Setting of day-2 operations "+code, func(t *testing.T) {
			t.Parallel()
			f := newDay2Fixture(t)
			f.local.errs["GET settings//imported-cluster-day2-ops-enabled"] = statusError(status)
			warnings, err := f.create(t, day2Manifest("ETCDSnapshotSave"))
			require.NoError(t, err)
			require.Len(t, warnings, 1)
			assert.True(t, strings.HasPrefix(warnings[0], "could not read the setting imported-cluster-day2-ops-enabled to check that Rancher runs day-2 operations on cluster c-m-x: "), warnings[0])
		})
		for _, feature := range []string{"imported-day-2-ops", "provisioningv2"} {
			t.Run("feature "+feature+" "+code, func(t *testing.T) {
				t.Parallel()
				f := newDay2Fixture(t)
				f.local.errs["GET features//"+feature] = statusError(status)
				warnings, err := f.create(t, day2Manifest("ETCDSnapshotSave"))
				require.NoError(t, err)
				require.Len(t, warnings, 1)
				assert.True(t, strings.HasPrefix(warnings[0], "could not read the feature "+feature+" to check that it is on: "), warnings[0])
			})
		}
		t.Run("etcd snapshot "+code, func(t *testing.T) {
			t.Parallel()
			f := newDay2Fixture(t)
			f.local.errs["GET etcdsnapshots/c-m-x/snap"] = statusError(status)
			warnings, err := f.create(t, day2Manifest("ETCDSnapshotRestore"))
			require.NoError(t, err)
			require.Len(t, warnings, 1, "the rest of the check is skipped")
			assert.True(t, strings.HasPrefix(warnings[0], "could not read etcd-snapshot snap to check it: "), warnings[0])
			assert.Zero(t, f.local.requestsOf("GET settings//imported-cluster-version-management"))
		})
		t.Run("Setting of version management "+code, func(t *testing.T) {
			t.Parallel()
			f := newDay2Fixture(t)
			f.putSnapshot(day2Snapshot("snap", "c-m-x", "successful", snapshotCreated, nil))
			f.local.errs["GET settings//imported-cluster-version-management"] = statusError(status)
			warnings, err := f.create(t, day2Manifest("ETCDSnapshotRestore", withArgs("restoreMode", "all")))
			require.NoError(t, err, "the mode rules are left out")
			require.Len(t, warnings, 2)
			assert.True(t, strings.HasPrefix(warnings[0], "could not read the setting imported-cluster-version-management to check spec.args.restoreMode: "), warnings[0])
			assert.True(t, strings.HasPrefix(warnings[1], "Rancher restores etcd of cluster c-m-x"), "the closing warning still follows: %s", warnings[1])
		})
		t.Run("group version document "+code, func(t *testing.T) {
			t.Parallel()
			f := newDay2Fixture(t)
			f.putSnapshot(day2Snapshot("snap", "c-m-x", "successful", snapshotCreated, nil))
			f.steve.setGroupVersion(day2GroupVersionPath, status, "")
			warnings, err := f.create(t, day2Manifest("ETCDSnapshotRestore", withArgs("restoreMode", "all")))
			require.NoError(t, err)
			require.Len(t, warnings, 2)
			assert.True(t, strings.HasPrefix(warnings[0], "could not read the operation.cattle.io API version to check spec.args.restoreMode: "), warnings[0])
		})
	}
	for name, body := range map[string]string{
		"not decodable": "this is not JSON", "empty object": `{}`, "another kind": `{"kind":"Status","status":"Failure"}`,
		"another group version": `{"kind":"APIResourceList","groupVersion":"apps/v1","resources":[{"name":"certificaterotations"}]}`,
	} {
		t.Run("group version document, "+name, func(t *testing.T) {
			t.Parallel()
			f := newDay2Fixture(t)
			f.putSnapshot(day2Snapshot("snap", "c-m-x", "successful", snapshotCreated, nil))
			f.steve.setGroupVersion(day2GroupVersionPath, http.StatusOK, body)
			warnings, err := f.create(t, day2Manifest("ETCDSnapshotRestore", withArgs("restoreMode", "all")))
			require.NoError(t, err)
			require.Len(t, warnings, 2)
			assert.True(t, strings.HasPrefix(warnings[0], "could not read the operation.cattle.io API version to check spec.args.restoreMode: "), warnings[0])
		})
	}
	t.Run("list of etcd snapshots, Kubernetes 403 then Steve answers", func(t *testing.T) {
		t.Parallel()
		f := newDay2Fixture(t)
		f.local.errs["LIST etcdsnapshots/c-m-x/"] = statusError(http.StatusForbidden)
		f.steve.setSnapshots("c-m-x", []unstructured.Unstructured{*day2Snapshot("s1", "c-m-x", "successful", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), nil)})
		warnings, err := f.create(t, day2Manifest("EncryptionKeyRotation"))
		require.NoError(t, err)
		assert.Empty(t, warnings, "the Steve list is the result")
		assert.Equal(t, 1, f.steve.snapshotRequests())
	})
	t.Run("list of etcd snapshots, Kubernetes 403 and Steve 403", func(t *testing.T) {
		t.Parallel()
		f := newDay2Fixture(t)
		f.local.errs["LIST etcdsnapshots/c-m-x/"] = statusError(http.StatusForbidden)
		f.steve.failSnapshots("c-m-x", http.StatusForbidden)
		warnings, err := f.create(t, day2Manifest("EncryptionKeyRotation"))
		require.NoError(t, err)
		require.Len(t, warnings, 1)
		assert.True(t, strings.HasPrefix(warnings[0], "could not list the etcd snapshots of cluster c-m-x to check that one exists: "), warnings[0])
	})
	t.Run("list of etcd snapshots, Kubernetes 500", func(t *testing.T) {
		t.Parallel()
		f := newDay2Fixture(t)
		f.local.errs["LIST etcdsnapshots/c-m-x/"] = statusError(http.StatusInternalServerError)
		warnings, err := f.create(t, day2Manifest("EncryptionKeyRotation"))
		require.NoError(t, err)
		require.Len(t, warnings, 1)
		assert.True(t, strings.HasPrefix(warnings[0], "could not list the etcd snapshots of cluster c-m-x to check that one exists: "), warnings[0])
		assert.Zero(t, f.steve.snapshotRequests(), "Steve is read only after a Kubernetes 403")
	})
	t.Run("etcd snapshot 404", func(t *testing.T) {
		t.Parallel()
		f := newDay2Fixture(t)
		_, err := f.create(t, day2Manifest("ETCDSnapshotRestore", withArgs("name", "nosuch")))
		ce := asCode(t, err)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Equal(t, "no etcd-snapshot nosuch in namespace c-m-x", ce.Message)
		assert.Equal(t, clierr.Rancher+" get etcd-snapshot --namespace 'c-m-x'", ce.Hint)
	})
}

// TestImportedDay2CreateRefusesRemovedFields asserts, for every row, that a
// field under spec the manifest sets and the dry-run answer lacks is refused: a
// version 2.16 field as CAPABILITY_UNSUPPORTED with the server in its details,
// any other as VALIDATION; that a null is not compared; and that a list is
// compared by the presence of its path, never per entry.
func TestImportedDay2CreateRefusesRemovedFields(t *testing.T) {
	t.Parallel()
	const server = "https://rancher.example"
	var rows []kinds.Kind
	for _, k := range kinds.All() {
		if k.GVR.Group == "operation.cattle.io" {
			rows = append(rows, k)
		}
	}
	fixtures := map[string]bool{"etcdsnapshotsave": false, "etcdsnapshotrestore": true, "encryptionkeyrotation": false, "certificaterotation": false}
	have := slices.Sorted(maps.Keys(fixtures))
	var want []string
	for _, k := range rows {
		want = append(want, k.Key)
	}
	slices.Sort(want)
	require.Equal(t, want, have, "no fixture for a row of the group, or a fixture for a row that is not one")

	check := func(obj, answer *unstructured.Unstructured) error {
		return checkImportedDay2Observed(t.Context(), resources.CreateCheckInput{Obj: obj, Server: func() string { return server }}, answer)
	}
	without := func(obj *unstructured.Unstructured, path ...string) *unstructured.Unstructured {
		answer := obj.DeepCopy()
		unstructured.RemoveNestedField(answer.Object, path...)
		return answer
	}
	for _, k := range rows {
		t.Run(k.Key, func(t *testing.T) {
			t.Parallel()
			base := day2Manifest(k.K8sKind)
			require.NoError(t, check(base, base.DeepCopy()), "an answer that keeps every field")

			cancelled := day2Manifest(k.K8sKind)
			require.NoError(t, unstructured.SetNestedField(cancelled.Object, true, "spec", "cancel"))
			ce := asCode(t, check(cancelled, without(cancelled, "spec", "cancel")))
			assert.Equal(t, clierr.CodeCapabilityUnsupported, ce.Code)
			assert.Equal(t, fmt.Sprintf("the connected server does not support spec.cancel of %s, which Rancher v2.16 added", k.K8sKind), ce.Message)
			assert.Equal(t, clierr.CapabilityUnsupportedDetails{Feature: k.GVR.Resource + ".spec.cancel", Server: server}, ce.Details)

			unknown := day2Manifest(k.K8sKind)
			require.NoError(t, unstructured.SetNestedField(unknown.Object, "x", "spec", "foo"))
			ce = asCode(t, check(unknown, without(unknown, "spec", "foo")))
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, fmt.Sprintf("the server removed spec.foo from the object: its version of %s has no such field", k.K8sKind), ce.Message)

			mode := day2Manifest(k.K8sKind, withArgs("restoreMode", "all"))
			ce = asCode(t, check(mode, without(mode, "spec", "args", "restoreMode")))
			if fixtures[k.Key] {
				assert.Equal(t, clierr.CodeCapabilityUnsupported, ce.Code)
				assert.Equal(t, "the connected server does not support spec.args.restoreMode of ETCDSnapshotRestore, which Rancher v2.16 added", ce.Message)
				assert.Equal(t, clierr.CapabilityUnsupportedDetails{Feature: "etcdsnapshotrestores.spec.args.restoreMode", Server: server}, ce.Details)
			} else {
				assert.Equal(t, clierr.CodeValidation, ce.Code, "restoreMode is a version 2.16 field of the restore only")
				assert.Equal(t, fmt.Sprintf("the server removed spec.args.restoreMode from the object: its version of %s has no such field", k.K8sKind), ce.Message)
			}

			nulls := day2Manifest(k.K8sKind)
			spec := nulls.Object["spec"].(map[string]any)
			spec["paused"] = nil
			if !fixtures[k.Key] {
				spec["args"] = nil
			} else {
				spec["args"].(map[string]any)["restoreMode"] = nil
			}
			answer := nulls.DeepCopy()
			unstructured.RemoveNestedField(answer.Object, "spec", "paused")
			if !fixtures[k.Key] {
				unstructured.RemoveNestedField(answer.Object, "spec", "args")
			} else {
				unstructured.RemoveNestedField(answer.Object, "spec", "args", "restoreMode")
			}
			require.NoError(t, check(nulls, answer), "a null is not compared")

			lists := day2Manifest(k.K8sKind, withArgs("services", []any{"etcd", "kubelet"}))
			kept := lists.DeepCopy()
			require.NoError(t, unstructured.SetNestedStringSlice(kept.Object, []string{"etcd"}, "spec", "args", "services"))
			assert.NoError(t, check(lists, kept), "a list is compared by its path, not per entry")
			ce = asCode(t, check(lists, without(lists, "spec", "args", "services")))
			assert.Equal(t, fmt.Sprintf("the server removed spec.args.services from the object: its version of %s has no such field", k.K8sKind), ce.Message)

			empty := day2Manifest(k.K8sKind, withArgs("services", []any{}))
			require.NoError(t, check(empty, empty.DeepCopy()), "an empty list the answer keeps")
			ce = asCode(t, check(empty, without(empty, "spec", "args", "services")))
			assert.Equal(t, clierr.CodeValidation, ce.Code, "an empty list is a value, and removing it is refused")
			emptyMap := day2Manifest(k.K8sKind)
			emptyMap.Object["spec"].(map[string]any)["args"] = map[string]any{}
			require.NoError(t, check(emptyMap, emptyMap.DeepCopy()))
			ce = asCode(t, check(emptyMap, without(emptyMap, "spec", "args")))
			assert.Equal(t, "the server removed spec.args from the object: its version of "+k.K8sKind+" has no such field", ce.Message)
		})
	}
	assert.NoError(t, check(day2Manifest("ETCDSnapshotSave"), nil), "no dry-run answer to compare with")
}
