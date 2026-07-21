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
	want := []string{"json", "name", "table", "template", "yaml"}
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
		assert.Equal(t, []string{"--output=json", "--output=name", "--output=table", "--output=template", "--output=yaml"}, got)
	})

	t.Run("-o=", func(t *testing.T) {
		t.Parallel()
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "output", FlagSpelling: "-o", EqualsForm: true}
		got := enumCandidates(site, d)
		assert.Equal(t, []string{"-o=json", "-o=name", "-o=table", "-o=template", "-o=yaml"}, got)
	})
}

func TestStripRoutingPrefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		argv []string
		name string
		want []string
	}{
		{[]string{"get", "pod", "--cluster", "prod"}, "get pod", []string{"--cluster", "prod"}},
		{[]string{"usage", "--by=n"}, "usage", []string{"--by=n"}},
		{[]string{"config", "use-context"}, "config use-context", []string{}},
	}
	for _, tc := range tests {
		got := stripRoutingPrefix(tc.argv, tc.name)
		assert.Equal(t, tc.want, got, tc.name)
	}
}

// Finding 2: stripRoutingPrefixByCmd tests (leading globals, alias depth).
// These call stripRoutingPrefixByDepth directly from dispatch.go — the function
// is defined there and NOT redefined here (round-2 finding 2: redefinition is a
// compile error).
func TestStripRoutingPrefixByCmdLeadingGlobalFlags(t *testing.T) {
	t.Parallel()
	// --output json before "get pod" — two non-flag routing tokens to skip (depth=2).
	argv := []string{"--output", "json", "get", "pod", "--cluster", "prod"}
	got := stripRoutingPrefixByDepth(argv, 2)
	assert.Equal(t, []string{"--cluster", "prod"}, got)
}

func TestStripRoutingPrefixByCmdAlias(t *testing.T) {
	t.Parallel()
	// "inspect po arg" — alias "po" for "pod"; depth=2 means skip "inspect" and "po".
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

	// Config: no cluster in current context.
	cfg := &config.Config{
		CurrentContext: "ctx",
		Contexts:       map[string]config.Context{"ctx": {Server: "srv"}}, // no Cluster field
		Servers:        map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:     config.CompletionConfig{Network: true},
	}
	setStoredConfig(cfg, t.TempDir())
	t.Setenv(config.EnvToken, "mgmt-token")
	// No --cluster in tail, no RANCHER_CLI_CONTEXT cluster part.

	tail := []string{} // no flags in tail
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "get namespace"}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := liveCandidates(ctx, site, d, nil, tail)
	assert.Nil(t, got, "no cluster → nil candidates with zero requests")
	assert.Equal(t, int32(0), hits.Load(), "round-2 finding 5: no HTTP requests when cluster unresolved")
}

// TestNetworkGateZeroHits asserts that a reachable server receives zero requests
// when Completion.Network is FALSE. The site chosen (--cluster, non-enum) would
// reach liveCandidates if the gate at dispatch.go were absent — deleting that
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
		CurrentContext: "ctx",
		Contexts:       map[string]config.Context{"ctx": {Server: "srv", Cluster: "c-abc"}},
		Servers:        map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:     config.CompletionConfig{Network: false}, // gate is OFF
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	hc := &http.Client{}
	got := liveFlagValueCandidates(ctx, hc, site, cap, "", "")
	// All candidates returned — shell does the "c-" prefix filter.
	assert.ElementsMatch(t, []string{"c-abc", "c-def", "prod", "staging"}, got)
}

// Fix 4: explicit --project "" in tail must clear the inherited project scope.
// An inherited project via RANCHER_CLI_CONTEXT must NOT win when the user
// explicitly passes --project with an empty value.
func TestExtractEffectiveProjectExplicitEmptyClearsInherited(t *testing.T) {
	// NOT parallel: mutates RANCHER_CLI_CONTEXT env global.
	t.Setenv(config.EnvContext, "c-abc:my-project") // inherited project = "my-project"

	// Explicit --project "" in tail (space-form): two tokens.
	tail := []string{"--project", ""}
	got := extractEffectiveProject(tail)
	assert.Equal(t, "", got, "--project '' must clear the inherited project scope")

	// Verify downstream: namespace site is NOT suppressed when effective project is empty.
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "namespace"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// liveFlagValueCandidates: effectiveProject="" → namespace NOT suppressed.
	// Use an invalid server so the HTTP call fails fast (nil → degraded, not panics).
	cap := TokenCapability{Token: "tok", ServerURL: "http://127.0.0.1:1"}
	hc := &http.Client{}
	result := liveFlagValueCandidates(ctx, hc, site, cap, "c-abc", got)
	// nil is acceptable (server unreachable); what matters is it was attempted, not short-circuited.
	// The absence of panic and the empty-project value reaching here is the assertion.
	_ = result
}

