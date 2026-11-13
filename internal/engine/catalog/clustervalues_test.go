package catalog

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registryChartInfo is an info answer whose default values declare both
// registry paths.
const registryChartInfo = `{"values":{"global":{"systemDefaultRegistry":"","cattle":{"systemDefaultRegistry":""}}},"chart":{"annotations":{}}}`

// windowsClusterObject is a management Cluster with Windows and Linux
// workers and RKE1 path prefixes.
const windowsClusterObject = `{"id":"c-1","metadata":{"name":"c-1"},"spec":{"displayName":"Production",` +
	`"rancherKubernetesEngineConfig":{"prefixPath":"/opt/rke","winPrefixPath":"c:/rke"}},` +
	`"status":{"windowsWorkerCount":1,"linuxWorkerCount":2}}`

// The warnings of a source the caller cannot read, for the fake's cluster.
const (
	warnClusterDenied = "cannot read cluster 'prod'; values.global.cattle.clusterName, values.global.cattle.rkePathPrefix, " +
		"values.global.cattle.rkeWindowsPathPrefix, values.global.cattle.windows.enabled were not sent"
	warnServerURLDenied = "cannot read the server-url setting; values.global.cattle.url was not sent"
	warnProjectsDenied  = "cannot list the projects of cluster 'prod'; values.global.cattle.systemProjectId was not sent"
	warnSettingDenied   = "cannot read the system-default-registry setting; the registry values are sent empty; name a registry with --system-default-registry"
)

var (
	cattleRegistry = valuePath{"global", "cattle", "systemDefaultRegistry"}
	globalRegistry = valuePath{"global", "systemDefaultRegistry"}
)

// withValues names the values file v.yaml.
func withValues(in *appInput) { in.strs[flagValues] = []string{"v.yaml"} }

// sentValues returns the values of the one install request f recorded.
func sentValues(t *testing.T, f *appFake) map[string]any {
	t.Helper()
	require.Equal(t, 1, f.postCount())
	chart := f.posts[0].body["charts"].([]any)[0].(map[string]any)
	v, _ := chart["values"].(map[string]any)
	return v
}

// sentAt reads the value a request sent at path.
func sentAt(tree map[string]any, path valuePath) (any, bool) {
	x, st, _ := lookup(tree, path)
	return x, st == pathPresent
}

// fileAt is a values file that sets path to value.
func fileAt(path valuePath, value string) string {
	var b strings.Builder
	for i, k := range path {
		b.WriteString(strings.Repeat("  ", i) + k + ":")
		if i < len(path)-1 {
			b.WriteString("\n")
		}
	}
	b.WriteString(" " + value + "\n")
	return b.String()
}

// installed runs Plan and Execute of f's install and returns the Effect.
func installed(t *testing.T, f *appFake, src files, edits ...func(*appInput)) map[string]any {
	t.Helper()
	eff, exec := plan(t, newTestInstall(f, src), installInput(edits...))
	_, err := exec(t.Context())
	require.NoError(t, err)
	out := diffPaths(eff)
	out["@warnings"] = eff.Warnings
	return out
}

