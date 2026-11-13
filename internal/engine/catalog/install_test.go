package catalog

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func diffPaths(eff safety.Effect) map[string]any {
	out := map[string]any{}
	for _, d := range eff.Diff {
		if d.To != nil {
			out[d.Path] = d.To
		} else {
			out[d.Path] = d.From
		}
	}
	return out
}

// TestInstallPreview asserts the Effect of an install: a create of the
// App previewed from server reads, with no precondition, the request's
// fields, the files' value paths hidden and the added cluster values shown
// in the diff, the warnings every install carries, and no request sent.
func TestInstallPreview(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	src := files{"v.yaml": "auth:\n  password: " + canary + "\n"}
	eff, _ := plan(t, newTestInstall(f, src), installInput(func(in *appInput) { in.strs[flagValues] = []string{"v.yaml"} }))

	assert.Equal(t, safety.ActionCreate, eff.Action)
	assert.Equal(t, "apps/"+testRelease, eff.Target)
	assert.Equal(t, "app", eff.DisplayKind)
	assert.Equal(t, testRelease, eff.DisplayName)
	assert.Equal(t, safety.PreviewServerReads, eff.PreviewSource)
	assert.Equal(t, testCluster, eff.Cluster)
	assert.Empty(t, eff.ResourceVersion)
	assert.False(t, eff.ConflictDetection)
	assert.False(t, eff.Reversible)
	assert.False(t, eff.Destructive)
	assert.Equal(t, map[string]any{
		"repo": testRepo, "chart": testChart, "version": "2.0.0", "namespace": testNS,
		"releaseName": testRelease, "wait": true, "timeout": "10m0s",
		"values.auth.password":                      "<hidden>",
		"values.global.cattle.clusterId":            testCluster,
		"values.global.cattle.clusterName":          "Production",
		"values.global.cattle.rkePathPrefix":        "",
		"values.global.cattle.rkeWindowsPathPrefix": "",
		"values.global.cattle.systemProjectId":      testSystemProject,
		"values.global.cattle.url":                  testServerURL,
	}, diffPaths(eff))
	assert.Equal(t, []string{
		"Rancher runs Helm in a pod as you; the chart's own writes are checked only when Helm runs them",
		"this preview comes from server reads; the server has no dry-run for this action",
		"uninstalling the release leaves the CRDs of the chart's crds/ directory and the resources marked helm.sh/resource-policy: keep",
		"rancher-cli adds the values the Rancher UI adds when it installs or upgrades a chart: values.global.cattle.clusterId, values.global.cattle.clusterName, " +
			"values.global.cattle.rkePathPrefix, values.global.cattle.rkeWindowsPathPrefix, values.global.cattle.systemProjectId, values.global.cattle.url",
		"Rancher may create, change or delete image pull secrets for this release; the preview cannot list them",
	}, eff.Warnings)
	assert.Zero(t, f.postCount())
}

// TestAppCommandsGatesAgree asserts each command's Plan sets
// eff.Destructive to what its Descriptor declares, which the schema's gates
// come from: install false, upgrade true, uninstall true.
func TestAppCommandsGatesAgree(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	f.put("apps", testNS, testRelease, map[string]any{"spec": map[string]any{"info": map[string]any{"status": "deployed"}}})
	cases := []struct {
		cmd runtime.MutatingCommand
		in  runtime.Input
	}{
		{newTestInstall(newAppFake(t), nil), installInput()},
		{newTestUpgrade(upgradeFake(t, nil), nil), upgradeInput()},
		{newTestUninstall(f), uninstallInput()},
	}
	for _, tc := range cases {
		eff, _ := plan(t, tc.cmd, tc.in)
		d := tc.cmd.Describe()
		assert.Equal(t, d.Destructive == "true", eff.Destructive, d.Name)
		assert.Contains(t, []string{"true", "false"}, d.Destructive, d.Name)
	}
}

// installBodyOf runs Plan and Execute and returns the one body the fake
// recorded.
func installBodyOf(t *testing.T, f *appFake, src engine.FileSource, in appInput) map[string]any {
	t.Helper()
	_, exec := plan(t, newTestInstall(f, src), in)
	_, err := exec(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, f.postCount())
	return f.posts[0].body
}

// bodyField is one flag's field in a request body and the action type of
// the Rancher API fields golden that holds it.
type bodyField struct {
	typ, field string
}

// installFlagFields names, for every flag and argument of app install, the
// body field it sets; the path flags set none.
var installFlagFields = map[string]bodyField{
	"release":                    {"ChartInstall", "releaseName"},
	engine.FlagCluster:           {},
	engine.FlagNamespace:         {"ChartInstallAction", "namespace"},
	flagRepo:                     {},
	flagChart:                    {"ChartInstall", "chartName"},
	flagVersion:                  {"ChartInstall", "version"},
	flagValues:                   {"ChartInstall", "values"},
	flagHelmWait:                 {"ChartInstallAction", "wait"},
	flagHelmTimeout:              {"ChartInstallAction", "timeout"},
	flagNoHooks:                  {"ChartInstallAction", "noHooks"},
	flagDisableOpenAPIValidation: {"ChartInstallAction", "disableOpenAPIValidation"},
	flagDescription:              {"ChartInstall", "description"},
	flagSystemDefaultRegistry:    {},
	flagNoSystemDefaultRegistry:  {},
	flagWait:                     {},
	flagTimeout:                  {},
}

