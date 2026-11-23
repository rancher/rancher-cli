package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	extLocalVersionPath = "/rancherversion"
	extLocalClusterPath = "/v1/management.cattle.io.clusters/local"
	extRepoListPath     = "/v1/catalog.cattle.io.clusterrepos"
	extRepoIndexPath    = extRepoListPath + "/partner-extensions"
	extPluginsPath      = "/k8s/clusters/local/apis/catalog.cattle.io/v1/uiplugins"
	extAppsListPath     = "/k8s/clusters/local/apis/catalog.cattle.io/v1/namespaces/cattle-ui-plugin-system/apps"
)

const extCompatibleAnnotations = `"catalog.cattle.io/ui-component":"plugins","catalog.cattle.io/ui-extensions-version":">= 3.0.0"`

// newExtensionSitesServer serves what the extension install sites read: a
// community server at v2.15.3, the local cluster, one active repository whose
// index offers the extension epinio in two versions, no UIPlugin and no App.
func newExtensionSitesServer(t *testing.T) *pathCountingServer {
	t.Helper()
	index := `{"entries":{"epinio":[` +
		`{"name":"epinio","version":"1.14.3","annotations":{` + extCompatibleAnnotations + `}},` +
		`{"name":"epinio","version":"1.14.2","annotations":{` + extCompatibleAnnotations + `}}]}}`
	return newPathCountingServer(t, map[string]string{
		extLocalVersionPath: `{"Version":"v2.15.3","RancherPrime":"false"}`,
		extLocalClusterPath: `{"status":{"info":{"kubernetesVersion":"v1.36.5+k3s1"}}}`,
		extRepoListPath:     steveItems(`{"metadata":{"name":"partner-extensions","state":{"name":"active"}},"spec":{"gitRepo":"https://github.com/rancher/partner-extensions"}}`),
		extRepoIndexPath:    index,
		extPluginsPath:      items(),
		extAppsListPath:     items(),
	}, nil)
}

// TestExtensionInstallSitesFullCallback asserts, through the real TAB
// callback, the candidates and the requests of every completion site of
// `extension install`: the chart name, --repo and --version (the version
// document, the local cluster, the ClusterRepos, the UIPlugins, the Apps and
// the index, each read once, the typed repository's index alone) and
// --timeout, which offers nothing. NOT parallel: t.Setenv mutates env
// globals, setStoredConfig mutates storedConfig.
func TestExtensionInstallSitesFullCallback(t *testing.T) {
	s := newExtensionSitesServer(t)
	reads := map[string]int{extLocalVersionPath: 1, extLocalClusterPath: 1, extRepoListPath: 1, extPluginsPath: 1, extAppsListPath: 1, extRepoIndexPath: 1}
	tests := []struct {
		name  string
		words []string
		cur   string
		want  []string
		hits  map[string]int
		noNet bool
	}{
		{name: "chart name", words: []string{"extension", "install"}, want: []string{"epinio"}, hits: reads},
		{name: "chart name with a partial", words: []string{"extension", "install"}, cur: "ep", want: []string{"epinio"}, hits: reads},
		{name: "chart name with a typed repository", words: []string{"extension", "install", "--repo", "partner-extensions"}, want: []string{"epinio"}, hits: reads},
		{name: "chart name with the network off", words: []string{"extension", "install"}, noNet: true, hits: map[string]int{}},
		{name: "--repo of the typed chart", words: []string{"extension", "install", "epinio", "--repo"}, want: []string{"partner-extensions"}, hits: reads},
		{name: "--repo= equals form", words: []string{"extension", "install", "epinio"}, cur: "--repo=", want: []string{"--repo=partner-extensions"}, hits: reads},
		{name: "--repo of a chart no repository offers", words: []string{"extension", "install", "ghost", "--repo"}, hits: reads},
		{name: "--repo without a chart offers no names", words: []string{"extension", "install", "--repo"}, hits: map[string]int{}},
		{name: "--repo with the network off", words: []string{"extension", "install", "epinio", "--repo"}, noNet: true, hits: map[string]int{}},
		{name: "--version of the typed chart", words: []string{"extension", "install", "epinio", "--version"}, want: []string{"1.14.2", "1.14.3"}, hits: reads},
		{name: "--version= equals form", words: []string{"extension", "install", "epinio"}, cur: "--version=", want: []string{"--version=1.14.2", "--version=1.14.3"}, hits: reads},
		{name: "--version with a typed repository", words: []string{"extension", "install", "epinio", "--repo", "partner-extensions", "--version"}, want: []string{"1.14.2", "1.14.3"}, hits: reads},
		{name: "--version with a repository that does not exist", words: []string{"extension", "install", "epinio", "--repo", "nope", "--version"}, hits: map[string]int{extLocalVersionPath: 1, extLocalClusterPath: 1, extRepoListPath: 1, extPluginsPath: 1, extAppsListPath: 1}},
		{name: "--version of a chart no repository offers", words: []string{"extension", "install", "ghost", "--version"}, hits: reads},
		{name: "--version without a chart offers no names", words: []string{"extension", "install", "--version"}, hits: map[string]int{}},
		{name: "--version with the network off", words: []string{"extension", "install", "epinio", "--version"}, noNet: true, hits: map[string]int{}},
		{name: "--timeout is a free duration", words: []string{"extension", "install", "epinio", "--timeout"}, hits: map[string]int{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completionFixture(t, s.srv.URL, !tt.noNet)
			s.reset()
			got := runCompletion(t, tt.words, tt.cur)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.hits, s.reset())
		})
	}
}