// TestClusterValues asserts each cluster value: added with its value when
// the files set nothing at its path, and shown with that value in the
// preview; sent as the file has it and shown as <hidden> when a file sets
// it; left out, with a warning naming it, when its source answers 403;
// values.global.cattle.windows.enabled is sent only for a cluster with
// Windows workers.
func TestClusterValues(t *testing.T) {
	t.Parallel()
	rows := []struct {
		path   valuePath
		want   any
		source string
		denied string
	}{
		{valuePath{"global", "cattle", "clusterId"}, testCluster, "", ""},
		{valuePath{"global", "cattle", "clusterName"}, "Production", clusterPath, warnClusterDenied},
		{valuePath{"global", "cattle", "systemProjectId"}, testSystemProject, projectsPath, warnProjectsDenied},
		{valuePath{"global", "cattle", "url"}, testServerURL, settingPath(settingServerURL), warnServerURLDenied},
		{valuePath{"global", "cattle", "rkePathPrefix"}, "/opt/rke", clusterPath, warnClusterDenied},
		{valuePath{"global", "cattle", "rkeWindowsPathPrefix"}, "c:/rke", clusterPath, warnClusterDenied},
		{valuePath{"global", "cattle", "windows", "enabled"}, true, clusterPath, warnClusterDenied},
	}
	var rowPaths, listPaths []string
	for _, r := range rows {
		rowPaths = append(rowPaths, r.path.String())
	}
	for _, v := range clusterValues {
		listPaths = append(listPaths, v.path.String())
	}
	require.ElementsMatch(t, listPaths, rowPaths, "every cluster value has one row, and every row is a cluster value")

	newFake := func(t *testing.T) *appFake {
		t.Helper()
		f := newAppFake(t)
		f.root.answers[clusterPath] = rootAnswer{status: 200, body: windowsClusterObject}
		return f
	}
	for _, r := range rows {
		t.Run(r.path.String(), func(t *testing.T) {
			t.Parallel()
			t.Run("absent", func(t *testing.T) {
				t.Parallel()
				f := newFake(t)
				diff := installed(t, f, nil)
				got, ok := sentAt(sentValues(t, f), r.path)
				require.True(t, ok, "not sent")
				assert.Equal(t, r.want, got)
				assert.Equal(t, r.want, diff[r.path.String()])
				assert.Contains(t, diff["@warnings"].([]string)[3], r.path.String())
			})
			t.Run("set in a file", func(t *testing.T) {
				t.Parallel()
				f := newFake(t)
				diff := installed(t, f, files{"v.yaml": fileAt(r.path, canary)}, withValues)
				got, _ := sentAt(sentValues(t, f), r.path)
				assert.Equal(t, canary, got)
				assert.Equal(t, hiddenText, diff[r.path.String()])
				assert.NotContains(t, diff["@warnings"].([]string)[3], r.path.String())
			})
			if r.source == "" {
				return
			}
			t.Run("source denied", func(t *testing.T) {
				t.Parallel()
				f := newFake(t)
				f.root.answers[r.source] = rootAnswer{status: http.StatusForbidden, body: `{"message":"no"}`}
				diff := installed(t, f, nil)
				sent := sentValues(t, f)
				_, ok := sentAt(sent, r.path)
				assert.False(t, ok, "sent with its source denied")
				assert.NotContains(t, diff, r.path.String())
				assert.Contains(t, diff["@warnings"], r.denied)
				id, _ := sentAt(sent, valuePath{"global", "cattle", "clusterId"})
				assert.Equal(t, testCluster, id)
			})
		})
	}
	t.Run("linux workers", func(t *testing.T) {
		t.Parallel()
		f := newAppFake(t)
		installed(t, f, nil)
		_, ok := sentAt(sentValues(t, f), valuePath{"global", "cattle", "windows", "enabled"})
		assert.False(t, ok)
	})
}

// TestClusterNameFallsBackToTheClusterName asserts a management Cluster
// with no spec.displayName gives its metadata.name.
func TestClusterNameFallsBackToTheClusterName(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	f.root.answers[clusterPath] = rootAnswer{status: 200, body: `{"metadata":{"name":"c-1"}}`}
	installed(t, f, nil)
	got, _ := sentAt(sentValues(t, f), valuePath{"global", "cattle", "clusterName"})
	assert.Equal(t, testCluster, got)
}

// TestClusterValueReadsThatAreNotDenials asserts a 404 of a Setting or of
// the project list counts as "" with no warning, and any other failure of
// those reads or of the management Cluster read is the command's error.
func TestClusterValueReadsThatAreNotDenials(t *testing.T) {
	t.Parallel()
	for _, path := range []string{settingPath(settingServerURL), projectsPath} {
		t.Run(path+" 404", func(t *testing.T) {
			t.Parallel()
			f := newAppFake(t)
			f.root.answers[path] = rootAnswer{status: http.StatusNotFound, body: `{"message":"gone"}`}
			diff := installed(t, f, nil)
			for _, w := range diff["@warnings"].([]string) {
				assert.NotContains(t, w, "cannot read")
				assert.NotContains(t, w, "cannot list the projects")
			}
			sent := sentValues(t, f)
			url, _ := sentAt(sent, valuePath{"global", "cattle", "url"})
			project, _ := sentAt(sent, valuePath{"global", "cattle", "systemProjectId"})
			if path == projectsPath {
				assert.Equal(t, "", project)
				assert.Equal(t, testServerURL, url)
			} else {
				assert.Equal(t, "", url)
				assert.Equal(t, testSystemProject, project)
			}
		})
	}
	for _, path := range []string{settingPath(settingServerURL), projectsPath, clusterPath} {
		for _, status := range []int{http.StatusInternalServerError, http.StatusNotFound} {
			if status == http.StatusNotFound && path != clusterPath {
				continue
			}
			t.Run(path+" "+strconv.Itoa(status), func(t *testing.T) {
				t.Parallel()
				f := newAppFake(t)
				f.root.answers[path] = rootAnswer{status: status, body: `{"message":"boom"}`}
				_, _, err := newTestInstall(f, nil).Plan(t.Context(), installInput())
				if status == http.StatusNotFound {
					require.ErrorContains(t, err, "cluster not found: 'c-1'")
					assert.Equal(t, clierr.CodeNotFound, clierr.From(err).Code)
				} else {
					require.ErrorContains(t, err, "GET "+path)
					assert.Equal(t, clierr.CodeGeneric, clierr.From(err).Code)
				}
				assert.Zero(t, f.postCount())
			})
		}
	}
}

