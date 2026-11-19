package provisioning

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/provisioning/complete"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// upgradeReleaseRows are the rke2 rows of the fixture release list: with
// serverArgs, with serverArgs {}, with serverArgs null, with none, a
// pre-release, and release numbers 2, 9 and 10 of one patch.
const upgradeReleaseRows = `[
 {"version":"v1.29.9+rke2r1","serverArgs":{"a":"b"}},
 {"version":"v1.30.0+rke2r1","serverArgs":{}},
 {"version":"v1.31.3+rke2r1","serverArgs":{}},
 {"version":"v1.31.4+rke2r1","serverArgs":{}},
 {"version":"v1.31.4+rke2r2","serverArgs":{}},
 {"version":"v1.31.4+rke2r9","serverArgs":{}},
 {"version":"v1.31.4+rke2r10","serverArgs":{}},
 {"version":"v1.31.5+rke2r1","serverArgs":{}},
 {"version":"v1.31.9+rke2r1"},
 {"version":"v1.32.0+rke2r1","serverArgs":{}},
 {"version":"v1.32.1+rke2r1","serverArgs":null},
 {"version":"v1.33.0-rc1+rke2r1","serverArgs":{}}
]`

const upgradeK3sRows = `[{"version":"v1.31.4+k3s1","serverArgs":{}},{"version":"v1.31.5+k3s1","serverArgs":{}}]`

// upgradeRow is one cluster of the table, with the sets its completion offers
// written out: whether `<cluster>` offers it and which versions
// `--k8s-version` offers. The Plan must accept exactly those versions and,
// when currentVersion is set, that version too: the version the cluster runs
// while its upgrade is not finished, which the Plan accepts and completion does
// not offer. For an imported cluster with a pending upgrade, the Plan accepts
// the running version and writes the config back to it; the pending upgrade is
// then not run.
type upgradeRow struct {
	name           string
	prov, mgmt     *unstructured.Unstructured
	setting        string
	listed         bool
	versions       []string
	currentVersion string
	distro         string
}

func namedProv(name string, obj *unstructured.Unstructured, clusterName string) *unstructured.Unstructured {
	obj.SetName(name)
	_ = unstructured.SetNestedField(obj.Object, clusterName, "status", "clusterName")
	return obj
}

func namedMgmt(id string, obj *unstructured.Unstructured) *unstructured.Unstructured {
	obj.SetName(id)
	return obj
}