const extAppPath = extAppsListPath + "/epinio"

// newExtensionUpgradeSitesServer serves what the extension upgrade sites read:
// the server of newExtensionSitesServer with the extension epinio installed at
// 1.14.2 from the repository.
func newExtensionUpgradeSitesServer(t *testing.T) *pathCountingServer {
	t.Helper()
	index := `{"entries":{"epinio":[` +
		`{"name":"epinio","version":"1.14.3","annotations":{` + extCompatibleAnnotations + `}},` +
		`{"name":"epinio","version":"1.14.2","annotations":{` + extCompatibleAnnotations + `}}]}}`
	app := `{"metadata":{"name":"epinio"},"spec":{"info":{"status":"deployed"},"chart":{"metadata":{"version":"1.14.2",` +
		`"annotations":{"catalog.cattle.io/ui-source-repo":"partner-extensions"}}}}}`
	return newPathCountingServer(t, map[string]string{
		extLocalVersionPath: `{"Version":"v2.15.3","RancherPrime":"false"}`,
		extLocalClusterPath: `{"status":{"info":{"kubernetesVersion":"v1.36.5+k3s1"}}}`,
		extRepoListPath:     steveItems(`{"metadata":{"name":"partner-extensions","state":{"name":"active"}},"spec":{"gitRepo":"https://github.com/rancher/partner-extensions"}}`),
		extRepoIndexPath:    index,
		extAppsListPath:     items(app),
		extAppPath:          app,
	}, nil)
}