// TestSystemProjectLookup asserts the System project is the project with
// the System project label, never one displayed as System without it; with
// none labeled the value is ""; with two, the lowest name.
func TestSystemProjectLookup(t *testing.T) {
	t.Parallel()
	item := func(name, display string, system bool) string {
		labels := ""
		if system {
			labels = `,"labels":{"authz.management.cattle.io/system-project":"true"}`
		}
		return `{"id":"c-1/` + name + `","metadata":{"name":"` + name + `"` + labels + `},"spec":{"clusterName":"c-1","displayName":"` + display + `"}}`
	}
	cases := map[string]struct {
		items []string
		want  string
	}{
		"labeled after an unlabeled System": {[]string{item("p-aaaaa", "System", false), item("p-sys", "Platform", true)}, "p-sys"},
		"none labeled":                      {[]string{item("p-aaaaa", "System", false)}, ""},
		"none visible":                      {nil, ""},
		"two labeled":                       {[]string{item("p-zzz", "System", true), item("p-bbb", "System", true)}, "p-bbb"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newAppFake(t)
			f.root.answers[projectsPath] = rootAnswer{status: 200, body: `{"data":[` + strings.Join(tc.items, ",") + `],"count":` +
				strconv.Itoa(len(tc.items)) + `,"revision":"1"}`}
			diff := installed(t, f, nil)
			got, _ := sentAt(sentValues(t, f), valuePath{"global", "cattle", "systemProjectId"})
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, diff["@warnings"], warnProjectsDenied)
		})
	}
}

// TestEachSourceIsReadOnce asserts one Plan reads the management Cluster,
// each Setting and the project list once at most, a chart with permits-os
// included, and reads them through steve on the Rancher server.
func TestEachSourceIsReadOnce(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	chartWith(f, `{"values":{"global":{"cattle":{"systemDefaultRegistry":""}}},"chart":{"annotations":{"catalog.cattle.io/permits-os":"linux,windows"}}}`)
	_, _ = plan(t, newTestInstall(f, nil), installInput())
	counts := map[string]int{}
	for _, p := range f.root.requests() {
		counts[p]++
	}
	for _, p := range []string{clusterPath, projectsPath, settingPath(settingServerURL), settingPath(settingSystemDefaultRegistry)} {
		assert.Equal(t, 1, counts[p], p)
	}
	for _, r := range f.reads {
		assert.NotContains(t, r, "settings")
		assert.NotContains(t, r, "projects")
		assert.NotContains(t, r, "clusters")
	}
}

