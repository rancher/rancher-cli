package provisioning

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func (f *upgradeFixture) strategyCommand() runtime.MutatingCommand {
	return NewUpgradeStrategy(f.factory, fakeRootFactory{rc: classifiedRoot{srv: f.srv}}).(runtime.MutatingCommand)
}

func (f *upgradeFixture) planStrategy(t *testing.T, in mcInput) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	return f.strategyCommand().Plan(t.Context(), in)
}

// strategyInput builds the input of the command: the cluster, then flag and
// value pairs. A bool flag is given "true" or "false".
func strategyInput(cluster string, pairs ...string) mcInput {
	in := mcInput{a: []string{cluster}, s: map[string]string{}, b: map[string]bool{}, is: map[string]bool{}}
	for i := 0; i+1 < len(pairs); i += 2 {
		name, value := pairs[i], pairs[i+1]
		in.is[name] = true
		switch {
		case strings.HasSuffix(name, "-drain"), strings.HasSuffix(name, "-drain-force"), strings.HasSuffix(name, "-delete-emptydir"):
			in.b[name] = value == "true"
		default:
			in.s[name] = value
		}
	}
	return in
}

// fullDrain is a drain options object that holds every field with a value
// that differs from the Rancher UI's default, and a field the command never
// writes.
func fullDrain(enabled bool) map[string]any {
	return map[string]any{
		"enabled": enabled, "deleteEmptyDirData": false, "disableEviction": true, "force": true,
		"gracePeriod": int64(30), "ignoreDaemonSets": false, "skipWaitForDeleteTimeoutSeconds": int64(5), "timeout": int64(60),
		"postDrainHooks": []any{map[string]any{"annotation": "after"}},
	}
}

// provisionedWith is the provisioned cluster prod with the given upgrade
// strategy and fields the command must leave alone.
func provisionedWith(strategy map[string]any) *unstructured.Unstructured {
	rke := map[string]any{"machinePools": []any{map[string]any{"name": "pool1", "quantity": int64(3)}}}
	if strategy != nil {
		rke["upgradeStrategy"] = strategy
	}
	obj := provisionedObject("v1.31.4+rke2r1", rke)
	_ = unstructured.SetNestedField(obj.Object, "creds", "spec", "cloudCredentialSecretName")
	return obj
}

func (f *upgradeFixture) putProvisioned(obj *unstructured.Unstructured) {
	f.local.put(provisioningClusterGVR, "fleet-default", "prod", obj)
}

// importedWith is the imported cluster edge of a distribution, whose
// management Cluster has the given annotation (nil for none) and upgrade
// strategy.
func (f *upgradeFixture) importedWith(distro string, annotation *string, strategy map[string]any) {
	annotations := map[string]any{}
	if annotation != nil {
		annotations[versionManagementAnnotation] = *annotation
	}
	m := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "Cluster",
		"metadata": map[string]any{"name": "c-m-edge1", "resourceVersion": "9", "generation": int64(2), "annotations": annotations},
		"spec": map[string]any{
			"desiredAgentImage": "agent:1",
			distro + "Config": map[string]any{
				"kubernetesVersion":        "v1.31.4+" + distro + "r1",
				"upgradeUnrelated":         "keep",
				distro + "upgradeStrategy": strategy,
			},
		},
		"status": map[string]any{
			"driver": distro, "version": map[string]any{"gitVersion": "v1.31.4+" + distro + "r1"},
			"conditions": []any{map[string]any{"type": "Upgraded", "status": "True"}},
		},
	}}
	f.local.put(provisioningClusterGVR, "fleet-default", "edge", importedProvisioning())
	f.local.put(managementClusterRow.GVR, "", "c-m-edge1", m)
	f.steve.set(m)
}

func (f *upgradeFixture) putVersionManagementSetting(value string) {
	f.local.put(settingsGVR, "", versionManagementSetting, settingObj(versionManagementSetting, value))
}

// lastPreview is the object of the first dry-run of the fixture's local client.
func (f *upgradeFixture) lastPreview(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	f.local.mu.Lock()
	defer f.local.mu.Unlock()
	require.NotEmpty(t, f.local.updates)
	require.True(t, f.local.updates[0].dryRun)
	return f.local.updates[0].obj
}

// setField returns a copy of obj with the field at path set.
func setField(t *testing.T, obj *unstructured.Unstructured, value any, path ...string) *unstructured.Unstructured {
	t.Helper()
	out := obj.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(out.Object, value, path...))
	return out
}

func strategyPath(provisioned bool, distro string, rest ...string) []string {
	if provisioned {
		return append([]string{"spec", "rkeConfig", "upgradeStrategy"}, rest...)
	}
	return append([]string{"spec", distro + "Config", distro + "upgradeStrategy"}, rest...)
}

// TestUpgradeStrategyFlagRefusalsMakeNoRequest asserts a flag set that cannot be
// written is refused before any request, with the text and the code.
func TestUpgradeStrategyFlagRefusalsMakeNoRequest(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name string
		in   mcInput
		text string
	}{
		{"no flag", strategyInput("prod"), "give at least one flag that sets the upgrade strategy, for example --worker-concurrency 1"},
		{"namespace only", strategyInput("prod", "namespace", "fleet-default"), "give at least one flag that sets the upgrade strategy, for example --worker-concurrency 1"},
		{"grace period zero", strategyInput("prod", "worker-grace-period", "0"), `--worker-grace-period "0" must be a whole number of seconds, 1 or more, or -1 for the pod's default`},
		{"grace period -2", strategyInput("prod", "worker-grace-period", "-2"), `--worker-grace-period "-2" must be a whole number of seconds, 1 or more, or -1 for the pod's default`},
		{"grace period fraction", strategyInput("prod", "control-plane-grace-period", "1.5"), `--control-plane-grace-period "1.5" must be a whole number of seconds, 1 or more, or -1 for the pod's default`},
		{"grace period text", strategyInput("prod", "control-plane-grace-period", "abc"), `--control-plane-grace-period "abc" must be a whole number of seconds, 1 or more, or -1 for the pod's default`},
		{"drain timeout zero", strategyInput("prod", "worker-drain-timeout", "0s"), `--worker-drain-timeout "0s" must be a duration of 1s or more in whole seconds, for example 90s or 5m`},
		{"drain timeout milliseconds", strategyInput("prod", "worker-drain-timeout", "1500ms"), `--worker-drain-timeout "1500ms" must be a duration of 1s or more in whole seconds, for example 90s or 5m`},
		{"drain timeout text", strategyInput("prod", "control-plane-drain-timeout", "abc"), `--control-plane-drain-timeout "abc" must be a duration of 1s or more in whole seconds, for example 90s or 5m`},
		{"drain timeout negative", strategyInput("prod", "control-plane-drain-timeout", "-5s"), `--control-plane-drain-timeout "-5s" must be a duration of 1s or more in whole seconds, for example 90s or 5m`},
		{"drain timeout no unit", strategyInput("prod", "control-plane-drain-timeout", "5"), `--control-plane-drain-timeout "5" must be a duration of 1s or more in whole seconds, for example 90s or 5m`},
	}
	for _, group := range []string{"control-plane", "worker"} {
		for _, detail := range []struct{ ending, value string }{{"drain-force", "true"}, {"delete-emptydir", "false"}, {"grace-period", "60"}, {"drain-timeout", "5m"}} {
			flag := group + "-" + detail.ending
			rows = append(rows, struct {
				name string
				in   mcInput
				text string
			}{flag + " with drain false", strategyInput("prod", group+"-drain", "false", flag, detail.value),
				"--" + flag + " conflicts with --" + group + "-drain=false; remove one"})
		}
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.putProvisioned(provisionedWith(nil))
			_, _, err := f.planStrategy(t, tc.in)
			ce := asCode(t, err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, tc.text, ce.Message)
			assert.Empty(t, f.local.requests)
			assert.Empty(t, *f.ids)
		})
	}
}

