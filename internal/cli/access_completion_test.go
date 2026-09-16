package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/access"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// accessFixture is a Rancher stand-in for the access completion sites the
// surface still decides: the management cluster list and the steve
// cluster collection `access projects` walks. It records every path it
// serves, so a test can assert the exact request budget of a site, not
// merely its candidates.
type accessFixture struct {
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
	// pages, keyed by request URI (path and query), take precedence over
	// the path-keyed bodies: the steve collection is paged by a continue
	// query the path alone cannot tell apart.
	pages map[string]any
}

func (f *accessFixture) record(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, path)
}

func (f *accessFixture) served() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.paths)
}

func (f *accessFixture) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = nil
}

// newAccessFixture starts the fixture and points a stored config with live
// completion enabled at it, with a management token in the environment.
func newAccessFixture(t *testing.T) *accessFixture {
	t.Helper()
	f := &accessFixture{}
	clusters := map[string]any{"items": []any{
		map[string]any{"metadata": map[string]any{"name": "c-abc12"}, "spec": map[string]any{"displayName": "testcluster"}},
		map[string]any{"metadata": map[string]any{"name": "c-def34"}, "spec": map[string]any{"displayName": "prod cluster"}},
	}}
	bodies := map[string]any{
		"/k8s/clusters/local/apis/management.cattle.io/v3/clusters": clusters,
		"/v1/management.cattle.io.clusters": map[string]any{"type": "collection", "count": 2, "revision": "1", "data": []any{
			map[string]any{"id": "c-abc12", "metadata": map[string]any{"name": "c-abc12"}, "spec": map[string]any{"displayName": "testcluster"}},
			map[string]any{"id": "c-zzz99", "metadata": map[string]any{"name": "c-zzz99"}, "spec": map[string]any{"displayName": "membercluster"}},
		}},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r.URL.Path)
		f.mu.Lock()
		body, ok := f.pages[r.URL.RequestURI()]
		f.mu.Unlock()
		if !ok {
			body, ok = bodies[r.URL.Path]
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// storeAccessConfig points the completion pipeline at the fixture. network
// false proves a site is served by the static layer.
func storeAccessConfig(t *testing.T, f *accessFixture, network bool) string {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: f.srv.URL}},
		Completion:    config.CompletionConfig{Network: network},
	}
	setStoredConfigForTest(t, cfg, dir)
	t.Setenv(config.EnvToken, "mgmt-token")
	return dir
}

func canDescriptor() runtime.Descriptor      { return access.NewCan(nil, nil).Describe() }
func listDescriptor() runtime.Descriptor     { return access.NewList(nil, nil).Describe() }
func whoCanDescriptor() runtime.Descriptor   { return access.NewWhoCan(nil, nil).Describe() }
func explainDescriptor() runtime.Descriptor  { return access.NewExplain(nil, nil).Describe() }
func clustersDescriptor() runtime.Descriptor { return access.NewClusters(nil, nil, nil).Describe() }
func projectsDescriptor() runtime.Descriptor { return access.NewProjects(nil, nil, nil).Describe() }

func accessCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func sorted(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// TestAccessCanVerbPositionalIsStaticAndOffline asserts the verb positional
// is served by the static layer: the resource vocabulary by default, the
// HTTP vocabulary under --non-resource-url, both with live completion off
// and zero requests to a reachable server.
func TestAccessCanVerbPositionalIsStaticAndOffline(t *testing.T) {
	// NOT parallel: mutates storedConfig and the token environment.
	f := newAccessFixture(t)
	storeAccessConfig(t, f, false)
	d := canDescriptor()
	site := CompletionSite{Kind: SitePositional, Index: 0}

	got := dispatchSite(accessCtx(t), site, d, nil, nil, nil)
	assert.Equal(t, sorted(access.ResourceVerbs), got)

	got = dispatchSite(accessCtx(t), site, d, nil, nil, []string{"--non-resource-url", "/healthz"})
	assert.Equal(t, sorted(access.HTTPVerbs), got)

	assert.Empty(t, f.served(), "a compiled-in vocabulary needs no request")
}

// TestAccessCanTypeFlagIsStaticEnum asserts --type offers its declared
// enum, unchanged by the network gate: group and user on the questions,
// user alone on clusters and projects.
func TestAccessCanTypeFlagIsStaticEnum(t *testing.T) {
	// NOT parallel: mutates storedConfig and the token environment.
	f := newAccessFixture(t)
	storeAccessConfig(t, f, false)
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "type", FlagSpelling: "--type"}
	for _, d := range []runtime.Descriptor{canDescriptor(), listDescriptor()} {
		assert.Equal(t, []string{"group", "user"}, dispatchSite(accessCtx(t), site, d, nil, nil, nil), d.Name)
	}
	for _, d := range []runtime.Descriptor{clustersDescriptor(), projectsDescriptor()} {
		assert.Equal(t, []string{"user"}, dispatchSite(accessCtx(t), site, d, nil, nil, []string{"--type"}), d.Name)
	}
	assert.Empty(t, f.served())
}

