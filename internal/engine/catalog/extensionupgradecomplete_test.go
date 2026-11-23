package catalog

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	catalogcomplete "github.com/rancher/rancher-cli/internal/engine/catalog/complete"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
)

const extUpgradeName = "extension upgrade"

// extWorldApp is an App of the upgrade world: its name (the chart), status,
// installed chart version and the ClusterRepo it came from ("" for none).
type extWorldApp struct {
	name, status, version, repo string
}

// extUpgradeWorld is what the upgrade command and its completion source both
// read: the ClusterRepos, their indexes and the Apps of the namespace.
type extUpgradeWorld struct {
	repoItems []string
	indexes   map[string]string
	apps      []extWorldApp
}

func (w extUpgradeWorld) fake(t *testing.T) *extFake {
	t.Helper()
	f := newExtFake(t)
	f.root.answers[reposPath] = rootAnswer{status: 200, body: collectionBody(w.repoItems...)}
	delete(f.root.answers, extIndexPath)
	for repo, body := range w.indexes {
		f.root.answers[reposPath+"/"+repo+"?link=index"] = rootAnswer{status: 200, body: body}
	}
	for _, a := range w.apps {
		putExtAppMeta(f, a.name, a.status, a.version, a.repo, map[string]any{"resourceVersion": "7"})
	}
	return f
}

// appJSON is an App as the completion source reads it.
func (a extWorldApp) appJSON(t *testing.T) string {
	t.Helper()
	annotations := map[string]string{}
	if a.repo != "" {
		annotations["catalog.cattle.io/ui-source-repo"] = a.repo
	}
	b, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"name": a.name},
		"spec": map[string]any{
			"info":  map[string]any{"status": a.status},
			"chart": map[string]any{"metadata": map[string]any{"version": a.version, "annotations": annotations}},
		},
	})
	require.NoError(t, err)
	return string(b)
}

func (w extUpgradeWorld) server(t *testing.T) *completiontest.Server {
	t.Helper()
	return completiontest.New(t, w.bodies(t))
}

// bodies are the responses of the world by request path.
func (w extUpgradeWorld) bodies(t *testing.T) map[string]string {
	t.Helper()
	bodies := map[string]string{
		"/rancherversion": `{"Version":"v2.15.3","RancherPrime":"false"}`,
		localCluster:      clusterBody("v1.36.5+k3s1", "v1.36.5+k3s1"),
		reposPath:         collectionBody(w.repoItems...),
	}
	for repo, body := range w.indexes {
		bodies[reposPath+"/"+repo] = body
	}
	var items []string
	for _, a := range w.apps {
		bodies[extAppsPath+"/"+a.name] = a.appJSON(t)
		items = append(items, a.appJSON(t))
	}
	bodies[extAppsPath] = `{"items":[` + joinComma(items) + `]}`
	return bodies
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}

// accepts reports whether the plan of the upgrade accepts the chart with the
// flags.
func (w extUpgradeWorld) accepts(t *testing.T, chart string, flags ...string) bool {
	t.Helper()
	_, _, err := newTestExtensionUpgrade(w.fake(t)).Plan(t.Context(), extInput(chart, flags...))
	return err == nil
}

func (w extUpgradeWorld) offered(t *testing.T, req completion.Request, limit int) []string {
	t.Helper()
	req.Command = extUpgradeName
	src := catalogcomplete.ExtensionUpgradeSource(extUpgradeName)
	_, claimed := src.Claim(req)
	require.True(t, claimed, "the source claims %+v", req)
	return src.Complete(t.Context(), w.server(t).Conn(limit), req).Names
}