// TestUpgradeStrategyAcceptsValuesTheFlagsOffer asserts the values the flags
// take: a grace period of -1 and 1 or more, a drain timeout in whole seconds
// written as an integer of seconds.
func TestUpgradeStrategyAcceptsValuesTheFlagsOffer(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]int64{"-1": -1, "1": 1, "300": 300} {
		got, err := parseGracePeriod("worker-grace-period", text)
		require.NoError(t, err, text)
		assert.Equal(t, want, got, text)
	}
	for text, want := range map[string]int64{"1s": 1, "30s": 30, "2m": 120, "10m": 600, "1h": 3600, "90s": 90} {
		got, err := parseDrainTimeout("worker-drain-timeout", text)
		require.NoError(t, err, text)
		assert.Equal(t, want, got, text)
	}
}

// TestUpgradeStrategyNoProvisioningCluster asserts the refusal of a cluster with
// no provisioning Cluster names where an imported cluster's strategy is set, and
// its hint lists the provisioning clusters of the namespace.
func TestUpgradeStrategyNoProvisioningCluster(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	_, _, err := f.planStrategy(t, strategyInput("ghost", "worker-concurrency", "2", "namespace", "fleet-other"))
	ce := asCode(t, err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "cluster ghost has no provisioning cluster in namespace fleet-other; an imported cluster whose management cluster names no Fleet workspace has none, "+
		"and its upgrade strategy is in its management cluster, in spec.rke2Config.rke2upgradeStrategy or spec.k3sConfig.k3supgradeStrategy", ce.Message)
	assert.Equal(t, clierr.NewCommand("get provisioning-clusters").FlagValue("--namespace", "fleet-other").Field(), ce.Hint)
	assert.Equal(t, 1, f.local.requestsOf("GET clusters/fleet-other/ghost"))
	assert.Empty(t, f.local.updates)
}

// TestUpgradeStrategyRefusesTheClustersTheChecksRefuse asserts the cluster
// checks run before any write: the local cluster and an imported cluster that
// has not reported its version end the Plan with no dry-run.
func TestUpgradeStrategyRefusesTheClustersTheChecksRefuse(t *testing.T) {
	t.Parallel()
	t.Run("local", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		obj := provisionedWith(nil)
		obj.SetName("local")
		_ = unstructured.SetNestedField(obj.Object, "local", "status", "clusterName")
		f.local.put(provisioningClusterGVR, "fleet-local", "local", obj)
		_, _, err := f.planStrategy(t, strategyInput("local", "namespace", "fleet-local", "worker-concurrency", "2"))
		ce := asCode(t, err)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Equal(t, "this CLI does not upgrade the local cluster", ce.Message)
		assert.Empty(t, f.local.updates)
	})
	t.Run("not ready", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		f.importedWith("rke2", new("true"), map[string]any{})
		m := importedManagement(new("true"), "v1.31.4+rke2r1", "", "True")
		f.local.put(managementClusterRow.GVR, "", "c-m-edge1", m)
		f.steve.set(m)
		_, _, err := f.planStrategy(t, strategyInput("edge", "worker-concurrency", "2"))
		ce := asCode(t, err)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.True(t, ce.Retryable)
		assert.Equal(t, "cluster edge has not reported its version yet; try again later", ce.Message)
		assert.Empty(t, f.local.updates)
	})
}

type fieldChange struct {
	path  []string
	value any
}

// TestUpgradeStrategyChangesOnlyTheNamedFields asserts that for each of the
// twelve flags alone and for a pair across the two groups, on each kind of
// cluster the flag applies to, the object sent to the server equals the object as
// read except the one field each flag names. The object as read holds fields the
// command must not touch.
func TestUpgradeStrategyChangesOnlyTheNamedFields(t *testing.T) {
	t.Parallel()
	storedProvisioned := func() map[string]any {
		return map[string]any{
			"controlPlaneConcurrency": "1", "workerConcurrency": "10%",
			"controlPlaneDrainOptions": fullDrain(true), "workerDrainOptions": fullDrain(false),
			"unrelatedStrategyField": "keep",
		}
	}
	d := func(group, field string, value any) fieldChange {
		return fieldChange{[]string{group + "DrainOptions", field}, value}
	}
	provisionedRows := []struct {
		name    string
		flags   []string
		changes []fieldChange
	}{
		{"control plane concurrency", []string{"control-plane-concurrency", "3"}, []fieldChange{{[]string{"controlPlaneConcurrency"}, "3"}}},
		{"worker concurrency", []string{"worker-concurrency", "25%"}, []fieldChange{{[]string{"workerConcurrency"}, "25%"}}},
		{"control plane drain off", []string{"control-plane-drain", "false"}, []fieldChange{d("controlPlane", "enabled", false)}},
		{"worker drain on", []string{"worker-drain", "true"}, []fieldChange{d("worker", "enabled", true)}},
		{"control plane force", []string{"control-plane-drain-force", "false"}, []fieldChange{d("controlPlane", "force", false)}},
		{"worker force", []string{"worker-drain-force", "false"}, []fieldChange{d("worker", "force", false)}},
		{"control plane emptydir", []string{"control-plane-delete-emptydir", "true"}, []fieldChange{d("controlPlane", "deleteEmptyDirData", true)}},
		{"worker emptydir", []string{"worker-delete-emptydir", "true"}, []fieldChange{d("worker", "deleteEmptyDirData", true)}},
		{"control plane grace period", []string{"control-plane-grace-period", "90"}, []fieldChange{d("controlPlane", "gracePeriod", int64(90))}},
		{"worker grace period", []string{"worker-grace-period", "-1"}, []fieldChange{d("worker", "gracePeriod", int64(-1))}},
		{"control plane drain timeout", []string{"control-plane-drain-timeout", "5m"}, []fieldChange{d("controlPlane", "timeout", int64(300))}},
		{"worker drain timeout", []string{"worker-drain-timeout", "90s"}, []fieldChange{d("worker", "timeout", int64(90))}},
		{"pair across groups", []string{"control-plane-concurrency", "2", "worker-drain-timeout", "10m", "worker-drain", "true"},
			[]fieldChange{{[]string{"controlPlaneConcurrency"}, "2"}, d("worker", "timeout", int64(600)), d("worker", "enabled", true)}},
	}
	for _, tc := range provisionedRows {
		t.Run("provisioned "+tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			stored := provisionedWith(storedProvisioned())
			f.putProvisioned(stored)
			want := stored
			for _, c := range tc.changes {
				want = setField(t, want, c.value, strategyPath(true, "", c.path...)...)
			}
			_, exec, err := f.planStrategy(t, strategyInput("prod", tc.flags...))
			require.NoError(t, err)
			assert.Equal(t, want.Object, f.lastPreview(t).Object)
			_, err = exec(t.Context())
			require.NoError(t, err)
			require.Len(t, f.local.updates, 2)
			assert.False(t, f.local.updates[1].dryRun)
			assert.Equal(t, want.Object, f.local.updates[1].obj.Object, "the write equals the preview")
		})
	}

	importedRows := []struct {
		name    string
		flags   []string
		changes []fieldChange
	}{
		{"control plane concurrency", []string{"control-plane-concurrency", "3"}, []fieldChange{{[]string{"serverConcurrency"}, int64(3)}}},
		{"worker concurrency", []string{"worker-concurrency", "4"}, []fieldChange{{[]string{"workerConcurrency"}, int64(4)}}},
		{"control plane drain off", []string{"control-plane-drain", "false"}, []fieldChange{{[]string{"drainServerNodes"}, false}}},
		{"worker drain on", []string{"worker-drain", "true"}, []fieldChange{{[]string{"drainWorkerNodes"}, true}}},
		{"pair across groups", []string{"control-plane-concurrency", "2", "worker-drain", "true"},
			[]fieldChange{{[]string{"serverConcurrency"}, int64(2)}, {[]string{"drainWorkerNodes"}, true}}},
	}
	for _, distro := range []string{"rke2", "k3s"} {
		for _, tc := range importedRows {
			t.Run("imported "+distro+" "+tc.name, func(t *testing.T) {
				t.Parallel()
				f := newUpgradeFixture(t)
				stored := map[string]any{"serverConcurrency": int64(1), "workerConcurrency": int64(2), "drainServerNodes": true, "drainWorkerNodes": false, "unrelatedStrategyField": "keep"}
				f.importedWith(distro, new("true"), stored)
				base := f.lastStored(t, "c-m-edge1")
				want := base
				for _, c := range tc.changes {
					want = setField(t, want, c.value, strategyPath(false, distro, c.path...)...)
				}
				_, exec, err := f.planStrategy(t, strategyInput("edge", tc.flags...))
				require.NoError(t, err)
				assert.Equal(t, want.Object, f.lastPreview(t).Object)
				_, err = exec(t.Context())
				require.NoError(t, err)
				require.Len(t, f.local.updates, 2)
				assert.Equal(t, want.Object, f.local.updates[1].obj.Object)
			})
		}
	}
}