func upgradeRows() []upgradeRow {
	yes, no, other, sysdef := "true", "false", "maybe", "system-default"
	azure := map[string]any{"machineSelectorConfig": []any{map[string]any{"config": map[string]any{"cloud-provider-name": "azure"}}}}
	unfinished := provisionedObject("v1.31.5+rke2r1", nil)
	_ = unstructured.SetNestedSlice(unfinished.Object, []any{map[string]any{"type": "Updated", "status": "Unknown", "message": "waiting"}}, "status", "conditions")
	paused := importedManagement(&yes, "v1.31.4+rke2r1", "v1.31.4+rke2r1", "True")
	_ = unstructured.SetNestedField(paused.Object, "true", "metadata", "annotations", "management.cattle.io/imported-cluster-version-management-paused")
	notReady := importedManagement(&yes, "v1.31.4+rke2r1", "", "True")
	hosted := importedManagement(&yes, "v1.31.4+rke2r1", "v1.31.4+rke2r1", "True")
	_ = unstructured.SetNestedField(hosted.Object, "EKS", "status", "driver")
	imported := importedManagement(&yes, "v1.31.4+rke2r1", "v1.31.4+rke2r1", "True")
	_ = unstructured.SetNestedField(imported.Object, "imported", "status", "driver")
	noDriver := importedManagement(&yes, "v1.31.4+rke2r1", "v1.31.4+rke2r1", "True")
	unstructured.RemoveNestedField(noDriver.Object, "status", "driver")
	manual := importedManagement(&yes, "v1.31.4+rke2r1", "v1.30.0+rke2r1", "True")
	k3s := importedManagement(&yes, "v1.31.4+k3s1", "v1.31.4+k3s1", "True")
	_ = unstructured.SetNestedField(k3s.Object, "k3s", "status", "driver")
	spec, _ := k3s.Object["spec"].(map[string]any)
	spec["k3sConfig"] = spec["rke2Config"]
	delete(spec, "rke2Config")
	_ = unstructured.SetNestedField(k3s.Object, "v1.31.4+k3s1", "spec", "k3sConfig", "kubernetesVersion")
	regUnfinished := importedManagement(&yes, "v1.31.4+rke2r1", "v1.31.5+rke2r1", "Unknown")
	regNoUpgraded := importedManagement(&yes, "v1.31.4+rke2r1", "v1.31.4+rke2r1", "True")
	unstructured.RemoveNestedField(regNoUpgraded.Object, "status", "conditions")
	newer := []string{"v1.31.4+rke2r2", "v1.31.4+rke2r9", "v1.31.4+rke2r10", "v1.31.5+rke2r1", "v1.32.0+rke2r1"}

	return []upgradeRow{
		{name: "prov-rke2", prov: namedProv("prov-rke2", provisionedObject("v1.31.4+rke2r1", nil), "c-m-p1"), listed: true, distro: "rke2", versions: newer},
		{name: "prov-azure", prov: namedProv("prov-azure", provisionedObject("v1.29.4+rke2r1", azure), "c-m-p2"), listed: true, distro: "rke2", versions: []string{"v1.29.9+rke2r1"}},
		{name: "prov-k3s", prov: namedProv("prov-k3s", provisionedObject("v1.31.4+k3s1", nil), "c-m-p3"), listed: true, distro: "k3s", versions: []string{"v1.31.5+k3s1"}},
		{name: "prov-unfinished", prov: namedProv("prov-unfinished", unfinished, "c-m-p4"), listed: true, distro: "rke2", versions: []string{"v1.32.0+rke2r1"}, currentVersion: "v1.31.5+rke2r1"},
		{name: "prov-finished", prov: namedProv("prov-finished", provisionedObject("v1.31.5+rke2r1", nil), "c-m-p5"), listed: true, distro: "rke2", versions: []string{"v1.32.0+rke2r1"}},
		{name: "reg-true", prov: namedProv("reg-true", importedProvisioning(), "c-m-r1"), mgmt: namedMgmt("c-m-r1", importedManagement(&yes, "v1.31.4+rke2r1", "v1.31.4+rke2r1", "True")),
			listed: true, distro: "rke2", versions: newer},
		{name: "reg-system-default", prov: namedProv("reg-system-default", importedProvisioning(), "c-m-r2"), mgmt: namedMgmt("c-m-r2", importedManagement(&sysdef, "v1.31.4+rke2r1", "v1.31.4+rke2r1", "True")),
			setting: "true", listed: true, distro: "rke2", versions: newer},
		{name: "reg-unfinished", prov: namedProv("reg-unfinished", importedProvisioning(), "c-m-r14"), mgmt: namedMgmt("c-m-r14", regUnfinished),
			listed: true, distro: "rke2", versions: newer, currentVersion: "v1.31.4+rke2r1"},
		{name: "reg-no-upgraded", prov: namedProv("reg-no-upgraded", importedProvisioning(), "c-m-r15"), mgmt: namedMgmt("c-m-r15", regNoUpgraded),
			listed: true, distro: "rke2", versions: newer, currentVersion: "v1.31.4+rke2r1"},
		{name: "reg-text-order", prov: namedProv("reg-text-order", importedProvisioning(), "c-m-r3"), mgmt: namedMgmt("c-m-r3", importedManagement(&yes, "v1.31.4+rke2r9", "v1.31.4+rke2r9", "True")),
			listed: true, distro: "rke2", versions: []string{"v1.31.5+rke2r1", "v1.32.0+rke2r1"}},
		{name: "reg-manual", prov: namedProv("reg-manual", importedProvisioning(), "c-m-r4"), mgmt: namedMgmt("c-m-r4", manual),
			listed: true, distro: "rke2", versions: newer},
		{name: "reg-k3s", prov: namedProv("reg-k3s", importedProvisioning(), "c-m-r5"), mgmt: namedMgmt("c-m-r5", k3s), listed: true, distro: "k3s", versions: []string{"v1.31.5+k3s1"}},
		{name: "reg-off", prov: namedProv("reg-off", importedProvisioning(), "c-m-r6"), mgmt: namedMgmt("c-m-r6", importedManagement(&no, "v1.31.4+rke2r1", "v1.31.4+rke2r1", "True")), distro: "rke2"},
		{name: "reg-setting-off", prov: namedProv("reg-setting-off", importedProvisioning(), "c-m-r7"), mgmt: namedMgmt("c-m-r7", importedManagement(&sysdef, "v1.31.4+rke2r1", "v1.31.4+rke2r1", "True")), setting: "false", distro: "rke2"},
		{name: "reg-other-annotation", prov: namedProv("reg-other-annotation", importedProvisioning(), "c-m-r8"), mgmt: namedMgmt("c-m-r8", importedManagement(&other, "v1.31.4+rke2r1", "v1.31.4+rke2r1", "True")), distro: "rke2"},
		{name: "reg-paused", prov: namedProv("reg-paused", importedProvisioning(), "c-m-r9"), mgmt: namedMgmt("c-m-r9", paused), distro: "rke2"},
		{name: "reg-not-ready", prov: namedProv("reg-not-ready", importedProvisioning(), "c-m-r10"), mgmt: namedMgmt("c-m-r10", notReady), distro: "rke2"},
		{name: "reg-hosted", prov: namedProv("reg-hosted", importedProvisioning(), "c-m-r11"), mgmt: namedMgmt("c-m-r11", hosted), distro: "rke2"},
		{name: "reg-imported", prov: namedProv("reg-imported", importedProvisioning(), "c-m-r12"), mgmt: namedMgmt("c-m-r12", imported), distro: "rke2"},
		{name: "reg-no-driver", prov: namedProv("reg-no-driver", importedProvisioning(), "c-m-r13"), mgmt: namedMgmt("c-m-r13", noDriver), distro: "rke2"},
		{name: "reg-unlinked", prov: namedProv("reg-unlinked", importedProvisioning(), ""), distro: "rke2"},
		{name: "local", prov: namedProv("local", importedProvisioning(), "local"), mgmt: namedMgmt("local", importedManagement(&yes, "v1.31.4+rke2r1", "v1.31.4+rke2r1", "True")), distro: "rke2"},
	}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return string(raw)
}

