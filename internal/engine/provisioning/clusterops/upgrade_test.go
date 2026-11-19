package clusterops

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func provCluster(name, version string, rke map[string]any, clusterName string) map[string]any {
	spec := map[string]any{"kubernetesVersion": version}
	if rke != nil {
		spec["rkeConfig"] = rke
	}
	return map[string]any{
		"metadata": map[string]any{"name": name, "namespace": "fleet-default"},
		"spec":     spec,
		"status":   map[string]any{"clusterName": clusterName},
	}
}

type mgmtOpts struct {
	driver     string
	annotation *string
	paused     string
	configVer  string
	running    string
	noConfig   bool
	noVersion  bool
	hosted     string
}

func mgmt(id string, o mgmtOpts) map[string]any {
	annotations := map[string]any{}
	if o.annotation != nil {
		annotations["rancher.io/imported-cluster-version-management"] = *o.annotation
	}
	if o.paused != "" {
		annotations["management.cattle.io/imported-cluster-version-management-paused"] = o.paused
	}
	spec := map[string]any{}
	if !o.noConfig && (o.driver == "rke2" || o.driver == "k3s") {
		spec[o.driver+"Config"] = map[string]any{"kubernetesVersion": o.configVer}
	}
	if o.hosted != "" {
		spec[o.hosted] = map[string]any{"region": "x"}
	}
	status := map[string]any{}
	if o.driver != "" {
		status["driver"] = o.driver
	}
	if !o.noVersion {
		status["version"] = map[string]any{"gitVersion": o.running}
	}
	return map[string]any{
		"metadata": map[string]any{"name": id, "annotations": annotations},
		"spec":     spec,
		"status":   status,
	}
}

func imported(o mgmtOpts) (map[string]any, map[string]any) {
	return provCluster("edge", "", nil, "c-m-abc12"), mgmt("c-m-abc12", o)
}

// TestNeedsVersionManagementSetting asserts the Setting is read only when the
// annotation is system-default, empty or absent.
func TestNeedsVersionManagementSetting(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]bool{"system-default": true, "": true, "true": false, "false": false, "other": false} {
		assert.Equal(t, want, NeedsVersionManagementSetting(mgmt("c", mgmtOpts{annotation: new(value)})), value)
	}
	assert.True(t, NeedsVersionManagementSetting(mgmt("c", mgmtOpts{})), "absent")
	assert.True(t, NeedsVersionManagementSetting(nil))
}

// TestCheckUpgradeClusterProvisioned asserts the facts of a provisioned
// cluster: its distribution is read from its version text, a custom cluster's
// empty rkeConfig counts, and the azure in-tree cloud provider is seen only in
// the machine selector config without a label selector.
func TestCheckUpgradeClusterProvisioned(t *testing.T) {
	t.Parallel()
	c, err := CheckUpgradeCluster(provCluster("prod", "v1.31.4+rke2r1", map[string]any{}, "c-m-x"), nil, SettingRead{})
	require.Nil(t, err)
	assert.Equal(t, UpgradeCluster{Name: "prod", Provisioned: true, Distro: "rke2", Current: "v1.31.4+rke2r1", Stored: "v1.31.4+rke2r1"}, c)

	c, err = CheckUpgradeCluster(provCluster("edge", "v1.31.4+k3s1", map[string]any{}, ""), nil, SettingRead{})
	require.Nil(t, err)
	assert.Equal(t, "k3s", c.Distro)

	azure := map[string]any{"machineSelectorConfig": []any{
		map[string]any{"machineLabelSelector": map[string]any{"matchLabels": map[string]any{"a": "b"}}, "config": map[string]any{"cloud-provider-name": "azure"}},
		map[string]any{"config": map[string]any{"cloud-provider-name": "azure"}},
	}}
	c, err = CheckUpgradeCluster(provCluster("prod", "v1.29.4+rke2r1", azure, "c-m-x"), nil, SettingRead{})
	require.Nil(t, err)
	assert.True(t, c.AzureInTree)

	labelled := map[string]any{"machineSelectorConfig": []any{
		map[string]any{"machineLabelSelector": map[string]any{}, "config": map[string]any{"cloud-provider-name": "azure"}},
		map[string]any{"config": map[string]any{"cloud-provider-name": "aws"}},
	}}
	c, err = CheckUpgradeCluster(provCluster("prod", "v1.29.4+rke2r1", labelled, "c-m-x"), nil, SettingRead{})
	require.Nil(t, err)
	assert.False(t, c.AzureInTree)
}

