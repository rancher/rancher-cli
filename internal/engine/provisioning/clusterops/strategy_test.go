package clusterops

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCheckStrategyClusterProvisioned asserts a provisioned cluster is accepted
// whatever its version text says: the strategy needs no version.
func TestCheckStrategyClusterProvisioned(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"v1.31.4+rke2r1", "v1.31.4+k3s1", "v1.32.0-rc1+rke2r1", "", "latest"} {
		for name, rke := range map[string]map[string]any{"empty rkeConfig": {}, "populated rkeConfig": {"upgradeStrategy": map[string]any{"workerConcurrency": "10%"}}} {
			c, err := CheckStrategyCluster(provCluster("prod", version, rke, "c-m-x"), nil)
			require.Nil(t, err, "%s %q", name, version)
			assert.Equal(t, StrategyCluster{Name: "prod", Provisioned: true}, c, "%s %q", name, version)
		}
	}
}

// strategyRefusal is one cluster CheckStrategyCluster refuses and the refusal.
type strategyRefusal struct {
	name      string
	cluster   map[string]any
	mgmt      map[string]any
	code      string
	retryable bool
	text      string
	hint      bool
}

// TestCheckStrategyClusterRefusals asserts every cluster the strategy command
// cannot write is refused with its code and text, and that the refusals are
// the ones of the version upgrade command where the rule is the same.
func TestCheckStrategyClusterRefusals(t *testing.T) {
	t.Parallel()
	linked := func() map[string]any { return provCluster("edge", "", nil, "c-m-abc12") }
	ready := func(driver string, annotation *string) map[string]any {
		return mgmt("c-m-abc12", mgmtOpts{driver: driver, annotation: annotation, configVer: "v1.31.4+" + driver + "r1", running: "v1.31.4+" + driver + "r1"})
	}
	rows := []strategyRefusal{
		{name: "local provisioned", cluster: provCluster("local", "v1.31.4+rke2r1", map[string]any{}, "local"), code: clierr.CodeValidation, text: "this CLI does not upgrade the local cluster"},
		{name: "local imported", cluster: provCluster("local", "", nil, "local"), mgmt: mgmt("local", mgmtOpts{driver: "k3s"}), code: clierr.CodeValidation, text: "this CLI does not upgrade the local cluster"},
		{name: "not linked", cluster: provCluster("edge", "", nil, ""), code: clierr.CodeGeneric, retryable: true,
			text: "Rancher has not linked cluster edge to its management cluster yet; try again later"},
		{name: "no driver", cluster: linked(), mgmt: mgmt("c-m-abc12", mgmtOpts{}), code: clierr.CodeGeneric, retryable: true,
			text: "Rancher has not detected the distribution of cluster edge yet; try again later"},
		{name: "annotation another value rke2", cluster: linked(), mgmt: ready("rke2", new("maybe")), code: clierr.CodeValidation, hint: true,
			text: "the annotation rancher.io/imported-cluster-version-management of cluster edge is maybe; Rancher accepts only true, false or system-default"},
		{name: "annotation another value k3s", cluster: linked(), mgmt: ready("k3s", new("maybe")), code: clierr.CodeValidation, hint: true,
			text: "the annotation rancher.io/imported-cluster-version-management of cluster edge is maybe; Rancher accepts only true, false or system-default"},
	}
	for _, driver := range []string{"rke2", "k3s"} {
		rows = append(rows,
			strategyRefusal{name: "no config " + driver, cluster: linked(), mgmt: mgmt("c-m-abc12", mgmtOpts{driver: driver, annotation: new("true"), noConfig: true, running: "v1.31.4+" + driver + "r1"}), code: clierr.CodeGeneric, retryable: true,
				text: "cluster edge has not reported its version yet; try again later"},
			strategyRefusal{name: "empty config version " + driver, cluster: linked(), mgmt: mgmt("c-m-abc12", mgmtOpts{driver: driver, annotation: new("true"), running: "v1.31.4+" + driver + "r1"}), code: clierr.CodeGeneric, retryable: true,
				text: "cluster edge has not reported its version yet; try again later"},
			strategyRefusal{name: "no status version " + driver, cluster: linked(), mgmt: mgmt("c-m-abc12", mgmtOpts{driver: driver, annotation: new("true"), configVer: "v1.31.4+" + driver + "r1", noVersion: true}), code: clierr.CodeGeneric, retryable: true,
				text: "cluster edge has not reported its version yet; try again later"},
		)
	}
	for _, driver := range []string{"imported", "k3os", "rancherd", "custom"} {
		rows = append(rows, strategyRefusal{name: "driver " + driver, cluster: linked(), mgmt: mgmt("c-m-abc12", mgmtOpts{driver: driver}), code: clierr.CodeValidation,
			text: "Rancher manages the Kubernetes version only of RKE2 and K3s clusters; cluster edge has driver " + driver})
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := CheckStrategyCluster(tc.cluster, tc.mgmt)
			require.NotNil(t, err)
			assert.Equal(t, tc.text, err.Message)
			assert.Equal(t, tc.code, err.Code)
			assert.Equal(t, tc.retryable, err.Retryable)
			assert.Equal(t, tc.hint, err.Hint != "")
		})
	}
}

