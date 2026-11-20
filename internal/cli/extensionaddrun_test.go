package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const addRepoPath = "/k8s/clusters/local/apis/catalog.cattle.io/v1/clusterrepos"

// addRepoPost is one POST the server received.
type addRepoPost struct {
	dryRun       string
	fieldManager string
	body         map[string]any
}

// addRepoServer serves an empty ClusterRepo list on the local cluster and
// records each create.
type addRepoServer struct {
	mu    sync.Mutex
	posts []addRepoPost
}

func (s *addRepoServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == addRepoPath:
		_ = json.NewEncoder(w).Encode(map[string]any{
			"apiVersion": "catalog.cattle.io/v1", "kind": "ClusterRepoList",
			"metadata": map[string]any{"resourceVersion": "1"}, "items": []any{},
		})
	case r.Method == http.MethodPost && r.URL.Path == addRepoPath:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		q := r.URL.Query()
		s.mu.Lock()
		s.posts = append(s.posts, addRepoPost{dryRun: q.Get("dryRun"), fieldManager: q.Get("fieldManager"), body: body})
		s.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(body)
	default:
		http.NotFound(w, r)
	}
}

func (s *addRepoServer) created() []addRepoPost {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]addRepoPost(nil), s.posts...)
}

// addRepoEnv points the CLI at the server. Not for a parallel test: it sets
// the process environment.
func addRepoEnv(t *testing.T) *addRepoServer {
	t.Helper()
	restoreRawArgv(t)
	extraArgsEnv(t)
	s := &addRepoServer{}
	srv := httptest.NewTLSServer(s)
	t.Cleanup(srv.Close)
	t.Setenv(config.EnvURL, srv.URL)
	t.Setenv(config.EnvCACert, string(pemFromServer(t, srv)))
	return s
}

// TestExtensionAddRepositoryPreviewSendsOnlyADryRun asserts that with
// --dry-run the command exits 0 and sends one create, a server-side dry-run
// with the field manager, and that the Effect says so; and that without
// --dry-run or --apply it asks for one and writes nothing.
func TestExtensionAddRepositoryPreviewSendsOnlyADryRun(t *testing.T) {
	s := addRepoEnv(t)
	stdout, stderr, code := runBounded(t, newRoot(t), []string{"extension", "add-repository", "partner-extensions", "--dry-run", "-o", "json"})
	require.Zero(t, code, stderr)
	data, _ := saveEnvelope(t, stdout)
	assert.Equal(t, "serverDryRun", data["previewSource"])
	assert.Equal(t, false, data["destructive"])
	posts := s.created()
	require.Len(t, posts, 1, "the preview is one dry-run create")
	assert.NotEmpty(t, posts[0].dryRun)
	assert.Equal(t, "cli.rancher.io", posts[0].fieldManager)

	s = addRepoEnv(t)
	_, stderr, code = runBounded(t, newRoot(t), []string{"extension", "add-repository", "partner-extensions", "-o", "json"})
	assert.NotZero(t, code)
	assert.Equal(t, clierr.CodeConfirmationRequired, renderedError(t, "json", stderr).Code)
	for _, p := range s.created() {
		assert.NotEmpty(t, p.dryRun, "without --apply every create is a server-side dry-run")
	}
}

// TestExtensionAddRepositoryApplyNeedsOnlyApply asserts --apply alone creates
// the ClusterRepo, without --yes, and that the dry-run create and the real
// create both carry the field manager and hold exactly the fields the Rancher
// UI sends.
func TestExtensionAddRepositoryApplyNeedsOnlyApply(t *testing.T) {
	s := addRepoEnv(t)
	stdout, stderr, code := runBounded(t, newRoot(t), []string{"extension", "add-repository", "partner-extensions", "--apply", "-o", "json"})
	require.Zero(t, code, stderr)
	_, warnings := saveEnvelope(t, stdout)
	assert.NotEmpty(t, warnings)
	posts := s.created()
	require.Len(t, posts, 2)
	assert.NotEmpty(t, posts[0].dryRun)
	assert.Empty(t, posts[1].dryRun)
	for _, p := range posts {
		assert.Equal(t, "cli.rancher.io", p.fieldManager)
		assert.Equal(t, map[string]any{
			"apiVersion": "catalog.cattle.io/v1",
			"kind":       "ClusterRepo",
			"metadata":   map[string]any{"name": "partner-extensions"},
			"spec":       map[string]any{"gitRepo": "https://github.com/rancher/partner-extensions", "gitBranch": "main"},
		}, p.body)
	}
}