// upgradeFlagFields is installFlagFields for app upgrade: the release, the
// chart name and the namespace of the body are the App's, and --chart sets no
// field.
var upgradeFlagFields = map[string]bodyField{
	"release":                    {"ChartUpgrade", "releaseName"},
	engine.FlagCluster:           {},
	engine.FlagNamespace:         {"ChartUpgradeAction", "namespace"},
	flagRepo:                     {},
	flagChart:                    {},
	flagVersion:                  {"ChartUpgrade", "version"},
	flagValues:                   {"ChartUpgrade", "values"},
	flagResetValues:              {"ChartUpgrade", "resetValues"},
	flagForce:                    {"ChartUpgradeAction", "force"},
	flagTakeOwnership:            {"ChartUpgradeAction", "takeOwnership"},
	flagCleanupOnFail:            {"ChartUpgradeAction", "cleanupOnFail"},
	flagHistoryMax:               {"ChartUpgradeAction", "historyMax"},
	flagHelmWait:                 {"ChartUpgradeAction", "wait"},
	flagHelmTimeout:              {"ChartUpgradeAction", "timeout"},
	flagNoHooks:                  {"ChartUpgradeAction", "noHooks"},
	flagDisableOpenAPIValidation: {"ChartUpgradeAction", "disableOpenAPIValidation"},
	flagDescription:              {"ChartUpgrade", "description"},
	flagSystemDefaultRegistry:    {},
	flagNoSystemDefaultRegistry:  {},
	flagWait:                     {},
	flagTimeout:                  {},
}

// uninstallFlagFields is installFlagFields for app uninstall.
var uninstallFlagFields = map[string]bodyField{
	"release":            {},
	engine.FlagCluster:   {},
	engine.FlagNamespace: {},
	flagKeepHistory:      {"ChartUninstallAction", "keepHistory"},
	flagNoHooks:          {"ChartUninstallAction", "noHooks"},
	flagHelmTimeout:      {"ChartUninstallAction", "timeout"},
	flagWait:             {},
	flagTimeout:          {},
}

// TestEveryAppFlagNamesABodyFieldEveryEditionHas asserts the flag tables
// list exactly the flags and arguments of the three commands, and every body
// field they name is a field of its action type in every edition of the
// Rancher API fields golden: each flag's oldest Rancher minor is the oldest
// the golden records.
func TestEveryAppFlagNamesABodyFieldEveryEditionHas(t *testing.T) {
	t.Parallel()
	golden := goldenActionFields(t)
	for _, tc := range []struct {
		d      runtime.Descriptor
		fields map[string]bodyField
	}{
		{(&installCmd{}).Describe(), installFlagFields},
		{(&upgradeCmd{}).Describe(), upgradeFlagFields},
		{(&uninstallCmd{}).Describe(), uninstallFlagFields},
	} {
		var names []string
		for _, a := range append(append([]runtime.ArgSpec{}, tc.d.Args...), tc.d.Flags...) {
			names = append(names, a.Name)
			_, ok := tc.fields[a.Name]
			assert.True(t, ok, "%s: --%s has no row in the flag table", tc.d.Name, a.Name)
		}
		for name, bf := range tc.fields {
			assert.Contains(t, names, name, "%s: the flag table lists %s, which the command does not take", tc.d.Name, name)
			if bf.field == "" {
				continue
			}
			assert.ElementsMatch(t, []string{"community", "prime"}, golden[bf.typ][bf.field],
				"%s: --%s sends %s.%s, which is not a field in every edition", tc.d.Name, name, bf.typ, bf.field)
		}
	}
}

// TestInstallBodyPerFlag asserts each flag of app install puts its value in
// the body field its row names, sent once after the re-reads.
func TestInstallBodyPerFlag(t *testing.T) {
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
		{flagChart, nil, "chartName", true, testChart},
		{flagNamespace(), nil, "namespace", false, testNS},
		{flagVersion, func(in *appInput) { in.s[flagVersion] = "1.0.0" }, "version", true, "1.0.0"},
		{flagValues, func(in *appInput) { in.strs[flagValues] = []string{"v.yaml"} }, "values", true, map[string]any{"a": float64(1), "global": testCattleValues()}},
		{flagHelmWait, func(in *appInput) { in.b[flagHelmWait] = false }, "wait", false, false},
		{flagHelmTimeout, func(in *appInput) { in.s[flagHelmTimeout] = "90s" }, "timeout", false, "1m30s"},
		{flagNoHooks, func(in *appInput) { in.b[flagNoHooks] = true }, "noHooks", false, true},
		{flagDisableOpenAPIValidation, func(in *appInput) { in.b[flagDisableOpenAPIValidation] = true }, "disableOpenAPIValidation", false, true},
		{flagDescription, func(in *appInput) { in.s[flagDescription] = "first" }, "description", true, "first"},
	}
	for _, tc := range cases {
		t.Run(tc.flag, func(t *testing.T) {
			t.Parallel()
			var edits []func(*appInput)
			if tc.edit != nil {
				edits = append(edits, tc.edit)
			}
			body := installBodyOf(t, newAppFake(t), src, installInput(edits...))
			got := body[tc.field]
			if tc.chart {
				got = body["charts"].([]any)[0].(map[string]any)[tc.field]
			}
			assert.Equal(t, tc.want, got)
			row := installFlagFields[tc.flag]
			assert.Equal(t, tc.field, row.field, "the flag table names another field for --%s", tc.flag)
		})
	}
}

func flagNamespace() string { return engine.FlagNamespace }

// testCattleValues is the global value app install adds for the fake's
// cluster when the files set nothing under it and the chart declares no
// registry path.
func testCattleValues() map[string]any {
	return map[string]any{"cattle": map[string]any{
		"clusterId": testCluster, "clusterName": "Production", "systemProjectId": testSystemProject,
		"url": testServerURL, "rkePathPrefix": "", "rkeWindowsPathPrefix": "",
	}}
}

// TestInstallBodyDefaults asserts the body of a bare install: Helm waits,
// with a 10m timeout, the chart entry carries the request id, the two
// source annotations and the cluster values, and no other field is sent.
func TestInstallBodyDefaults(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	body := installBodyOf(t, f, nil, installInput())
	assert.Equal(t, "/k8s/clusters/c-1/v1/catalog.cattle.io.clusterrepos/repo1?action=install", f.posts[0].path)
	assert.Equal(t, map[string]any{
		"timeout": "10m0s", "wait": true, "namespace": testNS,
		"charts": []any{map[string]any{
			"chartName": testChart, "version": "2.0.0", "releaseName": testRelease,
			"values": map[string]any{"global": testCattleValues()},
			"annotations": map[string]any{
				"cli.rancher.io/request-id":             testRequestID,
				"catalog.cattle.io/ui-source-repo-type": "cluster",
				"catalog.cattle.io/ui-source-repo":      testRepo,
			},
		}},
	}, body)
}

