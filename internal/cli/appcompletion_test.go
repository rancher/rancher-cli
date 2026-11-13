package cli

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The paths and bodies of the app install and app uninstall completion
// sites: the ClusterRepo object and its filtered index (the index of a
// disabled repository is served too, to show it is not read), and the apps of
// a namespace.
const (
	downRepoObject         = downRepos + "/rancher-charts"
	downRepoIndex          = "/k8s/clusters/c-aaa11/v1/catalog.cattle.io.clusterrepos/rancher-charts"
	downDisabledRepoObject = downRepos + "/disabled-charts"
	downDisabledRepoIndex  = "/k8s/clusters/c-aaa11/v1/catalog.cattle.io.clusterrepos/disabled-charts"
	localRepoObject        = localRepos + "/rancher-charts"
	localRepoIndex         = "/v1/catalog.cattle.io.clusterrepos/rancher-charts"
	downAppsPath           = "/k8s/clusters/c-aaa11/apis/catalog.cattle.io/v1/namespaces/default/apps"
	downUpgradeAppsPath    = "/k8s/clusters/c-aaa11/apis/catalog.cattle.io/v1/namespaces/web/apps"
	downUpgradeAppPath     = downUpgradeAppsPath + "/my-up"
)

// chartIndexBody is a ClusterRepo index with one chart per rule of the
// --chart source: a chart that installs anywhere (nginx, whose versions are
// served newest first with a prerelease first and two deprecated ones), a
// hidden chart, a chart that names another chart to install first (both
// annotations), a chart with a fixed namespace, a chart with a fixed release
// name and a chart with only deprecated versions.
const chartIndexBody = `{"entries":{
  "nginx": [
    {"version":"2.0.0-rc.1"},
    {"version":"1.1.0"},
    {"version":"1.0.0","deprecated":true},
    {"version":"0.9.0","annotations":{"catalog.cattle.io/deprecated":"true"}},
    {"version":"0.8.0"}],
  "hidden-chart": [{"version":"1.0.0","annotations":{"catalog.cattle.io/hidden":"true"}}],
  "auto-chart": [{"version":"1.0.0","annotations":{"catalog.cattle.io/auto-install":"crd-chart=1.0.0"}}],
  "auto-gvr-chart": [{"version":"1.0.0","annotations":{"catalog.cattle.io/auto-install-gvr":"things.example.io"}}],
  "fixed-namespace-chart": [{"version":"1.0.0","annotations":{"catalog.cattle.io/namespace":"cattle-monitoring-system"}}],
  "fixed-release-chart": [{"version":"1.0.0","annotations":{"catalog.cattle.io/release-name":"fleet"}}],
  "deprecated-chart": [{"version":"1.0.0","deprecated":true},{"version":"0.9.0","annotations":{"catalog.cattle.io/deprecated":"true"}}]
}}`

// upgradeAppBody is an App as the catalog serves it: its release status and the
// chart it runs, with the annotations of the chart.
func upgradeAppBody(name, status, chart, version string, annotations map[string]string) string {
	b, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"name": name},
		"spec": map[string]any{
			"info":  map[string]any{"status": status},
			"chart": map[string]any{"metadata": map[string]any{"name": chart, "version": version, "annotations": annotations}},
		},
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// sourceRepoAnnotations name a ClusterRepo as the source of an App's chart.
func sourceRepoAnnotations(repo string) map[string]string {
	return map[string]string{
		"catalog.cattle.io/ui-source-repo-type": "cluster",
		"catalog.cattle.io/ui-source-repo":      repo,
	}
}

// addAppCompletionBodies adds what the app install, app uninstall and app
// upgrade sites read to a parity server's bodies: the ClusterRepos and
// indexes, the apps, the app my-up of the namespace web (nginx 1.1.0 from
// rancher-charts) and the downstream namespaces.
func addAppCompletionBodies(bodies map[string]string) map[string]string {
	bodies[downRepoObject] = `{"metadata":{"name":"rancher-charts"},"spec":{"enabled":true}}`
	bodies[downRepoIndex] = chartIndexBody
	bodies[downDisabledRepoObject] = `{"metadata":{"name":"disabled-charts"},"spec":{"enabled":false}}`
	bodies[downDisabledRepoIndex] = chartIndexBody
	bodies[localRepoObject] = `{"metadata":{"name":"rancher-charts"},"spec":{"enabled":true}}`
	bodies[localRepoIndex] = chartIndexBody
	bodies[downAppsPath] = items(plain("my-app"), plain("other-app"))
	myUp := upgradeAppBody("my-up", "deployed", "nginx", "1.1.0", sourceRepoAnnotations("rancher-charts"))
	bodies[downUpgradeAppsPath] = items(myUp)
	bodies[downUpgradeAppPath] = myUp
	bodies[downNamespacesPath] = items(plain("kube-system"), plain("default"))
	return bodies
}