// TestCheckUpgradeClusterRefusals asserts each refusal of a cluster, its code
// and its text, in the order of the checks.
func TestCheckUpgradeClusterRefusals(t *testing.T) {
	t.Parallel()
	prov := func(version string) map[string]any { return provCluster("prod", version, map[string]any{}, "c-m-x") }
	rows := []struct {
		name      string
		cluster   map[string]any
		mgmt      map[string]any
		setting   SettingRead
		code      string
		retryable bool
		text      string
		hint      bool
	}{
		{name: "local provisioned", cluster: provCluster("local", "v1.31.4+rke2r1", map[string]any{}, "local"), code: clierr.CodeValidation, text: "this CLI does not upgrade the local cluster"},
		{name: "local imported", cluster: provCluster("local", "", nil, "local"), mgmt: mgmt("local", mgmtOpts{driver: "k3s"}), code: clierr.CodeValidation, text: "this CLI does not upgrade the local cluster"},
		{name: "provisioned pre-release", cluster: prov("v1.31.0-rc1+rke2r1"), code: clierr.CodeValidation,
			text: "the current version of cluster prod (v1.31.0-rc1+rke2r1) is not MAJOR.MINOR.PATCH (a pre-release included), which this CLI cannot compare with the version to upgrade to; change spec.kubernetesVersion of provisioning cluster prod in namespace fleet-default"},
		{name: "provisioned empty version", cluster: prov(""), code: clierr.CodeValidation,
			text: "the current version of cluster prod (none) is not MAJOR.MINOR.PATCH (a pre-release included), which this CLI cannot compare with the version to upgrade to; change spec.kubernetesVersion of provisioning cluster prod in namespace fleet-default"},
		{name: "provisioned unparseable version", cluster: prov("latest"), code: clierr.CodeValidation,
			text: "the current version of cluster prod (latest) is not MAJOR.MINOR.PATCH (a pre-release included), which this CLI cannot compare with the version to upgrade to; change spec.kubernetesVersion of provisioning cluster prod in namespace fleet-default"},
		{name: "not linked", cluster: provCluster("edge", "", nil, ""), code: clierr.CodeGeneric, retryable: true,
			text: "Rancher has not linked cluster edge to its management cluster yet; try again later"},
		{name: "hosted by config", cluster: provCluster("edge", "", nil, "c-m-abc12"), mgmt: mgmt("c-m-abc12", mgmtOpts{driver: "imported", hosted: "gkeConfig"}), code: clierr.CodeValidation, hint: true,
			text: "cluster edge is a hosted GKE cluster; its Kubernetes version is set in spec.gkeConfig, which this command does not change yet"},
		{name: "hosted by driver", cluster: provCluster("edge", "", nil, "c-m-abc12"), mgmt: mgmt("c-m-abc12", mgmtOpts{driver: "AKS"}), code: clierr.CodeValidation, hint: true,
			text: "cluster edge is a hosted AKS cluster; its Kubernetes version is set in spec.aksConfig, which this command does not change yet"},
		{name: "no driver", cluster: provCluster("edge", "", nil, "c-m-abc12"), mgmt: mgmt("c-m-abc12", mgmtOpts{}), code: clierr.CodeGeneric, retryable: true,
			text: "Rancher has not detected the distribution of cluster edge yet; try again later"},
		{name: "imported driver", cluster: provCluster("edge", "", nil, "c-m-abc12"), mgmt: mgmt("c-m-abc12", mgmtOpts{driver: "imported"}), code: clierr.CodeValidation,
			text: "Rancher manages the Kubernetes version only of RKE2 and K3s clusters; cluster edge has driver imported"},
		{name: "k3os driver", cluster: provCluster("edge", "", nil, "c-m-abc12"), mgmt: mgmt("c-m-abc12", mgmtOpts{driver: "k3os"}), code: clierr.CodeValidation,
			text: "Rancher manages the Kubernetes version only of RKE2 and K3s clusters; cluster edge has driver k3os"},
		{name: "paused", cluster: provCluster("edge", "", nil, "c-m-abc12"), mgmt: mgmt("c-m-abc12", mgmtOpts{driver: "rke2", annotation: new("true"), paused: "true", configVer: "v1.31.4+rke2r1", running: "v1.31.4+rke2r1"}), code: clierr.CodeValidation, hint: true,
			text: "Rancher paused version management of cluster edge for an etcd snapshot restore; try again after the restore"},
		{name: "annotation false", cluster: provCluster("edge", "", nil, "c-m-abc12"), mgmt: mgmt("c-m-abc12", mgmtOpts{driver: "rke2", annotation: new("false"), configVer: "v1.31.4+rke2r1", running: "v1.31.4+rke2r1"}), code: clierr.CodeValidation, hint: true,
			text: "version management is off for cluster edge: annotation rancher.io/imported-cluster-version-management is false"},
		{name: "annotation another value", cluster: provCluster("edge", "", nil, "c-m-abc12"), mgmt: mgmt("c-m-abc12", mgmtOpts{driver: "rke2", annotation: new("maybe"), configVer: "v1.31.4+rke2r1", running: "v1.31.4+rke2r1"}), code: clierr.CodeValidation, hint: true,
			text: "the annotation rancher.io/imported-cluster-version-management of cluster edge is maybe; Rancher accepts only true, false or system-default"},
		{name: "setting off, annotation absent", cluster: provCluster("edge", "", nil, "c-m-abc12"), mgmt: mgmt("c-m-abc12", mgmtOpts{driver: "rke2", configVer: "v1.31.4+rke2r1", running: "v1.31.4+rke2r1"}), setting: SettingRead{Value: "false", Read: true}, code: clierr.CodeValidation, hint: true,
			text: "version management is off for cluster edge: its annotation rancher.io/imported-cluster-version-management is absent and the setting imported-cluster-version-management is false"},
		{name: "setting off, annotation empty", cluster: provCluster("edge", "", nil, "c-m-abc12"), mgmt: mgmt("c-m-abc12", mgmtOpts{driver: "rke2", annotation: new(""), configVer: "v1.31.4+rke2r1", running: "v1.31.4+rke2r1"}), setting: SettingRead{Read: true}, code: clierr.CodeValidation, hint: true,
			text: "version management is off for cluster edge: its annotation rancher.io/imported-cluster-version-management is empty and the setting imported-cluster-version-management is empty"},
		{name: "setting off, annotation system-default", cluster: provCluster("edge", "", nil, "c-m-abc12"), mgmt: mgmt("c-m-abc12", mgmtOpts{driver: "k3s", annotation: new("system-default"), configVer: "v1.31.4+k3s1", running: "v1.31.4+k3s1"}), setting: SettingRead{Value: "false", Read: true}, code: clierr.CodeValidation, hint: true,
			text: "version management is off for cluster edge: its annotation rancher.io/imported-cluster-version-management is system-default and the setting imported-cluster-version-management is false"},
		{name: "setting unreadable", cluster: provCluster("edge", "", nil, "c-m-abc12"), mgmt: mgmt("c-m-abc12", mgmtOpts{driver: "rke2", annotation: new("system-default"), configVer: "v1.31.4+rke2r1", running: "v1.31.4+rke2r1"}), setting: SettingRead{Code: clierr.CodeRBAC}, code: clierr.CodeValidation, hint: true,
			text: "cannot read the setting imported-cluster-version-management (RBAC); set the annotation to true or false on cluster edge"},
		{name: "no config", cluster: provCluster("edge", "", nil, "c-m-abc12"), mgmt: mgmt("c-m-abc12", mgmtOpts{driver: "rke2", annotation: new("true"), noConfig: true, running: "v1.31.4+rke2r1"}), code: clierr.CodeGeneric, retryable: true,
			text: "cluster edge has not reported its version yet; try again later"},
		{name: "empty config version", cluster: provCluster("edge", "", nil, "c-m-abc12"), mgmt: mgmt("c-m-abc12", mgmtOpts{driver: "rke2", annotation: new("true"), running: "v1.31.4+rke2r1"}), code: clierr.CodeGeneric, retryable: true,
			text: "cluster edge has not reported its version yet; try again later"},
		{name: "no status version", cluster: provCluster("edge", "", nil, "c-m-abc12"), mgmt: mgmt("c-m-abc12", mgmtOpts{driver: "rke2", annotation: new("true"), configVer: "v1.31.4+rke2r1", noVersion: true}), code: clierr.CodeGeneric, retryable: true,
			text: "cluster edge has not reported its version yet; try again later"},
		{name: "imported pre-release", cluster: provCluster("edge", "", nil, "c-m-abc12"), mgmt: mgmt("c-m-abc12", mgmtOpts{driver: "rke2", annotation: new("true"), configVer: "v1.31.4+rke2r1", running: "v1.32.0-rc1+rke2r1"}), code: clierr.CodeValidation,
			text: "the current version of cluster edge (v1.32.0-rc1+rke2r1) is not MAJOR.MINOR.PATCH (a pre-release included), which this CLI cannot compare with the version to upgrade to; change spec.rke2Config.kubernetesVersion of management cluster c-m-abc12"},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := CheckUpgradeCluster(tc.cluster, tc.mgmt, tc.setting)
			require.NotNil(t, err)
			assert.Equal(t, tc.text, err.Message)
			assert.Equal(t, tc.code, err.Code)
			assert.Equal(t, tc.retryable, err.Retryable)
			assert.Equal(t, tc.hint, err.Hint != "")
		})
	}
}