// lastStored is the management Cluster as the command reads it through Steve:
// decoded from JSON: its numbers are float64.
func (f *upgradeFixture) lastStored(t *testing.T, id string) *unstructured.Unstructured {
	t.Helper()
	obj, ok := f.local.objects[objKey(managementClusterRow.GVR, "", id)]
	require.True(t, ok)
	return &unstructured.Unstructured{Object: jsonShaped(t, obj[0].Object)}
}

// TestUpgradeStrategyCreatesTheStrategyOfAnEmptyRKEConfig asserts a custom
// cluster whose spec.rkeConfig is empty gets spec.rkeConfig.upgradeStrategy
// created, and the machine pools are kept.
func TestUpgradeStrategyCreatesTheStrategyOfAnEmptyRKEConfig(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	f.putProvisioned(provisionedWith(nil))
	_, _, err := f.planStrategy(t, strategyInput("prod", "worker-concurrency", "2"))
	require.NoError(t, err)
	got := f.lastPreview(t)
	strategy, found, _ := unstructured.NestedMap(got.Object, "spec", "rkeConfig", "upgradeStrategy")
	require.True(t, found)
	assert.Equal(t, map[string]any{"workerConcurrency": "2"}, strategy)
	pools, _, _ := unstructured.NestedSlice(got.Object, "spec", "rkeConfig", "machinePools")
	assert.Len(t, pools, 1)
}

// uiDefaults are the Rancher UI's values for the drain options.
var uiDefaults = map[string]any{
	"deleteEmptyDirData": true, "disableEviction": false, "force": false, "gracePeriod": int64(-1),
	"ignoreDaemonSets": true, "skipWaitForDeleteTimeoutSeconds": int64(0), "timeout": int64(120),
}

// TestUpgradeStrategyDrainDefaults asserts that turning drain on for a group
// whose drain the cluster has not enabled writes every drain option the group lacks
// with the Rancher UI's value, for both groups and each of the seven options
// missing in turn, and that a drain already enabled gets nothing beyond the
// typed fields.
func TestUpgradeStrategyDrainDefaults(t *testing.T) {
	t.Parallel()
	groups := []struct{ flag, field string }{{"control-plane", "controlPlaneDrainOptions"}, {"worker", "workerDrainOptions"}}
	for _, g := range groups {
		fields := slices.Sorted(func(yield func(string) bool) {
			for k := range uiDefaults {
				if !yield(k) {
					return
				}
			}
		})
		for _, missing := range fields {
			t.Run(g.flag+" lacks "+missing, func(t *testing.T) {
				t.Parallel()
				stored := fullDrain(false)
				delete(stored, missing)
				f := newUpgradeFixture(t)
				f.putProvisioned(provisionedWith(map[string]any{g.field: stored}))
				_, _, err := f.planStrategy(t, strategyInput("prod", g.flag+"-drain", "true"))
				require.NoError(t, err)
				got, _, _ := unstructured.NestedMap(f.lastPreview(t).Object, "spec", "rkeConfig", "upgradeStrategy", g.field)
				want := fullDrain(true)
				want[missing] = uiDefaults[missing]
				assert.Equal(t, want, got)
			})
		}
		t.Run(g.flag+" lacks every option", func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.putProvisioned(provisionedWith(map[string]any{g.field: map[string]any{"enabled": false}}))
			_, _, err := f.planStrategy(t, strategyInput("prod", g.flag+"-drain", "true"))
			require.NoError(t, err)
			got, _, _ := unstructured.NestedMap(f.lastPreview(t).Object, "spec", "rkeConfig", "upgradeStrategy", g.field)
			want := map[string]any{"enabled": true}
			maps.Copy(want, uiDefaults)
			assert.Equal(t, want, got)
		})
		t.Run(g.flag+" drain options absent", func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.putProvisioned(provisionedWith(map[string]any{"workerConcurrency": "1"}))
			_, _, err := f.planStrategy(t, strategyInput("prod", g.flag+"-drain", "true"))
			require.NoError(t, err)
			got, _, _ := unstructured.NestedMap(f.lastPreview(t).Object, "spec", "rkeConfig", "upgradeStrategy", g.field)
			want := map[string]any{"enabled": true}
			maps.Copy(want, uiDefaults)
			assert.Equal(t, want, got)
		})
		t.Run(g.flag+" enabled absent counts as not enabled", func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.putProvisioned(provisionedWith(map[string]any{g.field: map[string]any{"force": true}}))
			_, _, err := f.planStrategy(t, strategyInput("prod", g.flag+"-drain", "true"))
			require.NoError(t, err)
			got, _, _ := unstructured.NestedMap(f.lastPreview(t).Object, "spec", "rkeConfig", "upgradeStrategy", g.field)
			want := map[string]any{"enabled": true}
			maps.Copy(want, uiDefaults)
			want["force"] = true
			assert.Equal(t, want, got, "a force the cluster has stays")
		})
		t.Run(g.flag+" drain already enabled writes no default", func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			stored := map[string]any{"enabled": true, "force": true}
			f.putProvisioned(provisionedWith(map[string]any{g.field: stored}))
			_, _, err := f.planStrategy(t, strategyInput("prod", g.flag+"-drain", "true", g.flag+"-grace-period", "45"))
			require.NoError(t, err)
			got, _, _ := unstructured.NestedMap(f.lastPreview(t).Object, "spec", "rkeConfig", "upgradeStrategy", g.field)
			assert.Equal(t, map[string]any{"enabled": true, "force": true, "gracePeriod": int64(45)}, got)
		})
		t.Run(g.flag+" a typed option wins over the default", func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.putProvisioned(provisionedWith(map[string]any{g.field: map[string]any{"enabled": false}}))
			_, _, err := f.planStrategy(t, strategyInput("prod", g.flag+"-drain", "true", g.flag+"-grace-period", "45", g.flag+"-drain-force", "true", g.flag+"-drain-timeout", "5m", g.flag+"-delete-emptydir", "false"))
			require.NoError(t, err)
			got, _, _ := unstructured.NestedMap(f.lastPreview(t).Object, "spec", "rkeConfig", "upgradeStrategy", g.field)
			want := map[string]any{"enabled": true}
			maps.Copy(want, uiDefaults)
			want["gracePeriod"], want["force"], want["timeout"], want["deleteEmptyDirData"] = int64(45), true, int64(300), false
			assert.Equal(t, want, got)
		})
		t.Run(g.flag+" detail without drain writes no default", func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.putProvisioned(provisionedWith(map[string]any{g.field: map[string]any{"enabled": false}}))
			eff, _, err := f.planStrategy(t, strategyInput("prod", g.flag+"-drain-force", "true"))
			require.NoError(t, err)
			got, _, _ := unstructured.NestedMap(f.lastPreview(t).Object, "spec", "rkeConfig", "upgradeStrategy", g.field)
			assert.Equal(t, map[string]any{"enabled": false, "force": true}, got)
			warningsContain(t, eff.Warnings, "--"+g.flag+"-drain-force applies once drain is on for the ")
		})
		t.Run(g.flag+" detail with drain enabled gets no warning", func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.putProvisioned(provisionedWith(map[string]any{g.field: map[string]any{"enabled": true}}))
			eff, _, err := f.planStrategy(t, strategyInput("prod", g.flag+"-drain-force", "true"))
			require.NoError(t, err)
			for _, w := range eff.Warnings {
				assert.NotContains(t, w, "applies once drain is on")
			}
		})
	}
}