// The reasons a live site carries when its source leaves a refusal of the
// command unfiltered.
const (
	appChartExceptionReason = "the index does not show requires-gvr or permits-os: a chart refused for one of them may be offered"

	appReleaseExceptionReason = "requires-gvr and permits-os, the annotations of a typed --version, and whether a typed --repo serves the app's chart, " +
		"need reads beyond the request allowance: an app refused for one of them may be offered"
)

// appLiveReasons lists the live rows that carry a reason and the reason each
// carries. Every other live or file row carries none.
var appLiveReasons = map[string]string{
	"app install --chart":   appChartExceptionReason,
	"app upgrade --chart":   appChartExceptionReason,
	"app upgrade --version": appChartExceptionReason,
	"app upgrade <release>": appReleaseExceptionReason,
}

// appCompletionSite is one value-taking flag or argument of a command of group
// app with how it completes: "live" (a source answers it, and words
// is a line whose TAB offers names), "file" (the shell completes files, so the
// surface offers nothing) or "none" (a value no list can offer). A "none" row
// and the row marked as an exception say why in reason.
type appCompletionSite struct {
	completion string
	reason     string
	words      []string
}

// appCompletionSites lists every value-taking flag and argument of the app
// commands. A flag or argument added to any of them with no row fails
// TestEveryValueOfAnAppCommandHasACompletion.
var appCompletionSites = map[string]appCompletionSite{
	"app install <release>":   {completion: "none", reason: "a new release name: no object to offer", words: []string{"app", "install"}},
	"app install --cluster":   {completion: "live", words: []string{"app", "install", "my-rel", "--cluster"}},
	"app install --namespace": {completion: "live", words: []string{"app", "install", "my-rel", "--cluster", "c-aaa11", "--namespace"}},
	"app install --repo":      {completion: "live", words: []string{"app", "install", "my-rel", "--cluster", "c-aaa11", "--repo"}},
	"app install --chart": {
		completion: "live",
		reason:     appChartExceptionReason,
		words:      []string{"app", "install", "my-rel", "--cluster", "c-aaa11", "--repo", "rancher-charts", "--chart"},
	},
	"app install --version":      {completion: "live", words: []string{"app", "install", "my-rel", "--cluster", "c-aaa11", "--repo", "rancher-charts", "--chart", "nginx", "--version"}},
	"app install --values":       {completion: "file", words: []string{"app", "install", "my-rel", "--cluster", "c-aaa11", "--values"}},
	"app install --helm-timeout": {completion: "none", reason: "a free duration, as get --timeout", words: []string{"app", "install", "my-rel", "--cluster", "c-aaa11", "--helm-timeout"}},
	"app install --timeout":      {completion: "none", reason: "a free duration, as get --timeout", words: []string{"app", "install", "my-rel", "--cluster", "c-aaa11", "--timeout"}},
	"app install --description":  {completion: "none", reason: "free text", words: []string{"app", "install", "my-rel", "--cluster", "c-aaa11", "--description"}},

	"app install --system-default-registry": {completion: "none", reason: "a registry host; there is no list to complete from", words: []string{"app", "install", "my-rel", "--cluster", "c-aaa11", "--system-default-registry"}},

	"app uninstall <release>":      {completion: "live", words: []string{"app", "uninstall", "--cluster", "c-aaa11", "--namespace", "default"}},
	"app uninstall --cluster":      {completion: "live", words: []string{"app", "uninstall", "my-app", "--cluster"}},
	"app uninstall --namespace":    {completion: "live", words: []string{"app", "uninstall", "my-app", "--cluster", "c-aaa11", "--namespace"}},
	"app uninstall --helm-timeout": {completion: "none", reason: "a free duration, as get --timeout", words: []string{"app", "uninstall", "my-app", "--cluster", "c-aaa11", "--helm-timeout"}},
	"app uninstall --timeout":      {completion: "none", reason: "a free duration, as get --timeout", words: []string{"app", "uninstall", "my-app", "--cluster", "c-aaa11", "--timeout"}},

	"app upgrade <release>":   {completion: "live", reason: appReleaseExceptionReason, words: []string{"app", "upgrade", "--cluster", "c-aaa11", "--namespace", "web"}},
	"app upgrade --cluster":   {completion: "live", words: []string{"app", "upgrade", "my-up", "--cluster"}},
	"app upgrade --namespace": {completion: "live", words: []string{"app", "upgrade", "my-up", "--cluster", "c-aaa11", "--namespace"}},
	"app upgrade --repo":      {completion: "live", words: []string{"app", "upgrade", "my-up", "--cluster", "c-aaa11", "--namespace", "web", "--repo"}},
	"app upgrade --chart": {
		completion: "live",
		reason:     appChartExceptionReason,
		words:      []string{"app", "upgrade", "my-up", "--cluster", "c-aaa11", "--namespace", "web", "--chart"},
	},
	"app upgrade --version": {
		completion: "live",
		reason:     appChartExceptionReason,
		words:      []string{"app", "upgrade", "my-up", "--cluster", "c-aaa11", "--namespace", "web", "--version"},
	},
	"app upgrade --values":                  {completion: "file", words: []string{"app", "upgrade", "my-up", "--cluster", "c-aaa11", "--values"}},
	"app upgrade --history-max":             {completion: "none", reason: "an integer", words: []string{"app", "upgrade", "my-up", "--cluster", "c-aaa11", "--history-max"}},
	"app upgrade --helm-timeout":            {completion: "none", reason: "a free duration, as get --timeout", words: []string{"app", "upgrade", "my-up", "--cluster", "c-aaa11", "--helm-timeout"}},
	"app upgrade --timeout":                 {completion: "none", reason: "a free duration, as get --timeout", words: []string{"app", "upgrade", "my-up", "--cluster", "c-aaa11", "--timeout"}},
	"app upgrade --description":             {completion: "none", reason: "free text", words: []string{"app", "upgrade", "my-up", "--cluster", "c-aaa11", "--description"}},
	"app upgrade --system-default-registry": {completion: "none", reason: "a registry host; there is no list to complete from", words: []string{"app", "upgrade", "my-up", "--cluster", "c-aaa11", "--system-default-registry"}},
}