// TestInstallHelmWait asserts --helm-wait: unset and given alone send
// true, --helm-wait=false sends false.
func TestInstallHelmWait(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		edit func(*appInput)
		want bool
	}{
		"unset":             {func(*appInput) {}, true},
		"--helm-wait":       {func(in *appInput) { in.b[flagHelmWait] = true }, true},
		"--helm-wait=false": {func(in *appInput) { in.b[flagHelmWait] = false }, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, installBodyOf(t, newAppFake(t), nil, installInput(tc.edit))["wait"])
		})
	}
}

// TestInstallRefusedBeforeAnyRead asserts the refusals that need no server
// read: an invalid release name, a --helm-timeout out of bounds and both
// registry flags together, each VALIDATION with no request sent.
func TestInstallRefusedBeforeAnyRead(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		edit func(*appInput)
		want string
	}{
		"54 characters":     {func(in *appInput) { in.args = []string{strings.Repeat("a", 54)} }, "is not valid: Helm takes at most 53 characters"},
		"upper case":        {func(in *appInput) { in.args = []string{"MyApp"} }, "is not valid"},
		"ends with a dash":  {func(in *appInput) { in.args = []string{"app-"} }, "is not valid"},
		"empty part":        {func(in *appInput) { in.args = []string{"a..b"} }, "is not valid"},
		"timeout zero":      {func(in *appInput) { in.s[flagHelmTimeout] = "0s" }, "--helm-timeout must be at least 1s and at most 15m"},
		"timeout negative":  {func(in *appInput) { in.s[flagHelmTimeout] = "-1m" }, "--helm-timeout must be at least 1s and at most 15m"},
		"timeout below 1s":  {func(in *appInput) { in.s[flagHelmTimeout] = "500ms" }, "--helm-timeout must be at least 1s and at most 15m"},
		"timeout over 15m":  {func(in *appInput) { in.s[flagHelmTimeout] = "16m" }, "Rancher removes a run's access one hour after the operation starts"},
		"timeout not Go":    {func(in *appInput) { in.s[flagHelmTimeout] = "ten" }, "is not a duration such as 90s or 10m"},
		"stdin is terminal": {func(in *appInput) { in.strs[flagValues] = []string{"-"}; in.terminal = true }, "stdin is a terminal"},
		"both registry flags": {func(in *appInput) {
			in.s[flagSystemDefaultRegistry] = "r.example.test"
			in.b[flagNoSystemDefaultRegistry] = true
		}, "--system-default-registry and --no-system-default-registry are given together; give at most one of them"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newAppFake(t)
			_, _, err := newTestInstall(f, nil).Plan(t.Context(), installInput(tc.edit))
			require.ErrorContains(t, err, tc.want)
			assert.Equal(t, clierr.CodeValidation, clierr.From(err).Code)
			assert.Empty(t, f.reads)
			assert.Empty(t, f.root.requests())
		})
	}
	for _, ok := range []string{strings.Repeat("a", 53), "a.b-c.d1", "1abc"} {
		_, _, err := newTestInstall(newAppFake(t), nil).Plan(t.Context(), installInput(func(in *appInput) { in.args = []string{ok} }))
		assert.NoError(t, err, ok)
	}
}

// TestInstallRepository asserts a missing ClusterRepo is NOT_FOUND naming
// the command that lists them, and a disabled one is VALIDATION naming the
// command that shows it.
func TestInstallRepository(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	f.remove("clusterrepos", "", testRepo)
	_, _, err := newTestInstall(f, nil).Plan(t.Context(), installInput())
	ce := clierr.From(err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, clierr.Rancher+" get clusterrepos --cluster 'prod'", ce.Hint)

	f = newAppFake(t)
	f.put("clusterrepos", "", testRepo, map[string]any{"spec": map[string]any{"enabled": false}})
	_, _, err = newTestInstall(f, nil).Plan(t.Context(), installInput())
	ce = clierr.From(err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "ClusterRepo 'repo1' is disabled", ce.Message)
	assert.Equal(t, clierr.Rancher+" get clusterrepo 'repo1' --cluster 'prod'", ce.Hint)
}

// TestInstallVersion asserts the version rules: an exact --version from
// the index, deprecated included; omitted, the first version that is
// neither a prerelease nor deprecated; every version a prerelease, the
// first, with a warning; a version not in the index, a range included, and
// every version deprecated are VALIDATION listing the index's versions.
func TestInstallVersion(t *testing.T) {
	t.Parallel()
	dep := func(v string) ChartVersion { return ChartVersion{Version: v, Deprecated: true} }
	cases := []struct {
		name     string
		versions []ChartVersion
		want     string
		version  string
		err      string
		hint     string
		warning  string
	}{
		{name: "default skips prerelease and deprecated", versions: []ChartVersion{{Version: "3.0.0-rc1"}, dep("2.1.0"), {Version: "2.0.0"}}, want: "2.0.0"},
		{name: "exact", versions: []ChartVersion{{Version: "2.0.0"}, {Version: "1.0.0"}}, want: "1.0.0", version: "1.0.0"},
		{name: "exact deprecated", versions: []ChartVersion{{Version: "2.0.0"}, dep("1.0.0")}, want: "1.0.0", version: "1.0.0"},
		{name: "every version a prerelease", versions: []ChartVersion{{Version: "2.0.0-rc2"}, {Version: "2.0.0-rc1"}}, want: "2.0.0-rc2",
			warning: "2.0.0-rc2 is a prerelease; every version of nginx that is not deprecated is one"},
		{name: "not in the index", versions: []ChartVersion{{Version: "2.0.0"}, {Version: "1.0.0"}}, version: "1.5.0",
			err:  "version '1.5.0' of chart 'nginx' is not in the index ClusterRepo 'repo1' serves for this cluster; --version names one exact version; the index has 2.0.0, 1.0.0",
			hint: clierr.Rancher + " charts --cluster 'prod' --repo 'repo1' --all-versions"},
		{name: "a range", versions: []ChartVersion{{Version: "2.0.0"}}, version: "^2.0.0", err: "is not in the index"},
		{name: "every version deprecated", versions: []ChartVersion{dep("2.0.0"), dep("1.0.0")},
			err:  "every version of 'nginx' is deprecated; name one with --version; the index has 2.0.0, 1.0.0",
			hint: clierr.Rancher + " charts --cluster 'prod' --repo 'repo1' --all-versions --include-deprecated"},
		{name: "no chart", versions: nil, err: "chart 'nginx' is not in the index ClusterRepo 'repo1' serves for this cluster",
			hint: clierr.Rancher + " charts --cluster 'prod' --repo 'repo1'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAppFake(t)
			entries := map[string][]ChartVersion{}
			if tc.versions != nil {
				entries[testChart] = tc.versions
			}
			f.root.answers[indexPath()] = rootAnswer{status: 200, body: indexJSON(entries)}
			for _, v := range tc.versions {
				f.root.answers[infoPath(testChart, strings.ReplaceAll(v.Version, "+", "%2B"))] = rootAnswer{status: 200, body: `{"values":{},"chart":{}}`}
			}
			in := installInput(func(in *appInput) {
				if tc.version != "" {
					in.s[flagVersion] = tc.version
				}
			})
			eff, _, err := newTestInstall(f, nil).Plan(t.Context(), in)
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				ce := clierr.From(err)
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				if tc.hint != "" {
					assert.Equal(t, tc.hint, ce.Hint)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, diffPaths(eff)["version"])
			if tc.warning != "" {
				assert.Contains(t, eff.Warnings, tc.warning)
			}
		})
	}
}