// TestUpgradeStrategyStoredDrainWarnings asserts that turning drain on keeps
// gracePeriod 0 and deleteEmptyDirData false that the cluster already has, and
// warns, naming the value and the flag that changes it; and that the warning
// does not appear where that value is not the one that applies.
func TestUpgradeStrategyStoredDrainWarnings(t *testing.T) {
	t.Parallel()
	const (
		graceText = "cluster prod has gracePeriod 0 for draining the %s nodes; a pod gets no time to stop (the command keeps it; --%s-grace-period changes it)"
		emptyText = "cluster prod has deleteEmptyDirData false for draining the %s nodes; the drain fails when a pod uses an emptyDir volume (the command keeps it; --%s-delete-emptydir changes it)"
	)
	zeros := func(enabled bool) map[string]any {
		return map[string]any{
			"enabled": enabled, "force": false, "deleteEmptyDirData": false, "disableEviction": false,
			"gracePeriod": int64(0), "timeout": int64(0), "skipWaitForDeleteTimeoutSeconds": int64(0),
		}
	}
	for _, g := range []struct{ flag, label, field string }{{"control-plane", "control plane", "controlPlaneDrainOptions"}, {"worker", "worker", "workerDrainOptions"}} {
		grace, empty := fmt.Sprintf(graceText, g.label, g.flag), fmt.Sprintf(emptyText, g.label, g.flag)
		rows := []struct {
			name      string
			stored    map[string]any
			pairs     []string
			wantGrace bool
			wantEmpty bool
			wantKept  map[string]any
		}{
			{"every option is zero in the cluster", zeros(false), []string{g.flag + "-drain", "true"}, true, true, map[string]any{"gracePeriod": int64(0), "deleteEmptyDirData": false, "timeout": int64(0)}},
			{"only the grace period is 0", map[string]any{"enabled": false, "gracePeriod": int64(0), "deleteEmptyDirData": true}, []string{g.flag + "-drain", "true"}, true, false, map[string]any{"gracePeriod": int64(0), "deleteEmptyDirData": true}},
			{"only delete emptyDir data is false", map[string]any{"enabled": false, "gracePeriod": int64(30), "deleteEmptyDirData": false}, []string{g.flag + "-drain", "true"}, false, true, map[string]any{"gracePeriod": int64(30), "deleteEmptyDirData": false}},
			{"a float64 zero from the server", map[string]any{"enabled": false, "gracePeriod": float64(0)}, []string{g.flag + "-drain", "true"}, true, false, nil},
			{"the fields are absent", map[string]any{"enabled": false}, []string{g.flag + "-drain", "true"}, false, false, map[string]any{"gracePeriod": int64(-1), "deleteEmptyDirData": true}},
			{"a typed grace period replaces gracePeriod 0 the cluster has", zeros(false), []string{g.flag + "-drain", "true", g.flag + "-grace-period", "60"}, false, true, map[string]any{"gracePeriod": int64(60), "deleteEmptyDirData": false}},
			{"a typed delete emptyDir replaces deleteEmptyDirData false the cluster has", zeros(false), []string{g.flag + "-drain", "true", g.flag + "-delete-emptydir", "true"}, true, false, map[string]any{"gracePeriod": int64(0), "deleteEmptyDirData": true}},
			{"drain already enabled", zeros(true), []string{g.flag + "-drain", "true", g.flag + "-drain-force", "true"}, false, false, map[string]any{"gracePeriod": int64(0), "deleteEmptyDirData": false}},
			{"drain turned off", zeros(true), []string{g.flag + "-drain", "false"}, false, false, nil},
			{"drain not typed", zeros(false), []string{g.flag + "-concurrency", "2"}, false, false, map[string]any{"gracePeriod": int64(0), "deleteEmptyDirData": false}},
		}
		for _, tc := range rows {
			t.Run(g.flag+" "+tc.name, func(t *testing.T) {
				t.Parallel()
				f := newUpgradeFixture(t)
				f.putProvisioned(provisionedWith(map[string]any{g.field: tc.stored}))
				eff, _, err := f.planStrategy(t, strategyInput("prod", tc.pairs...))
				require.NoError(t, err)
				assert.Equal(t, tc.wantGrace, slices.Contains(eff.Warnings, grace), "%q", eff.Warnings)
				assert.Equal(t, tc.wantEmpty, slices.Contains(eff.Warnings, empty), "%q", eff.Warnings)
				got, _, _ := unstructured.NestedMap(f.lastPreview(t).Object, "spec", "rkeConfig", "upgradeStrategy", g.field)
				for field, want := range tc.wantKept {
					assert.Equal(t, want, got[field], field)
				}
			})
		}
		t.Run(g.flag+" the other group's zeros in the cluster give no warning", func(t *testing.T) {
			t.Parallel()
			other := "workerDrainOptions"
			if g.field == other {
				other = "controlPlaneDrainOptions"
			}
			f := newUpgradeFixture(t)
			f.putProvisioned(provisionedWith(map[string]any{g.field: map[string]any{"enabled": false}, other: zeros(false)}))
			eff, _, err := f.planStrategy(t, strategyInput("prod", g.flag+"-drain", "true"))
			require.NoError(t, err)
			for _, w := range eff.Warnings {
				assert.NotContains(t, w, "has gracePeriod")
				assert.NotContains(t, w, "has deleteEmptyDirData")
			}
		})
	}
}