// TestEveryValueOfAnAppCommandHasACompletion asserts every value-taking flag
// and argument of every command of group app has a row in appCompletionSites
// and no row names one that is gone, that a row says why when it offers
// nothing, that a live or file row carries the reason appLiveReasons lists for
// it and no other row carries one, and that through the real TAB callback a
// live row offers names, and a file or none row offers nothing and sends no
// request. NOT parallel: the completion fixture mutates env globals and the
// stored config.
func TestEveryValueOfAnAppCommandHasACompletion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)

	declared := map[string]bool{}
	for _, cmd := range wr.reg.Commands() {
		d := cmd.Describe()
		if !strings.HasPrefix(d.Name, "app ") {
			continue
		}
		for _, a := range d.Args {
			declared[d.Name+" <"+a.Name+">"] = true
		}
		for _, f := range d.Flags {
			if f.Type != "bool" {
				declared[d.Name+" --"+f.Name] = true
			}
		}
	}
	require.NotEmpty(t, declared, "the app commands are registered")
	assert.ElementsMatch(t, slices.Collect(maps.Keys(declared)), slices.Collect(maps.Keys(appCompletionSites)),
		"every value-taking flag and argument has a row, and every row names one")
	for _, key := range slices.Sorted(maps.Keys(appLiveReasons)) {
		site, ok := appCompletionSites[key]
		assert.True(t, ok, "%s has a row", key)
		assert.Contains(t, []string{"live", "file"}, site.completion, "%s is a live or file row", key)
	}

	s := newCommandSitesServer(t)
	for _, key := range slices.Sorted(maps.Keys(appCompletionSites)) {
		site := appCompletionSites[key]
		t.Run(key, func(t *testing.T) {
			switch site.completion {
			case "none":
				assert.NotEmpty(t, site.reason, "a flag that offers nothing says why")
			case "file", "live":
				assert.Equal(t, appLiveReasons[key], site.reason, "a live or file row carries the reason the table lists for it, and none outside the table")
			default:
				require.Failf(t, "unknown completion", "%q", site.completion)
			}
			completionFixture(t, s.srv.URL, true)
			s.reset()
			got := runCompletion(t, site.words, "")
			hits := s.reset()
			if site.completion == "live" {
				assert.NotEmpty(t, got, "a live site offers names")
				assert.NotEmpty(t, hits, "a live site reads the server")
				return
			}
			assert.Empty(t, got)
			assert.Empty(t, hits)
		})
	}
}