// TestCheckStrategyClusterHosted asserts every hosted config field and every
// hosted driver is refused, each alone, with the same text as the version
// upgrade command.
func TestCheckStrategyClusterHosted(t *testing.T) {
	t.Parallel()
	rows := map[string]string{"eksConfig": "EKS", "aksConfig": "AKS", "gkeConfig": "GKE", "aliConfig": "Alibaba"}
	for field, driver := range rows {
		for name, opts := range map[string]mgmtOpts{"config": {driver: "imported", hosted: field}, "driver": {driver: driver}} {
			cluster, m := imported(opts)
			_, err := CheckStrategyCluster(cluster, m)
			require.NotNil(t, err, field+" "+name)
			assert.Equal(t, clierr.CodeValidation, err.Code)
			assert.Equal(t, "cluster edge is a hosted "+driver+" cluster; its Kubernetes version is set in spec."+field+", which this command does not change yet", err.Message, field+" "+name)
			assert.NotEmpty(t, err.Hint)
		}
	}
	cluster, m := imported(mgmtOpts{driver: "imported"})
	m["spec"].(map[string]any)["eksConfig"] = nil
	_, err := CheckStrategyCluster(cluster, m)
	require.NotNil(t, err)
	assert.NotContains(t, err.Message, "hosted", "a null config is not a hosted cluster")
}

// TestCheckStrategyClusterAcceptsEveryStateOfVersionManagement asserts an
// imported cluster of either distribution is accepted for each annotation value
// Rancher accepts, with the pause annotation set too: the command writes with a
// warning where version management is off. The annotation is added only when
// it is absent or empty.
func TestCheckStrategyClusterAcceptsEveryStateOfVersionManagement(t *testing.T) {
	t.Parallel()
	for _, driver := range []string{"rke2", "k3s"} {
		for _, annotation := range []*string{nil, new(""), new("true"), new("false"), new("system-default")} {
			for _, paused := range []string{"", "true"} {
				cluster, m := imported(mgmtOpts{driver: driver, annotation: annotation, paused: paused, configVer: "v1.31.4+" + driver + "r1", running: "v1.31.4+" + driver + "r1"})
				c, err := CheckStrategyCluster(cluster, m)
				text := "absent"
				if annotation != nil {
					text = *annotation
				}
				require.Nil(t, err, "%s annotation %q paused %q", driver, text, paused)
				assert.Equal(t, StrategyCluster{Name: "edge", ID: "c-m-abc12", Distro: driver, AddAnnotation: text == "absent" || text == ""}, c, "%s annotation %q", driver, text)
			}
		}
	}
}

// TestCheckStrategyClusterIgnoresTheRunningVersion asserts an imported cluster
// whose running version is a pre-release, or lower than its config version, is
// accepted: the strategy compares no version.
func TestCheckStrategyClusterIgnoresTheRunningVersion(t *testing.T) {
	t.Parallel()
	for _, running := range []string{"v1.32.0-rc1+rke2r1", "v1.29.0+rke2r1", "latest"} {
		cluster, m := imported(mgmtOpts{driver: "rke2", annotation: new("true"), configVer: "v1.31.4+rke2r1", running: running})
		_, err := CheckStrategyCluster(cluster, m)
		assert.Nil(t, err, running)
	}
}

// TestVersionManagementOf asserts the state for each annotation value and each
// state of the Setting read: the Setting counts only when the annotation leaves
// the decision to it, and an unreadable Setting is unknown, never on.
func TestVersionManagementOf(t *testing.T) {
	t.Parallel()
	settings := map[string]SettingRead{"true": {Value: "true", Read: true}, "false": {Value: "false", Read: true}, "unreadable": {Code: clierr.CodeRBAC}}
	rows := []struct {
		annotation *string
		want       map[string]VersionManagement
	}{
		{nil, map[string]VersionManagement{"true": VersionManagementEnabled, "false": VersionManagementDisabled, "unreadable": VersionManagementUnknown}},
		{new(""), map[string]VersionManagement{"true": VersionManagementEnabled, "false": VersionManagementDisabled, "unreadable": VersionManagementUnknown}},
		{new("system-default"), map[string]VersionManagement{"true": VersionManagementEnabled, "false": VersionManagementDisabled, "unreadable": VersionManagementUnknown}},
		{new("true"), map[string]VersionManagement{"true": VersionManagementEnabled, "false": VersionManagementEnabled, "unreadable": VersionManagementEnabled}},
		{new("false"), map[string]VersionManagement{"true": VersionManagementDisabled, "false": VersionManagementDisabled, "unreadable": VersionManagementDisabled}},
	}
	for _, row := range rows {
		text := "absent"
		if row.annotation != nil {
			text = *row.annotation
		}
		for name, setting := range settings {
			assert.Equal(t, row.want[name], VersionManagementOf(mgmt("c", mgmtOpts{annotation: row.annotation}), setting), "annotation %q setting %s", text, name)
		}
	}
	assert.Equal(t, VersionManagementUnknown, VersionManagementOf(mgmt("c", mgmtOpts{annotation: new("maybe")}), SettingRead{Value: "true", Read: true}), "a value Rancher refuses")
}
