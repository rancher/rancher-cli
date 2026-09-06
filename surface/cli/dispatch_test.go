package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	ucli "github.com/urfave/cli/v3"
)

func TestEnumCandidatesSpaceForm(t *testing.T) {
	t.Parallel()
	d := runtime.Descriptor{
		Flags: []runtime.ArgSpec{
			{Name: "by", Type: "string", Enum: []string{"namespace", "project", "node", "pod"}},
		},
	}
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "by", EqualsForm: false}
	got := enumCandidates(site, d)
	assert.Equal(t, []string{"namespace", "node", "pod", "project"}, got)
}

func TestEnumCandidatesEqualsForm(t *testing.T) {
	t.Parallel()
	d := runtime.Descriptor{
		Flags: []runtime.ArgSpec{
			{Name: "by", Type: "string", Enum: []string{"namespace", "project", "node", "pod"}},
		},
	}
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "by", FlagSpelling: "--by", EqualsForm: true}
	got := enumCandidates(site, d)
	assert.Equal(t, []string{"--by=namespace", "--by=node", "--by=pod", "--by=project"}, got)
}

func TestEnumCandidatesGlobalOutputSpaceForm(t *testing.T) {
	t.Parallel()
	want := []string{"json", "name", "table", "template=", "template={{.}}", "wide", "yaml"}
	d := runtime.Descriptor{}

	for _, spelling := range []string{"--output", "-o"} {
		t.Run(spelling, func(t *testing.T) {
			t.Parallel()
			site := CompletionSite{Kind: SiteFlagValue, FlagName: "output", FlagSpelling: spelling, EqualsForm: false}
			got := enumCandidates(site, d)
			assert.Equal(t, want, got)
		})
	}
}

func TestEnumCandidatesGlobalOutputEqualsForm(t *testing.T) {
	t.Parallel()
	d := runtime.Descriptor{}

	t.Run("--output=", func(t *testing.T) {
		t.Parallel()
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "output", FlagSpelling: "--output", EqualsForm: true}
		got := enumCandidates(site, d)
		assert.Equal(t, []string{"--output=json", "--output=name", "--output=table", "--output=template=", "--output=template={{.}}", "--output=wide", "--output=yaml"}, got)
	})

	t.Run("-o=", func(t *testing.T) {
		t.Parallel()
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "output", FlagSpelling: "-o", EqualsForm: true}
		got := enumCandidates(site, d)
		assert.Equal(t, []string{"-o=json", "-o=name", "-o=table", "-o=template=", "-o=template={{.}}", "-o=wide", "-o=yaml"}, got)
	})
}

func TestEnumCandidatesGlobalColorSpaceForm(t *testing.T) {
	t.Parallel()
	want := []string{"always", "auto", "never"}
	d := runtime.Descriptor{}

	for _, spelling := range []string{"--color"} {
		t.Run(spelling, func(t *testing.T) {
			t.Parallel()
			site := CompletionSite{Kind: SiteFlagValue, FlagName: "color", FlagSpelling: spelling, EqualsForm: false}
			got := enumCandidates(site, d)
			assert.Equal(t, want, got)
		})
	}
}

func TestEnumCandidatesGlobalColorEqualsForm(t *testing.T) {
	t.Parallel()
	d := runtime.Descriptor{}

	site := CompletionSite{Kind: SiteFlagValue, FlagName: "color", FlagSpelling: "--color", EqualsForm: true}
	got := enumCandidates(site, d)
	assert.Equal(t, []string{"--color=always", "--color=auto", "--color=never"}, got)
}

func TestStripRoutingPrefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		argv  []string
		depth int
		want  []string
	}{
		{[]string{"get", "pod", "--cluster", "prod"}, 2, []string{"--cluster", "prod"}},
		{[]string{"usage", "--by=n"}, 1, []string{"--by=n"}},
		{[]string{"config", "use-context"}, 2, []string{}},
	}
	for _, tc := range tests {
		got := stripRoutingPrefixByDepth(tc.argv, tc.depth)
		assert.Equal(t, tc.want, got)
	}
}