// TestCheckUpgradeClusterVersionManagement asserts an imported cluster is
// accepted exactly where version management is on: annotation true, or
// system-default, empty or absent with the Setting true.
func TestCheckUpgradeClusterVersionManagement(t *testing.T) {
	t.Parallel()
	annotations := []*string{nil, new(""), new("system-default"), new("true"), new("false"), new("other")}
	settings := []SettingRead{{Value: "true", Read: true}, {Value: "false", Read: true}, {Code: clierr.CodeRBAC}}
	for _, a := range annotations {
		for _, s := range settings {
			for _, paused := range []string{"", "true"} {
				cluster, m := imported(mgmtOpts{driver: "rke2", annotation: a, paused: paused, configVer: "v1.31.4+rke2r1", running: "v1.31.4+rke2r1"})
				c, err := CheckUpgradeCluster(cluster, m, s)
				annotation := "absent"
				if a != nil {
					annotation = *a
				}
				var want bool
				switch {
				case paused == "true", annotation == "false", annotation == "other":
					want = false
				case annotation == "true":
					want = true
				default:
					want = s.Read && s.Value == "true"
				}
				assert.Equal(t, want, err == nil, "annotation %s setting %+v paused %q", annotation, s, paused)
				if err == nil {
					assert.Equal(t, annotation == "absent" || annotation == "", c.AddAnnotation, "the annotation is added when absent or empty: %s", annotation)
				}
			}
		}
	}
}

