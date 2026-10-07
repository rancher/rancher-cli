package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/token"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// k8sList is a minimal Kubernetes-style list response for testing.
// The management K8s API uses the k8s items envelope, not Norman {"data":[...]}.
type k8sList struct {
	Items []map[string]any `json:"items"`
}

func makeListServer(t *testing.T, path string, items []map[string]any) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(k8sList{Items: items})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// The surface's one request of its own: resolving a typed display name
// to a cluster id before a source lists under it.

func TestResolveDisplayNameToClusterIDNotFound(t *testing.T) {
	t.Parallel()
	items := []map[string]any{
		{"metadata": map[string]any{"name": "c-abc"}, "spec": map[string]any{"displayName": "prod"}},
	}
	srv, _ := makeListServer(t, "/k8s/clusters/local/apis/management.cattle.io/v3/clusters", items)
	cap := TokenCapability{Token: "tok", ServerURL: srv.URL}
	hc := &http.Client{}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := resolveDisplayNameToClusterID(ctx, hc, cap, "nonexistent")
	assert.Equal(t, "", got, "not-found must return empty string")
}

func TestResolveDisplayNameToClusterIDAmbiguous(t *testing.T) {
	t.Parallel()
	// Two clusters with the same display name: ambiguous → return "".
	items := []map[string]any{
		{"metadata": map[string]any{"name": "c-abc"}, "spec": map[string]any{"displayName": "prod"}},
		{"metadata": map[string]any{"name": "c-def"}, "spec": map[string]any{"displayName": "prod"}},
	}
	srv, _ := makeListServer(t, "/k8s/clusters/local/apis/management.cattle.io/v3/clusters", items)
	cap := TokenCapability{Token: "tok", ServerURL: srv.URL}
	hc := &http.Client{}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := resolveDisplayNameToClusterID(ctx, hc, cap, "prod")
	assert.Equal(t, "", got, "ambiguous match must return empty string")
}

func TestResolveDisplayNameToClusterIDUnambiguous(t *testing.T) {
	t.Parallel()
	items := []map[string]any{
		{"metadata": map[string]any{"name": "c-abc"}, "spec": map[string]any{"displayName": "prod"}},
		{"metadata": map[string]any{"name": "c-def"}, "spec": map[string]any{"displayName": "staging"}},
	}
	srv, _ := makeListServer(t, "/k8s/clusters/local/apis/management.cattle.io/v3/clusters", items)
	cap := TokenCapability{Token: "tok", ServerURL: srv.URL}
	hc := &http.Client{}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := resolveDisplayNameToClusterID(ctx, hc, cap, "prod")
	assert.Equal(t, "c-abc", got, "single match must return the cluster ID")
}

func TestSanitizeCandidatesDropsHostileNames(t *testing.T) {
	t.Parallel()
	input := []string{
		"valid-name",
		"also.valid",
		"-starts-with-dash",
		"has space",
		"$(inject)",
		"valid123",
		"a",
	}
	got := sanitizeCandidates(input)
	assert.Equal(t, []string{"a", "also.valid", "valid-name", "valid123"}, got)
}

func TestNoNetworkOnStaticSite(t *testing.T) {
	// NOT parallel: mutates storedConfig and RANCHER_CLI_TOKEN env globals.
	// Tests the static-site path specifically: an enum flag-value site must return
	// candidates without any HTTP requests even when network is enabled and a
	// reachable server is configured. Removing the layer-1 enum early return in
	// dispatchSite would cause liveCandidates to run → server gets hit → test fails.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// Network ON, valid token, server configured: live path would be reached if
	// the enum early return in dispatchSite were removed.
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:    config.CompletionConfig{Network: true},
	}
	setStoredConfigForTest(t, cfg, t.TempDir())
	t.Setenv(config.EnvToken, "mgmt-token")

	d := runtime.Descriptor{
		Flags: []runtime.ArgSpec{
			{Name: "by", Type: "string", Enum: []string{"namespace", "project", "node", "pod"}},
		},
	}
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "by", EqualsForm: false}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := dispatchSite(ctx, site, d, nil, nil, []string{})
	assert.Equal(t, []string{"namespace", "node", "pod", "project"}, got)
	assert.Equal(t, int32(0), hits.Load(), "enum static site must yield zero HTTP requests")
}

// TestSchemaVerbFlagValueCompletionSurfaces drives `schema --verb <TAB>`
// through the real Adapt-installed ShellComplete callback (BuildRoot +
// root.Run, a marked line): the flag-value site must surface the descriptor's
// --verb enum via the layer-1 enumCandidates path - offline, sorted, exactly
// the enum set. Hermetic: no config file, network gate off, file credential
// store; a network or keyring touch would fail the empty-config run.
func TestSchemaVerbFlagValueCompletionSurfaces(t *testing.T) {
	// NOT parallel: CaptureArgv mutates rawArgv/rawCurrentWord/rawCompleting globals.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	origArgv, origWord, origProto := rawArgv, rawCurrentWord, rawCompleting
	t.Cleanup(func() {
		rawArgv = origArgv
		rawCurrentWord = origWord
		rawCompleting = origProto
	})
	scriptArgs := []string{"--rancher-completion-proto=2", "schema", "--verb",
		"--rancher-completion-cur=", "--generate-shell-completion"}
	CaptureArgv(scriptArgs)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	require.NoError(t, root.Run(t.Context(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...))))

	// The expected set is the descriptor's own enum (sorted, as
	// resolveStaticEnum emits) - no third hand-copied verb list; the enum's
	// content is asserted against the registry by TestSchemaVerbEnumMatchesCommandVerbs.
	var want []string
	for _, f := range runtime.NewSchemaCommand(nil, "", "", nil).Describe().Flags {
		if f.Name == "verb" {
			want = append(want, f.Enum...)
		}
	}
	require.NotEmpty(t, want, "schema's --verb flag must carry an enum")
	sort.Strings(want)

	got := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	assert.Equal(t, want, got, "schema --verb <TAB> must surface exactly the enum candidates")
}

