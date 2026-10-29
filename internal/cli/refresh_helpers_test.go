package cli

import (
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/credhelper"
	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

const (
	versionPath   = "/rancherversion"
	installIDPath = "/v1/management.cattle.io.settings/install-uuid"
	goodUUID      = "d3b07384-d9a0-4e7a-9c2b-5f6a7b8c9d0e"
	goodVersion   = `{"Version":"v2.15.2","GitCommit":"abc","RancherPrime":"false"}`
)

// factsHit is one request a test server saw.
type factsHit struct {
	path   string
	header http.Header
	at     time.Time
}

// factsServer is a TLS test server that counts the requests it gets. The two
// facts paths answer through routes that default to a good answer; a self
// subject review answers as u-1; every other path answers 404 unless a test
// adds a route for it.
type factsServer struct {
	*httptest.Server
	ca      string
	mu      sync.Mutex
	hits    []factsHit
	lastOwn time.Time
	routes  map[string]http.HandlerFunc
}

func newFactsServer(t *testing.T) *factsServer {
	t.Helper()
	s := &factsServer{routes: map[string]http.HandlerFunc{
		versionPath: func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(goodVersion)) },
		installIDPath: func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"value":"` + goodUUID + `"}`))
		},
	}}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits = append(s.hits, factsHit{path: r.URL.Path, header: r.Header.Clone(), at: time.Now()})
		route := s.routes[r.URL.Path]
		s.mu.Unlock()
		if r.URL.Path != versionPath && r.URL.Path != installIDPath {
			defer func() {
				s.mu.Lock()
				s.lastOwn = time.Now()
				s.mu.Unlock()
			}()
		}
		switch {
		case route != nil:
			route(w, r)
		case strings.HasSuffix(r.URL.Path, "/selfsubjectreviews"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview","status":{"userInfo":{"username":"u-1"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.StartTLS()
	t.Cleanup(s.Close)
	s.ca = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}))
	return s
}

// handle sets the answer for a path.
func (s *factsServer) handle(path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[path] = h
}

// hitsOn returns the requests the server saw on path.
func (s *factsServer) hitsOn(path string) []factsHit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(s.hits), func(h factsHit) bool { return h.path != path })
}

// factsHits returns how many requests the server saw on the two facts paths.
func (s *factsServer) factsHits() (version, install int) {
	return len(s.hitsOn(versionPath)), len(s.hitsOn(installIDPath))
}

// paths returns the path of every request in the order they arrived.
func (s *factsServer) paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, h := range s.hits {
		out = append(out, h.path)
	}
	return out
}

// clearHits forgets the requests seen so far.
func (s *factsServer) clearHits() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits = nil
}

// canonical is the canonical URL the facts file of the server is named by.
func (s *factsServer) canonical(t *testing.T) string {
	t.Helper()
	c, err := credhelper.CanonicalServerURL(s.URL)
	require.NoError(t, err)
	return c
}

// useServer points the environment at the server with a token and its CA.
func (s *factsServer) useServer(t *testing.T) {
	t.Helper()
	t.Setenv(config.EnvURL, s.URL)
	t.Setenv(config.EnvToken, "token-x:y")
	t.Setenv(config.EnvCACert, s.ca)
}

// shrinkBudget shortens the budget the facts requests share until the test
// ends.
func shrinkBudget(t *testing.T, d time.Duration) {
	t.Helper()
	old := factsBudget
	factsBudget = d
	t.Cleanup(func() { factsBudget = old })
}

// readFactsOf reads the stored facts of a server, and whether the file exists.
func readFactsOf(t *testing.T, dir, canonical string) (telemetry.Facts, bool) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "telemetry")); err != nil {
		return telemetry.Facts{}, false
	}
	st, err := telemetry.OpenStore(dir)
	require.NoError(t, err)
	defer func() { _ = st.Close() }()
	return st.ReadFacts(canonical)
}

// seedFacts writes the facts file of a server under dir.
func seedFacts(t *testing.T, dir, canonical string, fill func(*telemetry.Facts)) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	st, err := telemetry.OpenStore(dir)
	require.NoError(t, err)
	defer func() { _ = st.Close() }()
	var f telemetry.Facts
	fill(&f)
	require.NoError(t, st.WriteFacts(canonical, f))
}

// serverFiles returns the names in the directory of the facts files.
func serverFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "telemetry", "servers"))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// resetFactsRecords clears the process-wide records now and when the test
// ends. A test that uses it must not call t.Parallel().
func resetFactsRecords(t *testing.T) {
	t.Helper()
	httpx.ClearRecords()
	t.Cleanup(httpx.ClearRecords)
}

// writePEM writes the CA of the server to a file and returns its path.
func writePEM(t *testing.T, s *factsServer) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(p, []byte(s.ca), 0o600))
	return p
}