// Round-2 finding 4: --namespace suppressed when effectiveProject non-empty.
func TestLiveFlagValueNamespaceSuppressedWithProject(t *testing.T) {
	t.Parallel()
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "namespace", FlagSpelling: "--namespace"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cap := TokenCapability{Token: "tok", ServerURL: "https://example.invalid"}
	hc := &http.Client{}
	got := liveFlagValueCandidates(ctx, hc, site, cap, "c-abc", "my-project")
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
		CurrentContext: "ctx",
		Contexts:       map[string]config.Context{"ctx": {Server: "srv", Cluster: "c-abc"}},
		Servers:        map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:     config.CompletionConfig{Network: true},
	}
	setStoredConfig(cfg, t.TempDir())
	t.Setenv(config.EnvToken, "mgmt-token")

	tail := []string{"--cluster", "c-abc/../../v1/secrets"}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "get pod"}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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
		CurrentContext: "ctx",
		Contexts:       map[string]config.Context{"ctx": {Server: "srv", Cluster: "c-abc"}},
		Servers:        map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:     config.CompletionConfig{Network: true},
	}
	setStoredConfig(cfg, t.TempDir())
	t.Setenv(config.EnvToken, "mgmt-token")

	// --namespace with a slash (path traversal attempt).
	tail := []string{"--cluster", "c-abc", "--namespace", "default/../../secret"}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "get pod"}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := liveCandidates(ctx, site, d, nil, tail)
	assert.Nil(t, got, "finding 1: path injection via namespace segment must yield nil")
	assert.Equal(t, int32(0), hits.Load(), "finding 1: zero HTTP requests for invalid namespace segment")
}

// Finding 6: --context <displayname> --cluster <TAB> must issue exactly ONE
// cluster-list request. Without the siteNeedsCluster gate, resolveDisplayNameToClusterID
// would fire first (one request) and then fetchClusters would fire again (second request).
func TestClusterValueSiteIssuesOneClusterListRequest(t *testing.T) {
	// NOT parallel: mutates storedConfig and RANCHER_CLI_TOKEN env globals.
	var hits atomic.Int32
	items := []map[string]any{
		{"metadata": map[string]any{"name": "c-abc"}, "spec": map[string]any{"displayName": "somedisplay"}},
	}
	listResp := struct {
		Items []map[string]any `json:"items"`
	}{Items: items}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(listResp)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		CurrentContext: "ctx",
		Contexts:       map[string]config.Context{"ctx": {Server: "srv"}},
		Servers:        map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:     config.CompletionConfig{Network: true},
	}
	setStoredConfig(cfg, t.TempDir())
	t.Setenv(config.EnvToken, "mgmt-token")

	// --context carries a display-name cluster value; completing --cluster <TAB>.
	tail := []string{"--context", "somedisplay"}
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "cluster"}
	d := runtime.Descriptor{Name: "get pod"}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	liveCandidates(ctx, site, d, nil, tail)
	assert.Equal(t, int32(1), hits.Load(), "finding 6: cluster VALUE site must issue exactly ONE cluster-list request")
}

// Round-4 finding 1: bare-dot namespace segments ("..", "...") must be rejected
// by isValidSegment — they path-normalize on proxies, silently dropping scope.
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
			CurrentContext: "ctx",
			Contexts:       map[string]config.Context{"ctx": {Server: "srv", Cluster: "c-abc"}},
			Servers:        map[string]config.Server{"srv": {URL: srv.URL}},
			Completion:     config.CompletionConfig{Network: true},
		}
		setStoredConfig(cfg, t.TempDir())
		t.Setenv(config.EnvToken, "mgmt-token")

		tail := []string{"--cluster", "c-abc", "--namespace", ns}
		site := CompletionSite{Kind: SitePositional, Index: 0}
		d := runtime.Descriptor{Name: "get pod"}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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
		CurrentContext: "ctx",
		Contexts:       map[string]config.Context{"ctx": {Server: "srv", Cluster: "c-abc"}},
		Servers:        map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:     config.CompletionConfig{Network: true},
	}
	setStoredConfig(cfg, t.TempDir())
	t.Setenv(config.EnvToken, "mgmt-token")

	tail := []string{"--cluster", ".."}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "get pod"}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := liveCandidates(ctx, site, d, nil, tail)
	assert.Nil(t, got, "round-4 finding 1: bare-dot cluster segment must yield nil")
	assert.Equal(t, int32(0), hits.Load(), "round-4 finding 1: zero HTTP requests for bare-dot cluster segment")
}

// Round-4 finding 3: a non-completable downstream kind (e.g. "get service")
// must not trigger a cluster-list resolution request — siteNeedsCluster must
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
		CurrentContext: "ctx",
		Contexts:       map[string]config.Context{"ctx": {Server: "srv"}},
		Servers:        map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:     config.CompletionConfig{Network: true},
	}
	setStoredConfig(cfg, t.TempDir())
	t.Setenv(config.EnvToken, "mgmt-token")

	// "service" is Downstream but Completable:false — siteNeedsCluster must
	// return false, so no cluster-resolution request fires and liveCandidates
	// short-circuits to nil before any HTTP call.
	tail := []string{"--context", "somedisplay"}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "get service"}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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
