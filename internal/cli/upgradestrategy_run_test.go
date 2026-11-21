package cli

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	strategyImportedPath = "/k8s/clusters/local/apis/management.cattle.io/v3/clusters/c-m-edge1"
	strategyImportedList = "/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters/edge"
	strategyImportedID   = "/v1/management.cattle.io.clusters/c-m-edge1"
)

// strategyImportedServer serves an imported RKE2 cluster whose version
// management is on, and records every PUT.
type strategyImportedServer struct {
	mu   sync.Mutex
	puts []savePut
}

func (s *strategyImportedServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reply := func(obj any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(obj)
	}
	management := map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "Cluster",
		"metadata": map[string]any{"name": "c-m-edge1", "resourceVersion": "9", "annotations": map[string]any{"rancher.io/imported-cluster-version-management": "true"}},
		"spec": map[string]any{"rke2Config": map[string]any{
			"kubernetesVersion":   "v1.31.4+rke2r1",
			"rke2upgradeStrategy": map[string]any{"serverConcurrency": 1, "workerConcurrency": 1},
		}},
		"status": map[string]any{"driver": "rke2", "version": map[string]any{"gitVersion": "v1.31.4+rke2r1"}},
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == strategyImportedList:
		reply(map[string]any{
			"apiVersion": "provisioning.cattle.io/v1", "kind": "Cluster",
			"metadata": map[string]any{"name": "edge", "namespace": "fleet-default", "resourceVersion": "5"},
			"spec":     map[string]any{},
			"status":   map[string]any{"clusterName": "c-m-edge1"},
		})
	case r.Method == http.MethodGet && r.URL.Path == strategyImportedID:
		steve := maps.Clone(management)
		steve["id"], steve["type"] = "c-m-edge1", "management.cattle.io.cluster"
		reply(steve)
	case r.Method == http.MethodPut && r.URL.Path == strategyImportedPath:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		q := r.URL.Query()
		s.mu.Lock()
		s.puts = append(s.puts, savePut{path: r.URL.Path, dryRun: q.Get("dryRun") != "", fieldManager: q.Get("fieldManager"), body: body})
		s.mu.Unlock()
		reply(body)
	default:
		http.NotFound(w, r)
	}
}

func (s *strategyImportedServer) putsOf() []savePut {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.puts)
}

// strategyImportedEnv points the CLI at an imported cluster's server. Not for a
// parallel test: it sets the process environment.
func strategyImportedEnv(t *testing.T) *strategyImportedServer {
	t.Helper()
	restoreRawArgv(t)
	extraArgsEnv(t)
	s := &strategyImportedServer{}
	srv := httptest.NewTLSServer(s)
	t.Cleanup(srv.Close)
	t.Setenv(config.EnvURL, srv.URL)
	t.Setenv(config.EnvCACert, string(pemFromServer(t, srv)))
	return s
}

// TestUpgradeStrategyProvisionedGates asserts the real command on a provisioned
// cluster previews with a server dry-run, as a destructive change that can be
// reversed with this command,
// refuses --apply without --yes with CONFIRMATION_REQUIRED and writes nothing,
// and writes once with --apply --yes.
func TestUpgradeStrategyProvisionedGates(t *testing.T) {
	s, _ := saveEnv(t)
	stdout, stderr, code := runBounded(t, newRoot(t), []string{"cluster", "set-upgrade-strategy", "prod", "--worker-concurrency", "2", "--dry-run", "-o", "json"})
	require.Zero(t, code, stderr)
	data, warnings := saveEnvelope(t, stdout)
	assert.Equal(t, true, data["destructive"])
	assert.Equal(t, true, data["reversible"])
	assert.Equal(t, "serverDryRun", data["previewSource"])
	assert.NotEmpty(t, warnings)
	require.Len(t, s.putsOf(), 1)

	_, stderr, code = runBounded(t, newRoot(t), []string{"cluster", "set-upgrade-strategy", "prod", "--worker-concurrency", "2", "--apply", "-o", "json"})
	assert.NotZero(t, code)
	assert.Equal(t, clierr.CodeConfirmationRequired, renderedError(t, "json", stderr).Code)
	for _, put := range s.putsOf() {
		assert.True(t, put.dryRun, "no write without --yes")
	}

	stdout, stderr, code = runBounded(t, newRoot(t), []string{"cluster", "set-upgrade-strategy", "prod", "--worker-concurrency", "2", "--apply", "--yes", "-o", "json"})
	require.Zero(t, code, stderr)
	_, _ = saveEnvelope(t, stdout)
	puts := s.putsOf()
	require.Len(t, puts, 4, "the first preview, the preview the refusal followed, and the preview and the write of the applied run")
	last := puts[len(puts)-1]
	assert.False(t, last.dryRun)
	assert.Equal(t, "cli.rancher.io", last.fieldManager)
	strategy := last.body["spec"].(map[string]any)["rkeConfig"].(map[string]any)["upgradeStrategy"]
	assert.Equal(t, map[string]any{"workerConcurrency": "2"}, strategy)
}