// TestAccessProjectsClusterOffersTheSteveSet asserts access projects
// completes --cluster from the steve collection the command resolves it
// against (ids and unique display names), not from the management LIST
// every other cluster flag reads.
func TestAccessProjectsClusterOffersTheSteveSet(t *testing.T) {
	// NOT parallel: mutates storedConfig and the token environment.
	f := newAccessFixture(t)
	storeAccessConfig(t, f, true)
	d := projectsDescriptor()

	f.reset()
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "cluster", FlagSpelling: "--cluster"}
	got := liveCandidates(accessCtx(t), site, d, projectsCompleter(), []string{"--cluster"})
	assert.Equal(t, []string{"c-abc12", "c-zzz99", "local", "membercluster", "testcluster"}, sorted(got),
		"the collection omits local, so the completer inserts it as the command does")
	assert.Equal(t, []string{"/v1/management.cattle.io.clusters"}, f.served())
}

func stevePage(count int, cont string, items ...map[string]any) map[string]any {
	page := map[string]any{"type": "collection", "count": count, "revision": "7", "data": items}
	if cont != "" {
		page["continue"] = cont
	}
	return page
}

func steveCluster(id, display string) map[string]any {
	return map[string]any{"id": id, "metadata": map[string]any{"name": id}, "spec": map[string]any{"displayName": display}}
}

// TestAccessProjectsClusterCompletionFollowsEveryPage asserts the --cluster
// completer follows steve's continue token, inserts exactly one local row
// when the collection omits it, and decides unique display names over the
// whole set (a name repeated on a later page is offered by id only).
func TestAccessProjectsClusterCompletionFollowsEveryPage(t *testing.T) {
	// NOT parallel: mutates storedConfig and the token environment.
	f := newAccessFixture(t)
	storeAccessConfig(t, f, true)
	d := projectsDescriptor()
	f.mu.Lock()
	f.pages = map[string]any{
		"/v1/management.cattle.io.clusters":             stevePage(3, "p2", steveCluster("c-aaa11", "shared")),
		"/v1/management.cattle.io.clusters?continue=p2": stevePage(3, "p3", steveCluster("c-bbb22", "shared")),
		"/v1/management.cattle.io.clusters?continue=p3": stevePage(3, "", steveCluster("c-ccc33", "alone")),
	}
	f.mu.Unlock()

	f.reset()
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "cluster", FlagSpelling: "--cluster"}
	got := liveCandidates(accessCtx(t), site, d, projectsCompleter(), []string{"--cluster"})
	assert.Equal(t, []string{"alone", "c-aaa11", "c-bbb22", "c-ccc33", "local"}, sorted(got),
		"three pages read, the repeated name offered by id only, local inserted once")
	assert.Len(t, f.served(), 3)
}

// TestAccessProjectsClusterCompletionOffersNothingOnAnInconsistentRead
// asserts a paged read whose count does not add up offers no candidate.
func TestAccessProjectsClusterCompletionOffersNothingOnAnInconsistentRead(t *testing.T) {
	// NOT parallel: mutates storedConfig and the token environment.
	f := newAccessFixture(t)
	storeAccessConfig(t, f, true)
	d := projectsDescriptor()
	f.mu.Lock()
	f.pages = map[string]any{
		"/v1/management.cattle.io.clusters":             stevePage(5, "p2", steveCluster("c-aaa11", "a")),
		"/v1/management.cattle.io.clusters?continue=p2": stevePage(5, "", steveCluster("c-bbb22", "b")),
	}
	f.mu.Unlock()
	f.reset()
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "cluster", FlagSpelling: "--cluster"}
	assert.Empty(t, liveCandidates(accessCtx(t), site, d, projectsCompleter(), []string{"--cluster"}))
}

