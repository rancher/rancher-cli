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
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// k8sList is a minimal Kubernetes-style list response for testing (finding 10).
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

func TestFetchClustersReturnsSanitizedSorted(t *testing.T) {
	// No keyring.MockInit: fetchClusters does HTTP only, never touches keyring (round-2 finding 9).
	t.Parallel()
	items := []map[string]any{
		{"metadata": map[string]any{"name": "c-abc"}, "spec": map[string]any{"displayName": "prod"}},
		{"metadata": map[string]any{"name": "c-def"}, "spec": map[string]any{"displayName": "staging"}},
	}
	// Finding 10: management K8s API path, not Norman /v3/clusters.
	srv, hits := makeListServer(t, "/k8s/clusters/local/apis/management.cattle.io/v3/clusters", items)
	cap := TokenCapability{Token: "tok", ServerURL: srv.URL}
	hc := &http.Client{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := fetchClusters(ctx, hc, cap, "")
	assert.ElementsMatch(t, []string{"c-abc", "c-def", "prod", "staging"}, got)
	assert.Equal(t, int32(1), hits.Load())
}

func TestFetchClustersDropsSpacedDisplayNames(t *testing.T) {
	// No keyring.MockInit: fetchClusters does HTTP only (round-2 finding 9).
	t.Parallel()
	items := []map[string]any{
		{"metadata": map[string]any{"name": "c-abc"}, "spec": map[string]any{"displayName": "my cluster"}},
	}
	srv, _ := makeListServer(t, "/k8s/clusters/local/apis/management.cattle.io/v3/clusters", items)
	cap := TokenCapability{Token: "tok", ServerURL: srv.URL}
	hc := &http.Client{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := fetchClusters(ctx, hc, cap, "")
	assert.Equal(t, []string{"c-abc"}, got)
}

func TestFetchClustersTimeoutYieldsEmpty(t *testing.T) {
	// No keyring.MockInit: fetchClusters does HTTP only (round-2 finding 9).
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)
	cap := TokenCapability{Token: "tok", ServerURL: srv.URL}
	hc := &http.Client{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	got := fetchClusters(ctx, hc, cap, "")
	assert.Empty(t, got)
}

func TestFetchClusters401YieldsEmpty(t *testing.T) {
	// No keyring.MockInit: fetchClusters does HTTP only (round-2 finding 9).
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	cap := TokenCapability{Token: "tok", ServerURL: srv.URL}
	hc := &http.Client{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := fetchClusters(ctx, hc, cap, "")
	assert.Empty(t, got)
}

// AC-24 / finding 11: two-request path — cluster display-name resolution +
// downstream listing. Finding 3: retargeted at resolveDisplayNameToClusterID
// (the real production path in dispatch.go); resolveClusterDisplayName was dead
// code and has been deleted.
func TestTwoRequestFlowClusterResolution(t *testing.T) {
	// No keyring.MockInit: fetch functions do HTTP only (round-2 finding 9).
	t.Parallel()
	var hits atomic.Int32
	clusterItems := []map[string]any{
		{"metadata": map[string]any{"name": "c-abc"}, "spec": map[string]any{"displayName": "prod"}},
	}
	nsItems := []map[string]any{
		{"metadata": map[string]any{"name": "default"}},
		{"metadata": map[string]any{"name": "kube-system"}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/clusters") && !strings.Contains(r.URL.Path, "namespaces") {
			_ = json.NewEncoder(w).Encode(k8sList{Items: clusterItems})
		} else {
			_ = json.NewEncoder(w).Encode(k8sList{Items: nsItems})
		}
	}))
	t.Cleanup(srv.Close)
	cap := TokenCapability{Token: "tok", ServerURL: srv.URL}
	hc := &http.Client{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Finding 3: use resolveDisplayNameToClusterID (real production function).
	resolvedID := resolveDisplayNameToClusterID(ctx, hc, cap, "prod")
	assert.Equal(t, "c-abc", resolvedID)
	got := fetchK8sResources(ctx, hc, cap, resolvedID, "/api/v1/namespaces", "")
	assert.ElementsMatch(t, []string{"default", "kube-system"}, got)
	assert.LessOrEqual(t, hits.Load(), int32(2), "AC-24: at most 2 HTTP requests")
}

// Finding 3: direct tests for resolveDisplayNameToClusterID edge cases
// (ambiguous and not-found), which the deleted dead twin never covered correctly.

func TestResolveDisplayNameToClusterIDNotFound(t *testing.T) {
	t.Parallel()
	items := []map[string]any{
		{"metadata": map[string]any{"name": "c-abc"}, "spec": map[string]any{"displayName": "prod"}},
	}
	srv, _ := makeListServer(t, "/k8s/clusters/local/apis/management.cattle.io/v3/clusters", items)
	cap := TokenCapability{Token: "tok", ServerURL: srv.URL}
	hc := &http.Client{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := resolveDisplayNameToClusterID(ctx, hc, cap, "nonexistent")
	assert.Equal(t, "", got, "finding 3: not-found must return empty string")
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := resolveDisplayNameToClusterID(ctx, hc, cap, "prod")
	assert.Equal(t, "", got, "finding 3: ambiguous match must return empty string")
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := resolveDisplayNameToClusterID(ctx, hc, cap, "prod")
	assert.Equal(t, "c-abc", got, "finding 3: single match must return the cluster ID")
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

func TestEqualsFormEmit(t *testing.T) {
	t.Parallel()
	candidates := []string{"namespace", "node", "pod", "project"}
	got := equalsFormCandidates("--by", candidates)
	assert.Equal(t, []string{
		"--by=namespace",
		"--by=node",
		"--by=pod",
		"--by=project",
	}, got)
}

// AC-8: projectCandidates must deduplicate its final output.
// Two items with the same display name each appear once after sanitize+dedupe.
func TestProjectCandidatesDeduplicates(t *testing.T) {
	t.Parallel()
	items := []map[string]any{
		{"metadata": map[string]any{"name": "p-aaa"}, "spec": map[string]any{"displayName": "staging"}},
		// Second item with the same display name: displayFreq["staging"]==2 → display dropped,
		// but the two distinct IDs must each appear exactly once (IDs are unique here).
		{"metadata": map[string]any{"name": "p-bbb"}, "spec": map[string]any{"displayName": "staging"}},
		// Third item: unique display name — should appear once alongside its ID.
		{"metadata": map[string]any{"name": "p-ccc"}, "spec": map[string]any{"displayName": "prod"}},
	}
	got := projectCandidates(items)
	// IDs p-aaa, p-bbb, p-ccc + unique display "prod"; "staging" is dropped (not unique).
	assert.Equal(t, []string{"p-aaa", "p-bbb", "p-ccc", "prod"}, got)
	// Verify no duplicates even when raw output would contain them.
	for i := 1; i < len(got); i++ {
		assert.NotEqual(t, got[i-1], got[i], "dedupe: consecutive equal elements at index %d", i)
	}
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

	// Network ON, valid token, cluster in context: live path would be reached if
	// the enum early return in dispatchSite were removed.
	cfg := &config.Config{
		CurrentContext: "ctx",
		Contexts:       map[string]config.Context{"ctx": {Server: "srv", Cluster: "c-abc"}},
		Servers:        map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:     config.CompletionConfig{Network: true},
	}
	setStoredConfig(cfg, t.TempDir())
	t.Setenv(config.EnvToken, "mgmt-token")

	d := runtime.Descriptor{
		Flags: []runtime.ArgSpec{
			{Name: "by", Type: "string", Enum: []string{"namespace", "project", "node", "pod"}},
		},
	}
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "by", EqualsForm: false}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := dispatchSite(ctx, site, d, nil, nil, []string{})
	assert.Equal(t, []string{"namespace", "node", "pod", "project"}, got)
	assert.Equal(t, int32(0), hits.Load(), "enum static site must yield zero HTTP requests")
}

// TestSchemaVerbFlagValueCompletionSurfaces drives `schema --verb <TAB>`
// through the real Adapt-installed ShellComplete callback (BuildRoot +
// root.Run, v2 protocol): the flag-value site must surface the descriptor's
// --verb enum via the layer-1 enumCandidates path — offline, sorted, exactly
// the enum set. Hermetic: no config file, network gate off, file credential
// store; a network or keyring touch would fail the empty-config run.
func TestSchemaVerbFlagValueCompletionSurfaces(t *testing.T) {
	// NOT parallel: CaptureArgv mutates rawArgv/rawCurrentWord/rawProtoV2 globals.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	origArgv, origWord, origProto := rawArgv, rawCurrentWord, rawProtoV2
	t.Cleanup(func() { rawArgv = origArgv; rawCurrentWord = origWord; rawProtoV2 = origProto })
	scriptArgs := []string{"--rancher-completion-proto=2", "schema", "--verb",
		"--rancher-completion-cur=", "--generate-shell-completion"}
	CaptureArgv(scriptArgs)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	require.NoError(t, root.Run(context.Background(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...))))

	// The expected set is the descriptor's own enum (sorted, as
	// resolveStaticEnum emits) — no third hand-copied verb list; the enum's
	// content is pinned to the registry by TestSchemaVerbEnumMatchesCommandVerbs.
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

// Finding 4: unit-test the generic GVR path builders directly. A synthetic
// downstream kind added to the kinds table gets the correct API path without
// any dispatch edit.
func TestGVRPathCoreGroup(t *testing.T) {
	t.Parallel()
	k := kinds.Kind{
		GVR: schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
	}
	assert.Equal(t, "/api/v1/pods", gvrPath(k))
	assert.Equal(t, "/api/v1/namespaces/{namespace}/pods", gvrNamespacedPath(k))
}

func TestGVRPathNamedGroup(t *testing.T) {
	t.Parallel()
	k := kinds.Kind{
		GVR: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
	}
	assert.Equal(t, "/apis/apps/v1/deployments", gvrPath(k))
	assert.Equal(t, "/apis/apps/v1/namespaces/{namespace}/deployments", gvrNamespacedPath(k))
}

// TestSchemaPositionalCompletionSurfaces drives schema's SE-12 selector
// candidates through the real Adapt-installed ShellComplete callback
// (BuildRoot + root.Run), the same wiring path the use-context callback tests
// pin. The candidates come from schemaCmd.CompleteArgs via the config-local
// dispatch layer; no network is involved. Multi-token selector candidates
// ("get pods") are dropped at this layer by the AC-21 completion charset
// (sanitizeCandidates rejects spaces), so the shell sees the single-token
// subset — the full candidate set is pinned at the runtime layer by
// TestSchemaPositionalCompletionShape.
func TestSchemaPositionalCompletionSurfaces(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out

	require.NoError(t, root.Run(context.Background(), []string{
		"rancher", "--config", dir, "schema", "--generate-shell-completion",
	}))
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
			"the AC-21 charset admits no multi-token candidate at the surface layer")
	}
}