// chartWith sets the default values and annotations the info link answers
// for nginx 2.0.0.
func chartWith(f *appFake, info string) {
	f.root.answers[infoPath(testChart, "2.0.0")] = rootAnswer{status: 200, body: info}
}

// TestInstallChartRefusals asserts the refusals the chart's default values
// and annotations call for, each VALIDATION naming the chart and version.
func TestInstallChartRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, info, want string
		setup            func(*appFake)
	}{
		{name: "auto-install", info: `{"chart":{"annotations":{"catalog.cattle.io/auto-install":" nginx-crd=match , other=1.0 "}}}`,
			want: "chart 'nginx' '2.0.0' names nginx-crd, other to install first (annotation catalog.cattle.io/auto-install)"},
		{name: "auto-install-gvr", info: `{"chart":{"annotations":{"catalog.cattle.io/auto-install-gvr":"example.io/v1/Thing"}}}`,
			want: "chart 'nginx' '2.0.0' names example.io/v1/Thing to install first (annotation catalog.cattle.io/auto-install-gvr)"},
		{name: "fixed namespace", info: `{"chart":{"annotations":{"catalog.cattle.io/namespace":"cattle-system"}}}`,
			want: "chart 'nginx' '2.0.0' installs only in namespace 'cattle-system' (annotation catalog.cattle.io/namespace); run it with --namespace 'cattle-system'"},
		{name: "fixed release name", info: `{"chart":{"annotations":{"catalog.cattle.io/release-name":"nginx-system"}}}`,
			want: "chart 'nginx' '2.0.0' installs only as release 'nginx-system'"},
		{name: "requires a resource the cluster does not serve", info: `{"chart":{"annotations":{"catalog.cattle.io/requires-gvr":"monitoring.coreos.com.prometheus"}}}`,
			want: "chart 'nginx' '2.0.0' needs the resource monitoring.coreos.com.prometheus on the cluster (annotation catalog.cattle.io/requires-gvr), and the cluster does not serve it"},
		{name: "requires another version", info: `{"chart":{"annotations":{"catalog.cattle.io/requires-gvr":"monitoring.coreos.com.prometheus/v2"}}}`,
			want: "needs the resource monitoring.coreos.com.prometheus/v2",
			setup: func(f *appFake) {
				f.resources = []engine.APIResource{{Group: "monitoring.coreos.com", Version: "v1", Name: "prometheuses", Kind: "Prometheus"}}
			}},
		{name: "windows workers for a linux chart", info: `{"chart":{"annotations":{"catalog.cattle.io/permits-os":"linux"}}}`,
			want: "chart 'nginx' '2.0.0' permits only linux nodes (annotation catalog.cattle.io/permits-os), and cluster 'prod' has windows workers",
			setup: func(f *appFake) {
				f.root.answers["/v1/management.cattle.io.clusters/"+testCluster] = rootAnswer{status: 200, body: `{"status":{"windowsWorkerCount":1,"linuxWorkerCount":1}}`}
			}},
		{name: "a rancher repository without permits-os on a windows provider", info: `{"chart":{}}`,
			want: "permits only linux nodes",
			setup: func(f *appFake) {
				f.put("clusterrepos", "", testRepo, map[string]any{"spec": map[string]any{"gitRepo": "https://git.rancher.io/charts"}})
				f.root.answers["/v1/management.cattle.io.clusters/"+testCluster] = rootAnswer{status: 200, body: `{"status":{"provider":"rke.windows"}}`}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAppFake(t)
			chartWith(f, tc.info)
			if tc.setup != nil {
				tc.setup(f)
			}
			_, _, err := newTestInstall(f, nil).Plan(t.Context(), installInput())
			require.ErrorContains(t, err, tc.want)
			ce := clierr.From(err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Empty(t, ce.Hint)
			assert.Zero(t, f.postCount())
		})
	}
}

