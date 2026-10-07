package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/rbac"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCompleter hands the dispatcher a source built in the test. The
// dispatcher does not check declaring packages (BuildRoot does), so a
// test source can stand in for an owner's.
type fakeCompleter struct{ src completion.Source }

func (f fakeCompleter) LiveSource() completion.Source { return f.src }

// claiming returns a source that claims every site with reqs and answers
// names, recording the request it saw.
func claiming(reqs completion.Requirements, names []string, saw *completion.Request) completion.Source {
	return completion.Source{
		Claim: func(completion.Request) (completion.Requirements, bool) { return reqs, true },
		Complete: func(_ context.Context, _ *completion.Connection, req completion.Request) completion.Result {
			if saw != nil {
				*saw = req
			}
			return completion.Result{Names: names}
		},
	}
}

// declining returns a source that owns no site.
func declining() completion.Source {
	return completion.Source{
		Claim: func(completion.Request) (completion.Requirements, bool) { return completion.Requirements{}, false },
		Complete: func(context.Context, *completion.Connection, completion.Request) completion.Result {
			return completion.Result{}
		},
	}
}

var oneListing = completion.Requirements{Allowance: completion.AllowanceOne}

// TestEmitResult asserts the answer policy: names sanitized, deduped and
// sorted; an equals-form site gets full tokens; a composite prefix is
// recomposed in front of every survivor; an invalid prefix drops the
// whole result; no survivors answer nil.
func TestEmitResult(t *testing.T) {
	t.Parallel()
	space := CompletionSite{Kind: SiteFlagValue, FlagName: "role", FlagSpelling: "--role"}
	equals := CompletionSite{Kind: SiteFlagValue, FlagName: "machine-config", FlagSpelling: "--machine-config", EqualsForm: true}
	tests := []struct {
		name string
		site CompletionSite
		res  completion.Result
		want []string
	}{
		{name: "sanitize dedupe sort", site: space, res: completion.Result{Names: []string{"b", "a", "b", "bad name!", "-flag", "a/b"}}, want: []string{"a", "b"}},
		{name: "equals form", site: equals, res: completion.Result{Names: []string{"web", "db"}}, want: []string{"--machine-config=db", "--machine-config=web"}},
		{name: "prefix recomposed", site: space, res: completion.Result{Names: []string{"web", "db"}, Prefix: "amazonec2config/"}, want: []string{"amazonec2config/db", "amazonec2config/web"}},
		{name: "prefix and equals form", site: equals, res: completion.Result{Names: []string{"web"}, Prefix: "amazonec2config/"}, want: []string{"--machine-config=amazonec2config/web"}},
		{name: "prefix without slash drops all", site: space, res: completion.Result{Names: []string{"web"}, Prefix: "amazonec2config"}},
		{name: "prefix with two slashes drops all", site: space, res: completion.Result{Names: []string{"web"}, Prefix: "a/b/"}},
		{name: "prefix of a bare slash drops all", site: space, res: completion.Result{Names: []string{"web"}, Prefix: "/"}},
		{name: "prefix outside the charset drops all", site: space, res: completion.Result{Names: []string{"web"}, Prefix: "bad name/"}},
		{name: "no survivors", site: space, res: completion.Result{Names: []string{"bad name!"}}},
		{name: "empty", site: space, res: completion.Result{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, emitResult(tt.site, tt.res))
		})
	}
}