// TestCheckUpgradeClusterImportedFacts asserts the facts of an imported
// cluster: its distribution is its driver, its current version is the version
// it runs, and a config version below the running one is marked manually
// upgraded.
func TestCheckUpgradeClusterImportedFacts(t *testing.T) {
	t.Parallel()
	for _, driver := range []string{"rke2", "k3s"} {
		cluster, m := imported(mgmtOpts{driver: driver, annotation: new("true"), configVer: "v1.31.4+" + driver + "r1", running: "v1.31.4+" + driver + "r1"})
		c, err := CheckUpgradeCluster(cluster, m, SettingRead{})
		require.Nil(t, err)
		assert.Equal(t, UpgradeCluster{Name: "edge", ID: "c-m-abc12", Distro: driver, Current: "v1.31.4+" + driver + "r1", Stored: "v1.31.4+" + driver + "r1"}, c)
	}
	cluster, m := imported(mgmtOpts{driver: "k3s", annotation: new("true"), configVer: "v1.30.5+k3s1", running: "v1.31.4+k3s1"})
	c, err := CheckUpgradeCluster(cluster, m, SettingRead{})
	require.Nil(t, err)
	assert.True(t, c.ManuallyUpgraded)
	assert.Equal(t, "v1.31.4+k3s1", c.Current)
	assert.Equal(t, "v1.30.5+k3s1", c.Stored)

	cluster, m = imported(mgmtOpts{driver: "k3s", annotation: new("true"), configVer: "v1.32.0+k3s1", running: "v1.31.4+k3s1"})
	c, err = CheckUpgradeCluster(cluster, m, SettingRead{})
	require.Nil(t, err)
	assert.False(t, c.ManuallyUpgraded, "a config version above the running one is a pending upgrade")
}

