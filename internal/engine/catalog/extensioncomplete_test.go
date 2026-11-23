package catalog

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
	catalogcomplete "github.com/rancher/rancher-cli/internal/engine/catalog/complete"
	"github.com/rancher/rancher-cli/internal/engine/catalog/extension"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
)

const (
	extInstallName    = "extension install"
	extUIPluginsPath  = "/k8s/clusters/local/apis/catalog.cattle.io/v1/uiplugins"
	extAppsPath       = "/k8s/clusters/local/apis/catalog.cattle.io/v1/namespaces/" + extUIPlugin + "/apps"
	extIncompatible   = `"catalog.cattle.io/ui-component":"plugins","catalog.cattle.io/ui-extensions-version":">= 9.0.0"`
	extBundleAnnot    = `,"` + extension.AnnotationBundleMembers + `":"one,two"`
	extPartnerAddress = "https://github.com/rancher/partner-extensions"
)

// extVersion is one version of a chart in a test index.
type extVersion struct {
	version, appVersion, annotations string
}

func okVersion(v string) extVersion { return extVersion{version: v, annotations: extCompatible} }

// extIndex is an index of charts, each with its versions newest first.
func extIndex(charts map[string][]extVersion) string {
	var parts []string
	for _, name := range slices.Sorted(maps.Keys(charts)) {
		var entries []string
		for _, v := range charts[name] {
			app := v.appVersion
			if app == "" {
				app = v.version
			}
			entries = append(entries, fmt.Sprintf(`{"name":%q,"version":%q,"appVersion":%q,"annotations":{%s}}`, name, v.version, app, v.annotations))
		}
		parts = append(parts, fmt.Sprintf("%q:[%s]", name, strings.Join(entries, ",")))
	}
	return `{"entries":{` + strings.Join(parts, ",") + `}}`
}

// extWorld is what the install command and its completion source both read.
type extWorld struct {
	repoItems []string
	indexes   map[string]string
	apps      []string
	plugins   []string
}

// fake is the world as the plan sees it.
func (w extWorld) fake(t *testing.T) *extFake {
	t.Helper()
	f := newExtFake(t)
	f.root.answers[reposPath] = rootAnswer{status: 200, body: collectionBody(w.repoItems...)}
	delete(f.root.answers, extIndexPath)
	for repo, body := range w.indexes {
		f.root.answers[reposPath+"/"+repo+"?link=index"] = rootAnswer{status: 200, body: body}
	}
	for _, name := range w.apps {
		f.put("apps", extUIPlugin, name, map[string]any{"spec": map[string]any{"info": map[string]any{"status": "deployed"}}})
	}
	for _, name := range w.plugins {
		f.put("uiplugins", "other-ns", "dev-"+name, map[string]any{"spec": map[string]any{"plugin": map[string]any{"name": name, "version": "0.1.0"}}})
	}
	return f
}

// server is the world as the completion source sees it.
func (w extWorld) server(t *testing.T) *completiontest.Server {
	t.Helper()
	return completiontest.New(t, w.bodies())
}

// bodies are the responses of the world by request path.
func (w extWorld) bodies() map[string]string {
	bodies := map[string]string{
		"/rancherversion": `{"Version":"v2.15.3","RancherPrime":"false"}`,
		localCluster:      clusterBody("v1.36.5+k3s1", "v1.36.5+k3s1"),
		reposPath:         collectionBody(w.repoItems...),
	}
	for repo, body := range w.indexes {
		bodies[reposPath+"/"+repo] = body
	}
	var plugins, apps []string
	for _, name := range w.plugins {
		plugins = append(plugins, `{"spec":{"plugin":{"name":"`+name+`"}}}`)
	}
	for _, name := range w.apps {
		apps = append(apps, `{"metadata":{"name":"`+name+`"}}`)
	}
	bodies[extUIPluginsPath] = `{"items":[` + strings.Join(plugins, ",") + `]}`
	bodies[extAppsPath] = `{"items":[` + strings.Join(apps, ",") + `]}`
	return bodies
}

// serverEndingContext serves bodies by request path and ends the returned
// context when the request for endAt arrives, as the time limit of a TAB ends
// during a read.
func serverEndingContext(t *testing.T, bodies map[string]string, endAt string) (*completiontest.Server, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	srv := completiontest.NewByAccept(t, func(path, _ string) (string, int) {
		if path == endAt {
			cancel()
		}
		body, ok := bodies[path]
		if !ok {
			return "", http.StatusNotFound
		}
		return body, http.StatusOK
	})
	return srv, ctx
}