// TestInstallChartChecksThatPass asserts the annotation checks pass when
// the target meets them, a hidden chart included, and a check whose read
// is denied gives a warning instead.
func TestInstallChartChecksThatPass(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, info, warning string
		setup               func(*appFake)
	}{
		{name: "matching namespace and release name", info: `{"chart":{"annotations":{"catalog.cattle.io/namespace":"web","catalog.cattle.io/release-name":"my-nginx"}}}`},
		{name: "hidden", info: `{"chart":{"annotations":{"catalog.cattle.io/hidden":"true"}}}`},
		{name: "requires a served resource at its preferred version", info: `{"chart":{"annotations":{"catalog.cattle.io/requires-gvr":"monitoring.coreos.com.prometheus/v1, configmap"}}}`,
			setup: func(f *appFake) {
				f.resources = []engine.APIResource{
					{Group: "monitoring.coreos.com", Version: "v1", Name: "prometheuses", Kind: "Prometheus"},
					{Version: "v1", Name: "configmaps", Kind: "ConfigMap"},
				}
			}},
		{name: "discovery could not list the group", info: `{"chart":{"annotations":{"catalog.cattle.io/requires-gvr":"monitoring.coreos.com.prometheus"}}}`,
			warning: "the cluster could not list every API group; chart 'nginx' '2.0.0' needs monitoring.coreos.com.prometheus (annotation catalog.cattle.io/requires-gvr), which was not checked",
			setup: func(f *appFake) {
				f.discoveryErr = &engine.DiscoveryIncomplete{Failed: map[schema.GroupVersion]error{{Group: "monitoring.coreos.com", Version: "v1"}: errors.New("x")}}
			}},
		{name: "linux workers for a linux chart", info: `{"chart":{"annotations":{"catalog.cattle.io/permits-os":"linux,windows"}}}`},
		{name: "management cluster not readable", info: `{"chart":{"annotations":{"catalog.cattle.io/permits-os":"linux"}}}`,
			warning: "cannot read cluster 'prod'; chart 'nginx' '2.0.0' permits only linux nodes (annotation catalog.cattle.io/permits-os), which was not checked against its workers",
			setup: func(f *appFake) {
				f.root.answers["/v1/management.cattle.io.clusters/"+testCluster] = rootAnswer{status: http.StatusForbidden, body: `{"message":"no"}`}
			}},
		{name: "another repository with windows workers", info: `{"chart":{}}`,
			setup: func(f *appFake) {
				f.root.answers["/v1/management.cattle.io.clusters/"+testCluster] = rootAnswer{status: 200, body: `{"status":{"windowsWorkerCount":3}}`}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAppFake(t)
			chartWith(f, tc.info)
			if tc.setup != nil {
				tc.setup(f)
			}
			eff, _, err := newTestInstall(f, nil).Plan(t.Context(), installInput())
			require.NoError(t, err)
			if tc.warning != "" {
				assert.Contains(t, eff.Warnings, tc.warning)
			}
		})
	}
}

// TestInstallExistingApp asserts an App of the release name is CONFLICT:
// it names the command that shows it, a failed one also the uninstall
// preview, and an uninstalled one with kept history asks for another name.
func TestInstallExistingApp(t *testing.T) {
	t.Parallel()
	get := clierr.Rancher + " get app 'my-nginx' --cluster 'prod' --namespace 'web'"
	cases := map[string]struct{ status, msg, hint string }{
		"deployed": {"deployed", "app 'my-nginx' already exists in namespace 'web' (status deployed)", get},
		"failed": {"failed", "app 'my-nginx' already exists in namespace 'web' (status failed); to remove the failed release, add --apply --yes after reading the preview of " +
			clierr.Rancher + " app uninstall 'my-nginx' --cluster 'prod' --namespace 'web' --dry-run", get},
		"uninstalled": {"uninstalled", "is uninstalled and Helm keeps its history; Helm does not install a release name with kept history; choose another release name", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newAppFake(t)
			f.put("apps", testNS, testRelease, map[string]any{"spec": map[string]any{"info": map[string]any{"status": tc.status}}})
			_, _, err := newTestInstall(f, nil).Plan(t.Context(), installInput())
			require.ErrorContains(t, err, tc.msg)
			ce := clierr.From(err)
			assert.Equal(t, clierr.CodeConflict, ce.Code)
			assert.Equal(t, tc.hint, ce.Hint)
		})
	}
}

// TestRunningOperationRule asserts which Operations of the release refuse
// an install and an uninstall: one with no conditions, one whose
// observedGeneration is behind, one with Reconciling=True; not one that
// finished, one older than an hour, or one of another release.
func TestRunningOperationRule(t *testing.T) {
	t.Parallel()
	recent := testNow.Add(-10 * time.Minute)
	cases := map[string]struct {
		op     map[string]any
		refuse bool
	}{
		"no conditions":        {operation(testRelease, recent, "", 1, 1), true},
		"observedGeneration 0": {operation(testRelease, recent, "False", 1, 0), true},
		"reconciling":          {operation(testRelease, recent, "True", 1, 1), true},
		"finished":             {operation(testRelease, recent, "False", 1, 1), false},
		"older than an hour":   {operation(testRelease, testNow.Add(-61*time.Minute), "True", 1, 1), false},
		"another release":      {operation("other", recent, "True", 1, 1), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newAppFake(t)
			f.put("operations", testNS, "helm-operation-x1", tc.op)
			_, _, err := newTestInstall(f, nil).Plan(t.Context(), installInput())
			f.put("apps", testNS, testRelease, map[string]any{"spec": map[string]any{"info": map[string]any{"status": "deployed"}}})
			_, _, uerr := newTestUninstall(f).Plan(t.Context(), uninstallInput())
			if !tc.refuse {
				require.NoError(t, err)
				require.NoError(t, uerr)
				return
			}
			for _, e := range []error{err, uerr} {
				require.ErrorContains(t, e, "operation 'helm-operation-x1' for release 'my-nginx' in namespace 'web' may still be running")
				ce := clierr.From(e)
				assert.Equal(t, clierr.CodeConflict, ce.Code)
				assert.Equal(t, clierr.Rancher+" get operation 'helm-operation-x1' --cluster 'prod' --namespace 'web'", ce.Hint)
			}
		})
	}
}

// TestInstallOperationsNotListable asserts a caller who cannot list
// operations gets a warning that the check was not made.
func TestInstallOperationsNotListable(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	f.fail("list:operations/"+testNS, clierr.New(clierr.CodeRBAC, "forbidden"))
	eff, _ := plan(t, newTestInstall(f, nil), installInput())
	assert.Contains(t, eff.Warnings, "you cannot list operations in namespace 'web'; a running operation for release 'my-nginx' was not checked")
}

