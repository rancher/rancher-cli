package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/access"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// accessFixture is a Rancher stand-in for the access completion sites: the
// two aggregated discovery roots of cluster c-abc12, the management cluster
// list a display name resolves through, and the management user list --as
// reads. It records every path it serves, so a test can assert the exact
// request budget of a site, not merely its candidates.
type accessFixture struct {
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
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

// discoveryResource builds one aggregated-discovery resource entry.
func discoveryResource(plural, singular string, shortNames []string, namespaced bool, subresources ...string) map[string]any {
	scope := "Cluster"
	if namespaced {
		scope = "Namespaced"
	}
	subs := make([]any, 0, len(subresources))
	for _, s := range subresources {
		subs = append(subs, map[string]any{"subresource": s, "verbs": []any{"get"}})
	}
	r := map[string]any{
		"resource": plural, "singularResource": singular, "scope": scope,
		"responseKind": map[string]any{"kind": strings.ToUpper(plural[:1]) + plural[1:len(plural)-1]},
		"verbs":        []any{"get", "list"}, "subresources": subs,
	}
	if len(shortNames) > 0 {
		r["shortNames"] = shortNames
	}
	return r
}

func discoveryGroup(name, version string, resources ...map[string]any) map[string]any {
	items := make([]any, 0, len(resources))
	for _, r := range resources {
		items = append(items, r)
	}
	return map[string]any{
		"metadata": map[string]any{"name": name},
		"versions": []any{map[string]any{"version": version, "resources": items}},
	}
}

func discoveryDoc(groups ...map[string]any) map[string]any {
	items := make([]any, 0, len(groups))
	for _, g := range groups {
		items = append(items, g)
	}
	return map[string]any{"kind": "APIGroupDiscoveryList", "apiVersion": "apidiscovery.k8s.io/v2", "items": items}
}

// newAccessFixture starts the fixture and points a stored config with live
// completion enabled at it, with a management token in the environment.
func newAccessFixture(t *testing.T) *accessFixture {
	t.Helper()
	f := &accessFixture{}
	core := discoveryDoc(discoveryGroup("", "v1",
		discoveryResource("pods", "pod", []string{"po"}, true, "log", "status", "exec"),
		discoveryResource("nodes", "node", []string{"no"}, false, "status", "proxy"),
		discoveryResource("services", "service", []string{"svc"}, true, "status"),
	))
	grouped := discoveryDoc(
		discoveryGroup("apps", "v1", discoveryResource("deployments", "deployment", []string{"deploy"}, true, "scale", "status")),
		discoveryGroup("networking.k8s.io", "v1", discoveryResource("ingresses", "ingress", []string{"ing"}, true, "status")),
		discoveryGroup("gateway.example.io", "v1", discoveryResource("ingresses", "ingress", nil, true, "conditions")),
	)
	clusters := map[string]any{"items": []any{
		map[string]any{"metadata": map[string]any{"name": "c-abc12"}, "spec": map[string]any{"displayName": "testcluster"}},
		map[string]any{"metadata": map[string]any{"name": "c-def34"}, "spec": map[string]any{"displayName": "prod cluster"}},
	}}
	users := map[string]any{"items": []any{
		map[string]any{"metadata": map[string]any{"name": "u-abc12"}, "username": "alice", "displayName": "Alice"},
		map[string]any{"metadata": map[string]any{"name": "u-def34"}, "username": "bob", "displayName": "Bob"},
		map[string]any{"metadata": map[string]any{"name": "u-sys11"}, "username": "svc", "principalIds": []any{"system://provisioning"}},
	}}
	bodies := map[string]any{
		"/k8s/clusters/c-abc12/api":                                 core,
		"/k8s/clusters/c-abc12/apis":                                grouped,
		"/k8s/clusters/local/apis/management.cattle.io/v3/clusters": clusters,
		"/k8s/clusters/local/apis/management.cattle.io/v3/users":    users,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r.URL.Path)
		body, ok := bodies[r.URL.Path]
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
	setStoredConfig(cfg, dir)
	t.Setenv(config.EnvToken, "mgmt-token")
	return dir
}

func canDescriptor() runtime.Descriptor    { return access.NewCan(nil, nil).Describe() }
func listDescriptor() runtime.Descriptor   { return access.NewList(nil, nil).Describe() }
func whoCanDescriptor() runtime.Descriptor { return access.NewWhoCan(nil, nil).Describe() }

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

// TestAccessCanTypeFlagIsStaticEnum asserts --type offers its declared enum,
// unchanged by the network gate.
func TestAccessCanTypeFlagIsStaticEnum(t *testing.T) {
	// NOT parallel: mutates storedConfig and the token environment.
	f := newAccessFixture(t)
	storeAccessConfig(t, f, false)
	for _, d := range []runtime.Descriptor{canDescriptor(), listDescriptor()} {
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "type", FlagSpelling: "--type"}
		assert.Equal(t, []string{"group", "user"}, dispatchSite(accessCtx(t), site, d, nil, nil, nil), d.Name)
	}
	assert.Empty(t, f.served())
}