// TestRegistryValues asserts the registry values of a chart that declares
// a registry path: both paths set to one value, from the first source that
// gives a non-empty string, in order: --system-default-registry, the files'
// values merged over the chart's defaults (the cattle path first), the
// management Cluster's imported registry, its provisioned cluster
// registry, then the system-default-registry Setting (value, else
// default), which is read only when that step is reached.
func TestRegistryValues(t *testing.T) {
	t.Parallel()
	const (
		flagHost     = "flag.example.test"
		fileHost     = "file.example.test"
		plainHost    = "plain.example.test"
		chartHost    = "chart.example.test"
		importedHost = "imported.example.test"
		secretsHost  = "secrets.example.test"
	)
	cluster := func(spec string) string {
		return `{"metadata":{"name":"c-1"},"spec":{"displayName":"Production"` + spec + `},"status":{"linuxWorkerCount":2}}`
	}
	imported := `,"importedConfig":{"privateRegistryURL":"` + importedHost + `"}`
	secrets := `,"clusterSecrets":{"privateRegistryURL":"` + secretsHost + `"}`
	cases := []struct {
		name        string
		info        string
		file        string
		cluster     string
		setting     string
		host        string
		want        string
		settingRead bool
		added       bool
	}{
		{name: "the flag over a file, the cluster and the Setting", file: fileAt(cattleRegistry, fileHost), cluster: cluster(imported), host: flagHost, want: flagHost, added: true},
		{name: "the file's cattle path over its global path", file: "global:\n  systemDefaultRegistry: " + plainHost + "\n  cattle:\n    systemDefaultRegistry: " + fileHost + "\n", cluster: cluster(imported), want: fileHost},
		{name: "the file over the chart's default", info: `{"values":{"global":{"cattle":{"systemDefaultRegistry":"` + chartHost + `"}}}}`, file: fileAt(cattleRegistry, fileHost), want: fileHost},
		{name: "the file's global path when the cattle path is empty", file: fileAt(globalRegistry, plainHost), cluster: cluster(imported), want: plainHost, added: true},
		{name: "the chart's default over the cluster", info: `{"values":{"global":{"systemDefaultRegistry":"` + chartHost + `"}}}`, cluster: cluster(imported), want: chartHost, added: true},
		{name: "the imported registry over the provisioned one", cluster: cluster(imported + secrets), want: importedHost, added: true},
		{name: "the provisioned registry over the Setting", cluster: cluster(secrets), want: secretsHost, added: true},
		{name: "the Setting's default", want: testSettingRegistry, settingRead: true, added: true},
		{name: "the Setting's value over its default", setting: `{"value":"value.example.test","default":"` + testSettingRegistry + `"}`, want: "value.example.test", settingRead: true, added: true},
		{name: "a chart that declares one path gets both", info: `{"values":{"global":{"cattle":{"systemDefaultRegistry":""}}}}`, cluster: cluster(secrets), want: secretsHost, added: true},
		{name: "empty from every source", setting: `{"value":"","default":""}`, want: "", settingRead: true, added: true},
		{name: "an empty file value falls through", file: fileAt(cattleRegistry, `""`), cluster: cluster(imported), want: importedHost, added: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAppFake(t)
			info := registryChartInfo
			if tc.info != "" {
				info = tc.info
			}
			chartWith(f, info)
			if tc.cluster != "" {
				f.root.answers[clusterPath] = rootAnswer{status: 200, body: tc.cluster}
			}
			if tc.setting != "" {
				f.root.answers[settingPath(settingSystemDefaultRegistry)] = rootAnswer{status: 200, body: tc.setting}
			}
			edits := []func(*appInput){}
			src := files{}
			if tc.file != "" {
				src["v.yaml"] = tc.file
				edits = append(edits, withValues)
			}
			if tc.host != "" {
				edits = append(edits, func(in *appInput) { in.s[flagSystemDefaultRegistry] = tc.host })
			}
			diff := installed(t, f, src, edits...)
			sent := sentValues(t, f)
			for _, p := range registryPaths {
				got, ok := sentAt(sent, p)
				assert.True(t, ok, "%s not sent", p)
				assert.Equal(t, tc.want, got, p.String())
				assert.Equal(t, tc.want, diff[p.String()], p.String())
			}
			assert.Equal(t, tc.settingRead, slices.Contains(f.root.requests(), settingPath(settingSystemDefaultRegistry)))
			added := diff["@warnings"].([]string)[3]
			assert.Equal(t, tc.added, strings.Contains(added, cattleRegistry.String()), added)
		})
	}
}

// TestRegistryNotSentForAChartThatDeclaresNeither asserts a chart whose
// default values declare no registry path gets the cluster values and no
// registry value, and the system-default-registry Setting is not read; a
// file's value at a registry path is sent as the file has it and shown as
// <hidden>.
func TestRegistryNotSentForAChartThatDeclaresNeither(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	installed(t, f, nil)
	sent := sentValues(t, f)
	assert.Equal(t, map[string]any{"global": testCattleValues()}, sent)
	assert.NotContains(t, f.root.requests(), settingPath(settingSystemDefaultRegistry))

	f = newAppFake(t)
	diff := installed(t, f, files{"v.yaml": fileAt(globalRegistry, fileRegistry)}, withValues)
	got, _ := sentAt(sentValues(t, f), globalRegistry)
	assert.Equal(t, fileRegistry, got)
	assert.Equal(t, hiddenText, diff[globalRegistry.String()])
	_, ok := sentAt(sentValues(t, f), cattleRegistry)
	assert.False(t, ok)
}