// extUpgradeMixedWorld has a case for every rule of the upgrade: Apps
// whose newest compatible version is newer than the installed one, equal to
// it, a bundle, a chart that is no extension, an App that failed, one Helm
// kept the history of, one that names no ClusterRepo, ClusterRepos the Rancher
// UI does not read or that are not ready, a ClusterRepo that does not exist
// and a chart whose application version differs from its version.
func extUpgradeMixedWorld() extUpgradeWorld {
	partner := extIndex(map[string][]extVersion{
		"good":       {okVersion("2.0.0"), okVersion("1.0.0")},
		"latest":     {okVersion("2.0.0"), okVersion("1.0.0")},
		"bad-newest": {{version: "3.0.0", annotations: extIncompatible}, okVersion("2.0.0"), okVersion("1.0.0")},
		"no-compat":  {{version: "1.0.0", annotations: extIncompatible}},
		"bundle":     {{version: "2.0.0", annotations: extCompatible + extBundleAnnot}, {version: "1.0.0", annotations: extCompatible + extBundleAnnot}},
		"plain":      {{version: "2.0.0", annotations: `"catalog.cattle.io/display-name":"Plain"`}, {version: "1.0.0", annotations: `"catalog.cattle.io/display-name":"Plain"`}},
		"appver":     {{version: "1.2.3", appVersion: "9.9.9", annotations: extCompatible}},
		"failed":     {okVersion("2.0.0"), okVersion("1.0.0")},
		"gone":       {okVersion("2.0.0"), okVersion("1.0.0")},
		"kept":       {okVersion("2.0.0"), okVersion("1.0.0")},
		"norepo":     {okVersion("2.0.0"), okVersion("1.0.0")},
	})
	other := extIndex(map[string][]extVersion{"off-app": {okVersion("2.0.0"), okVersion("1.0.0")}})
	return extUpgradeWorld{
		repoItems: []string{
			repoItemAt(extRepo, "active", extPartnerAddress),
			`{"metadata":{"name":"off","state":{"name":"active"}},"spec":{"enabled":false}}`,
			repoItemBody("quiet", "active", `,"annotations":{"clusterrepo.cattle.io/hidden":"true"}`),
			repoItemAt("slow", "downloading", "https://example.test/slow"),
		},
		indexes: map[string]string{extRepo: partner, "off": other, "quiet": other, "slow": other},
		apps: []extWorldApp{
			{"good", "deployed", "1.0.0", extRepo},
			{"latest", "deployed", "2.0.0", extRepo},
			{"bad-newest", "deployed", "1.0.0", extRepo},
			{"no-compat", "deployed", "1.0.0", extRepo},
			{"bundle", "deployed", "1.0.0", extRepo},
			{"plain", "deployed", "1.0.0", extRepo},
			{"appver", "deployed", "1.2.3", extRepo},
			{"failed", "failed", "1.0.0", extRepo},
			{"kept", "uninstalled", "1.0.0", extRepo},
			{"norepo", "deployed", "1.0.0", ""},
			{"off-app", "deployed", "1.0.0", "off"},
			{"quiet-app", "deployed", "1.0.0", "quiet"},
			{"slow-app", "deployed", "1.0.0", "slow"},
			{"gone", "deployed", "1.0.0", "removed"},
		},
	}
}