// TestAppInstallVersionCompletionOffersAPlus asserts, through the real TAB
// callback, that app install --version offers a version that holds "+" (a
// chart version such as 108.0.0+up0.9.0) as served, in the space and the
// equals form, and still offers no value that starts with "+". NOT parallel:
// the completion fixture mutates env globals and the stored config.
func TestAppInstallVersionCompletionOffersAPlus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	bodies := addAppCompletionBodies(map[string]string{})
	bodies[downRepoIndex] = `{"entries":{"nginx":[{"version":"108.0.0+up0.9.0"},{"version":"+1.0.0"},{"version":"1.0.0"}]}}`
	s := newPathCountingServer(t, bodies, steveClustersPages)
	words := []string{"app", "install", "--cluster", "c-aaa11", "--repo", "rancher-charts", "--chart", "nginx"}
	completionFixture(t, s.srv.URL, true)
	assert.Equal(t, []string{"1.0.0", "108.0.0+up0.9.0"}, runCompletion(t, append(slices.Clone(words), "--version"), ""))
	assert.Equal(t, []string{"--version=1.0.0", "--version=108.0.0+up0.9.0"}, runCompletion(t, words, "--version="))
}

// upgradeChartIndexBody is the index of rancher-charts for the app upgrade
// sites: up-chart newest first, with a version of each kind the --version
// source decides on, and a second chart.
const upgradeChartIndexBody = `{"entries":{"up-chart":[
  {"version":"3.0.0"},
  {"version":"2.0.0","annotations":{"catalog.cattle.io/auto-install":"crd-chart=1.0.0"}},
  {"version":"1.9.0","annotations":{"catalog.cattle.io/auto-install-gvr":"things.example.io"}},
  {"version":"1.8.0","annotations":{"catalog.cattle.io/namespace":"cattle-x"}},
  {"version":"1.7.0","annotations":{"catalog.cattle.io/release-name":"fleet"}},
  {"version":"1.6.0","annotations":{"catalog.cattle.io/namespace":"web"}},
  {"version":"1.5.0","annotations":{"catalog.cattle.io/release-name":"my-up"}},
  {"version":"1.0.0"}],
  "other-chart":[{"version":"1.0.0"}]}}`

// upgradeApps are the Apps of the namespace web for the app upgrade sites: one
// that an upgrade accepts, one per refused status, one per source that the
// command refuses, an App that names a chart to install first in each
// annotation, an App of another chart, an App that runs a version the index
// lists with a fixed namespace of another (the version the App runs is exempt)
// and an App that runs a version the index does not list and names a chart to
// install first.
var upgradeApps = []struct {
	name, status, chart, version string
	annotations                  map[string]string
}{
	{"my-up", "deployed", "up-chart", "1.2.0", sourceRepoAnnotations("rancher-charts")},
	{"failed-app", "failed", "up-chart", "1.2.0", sourceRepoAnnotations("rancher-charts")},
	{"superseded-app", "superseded", "up-chart", "1.2.0", sourceRepoAnnotations("rancher-charts")},
	{"uninstalled-app", "uninstalled", "up-chart", "1.2.0", sourceRepoAnnotations("rancher-charts")},
	{"uninstalling-app", "uninstalling", "up-chart", "1.2.0", sourceRepoAnnotations("rancher-charts")},
	{"pending-install-app", "pending-install", "up-chart", "1.2.0", sourceRepoAnnotations("rancher-charts")},
	{"pending-upgrade-app", "pending-upgrade", "up-chart", "1.2.0", sourceRepoAnnotations("rancher-charts")},
	{"pending-rollback-app", "pending-rollback", "up-chart", "1.2.0", sourceRepoAnnotations("rancher-charts")},
	{"unknown-app", "unknown", "up-chart", "1.2.0", sourceRepoAnnotations("rancher-charts")},
	{"blank-status-app", "", "up-chart", "1.2.0", sourceRepoAnnotations("rancher-charts")},
	{"no-source-app", "deployed", "up-chart", "1.2.0", nil},
	{"git-source-app", "deployed", "up-chart", "1.2.0", map[string]string{
		"catalog.cattle.io/ui-source-repo-type": "git", "catalog.cattle.io/ui-source-repo": "rancher-charts"}},
	{"disabled-source-app", "deployed", "up-chart", "1.2.0", sourceRepoAnnotations("disabled-charts")},
	{"absent-source-app", "deployed", "up-chart", "1.2.0", sourceRepoAnnotations("gone-charts")},
	{"default-enabled-source-app", "deployed", "up-chart", "1.2.0", sourceRepoAnnotations("no-enabled-field")},
	{"auto-app", "deployed", "up-chart", "2.0.0", withAnnotation(sourceRepoAnnotations("rancher-charts"), "catalog.cattle.io/auto-install", "crd-chart=1.0.0")},
	{"auto-gvr-app", "deployed", "up-chart", "1.9.0", withAnnotation(sourceRepoAnnotations("rancher-charts"), "catalog.cattle.io/auto-install-gvr", "things.example.io")},
	{"other-chart-app", "deployed", "other-chart", "1.2.0", sourceRepoAnnotations("rancher-charts")},
	{"kept-ns-app", "deployed", "up-chart", "1.8.0", withAnnotation(sourceRepoAnnotations("rancher-charts"), "catalog.cattle.io/namespace", "cattle-x")},
	{"unlisted-auto-app", "deployed", "up-chart", "0.2.0", withAnnotation(sourceRepoAnnotations("rancher-charts"), "catalog.cattle.io/auto-install", "crd-chart=1.0.0")},
}