// TestPreviewShowsAFileRegistryValueOnlyAsAStringOfAChartThatDeclaresOne
// asserts a file's value at a registry path is shown only when the chart
// declares a registry path and the value is a string: a list or a mapping
// there, or any value on a chart that declares neither path, is <hidden>.
// The files' global.cattle: null makes the CLI add neither registry path,
// so the file's own value at global.systemDefaultRegistry is the one sent.
func TestPreviewShowsAFileRegistryValueOnlyAsAStringOfAChartThatDeclaresOne(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, info, value string
		want              any
	}{
		{"string, chart declares", registryChartInfo, fileRegistry, fileRegistry},
		{"list, chart declares", registryChartInfo, "[" + canary + "]", hiddenText},
		{"mapping, chart declares", registryChartInfo, "{a: " + canary + "}", hiddenText},
		{"empty mapping, chart declares", registryChartInfo, "{}", hiddenText},
		{"string, chart declares neither", "", fileRegistry, hiddenText},
		{"list, chart declares neither", "", "[" + canary + "]", hiddenText},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAppFake(t)
			if tc.info != "" {
				chartWith(f, tc.info)
			}
			src := files{"v.yaml": "global:\n  cattle: null\n  systemDefaultRegistry: " + tc.value + "\n"}
			eff, _ := plan(t, newTestInstall(f, src), installInput(withValues))
			want := tc.want
			path := globalRegistry.String()
			if strings.HasPrefix(tc.value, "{a") {
				path += ".a"
			}
			for _, d := range eff.Diff {
				assert.NotContains(t, fmt.Sprint(d.To), canary, d.Path)
			}
			assert.Equal(t, want, diffPaths(eff)[path])
		})
	}
}

const fileRegistry = "file-registry.example.test"

// TestRegistryFlagRefusals asserts either registry flag on a chart that
// declares neither registry path is VALIDATION naming the chart and both
// paths, with no request sent. Both flags together are refused before any
// read (TestInstallRefusedBeforeAnyRead).
func TestRegistryFlagRefusals(t *testing.T) {
	t.Parallel()
	for flag, edit := range map[string]func(*appInput){
		flagSystemDefaultRegistry:   func(in *appInput) { in.s[flagSystemDefaultRegistry] = "r.example.test" },
		flagNoSystemDefaultRegistry: func(in *appInput) { in.b[flagNoSystemDefaultRegistry] = true },
	} {
		f := newAppFake(t)
		_, _, err := newTestInstall(f, nil).Plan(t.Context(), installInput(edit))
		require.ErrorContains(t, err, "chart 'nginx' '2.0.0' declares neither global.cattle.systemDefaultRegistry nor global.systemDefaultRegistry in its default values; --"+
			flag+" applies only to a chart that declares one; run the command without it")
		assert.Equal(t, clierr.CodeValidation, clierr.From(err).Code)
		assert.Zero(t, f.postCount())
	}
}

// TestNoSystemDefaultRegistry asserts --no-system-default-registry sends
// neither registry path, also when a values file sets one: the file's
// value is dropped, the preview lists no entry for it and warns, naming the
// dropped path, and the Setting is not read.
func TestNoSystemDefaultRegistry(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	chartWith(f, registryChartInfo)
	diff := installed(t, f, files{"v.yaml": fileAt(cattleRegistry, fileRegistry)}, withValues, func(in *appInput) { in.b[flagNoSystemDefaultRegistry] = true })
	sent := sentValues(t, f)
	for _, p := range registryPaths {
		_, ok := sentAt(sent, p)
		assert.False(t, ok, p.String())
		assert.NotContains(t, diff, p.String())
	}
	assert.Equal(t, testCattleValues(), sent["global"])
	assert.Contains(t, diff["@warnings"], "--no-system-default-registry leaves out values.global.cattle.systemDefaultRegistry set in the values files")
	assert.NotContains(t, f.root.requests(), settingPath(settingSystemDefaultRegistry))
}