// accepts reports whether the plan of the install accepts the chart with the
// flags.
func (w extWorld) accepts(t *testing.T, chart string, flags ...string) bool {
	t.Helper()
	_, _, err := newTestExtensionInstall(w.fake(t)).Plan(t.Context(), extInput(chart, flags...))
	return err == nil
}

func offered(t *testing.T, w extWorld, req completion.Request) []string {
	t.Helper()
	req.Command = extInstallName
	src := catalogcomplete.ExtensionInstallSource(extInstallName)
	_, claimed := src.Claim(req)
	require.True(t, claimed, "the source claims %+v", req)
	return src.Complete(t.Context(), w.server(t).Conn(5+completion.ExtensionRepositoryLimit), req).Names
}

// extMixedWorld has a case for every rule of the install: an installable
// chart, an App, a UIPlugin with no App, a chart with no compatible version,
// a newest version that fails a check, two repositories that offer a chart, a
// bundle, a chart that is no extension, a hidden extension, a chart whose
// application version differs from its version, and repositories the Rancher
// UI does not read (disabled, hidden, still downloading).
func extMixedWorld() extWorld {
	partner := extIndex(map[string][]extVersion{
		"good":          {okVersion("2.0.0"), okVersion("1.0.0")},
		"installed-app": {okVersion("1.0.0")},
		"loaded":        {okVersion("1.0.0")},
		"old-only":      {{version: "1.0.0", annotations: extIncompatible}},
		"newer-bad":     {{version: "3.0.0", annotations: extIncompatible}, okVersion("2.0.0")},
		"twice":         {okVersion("1.0.0")},
		"bundle":        {{version: "1.0.0", annotations: extCompatible + extBundleAnnot}},
		"plain":         {{version: "1.0.0", annotations: `"catalog.cattle.io/display-name":"Plain"`}},
		"hidden-ext":    {{version: "1.0.0", annotations: extCompatible + `,"catalog.cattle.io/hidden":"true"`}},
		"appver":        {{version: "1.2.3", appVersion: "9.9.9", annotations: extCompatible}},
	})
	mirror := extIndex(map[string][]extVersion{
		"twice":       {okVersion("1.0.0"), okVersion("0.9.0")},
		"mirror-only": {okVersion("1.0.0")},
	})
	other := extIndex(map[string][]extVersion{"unseen": {okVersion("1.0.0")}})
	return extWorld{
		repoItems: []string{
			repoItemAt(extRepo, "active", extPartnerAddress),
			repoItemAt("mirror", "active", "https://example.test/mirror"),
			`{"metadata":{"name":"off","state":{"name":"active"}},"spec":{"enabled":false}}`,
			repoItemBody("quiet", "active", `,"annotations":{"clusterrepo.cattle.io/hidden":"true"}`),
			repoItemAt("slow", "downloading", "https://example.test/slow"),
		},
		indexes: map[string]string{extRepo: partner, "mirror": mirror, "off": other, "quiet": other, "slow": other},
		apps:    []string{"installed-app"},
		plugins: []string{"loaded"},
	}
}