func withAnnotation(m map[string]string, key, value string) map[string]string {
	m[key] = value
	return m
}

// newAppUpgradeServer serves the app upgrade sites: the Apps of upgradeApps,
// each also as an object, the ClusterRepos (one enabled, one with no
// spec.enabled, one disabled, whose index is served too, to show it is not
// read) and the index of rancher-charts.
func newAppUpgradeServer(t *testing.T) *pathCountingServer {
	t.Helper()
	bodies := map[string]string{
		downRepos:                       reposBody,
		downRepoObject:                  `{"metadata":{"name":"rancher-charts"},"spec":{"enabled":true}}`,
		downRepoIndex:                   upgradeChartIndexBody,
		downDisabledRepoObject:          `{"metadata":{"name":"disabled-charts"},"spec":{"enabled":false}}`,
		downDisabledRepoIndex:           upgradeChartIndexBody,
		downRepos + "/no-enabled-field": `{"metadata":{"name":"no-enabled-field"},"spec":{"url":"https://charts.example.test"}}`,
		downNamespacesPath:              items(plain("web")),
	}
	var all []string
	for _, a := range upgradeApps {
		body := upgradeAppBody(a.name, a.status, a.chart, a.version, a.annotations)
		bodies[downUpgradeAppsPath+"/"+a.name] = body
		all = append(all, body)
	}
	bodies[downUpgradeAppsPath] = items(all...)
	return newPathCountingServer(t, bodies, steveClustersPages)
}

// upgradeWords is a line of app upgrade for the release (none when empty) in
// the namespace web of the cluster c-aaa11, then tail.
func upgradeWords(release string, tail ...string) []string {
	words := []string{"app", "upgrade"}
	if release != "" {
		words = append(words, release)
	}
	return append(words, append([]string{"--cluster", "c-aaa11", "--namespace", "web"}, tail...)...)
}

// completeUpgrade runs one TAB on the line and returns the candidates and the
// requests behind them. NOT parallel: the completion fixture mutates env
// globals and the stored config.
func completeUpgrade(t *testing.T, s *pathCountingServer, words []string) ([]string, map[string]int) {
	t.Helper()
	completionFixture(t, s.srv.URL, true)
	s.reset()
	got := runCompletion(t, words, "")
	return got, s.reset()
}

var upgradeReleaseReads = map[string]int{downUpgradeAppsPath: 1, downRepos: 1}

// TestAppUpgradeReleaseCompletion asserts, through the real TAB callback, the
// apps app upgrade offers for a namespace when nothing else is typed: the apps
// whose status Helm can upgrade, whose source is an enabled ClusterRepo and
// whose chart names no chart to install first, from two requests. NOT
// parallel: the completion fixture mutates env globals and the stored config.
func TestAppUpgradeReleaseCompletion(t *testing.T) {
	s := newAppUpgradeServer(t)
	got, hits := completeUpgrade(t, s, upgradeWords(""))
	assert.ElementsMatch(t, []string{"default-enabled-source-app", "failed-app", "kept-ns-app", "my-up", "other-chart-app", "superseded-app"}, got)
	assert.Equal(t, upgradeReleaseReads, hits)
}