// TestUpgradeStrategyImportedFlags asserts an imported cluster refuses each of
// the eight drain options and every concurrency that is not a whole number of 1
// or more, after the cluster is known and before the dry-run, and takes the two
// drain flags.
func TestUpgradeStrategyImportedFlags(t *testing.T) {
	t.Parallel()
	const refusal = "--%s is for a provisioned cluster only; the upgrade plan of an imported cluster (Rancher's system-upgrade-controller) drains with force and has no other drain option; only --control-plane-drain and --worker-drain apply"
	for _, group := range []string{"control-plane", "worker"} {
		for _, detail := range []struct{ ending, value string }{{"drain-force", "true"}, {"delete-emptydir", "true"}, {"grace-period", "60"}, {"drain-timeout", "5m"}} {
			flag := group + "-" + detail.ending
			t.Run(flag, func(t *testing.T) {
				t.Parallel()
				f := newUpgradeFixture(t)
				f.importedWith("rke2", new("true"), map[string]any{})
				_, _, err := f.planStrategy(t, strategyInput("edge", flag, detail.value))
				ce := asCode(t, err)
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				assert.Equal(t, fmt.Sprintf(refusal, flag), ce.Message)
				assert.Empty(t, f.local.updates)
			})
		}
		for _, value := range []string{"0", "-1", "2.5", "50%", "abc", "1e2", "+3x"} {
			t.Run(group+" concurrency "+value, func(t *testing.T) {
				t.Parallel()
				f := newUpgradeFixture(t)
				f.importedWith("k3s", new("true"), map[string]any{})
				_, _, err := f.planStrategy(t, strategyInput("edge", group+"-concurrency", value))
				ce := asCode(t, err)
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				assert.Equal(t, fmt.Sprintf("--%s-concurrency %q must be a whole number of 1 or more for an imported cluster", group, value), ce.Message)
				assert.Empty(t, f.local.updates)
			})
		}
	}
}

// TestUpgradeStrategyImportedWritesIntegers asserts an imported cluster's
// concurrency is written as an integer and the fields are those of its
// distribution.
func TestUpgradeStrategyImportedWritesIntegers(t *testing.T) {
	t.Parallel()
	for _, distro := range []string{"rke2", "k3s"} {
		f := newUpgradeFixture(t)
		f.importedWith(distro, new("true"), map[string]any{})
		_, _, err := f.planStrategy(t, strategyInput("edge", "control-plane-concurrency", "2", "worker-concurrency", "7"))
		require.NoError(t, err, distro)
		strategy, found, _ := unstructured.NestedMap(f.lastPreview(t).Object, "spec", distro+"Config", distro+"upgradeStrategy")
		require.True(t, found, distro)
		assert.Equal(t, map[string]any{"serverConcurrency": int64(2), "workerConcurrency": int64(7)}, strategy, distro)
	}
}

// TestUpgradeStrategyImportedAnnotation asserts the version management
// annotation is set to system-default when it is absent or empty, left as it
// is otherwise, and that adding it comes with its warning.
func TestUpgradeStrategyImportedAnnotation(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		annotation *string
		want       string
		added      bool
	}{
		"absent": {nil, "system-default", true}, "empty": {new(""), "system-default", true},
		"true": {new("true"), "true", false}, "false": {new("false"), "false", false}, "system-default": {new("system-default"), "system-default", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.putVersionManagementSetting("true")
			f.importedWith("rke2", tc.annotation, map[string]any{})
			eff, _, err := f.planStrategy(t, strategyInput("edge", "worker-concurrency", "2"))
			require.NoError(t, err)
			value, _, _ := unstructured.NestedString(f.lastPreview(t).Object, "metadata", "annotations", versionManagementAnnotation)
			assert.Equal(t, tc.want, value)
			has := slices.Contains(eff.Warnings, annotationWarning("edge"))
			assert.Equal(t, tc.added, has, "%q", eff.Warnings)
		})
	}
}

// TestUpgradeStrategyRestoreRefusal asserts only a running etcd snapshot restore
// that rewrites the upgrade strategy (restoreRKEConfig all) refuses, with text
// that names restoreRKEConfig all and no hint. A restore that
// ended, another restore, another operation and an upgrade refuse nothing.
func TestUpgradeStrategyRestoreRefusal(t *testing.T) {
	t.Parallel()
	restore := func(config string) map[string]any {
		r := map[string]any{"name": "s1", "generation": int64(2)}
		if config != "" {
			r["restoreRKEConfig"] = config
		}
		return r
	}
	controlPlane := func(phase string, applied map[string]any) *unstructured.Unstructured {
		status := map[string]any{}
		if applied != nil {
			status["etcdSnapshotRestore"] = applied
		}
		if phase != "" {
			status["etcdSnapshotRestorePhase"] = phase
		}
		return &unstructured.Unstructured{Object: map[string]any{"status": status}}
	}
	rows := []struct {
		name    string
		request map[string]any
		cp      *unstructured.Unstructured
		refuses bool
		message string
	}{
		{"all, status copy differs", restore("all"), controlPlane("", nil), true, ""},
		{"all, running phase", restore("all"), controlPlane("Restarting", restore("all")), true, "cluster prod has an etcd snapshot restore in progress (phase Restarting); it restores the cluster configuration (restoreRKEConfig all) and would replace the upgrade strategy this command writes; try again after it ends"},
		{"all, status copy equal, finished", restore("all"), controlPlane("Finished", restore("all")), false, ""},
		{"all, status copy equal, failed", restore("all"), controlPlane("Failed", restore("all")), false, ""},
		{"all, status copy equal, no phase", restore("all"), controlPlane("", restore("all")), true, ""},
		{"all, status copy differs, finished", restore("all"), controlPlane("Finished", restore("none")), true, ""},
		{"none, running", restore("none"), controlPlane("Restarting", nil), false, ""},
		{"empty, running", restore(""), controlPlane("", nil), false, ""},
		{"kubernetes version, running", restore("kubernetesVersion"), controlPlane("Restarting", nil), false, ""},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			prov := provisionedWith(nil)
			require.NoError(t, unstructured.SetNestedField(prov.Object, tc.request, "spec", "rkeConfig", "etcdSnapshotRestore"))
			f.putProvisioned(prov)
			f.local.put(rkeControlPlaneGVR, "fleet-default", "prod", tc.cp)
			_, _, err := f.planStrategy(t, strategyInput("prod", "worker-concurrency", "2"))
			if !tc.refuses {
				require.NoError(t, err)
				assert.Len(t, f.local.updates, 1)
				return
			}
			ce := asCode(t, err)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.True(t, ce.Retryable)
			assert.Empty(t, ce.Hint)
			assert.Contains(t, ce.Message, "cluster prod has an etcd snapshot restore in progress")
			assert.True(t, strings.HasSuffix(ce.Message, "; it restores the cluster configuration (restoreRKEConfig all) and would replace the upgrade strategy this command writes; try again after it ends"), ce.Message)
			if tc.message != "" {
				assert.Equal(t, tc.message, ce.Message)
			}
			assert.Empty(t, f.local.updates)
		})
	}
}

// TestUpgradeStrategyOtherOperationsRefuseNothing asserts that no other
// operation running alone, and no upgrade, stops the command.
func TestUpgradeStrategyOtherOperationsRefuseNothing(t *testing.T) {
	t.Parallel()
	requests := map[string]map[string]any{
		"etcdSnapshotCreate":   {"generation": int64(1)},
		"rotateCertificates":   {"generation": int64(1)},
		"rotateEncryptionKeys": {"generation": int64(1)},
	}
	for field, request := range requests {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			prov := provisionedWith(nil)
			require.NoError(t, unstructured.SetNestedField(prov.Object, request, "spec", "rkeConfig", field))
			f.putProvisioned(prov)
			f.local.put(rkeControlPlaneGVR, "fleet-default", "prod", &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{}}})
			_, _, err := f.planStrategy(t, strategyInput("prod", "worker-concurrency", "2"))
			require.NoError(t, err)
			assert.Len(t, f.local.updates, 1)
		})
	}
	t.Run("an upgrade", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		f.putProvisioned(provisionedWith(nil))
		f.local.put(rkeControlPlaneGVR, "fleet-default", "prod", &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"appliedSpec": map[string]any{"kubernetesVersion": "v1.30.4+rke2r1"}}}})
		_, _, err := f.planStrategy(t, strategyInput("prod", "worker-concurrency", "2"))
		require.NoError(t, err)
		assert.Len(t, f.local.updates, 1)
	})
}