// TestInstallMissingNamespace asserts a missing namespace is NOT_FOUND
// with the two create namespace previews, the second naming the project.
func TestInstallMissingNamespace(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	f.remove("namespaces", "", testNS)
	_, _, err := newTestInstall(f, nil).Plan(t.Context(), installInput())
	ce := clierr.From(err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "namespace 'web' not found on cluster 'prod'; create it first; a project member creates it in the project (<project> is its name) with "+
		clierr.Rancher+" create namespace 'web' --cluster 'prod' --project <project> --dry-run", ce.Message)
	assert.Equal(t, clierr.Rancher+" create namespace 'web' --cluster 'prod' --dry-run", ce.Hint)
}

// TestInstallIdentity asserts who the preview says Helm runs as: the
// caller, or the ClusterRepo's service account when it sets both fields
// and the namespace holds no ":"; with a service account the follow-up
// names the operation only when the caller may get it.
func TestInstallIdentity(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		spec     map[string]any
		allowed  bool
		identity string
		followOp bool
	}{
		"caller":                 {map[string]any{}, false, "you", true},
		"service account":        {map[string]any{"serviceAccount": "helm", "serviceAccountNamespace": "tools"}, true, "system:serviceaccount:tools:helm", true},
		"service account denied": {map[string]any{"serviceAccount": "helm", "serviceAccountNamespace": "tools"}, false, "system:serviceaccount:tools:helm", false},
		"no namespace":           {map[string]any{"serviceAccount": "helm"}, false, "you", true},
		"colon in namespace":     {map[string]any{"serviceAccount": "helm", "serviceAccountNamespace": "a:b"}, false, "you", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newAppFake(t)
			f.allowed = tc.allowed
			f.put("clusterrepos", "", testRepo, map[string]any{"spec": tc.spec})
			eff, exec := plan(t, newTestInstall(f, nil), installInput())
			assert.Equal(t, identityWarning(tc.identity), eff.Warnings[0])
			res, err := exec(t.Context())
			require.NoError(t, err)
			follow := res.Data.(result).FollowUp
			assert.Equal(t, tc.followOp, len(follow) == 2, "follow-ups %v", follow)
		})
	}
}

// TestInstallResult asserts the result of an accepted request: the
// operation, the request id, the follow-up commands and the warning that
// nothing checks for a change after the request.
func TestInstallResult(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	_, exec := plan(t, newTestInstall(f, nil), installInput())
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, result{
		Release: testRelease, Namespace: testNS,
		OperationName: "helm-operation-abcde", OperationNamespace: "web", RequestID: testRequestID,
		FollowUp: []string{
			clierr.Rancher + " get operation 'helm-operation-abcde' --cluster 'prod' --namespace 'web' --wait",
			clierr.Rancher + " get app 'my-nginx' --cluster 'prod' --namespace 'web'",
		},
	}, res.Data)
	assert.Contains(t, res.Warnings, warnNoWatch)
	assert.Contains(t, res.Text, "operation helm-operation-abcde in namespace web")
}

// TestInstallExecuteRereads asserts Execute reads the App, the operations
// and the index entry again and sends nothing when one changed: each
// change is a retryable CONFLICT; a digest that is empty on either side is
// sent with a warning.
func TestInstallExecuteRereads(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		change  func(*appFake)
		err     string
		warning string
	}{
		{name: "app created", change: func(f *appFake) {
			f.put("apps", testNS, testRelease, map[string]any{})
		}, err: "app 'my-nginx' was created since the preview; run the command again to see a new preview"},
		{name: "operation started", change: func(f *appFake) {
			f.put("operations", testNS, "helm-operation-z", operation(testRelease, testNow, "", 1, 0))
		}, err: "operation 'helm-operation-z' for release 'my-nginx' started since the preview"},
		{name: "digest changed", change: func(f *appFake) {
			f.root.answers[indexPath()] = rootAnswer{status: 200, body: indexJSON(map[string][]ChartVersion{testChart: {{Version: "2.0.0", Digest: "sha256:new"}}})}
		}, err: "the content of chart 'nginx' '2.0.0' changed in the index since the preview"},
		{name: "version gone", change: func(f *appFake) {
			f.root.answers[indexPath()] = rootAnswer{status: 200, body: indexJSON(map[string][]ChartVersion{testChart: {{Version: "1.0.0"}}})}
		}, err: "version '2.0.0' of chart 'nginx' left the index since the preview"},
		{name: "digest empty now", change: func(f *appFake) {
			f.root.answers[indexPath()] = rootAnswer{status: 200, body: indexJSON(map[string][]ChartVersion{testChart: {{Version: "2.0.0"}}})}
		}, warning: "the index gives no digest for chart nginx 2.0.0; its content was not compared with the preview's"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAppFake(t)
			_, exec := plan(t, newTestInstall(f, nil), installInput())
			tc.change(f)
			res, err := exec(t.Context())
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				ce := clierr.From(err)
				assert.Equal(t, clierr.CodeConflict, ce.Code)
				assert.True(t, ce.Retryable)
				assert.Zero(t, f.postCount())
				return
			}
			require.NoError(t, err)
			assert.Contains(t, res.Warnings, tc.warning)
			assert.Equal(t, 1, f.postCount())
		})
	}
}

