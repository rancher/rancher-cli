package catalog

import (
	"maps"
	"slices"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// upgradeActionPath is the upgrade action of the fixture's ClusterRepo.
const upgradeActionPath = "/k8s/clusters/" + testCluster + "/v1/catalog.cattle.io.clusterrepos/" + testRepo + "?action=upgrade"

// TestUpgradeBodyPerFlag asserts each flag of app upgrade puts its value in
// the body field its row names, sent once after the re-reads, and that the
// row of the flag table names that field.
func TestUpgradeBodyPerFlag(t *testing.T) {
	t.Parallel()
	src := files{"v.yaml": "a: 1\n"}
	cases := []struct {
		flag  string
		edit  func(*appInput)
		field string
		chart bool
		want  any
	}{
		{"release", nil, "releaseName", true, testRelease},
		{flagNamespace(), nil, "namespace", false, testNS},
		{flagVersion, withFlag(flagVersion, "2.0.0"), "version", true, "2.0.0"},
		{flagValues, func(in *appInput) { in.strs[flagValues] = []string{"v.yaml"} }, "values", true, map[string]any{"a": float64(1), "global": testCattleValues()}},
		{flagResetValues, withBool(flagResetValues, true), "resetValues", true, true},
		{flagForce, withBool(flagForce, true), "force", false, true},
		{flagTakeOwnership, withBool(flagTakeOwnership, true), "takeOwnership", false, true},
		{flagCleanupOnFail, withBool(flagCleanupOnFail, true), "cleanupOnFail", false, true},
		{flagHistoryMax, withFlag(flagHistoryMax, "10"), "historyMax", false, float64(10)},
		{flagHelmWait, withBool(flagHelmWait, false), "wait", false, false},
		{flagHelmTimeout, withFlag(flagHelmTimeout, "90s"), "timeout", false, "1m30s"},
		{flagNoHooks, withBool(flagNoHooks, true), "noHooks", false, true},
		{flagDisableOpenAPIValidation, withBool(flagDisableOpenAPIValidation, true), "disableOpenAPIValidation", false, true},
		{flagDescription, withFlag(flagDescription, "second"), "description", true, "second"},
	}
	for _, tc := range cases {
		t.Run(tc.flag, func(t *testing.T) {
			t.Parallel()
			var edits []func(*appInput)
			if tc.edit != nil {
				edits = append(edits, tc.edit)
			}
			body := upgradeBodyOf(t, upgradeFake(t, nil), src, upgradeInput(edits...))
			got := body[tc.field]
			if tc.chart {
				got = upgradeChartOf(t, body)[tc.field]
			}
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.field, upgradeFlagFields[tc.flag].field, "the flag table names another field for --%s", tc.flag)
		})
	}
}

// TestUpgradeBodyDefaults asserts the body of an upgrade that types no option:
// Helm waits, with a 10m timeout, the chart entry carries the App's chart, the
// version it runs, the release, the values, the request id and the two source
// annotations, install is never sent, and no other field is.
func TestUpgradeBodyDefaults(t *testing.T) {
	t.Parallel()
	f := upgradeFake(t, nil)
	body := upgradeBodyOf(t, f, nil, upgradeInput())
	assert.Equal(t, upgradeActionPath, f.posts[0].path)
	assert.Equal(t, []string{"charts", "namespace", "timeout", "wait"}, slices.Sorted(maps.Keys(body)))
	assert.Equal(t, true, body["wait"])
	assert.Equal(t, "10m0s", body["timeout"])
	assert.Equal(t, testNS, body["namespace"])
	_, installed := body["install"]
	assert.False(t, installed, "Rancher sets install on every upgrade")
	chart := upgradeChartOf(t, body)
	assert.Equal(t, []string{"annotations", "chartName", "releaseName", "values", "version"}, slices.Sorted(maps.Keys(chart)))
	assert.Equal(t, testChart, chart["chartName"])
	assert.Equal(t, "1.0.0", chart["version"])
	assert.Equal(t, testRelease, chart["releaseName"])
	assert.Equal(t, map[string]any{
		annotationRequestID: testRequestID, annotationSourceRepoType: "cluster", annotationSourceRepo: testRepo,
	}, chart["annotations"])
	assert.Equal(t, map[string]any{"global": testCattleValues()}, chart["values"])
}