// TestUpgradeStrategyControlPlaneReadFailureWarns asserts an RKEControlPlane that
// is refused, fails or is not found warns and the command goes on, and that an
// imported cluster reads no RKEControlPlane.
func TestUpgradeStrategyControlPlaneReadFailureWarns(t *testing.T) {
	t.Parallel()
	for name, readErr := range map[string]error{
		"not found": clierr.New(clierr.CodeNotFound, "missing"),
		"forbidden": clierr.New(clierr.CodeRBAC, "forbidden"),
		"failed":    clierr.New(clierr.CodeGeneric, "boom"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.putProvisioned(provisionedWith(nil))
			f.local.errs["GET "+objKey(rkeControlPlaneGVR, "fleet-default", "prod")] = readErr
			eff, _, err := f.planStrategy(t, strategyInput("prod", "worker-concurrency", "2"))
			require.NoError(t, err)
			warningsContain(t, eff.Warnings, "cannot read the RKEControlPlane of cluster prod ("+clierr.From(readErr).Code+") to check for an etcd snapshot restore in progress; such a restore would replace the upgrade strategy this command writes")
			assert.Len(t, f.local.updates, 1)
		})
	}
	t.Run("imported cluster", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		f.importedWith("rke2", new("true"), map[string]any{})
		_, _, err := f.planStrategy(t, strategyInput("edge", "worker-concurrency", "2"))
		require.NoError(t, err)
		assert.Zero(t, f.local.requestsOf("GET rkecontrolplanes"))
	})
}

// TestUpgradeStrategyRequestCounts asserts the requests of each kind of cluster:
// the provisioning Cluster, the RKEControlPlane and one dry-run for a provisioned
// cluster; the provisioning Cluster, the management Cluster through Steve, the
// Setting only when the annotation leaves it open, and one dry-run for an
// imported one. The write adds one request.
func TestUpgradeStrategyRequestCounts(t *testing.T) {
	t.Parallel()
	t.Run("provisioned", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		f.putProvisioned(provisionedWith(nil))
		_, exec, err := f.planStrategy(t, strategyInput("prod", "worker-concurrency", "2"))
		require.NoError(t, err)
		assert.Equal(t, []string{"GET clusters/fleet-default/prod", "GET rkecontrolplanes/fleet-default/prod", "PUT clusters/fleet-default/prod"}, f.local.requests)
		_, err = exec(t.Context())
		require.NoError(t, err)
		assert.Len(t, f.local.requests, 4)
	})
	for name, tc := range map[string]struct {
		annotation *string
		settings   int
	}{"annotation true": {new("true"), 0}, "annotation absent": {nil, 1}, "annotation system-default": {new("system-default"), 1}} {
		t.Run("imported "+name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.putVersionManagementSetting("true")
			f.importedWith("rke2", tc.annotation, map[string]any{})
			_, _, err := f.planStrategy(t, strategyInput("edge", "worker-concurrency", "2"))
			require.NoError(t, err)
			assert.Equal(t, 1+tc.settings+1, len(f.local.requests), "%v", f.local.requests)
			assert.Equal(t, tc.settings, f.local.requestsOf("GET settings/"))
			assert.Equal(t, 1, f.local.requestsOf("PUT clusters//c-m-edge1"))
		})
	}
}

// TestUpgradeStrategyEffect asserts the Effect of each kind of cluster: the
// preview comes from a server dry-run, only a provisioned cluster's write is
// destructive, and an imported cluster's is not reversible exactly when a flag
// turns drain on for a cluster whose drain is not on.
func TestUpgradeStrategyEffect(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		flags      []string
		controlOn  bool
		workerOn   bool
		reversible bool
	}{
		{"concurrency only", []string{"worker-concurrency", "2"}, false, false, true},
		{"control plane drain on, cluster drain off", []string{"control-plane-drain", "true"}, false, false, false},
		{"worker drain on, cluster drain off", []string{"worker-drain", "true"}, false, false, false},
		{"control plane drain on, cluster drain on", []string{"control-plane-drain", "true"}, true, false, true},
		{"drain off, cluster drain on", []string{"control-plane-drain", "false"}, true, false, true},
		{"drain off, cluster drain off", []string{"worker-drain", "false"}, false, false, true},
		{"both typed on, one turning on", []string{"control-plane-drain", "true", "worker-drain", "true"}, true, false, false},
		{"both typed on, both already on", []string{"control-plane-drain", "true", "worker-drain", "true"}, true, true, true},
		{"one on, one off", []string{"control-plane-drain", "false", "worker-drain", "true"}, true, true, true},
	} {
		t.Run("imported "+tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.importedWith("rke2", new("true"), map[string]any{"drainServerNodes": tc.controlOn, "drainWorkerNodes": tc.workerOn})
			eff, _, err := f.planStrategy(t, strategyInput("edge", tc.flags...))
			require.NoError(t, err)
			assert.False(t, eff.Destructive)
			assert.Equal(t, tc.reversible, eff.Reversible)
			assert.Equal(t, safety.PreviewServerDryRun, eff.PreviewSource)
			assert.Equal(t, "c-m-edge1", eff.DisplayName)
			assert.True(t, eff.ConflictDetection)
			assert.Equal(t, "9", eff.ResourceVersion)
		})
		t.Run("provisioned "+tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.putProvisioned(provisionedWith(map[string]any{"controlPlaneDrainOptions": map[string]any{"enabled": tc.controlOn}, "workerDrainOptions": map[string]any{"enabled": tc.workerOn}}))
			eff, _, err := f.planStrategy(t, strategyInput("prod", tc.flags...))
			require.NoError(t, err)
			assert.True(t, eff.Destructive)
			assert.True(t, eff.Reversible, "a provisioned cluster's change is always reversible")
			assert.Equal(t, safety.PreviewServerDryRun, eff.PreviewSource)
			assert.Equal(t, "prod", eff.DisplayName)
			assert.Equal(t, "7", eff.ResourceVersion)
		})
	}
}