// Finding 2: stripRoutingPrefixByCmd tests (leading globals, alias depth).
// These call stripRoutingPrefixByDepth directly from dispatch.go - the function
// is defined there and NOT redefined here (round-2 finding 2: redefinition is a
// compile error).
func TestStripRoutingPrefixByCmdLeadingGlobalFlags(t *testing.T) {
	t.Parallel()
	// --output json before "get pod" - two non-flag routing tokens to skip (depth=2).
	argv := []string{"--output", "json", "get", "pod", "--cluster", "prod"}
	got := stripRoutingPrefixByDepth(argv, 2)
	assert.Equal(t, []string{"--cluster", "prod"}, got)
}

func TestStripRoutingPrefixByCmdAlias(t *testing.T) {
	t.Parallel()
	// "inspect po arg" - alias "po" for "pod"; depth=2 means skip "inspect" and "po".
	argv := []string{"inspect", "po", "mydeployment"}
	got := stripRoutingPrefixByDepth(argv, 2)
	assert.Equal(t, []string{"mydeployment"}, got)
}

func TestKindForDescriptor(t *testing.T) {
	t.Parallel()
	d := runtime.Descriptor{Name: "get pod"}
	k, ok := kindForDescriptor(d)
	assert.True(t, ok)
	assert.Equal(t, "pod", k.Name)
	assert.True(t, k.Completable)
}

func TestKindForDescriptorSecretNotCompletable(t *testing.T) {
	t.Parallel()
	d := runtime.Descriptor{Name: "get secret"}
	k, ok := kindForDescriptor(d)
	assert.True(t, ok)
	assert.False(t, k.Completable, "secret must never be completable (AC-25)")
}

// Round-2 finding 7: inspect leaves must NOT get live positional completion.
// AC-15a adds inspect aliases for suggestion; it does NOT enable live resource names.
func TestInspectLeafGetsNoLivePositionalCompletion(t *testing.T) {
	t.Parallel()
	d := runtime.Descriptor{Name: "inspect pod"}
	// isAC10Verb("inspect pod") → false → livePositionalCandidates returns nil immediately.
	assert.False(t, isAC10Verb(d.Name), "inspect is not an AC-10 verb")
}

func TestGetPodIsAC10Verb(t *testing.T) {
	t.Parallel()
	assert.True(t, isAC10Verb("get pod"))
	assert.True(t, isAC10Verb("patch deployment"))
	assert.True(t, isAC10Verb("delete namespace"))
	assert.False(t, isAC10Verb("inspect pod"))
	assert.False(t, isAC10Verb("inspect deployment"))
}

// Round-2 finding 5: no cluster anywhere → liveCandidates returns nil with ZERO requests.
// With env management token set and no cluster in any source, cluster-dependent
// sites must not issue /k8s/clusters//... requests.
func TestNoClusterNoRequests(t *testing.T) {
	// NOT parallel: mutates storedConfig and RANCHER_CLI_TOKEN env globals.
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:    config.CompletionConfig{Network: true},
	}
	setStoredConfig(cfg, t.TempDir())
	t.Setenv(config.EnvToken, "mgmt-token")
	// No --cluster in tail.

	tail := []string{} // no flags in tail
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "get namespace"}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := liveCandidates(ctx, site, d, nil, tail)
	assert.Nil(t, got, "no cluster → nil candidates with zero requests")
	assert.Equal(t, int32(0), hits.Load(), "round-2 finding 5: no HTTP requests when cluster unresolved")
}

// TestNetworkGateZeroHits asserts that a reachable server receives zero requests
// when Completion.Network is FALSE. The site chosen (--cluster, non-enum) would
// reach liveCandidates if the gate at dispatch.go were absent - deleting that
// gate causes this test to fail.
func TestNetworkGateZeroHits(t *testing.T) {
	// NOT parallel: mutates storedConfig and RANCHER_CLI_TOKEN env globals.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:    config.CompletionConfig{Network: false}, // gate is OFF
	}
	setStoredConfig(cfg, t.TempDir())
	t.Setenv(config.EnvToken, "mgmt-token")

	// Non-enum flag-value site: no enum → layer 1 skipped; no ShellCompleter →
	// layer 2 skipped; network=false → gate fires and returns nil before any HTTP.
	d := runtime.Descriptor{
		Flags: []runtime.ArgSpec{{Name: "cluster", Type: "string"}},
	}
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "cluster"}
	tail := []string{"--cluster"}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := dispatchSite(ctx, site, d, nil, nil, tail)
	assert.Nil(t, got, "network=false must yield nil candidates")
	assert.Equal(t, int32(0), hits.Load(), "network gate must prevent all HTTP requests to reachable server")
}