// TestExtensionUpgradeAcceptsExactlyWhatCompletionOffers asserts, on one
// world, that each chart name, repository and version the completion source
// offers is accepted by the plan of `extension upgrade`, and that every other
// value of the world is refused: an App already at the newest version, a
// bundle, a chart that is no extension, an App Helm cannot upgrade, one that
// names no ClusterRepo, ClusterRepos that are disabled, hidden or not ready,
// the installed version and a version that fails a check.
func TestExtensionUpgradeAcceptsExactlyWhatCompletionOffers(t *testing.T) {
	t.Parallel()
	w := extUpgradeMixedWorld()
	var charts []string
	for _, a := range w.apps {
		charts = append(charts, a.name)
	}
	charts = append(charts, "ghost")
	repos := []string{extRepo, "off", "quiet", "slow", "removed", "nope"}

	t.Run("chart names", func(t *testing.T) {
		t.Parallel()
		got := w.offered(t, completion.Request{}, 5+completion.ExtensionRepositoryLimit)
		assert.ElementsMatch(t, []string{"good", "bad-newest", "failed"}, got)
		for _, chart := range charts {
			assert.Equal(t, slices.Contains(got, chart), w.accepts(t, chart), "chart %s", chart)
		}
	})

	t.Run("chart names with a typed repository", func(t *testing.T) {
		t.Parallel()
		got := w.offered(t, completion.Request{Tail: []string{"--repo", "off"}}, 5+completion.ExtensionRepositoryLimit)
		assert.Empty(t, got)
		got = w.offered(t, completion.Request{Tail: []string{"--repo", extRepo}}, 5+completion.ExtensionRepositoryLimit)
		assert.ElementsMatch(t, []string{"good", "bad-newest", "failed"}, got)
		for _, chart := range charts {
			assert.Equal(t, slices.Contains(got, chart), w.accepts(t, chart, flagRepo, extRepo), "chart %s", chart)
		}
	})

	t.Run("repositories", func(t *testing.T) {
		t.Parallel()
		// Two requests cannot read an index, so the repository is offered when
		// the rules about the ClusterRepo and the App accept it; a refusal
		// that needs the index (a version, a bundle, a chart that is no
		// extension) leaves it offered.
		repoRefusals := []string{"is hidden", "is disabled", "is not ready", "came from ClusterRepo", "not found", "does not name", "has the status", "is not installed"}
		for _, chart := range charts {
			got := w.offered(t, completion.Request{Flag: flagRepo, Positionals: []string{chart}}, 2)
			for _, repo := range repos {
				_, _, err := newTestExtensionUpgrade(w.fake(t)).Plan(t.Context(), extInput(chart, flagRepo, repo))
				refused := err != nil && slices.ContainsFunc(repoRefusals, func(s string) bool { return strings.Contains(err.Error(), s) })
				assert.Equal(t, slices.Contains(got, repo), !refused, "chart %s repository %s", chart, repo)
			}
		}
	})

	t.Run("versions", func(t *testing.T) {
		t.Parallel()
		candidates := []string{"1.0.0", "1.2.3", "2.0.0", "3.0.0", "9.9.9"}
		want := map[string][]string{"good": {"2.0.0"}, "latest": {"1.0.0"}, "bad-newest": {"2.0.0"}, "failed": {"2.0.0"}, "gone": nil}
		for _, chart := range charts {
			got := w.offered(t, completion.Request{Flag: flagVersion, Positionals: []string{chart}}, 5+completion.ExtensionRepositoryLimit)
			if exact, ok := want[chart]; ok {
				assert.ElementsMatch(t, exact, got, "chart %s", chart)
			}
			for _, v := range candidates {
				assert.Equal(t, slices.Contains(got, v), w.accepts(t, chart, flagVersion, v), "chart %s version %s", chart, v)
			}
		}
	})
}

// TestExtensionUpgradeCompletionRequestCounts asserts the requests each site
// makes, within the allowance the source claims for it.
func TestExtensionUpgradeCompletionRequestCounts(t *testing.T) {
	t.Parallel()
	w := extUpgradeMixedWorld()
	src := catalogcomplete.ExtensionUpgradeSource(extUpgradeName)
	cases := []struct {
		name      string
		req       completion.Request
		allowance completion.Allowance
		requests  int
	}{
		{name: "chart names", req: completion.Request{}, allowance: completion.AllowanceExtension, requests: 4 + 1},
		{name: "--repo", req: completion.Request{Flag: flagRepo, Positionals: []string{"good"}}, allowance: completion.AllowanceTwo, requests: 2},
		{name: "--version", req: completion.Request{Flag: flagVersion, Positionals: []string{"good"}}, allowance: completion.AllowanceExtension, requests: 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.req.Command = extUpgradeName
			reqs, claimed := src.Claim(tc.req)
			require.True(t, claimed)
			assert.Equal(t, tc.allowance, reqs.Allowance)
			srv := w.server(t)
			src.Complete(t.Context(), srv.Conn(5+completion.ExtensionRepositoryLimit), tc.req)
			assert.Len(t, srv.Requested(), tc.requests)
		})
	}
}

// TestExtensionUpgradeCompletionDeclinesOtherSites asserts the source claims
// no other site of the command: a line with no chart typed for --repo and
// --version, --timeout, and a second positional.
func TestExtensionUpgradeCompletionDeclinesOtherSites(t *testing.T) {
	t.Parallel()
	src := catalogcomplete.ExtensionUpgradeSource(extUpgradeName)
	for _, req := range []completion.Request{
		{Flag: flagRepo}, {Flag: flagVersion}, {Flag: "timeout", Positionals: []string{"good"}}, {Index: 1}, {Command: "other"},
	} {
		if req.Command == "" {
			req.Command = extUpgradeName
		}
		_, claimed := src.Claim(req)
		assert.False(t, claimed, "%+v", req)
	}
}