// TestAppUpgradeReleaseCompletionLeavesOutARefusedStatus asserts an app is not
// offered for each status the command refuses, and is for each it accepts.
// NOT parallel: the completion fixture mutates env globals and the stored
// config.
func TestAppUpgradeReleaseCompletionLeavesOutARefusedStatus(t *testing.T) {
	s := newAppUpgradeServer(t)
	got, _ := completeUpgrade(t, s, upgradeWords(""))
	for _, name := range []string{"my-up", "failed-app", "superseded-app"} {
		assert.Contains(t, got, name, "an app that is upgradable by its status")
	}
	for _, name := range []string{"uninstalled-app", "uninstalling-app", "pending-install-app", "pending-upgrade-app", "pending-rollback-app", "unknown-app", "blank-status-app"} {
		assert.NotContains(t, got, name, "an app Helm cannot upgrade by its status")
	}
}

// TestAppUpgradeReleaseCompletionNeedsAnEnabledSource asserts, with no --repo
// typed, an app with no ClusterRepo source, one whose source is of another
// type, and one whose ClusterRepo is disabled or absent are not offered, and
// one whose ClusterRepo has no spec.enabled is. NOT parallel: the completion
// fixture mutates env globals and the stored config.
func TestAppUpgradeReleaseCompletionNeedsAnEnabledSource(t *testing.T) {
	s := newAppUpgradeServer(t)
	got, _ := completeUpgrade(t, s, upgradeWords(""))
	assert.Contains(t, got, "default-enabled-source-app")
	for _, name := range []string{"no-source-app", "git-source-app", "disabled-source-app", "absent-source-app"} {
		assert.NotContains(t, got, name)
	}
}

// TestAppUpgradeReleaseCompletionLeavesOutAChartThatNamesAnotherToInstallFirst
// asserts, with no --version typed, an app whose chart has an auto-install or
// auto-install-gvr annotation is not offered, and with --version typed it is,
// because the typed version's annotations need a read beyond the allowance.
// NOT parallel: the completion fixture mutates env globals and the stored
// config.
func TestAppUpgradeReleaseCompletionLeavesOutAChartThatNamesAnotherToInstallFirst(t *testing.T) {
	s := newAppUpgradeServer(t)
	got, _ := completeUpgrade(t, s, upgradeWords(""))
	for _, name := range []string{"auto-app", "auto-gvr-app", "unlisted-auto-app"} {
		assert.NotContains(t, got, name)
	}
	got, hits := completeUpgrade(t, s, upgradeWords("", "--version", "1.0.0"))
	for _, name := range []string{"auto-app", "auto-gvr-app", "unlisted-auto-app"} {
		assert.Contains(t, got, name, "a typed --version is not checked against the app's annotations")
	}
	assert.Equal(t, upgradeReleaseReads, hits)
}

// TestAppUpgradeReleaseCompletionWithAChartTyped asserts a typed --chart
// leaves out the apps of another chart, without a request of its own. NOT
// parallel: the completion fixture mutates env globals and the stored config.
func TestAppUpgradeReleaseCompletionWithAChartTyped(t *testing.T) {
	s := newAppUpgradeServer(t)
	got, hits := completeUpgrade(t, s, upgradeWords("", "--chart", "up-chart"))
	assert.NotContains(t, got, "other-chart-app")
	assert.Contains(t, got, "my-up")
	assert.Equal(t, upgradeReleaseReads, hits)
	got, hits = completeUpgrade(t, s, upgradeWords("", "--chart", "other-chart"))
	assert.Equal(t, []string{"other-chart-app"}, got)
	assert.Equal(t, upgradeReleaseReads, hits)
}

// TestAppUpgradeReleaseCompletionWithARepoTyped asserts a typed --repo that is
// disabled or absent in the list of ClusterRepos offers nothing, and an enabled
// one offers the apps whatever their own source says. NOT parallel: the
// completion fixture mutates env globals and the stored config.
func TestAppUpgradeReleaseCompletionWithARepoTyped(t *testing.T) {
	s := newAppUpgradeServer(t)
	for _, repo := range []string{"disabled-charts", "gone-charts"} {
		got, hits := completeUpgrade(t, s, upgradeWords("", "--repo", repo))
		assert.Empty(t, got, repo)
		assert.Equal(t, upgradeReleaseReads, hits, repo)
	}
	got, hits := completeUpgrade(t, s, upgradeWords("", "--repo", "rancher-charts"))
	assert.Contains(t, got, "no-source-app", "the app's own source is not checked when --repo names the repository")
	assert.Contains(t, got, "my-up")
	assert.NotContains(t, got, "auto-app")
	assert.Equal(t, upgradeReleaseReads, hits)
}