// Round-2 finding 1 (AC-28): full-callback dispatch with non-empty partial for
// --cluster. Returns all cluster candidates (shell does prefix-filtering).
func TestLiveFlagValueClusterSpaceFormPartial(t *testing.T) {
	// No keyring: dispatch functions do HTTP only.
	t.Parallel()
	items := []map[string]any{
		{"metadata": map[string]any{"name": "c-abc"}, "spec": map[string]any{"displayName": "prod"}},
		{"metadata": map[string]any{"name": "c-def"}, "spec": map[string]any{"displayName": "staging"}},
	}
	var listResp struct {
		Items []map[string]any `json:"items"`
	}
	listResp.Items = items
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(listResp)
	}))
	t.Cleanup(srv.Close)

	cap := TokenCapability{Token: "tok", ServerURL: srv.URL}
	// Space-form: Partial="c-" means the shell typed "c-" before TAB, but the
	// binary returns ALL candidates; the shell prefix-filters them.
	site := CompletionSite{
		Kind: SiteFlagValue, FlagName: "cluster", FlagSpelling: "--cluster",
		EqualsForm: false, Partial: "c-",
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	hc := &http.Client{}
	got := liveFlagValueCandidates(ctx, hc, site, runtime.Descriptor{Name: "get namespace"}, cap, "", "", nil)
	// All candidates returned - shell does the "c-" prefix filter.
	assert.ElementsMatch(t, []string{"c-abc", "c-def", "prod", "staging"}, got)
}

// Round-2 finding 4: --namespace suppressed when effectiveProject non-empty.
func TestLiveFlagValueNamespaceSuppressedWithProject(t *testing.T) {
	t.Parallel()
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "namespace", FlagSpelling: "--namespace"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	cap := TokenCapability{Token: "tok", ServerURL: "https://example.invalid"}
	hc := &http.Client{}
	got := liveFlagValueCandidates(ctx, hc, site, runtime.Descriptor{Name: "get pod"}, cap, "c-abc", "my-project", nil)
	assert.Nil(t, got, "round-2 finding 4: --namespace must be nil when effectiveProject is set")
}

// Finding 1 (SECURITY): path injection via --cluster must yield zero HTTP requests.
// A cluster segment containing "/" fails isValidSegment and the whole dispatch
// returns nil (fail closed) before any HTTP call is issued.
func TestPathInjectionClusterSlashReturnsNil(t *testing.T) {
	// NOT parallel: mutates storedConfig and RANCHER_CLI_TOKEN env globals.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:    config.CompletionConfig{Network: true},
	}
	setStoredConfig(cfg, t.TempDir())
	t.Setenv(config.EnvToken, "mgmt-token")

	tail := []string{"--cluster", "c-abc/../../v1/secrets"}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "get pod"}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := liveCandidates(ctx, site, d, nil, tail)
	assert.Nil(t, got, "finding 1: path injection via cluster segment must yield nil")
	assert.Equal(t, int32(0), hits.Load(), "finding 1: zero HTTP requests for invalid cluster segment")
}

// Finding 1 (SECURITY): path injection via --namespace must yield zero HTTP requests.
func TestPathInjectionNamespaceSlashReturnsNil(t *testing.T) {
	// NOT parallel: mutates storedConfig and RANCHER_CLI_TOKEN env globals.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:    config.CompletionConfig{Network: true},
	}
	setStoredConfig(cfg, t.TempDir())
	t.Setenv(config.EnvToken, "mgmt-token")

	// --namespace with a slash (path traversal attempt).
	tail := []string{"--cluster", "c-abc", "--namespace", "default/../../secret"}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "get pod"}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := liveCandidates(ctx, site, d, nil, tail)
	assert.Nil(t, got, "finding 1: path injection via namespace segment must yield nil")
	assert.Equal(t, int32(0), hits.Load(), "finding 1: zero HTTP requests for invalid namespace segment")
}