// TestExtensionUpgradeSitesFullCallback asserts, through the real TAB
// callback, the candidates and the requests of every completion site of
// `extension upgrade`: the chart name (the Apps, the ClusterRepos, the index of
// the App's repository and the compatibility reads), --repo (the App and the
// ClusterRepos), --version (the same reads as the chart name for one App) and
// --timeout, which offers nothing. NOT parallel: t.Setenv mutates env
// globals, setStoredConfig mutates storedConfig.
func TestExtensionUpgradeSitesFullCallback(t *testing.T) {
	s := newExtensionUpgradeSitesServer(t)
	names := map[string]int{extLocalVersionPath: 1, extLocalClusterPath: 1, extRepoListPath: 1, extAppsListPath: 1, extRepoIndexPath: 1}
	versions := map[string]int{extLocalVersionPath: 1, extLocalClusterPath: 1, extRepoListPath: 1, extAppPath: 1, extRepoIndexPath: 1}
	tests := []struct {
		name  string
		words []string
		cur   string
		want  []string
		hits  map[string]int
		noNet bool
	}{
		{name: "chart name", words: []string{"extension", "upgrade"}, want: []string{"epinio"}, hits: names},
		{name: "chart name with a partial", words: []string{"extension", "upgrade"}, cur: "ep", want: []string{"epinio"}, hits: names},
		{name: "chart name with the typed repository", words: []string{"extension", "upgrade", "--repo", "partner-extensions"}, want: []string{"epinio"}, hits: names},
		{name: "chart name with another repository", words: []string{"extension", "upgrade", "--repo", "other"}, hits: map[string]int{extAppsListPath: 1, extRepoListPath: 1}},
		{name: "chart name with the network off", words: []string{"extension", "upgrade"}, noNet: true, hits: map[string]int{}},
		{name: "--repo of the typed chart", words: []string{"extension", "upgrade", "epinio", "--repo"}, want: []string{"partner-extensions"}, hits: map[string]int{extAppPath: 1, extRepoListPath: 1}},
		{name: "--repo= equals form", words: []string{"extension", "upgrade", "epinio"}, cur: "--repo=", want: []string{"--repo=partner-extensions"}, hits: map[string]int{extAppPath: 1, extRepoListPath: 1}},
		{name: "--repo without a chart offers no names", words: []string{"extension", "upgrade", "--repo"}, hits: map[string]int{}},
		{name: "--repo with the network off", words: []string{"extension", "upgrade", "epinio", "--repo"}, noNet: true, hits: map[string]int{}},
		{name: "--version of the typed chart", words: []string{"extension", "upgrade", "epinio", "--version"}, want: []string{"1.14.3"}, hits: versions},
		{name: "--version= equals form", words: []string{"extension", "upgrade", "epinio"}, cur: "--version=", want: []string{"--version=1.14.3"}, hits: versions},
		{name: "--version with another repository", words: []string{"extension", "upgrade", "epinio", "--repo", "other", "--version"}, hits: map[string]int{extAppPath: 1}},
		{name: "--version without a chart offers no names", words: []string{"extension", "upgrade", "--version"}, hits: map[string]int{}},
		{name: "--version with the network off", words: []string{"extension", "upgrade", "epinio", "--version"}, noNet: true, hits: map[string]int{}},
		{name: "--timeout is a free duration", words: []string{"extension", "upgrade", "epinio", "--timeout"}, hits: map[string]int{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completionFixture(t, s.srv.URL, !tt.noNet)
			s.reset()
			got := runCompletion(t, tt.words, tt.cur)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.hits, s.reset())
		})
	}
}

// TestExtensionUninstallSitesFullCallback asserts, through the real TAB
// callback, the candidates and the requests of every completion site of
// `extension uninstall`: the chart name (one list of the Apps), a second
// positional and --timeout, which offer nothing. NOT parallel: t.Setenv
// mutates env globals, setStoredConfig mutates storedConfig.
func TestExtensionUninstallSitesFullCallback(t *testing.T) {
	s := newExtensionUpgradeSitesServer(t)
	tests := []struct {
		name  string
		words []string
		cur   string
		want  []string
		hits  map[string]int
		noNet bool
	}{
		{name: "chart name", words: []string{"extension", "uninstall"}, want: []string{"epinio"}, hits: map[string]int{extAppsListPath: 1}},
		{name: "chart name with a partial", words: []string{"extension", "uninstall"}, cur: "ep", want: []string{"epinio"}, hits: map[string]int{extAppsListPath: 1}},
		{name: "chart name with the network off", words: []string{"extension", "uninstall"}, noNet: true, hits: map[string]int{}},
		{name: "a second positional offers nothing", words: []string{"extension", "uninstall", "epinio"}, hits: map[string]int{}},
		{name: "--timeout is a free duration", words: []string{"extension", "uninstall", "epinio", "--timeout"}, hits: map[string]int{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completionFixture(t, s.srv.URL, !tt.noNet)
			s.reset()
			got := runCompletion(t, tt.words, tt.cur)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.hits, s.reset())
		})
	}
}