// TestAccessCanResourcePositionalUsesDiscovery asserts the resource
// positional offers base resources from both discovery roots, core and
// grouped alike, never a "<resource>/<sub>" name, and that an id-shaped
// cluster costs exactly the two roots while a display name costs one
// resolution request more.
func TestAccessCanResourcePositionalUsesDiscovery(t *testing.T) {
	// NOT parallel: mutates storedConfig and the token environment.
	f := newAccessFixture(t)
	storeAccessConfig(t, f, true)
	d := canDescriptor()
	site := CompletionSite{Kind: SitePositional, Index: 1}

	got := liveCandidates(accessCtx(t), site, d, nil, []string{"get", "--cluster", "testcluster"})
	assert.Equal(t, []string{"deployments", "ingresses", "nodes", "pods", "services"}, got)
	for _, c := range got {
		assert.NotContains(t, c, "/", "a subresource is never a resource candidate")
	}
	assert.Len(t, f.served(), 3, "a display name costs the resolution request plus the two roots")

	f.reset()
	got = liveCandidates(accessCtx(t), site, d, nil, []string{"get", "--cluster", "c-abc12"})
	assert.Equal(t, []string{"deployments", "ingresses", "nodes", "pods", "services"}, got)
	assert.Len(t, f.served(), 2, "an id-shaped cluster costs the two discovery roots only")
}

// TestAccessCanDisplayNameResolvesAtEveryClusterSite asserts every
// cluster-dependent site resolves a display name to its id before the
// discovery roots, never puts the display name in a path, and makes no
// request at all when --cluster is absent.
func TestAccessCanDisplayNameResolvesAtEveryClusterSite(t *testing.T) {
	// NOT parallel: mutates storedConfig and the token environment.
	f := newAccessFixture(t)
	storeAccessConfig(t, f, true)
	d := canDescriptor()
	sites := []struct {
		name string
		site CompletionSite
		tail []string
	}{
		{"resource positional", CompletionSite{Kind: SitePositional, Index: 1}, []string{"get"}},
		{"group flag", CompletionSite{Kind: SiteFlagValue, FlagName: "group", FlagSpelling: "--group"}, []string{"get", "deployments"}},
		{"subresource flag", CompletionSite{Kind: SiteFlagValue, FlagName: "subresource", FlagSpelling: "--subresource"}, []string{"get", "pods"}},
	}
	for _, tc := range sites {
		t.Run(tc.name, func(t *testing.T) {
			f.reset()
			cands := liveCandidates(accessCtx(t), tc.site, d, nil, append(slices.Clone(tc.tail), "--cluster", "testcluster"))
			assert.NotEmpty(t, cands)
			paths := f.served()
			require.Equal(t, []string{
				"/k8s/clusters/local/apis/management.cattle.io/v3/clusters",
				"/k8s/clusters/c-abc12/api",
				"/k8s/clusters/c-abc12/apis",
			}, paths, "one resolution request, then the two discovery roots")
			for _, p := range paths {
				assert.NotContains(t, p, "/k8s/clusters/testcluster/", "a display name is never a path segment")
			}

			f.reset()
			assert.Empty(t, liveCandidates(accessCtx(t), tc.site, d, nil, tc.tail), "no --cluster: nothing to offer")
			assert.Empty(t, f.served(), "no --cluster: zero requests")
		})
	}
}

// TestAccessCanGroupFlagOffersAPIGroups asserts --group offers the named
// API groups only; the core group has no name to offer.
func TestAccessCanGroupFlagOffersAPIGroups(t *testing.T) {
	// NOT parallel: mutates storedConfig and the token environment.
	f := newAccessFixture(t)
	storeAccessConfig(t, f, true)
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "group", FlagSpelling: "--group"}
	got := liveCandidates(accessCtx(t), site, canDescriptor(), nil, []string{"get", "deployments", "--cluster", "c-abc12"})
	assert.Equal(t, []string{"apps", "gateway.example.io", "networking.k8s.io"}, got)
}