// TestAccessProjectsClusterCompletionKeepsAReturnedLocal asserts a
// collection that lists local yields one local candidate, not two.
func TestAccessProjectsClusterCompletionKeepsAReturnedLocal(t *testing.T) {
	// NOT parallel: mutates storedConfig and the token environment.
	f := newAccessFixture(t)
	storeAccessConfig(t, f, true)
	d := projectsDescriptor()
	f.mu.Lock()
	f.pages = map[string]any{"/v1/management.cattle.io.clusters": stevePage(2, "", steveCluster("local", "local"), steveCluster("c-aaa11", "prod"))}
	f.mu.Unlock()
	f.reset()
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "cluster", FlagSpelling: "--cluster"}
	assert.Equal(t, []string{"c-aaa11", "local", "prod"}, sorted(liveCandidates(accessCtx(t), site, d, projectsCompleter(), []string{"--cluster"})))
}

// TestAccessProjectsClusterCompletionFailsClosedLikeTheCommandReader
// asserts the completer offers nothing wherever the command's steve reader
// would refuse the collection: a missing count, a missing revision, an item
// naming itself differently, a repeated id that disagrees with itself, a
// repeated continue token, and a walk that reaches the page bound with a
// page still pending.
func TestAccessProjectsClusterCompletionFailsClosedLikeTheCommandReader(t *testing.T) {
	// NOT parallel: mutates storedConfig and the token environment.
	f := newAccessFixture(t)
	storeAccessConfig(t, f, true)
	d := projectsDescriptor()
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "cluster", FlagSpelling: "--cluster"}
	base := "/v1/management.cattle.io.clusters"
	noCount := stevePage(1, "", steveCluster("c-aaa11", "a"))
	delete(noCount, "count")
	noRevision := stevePage(1, "", steveCluster("c-aaa11", "a"))
	delete(noRevision, "revision")
	renamed := steveCluster("c-aaa11", "a")
	renamed["metadata"] = map[string]any{"name": "c-other"}
	looping := map[string]any{}
	for i := range access.SteveMaxPages + 5 {
		key := base
		if i > 0 {
			key = base + "?continue=t" + strconv.Itoa(i)
		}
		looping[key] = stevePage(1000, "t"+strconv.Itoa(i+1), steveCluster("c-"+strconv.Itoa(i), "n"))
	}
	cases := map[string]map[string]any{
		"missing count":         {base: noCount},
		"missing revision":      {base: noRevision},
		"metadata.name differs": {base: stevePage(1, "", renamed)},
		"disagreeing duplicate": {base: stevePage(2, "", steveCluster("c-aaa11", "a"), steveCluster("c-aaa11", "b"))},
		"repeated token":        {base: stevePage(3, "p2", steveCluster("c-aaa11", "a")), base + "?continue=p2": stevePage(3, "p2", steveCluster("c-bbb22", "b"))},
		"page bound reached":    looping,
	}
	for name, pages := range cases {
		f.mu.Lock()
		f.pages = pages
		f.mu.Unlock()
		f.reset()
		assert.Empty(t, liveCandidates(accessCtx(t), site, d, projectsCompleter(), []string{"--cluster"}), name)
	}
}

// TestAccessTimeoutOffersTheDurationLadder asserts every access command
// completes --timeout from the same static ladder, each value one the
// command accepts, with no request made.
func TestAccessTimeoutOffersTheDurationLadder(t *testing.T) {
	// NOT parallel: mutates storedConfig and the token environment.
	f := newAccessFixture(t)
	storeAccessConfig(t, f, true)
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "timeout", FlagSpelling: "--timeout"}
	for _, d := range []runtime.Descriptor{canDescriptor(), listDescriptor(), whoCanDescriptor(), explainDescriptor(), clustersDescriptor(), projectsDescriptor()} {
		require.True(t, slices.ContainsFunc(d.Flags, func(a runtime.ArgSpec) bool { return a.Name == "timeout" }), d.Name+" carries --timeout")
		f.reset()
		assert.Equal(t, []string{"10m", "1m", "2m", "30s", "5m"}, dispatchSite(accessCtx(t), site, d, nil, nil, []string{"--timeout"}), d.Name)
		assert.Empty(t, f.served(), d.Name+": the ladder is static")
	}
	equals := CompletionSite{Kind: SiteFlagValue, FlagName: "timeout", FlagSpelling: "--timeout", EqualsForm: true}
	assert.Equal(t, []string{"--timeout=10m", "--timeout=1m", "--timeout=2m", "--timeout=30s", "--timeout=5m"}, dispatchSite(accessCtx(t), equals, projectsDescriptor(), nil, nil, []string{"--timeout="}))
}

// projectsCompleter is the zero-dependency probe of `access projects`, the
// owner the dispatcher consults first for the command's own --cluster
// source; a nil owner would fall through to the shared management list.
func projectsCompleter() completion.Completer {
	return access.NewProjects(nil, nil, nil).(completion.Completer)
}