// TestCheckUpgradeClusterHostedFields asserts every hosted config field and
// every hosted driver is refused, each alone.
func TestCheckUpgradeClusterHostedFields(t *testing.T) {
	t.Parallel()
	rows := map[string]string{"eksConfig": "EKS", "aksConfig": "AKS", "gkeConfig": "GKE", "aliConfig": "Alibaba"}
	for field, driver := range rows {
		cluster, m := imported(mgmtOpts{driver: "imported", hosted: field})
		_, err := CheckUpgradeCluster(cluster, m, SettingRead{})
		require.NotNil(t, err, field)
		assert.Contains(t, err.Message, "hosted "+driver+" cluster", field)
		assert.Contains(t, err.Message, "spec."+field, field)

		cluster, m = imported(mgmtOpts{driver: driver})
		_, err = CheckUpgradeCluster(cluster, m, SettingRead{})
		require.NotNil(t, err, driver)
		assert.Contains(t, err.Message, "hosted "+driver+" cluster", driver)
		assert.Contains(t, err.Message, "spec."+field, driver)
	}
	cluster, m := imported(mgmtOpts{driver: "imported"})
	m["spec"].(map[string]any)["eksConfig"] = nil
	_, err := CheckUpgradeCluster(cluster, m, SettingRead{})
	require.NotNil(t, err)
	assert.NotContains(t, err.Message, "hosted", "a null config is not a hosted cluster")
}

func rows(versions ...string) []Release {
	var out []Release
	for _, v := range versions {
		out = append(out, Release{Version: v, ServerArgs: map[string]any{}})
	}
	return out
}

func provisionedCluster(current string) UpgradeCluster {
	return UpgradeCluster{Name: "prod", Provisioned: true, Distro: provisionedDistro(current), Current: current, Stored: current}
}

func importedCluster(distro, current string) UpgradeCluster {
	return UpgradeCluster{Name: "edge", ID: "c-m-abc12", Distro: distro, Current: current, Stored: current}
}