// TestClaimLiveSourceOrder asserts the dispatch order: the command's own
// source wins; a declining command falls through to the shared flag table
// for a flag site; a first positional consults positionalKinds for a
// bespoke command or the kind of a generated verb, then kindSources, then
// the generic kinds source, which declines a kind that does not complete;
// no claim anywhere reports none; a later positional is never claimed.
func TestClaimLiveSourceOrder(t *testing.T) {
	t.Parallel()
	own := completion.Requirements{Allowance: completion.AllowanceOne}
	shared := completion.Requirements{Cluster: completion.ClusterResolved, Allowance: completion.AllowanceOne}
	kind := completion.Requirements{Credential: completion.CredentialManagement, Allowance: completion.AllowanceOne}
	flags := map[string]completion.Source{"cluster": claiming(shared, nil, nil)}
	kindTable := map[string]completion.Source{"user": claiming(kind, nil, nil), "managementcluster": claiming(kind, nil, nil)}
	positional := map[string]string{"inspect cluster": "managementcluster"}
	flagSite := CompletionSite{Kind: SiteFlagValue, FlagName: "cluster"}
	first := CompletionSite{Kind: SitePositional, Index: 0}
	second := CompletionSite{Kind: SitePositional, Index: 1}
	tests := []struct {
		name  string
		site  CompletionSite
		d     runtime.Descriptor
		lc    completion.Completer
		want  completion.Requirements
		owned bool
	}{
		{name: "command source wins over the flag table", site: flagSite, d: runtime.Descriptor{Name: "access projects"}, lc: fakeCompleter{claiming(own, nil, nil)}, want: own, owned: true},
		{name: "declining command falls through to the flag table", site: flagSite, d: runtime.Descriptor{Name: "get pod"}, lc: fakeCompleter{declining()}, want: shared, owned: true},
		{name: "no command source uses the flag table", site: flagSite, d: runtime.Descriptor{Name: "get pod"}, want: shared, owned: true},
		{name: "flag outside the table", site: CompletionSite{Kind: SiteFlagValue, FlagName: "node"}, d: runtime.Descriptor{Name: "get pod"}},
		{name: "bespoke positional through positionalKinds", site: first, d: runtime.Descriptor{Name: "inspect cluster"}, want: kind, owned: true},
		{name: "generated verb positional through the kind key", site: first, d: runtime.Descriptor{Name: "delete user"}, want: kind, owned: true},
		{name: "generated verb kind without a table entry uses the generic source", site: first, d: runtime.Descriptor{Name: "get pod"}, want: completion.Requirements{Cluster: completion.ClusterResolved, Credential: completion.CredentialAny, Allowance: completion.AllowanceOne}, owned: true},
		{name: "generated verb kind that does not complete", site: first, d: runtime.Descriptor{Name: "get secret"}},
		{name: "unknown bespoke positional", site: first, d: runtime.Descriptor{Name: "inspect project"}},
		{name: "second positional never claimed", site: second, d: runtime.Descriptor{Name: "delete user"}},
		{name: "command source owns a positional", site: second, d: runtime.Descriptor{Name: "oidc-client rotate-secret"}, lc: fakeCompleter{claiming(own, nil, nil)}, want: own, owned: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := completion.Request{Flag: tt.site.FlagName, Index: tt.site.Index}
			_, reqs, owned := claimLiveSource(req, tt.d, tt.lc, flags, kindTable, positional)
			assert.Equal(t, tt.owned, owned)
			assert.Equal(t, tt.want, reqs)
		})
	}
}

// TestClusterAndCredentialNeeds asserts the pure checks: a resolved
// cluster need wants --cluster typed, in any spelling (a display name may
// carry a space; the segment rule lives in the path builders); a
// management credential need refuses an exec-credential-only token; an
// exec-credential-only token cannot resolve a display name.
func TestClusterAndCredentialNeeds(t *testing.T) {
	t.Parallel()
	resolved := completion.Requirements{Cluster: completion.ClusterResolved}
	none := completion.Requirements{Cluster: completion.ClusterNone}
	assert.True(t, clusterNeedTyped(resolved, completion.Request{Cluster: "c-x"}))
	assert.True(t, clusterNeedTyped(resolved, completion.Request{Cluster: "prod"}))
	assert.False(t, clusterNeedTyped(resolved, completion.Request{}))
	assert.True(t, clusterNeedTyped(resolved, completion.Request{Cluster: "prod cluster"}))
	assert.True(t, clusterNeedTyped(resolved, completion.Request{Cluster: "a/b"}), "a slash is refused where the resolved id enters a path, not here")
	assert.True(t, clusterNeedTyped(none, completion.Request{}))
	assert.True(t, clusterNeedTyped(none, completion.Request{Cluster: "prod cluster"}))

	mgmt := completion.Requirements{Credential: completion.CredentialManagement}
	anyCred := completion.Requirements{Credential: completion.CredentialAny}
	assert.True(t, credentialNeedMet(mgmt, TokenCapability{Token: "t"}))
	assert.False(t, credentialNeedMet(mgmt, TokenCapability{Token: "t", IDOnly: true}))
	assert.True(t, credentialNeedMet(anyCred, TokenCapability{Token: "t", IDOnly: true}))

	idOnly := TokenCapability{Token: "t", IDOnly: true}
	full := TokenCapability{Token: "t"}
	assert.True(t, clusterResolvable(resolved, completion.Request{Cluster: "c-x"}, idOnly))
	assert.True(t, clusterResolvable(resolved, completion.Request{Cluster: "local"}, idOnly))
	assert.False(t, clusterResolvable(resolved, completion.Request{Cluster: "prod"}, idOnly))
	assert.True(t, clusterResolvable(resolved, completion.Request{Cluster: "prod"}, full))
	assert.True(t, clusterResolvable(none, completion.Request{Cluster: "prod"}, idOnly))
}