// TestExtensionInstallAcceptsExactlyWhatCompletionOffers asserts, on one
// world, that each chart name, repository and version the completion source
// offers is accepted by the plan of `extension install`, and that every
// other value of the world is refused: an App, a UIPlugin with no App, a
// chart with no compatible version, two repositories without --repo, a
// bundle, a chart that is no extension, a hidden extension, repositories the
// Rancher UI does not read and a version that fails a check.
func TestExtensionInstallAcceptsExactlyWhatCompletionOffers(t *testing.T) {
	t.Parallel()
	w := extMixedWorld()
	charts := []string{"good", "installed-app", "loaded", "old-only", "newer-bad", "twice", "bundle", "plain", "hidden-ext", "appver", "mirror-only", "unseen", "ghost"}
	repos := []string{extRepo, "mirror", "off", "quiet", "slow", "nope"}

	t.Run("chart names", func(t *testing.T) {
		t.Parallel()
		got := offered(t, w, completion.Request{})
		assert.ElementsMatch(t, []string{"good", "newer-bad", "appver", "mirror-only"}, got)
		for _, chart := range charts {
			assert.Equal(t, slices.Contains(got, chart), w.accepts(t, chart), "chart %s", chart)
		}
	})

	t.Run("chart names with a typed repository", func(t *testing.T) {
		t.Parallel()
		got := offered(t, w, completion.Request{Tail: []string{"--repo", "mirror"}})
		assert.ElementsMatch(t, []string{"twice", "mirror-only"}, got)
		for _, chart := range charts {
			assert.Equal(t, slices.Contains(got, chart), w.accepts(t, chart, flagRepo, "mirror"), "chart %s", chart)
		}
	})

	t.Run("repositories", func(t *testing.T) {
		t.Parallel()
		want := map[string][]string{
			"good": {extRepo}, "newer-bad": {extRepo}, "appver": {extRepo}, "mirror-only": {"mirror"}, "twice": {extRepo, "mirror"},
		}
		for _, chart := range charts {
			got := offered(t, w, completion.Request{Flag: flagRepo, Positionals: []string{chart}})
			assert.ElementsMatch(t, want[chart], got, "chart %s", chart)
			for _, repo := range repos {
				assert.Equal(t, slices.Contains(got, repo), w.accepts(t, chart, flagRepo, repo), "chart %s repository %s", chart, repo)
			}
		}
	})

	t.Run("versions", func(t *testing.T) {
		t.Parallel()
		type site struct {
			chart, repo string
			want        []string
		}
		for _, s := range []site{
			{"good", "", []string{"2.0.0", "1.0.0"}},
			{"newer-bad", "", []string{"2.0.0"}},
			{"appver", "", []string{"1.2.3"}},
			{"twice", "", nil},
			{"twice", "mirror", []string{"1.0.0", "0.9.0"}},
			{"twice", extRepo, []string{"1.0.0"}},
			{"installed-app", "", nil},
			{"loaded", "", nil},
			{"old-only", "", nil},
			{"bundle", "", nil},
			{"plain", "", nil},
			{"ghost", "", nil},
		} {
			req := completion.Request{Flag: flagVersion, Positionals: []string{s.chart}}
			var flags []string
			if s.repo != "" {
				req.Tail = []string{"--repo", s.repo}
				flags = []string{flagRepo, s.repo}
			}
			got := offered(t, w, req)
			assert.ElementsMatch(t, s.want, got, "chart %s repository %q", s.chart, s.repo)
			for _, v := range []string{"3.0.0", "2.0.0", "1.2.3", "1.0.0", "0.9.0", "9.9.9"} {
				assert.Equal(t, slices.Contains(got, v), w.accepts(t, s.chart, append([]string{flagVersion, v}, flags...)...), "chart %s repository %q version %s", s.chart, s.repo, v)
			}
		}
	})
}

// TestExtensionInstallCompletionOffersNothingWhenTheContextEnds asserts that
// a context that ends during the index read of the second repository offers
// no name: the chart "twice" is in both repositories; a name list built
// from the first index alone would offer a chart the command refuses without
// --repo.
func TestExtensionInstallCompletionOffersNothingWhenTheContextEnds(t *testing.T) {
	t.Parallel()
	w := extWorld{
		repoItems: []string{repoItemAt("alpha", "active", "https://example.test/alpha"), repoItemAt("beta", "active", "https://example.test/beta")},
		indexes: map[string]string{
			"alpha": extIndex(map[string][]extVersion{"solo": {okVersion("1.0.0")}, "twice": {okVersion("1.0.0")}}),
			"beta":  extIndex(map[string][]extVersion{"twice": {okVersion("1.0.0")}}),
		},
	}
	assert.ElementsMatch(t, []string{"solo"}, offered(t, w, completion.Request{}), "with the time to read both indexes, a chart of two repositories is left out")
	srv, ctx := serverEndingContext(t, w.bodies(), reposPath+"/beta")
	req := completion.Request{Command: extInstallName}
	got := catalogcomplete.ExtensionInstallSource(extInstallName).Complete(ctx, srv.Conn(5+completion.ExtensionRepositoryLimit), req)
	assert.Empty(t, got.Names)
	require.NotEmpty(t, srv.Requested())
	assert.Equal(t, reposPath+"/beta?link=index", srv.Requested()[len(srv.Requested())-1], "the context ended during the read of the second index")
}

// TestExtensionRepositoryLimitValue asserts the number of ClusterRepos the
// extension completion sources read. Each one adds a sequential index read to
// a TAB; a change to the number must change this test too.
func TestExtensionRepositoryLimitValue(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 8, completion.ExtensionRepositoryLimit)
}