// TestRegistryDeniedReads asserts a denied read on the registry's way: the
// management Cluster's 403 names the registry as not read and the Setting
// gives the value; the Setting's 403 sends both paths empty and names the
// flag; a denied Setting is not warned about when an earlier step gave the
// value.
func TestRegistryDeniedReads(t *testing.T) {
	t.Parallel()
	denied := rootAnswer{status: http.StatusForbidden, body: `{"message":"no"}`}

	f := newAppFake(t)
	chartWith(f, registryChartInfo)
	f.root.answers[clusterPath] = denied
	diff := installed(t, f, nil)
	assert.Contains(t, diff["@warnings"], warnClusterDenied+", and the cluster's registry was not read")
	got, _ := sentAt(sentValues(t, f), cattleRegistry)
	assert.Equal(t, testSettingRegistry, got)

	f = newAppFake(t)
	chartWith(f, registryChartInfo)
	f.root.answers[settingPath(settingSystemDefaultRegistry)] = denied
	diff = installed(t, f, nil)
	assert.Contains(t, diff["@warnings"], warnSettingDenied)
	for _, p := range registryPaths {
		got, _ := sentAt(sentValues(t, f), p)
		assert.Equal(t, "", got, p.String())
	}

	for name, edit := range map[string]func(*appInput){
		"flag": func(in *appInput) { in.s[flagSystemDefaultRegistry] = "r.example.test" },
		"file": withValues,
	} {
		f := newAppFake(t)
		chartWith(f, registryChartInfo)
		f.root.answers[settingPath(settingSystemDefaultRegistry)] = denied
		f.root.answers[clusterPath] = denied
		diff := installed(t, f, files{"v.yaml": fileAt(globalRegistry, fileRegistry)}, edit)
		assert.NotContains(t, diff["@warnings"], warnSettingDenied, name)
		assert.Contains(t, diff["@warnings"], warnClusterDenied, name)
		assert.NotContains(t, f.root.requests(), settingPath(settingSystemDefaultRegistry), name)
	}
}

// TestClusterValueWarningOrder asserts the warnings of the composition come
// after every other warning of the chart checks and the operations check,
// before the pull-secret warning, in their order.
func TestClusterValueWarningOrder(t *testing.T) {
	t.Parallel()
	denied := rootAnswer{status: http.StatusForbidden, body: `{"message":"no"}`}
	f := newAppFake(t)
	chartWith(f, `{"values":{"global":{"cattle":{"systemDefaultRegistry":""}}},"chart":{"annotations":{"catalog.cattle.io/permits-os":"linux"}}}`)
	for _, p := range []string{clusterPath, settingPath(settingServerURL), projectsPath, settingPath(settingSystemDefaultRegistry)} {
		f.root.answers[p] = denied
	}
	f.fail("list:operations/"+testNS, clierr.New(clierr.CodeRBAC, "forbidden"))
	eff, _ := plan(t, newTestInstall(f, nil), installInput())
	assert.Equal(t, []string{
		"Rancher runs Helm in a pod as you; the chart's own writes are checked only when Helm runs them",
		warnServerReads,
		"uninstalling the release leaves the CRDs of the chart's crds/ directory and the resources marked helm.sh/resource-policy: keep",
		"cannot read cluster 'prod'; chart 'nginx' '2.0.0' permits only linux nodes (annotation catalog.cattle.io/permits-os), which was not checked against its workers",
		"you cannot list operations in namespace 'web'; a running operation for release 'my-nginx' was not checked",
		"rancher-cli adds the values the Rancher UI adds when it installs or upgrades a chart: values.global.cattle.clusterId, " +
			"values.global.cattle.systemDefaultRegistry, values.global.systemDefaultRegistry",
		warnClusterDenied + ", and the cluster's registry was not read",
		warnServerURLDenied,
		warnProjectsDenied,
		warnSettingDenied,
		warnPullSecretsInstall,
	}, eff.Warnings)

	f = newAppFake(t)
	chartWith(f, registryChartInfo)
	eff, _ = plan(t, newTestInstall(f, files{"v.yaml": fileAt(globalRegistry, fileRegistry)}), installInput(withValues, func(in *appInput) { in.b[flagNoSystemDefaultRegistry] = true }))
	assert.Equal(t, []string{
		"rancher-cli adds the values the Rancher UI adds when it installs or upgrades a chart: values.global.cattle.clusterId, values.global.cattle.clusterName, " +
			"values.global.cattle.rkePathPrefix, values.global.cattle.rkeWindowsPathPrefix, values.global.cattle.systemProjectId, values.global.cattle.url",
		"--no-system-default-registry leaves out values.global.systemDefaultRegistry set in the values files",
		warnPullSecretsInstall,
	}, eff.Warnings[3:])
}