// TestCheckUpgradeTarget asserts the outcome of each (current, target) pair
// over both kinds of cluster and its code: a lower version is refused by
// patch, minor and release number, an equal one and a higher one are
// accepted, and the checks run in their order.
func TestCheckUpgradeTarget(t *testing.T) {
	t.Parallel()
	all := rows("v1.30.4+rke2r1", "v1.31.3+rke2r1", "v1.31.4+rke2r1", "v1.31.4+rke2r2", "v1.31.4+rke2r9", "v1.31.4+rke2r10", "v1.31.5+rke2r1", "v1.32.0+rke2r1",
		"v1.31.4+k3s1", "v1.31.0-rc1+rke2r1")
	table := []struct {
		name    string
		cluster UpgradeCluster
		target  string
		want    string
	}{
		{"prov patch up", provisionedCluster("v1.31.4+rke2r1"), "v1.31.5+rke2r1", ""},
		{"prov patch down", provisionedCluster("v1.31.4+rke2r1"), "v1.31.3+rke2r1", "is lower than the current version"},
		{"prov minor up", provisionedCluster("v1.31.4+rke2r1"), "v1.32.0+rke2r1", ""},
		{"prov minor down", provisionedCluster("v1.31.4+rke2r1"), "v1.30.4+rke2r1", "is lower than the current version"},
		{"prov release up", provisionedCluster("v1.31.4+rke2r1"), "v1.31.4+rke2r2", ""},
		{"prov release number up past nine", provisionedCluster("v1.31.4+rke2r9"), "v1.31.4+rke2r10", ""},
		{"prov release down", provisionedCluster("v1.31.4+rke2r2"), "v1.31.4+rke2r1", "is lower than the current version"},
		{"prov release down past nine", provisionedCluster("v1.31.4+rke2r10"), "v1.31.4+rke2r9", "is lower than the current version"},
		{"prov equal", provisionedCluster("v1.31.4+rke2r1"), "v1.31.4+rke2r1", ""},
		{"prov other distribution", provisionedCluster("v1.31.4+rke2r1"), "v1.31.4+k3s1", "runs RKE2; Rancher does not change the distribution of a cluster"},
		{"prov pre-release target", provisionedCluster("v1.31.4+rke2r1"), "v1.31.0-rc1+rke2r1", "is not MAJOR.MINOR.PATCH (a pre-release included), which this CLI cannot compare with the current version v1.31.4+rke2r1"},
		{"prov unparseable target", provisionedCluster("v1.31.4+rke2r1"), "latest", "is not MAJOR.MINOR.PATCH (a pre-release included), which this CLI cannot compare with the current version v1.31.4+rke2r1"},
		{"prov unlisted target", provisionedCluster("v1.31.4+rke2r1"), "v1.33.0+rke2r1", "is not an RKE2 release this server offers for an upgrade"},
		{"prov no v prefix", provisionedCluster("v1.31.4+rke2r1"), "1.31.5+rke2r1", "is not an RKE2 release this server offers for an upgrade"},
		{"reg patch up", importedCluster("rke2", "v1.31.4+rke2r1"), "v1.31.5+rke2r1", ""},
		{"reg patch down", importedCluster("rke2", "v1.31.4+rke2r1"), "v1.31.3+rke2r1", "on Rancher v2.16 and later"},
		{"reg release up", importedCluster("rke2", "v1.31.4+rke2r1"), "v1.31.4+rke2r2", ""},
		{"reg release up past nine", importedCluster("rke2", "v1.31.4+rke2r9"), "v1.31.4+rke2r10", "Rancher compares v1.31.4+rke2r10 with v1.31.4+rke2r9 as text, where rke2r10 is lower than rke2r9, and would not upgrade an imported cluster to it"},
		{"reg release down past nine", importedCluster("rke2", "v1.31.4+rke2r10"), "v1.31.4+rke2r9", "is lower than the current version"},
		{"reg equal", importedCluster("rke2", "v1.31.4+rke2r1"), "v1.31.4+rke2r1", ""},
		{"reg other distribution", importedCluster("rke2", "v1.31.4+rke2r1"), "v1.31.4+k3s1", "runs RKE2; Rancher does not change the distribution of a cluster"},
		{"reg k3s target on rke2 list", importedCluster("k3s", "v1.30.5+k3s1"), "v1.31.4+rke2r1", "runs K3s; Rancher does not change the distribution of a cluster"},
	}
	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := CheckUpgradeTarget(tc.cluster, all, tc.target)
			if tc.want == "" {
				assert.Nil(t, err)
				return
			}
			require.NotNil(t, err)
			assert.Equal(t, clierr.CodeValidation, err.Code)
			assert.Contains(t, err.Message, tc.want)
		})
	}
}