// TestAppUpgradeVersionCompletion asserts the versions offered for an app
// from three requests, the app, its ClusterRepo and the index: the index's
// versions that the command accepts, and the version the app runs, which the
// index does not list. NOT parallel: the completion fixture mutates env
// globals and the stored config.
func TestAppUpgradeVersionCompletion(t *testing.T) {
	s := newAppUpgradeServer(t)
	got, hits := completeUpgrade(t, s, upgradeWords("my-up", "--version"))
	assert.ElementsMatch(t, []string{"1.0.0", "1.2.0", "1.5.0", "1.6.0", "3.0.0"}, got)
	assert.Equal(t, map[string]int{downUpgradeAppPath: 1, downRepoObject: 1, downRepoIndex: 1}, hits)
}

// TestAppUpgradeVersionCompletionOffersTheVersionTheAppRuns asserts the
// version the app runs is offered when the index does not list it, and once
// when it does. NOT parallel: the completion fixture mutates env globals and
// the stored config.
func TestAppUpgradeVersionCompletionOffersTheVersionTheAppRuns(t *testing.T) {
	s := newAppUpgradeServer(t)
	got, _ := completeUpgrade(t, s, upgradeWords("my-up", "--version"))
	assert.Contains(t, got, "1.2.0")
	got, _ = completeUpgrade(t, s, upgradeWords("kept-ns-app", "--version"))
	assert.Equal(t, 1, strings.Count(strings.Join(got, "\n"), "1.8.0"))
}

// TestAppUpgradeVersionCompletionChecksTheAnnotationsOfAnotherVersion asserts
// a version other than the one the app runs is not offered when it names a
// chart to install first (either annotation), has a fixed namespace of another
// or a fixed release name of another, and is offered when the fixed namespace
// or release name is the app's. NOT parallel: the completion fixture mutates
// env globals and the stored config.
func TestAppUpgradeVersionCompletionChecksTheAnnotationsOfAnotherVersion(t *testing.T) {
	s := newAppUpgradeServer(t)
	got, _ := completeUpgrade(t, s, upgradeWords("my-up", "--version"))
	tests := []struct {
		version string
		offered bool
		why     string
	}{
		{"2.0.0", false, "auto-install"},
		{"1.9.0", false, "auto-install-gvr"},
		{"1.8.0", false, "a namespace annotation that differs from --namespace"},
		{"1.7.0", false, "a release-name annotation that differs from the release"},
		{"1.6.0", true, "a namespace annotation equal to --namespace"},
		{"1.5.0", true, "a release-name annotation equal to the release"},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			if tt.offered {
				assert.Contains(t, got, tt.version, tt.why)
			} else {
				assert.NotContains(t, got, tt.version, tt.why)
			}
		})
	}
}

// TestAppUpgradeVersionCompletionExemptsTheVersionTheAppRuns asserts the
// version the app runs is offered although its namespace annotation differs
// from --namespace, and that it is left out when it names a chart to install
// first. NOT parallel: the completion fixture mutates env globals and the
// stored config.
func TestAppUpgradeVersionCompletionExemptsTheVersionTheAppRuns(t *testing.T) {
	s := newAppUpgradeServer(t)
	got, _ := completeUpgrade(t, s, upgradeWords("kept-ns-app", "--version"))
	assert.Contains(t, got, "1.8.0")
	assert.NotContains(t, got, "1.5.0", "another version with a release-name annotation that differs from the release")
	got, _ = completeUpgrade(t, s, upgradeWords("unlisted-auto-app", "--version"))
	assert.NotContains(t, got, "0.2.0")
	assert.Contains(t, got, "3.0.0")
}