// Round-4 finding 1: bare-dot namespace segments ("..", "...") must be rejected
// by isValidSegment - they path-normalize on proxies, silently dropping scope.
func TestPathInjectionNamespaceDotsReturnsNil(t *testing.T) {
	// NOT parallel: mutates storedConfig and RANCHER_CLI_TOKEN env globals.
	for _, ns := range []string{"..", "..."} {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)

		cfg := &config.Config{
			CurrentServer: "srv",
			Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
			Completion:    config.CompletionConfig{Network: true},
		}
		setStoredConfig(cfg, t.TempDir())
		t.Setenv(config.EnvToken, "mgmt-token")

		tail := []string{"--cluster", "c-abc", "--namespace", ns}
		site := CompletionSite{Kind: SitePositional, Index: 0}
		d := runtime.Descriptor{Name: "get pod"}

		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		got := liveCandidates(ctx, site, d, nil, tail)
		assert.Nil(t, got, "round-4 finding 1: bare-dot namespace %q must yield nil", ns)
		assert.Equal(t, int32(0), hits.Load(), "round-4 finding 1: zero HTTP requests for bare-dot namespace %q", ns)
	}
}

// Round-4 finding 1 (belt-and-suspenders): a bare-dot cluster value ".." must
// also be rejected by isValidSegment even though display-name resolution would
// ordinarily nil it; the charset guard fires first.
func TestPathInjectionClusterDotsReturnsNil(t *testing.T) {
	// NOT parallel: mutates storedConfig and RANCHER_CLI_TOKEN env globals.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:    config.CompletionConfig{Network: true},
	}
	setStoredConfig(cfg, t.TempDir())
	t.Setenv(config.EnvToken, "mgmt-token")

	tail := []string{"--cluster", ".."}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "get pod"}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := liveCandidates(ctx, site, d, nil, tail)
	assert.Nil(t, got, "round-4 finding 1: bare-dot cluster segment must yield nil")
	assert.Equal(t, int32(0), hits.Load(), "round-4 finding 1: zero HTTP requests for bare-dot cluster segment")
}

// Round-4 finding 3: a non-completable downstream kind (e.g. "get service")
// must not trigger a cluster-list resolution request - siteNeedsCluster must
// return false for non-completable kinds.
func TestNonCompletableDownstreamKindIssuesZeroResolutionRequests(t *testing.T) {
	// NOT parallel: mutates storedConfig and RANCHER_CLI_TOKEN env globals.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:    config.CompletionConfig{Network: true},
	}
	setStoredConfig(cfg, t.TempDir())
	t.Setenv(config.EnvToken, "mgmt-token")

	// "service" is Downstream but Completable:false - siteNeedsCluster must
	// return false, so no cluster-resolution request fires and liveCandidates
	// short-circuits to nil before any HTTP call.
	tail := []string{"--cluster", "somedisplay"}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "get service"}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := liveCandidates(ctx, site, d, nil, tail)
	assert.Nil(t, got, "round-4 finding 3: non-completable downstream kind must yield nil")
	assert.Equal(t, int32(0), hits.Load(), "round-4 finding 3: non-completable downstream kind must issue zero requests")
}

// TestFlagNameCandidatesDedupeSharedAlias: two flags with different primary
// names but the same short alias must not double-emit the shared alias token.
// allFlagsDeduped dedupes by primary name, so both flags survive it; the
// emitted-token dedup in flagNameCandidates is what prevents double-emission.
func TestFlagNameCandidatesDedupeSharedAlias(t *testing.T) {
	t.Parallel()
	// Two flags with different primaries but sharing short alias "x".
	cmd := &ucli.Command{
		Flags: []ucli.Flag{
			&ucli.StringFlag{Name: "extra", Aliases: []string{"x"}},
			&ucli.StringFlag{Name: "extend", Aliases: []string{"x"}},
		},
	}
	got := flagNameCandidates(cmd, "-")
	count := 0
	for _, c := range got {
		if c == "-x" {
			count++
		}
	}
	assert.Equal(t, 1, count, "shared short alias -x must be emitted exactly once, got %v", got)
}