// TestTokenCluster asserts which cluster the token is resolved for: the
// typed one for a resolved cluster need, the management cluster for a
// source that works with any credential, none for a management-only
// source.
func TestTokenCluster(t *testing.T) {
	t.Parallel()
	req := completion.Request{Cluster: "c-x"}
	assert.Equal(t, "c-x", tokenCluster(completion.Requirements{Cluster: completion.ClusterResolved, Credential: completion.CredentialAny}, req))
	assert.Equal(t, "c-x", tokenCluster(completion.Requirements{Cluster: completion.ClusterResolved, Credential: completion.CredentialManagement}, req))
	assert.Equal(t, "local", tokenCluster(completion.Requirements{Cluster: completion.ClusterNone, Credential: completion.CredentialAny}, req))
	assert.Empty(t, tokenCluster(completion.Requirements{Cluster: completion.ClusterNone, Credential: completion.CredentialManagement}, req))
}

// countingServer answers every path with a fixed list and counts requests.
func countingServer(t *testing.T, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// liveFixture points the stored config at srv with the network on and a
// management token in the environment.
func liveFixture(t *testing.T, srv *httptest.Server) {
	t.Helper()
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:    config.CompletionConfig{Network: true},
	}
	setStoredConfigForTest(t, cfg, t.TempDir())
	t.Setenv(config.EnvToken, "mgmt-token")
}

// idOnlyFixture points the stored config and the config directory at a
// hermetic directory for srv with the network on and no management token
// anywhere: the only credential is one unexpired exec-credential cache
// entry per named cluster, so the resolved token is exec-credential-only.
// The config directory variable is set too, since the root's Before hook
// resolves the directory again on a full callback and would otherwise
// load the operator's real configuration.
func idOnlyFixture(t *testing.T, srv *httptest.Server, clusters ...string) {
	t.Helper()
	t.Setenv(config.EnvToken, "")
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)
	store := config.NewFileStore(dir)
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:    config.CompletionConfig{Network: true},
	}
	for _, c := range clusters {
		require.NoError(t, store.SetCachedCred(
			config.CacheKey("k8s-ctx", srv.URL, c),
			"exec-token-"+c, time.Now().Add(time.Hour),
			"k8s-ctx", srv.URL, c,
			config.CacheBindingForSeed(dir, cfg, "k8s-ctx", srv.URL, c),
		))
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	setStoredConfigForTest(t, cfg, dir)
}

