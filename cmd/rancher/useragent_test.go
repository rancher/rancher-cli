package main

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// userAgentRequest is one request the recording server saw.
type userAgentRequest struct {
	path, agent string
}

// TestMainSendsTheInvocationUserAgent asserts, for main run as a process,
// that "auth status" sends every request it makes to the server (the
// /rancherversion read goes through httpx.New, the others through the
// Kubernetes client) with the User-Agent built from the child's own
// environment: the harness and CI variables the child is given appear in
// it, and a child given neither sends none. The child's environment is the
// explicit list below, never the test process's own: an agent running the
// tests sets AI_AGENT. VERSION is "dev" in the test binary.
func TestMainSendsTheInvocationUserAgent(t *testing.T) {
	t.Parallel()
	platform := runtime.GOOS + "/" + runtime.GOARCH
	cases := []struct {
		name  string
		extra []string
		want  string
	}{
		{"inside an agent session in CI", []string{"AI_AGENT=claude-code_2-1-308_agent", "CI=true"}, "cli.rancher.io/dev (" + platform + "; agent=claude-code; ci)"},
		{"exec plugin in an unlisted harness", []string{"KUBERNETES_EXEC_INFO={}", "AI_AGENT=my-harness"}, "cli.rancher.io/dev (" + platform + "; exec-plugin; agent=agent)"},
		{"no variable", nil, "cli.rancher.io/dev (" + platform + ")"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var seen []userAgentRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seen = append(seen, userAgentRequest{r.URL.Path, r.Header.Get("User-Agent")})
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/rancherversion":
					_, _ = w.Write([]byte(`{"Version":"v2.15.0","GitCommit":"abc","RancherPrime":"false"}`))
				case "/k8s/clusters/local/version":
					_, _ = w.Write([]byte(`{"major":"1","minor":"36","gitVersion":"v1.36.0"}`))
				default:
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{}`))
				}
			}))
			t.Cleanup(srv.Close)
			env := append([]string{
				"HOME=" + t.TempDir(),
				"TMPDIR=" + t.TempDir(),
				// A test binary built with -cover warns on stderr at exit
				// when this is unset.
				"GOCOVERDIR=" + t.TempDir(),
				config.EnvConfigDir + "=" + t.TempDir(),
				config.EnvCredentialStore + "=file",
				config.EnvURL + "=" + srv.URL,
				config.EnvToken + "=token-abc:secret",
			}, tc.extra...)

			_, _, _ = runMainProcess(t, env, t.TempDir(), "auth", "status")

			mu.Lock()
			defer mu.Unlock()
			var versionRequests int
			for _, req := range seen {
				assert.Equal(t, tc.want, req.agent, "request to %s", req.path)
				if req.path == "/rancherversion" {
					versionRequests++
				}
			}
			assert.Equal(t, 1, versionRequests, "the /rancherversion read through httpx.New was not seen; requests: %v", seen)
			require.NotEmpty(t, seen)
		})
	}
}
