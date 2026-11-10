package cli

import (
	"maps"
	"slices"
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

// addAppCompletionBodies adds what the app install and app uninstall sites
// read to a parity server's bodies: the ClusterRepos and indexes, the apps and
// the downstream namespaces.
func addAppCompletionBodies(bodies map[string]string) map[string]string {
	bodies[downRepoObject] = `{"metadata":{"name":"rancher-charts"},"spec":{"enabled":true}}`
	bodies[downRepoIndex] = chartIndexBody
	bodies[downDisabledRepoObject] = `{"metadata":{"name":"disabled-charts"},"spec":{"enabled":false}}`
	bodies[downDisabledRepoIndex] = chartIndexBody
	bodies[localRepoObject] = `{"metadata":{"name":"rancher-charts"},"spec":{"enabled":true}}`
	bodies[localRepoIndex] = chartIndexBody
	bodies[downAppsPath] = items(plain("my-app"), plain("other-app"))
	bodies[downNamespacesPath] = items(plain("kube-system"), plain("default"))
	return bodies
}

// appChartExceptionReason is the one reason --chart carries beside a live
// source.
const appChartExceptionReason = "the index does not show global.cattle, requires-gvr or permits-os: a chart refused for one of them may be offered"

// appCompletionSite is one value-taking flag or argument of app install or
// app uninstall with how it completes: "live" (a source answers it, and words
// is a line whose TAB offers names), "file" (the shell completes files, so the
// surface offers nothing) or "none" (a value no list can offer). A "none" row
// and the row marked as an exception say why in reason.
type appCompletionSite struct {
	completion string
	reason     string
	words      []string
}

// appCompletionSites lists every value-taking flag and argument of the two
// commands. A flag or argument added to either command with no row fails
// TestEveryValueOfAppInstallAndUninstallHasACompletion.
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
	"app install --helm-timeout": {completion: "none", reason: "a free duration, as --timeout", words: []string{"app", "install", "my-rel", "--cluster", "c-aaa11", "--helm-timeout"}},
	"app install --description":  {completion: "none", reason: "free text", words: []string{"app", "install", "my-rel", "--cluster", "c-aaa11", "--description"}},

	"app uninstall <release>":      {completion: "live", words: []string{"app", "uninstall", "--cluster", "c-aaa11", "--namespace", "default"}},
	"app uninstall --cluster":      {completion: "live", words: []string{"app", "uninstall", "my-app", "--cluster"}},
	"app uninstall --namespace":    {completion: "live", words: []string{"app", "uninstall", "my-app", "--cluster", "c-aaa11", "--namespace"}},
	"app uninstall --helm-timeout": {completion: "none", reason: "a free duration, as --timeout", words: []string{"app", "uninstall", "my-app", "--cluster", "c-aaa11", "--helm-timeout"}},
}

// TestEveryValueOfAppInstallAndUninstallHasACompletion asserts every
// value-taking flag and argument of app install and app uninstall has a row in
// appCompletionSites and no row names one that is gone, that a row says why
// when it offers nothing and the only live row with a reason is app install
// --chart, and that through the real TAB callback a live row offers names, and
// a file or none row offers nothing and sends no request. NOT parallel: the
// completion fixture mutates env globals and the stored config.
func TestEveryValueOfAppInstallAndUninstallHasACompletion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)

	declared := map[string]bool{}
	for _, cmd := range wr.reg.Commands() {
		d := cmd.Describe()
		if d.Name != "app install" && d.Name != "app uninstall" {
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
	require.NotEmpty(t, declared, "the two commands are registered")
	assert.ElementsMatch(t, slices.Collect(maps.Keys(declared)), slices.Collect(maps.Keys(appCompletionSites)),
		"every value-taking flag and argument has a row, and every row names one")

	s := newCommandSitesServer(t)
	for _, key := range slices.Sorted(maps.Keys(appCompletionSites)) {
		site := appCompletionSites[key]
		t.Run(key, func(t *testing.T) {
			switch site.completion {
			case "none":
				assert.NotEmpty(t, site.reason, "a flag that offers nothing says why")
			case "file", "live":
				if key == "app install --chart" {
					assert.Equal(t, appChartExceptionReason, site.reason)
				} else {
					assert.Empty(t, site.reason, "only app install --chart carries a reason beside a live source")
				}
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