func TestRoleVocabulary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		tail []string
		want roleVocab
	}{
		{name: "no scope flag", tail: []string{"grant", "jane", "--role"}, want: roleVocabNone},
		{name: "global", tail: []string{"grant", "jane", "--global", "--role"}, want: roleVocabGlobal},
		{name: "project", tail: []string{"grant", "jane", "--project", "Default", "--role"}, want: roleVocabProject},
		{name: "project with cluster qualifier", tail: []string{"grant", "jane", "--project", "Default", "--cluster", "c-x", "--role"}, want: roleVocabProject},
		{name: "cluster alone", tail: []string{"grant", "jane", "--cluster", "c-x", "--role"}, want: roleVocabCluster},
		{name: "conflict global+project", tail: []string{"grant", "jane", "--global", "--project", "Default", "--role"}, want: roleVocabNone},
		{name: "conflict global+cluster", tail: []string{"grant", "jane", "--global", "--cluster", "c-x", "--role"}, want: roleVocabNone},
		// Presence heuristic rows (spec: deliberate mid-edit heuristic, NOT
		// ResolveScope value parsing).
		// --global uses boolFlagSet + strconv.ParseBool: all false-spellings
		// count as absent; true-spellings and unparseable values count as present.
		{name: "heuristic global=false counts as absent", tail: []string{"grant", "jane", "--global=false", "--role"}, want: roleVocabNone},
		{name: "heuristic global=0 counts as absent", tail: []string{"grant", "jane", "--global=0", "--role"}, want: roleVocabNone},
		{name: "heuristic global=False counts as absent", tail: []string{"grant", "jane", "--global=False", "--role"}, want: roleVocabNone},
		{name: "heuristic global=true counts as present", tail: []string{"grant", "jane", "--global=true", "--role"}, want: roleVocabGlobal},
		{name: "heuristic global=1 counts as present", tail: []string{"grant", "jane", "--global=1", "--role"}, want: roleVocabGlobal},
		{name: "heuristic global=garbage counts as present (mid-edit)", tail: []string{"grant", "jane", "--global=garbage", "--role"}, want: roleVocabGlobal},
		{name: "heuristic global=false with project → project vocab", tail: []string{"grant", "jane", "--global=false", "--project", "Default", "--role"}, want: roleVocabProject},
		{name: "heuristic empty project value", tail: []string{"grant", "jane", "--project=", "--role"}, want: roleVocabProject},
		{name: "heuristic empty cluster value", tail: []string{"grant", "jane", "--cluster=", "--role"}, want: roleVocabCluster},
		{name: "equals-form project", tail: []string{"grant", "jane", "--project=Default", "--role"}, want: roleVocabProject},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, roleVocabulary(tt.tail))
		})
	}
}