// TestParentsThatAreNotMappings asserts a parent of an added path that a
// file sets to null, a scalar or a list: nothing is added below it, the
// file's value is sent as it is, --system-default-registry is VALIDATION
// naming the path, and without a flag the CLI adds neither registry path
// while a file's value at the other one is sent.
func TestParentsThatAreNotMappings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, file string
		want       map[string]any
		blocked    string
	}{
		{"global null", "global: null\n", map[string]any{"global": nil}, "values.global"},
		{"global scalar", "global: 3\n", map[string]any{"global": float64(3)}, "values.global"},
		{"global.cattle a list", "global:\n  cattle: []\n  systemDefaultRegistry: " + fileRegistry + "\n",
			map[string]any{"global": map[string]any{"cattle": []any{}, "systemDefaultRegistry": fileRegistry}}, "values.global.cattle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAppFake(t)
			chartWith(f, registryChartInfo)
			diff := installed(t, f, files{"v.yaml": tc.file}, withValues)
			assert.Equal(t, tc.want, sentValues(t, f))
			for _, w := range diff["@warnings"].([]string) {
				assert.NotContains(t, w, "rancher-cli adds")
			}
			assert.NotContains(t, f.root.requests(), settingPath(settingSystemDefaultRegistry))

			f = newAppFake(t)
			chartWith(f, registryChartInfo)
			_, _, err := newTestInstall(f, files{"v.yaml": tc.file}).Plan(t.Context(), installInput(withValues, func(in *appInput) { in.s[flagSystemDefaultRegistry] = "r.example.test" }))
			require.ErrorContains(t, err, "--system-default-registry cannot set values.global.cattle.systemDefaultRegistry: the values files set "+tc.blocked+" to a value that is not a mapping")
			assert.Equal(t, clierr.CodeValidation, clierr.From(err).Code)

			f = newAppFake(t)
			chartWith(f, registryChartInfo)
			installed(t, f, files{"v.yaml": tc.file}, withValues, func(in *appInput) { in.b[flagNoSystemDefaultRegistry] = true })
			sent := sentValues(t, f)
			_, ok := sentAt(sent, globalRegistry)
			assert.False(t, ok)
		})
	}
	t.Run("global.cattle.windows a scalar", func(t *testing.T) {
		t.Parallel()
		f := newAppFake(t)
		f.root.answers[clusterPath] = rootAnswer{status: 200, body: windowsClusterObject}
		installed(t, f, files{"v.yaml": "global:\n  cattle:\n    windows: true\n"}, withValues)
		got, _ := sentAt(sentValues(t, f), valuePath{"global", "cattle", "windows"})
		assert.Equal(t, true, got)
	})
}

// TestPreviewListsTheLeavesSent asserts the preview's value entries are the
// leaves of the values sent, each once: a file's empty mapping that gains
// added values below it has no entry, and a file's mapping at a registry
// path that the registry value replaces leaves no entry below it.
func TestPreviewListsTheLeavesSent(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	chartWith(f, registryChartInfo)
	f.root.answers[clusterPath] = rootAnswer{status: 200, body: `{"metadata":{"name":"c-1"},"spec":{"importedConfig":{"privateRegistryURL":"r.example.test"}}}`}
	eff, _ := plan(t, newTestInstall(f, files{"v.yaml": "global:\n  cattle:\n    systemDefaultRegistry:\n      a: " + canary + "\n  other: {}\n"}), installInput(withValues))
	var paths []string
	for _, d := range eff.Diff {
		if strings.HasPrefix(d.Path, "values.") {
			paths = append(paths, d.Path)
		}
	}
	assert.Equal(t, []string{
		"values.global.cattle.clusterId", "values.global.cattle.clusterName", "values.global.cattle.rkePathPrefix",
		"values.global.cattle.rkeWindowsPathPrefix", "values.global.cattle.systemDefaultRegistry", "values.global.cattle.systemProjectId",
		"values.global.cattle.url", "values.global.other", "values.global.systemDefaultRegistry",
	}, paths)
	diff := diffPaths(eff)
	assert.Equal(t, "r.example.test", diff["values.global.cattle.systemDefaultRegistry"])
	assert.Equal(t, hiddenText, diff["values.global.other"])

	f = newAppFake(t)
	eff, _ = plan(t, newTestInstall(f, files{"v.yaml": "global:\n  cattle: {}\n"}), installInput(withValues))
	assert.NotContains(t, diffPaths(eff), "values.global.cattle")
}

