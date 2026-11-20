package provisioning

import (
	"maps"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/provisioning/complete"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// strategyRow is one cluster of the table with whether `<cluster>` offers it,
// written out. The Plan must accept the cluster exactly where it is offered.
type strategyRow struct {
	name       string
	prov, mgmt *unstructured.Unstructured
	offered    bool
}

func strategyRows() []strategyRow {
	yes, no, other, empty, sysdef := "true", "false", "maybe", "", "system-default"
	mgmtOf := func(annotation *string, mutate func(*unstructured.Unstructured)) *unstructured.Unstructured {
		m := importedManagement(annotation, "v1.31.4+rke2r1", "v1.31.4+rke2r1", "True")
		if mutate != nil {
			mutate(m)
		}
		return m
	}
	hostedBy := func(field string) func(*unstructured.Unstructured) {
		return func(m *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(m.Object, map[string]any{"region": "x"}, "spec", field)
		}
	}
	driverOf := func(driver string) func(*unstructured.Unstructured) {
		return func(m *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(m.Object, driver, "status", "driver")
			_ = unstructured.SetNestedField(m.Object, map[string]any{"kubernetesVersion": "v1.31.4+rke2r1"}, "spec", driver+"Config")
		}
	}
	k3s := mgmtOf(&yes, func(m *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(m.Object, "k3s", "status", "driver")
		spec, _ := m.Object["spec"].(map[string]any)
		spec["k3sConfig"] = map[string]any{"kubernetesVersion": "v1.31.4+k3s1"}
		delete(spec, "rke2Config")
	})
	imp := func(name string, m *unstructured.Unstructured, offered bool) strategyRow {
		id := "c-m-" + name
		if m != nil {
			m.SetName(id)
		}
		return strategyRow{name: name, prov: namedProv(name, importedProvisioning(), id), mgmt: m, offered: offered}
	}
	unlinked := imp("unlinked", nil, false)
	unlinked.prov = namedProv("unlinked", importedProvisioning(), "")
	local := imp("local", mgmtOf(&yes, nil), false)
	local.prov = namedProv("local", importedProvisioning(), "local")
	local.mgmt.SetName("local")
	prov := func(name, version string, rke map[string]any) strategyRow {
		return strategyRow{name: name, prov: namedProv(name, provisionedObject(version, rke), "c-m-"+name), offered: true}
	}
	return []strategyRow{
		prov("prov-rke2", "v1.31.4+rke2r1", nil),
		prov("prov-k3s", "v1.31.4+k3s1", nil),
		prov("prov-azure", "v1.29.4+rke2r1", map[string]any{"machineSelectorConfig": []any{map[string]any{"config": map[string]any{"cloud-provider-name": "azure"}}}}),
		prov("prov-prerelease", "v1.32.0-rc1+rke2r1", nil),
		prov("prov-no-version", "", nil),
		{name: "prov-local", prov: namedProv("prov-local", provisionedObject("v1.31.4+rke2r1", nil), "local"), offered: false},
		imp("rke2-true", mgmtOf(&yes, nil), true),
		imp("rke2-absent", mgmtOf(nil, nil), true),
		imp("rke2-empty", mgmtOf(&empty, nil), true),
		imp("rke2-system-default", mgmtOf(&sysdef, nil), true),
		imp("version-management-off", mgmtOf(&no, nil), true),
		imp("annotation-other", mgmtOf(&other, nil), false),
		imp("paused", mgmtOf(&yes, func(m *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(m.Object, "true", "metadata", "annotations", "management.cattle.io/imported-cluster-version-management-paused")
		}), true),
		imp("k3s", k3s, true),
		imp("config-nil", mgmtOf(&yes, func(m *unstructured.Unstructured) { unstructured.RemoveNestedField(m.Object, "spec", "rke2Config") }), false),
		imp("config-version-empty", mgmtOf(&yes, func(m *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(m.Object, "", "spec", "rke2Config", "kubernetesVersion")
		}), false),
		imp("status-version-nil", mgmtOf(&yes, func(m *unstructured.Unstructured) { unstructured.RemoveNestedField(m.Object, "status", "version") }), false),
		imp("hosted-eks", mgmtOf(&yes, hostedBy("eksConfig")), false),
		imp("hosted-aks", mgmtOf(&yes, hostedBy("aksConfig")), false),
		imp("hosted-gke", mgmtOf(&yes, hostedBy("gkeConfig")), false),
		imp("hosted-ali", mgmtOf(&yes, hostedBy("aliConfig")), false),
		imp("driver-eks", mgmtOf(&yes, driverOf("EKS")), false),
		imp("generic-imported", mgmtOf(&yes, driverOf("imported")), false),
		imp("k3os", mgmtOf(&yes, driverOf("k3os")), false),
		imp("rancherd", mgmtOf(&yes, driverOf("rancherd")), false),
		imp("no-driver", mgmtOf(&yes, func(m *unstructured.Unstructured) { unstructured.RemoveNestedField(m.Object, "status", "driver") }), false),
		unlinked,
		local,
	}
}

func strategyBodies(t *testing.T, rows []strategyRow) map[string]string {
	t.Helper()
	var provItems, mgmtItems []any
	bodies := map[string]string{}
	for _, r := range rows {
		provItems = append(provItems, r.prov.Object)
		bodies[upgradeProvPath+"/"+r.name] = jsonOf(t, r.prov.Object)
		if r.mgmt != nil {
			steveObj := r.mgmt.DeepCopy()
			steveObj.Object["id"] = r.mgmt.GetName()
			mgmtItems = append(mgmtItems, steveObj.Object)
		}
	}
	bodies[upgradeProvListPath] = jsonOf(t, map[string]any{"data": provItems})
	bodies["/v1/management.cattle.io.clusters"] = jsonOf(t, map[string]any{"data": mgmtItems})
	return bodies
}

// TestUpgradeStrategyCompletionAndPlanAgree asserts, over a table of clusters,
// that `<cluster>` offers the set written in each row and that the Plan accepts
// a cluster exactly where it is offered. Neither is compared with the other.
// The source sends two requests at most and no request for the Setting.
func TestUpgradeStrategyCompletionAndPlanAgree(t *testing.T) {
	t.Parallel()
	rows := strategyRows()
	bodies := strategyBodies(t, rows)
	conn := completiontest.New(t, bodies).Conn(2)
	offered := complete.UpgradeStrategySource().Complete(t.Context(), conn, completion.Request{Index: 0}).Names
	assert.LessOrEqual(t, conn.Used(), 2)
	assert.False(t, conn.Exceeded())
	var want []string
	for _, r := range rows {
		if r.offered {
			want = append(want, r.name)
		}
	}
	assert.ElementsMatch(t, want, offered, "the clusters <cluster> offers")

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.srv = upgradeListServer(t, f.steve)
			f.local.put(provisioningClusterGVR, "fleet-default", r.name, r.prov)
			f.local.put(rkeControlPlaneGVR, "fleet-default", r.name, &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{}}})
			f.putVersionManagementSetting("false")
			if r.mgmt != nil {
				f.steve.set(r.mgmt)
			}
			_, _, err := f.planStrategy(t, strategyInput(r.name, "worker-concurrency", "2"))
			if r.offered {
				assert.NoError(t, err, "the Plan accepts %s", r.name)
			} else {
				assert.Error(t, err, "the Plan refuses %s", r.name)
			}
		})
	}
}