// TestAuthGetTokenAuthFlowCompletionSurfaces drives `auth get-token --auth-flow <TAB>`
// through the real Adapt-installed ShellComplete callback: the flag-value site
// must surface the descriptor's --auth-flow enum offline, sorted, exactly the
// enum set. Hermetic: no config file, network gate off, file credential store.
func TestAuthGetTokenAuthFlowCompletionSurfaces(t *testing.T) {
	// NOT parallel: CaptureArgv mutates rawArgv/rawCurrentWord/rawCompleting globals.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	origArgv, origWord, origProto := rawArgv, rawCurrentWord, rawCompleting
	t.Cleanup(func() {
		rawArgv = origArgv
		rawCurrentWord = origWord
		rawCompleting = origProto
	})
	scriptArgs := []string{"--rancher-completion-proto=2", "auth", "get-token", "--auth-flow",
		"--rancher-completion-cur=", "--generate-shell-completion"}
	CaptureArgv(scriptArgs)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	require.NoError(t, root.Run(t.Context(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...))))

	var want []string
	for _, f := range token.NewTokenCommand(t.TempDir(), nil, nil).Describe().Flags {
		if f.Name == "auth-flow" {
			want = append(want, f.Enum...)
		}
	}
	require.NotEmpty(t, want, "auth get-token's --auth-flow flag must carry an enum")
	sort.Strings(want)

	got := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	assert.Equal(t, want, got, "auth get-token --auth-flow <TAB> must surface exactly the enum candidates")
}

// TestTokenShimAuthFlowCompletionSurfaces drives `token --auth-flow <TAB>`
// through the real Adapt-installed ShellComplete callback: the flag-value site
// must surface the shim descriptor's --auth-flow enum offline, sorted, exactly
// the enum set. Hermetic: no config file, network gate off, file credential store.
func TestTokenShimAuthFlowCompletionSurfaces(t *testing.T) {
	// NOT parallel: CaptureArgv mutates rawArgv/rawCurrentWord/rawCompleting globals.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	origArgv, origWord, origProto := rawArgv, rawCurrentWord, rawCompleting
	t.Cleanup(func() {
		rawArgv = origArgv
		rawCurrentWord = origWord
		rawCompleting = origProto
	})
	scriptArgs := []string{"--rancher-completion-proto=2", "token", "--auth-flow",
		"--rancher-completion-cur=", "--generate-shell-completion"}
	CaptureArgv(scriptArgs)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	require.NoError(t, root.Run(t.Context(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...))))

	var want []string
	for _, f := range token.NewLegacyTokenCommand(t.TempDir(), nil, nil).Describe().Flags {
		if f.Name == "auth-flow" {
			want = append(want, f.Enum...)
		}
	}
	require.NotEmpty(t, want, "token shim's --auth-flow flag must carry an enum")
	sort.Strings(want)

	got := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	assert.Equal(t, want, got, "token --auth-flow <TAB> must surface exactly the enum candidates")
}

// TestSchemaPositionalCompletionSurfaces drives schema's selector
// candidates through the real Adapt-installed ShellComplete callback
// (BuildRoot + root.Run), the same wiring path the use-context callback tests
// assert. The candidates come from schemaCmd.CompleteArgs via the config-local
// dispatch layer; no network is involved. Multi-token selector candidates
// ("get pods") are dropped at this layer by the completion charset
// (sanitizeCandidates rejects spaces), so the shell sees the single-token
// subset - the full candidate set is asserted at the runtime layer by
// TestSchemaPositionalCompletionShape.
func TestSchemaPositionalCompletionSurfaces(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out

	require.NoError(t, runCompletionRequest(t, root, []string{"--config", dir, "schema"}, ""))
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	require.NotEmpty(t, lines)

	for _, want := range []string{"pod", "pods", "po", "cluster", "managementcluster"} {
		assert.Contains(t, lines, want, "kind names, aliases, and keys surface as candidates")
	}
	kubeconfigs := 0
	for _, l := range lines {
		if l == "kubeconfig" {
			kubeconfigs++
		}
	}
	assert.Equal(t, 1, kubeconfigs, "the kubeconfig command/kind collision surfaces exactly once")
	assert.True(t, sort.StringsAreSorted(lines), "surfaced candidates stay sorted: %v", lines)
	for _, l := range lines {
		assert.NotContains(t, l, " ",
			"the completion charset admits no multi-token candidate at the surface layer")
	}
}

// TestNewConfigViewSortsNames asserts the completion output order for server
// names at the source every delegating command shares.
func TestNewConfigViewSortsNames(t *testing.T) {
	t.Parallel()
	view := newConfigView(&config.Config{
		Servers: map[string]config.Server{"zebra-srv": {}, "alpha-srv": {}, "mid-srv": {}},
	}, "")
	assert.Equal(t, []string{"alpha-srv", "mid-srv", "zebra-srv"}, view.ServerNames())
}