// TestUpgradeCompletionAndPlanAgree asserts, over a table of clusters and one
// fixture release list, that the clusters `<cluster>` offers and the versions
// `--k8s-version` offers are the expected sets written in each row, that the
// Plan accepts exactly the versions the row expects and its currentVersion,
// that the completion
// sources send no more requests than their claims, and that the Plan refuses
// every version of the lists the row does not expect. Completion is compared
// with the sets of the row and the Plan with the same sets; neither is
// compared with the other.
func TestUpgradeCompletionAndPlanAgree(t *testing.T) {
	t.Parallel()
	rows := upgradeRows()

	var provItems, mgmtItems []any
	bodies := map[string]string{}
	settingByRow := map[string]string{}
	for _, r := range rows {
		provItems = append(provItems, r.prov.Object)
		bodies[upgradeProvPath+"/"+r.name] = jsonOf(t, r.prov.Object)
		if r.mgmt != nil {
			steveObj := r.mgmt.DeepCopy()
			steveObj.Object["id"] = r.mgmt.GetName()
			mgmtItems = append(mgmtItems, steveObj.Object)
			bodies["/v1/management.cattle.io.clusters/"+r.mgmt.GetName()] = jsonOf(t, steveObj.Object)
		}
		if r.setting != "" {
			settingByRow[r.name] = r.setting
		}
	}
	bodies[upgradeProvListPath] = jsonOf(t, map[string]any{"data": provItems})
	bodies["/v1/management.cattle.io.clusters"] = jsonOf(t, map[string]any{"data": mgmtItems})
	bodies["/v1-rke2-release/release"] = `{"data":` + upgradeReleaseRows + `}`
	bodies["/v1-k3s-release/release"] = `{"data":` + upgradeK3sRows + `}`

	// The list site reads one Setting for all rows: its value is the one the
	// imported rows that need it expect.
	listSetting := `{"value":"true"}`
	listBodies := maps.Clone(bodies)
	listBodies[upgradeSettingPath] = listSetting
	listServer := completiontest.New(t, listBodies)
	listConn := listServer.Conn(3)
	offered := complete.UpgradeSource().Complete(t.Context(), listConn, completion.Request{Index: 0}).Names
	assert.LessOrEqual(t, listConn.Used(), 3)
	assert.False(t, listConn.Exceeded())
	var wantListed []string
	for _, r := range rows {
		// The Setting the list server returns is true: a row that only its own
		// Setting turns off is offered here, and the list under a false Setting
		// is checked below.
		if r.listed || r.name == "reg-setting-off" {
			wantListed = append(wantListed, r.name)
		}
	}
	assert.ElementsMatch(t, wantListed, offered, "the clusters <cluster> offers")

	listBodies[upgradeSettingPath] = `{"value":"false"}`
	offeredOff := complete.UpgradeSource().Complete(t.Context(), completiontest.New(t, listBodies).Conn(3), completion.Request{Index: 0}).Names
	wantOff := slices.DeleteFunc(slices.Clone(wantListed), func(n string) bool { return n == "reg-system-default" || n == "reg-setting-off" })
	assert.ElementsMatch(t, wantOff, offeredOff, "the clusters <cluster> offers while the Setting is off")

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			ownBodies := maps.Clone(bodies)
			ownSetting := r.setting
			if ownSetting == "" {
				ownSetting = "true"
			}
			ownBodies[upgradeSettingPath] = `{"value":"` + ownSetting + `"}`
			s := completiontest.New(t, ownBodies)
			conn := s.Conn(4)
			versions := complete.UpgradeSource().Complete(t.Context(), conn, completion.Request{Flag: "k8s-version", Positionals: []string{r.name}}).Names
			assert.LessOrEqual(t, conn.Used(), 4)
			assert.False(t, conn.Exceeded())
			assert.ElementsMatch(t, r.versions, versions, "the versions --k8s-version offers")

			f := newUpgradeFixture(t)
			f.srv = upgradeListServer(t, f.steve)
			f.local.put(provisioningClusterGVR, "fleet-default", r.name, r.prov)
			f.local.put(rkeControlPlaneGVR, "fleet-default", r.name, &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{}}})
			f.local.put(settingsGVR, "", versionManagementSetting, settingObj(versionManagementSetting, ownSetting))
			if r.mgmt != nil {
				f.steve.set(r.mgmt)
			}
			for _, v := range listedVersions() {
				_, _, err := f.plan(t, upgradeInput(r.name, v))
				if slices.Contains(r.versions, v) || v == r.currentVersion {
					assert.NoError(t, err, "%s: the Plan accepts %s", r.name, v)
				} else {
					assert.Error(t, err, "%s: the Plan refuses %s", r.name, v)
				}
			}
		})
	}
}