// TestLiveSourceCandidatesPolicy asserts the surface's policy around a
// claimed source: an allowance outside the class answers nothing without
// a request; a resolved cluster need without --cluster answers nothing; a
// typed display name costs one resolution request and reaches the source
// as the id; a source that ignores the refusal of an extra request and
// still answers has its whole result dropped; a source with no cluster
// need is not held back by an unusable --cluster value.
// NOT parallel: mutates storedConfig and the token environment variable.
func TestLiveSourceCandidatesPolicy(t *testing.T) {
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "x"}
	d := runtime.Descriptor{Name: "x"}
	none := map[string]completion.Source{}
	noPositional := map[string]string{}

	t.Run("allowance outside the class", func(t *testing.T) {
		srv, hits := countingServer(t, `{"items":[]}`)
		liveFixture(t, srv)
		lc := fakeCompleter{claiming(completion.Requirements{Allowance: 0}, []string{"a"}, nil)}
		got, owned := liveSourceCandidates(t.Context(), site, d, lc, nil, none, none, noPositional)
		assert.True(t, owned)
		assert.Nil(t, got)
		assert.Zero(t, hits.Load())
	})

	t.Run("cluster need without a typed cluster", func(t *testing.T) {
		srv, hits := countingServer(t, `{"items":[]}`)
		liveFixture(t, srv)
		lc := fakeCompleter{claiming(completion.Requirements{Cluster: completion.ClusterResolved, Allowance: completion.AllowanceOne}, []string{"a"}, nil)}
		got, owned := liveSourceCandidates(t.Context(), site, d, lc, nil, none, none, noPositional)
		assert.True(t, owned)
		assert.Nil(t, got)
		assert.Zero(t, hits.Load())
		got, _ = liveSourceCandidates(t.Context(), site, d, lc, []string{"--cluster", "prod cluster"}, none, none, noPositional)
		assert.Nil(t, got, "a display name the collection does not carry resolves to nothing")
		assert.Equal(t, int32(1), hits.Load(), "one resolution request, counted apart")
	})

	t.Run("the positionals reach the source with flag values consumed", func(t *testing.T) {
		srv, hits := countingServer(t, `{"items":[]}`)
		liveFixture(t, srv)
		var saw completion.Request
		lc := fakeCompleter{claiming(oneListing, []string{"a"}, &saw)}
		pooled := runtime.Descriptor{Name: "x", Flags: []runtime.ArgSpec{{Name: "pool", Type: "string"}, {Name: "namespace", Type: "string"}, {Name: "yes", Type: "bool"}}}
		tail := []string{"--output", "json", "prod", "--namespace", "team-a", "--yes", "second", "--pool=p", "--pool"}
		got, owned := liveSourceCandidates(t.Context(), site, pooled, lc, tail, none, none, noPositional)
		assert.True(t, owned)
		assert.Equal(t, []string{"a"}, got)
		assert.Equal(t, []string{"prod", "second"}, saw.Positionals, "a global flag's value, a leaf flag's value and an equals-form token are not positionals; a boolean flag consumes none")
		assert.Equal(t, tail, saw.Tail)
		assert.Zero(t, hits.Load())
	})

	t.Run("a positional site's positionals are every positional in the tail", func(t *testing.T) {
		srv, _ := countingServer(t, `{"items":[]}`)
		liveFixture(t, srv)
		var saw completion.Request
		lc := fakeCompleter{claiming(oneListing, []string{"a"}, &saw)}
		positional := CompletionSite{Kind: SitePositional, Index: 1, Partial: "sec"}
		liveSourceCandidates(t.Context(), positional, d, lc, []string{"my-client"}, none, none, noPositional)
		assert.Equal(t, []string{"my-client"}, saw.Positionals, "the current word is not in the tail, so no positional is dropped")
	})

	t.Run("a spaced display name resolves and the id reaches the source", func(t *testing.T) {
		srv, hits := countingServer(t, `{"items":[{"metadata":{"name":"c-ddd44"},"spec":{"displayName":"prod cluster"}}]}`)
		liveFixture(t, srv)
		var saw completion.Request
		lc := fakeCompleter{claiming(completion.Requirements{Cluster: completion.ClusterResolved, Allowance: completion.AllowanceOne}, []string{"a"}, &saw)}
		got, _ := liveSourceCandidates(t.Context(), site, d, lc, []string{"--cluster", "prod cluster"}, none, none, noPositional)
		assert.Equal(t, []string{"a"}, got)
		assert.Equal(t, "prod cluster", saw.Cluster)
		assert.Equal(t, "c-ddd44", saw.ClusterID)
		assert.Equal(t, int32(1), hits.Load())

		got, _ = liveSourceCandidates(t.Context(), site, d, lc, []string{"--cluster", "c-x/evil"}, none, none, noPositional)
		assert.Equal(t, "c-x/evil", saw.ClusterID, "an id-shaped value is not resolved and reaches the source verbatim; the seam's builders refuse it as a segment (asserted in the kind source's tests)")
		assert.Equal(t, int32(1), hits.Load(), "no resolution request for an id-shaped value")
		assert.Equal(t, []string{"a"}, got, "this fake source builds no path")
	})

	t.Run("cluster id passes through, display name resolves first", func(t *testing.T) {
		srv, hits := countingServer(t, `{"items":[{"metadata":{"name":"c-aaa11"},"spec":{"displayName":"prod"}}]}`)
		liveFixture(t, srv)
		var saw completion.Request
		lc := fakeCompleter{claiming(completion.Requirements{Cluster: completion.ClusterResolved, Allowance: completion.AllowanceOne}, []string{"a"}, &saw)}
		got, _ := liveSourceCandidates(t.Context(), site, d, lc, []string{"--cluster", "c-aaa11", "--project", "p-1"}, none, none, noPositional)
		assert.Equal(t, []string{"a"}, got)
		assert.Equal(t, "c-aaa11", saw.Cluster)
		assert.Equal(t, "c-aaa11", saw.ClusterID)
		assert.Equal(t, "p-1", saw.Project)
		assert.Zero(t, hits.Load(), "an id-shaped cluster costs no resolution request")

		got, _ = liveSourceCandidates(t.Context(), site, d, lc, []string{"--cluster", "prod"}, none, none, noPositional)
		assert.Equal(t, []string{"a"}, got)
		assert.Equal(t, "prod", saw.Cluster)
		assert.Equal(t, "c-aaa11", saw.ClusterID)
		assert.Equal(t, int32(1), hits.Load(), "one resolution request, counted apart from the allowance")

		got, _ = liveSourceCandidates(t.Context(), site, d, lc, []string{"--cluster", "unknown"}, none, none, noPositional)
		assert.Nil(t, got, "an unresolvable display name answers nothing")
	})

	t.Run("a source past its allowance is dropped whole", func(t *testing.T) {
		srv, hits := countingServer(t, `{"items":[{"metadata":{"name":"a"}}]}`)
		liveFixture(t, srv)
		greedy := completion.Source{
			Claim: func(completion.Request) (completion.Requirements, bool) { return oneListing, true },
			Complete: func(ctx context.Context, conn *completion.Connection, _ completion.Request) completion.Result {
				p, err := completion.ManagementPath("management.cattle.io", "v3", "users")
				require.NoError(t, err)
				var list completion.ItemList
				_ = conn.GetJSON(ctx, p, "", &list)
				_ = conn.GetJSON(ctx, p, "", &list) // refused; the error is ignored on purpose
				return completion.Result{Names: list.Names()}
			},
		}
		got, owned := liveSourceCandidates(t.Context(), site, d, fakeCompleter{greedy}, nil, none, none, noPositional)
		assert.True(t, owned)
		assert.Nil(t, got)
		assert.Equal(t, int32(1), hits.Load(), "the second request is never sent")
	})

	t.Run("no cluster need is not held back by an unusable cluster value", func(t *testing.T) {
		srv, hits := countingServer(t, `{"items":[]}`)
		liveFixture(t, srv)
		var saw completion.Request
		lc := fakeCompleter{claiming(oneListing, []string{"b", "a"}, &saw)}
		got, _ := liveSourceCandidates(t.Context(), site, d, lc, []string{"--cluster", "prod cluster"}, none, none, noPositional)
		assert.Equal(t, []string{"a", "b"}, got)
		assert.Equal(t, "prod cluster", saw.Cluster)
		assert.Empty(t, saw.ClusterID)
		assert.Zero(t, hits.Load())
	})

	t.Run("exec-credential-only token refuses a management need without a request", func(t *testing.T) {
		srv, hits := countingServer(t, `{"items":[{"metadata":{"name":"a"}}]}`)
		idOnlyFixture(t, srv, "c-aaa11")
		require.True(t, resolveCompletionToken("c-aaa11").IDOnly, "the fixture must resolve to an exec-credential-only token")
		mgmt := completion.Requirements{Cluster: completion.ClusterResolved, Credential: completion.CredentialManagement, Allowance: completion.AllowanceOne}
		got, owned := liveSourceCandidates(t.Context(), site, d, fakeCompleter{claiming(mgmt, []string{"a"}, nil)}, []string{"--cluster", "c-aaa11"}, none, none, noPositional)
		assert.True(t, owned)
		assert.Nil(t, got)
		assert.Zero(t, hits.Load())
	})

	t.Run("exec-credential-only token serves any-credential sources by cluster id", func(t *testing.T) {
		srv, hits := countingServer(t, `{"items":[{"metadata":{"name":"a"}}]}`)
		idOnlyFixture(t, srv, "c-aaa11", "local")
		var saw completion.Request
		downstream := completion.Requirements{Cluster: completion.ClusterResolved, Credential: completion.CredentialAny, Allowance: completion.AllowanceOne}
		got, _ := liveSourceCandidates(t.Context(), site, d, fakeCompleter{claiming(downstream, []string{"a"}, &saw)}, []string{"--cluster", "c-aaa11"}, none, none, noPositional)
		assert.Equal(t, []string{"a"}, got)
		assert.Equal(t, "c-aaa11", saw.ClusterID)
		assert.Zero(t, hits.Load(), "no resolution request under an id-only token")

		got, _ = liveSourceCandidates(t.Context(), site, d, fakeCompleter{claiming(downstream, []string{"a"}, &saw)}, []string{"--cluster", "prod"}, none, none, noPositional)
		assert.Nil(t, got, "a display name cannot be resolved with an id-only token")
		assert.Zero(t, hits.Load())

		local := completion.Requirements{Cluster: completion.ClusterNone, Credential: completion.CredentialAny, Allowance: completion.AllowanceOne}
		got, _ = liveSourceCandidates(t.Context(), site, d, fakeCompleter{claiming(local, []string{"b"}, &saw)}, []string{"--cluster", "prod cluster"}, none, none, noPositional)
		assert.Equal(t, []string{"b"}, got, "a source with no cluster need is served by the management cluster's cache entry")
		assert.Empty(t, saw.ClusterID)
	})

	t.Run("no claim answers nothing", func(t *testing.T) {
		srv, hits := countingServer(t, `{"items":[]}`)
		liveFixture(t, srv)
		_, owned := liveSourceCandidates(t.Context(), site, d, fakeCompleter{declining()}, nil, none, none, noPositional)
		assert.False(t, owned)
		_, owned = liveSourceCandidates(t.Context(), site, d, nil, nil, none, none, noPositional)
		assert.False(t, owned)
		assert.Zero(t, hits.Load())
	})
}