// TestUpgradeStrategyImportedGates asserts the real command on an imported
// cluster is not destructive, writes with --apply alone, and is not reversible
// only when a flag turns a drain on.
func TestUpgradeStrategyImportedGates(t *testing.T) {
	s := strategyImportedEnv(t)
	stdout, stderr, code := runBounded(t, newRoot(t), []string{"cluster", "set-upgrade-strategy", "edge", "--worker-concurrency", "2", "--dry-run", "-o", "json"})
	require.Zero(t, code, stderr)
	data, _ := saveEnvelope(t, stdout)
	assert.Equal(t, false, data["destructive"])
	assert.Equal(t, true, data["reversible"])
	assert.Equal(t, "serverDryRun", data["previewSource"])

	stdout, stderr, code = runBounded(t, newRoot(t), []string{"cluster", "set-upgrade-strategy", "edge", "--worker-drain", "--dry-run", "-o", "json"})
	require.Zero(t, code, stderr)
	data, warnings := saveEnvelope(t, stdout)
	assert.Equal(t, false, data["reversible"])
	assert.Contains(t, warnings, "turning drain on adds the drain to Rancher's upgrade plan; turning it off later does not remove it")

	stdout, stderr, code = runBounded(t, newRoot(t), []string{"cluster", "set-upgrade-strategy", "edge", "--worker-concurrency", "2", "--apply", "-o", "json"})
	require.Zero(t, code, stderr)
	_, _ = saveEnvelope(t, stdout)
	puts := s.putsOf()
	require.Len(t, puts, 4, "two previews, then the preview and the write of the applied run")
	last := puts[3]
	assert.False(t, last.dryRun)
	assert.Equal(t, "cli.rancher.io", last.fieldManager)
	config := last.body["spec"].(map[string]any)["rke2Config"].(map[string]any)["rke2upgradeStrategy"]
	assert.Equal(t, map[string]any{"serverConcurrency": float64(1), "workerConcurrency": float64(2)}, config)
}

// TestUpgradeStrategyEmptyValueIsRefusedByThePipeline asserts a typed empty
// concurrency is refused with the pipeline's own text before the command runs,
// with no request.
func TestUpgradeStrategyEmptyValueIsRefusedByThePipeline(t *testing.T) {
	s, _ := saveEnv(t)
	s.take()
	_, stderr, code := runBounded(t, newRoot(t), []string{"cluster", "set-upgrade-strategy", "prod", "--worker-concurrency", "", "-o", "json"})
	assert.NotZero(t, code)
	refusal := renderedError(t, "json", stderr)
	assert.Equal(t, clierr.CodeValidation, refusal.Code)
	assert.Equal(t, "--worker-concurrency is empty; pass a value or omit --worker-concurrency", refusal.Message)
	assert.Empty(t, s.take())
}

// TestUpgradeStrategyConcurrencySuggestionsAreAccepted asserts every value the
// concurrency flags offer is accepted by the command for a provisioned and for
// an imported cluster.
func TestUpgradeStrategyConcurrencySuggestionsAreAccepted(t *testing.T) {
	for _, value := range strategyConcurrencySuggestions {
		for _, flag := range []string{"--control-plane-concurrency", "--worker-concurrency"} {
			saveEnv(t)
			_, stderr, code := runBounded(t, newRoot(t), []string{"cluster", "set-upgrade-strategy", "prod", flag, value, "--dry-run", "-o", "json"})
			assert.Zero(t, code, "provisioned %s %s: %s", flag, value, stderr)
			strategyImportedEnv(t)
			_, stderr, code = runBounded(t, newRoot(t), []string{"cluster", "set-upgrade-strategy", "edge", flag, value, "--dry-run", "-o", "json"})
			assert.Zero(t, code, "imported %s %s: %s", flag, value, stderr)
		}
	}
}