const upgradeProvPath = "/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters"
const upgradeProvListPath = "/v1/provisioning.cattle.io.clusters/fleet-default"
const upgradeSettingPath = "/k8s/clusters/local/apis/management.cattle.io/v3/settings/imported-cluster-version-management"

// listedVersions are the versions of both fixture lists, plus the ones no
// list has.
func listedVersions() []string {
	var rows []struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal([]byte(upgradeReleaseRows), &rows)
	var k3s []struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal([]byte(upgradeK3sRows), &k3s)
	var out []string
	for _, r := range rows {
		out = append(out, r.Version)
	}
	for _, r := range k3s {
		out = append(out, r.Version)
	}
	return append(out, "v1.34.0+rke2r1", "v1.31.4+rke2")
}

// upgradeListServer serves the fixture release lists and the management
// Clusters of the store through Steve.
func upgradeListServer(t *testing.T, store *steveStore) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1-rke2-release/release":
			_, _ = w.Write([]byte(`{"data":` + upgradeReleaseRows + `}`))
		case r.URL.Path == "/v1-k3s-release/release":
			_, _ = w.Write([]byte(`{"data":` + upgradeK3sRows + `}`))
		case strings.HasPrefix(r.URL.Path, "/v1/management.cattle.io.clusters/"):
			if obj, ok := store.get(strings.TrimPrefix(r.URL.Path, "/v1/management.cattle.io.clusters/")); ok {
				_ = json.NewEncoder(w).Encode(obj.Object)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}