// TestAppUpgradeVersionCompletionOffersNothingItsCommandRefuses asserts no
// versions for a typed --repo that is disabled or absent (after the app and
// the ClusterRepo, without reading the index), for a typed --chart that is not
// the app's chart and for an app with a status Helm cannot upgrade (after the
// app alone). NOT parallel: the completion fixture mutates env globals and the
// stored config.
func TestAppUpgradeVersionCompletionOffersNothingItsCommandRefuses(t *testing.T) {
	s := newAppUpgradeServer(t)
	got, hits := completeUpgrade(t, s, upgradeWords("my-up", "--repo", "disabled-charts", "--version"))
	assert.Empty(t, got)
	assert.Equal(t, map[string]int{downUpgradeAppPath: 1, downDisabledRepoObject: 1}, hits)
	got, hits = completeUpgrade(t, s, upgradeWords("my-up", "--repo", "gone-charts", "--version"))
	assert.Empty(t, got)
	assert.Equal(t, map[string]int{downUpgradeAppPath: 1, downRepos + "/gone-charts": 1}, hits)
	got, hits = completeUpgrade(t, s, upgradeWords("my-up", "--chart", "other-chart", "--version"))
	assert.Empty(t, got)
	assert.Equal(t, map[string]int{downUpgradeAppPath: 1}, hits)
	got, hits = completeUpgrade(t, s, upgradeWords("uninstalled-app", "--version"))
	assert.Empty(t, got)
	assert.Equal(t, map[string]int{downUpgradeAppsPath + "/uninstalled-app": 1}, hits)
	got, hits = completeUpgrade(t, s, upgradeWords("no-source-app", "--version"))
	assert.Empty(t, got, "no source ClusterRepo and no --repo typed")
	assert.Equal(t, map[string]int{downUpgradeAppsPath + "/no-source-app": 1}, hits)
}

// TestAppUpgradeVersionCompletionUsesTheTypedRepo asserts a typed --repo
// replaces the app's source ClusterRepo as the index to read. NOT parallel:
// the completion fixture mutates env globals and the stored config.
func TestAppUpgradeVersionCompletionUsesTheTypedRepo(t *testing.T) {
	s := newAppUpgradeServer(t)
	got, hits := completeUpgrade(t, s, upgradeWords("disabled-source-app", "--repo", "rancher-charts", "--version"))
	assert.Contains(t, got, "3.0.0")
	assert.Equal(t, map[string]int{downUpgradeAppsPath + "/disabled-source-app": 1, downRepoObject: 1, downRepoIndex: 1}, hits)
}

// TestAppUpgradeRepoCompletion asserts --repo offers the app's source
// ClusterRepo alone, from two requests, and nothing for an app with no
// ClusterRepo source, a disabled or absent one, a typed --chart that is not
// the app's chart or an app with a status Helm cannot upgrade. NOT parallel:
// the completion fixture mutates env globals and the stored config.
func TestAppUpgradeRepoCompletion(t *testing.T) {
	s := newAppUpgradeServer(t)
	got, hits := completeUpgrade(t, s, upgradeWords("my-up", "--repo"))
	assert.Equal(t, []string{"rancher-charts"}, got)
	assert.Equal(t, map[string]int{downUpgradeAppPath: 1, downRepoObject: 1}, hits)
	got, _ = completeUpgrade(t, s, upgradeWords("default-enabled-source-app", "--repo"))
	assert.Equal(t, []string{"no-enabled-field"}, got, "a ClusterRepo with no spec.enabled is enabled")
	for _, name := range []string{"no-source-app", "git-source-app", "disabled-source-app", "absent-source-app", "uninstalled-app"} {
		got, _ = completeUpgrade(t, s, upgradeWords(name, "--repo"))
		assert.Empty(t, got, name)
	}
	got, _ = completeUpgrade(t, s, upgradeWords("my-up", "--chart", "other-chart", "--repo"))
	assert.Empty(t, got)
}

// TestAppUpgradeChartCompletion asserts --chart offers exactly the app's
// chart from one request, and nothing when the app cannot be read or has a
// status Helm cannot upgrade. NOT parallel: the completion fixture mutates env
// globals and the stored config.
func TestAppUpgradeChartCompletion(t *testing.T) {
	s := newAppUpgradeServer(t)
	got, hits := completeUpgrade(t, s, upgradeWords("my-up", "--chart"))
	assert.Equal(t, []string{"up-chart"}, got, "the app's chart, not the repository's other charts")
	assert.Equal(t, map[string]int{downUpgradeAppPath: 1}, hits)
	got, _ = completeUpgrade(t, s, upgradeWords("other-chart-app", "--chart"))
	assert.Equal(t, []string{"other-chart"}, got)
	got, hits = completeUpgrade(t, s, upgradeWords("no-such-app", "--chart"))
	assert.Empty(t, got)
	assert.Equal(t, map[string]int{downUpgradeAppsPath + "/no-such-app": 1}, hits)
	got, _ = completeUpgrade(t, s, upgradeWords("uninstalled-app", "--chart"))
	assert.Empty(t, got)
	got, hits = completeUpgrade(t, s, upgradeWords("", "--chart"))
	assert.Empty(t, got, "no release typed yet")
	assert.Empty(t, hits)
}