// TestFilterCoversTheFilesOnly asserts the server-text filter of composed
// values hides a file's value and leaves an added value as it is.
func TestFilterCoversTheFilesOnly(t *testing.T) {
	t.Parallel()
	v, err := ReadValues(files{"v.yaml": "password: " + canary + "\n"}, []string{"v.yaml"}, false)
	require.NoError(t, err)
	sent, added, _ := v.compose([]addedValue{{path: valuePath{"global", "cattle", "url"}, value: testServerURL}}, nil)
	require.Equal(t, []string{"values.global.cattle.url"}, added)
	assert.Equal(t, "at "+testServerURL+" refused "+hiddenText, v.Filter("at "+testServerURL+" refused "+canary))
	assert.Equal(t, hiddenText, sent.String())
}

// TestRefusedInstallKeepsTheAddedValuesInItsText asserts an install the
// server refuses with a 400 whose text quotes an added value and a file's
// value shows the added value and hides the file's.
func TestRefusedInstallKeepsTheAddedValuesInItsText(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	f.postAnswer = func(string) ([]byte, error) {
		body := []byte(`{"type":"error","message":"chart refused url ` + testServerURL + ` and password ` + canary + `"}`)
		return nil, &engine.ActionRefused{
			Err:  clierr.FromResponse("POST /x on the Rancher server", http.MethodPost, http.StatusBadRequest, body, nil),
			Body: body,
		}
	}
	_, exec := plan(t, newTestInstall(f, files{"v.yaml": "password: " + canary + "\n"}), installInput(withValues))
	_, err := exec(t.Context())
	ce := clierr.From(err)
	assert.Contains(t, ce.Message, "url "+testServerURL+" and password "+hiddenText)
	assert.NotContains(t, ce.Message+ce.Hint, canary)
	assert.Equal(t, 1, f.postCount())
}

// TestComposeNeverOverwritesAFileValue asserts an added value without
// replace is never set where the files hold a value, a falsy one included,
// and the files' values are not changed by the composition.
func TestComposeNeverOverwritesAFileValue(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`""`, "null", "false", "0", canary} {
		v, err := ReadValues(files{"v.yaml": "a:\n  b: " + raw + "\n"}, []string{"v.yaml"}, false)
		require.NoError(t, err)
		before := maps.Clone(v.tree["a"].(map[string]any))
		sent, added, _ := v.compose([]addedValue{{path: valuePath{"a", "b"}, value: "added"}, {path: valuePath{"a", "c"}, value: "added"}}, nil)
		assert.Equal(t, before["b"], sent.tree["a"].(map[string]any)["b"], raw)
		assert.Equal(t, []string{"values.a.c"}, added, raw)
		assert.Equal(t, before, v.tree["a"], raw)
	}
}

// TestASourceIsReadOnlyForAValueItAdds asserts a Setting or the project
// list is not read when the files set every value it gives, and a denied
// management Cluster's warning names only the values it would have added,
// with "was" for one value.
func TestASourceIsReadOnlyForAValueItAdds(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	f.root.answers[clusterPath] = rootAnswer{status: http.StatusForbidden, body: `{"message":"no"}`}
	src := files{"v.yaml": "global:\n  cattle:\n    url: u\n    systemProjectId: p\n    clusterName: n\n    rkePathPrefix: r\n"}
	diff := installed(t, f, src, withValues)
	assert.NotContains(t, f.root.requests(), settingPath(settingServerURL))
	assert.NotContains(t, f.root.requests(), projectsPath)
	assert.Contains(t, diff["@warnings"], "cannot read cluster 'prod'; values.global.cattle.rkeWindowsPathPrefix, values.global.cattle.windows.enabled were not sent")

	f = newAppFake(t)
	f.root.answers[clusterPath] = rootAnswer{status: http.StatusForbidden, body: `{"message":"no"}`}
	src = files{"v.yaml": "global:\n  cattle:\n    clusterName: n\n    rkePathPrefix: r\n    rkeWindowsPathPrefix: w\n"}
	diff = installed(t, f, src, withValues)
	assert.Contains(t, diff["@warnings"], "cannot read cluster 'prod'; values.global.cattle.windows.enabled was not sent")
}