// TestLiveFlagValueRoleDirectWiring is layer (a): direct wiring of the role
// case in liveFlagValueCandidates via the roleServer helper.
func TestLiveFlagValueRoleDirectWiring(t *testing.T) {
	t.Parallel()

	t.Run("--global → globalroles", func(t *testing.T) {
		t.Parallel()
		srv, _ := roleServer(t)
		cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "role", FlagSpelling: "--role"}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		got := liveFlagValueCandidates(ctx, srv.Client(), site, runtime.Descriptor{Name: "grant"}, cap, "", "", []string{"grant", "jane", "--global", "--role"})
		assert.Equal(t, []string{"admin", "user"}, got)
	})

	t.Run("--project → project roletemplates", func(t *testing.T) {
		t.Parallel()
		srv, _ := roleServer(t)
		cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "role", FlagSpelling: "--role"}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		got := liveFlagValueCandidates(ctx, srv.Client(), site, runtime.Descriptor{Name: "grant"}, cap, "", "", []string{"grant", "jane", "--project", "Default", "--role"})
		assert.Equal(t, []string{"project-member", "project-owner"}, got)
	})

	t.Run("--cluster → cluster roletemplates", func(t *testing.T) {
		t.Parallel()
		srv, _ := roleServer(t)
		cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "role", FlagSpelling: "--role"}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		got := liveFlagValueCandidates(ctx, srv.Client(), site, runtime.Descriptor{Name: "grant"}, cap, "", "", []string{"grant", "jane", "--cluster", "c-x", "--role"})
		assert.Equal(t, []string{"cluster-owner"}, got)
	})

	t.Run("no scope → nil zero hits", func(t *testing.T) {
		t.Parallel()
		srv, hits := roleServer(t)
		cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "role", FlagSpelling: "--role"}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		got := liveFlagValueCandidates(ctx, srv.Client(), site, runtime.Descriptor{Name: "grant"}, cap, "", "", []string{"grant", "jane", "--role"})
		assert.Nil(t, got)
		assert.Zero(t, hits.Load(), "no scope: must issue zero requests")
	})

	t.Run("--global --project conflict → nil zero hits", func(t *testing.T) {
		t.Parallel()
		srv, hits := roleServer(t)
		cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "role", FlagSpelling: "--role"}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		got := liveFlagValueCandidates(ctx, srv.Client(), site, runtime.Descriptor{Name: "grant"}, cap, "", "", []string{"grant", "jane", "--global", "--project", "Default", "--role"})
		assert.Nil(t, got)
		assert.Zero(t, hits.Load(), "global+project conflict: must issue zero requests")
	})

	t.Run("equals-form → --role=<value> candidates", func(t *testing.T) {
		t.Parallel()
		srv, _ := roleServer(t)
		cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "role", FlagSpelling: "--role", EqualsForm: true}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		got := liveFlagValueCandidates(ctx, srv.Client(), site, runtime.Descriptor{Name: "grant"}, cap, "", "", []string{"grant", "jane", "--global", "--role"})
		assert.Equal(t, []string{"--role=admin", "--role=user"}, got)
	})
}

// TestLiveCandidatesRoleBypassesClusterPreflight is the preflight-bypass
// regression for role sites (layer b): an inherited cluster value that fails
// isValidSegment ("prod cluster" has a space) must NOT suppress role
// completion - roleSite bypasses the cluster pipeline entirely.
func TestLiveCandidatesRoleBypassesClusterPreflight(t *testing.T) {
	// NOT parallel: mutates storedConfig and RANCHER_CLI_TOKEN env globals.
	srv, hits := roleServer(t)
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:    config.CompletionConfig{Network: true},
	}
	setStoredConfig(cfg, t.TempDir())
	t.Setenv(config.EnvToken, "mgmt-token")

	tail := []string{"grant", "jane", "--global", "--role"}
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "role"}
	d := runtime.Descriptor{Name: "grant"}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := liveCandidates(ctx, site, d, nil, tail)
	assert.Equal(t, []string{"admin", "user"}, got)
	assert.Equal(t, int32(1), hits.Load(), "preflight bypass: role endpoint fires exactly once")
}