// TestUpgradeStrategyWarnings asserts each warning of the command appears where
// it applies and not where it does not.
func TestUpgradeStrategyWarnings(t *testing.T) {
	t.Parallel()
	const (
		always   = "the strategy applies to the next upgrade and to machine rollouts"
		running  = "an upgrade is in progress; the new strategy applies to it at once"
		turnedOn = "turning drain on adds the drain to Rancher's upgrade plan; turning it off later does not remove it"
		kept     = "Rancher keeps the drain in its existing upgrade plan; this change does not remove it"
		same     = "cluster %s already has the values you typed; the command changes nothing"
	)
	setCondition := func(obj *unstructured.Unstructured, typ, status string) {
		require.NoError(t, unstructured.SetNestedSlice(obj.Object, []any{map[string]any{"type": typ, "status": status, "message": "x"}}, "status", "conditions"))
	}
	t.Run("provisioned", func(t *testing.T) {
		t.Parallel()
		for name, tc := range map[string]struct {
			status  string
			warning bool
		}{"updated true": {"True", false}, "updated false": {"False", true}, "updated unknown": {"Unknown", true}} {
			t.Run("running upgrade "+name, func(t *testing.T) {
				t.Parallel()
				f := newUpgradeFixture(t)
				prov := provisionedWith(nil)
				setCondition(prov, "Updated", tc.status)
				f.putProvisioned(prov)
				eff, _, err := f.planStrategy(t, strategyInput("prod", "worker-concurrency", "2"))
				require.NoError(t, err)
				assert.Equal(t, tc.warning, slices.Contains(eff.Warnings, running), "%q", eff.Warnings)
				assert.Contains(t, eff.Warnings, always)
			})
		}
		t.Run("no Updated condition", func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			prov := provisionedWith(nil)
			require.NoError(t, unstructured.SetNestedSlice(prov.Object, []any{}, "status", "conditions"))
			f.putProvisioned(prov)
			eff, _, err := f.planStrategy(t, strategyInput("prod", "worker-concurrency", "2"))
			require.NoError(t, err)
			assert.NotContains(t, eff.Warnings, running)
		})
		t.Run("zero concurrency", func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.putProvisioned(provisionedWith(map[string]any{"controlPlaneConcurrency": "0"}))
			eff, _, err := f.planStrategy(t, strategyInput("prod", "worker-concurrency", "0"))
			require.NoError(t, err)
			assert.Contains(t, eff.Warnings, "every control plane node is upgraded at once")
			assert.Contains(t, eff.Warnings, "every worker node is upgraded at once")
		})
		t.Run("no zero concurrency", func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.putProvisioned(provisionedWith(nil))
			eff, _, err := f.planStrategy(t, strategyInput("prod", "worker-concurrency", "2"))
			require.NoError(t, err)
			for _, w := range eff.Warnings {
				assert.NotContains(t, w, "upgraded at once")
			}
		})
		t.Run("nothing changes", func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.putProvisioned(provisionedWith(map[string]any{"workerConcurrency": "2"}))
			eff, exec, err := f.planStrategy(t, strategyInput("prod", "worker-concurrency", "2"))
			require.NoError(t, err)
			assert.Contains(t, eff.Warnings, fmt.Sprintf(same, "prod"))
			assert.Empty(t, eff.Diff)
			assert.Len(t, f.local.updates, 1, "the object as read still goes through the dry-run")
			_, err = exec(t.Context())
			require.NoError(t, err)
			assert.Len(t, f.local.updates, 2)
		})
		t.Run("something changes", func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.putProvisioned(provisionedWith(map[string]any{"workerConcurrency": "2"}))
			eff, _, err := f.planStrategy(t, strategyInput("prod", "worker-concurrency", "3"))
			require.NoError(t, err)
			assert.NotContains(t, eff.Warnings, fmt.Sprintf(same, "prod"))
		})
		t.Run("the imported warnings do not appear", func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.putProvisioned(provisionedWith(map[string]any{"workerDrainOptions": map[string]any{"enabled": true}}))
			eff, _, err := f.planStrategy(t, strategyInput("prod", "worker-drain", "false", "control-plane-drain", "true"))
			require.NoError(t, err)
			assert.NotContains(t, eff.Warnings, turnedOn)
			assert.NotContains(t, eff.Warnings, kept)
		})
	})

	t.Run("imported", func(t *testing.T) {
		t.Parallel()
		for name, tc := range map[string]struct {
			annotation *string
			setting    string
			want       string
			absent     bool
		}{
			"annotation true":                     {new("true"), "false", "", true},
			"annotation false":                    {new("false"), "true", "version management is off for cluster edge; the strategy applies once it is on", false},
			"system-default, setting true":        {new("system-default"), "true", "", true},
			"system-default, setting false":       {new("system-default"), "false", "version management is off for cluster edge; the strategy applies once it is on", false},
			"absent, setting false":               {nil, "false", "version management is off for cluster edge; the strategy applies once it is on", false},
			"empty, setting true":                 {new(""), "true", "", true},
			"system-default, setting not present": {new("system-default"), "", "cannot tell whether version management is on for cluster edge (the setting imported-cluster-version-management cannot be read: NOT_FOUND); the strategy applies once it is on", false},
		} {
			t.Run("version management "+name, func(t *testing.T) {
				t.Parallel()
				f := newUpgradeFixture(t)
				if tc.setting != "" {
					f.putVersionManagementSetting(tc.setting)
				}
				f.importedWith("rke2", tc.annotation, map[string]any{})
				eff, _, err := f.planStrategy(t, strategyInput("edge", "worker-concurrency", "2"))
				require.NoError(t, err)
				for _, w := range eff.Warnings {
					if strings.Contains(w, "version management is off") || strings.Contains(w, "cannot tell whether version management") {
						assert.Equal(t, tc.want, w)
						return
					}
				}
				assert.True(t, tc.absent, "no version management warning: %q", eff.Warnings)
			})
		}
		for name, tc := range map[string]struct {
			status  string
			warning bool
		}{"upgraded true": {"True", false}, "upgraded unknown": {"Unknown", true}, "upgraded false": {"False", false}} {
			t.Run("running upgrade "+name, func(t *testing.T) {
				t.Parallel()
				f := newUpgradeFixture(t)
				f.importedWith("rke2", new("true"), map[string]any{})
				m := f.lastStored(t, "c-m-edge1")
				setCondition(m, "Upgraded", tc.status)
				f.local.put(managementClusterRow.GVR, "", "c-m-edge1", m)
				f.steve.set(m)
				eff, _, err := f.planStrategy(t, strategyInput("edge", "worker-concurrency", "2"))
				require.NoError(t, err)
				assert.Equal(t, tc.warning, slices.Contains(eff.Warnings, running), "%q", eff.Warnings)
				assert.NotContains(t, eff.Warnings, always, "the provisioned warning is for a provisioned cluster")
			})
		}
		for name, tc := range map[string]struct {
			stored map[string]any
			flags  []string
			on     bool
			kept   bool
		}{
			"turn on, cluster drain off":    {map[string]any{"drainWorkerNodes": false}, []string{"worker-drain", "true"}, true, false},
			"turn on, cluster drain absent": {map[string]any{}, []string{"control-plane-drain", "true"}, true, false},
			"turn on, cluster drain on":     {map[string]any{"drainWorkerNodes": true}, []string{"worker-drain", "true"}, false, false},
			"turn off, cluster drain on":    {map[string]any{"drainWorkerNodes": true}, []string{"worker-drain", "false"}, false, true},
			"turn off, cluster drain off":   {map[string]any{"drainWorkerNodes": false}, []string{"worker-drain", "false"}, false, false},
			"concurrency only":              {map[string]any{"drainWorkerNodes": true}, []string{"worker-concurrency", "2"}, false, false},
			"one on and one off typed":      {map[string]any{"drainWorkerNodes": true}, []string{"worker-drain", "false", "control-plane-drain", "true"}, true, true},
		} {
			t.Run("drain "+name, func(t *testing.T) {
				t.Parallel()
				f := newUpgradeFixture(t)
				f.importedWith("rke2", new("true"), tc.stored)
				eff, _, err := f.planStrategy(t, strategyInput("edge", tc.flags...))
				require.NoError(t, err)
				assert.Equal(t, tc.on, slices.Contains(eff.Warnings, turnedOn), "%q", eff.Warnings)
				assert.Equal(t, tc.kept, slices.Contains(eff.Warnings, kept), "%q", eff.Warnings)
			})
		}
		for _, flag := range []string{"worker-drain", "control-plane-drain"} {
			t.Run("turn off, drain field absent writes nothing "+flag, func(t *testing.T) {
				t.Parallel()
				f := newUpgradeFixture(t)
				f.importedWith("rke2", new("true"), map[string]any{"workerConcurrency": int64(2)})
				eff, _, err := f.planStrategy(t, strategyInput("edge", flag, "false"))
				require.NoError(t, err)
				assert.Empty(t, eff.Diff)
				assert.Contains(t, eff.Warnings, fmt.Sprintf(same, "edge"))
				got, _, _ := unstructured.NestedMap(f.lastPreview(t).Object, "spec", "rke2Config", "rke2upgradeStrategy")
				assert.NotContains(t, got, "drainWorkerNodes")
				assert.NotContains(t, got, "drainServerNodes")
			})
		}
		t.Run("turn off one group, the other drain on is untouched", func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.importedWith("rke2", new("true"), map[string]any{"drainServerNodes": true})
			eff, _, err := f.planStrategy(t, strategyInput("edge", "worker-drain", "false"))
			require.NoError(t, err)
			assert.Empty(t, eff.Diff)
			assert.Contains(t, eff.Warnings, fmt.Sprintf(same, "edge"))
		})
		t.Run("nothing changes", func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.importedWith("rke2", new("true"), map[string]any{"workerConcurrency": int64(2)})
			eff, _, err := f.planStrategy(t, strategyInput("edge", "worker-concurrency", "2"))
			require.NoError(t, err)
			assert.Contains(t, eff.Warnings, fmt.Sprintf(same, "edge"))
			assert.Empty(t, eff.Diff)
		})
		t.Run("equal values with the annotation to add", func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.putVersionManagementSetting("true")
			f.importedWith("rke2", nil, map[string]any{"workerConcurrency": int64(2)})
			eff, _, err := f.planStrategy(t, strategyInput("edge", "worker-concurrency", "2"))
			require.NoError(t, err)
			assert.Contains(t, eff.Warnings, annotationWarning("edge"))
			assert.NotContains(t, eff.Warnings, fmt.Sprintf(same, "edge"))
			assert.NotEmpty(t, eff.Diff)
		})
		t.Run("the provisioned warnings do not appear", func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.importedWith("rke2", new("true"), map[string]any{"serverConcurrency": int64(0)})
			eff, _, err := f.planStrategy(t, strategyInput("edge", "worker-concurrency", "2"))
			require.NoError(t, err)
			for _, w := range eff.Warnings {
				assert.NotContains(t, w, "upgraded at once")
				assert.NotContains(t, w, "RKEControlPlane")
			}
		})
	})
}