// TestCheckUpgradeTargetNeedsServerArgs asserts a row with no serverArgs, with
// a null one and with an empty object: the first two are not accepted, the
// empty object is.
func TestCheckUpgradeTargetNeedsServerArgs(t *testing.T) {
	t.Parallel()
	releases := []Release{
		{Version: "v1.31.5+rke2r1"},
		{Version: "v1.31.6+rke2r1", ServerArgs: nil},
		{Version: "v1.31.7+rke2r1", ServerArgs: map[string]any{}},
		{Version: "v1.31.8+rke2r1", ServerArgs: map[string]any{"a": "b"}},
	}
	c := provisionedCluster("v1.31.4+rke2r1")
	assert.NotNil(t, CheckUpgradeTarget(c, releases, "v1.31.5+rke2r1"))
	assert.NotNil(t, CheckUpgradeTarget(c, releases, "v1.31.6+rke2r1"))
	assert.Nil(t, CheckUpgradeTarget(c, releases, "v1.31.7+rke2r1"))
	assert.Nil(t, CheckUpgradeTarget(c, releases, "v1.31.8+rke2r1"))
	assert.Equal(t, []string{"v1.31.8+rke2r1", "v1.31.7+rke2r1"}, AcceptedTargets(c, releases))
}

// TestCheckUpgradeTargetOrder asserts a pre-release is refused as a
// pre-release before it is looked up, the other distribution is named before
// the missing row, and an unlisted lower version is refused as unlisted.
func TestCheckUpgradeTargetOrder(t *testing.T) {
	t.Parallel()
	c := provisionedCluster("v1.31.4+rke2r1")
	err := CheckUpgradeTarget(c, rows("v1.31.4+rke2r1"), "v1.30.0-rc1+k3s1")
	require.NotNil(t, err)
	assert.Contains(t, err.Message, "pre-release")
	err = CheckUpgradeTarget(c, rows("v1.31.4+rke2r1"), "v1.30.0+k3s1")
	require.NotNil(t, err)
	assert.Contains(t, err.Message, "does not change the distribution")
	err = CheckUpgradeTarget(c, rows("v1.31.4+rke2r1"), "v1.30.0+rke2r1")
	require.NotNil(t, err)
	assert.Contains(t, err.Message, "release this server offers for an upgrade")
}

// TestCheckUpgradeTargetAzure asserts a provisioned cluster with the azure
// in-tree cloud provider refuses 1.30.0 and newer and takes older versions.
func TestCheckUpgradeTargetAzure(t *testing.T) {
	t.Parallel()
	releases := rows("v1.29.9+rke2r1", "v1.30.0+rke2r1", "v1.31.4+rke2r1")
	c := provisionedCluster("v1.29.4+rke2r1")
	c.AzureInTree = true
	assert.Nil(t, CheckUpgradeTarget(c, releases, "v1.29.9+rke2r1"))
	for _, target := range []string{"v1.30.0+rke2r1", "v1.31.4+rke2r1"} {
		err := CheckUpgradeTarget(c, releases, target)
		require.NotNil(t, err, target)
		assert.Equal(t, "cluster prod uses the azure in-tree cloud provider, which Kubernetes 1.30 removed; the Rancher UI does not offer 1.30 or newer for it", err.Message)
		assert.Equal(t, clierr.CodeValidation, err.Code)
	}
	c.AzureInTree = false
	assert.Nil(t, CheckUpgradeTarget(c, releases, "v1.31.4+rke2r1"))
	r := importedCluster("rke2", "v1.29.4+rke2r1")
	r.AzureInTree = true
	assert.Nil(t, CheckUpgradeTarget(r, releases, "v1.31.4+rke2r1"), "the azure rule is for provisioned clusters only")
}