// TestRoleSiteThroughTheGrantProbe asserts the dispatcher serves grant's
// --role through the command's own source: the zero-dependency probe
// adaptEntry installs the callback on answers the scope's roles, an
// unusable --cluster value on the line does not suppress it, and the
// subject positional is served through the same source.
// NOT parallel: mutates storedConfig and the token environment variable.
func TestRoleSiteThroughTheGrantProbe(t *testing.T) {
	s := newGrantRevokeServer(t)
	liveFixture(t, s.srv)
	probe, ok := rbac.NewGrant(nil).(completion.Completer)
	require.True(t, ok, "grant must own live completion")
	d := runtime.Descriptor{Name: "grant"}

	role := CompletionSite{Kind: SiteFlagValue, FlagName: "role", FlagSpelling: "--role"}
	got := liveCandidates(t.Context(), role, d, probe, []string{"grant", "jane", "--cluster", "prod cluster", "--role"})
	assert.Equal(t, []string{"cluster-owner"}, got)
	assert.Equal(t, map[string]int{roleTemplatesPath: 1}, s.reset())

	got = liveCandidates(t.Context(), role, d, probe, []string{"grant", "jane", "--role"})
	assert.Nil(t, got)
	assert.Equal(t, map[string]int{}, s.reset())

	subject := CompletionSite{Kind: SitePositional, Index: 0}
	got = liveCandidates(t.Context(), subject, d, probe, []string{"grant"})
	assert.Contains(t, got, "u-alice")
	assert.Equal(t, map[string]int{mgmtUsersPath: 1}, s.reset())
}

// TestNewCompletionClientBoundsUnresponsiveServer asserts newCompletionClient
// itself bounds requests via its Options.Timeout, independent of any
// ctx deadline the caller happens to add. The GET is issued with a
// bare, deadline-free context - the same shape a caller that forgot to
// wrap ctx with completionTimeout() would use - so the client's own
// timeout is the only thing that can stop this from hanging forever.
func TestNewCompletionClientBoundsUnresponsiveServer(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(func() { close(block); srv.Close() })

	hc, err := newCompletionClient("")
	require.NoError(t, err)
	start := time.Now()
	p, err := completion.ManagementPath("management.cattle.io", "v3", "clusters")
	require.NoError(t, err)
	var dst any
	err = completion.NewConnection(hc, "token-abc:secret", srv.URL, false, 1).GetJSON(t.Context(), p, "", &dst)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Less(t, elapsed, 5*time.Second, "newCompletionClient must bound the request even with a deadline-free ctx")
}