// TestUpgradeBodyTakesTheAppsNamespaceAndName asserts the body names the
// release and namespace the App holds in spec.name and spec.namespace, also
// when they differ from the App's object name and the typed namespace: the
// request never carries a name that would make Helm install another release.
func TestUpgradeBodyTakesTheAppsNamespaceAndName(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	a := f.putUpgradeApp(func(a *upgradeApp) {
		a.name, a.namespace, a.secret = "real-name", "real-ns", "sh.helm.release.v1.real-name.v3"
	})
	f.putRelease(a, releaseFixture{})
	eff, exec, err := planUpgrade(t, f, nil, upgradeInput())
	require.NoError(t, err)
	paths := diffPaths(eff)
	assert.Equal(t, "real-ns", paths["namespace"])
	assert.Equal(t, "real-name", paths["releaseName"])
	_, err = exec(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, f.postCount())
	body := f.posts[0].body
	assert.Equal(t, "real-ns", body["namespace"])
	assert.Equal(t, "real-name", upgradeChartOf(t, body)["releaseName"])
	assert.Contains(t, f.root.requests(), releaseSecretPathIn("real-ns", "sh.helm.release.v1.real-name.v3"))
}

// TestUpgradeVersionDefaultsToTheAppsVersion asserts an upgrade with no
// --version sends the chart version the App runs, and a typed version sends
// that one.
func TestUpgradeVersionDefaultsToTheAppsVersion(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		edit func(*appInput)
		want string
	}{
		"omitted":   {func(*appInput) {}, "1.0.0"},
		"newer":     {withFlag(flagVersion, "2.0.0"), "2.0.0"},
		"typed one": {withFlag(flagVersion, "1.0.0"), "1.0.0"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body := upgradeBodyOf(t, upgradeFake(t, nil), nil, upgradeInput(tc.edit))
			assert.Equal(t, tc.want, upgradeChartOf(t, body)["version"])
		})
	}
}

// TestUpgradeChartFlagSetsNoBodyField asserts --chart at the App's chart sends
// the same body as no --chart: the chart entry names the App's chart.
func TestUpgradeChartFlagSetsNoBodyField(t *testing.T) {
	t.Parallel()
	typed := upgradeBodyOf(t, upgradeFake(t, nil), nil, upgradeInput(withFlag(flagChart, testChart)))
	plain := upgradeBodyOf(t, upgradeFake(t, nil), nil, upgradeInput())
	assert.Equal(t, plain, typed)
	assert.Empty(t, upgradeFlagFields[flagChart].field)
}

// TestUpgradeForceWithTakeOwnershipRefused asserts the two flags together are
// VALIDATION before any server read.
func TestUpgradeForceWithTakeOwnershipRefused(t *testing.T) {
	t.Parallel()
	f := upgradeFake(t, nil)
	ce := upgradeRefusal(t, f, nil, upgradeInput(withBool(flagForce, true), withBool(flagTakeOwnership, true)))
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, 2, ce.ExitCode())
	assert.Contains(t, ce.Message, "Helm refuses --force-replace (sent for --force) together with --force-conflicts (sent for --take-ownership); give one of --force and --take-ownership")
	assert.Empty(t, f.reads)
	assert.Empty(t, f.root.requests())
}

// TestUpgradeHistoryMax asserts --history-max 0, 101 and a value that is not
// an integer are VALIDATION before any server read, 1 and 100 are sent as
// historyMax, and with the flag absent the body carries no historyMax.
func TestUpgradeHistoryMax(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"0", "101", "abc", "-1", "", "5.5"} {
		t.Run("refused "+raw, func(t *testing.T) {
			t.Parallel()
			f := upgradeFake(t, nil)
			ce := upgradeRefusal(t, f, nil, upgradeInput(withFlag(flagHistoryMax, raw)))
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, 2, ce.ExitCode())
			assert.Contains(t, ce.Message, "--history-max must be a whole number from 1 to 100; Rancher replaces 0 with 5, although Helm reads 0 as no limit")
			assert.Empty(t, f.reads)
			assert.Empty(t, f.root.requests())
		})
	}
	for raw, want := range map[string]float64{"1": 1, "100": 100} {
		t.Run("sent "+raw, func(t *testing.T) {
			t.Parallel()
			body := upgradeBodyOf(t, upgradeFake(t, nil), nil, upgradeInput(withFlag(flagHistoryMax, raw)))
			assert.Equal(t, want, body["historyMax"])
		})
	}
	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		body := upgradeBodyOf(t, upgradeFake(t, nil), nil, upgradeInput())
		assert.NotContains(t, body, "historyMax")
	})
}