// TestAccessCanSubresourceFollowsResourceResolution asserts --subresource
// offers exactly the typed resource's subresources, under every spelling
// the command itself accepts, and nothing at all for a name two non-core
// groups serve.
func TestAccessCanSubresourceFollowsResourceResolution(t *testing.T) {
	// NOT parallel: mutates storedConfig and the token environment.
	f := newAccessFixture(t)
	storeAccessConfig(t, f, true)
	d := canDescriptor()
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "subresource", FlagSpelling: "--subresource"}
	cases := []struct {
		name string
		tail []string
		want []string
	}{
		{"plural", []string{"get", "pods", "--cluster", "c-abc12"}, []string{"exec", "log", "status"}},
		{"singular", []string{"get", "pod", "--cluster", "c-abc12"}, []string{"exec", "log", "status"}},
		{"short name", []string{"get", "po", "--cluster", "c-abc12"}, []string{"exec", "log", "status"}},
		{"cluster before the verb", []string{"--cluster", "c-abc12", "get", "pods"}, []string{"exec", "log", "status"}},
		{"equals-form cluster", []string{"get", "--cluster=c-abc12", "pods"}, []string{"exec", "log", "status"}},
		{"namespace flag between", []string{"get", "-n", "team-a", "pods", "--cluster", "c-abc12"}, []string{"exec", "log", "status"}},
		{"boolean flag consumes no positional", []string{"get", "--all-namespaces", "pods", "--cluster", "c-abc12"}, []string{"exec", "log", "status"}},
		{"cluster scoped", []string{"get", "nodes", "--cluster", "c-abc12"}, []string{"proxy", "status"}},
		{"qualified", []string{"get", "deployments.apps", "--cluster", "c-abc12"}, []string{"scale", "status"}},
		{"group flag", []string{"get", "deployments", "--group", "apps", "--cluster", "c-abc12"}, []string{"scale", "status"}},
		{"ambiguous bare name", []string{"get", "ingresses", "--cluster", "c-abc12"}, nil},
		{"qualified ambiguous name", []string{"get", "ingresses.networking.k8s.io", "--cluster", "c-abc12"}, []string{"status"}},
		{"no resource typed", []string{"get", "--cluster", "c-abc12"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.reset()
			got := liveCandidates(accessCtx(t), site, d, nil, tc.tail)
			assert.Equal(t, tc.want, got)
			if tc.name == "cluster scoped" {
				assert.NotContains(t, got, "log", "nodes have no log subresource")
			}
		})
	}
}

// TestAccessSubjectFlagOffersUsers asserts --as offers the management user
// union for both commands, in the spaced and the --as= form, never touches
// a cluster, and is unaffected by an unrelated --cluster on the line -
// including a display name with a space, which no path could carry.
func TestAccessSubjectFlagOffersUsers(t *testing.T) {
	// NOT parallel: mutates storedConfig and the token environment.
	f := newAccessFixture(t)
	dir := storeAccessConfig(t, f, true)
	// A cluster-scoped exec credential exists beside the management token:
	// the bypass must keep reading management users, not select it.
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetCachedCred(config.CacheKey("k8s", f.srv.URL, "c-abc12"), "exec-tok",
		time.Now().Add(time.Hour), "k8s", f.srv.URL, "c-abc12"))

	want := []string{"alice", "bob", "u-abc12", "u-def34"}
	for _, d := range []runtime.Descriptor{canDescriptor(), listDescriptor()} {
		cases := []struct {
			name string
			site CompletionSite
			tail []string
			want []string
		}{
			{"spaced", CompletionSite{Kind: SiteFlagValue, FlagName: "as", FlagSpelling: "--as"}, []string{"get", "pods", "--as"}, want},
			{"equals form", CompletionSite{Kind: SiteFlagValue, FlagName: "as", FlagSpelling: "--as", EqualsForm: true},
				[]string{"get", "pods", "--as="}, []string{"--as=alice", "--as=bob", "--as=u-abc12", "--as=u-def34"}},
			{"display name with a space after", CompletionSite{Kind: SiteFlagValue, FlagName: "as", FlagSpelling: "--as"},
				[]string{"get", "pods", "--cluster", "prod cluster", "--as"}, want},
			{"display name with a space before", CompletionSite{Kind: SiteFlagValue, FlagName: "as", FlagSpelling: "--as"},
				[]string{"get", "pods", "--as", "--cluster", "prod cluster"}, want},
		}
		for _, tc := range cases {
			t.Run(d.Name+" "+tc.name, func(t *testing.T) {
				f.reset()
				got := liveCandidates(accessCtx(t), tc.site, d, nil, tc.tail)
				assert.Equal(t, tc.want, sorted(got))
				for _, c := range got {
					assert.NotContains(t, c, "://", "the completion protocol splits a candidate at its first colon")
					assert.NotContains(t, c, "system:serviceaccount:")
				}
				assert.Equal(t, []string{"/k8s/clusters/local/apis/management.cattle.io/v3/users"}, f.served(),
					"exactly one management user list, no cluster lookup, no downstream request")
			})
		}
	}
}