// TestExtensionUpgradeCompletionOffersNothingBeyondTheBound asserts that Apps
// that name more ClusterRepos than completion.ExtensionRepositoryLimit offer
// no chart name.
func TestExtensionUpgradeCompletionOffersNothingBeyondTheBound(t *testing.T) {
	t.Parallel()
	build := func(n int) extUpgradeWorld {
		w := extUpgradeWorld{indexes: map[string]string{}}
		for i := range n {
			name := "repo" + string(rune('a'+i))
			chart := "chart" + string(rune('a'+i))
			w.repoItems = append(w.repoItems, repoItemAt(name, "active", "https://example.test/"+name))
			w.indexes[name] = extIndex(map[string][]extVersion{chart: {okVersion("2.0.0"), okVersion("1.0.0")}})
			w.apps = append(w.apps, extWorldApp{chart, "deployed", "1.0.0", name})
		}
		return w
	}
	atBound := build(completion.ExtensionRepositoryLimit)
	assert.Len(t, atBound.offered(t, completion.Request{}, 5+completion.ExtensionRepositoryLimit), completion.ExtensionRepositoryLimit)
	beyond := build(completion.ExtensionRepositoryLimit + 1)
	assert.Empty(t, beyond.offered(t, completion.Request{}, 5+completion.ExtensionRepositoryLimit))
}

// TestExtensionUpgradeCompletionOffersNothingWhenTheContextEnds asserts that
// a context that ends during a read that follows the App offers nothing: the
// index of the second repository for the chart names, and the version
// document for the versions. Without the version document the Extensions API
// version of the server is unknown and a version the command refuses would
// be offered.
func TestExtensionUpgradeCompletionOffersNothingWhenTheContextEnds(t *testing.T) {
	t.Parallel()
	w := extUpgradeWorld{
		repoItems: []string{repoItemAt("alpha", "active", "https://example.test/alpha"), repoItemAt("beta", "active", "https://example.test/beta")},
		indexes: map[string]string{
			"alpha": extIndex(map[string][]extVersion{"first": {okVersion("2.0.0"), okVersion("1.0.0")}, "checked": {{version: "3.0.0", annotations: extIncompatible}, okVersion("2.0.0"), okVersion("1.0.0")}}),
			"beta":  extIndex(map[string][]extVersion{"second": {okVersion("2.0.0"), okVersion("1.0.0")}}),
		},
		apps: []extWorldApp{
			{"first", "deployed", "1.0.0", "alpha"},
			{"checked", "deployed", "1.0.0", "alpha"},
			{"second", "deployed", "1.0.0", "beta"},
		},
	}
	src := catalogcomplete.ExtensionUpgradeSource(extUpgradeName)
	cases := []struct {
		name    string
		req     completion.Request
		endAt   string
		offered []string
	}{
		{"chart names", completion.Request{}, reposPath + "/beta", []string{"first", "checked", "second"}},
		{"versions", completion.Request{Flag: flagVersion, Positionals: []string{"checked"}}, "/rancherversion", []string{"2.0.0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.req.Command = extUpgradeName
			assert.ElementsMatch(t, tc.offered, w.offered(t, tc.req, 5+completion.ExtensionRepositoryLimit), "with the time to read, the source offers these")
			srv, ctx := serverEndingContext(t, w.bodies(t), tc.endAt)
			got := src.Complete(ctx, srv.Conn(5+completion.ExtensionRepositoryLimit), tc.req)
			assert.Empty(t, got.Names)
			require.NotEmpty(t, srv.Requested())
			assert.True(t, slices.ContainsFunc(srv.Requested(), func(uri string) bool {
				path, _, _ := strings.Cut(uri, "?")
				return path == tc.endAt
			}), "the context ended during a read the source made")
		})
	}
}