// TestAcceptedTargets asserts the accepted rows are the ones
// CheckUpgradeTarget accepts, newest first, verbatim, without the current
// version.
func TestAcceptedTargets(t *testing.T) {
	t.Parallel()
	releases := append(rows("v1.30.4+rke2r1", "v1.31.3+rke2r1", "v1.31.4+rke2r1", "v1.31.4+rke2r2", "v1.31.4+rke2r10", "v1.31.4+rke2r9", "v1.32.0+rke2r1",
		"v1.31.4+k3s1", "v1.33.0-rc1+rke2r1"), Release{Version: "v1.31.9+rke2r1"})
	c := provisionedCluster("v1.31.4+rke2r1")
	want := []string{"v1.32.0+rke2r1", "v1.31.4+rke2r10", "v1.31.4+rke2r9", "v1.31.4+rke2r2"}
	assert.Equal(t, want, AcceptedTargets(c, releases))

	r := importedCluster("rke2", "v1.31.4+rke2r9")
	assert.Equal(t, []string{"v1.32.0+rke2r1"}, AcceptedTargets(r, releases), "the imported rule drops rke2r10, which Rancher compares as text; rke2r2 is lower")
	for _, v := range want {
		assert.Nil(t, CheckUpgradeTarget(c, releases, v), v)
	}
}

// TestAcceptedTargetsHaveNoDuplicatesAndOmitTheCurrentVersion asserts an
// imported cluster whose config version is above the version it runs gets
// that version once, a row listed twice appears once, and the current version
// is left out whether or not the stored version equals it.
func TestAcceptedTargetsHaveNoDuplicatesAndOmitTheCurrentVersion(t *testing.T) {
	t.Parallel()
	releases := rows("v1.31.4+rke2r1", "v1.31.5+rke2r1", "v1.31.5+rke2r1", "v1.32.0+rke2r1")
	above := importedCluster("rke2", "v1.31.4+rke2r1")
	above.Stored = "v1.31.5+rke2r1"
	assert.Equal(t, []string{"v1.32.0+rke2r1", "v1.31.5+rke2r1"}, AcceptedTargets(above, releases))
	same := importedCluster("rke2", "v1.31.4+rke2r1")
	assert.Equal(t, []string{"v1.32.0+rke2r1", "v1.31.5+rke2r1"}, AcceptedTargets(same, releases))
	assert.NotContains(t, AcceptedTargets(same, releases), same.Current)
}

// TestNewestSuccessfulSnapshot asserts the newest successful etcd snapshot
// object of the cluster is chosen: failed ones, other clusters' and objects
// with no parseable time are not.
func TestNewestSuccessfulSnapshot(t *testing.T) {
	t.Parallel()
	rec := func(name, cluster, status, at string) map[string]any {
		return map[string]any{
			"metadata":     map[string]any{"name": name},
			"spec":         map[string]any{"clusterName": cluster},
			"snapshotFile": map[string]any{"status": status, "createdAt": at},
		}
	}
	items := []map[string]any{
		rec("old", "prod", "successful", "2026-11-01T10:00:00Z"),
		rec("newest-failed", "prod", "failed", "2026-11-05T10:00:00Z"),
		rec("other", "other", "successful", "2026-11-06T10:00:00Z"),
		rec("new", "prod", "successful", "2026-11-03T10:00:00Z"),
		rec("no-time", "prod", "successful", ""),
	}
	got, ok := NewestSuccessfulSnapshot(items, "prod")
	require.True(t, ok)
	assert.Equal(t, "new", nestedString(got, "metadata", "name"))
	_, ok = NewestSuccessfulSnapshot(items, "nobody")
	assert.False(t, ok)
	_, ok = NewestSuccessfulSnapshot(nil, "prod")
	assert.False(t, ok)
	got, ok = NewestSuccessfulSnapshot(items, "c-m-abc12")
	assert.False(t, ok, "an imported cluster's etcd snapshot objects name the cluster's id")
	assert.Nil(t, got)
}