// TestAccessWhoCanSharesTheQuestionSites asserts who-can completes its
// verb, resource, --group, and --subresource exactly as access can does
// (same vocabularies, same discovery roots, same request budget), and that
// it has no --as site: nothing is offered and nothing is fetched.
func TestAccessWhoCanSharesTheQuestionSites(t *testing.T) {
	// NOT parallel: mutates storedConfig and the token environment.
	f := newAccessFixture(t)
	storeAccessConfig(t, f, true)
	d := whoCanDescriptor()

	verb := CompletionSite{Kind: SitePositional, Index: 0}
	assert.Equal(t, sorted(access.ResourceVerbs), dispatchSite(accessCtx(t), verb, d, nil, nil, nil))
	assert.Equal(t, sorted(access.HTTPVerbs), dispatchSite(accessCtx(t), verb, d, nil, nil, []string{"--non-resource-url", "/healthz"}))
	assert.Empty(t, f.served(), "verbs are compiled in")

	resource := CompletionSite{Kind: SitePositional, Index: 1}
	assert.Equal(t, []string{"deployments", "ingresses", "nodes", "pods", "services"},
		liveCandidates(accessCtx(t), resource, d, nil, []string{"get", "--cluster", "c-abc12"}))
	assert.Len(t, f.served(), 2, "an id-shaped cluster costs the two discovery roots only")
	assert.Nil(t, liveCandidates(accessCtx(t), resource, d, nil, []string{"get", "--cluster", "c-abc12", "--non-resource-url", "/healthz"}),
		"no resource positional beside --non-resource-url")

	f.reset()
	group := CompletionSite{Kind: SiteFlagValue, FlagName: "group", FlagSpelling: "--group"}
	assert.Equal(t, []string{"apps", "gateway.example.io", "networking.k8s.io"},
		liveCandidates(accessCtx(t), group, d, nil, []string{"get", "deployments", "--cluster", "c-abc12"}))
	sub := CompletionSite{Kind: SiteFlagValue, FlagName: "subresource", FlagSpelling: "--subresource"}
	assert.Equal(t, []string{"exec", "log", "status"},
		liveCandidates(accessCtx(t), sub, d, nil, []string{"get", "pods", "--cluster", "c-abc12"}))
	assert.Equal(t, []string{"exec", "log", "status"},
		liveCandidates(accessCtx(t), sub, d, nil, []string{"get", "--show-denied", "pods", "--cluster", "c-abc12"}),
		"a boolean flag consumes no positional")

	f.reset()
	as := CompletionSite{Kind: SiteFlagValue, FlagName: "as", FlagSpelling: "--as"}
	assert.Nil(t, liveCandidates(accessCtx(t), as, d, nil, nil))
	assert.Empty(t, f.served(), "who-can has no --as; nothing is fetched for it")
}

func explainDescriptor() runtime.Descriptor { return access.NewExplain(nil, nil).Describe() }

// TestAccessExplainSharesTheQuestionAndSubjectSites asserts explain
// completes its verb, resource, --group and --subresource exactly as
// access can does, and its --as exactly as access can does (users by id
// and username, the caller's cache, no group principals).
func TestAccessExplainSharesTheQuestionAndSubjectSites(t *testing.T) {
	// NOT parallel: mutates storedConfig and the token environment.
	f := newAccessFixture(t)
	storeAccessConfig(t, f, true)
	d := explainDescriptor()

	verb := CompletionSite{Kind: SitePositional, Index: 0}
	assert.Equal(t, sorted(access.ResourceVerbs), dispatchSite(accessCtx(t), verb, d, nil, nil, nil))
	assert.Empty(t, f.served(), "verbs are compiled in")

	resource := CompletionSite{Kind: SitePositional, Index: 1}
	assert.Equal(t, []string{"deployments", "ingresses", "nodes", "pods", "services"},
		liveCandidates(accessCtx(t), resource, d, nil, []string{"get", "--cluster", "c-abc12"}))
	assert.Len(t, f.served(), 2, "an id-shaped cluster costs the two discovery roots only")

	f.reset()
	as := CompletionSite{Kind: SiteFlagValue, FlagName: "as", FlagSpelling: "--as"}
	canAs := liveCandidates(accessCtx(t), as, canDescriptor(), nil, []string{"get", "pods", "--cluster", "c-abc12"})
	f.reset()
	assert.Equal(t, canAs, liveCandidates(accessCtx(t), as, d, nil, []string{"get", "pods", "--cluster", "c-abc12"}),
		"explain offers exactly what access can offers for --as")
	assert.NotEmpty(t, canAs, "the fixture's users are offered")
}