// TestActionAnswersSendOnePost asserts, for app install and app upgrade, each
// answer of the action and each transport failure after the request was
// written gives exactly one POST:
// 429 and 503 keep the status table's code, 500, 502, 504 and a failure after
// the write are GENERIC, not retryable, carry the request id and name the
// reads to make before a rerun.
func TestActionAnswersSendOnePost(t *testing.T) {
	t.Parallel()
	unknown := func(status int, cause error) error {
		if status != 0 {
			cause = clierr.ResponseError(http.MethodPost, status, nil)
		}
		return &engine.ActionOutcomeUnknown{Err: cause}
	}
	cases := map[string]struct {
		err       error
		code      string
		retryable bool
		unknown   bool
	}{
		"429":       {clierr.FromResponse("POST", http.MethodPost, http.StatusTooManyRequests, nil, nil), clierr.CodeGeneric, true, false},
		"503":       {clierr.FromResponse("POST", http.MethodPost, http.StatusServiceUnavailable, nil, nil), clierr.CodeGeneric, true, false},
		"500":       {unknown(http.StatusInternalServerError, nil), clierr.CodeGeneric, false, true},
		"502":       {unknown(http.StatusBadGateway, nil), clierr.CodeGeneric, false, true},
		"504":       {unknown(http.StatusGatewayTimeout, nil), clierr.CodeGeneric, false, true},
		"reset":     {unknown(0, errors.New("connection reset by peer")), clierr.CodeGeneric, false, true},
		"timed out": {unknown(0, context.DeadlineExceeded), clierr.CodeGeneric, false, true},
	}
	commands := map[string]func(*testing.T, *appFake) runtime.ExecuteFunc{
		"install": func(t *testing.T, f *appFake) runtime.ExecuteFunc {
			t.Helper()
			_, exec := plan(t, newTestInstall(f, nil), installInput())
			return exec
		},
		"upgrade": func(t *testing.T, f *appFake) runtime.ExecuteFunc {
			t.Helper()
			_, exec := plan(t, newTestUpgrade(f, nil), upgradeInput())
			return exec
		},
	}
	for command, start := range commands {
		for name, tc := range cases {
			t.Run(command+" "+name, func(t *testing.T) {
				t.Parallel()
				f := newAppFake(t)
				if command == "upgrade" {
					f = upgradeFake(t, nil)
				}
				f.postAnswer = func(string) ([]byte, error) { return nil, tc.err }
				exec := start(t, f)
				_, err := exec(t.Context())
				require.Error(t, err)
				assert.Equal(t, 1, f.postCount())
				ce := clierr.From(err)
				assert.Equal(t, tc.code, ce.Code)
				assert.Equal(t, tc.retryable, ce.Retryable)
				if tc.unknown {
					assert.Contains(t, ce.Message, "the answer did not say whether Rancher ran it")
					assert.Contains(t, ce.Message, testRequestID)
					assert.Contains(t, ce.Message, "spec.chart.metadata.annotations")
					assert.Contains(t, ce.Message, "is the revision this request created")
					assert.Contains(t, ce.Message, clierr.Rancher+" get app 'my-nginx' --cluster 'prod' --namespace 'web'")
					assert.Equal(t, clierr.Rancher+" get operations --cluster 'prod' --namespace 'web'", ce.Hint)
				}
			})
		}
	}
}

// TestActionErrorTextIsFiltered asserts the server's text of a refused
// action reaches the error with every value hidden, in its plain and its
// JSON-escaped form, a multi-line value included, for each status.
func TestActionErrorTextIsFiltered(t *testing.T) {
	t.Parallel()
	multi := "line-one-" + canary + "\nline-two-secret"
	src := files{"v.yaml": "password: " + canary + "\nquoted: 'say \"" + canary + "\"'\nmulti: |\n  line-one-" + canary + "\n  line-two-secret\n"}
	bodies := []string{
		`{"message":"chart refused value ` + canary + `"}`,
		`{"message":"value say \"` + canary + `\" refused"}`,
		"plain text " + strings.ReplaceAll(multi, "\n", " and ") + " line-two-secret",
	}
	for _, status := range []int{400, 403, 404, 409, 500, 503} {
		for i, body := range bodies {
			f := newAppFake(t)
			f.postAnswer = func(string) ([]byte, error) {
				return nil, clierr.FromResponse("POST /x on the Rancher server", http.MethodPost, status, []byte(body), nil)
			}
			_, exec := plan(t, newTestInstall(f, src), installInput(func(in *appInput) { in.strs[flagValues] = []string{"v.yaml"} }))
			_, err := exec(t.Context())
			require.Error(t, err)
			ce := clierr.From(err)
			assert.NotContains(t, ce.Message+ce.Hint, canary, "status %d body %d", status, i)
			assert.NotContains(t, ce.Message+ce.Hint, "line-two-secret", "status %d body %d", status, i)
			assert.Equal(t, 1, f.postCount())
		}
	}
}

// TestInstallPlanDisplayIdentity asserts the preview names the App by the
// release, DisplayKind app, and the committed result names the same
// release, never with the dry-run preview's "*" marker.
func TestInstallPlanDisplayIdentity(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	eff, exec := plan(t, newTestInstall(f, nil), installInput())
	assert.Equal(t, "app", eff.DisplayKind)
	assert.Equal(t, testRelease, eff.DisplayName)
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, testRelease, res.Data.(result).Release)
	assert.NotContains(t, res.Text, "*")
	assert.True(t, strings.HasPrefix(res.Text, "app "+testRelease+":"), res.Text)
}

// TestInstallManagementReadFailure asserts the management cluster is read
// for every install, with or without permits-os: only a refused read turns
// into warnings; any other failure is the command's error.
func TestInstallManagementReadFailure(t *testing.T) {
	t.Parallel()
	for _, info := range []string{`{"chart":{"annotations":{"catalog.cattle.io/permits-os":"linux"}}}`, `{"chart":{}}`} {
		f := newAppFake(t)
		chartWith(f, info)
		f.root.answers["/v1/management.cattle.io.clusters/"+testCluster] = rootAnswer{status: http.StatusInternalServerError, body: `{"message":"boom"}`}
		_, _, err := newTestInstall(f, nil).Plan(t.Context(), installInput())
		require.Error(t, err)
		assert.Equal(t, clierr.CodeGeneric, clierr.From(err).Code, info)
	}
}

// TestInstallExecuteIndexReadFailure asserts a failed read of the index in
// Execute is the retryable CONFLICT that asks for a new preview, and no
// request is sent.
func TestInstallExecuteIndexReadFailure(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		f := newAppFake(t)
		_, exec := plan(t, newTestInstall(f, nil), installInput())
		f.root.answers[indexPath()] = rootAnswer{status: status, body: `{"message":"gone"}`}
		_, err := exec(t.Context())
		require.ErrorContains(t, err, "the index of ClusterRepo 'repo1' could not be read again")
		ce := clierr.From(err)
		assert.Equal(t, clierr.CodeConflict, ce.Code, status)
		assert.True(t, ce.Retryable, status)
		assert.Zero(t, f.postCount())
	}
}