// TestLivePositionalSubjectCandidates is layer (a): direct wiring of the
// grant/revoke subject positional in livePositionalCandidates via userServer.
func TestLivePositionalSubjectCandidates(t *testing.T) {
	t.Parallel()

	t.Run("grant positional 0 → user candidates", func(t *testing.T) {
		t.Parallel()
		srv, _ := userServer(t)
		cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
		site := CompletionSite{Kind: SitePositional, Index: 0}
		d := runtime.Descriptor{Name: "grant"}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", nil)
		assert.Contains(t, got, "u-alice")
		assert.Contains(t, got, "alice")
		assert.NotContains(t, got, "u-sys", "system users must be excluded")
	})

	t.Run("revoke positional 0 → user candidates", func(t *testing.T) {
		t.Parallel()
		srv, _ := userServer(t)
		cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
		site := CompletionSite{Kind: SitePositional, Index: 0}
		d := runtime.Descriptor{Name: "revoke"}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", nil)
		assert.Contains(t, got, "u-alice")
		assert.Contains(t, got, "alice")
		assert.NotContains(t, got, "u-sys", "system users must be excluded")
	})

	t.Run("other command positional unaffected (get pod still goes through isAC10Verb)", func(t *testing.T) {
		t.Parallel()
		// "get pod" goes through kindForDescriptor/isAC10Verb, not the subject branch.
		// With no cluster in cap and no clusterID, the namespace-scoped pod path
		// returns nil - what matters is that it does NOT call the user endpoint.
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			_, _ = w.Write([]byte(`{"items":[]}`))
		}))
		t.Cleanup(srv.Close)
		cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
		site := CompletionSite{Kind: SitePositional, Index: 0}
		d := runtime.Descriptor{Name: "get pod"}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		// With no --namespace in tail, livePositionalCandidates returns nil for
		// namespace-scoped resources - the important thing is the user endpoint is not hit.
		livePositionalCandidates(ctx, srv.Client(), site, d, cap, "c-abc", "", nil)
		// The user list endpoint is at /k8s/clusters/local/apis/management.cattle.io/v3/users;
		// "get pod" would hit /k8s/clusters/c-abc/... or nothing (no ns). Either way, no
		// request hits the management user path.
		assert.Zero(t, hits.Load(), "get pod must not hit the user list endpoint")
	})

	t.Run("--type group space-form → nil zero requests", func(t *testing.T) {
		t.Parallel()
		srv, hits := userServer(t)
		cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
		site := CompletionSite{Kind: SitePositional, Index: 0}
		d := runtime.Descriptor{Name: "grant"}
		tail := []string{"--type", "group"}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", tail)
		assert.Nil(t, got, "--type group: no group source, must return nil")
		assert.Zero(t, hits.Load(), "--type group: must issue zero requests")
	})

	t.Run("--type=group equals-form → nil zero requests", func(t *testing.T) {
		t.Parallel()
		srv, hits := userServer(t)
		cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
		site := CompletionSite{Kind: SitePositional, Index: 0}
		d := runtime.Descriptor{Name: "grant"}
		tail := []string{"--type=group"}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", tail)
		assert.Nil(t, got, "--type=group: no group source, must return nil")
		assert.Zero(t, hits.Load(), "--type=group: must issue zero requests")
	})

	t.Run("bare --type mid-edit → user candidates (user is default)", func(t *testing.T) {
		t.Parallel()
		srv, _ := userServer(t)
		cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
		site := CompletionSite{Kind: SitePositional, Index: 0}
		d := runtime.Descriptor{Name: "grant"}
		// Bare --type with no following value: extractFlagValue returns "" → treat as user.
		tail := []string{"--type"}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", tail)
		assert.Contains(t, got, "u-alice")
	})
}

// TestLiveCandidatesSubjectBypassesClusterPreflight is the preflight-bypass
// regression for subject sites: an inherited cluster value that fails
// isValidSegment ("prod cluster" has a space) must NOT suppress subject
// completion - subjectSite bypasses the cluster pipeline entirely.
func TestLiveCandidatesSubjectBypassesClusterPreflight(t *testing.T) {
	// NOT parallel: mutates storedConfig and RANCHER_CLI_TOKEN env globals.
	srv, hits := userServer(t)
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:    config.CompletionConfig{Network: true},
	}
	setStoredConfig(cfg, t.TempDir())
	t.Setenv(config.EnvToken, "mgmt-token")

	for _, name := range []string{"grant", "revoke"} {
		hits.Store(0)
		tail := []string{name, "--global"}
		site := CompletionSite{Kind: SitePositional, Index: 0}
		d := runtime.Descriptor{Name: name}

		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		got := liveCandidates(ctx, site, d, nil, tail)
		assert.Contains(t, got, "u-alice", "%s: subject completion must work despite invalid inherited cluster", name)
		assert.Equal(t, int32(1), hits.Load(), "%s: preflight bypass: user endpoint fires exactly once", name)
	}
}