// TestUpgradeStrategyCompletionNamespaceAndFailures asserts a typed namespace
// changes the list, and a failed provisioning list or a failed management list
// offers nothing.
func TestUpgradeStrategyCompletionNamespaceAndFailures(t *testing.T) {
	t.Parallel()
	rows := strategyRows()
	bodies := strategyBodies(t, rows)
	team := maps.Clone(bodies)
	team["/v1/provisioning.cattle.io.clusters/team-a"] = jsonOf(t, map[string]any{"data": []any{provisionedObject("v1.31.4+rke2r1", nil).Object}})
	got := complete.UpgradeStrategySource().Complete(t.Context(), completiontest.New(t, team).Conn(2), completion.Request{Index: 0, Tail: []string{"--namespace", "team-a"}}).Names
	assert.Equal(t, []string{"prod"}, got)

	noProv := maps.Clone(bodies)
	delete(noProv, upgradeProvListPath)
	assert.Empty(t, complete.UpgradeStrategySource().Complete(t.Context(), completiontest.New(t, noProv).Conn(2), completion.Request{Index: 0}).Names)
	noMgmt := maps.Clone(bodies)
	delete(noMgmt, "/v1/management.cattle.io.clusters")
	assert.Empty(t, complete.UpgradeStrategySource().Complete(t.Context(), completiontest.New(t, noMgmt).Conn(2), completion.Request{Index: 0}).Names)
}

// TestUpgradeStrategyFlagValuesPassTheFlagParser asserts every value the grace
// period and drain timeout flags offer for a provisioned cluster is accepted by
// the command's own flag reader.
func TestUpgradeStrategyFlagValuesPassTheFlagParser(t *testing.T) {
	t.Parallel()
	prodPath := upgradeProvPath + "/prod"
	bodies := map[string]string{prodPath: jsonOf(t, provisionedWith(nil).Object)}
	for _, group := range []string{"control-plane", "worker"} {
		for flag, parse := range map[string]func(string, string) (int64, error){
			group + "-grace-period":  parseGracePeriod,
			group + "-drain-timeout": parseDrainTimeout,
		} {
			conn := completiontest.New(t, bodies).Conn(1)
			got := complete.UpgradeStrategySource().Complete(t.Context(), conn, completion.Request{Flag: flag, Positionals: []string{"prod"}}).Names
			require.NotEmpty(t, got, flag)
			assert.False(t, conn.Exceeded())
			for _, value := range got {
				_, err := parse(flag, value)
				assert.NoError(t, err, "--%s %s", flag, value)
			}
		}
	}
}