// TestInstallReadsTheIndexEntryAnnotations asserts the chart annotations
// come from the index entry, as the Rancher UI reads them, and from the info
// link's chart only for an entry that carries none.
func TestInstallReadsTheIndexEntryAnnotations(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	f.root.answers[indexPath()] = rootAnswer{status: 200, body: indexJSON(map[string][]ChartVersion{testChart: {
		{Version: "2.0.0", Digest: "sha256:aaa", Annotations: map[string]string{annotationNamespace: "cattle-system"}},
	}})}
	chartWith(f, `{"chart":{"annotations":{}}}`)
	_, _, err := newTestInstall(f, nil).Plan(t.Context(), installInput())
	require.ErrorContains(t, err, "installs only in namespace 'cattle-system'")

	f = newAppFake(t)
	f.root.answers[indexPath()] = rootAnswer{status: 200, body: indexJSON(map[string][]ChartVersion{testChart: {
		{Version: "2.0.0", Digest: "sha256:aaa", Annotations: map[string]string{annotationNamespace: testNS}},
	}})}
	chartWith(f, `{"chart":{"annotations":{"catalog.cattle.io/namespace":"cattle-system"}}}`)
	_, _, err = newTestInstall(f, nil).Plan(t.Context(), installInput())
	require.NoError(t, err)
}

// TestInstallResultNamesTheNoAppCase asserts the install result warns, once,
// that a chart that fails before Helm stores the release leaves no app and
// the operation shows the failure.
func TestInstallResultNamesTheNoAppCase(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	_, exec := plan(t, newTestInstall(f, nil), installInput())
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Contains(t, res.Warnings, noAppNote)
	assert.NotContains(t, res.Text, noAppNote, "the note is a warning, printed once")
}

// TestOutcomeUnknownShowsRancherMessage asserts an unknown outcome whose
// answer carries Rancher's message shows it, with every value of the
// request hidden, and keeps the code, retry advice and hint.
func TestOutcomeUnknownShowsRancherMessage(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	f.postAnswer = func(string) ([]byte, error) {
		return nil, &engine.ActionOutcomeUnknown{
			Err:  clierr.ResponseError(http.MethodPost, http.StatusInternalServerError, nil),
			Body: []byte(`{"type":"error","message":"failed to fetch chart: value ` + canary + ` refused"}`),
		}
	}
	_, exec := plan(t, newTestInstall(f, files{"v.yaml": "password: " + canary + "\n"}), installInput(func(in *appInput) { in.strs[flagValues] = []string{"v.yaml"} }))
	_, err := exec(t.Context())
	ce := clierr.From(err)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.False(t, ce.Retryable)
	assert.Contains(t, ce.Message, "(the server answered 500: failed to fetch chart: value <hidden> refused)")
	assert.NotContains(t, ce.Message, canary)
	assert.Equal(t, clierr.Rancher+" get operations --cluster 'prod' --namespace 'web'", ce.Hint)
	assert.Equal(t, 1, f.postCount())
}

// TestRefusedActionHidesValuesBeforeTheCut asserts a refused action takes
// its text from the answer's body with the values hidden before the cut,
// and keeps the status table's code, retry advice and hint.
func TestRefusedActionHidesValuesBeforeTheCut(t *testing.T) {
	t.Parallel()
	body := `{"type":"error","message":"refused ` + strings.Repeat("a", 190) + canary + ` tail"}`
	f := newAppFake(t)
	f.postAnswer = func(string) ([]byte, error) {
		return nil, &engine.ActionRefused{
			Err:  clierr.FromResponse("POST /x on the Rancher server", http.MethodPost, http.StatusForbidden, []byte(body), nil),
			Body: []byte(body),
		}
	}
	_, exec := plan(t, newTestInstall(f, files{"v.yaml": "password: " + canary + "\n"}), installInput(func(in *appInput) { in.strs[flagValues] = []string{"v.yaml"} }))
	_, err := exec(t.Context())
	ce := clierr.From(err)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Contains(t, ce.Message, "refused "+strings.Repeat("a", 190)+"<h")
	assert.NotContains(t, ce.Message+ce.Hint, canary[:4])
}

// TestRefusedActionKeepsTheTableWording asserts a refused action keeps the
// status table's code, retry advice, hint and wording: a 401 is AUTH with
// the credential hint, unchanged when values hold words of the hint, and a
// 503 names its status.
func TestRefusedActionKeepsTheTableWording(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status   int
		code     string
		hint     string
		contains string
		values   string
	}{
		{http.StatusUnauthorized, clierr.CodeAuth, clierr.AuthStatusRemedy, "", "password: " + canary + "\n"},
		{http.StatusUnauthorized, clierr.CodeAuth, clierr.AuthStatusRemedy, "", "password: " + canary + "\na: rancher\nb: status\n"},
		{http.StatusServiceUnavailable, clierr.CodeGeneric, "", "503", "password: " + canary + "\n"},
	} {
		body := `{"type":"error","message":"no ` + canary + `"}`
		f := newAppFake(t)
		request := &clierr.Request{Server: "rancher.test"}
		f.postAnswer = func(string) ([]byte, error) {
			return nil, &engine.ActionRefused{
				Err:     clierr.FromResponse("POST /x on the Rancher server", http.MethodPost, tc.status, []byte(body), request),
				Body:    []byte(body),
				Request: request,
			}
		}
		_, exec := plan(t, newTestInstall(f, files{"v.yaml": tc.values}), installInput(func(in *appInput) { in.strs[flagValues] = []string{"v.yaml"} }))
		_, err := exec(t.Context())
		ce := clierr.From(err)
		assert.Equal(t, tc.code, ce.Code, tc.status)
		assert.Equal(t, tc.hint, ce.Hint, tc.status)
		assert.Contains(t, ce.Message, tc.contains, tc.status)
		assert.NotContains(t, ce.Message+ce.Hint, canary[:4], tc.status)
	}
}