// TestExtensionInstallCompletionOffersNothingBeyondTheBound asserts that a
// server where the Rancher UI reads more ClusterRepos than the bound offers
// no name, and that the same server within the bound offers them.
func TestExtensionInstallCompletionOffersNothingBeyondTheBound(t *testing.T) {
	t.Parallel()
	build := func(n int) extWorld {
		w := extWorld{indexes: map[string]string{}}
		for i := range n {
			name := fmt.Sprintf("repo-%02d", i)
			w.repoItems = append(w.repoItems, repoItemAt(name, "active", "https://example.test/"+name))
			w.indexes[name] = extIndex(map[string][]extVersion{"chart-" + name: {okVersion("1.0.0")}})
		}
		return w
	}
	within := build(completion.ExtensionRepositoryLimit)
	assert.Len(t, offered(t, within, completion.Request{}), completion.ExtensionRepositoryLimit)
	beyond := build(completion.ExtensionRepositoryLimit + 1)
	assert.Empty(t, offered(t, beyond, completion.Request{}))
	assert.Empty(t, offered(t, beyond, completion.Request{Flag: flagRepo, Positionals: []string{"chart-repo-00"}}))
	assert.Equal(t, []string{"chart-repo-00"}, offered(t, beyond, completion.Request{Tail: []string{"--repo", "repo-00"}}), "a typed repository is read alone")
}

// TestExtensionInstallCompletionFitsTheTimeLimitOnLargeIndexes asserts that
// the offered names arrive inside the default time limit of one TAB when the
// indexes of the repositories are as large as the ones of a default server,
// and that the source stays within its request allowance. The race detector
// slows the decode of the indexes several times over: a build with it uses
// indexes a quarter of the size and the maximum time limit, because at the full
// size the source passes even that limit and offers nothing.
func TestExtensionInstallCompletionFitsTheTimeLimitOnLargeIndexes(t *testing.T) {
	large := func(prefix string, charts int) string {
		entries := map[string][]extVersion{}
		pad := strings.Repeat("x", 200)
		for i := range charts {
			var versions []extVersion
			for j := range 10 {
				versions = append(versions, extVersion{version: fmt.Sprintf("1.%d.0", j), annotations: `"catalog.cattle.io/display-name":"` + pad + `","catalog.cattle.io/kube-version":">= 1.20.0-0"`})
			}
			entries[fmt.Sprintf("%s-%04d", prefix, i)] = versions
		}
		return extIndex(entries)
	}
	charts, sizeFloor, limit := 1000, 5<<20, config.CompletionTimeoutDefault
	if raceDetectorOn() {
		charts, sizeFloor, limit = 250, 5<<18, config.CompletionTimeoutMax
	}
	w := extWorld{
		repoItems: []string{repoItemAt("rancher-charts", "active", "https://git.rancher.io/charts"), repoItemAt("rancher-partner-charts", "active", "https://git.rancher.io/partner-charts"), repoItemAt(extRepo, "active", extPartnerAddress)},
		indexes: map[string]string{
			"rancher-charts":         large("charts", charts),
			"rancher-partner-charts": large("partner", charts),
			extRepo:                  extIndex(map[string][]extVersion{"epinio": {okVersion("1.14.3"), okVersion("1.14.2")}}),
		},
	}
	size := 0
	for _, body := range w.indexes {
		size += len(body)
	}
	require.Greater(t, size, sizeFloor, "the fixture is as large as the indexes of a default server, or a quarter of that under the race detector")
	srv := w.server(t)
	conn := srv.Conn(5 + completion.ExtensionRepositoryLimit)
	ctx, cancel := context.WithTimeout(t.Context(), limit)
	defer cancel()
	start := time.Now()
	got := catalogcomplete.ExtensionInstallSource(extInstallName).Complete(ctx, conn, completion.Request{Command: extInstallName})
	elapsed := time.Since(start)
	t.Logf("offered %d names from %d bytes of indexes in %s (limit %s)", len(got.Names), size, elapsed, limit)
	assert.Equal(t, []string{"epinio"}, got.Names)
	assert.False(t, conn.Exceeded())
	assert.Less(t, elapsed, limit)
}

// raceDetectorOn reports whether the test binary was built with -race.
func raceDetectorOn() bool {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return false
	}
	return slices.ContainsFunc(info.Settings, func(s debug.BuildSetting) bool { return s.Key == "-race" && s.Value == "true" })
}