// TestUpgradeStrategyServerRefusalKeepsItsMessage asserts a dry-run the server
// refuses, as it does a strategy value the CRD pattern does not match, ends
// the Plan with the server's message and no write.
func TestUpgradeStrategyServerRefusalKeepsItsMessage(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	f.putProvisioned(provisionedWith(nil))
	f.local.updateErr = clierr.New(clierr.CodeValidation, `Cluster.provisioning.cattle.io "prod" is invalid: spec.rkeConfig.upgradeStrategy.workerConcurrency: Invalid value: "x": must match the pattern`)
	_, _, err := f.planStrategy(t, strategyInput("prod", "worker-concurrency", "x"))
	ce := asCode(t, err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "must match the pattern")
	assert.Empty(t, f.local.updates)
}

// TestUpgradeStrategyResult asserts the result text names the typed cluster and
// the data is the written object.
func TestUpgradeStrategyResult(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	f.putProvisioned(provisionedWith(nil))
	_, exec, err := f.planStrategy(t, strategyInput("prod", "worker-concurrency", "2"))
	require.NoError(t, err)
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "cluster/prod upgrade strategy set\n", res.Text)
	assert.Equal(t, "Cluster", res.Kind)
	data, ok := res.Data.(map[string]any)
	require.True(t, ok)
	workerConcurrency, _, _ := unstructured.NestedString(data, "spec", "rkeConfig", "upgradeStrategy", "workerConcurrency")
	assert.Equal(t, "2", workerConcurrency)
}

// TestUpgradeStrategyDeclaresNoWaitFlags asserts the command has no --wait and
// no --timeout, and every flag of the table.
func TestUpgradeStrategyDeclaresNoWaitFlags(t *testing.T) {
	t.Parallel()
	d := upgradeStrategyCmd{}.Describe()
	var names []string
	for _, flag := range d.Flags {
		names = append(names, flag.Name)
	}
	assert.ElementsMatch(t, []string{
		"control-plane-concurrency", "control-plane-drain", "control-plane-drain-force", "control-plane-delete-emptydir", "control-plane-grace-period", "control-plane-drain-timeout",
		"worker-concurrency", "worker-drain", "worker-drain-force", "worker-delete-emptydir", "worker-grace-period", "worker-drain-timeout", "namespace",
	}, names)
	assert.Equal(t, "dependsOnInput", d.Destructive)
}

// TestUpgradeStrategyWritesCarryTheFieldManager asserts the preview and the write
// of each kind of cluster reach a server as PUTs to the cluster's own path, each
// with fieldManager=cli.rancher.io, and the preview with dryRun=All.
func TestUpgradeStrategyWritesCarryTheFieldManager(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		prepare func(f *upgradeFixture)
		cluster string
		path    string
	}{
		"provisioned": {func(f *upgradeFixture) { f.putProvisioned(provisionedWith(nil)) }, "prod",
			"/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters/prod"},
		"imported": {func(f *upgradeFixture) { f.importedWith("rke2", new("true"), map[string]any{}) }, "edge",
			"/k8s/clusters/local/apis/management.cattle.io/v3/clusters/c-m-edge1"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var (
				mu     sync.Mutex
				writes []recordedWrite
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					if r.URL.Query().Get("fieldManager") != "cli.rancher.io" {
						t.Errorf("%s %s was sent without fieldManager=cli.rancher.io", r.Method, r.URL.RequestURI())
					}
					mu.Lock()
					writes = append(writes, recordedWrite{method: r.Method, path: r.URL.Path, query: r.URL.Query()})
					mu.Unlock()
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(body)
			}))
			t.Cleanup(srv.Close)
			rc, err := kube.RESTConfig(config.Effective{URL: srv.URL, Token: "id:secret"}, engine.LocalCluster)
			require.NoError(t, err)
			rc.Transport = srv.Client().Transport
			client, err := provider.NewClusterClient(rc, engine.LocalCluster)
			require.NoError(t, err)

			f := newUpgradeFixture(t)
			tc.prepare(f)
			f.local.write = client
			_, exec, err := f.planStrategy(t, strategyInput(tc.cluster, "worker-concurrency", "2"))
			require.NoError(t, err)
			_, err = exec(t.Context())
			require.NoError(t, err)

			mu.Lock()
			defer mu.Unlock()
			require.Len(t, writes, 2, "the preview and the write")
			for _, w := range writes {
				assert.Equal(t, http.MethodPut, w.method)
				assert.Equal(t, tc.path, w.path)
				assert.Equal(t, "cli.rancher.io", w.query.Get("fieldManager"))
			}
			assert.Equal(t, "All", writes[0].query.Get("dryRun"), "the preview is a server dry-run")
			assert.False(t, writes[1].query.Has("dryRun"), "the write is not")
		})
	}
}
