package kube

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/invocation"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// agentRecorder is a server that records the User-Agent of every request.
type agentRecorder struct {
	mu     sync.Mutex
	agents []string
}

func (a *agentRecorder) take() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	got := a.agents
	a.agents = nil
	return got
}

func newAgentRecorder(t *testing.T) (*agentRecorder, *httptest.Server) {
	t.Helper()
	rec := &agentRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.agents = append(rec.agents, r.Header.Get("User-Agent"))
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

// TestEveryKubeClientSendsTheCLIUserAgent asserts that the dynamic, discovery,
// table and typed clients all send httpx.UserAgent() on every request, also
// when the config the caller passes names a User-Agent of its own: the
// client replaces it. Not parallel: it sets the process-wide value.
func TestEveryKubeClientSendsTheCLIUserAgent(t *testing.T) {
	const want = "cli.rancher.io/kube-test (test/test; agent=codex)"
	t.Cleanup(httpx.SetUserAgent(want))

	for _, callerAgent := range []string{"", "caller/1.0", "Go-http-client/1.1"} {
		t.Run("config agent "+callerAgent, func(t *testing.T) {
			rec, srv := newAgentRecorder(t)
			rc := &rest.Config{Host: srv.URL, Transport: srv.Client().Transport, UserAgent: callerAgent}

			dc, err := Dynamic(rc)
			require.NoError(t, err)
			_, _ = dc.Resource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).List(t.Context(), metav1.ListOptions{})
			_, _ = dc.Resource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).Get(t.Context(), "p", metav1.GetOptions{})
			assert.Equal(t, []string{want, want}, rec.take(), "dynamic client")

			disc, err := Discovery(rc)
			require.NoError(t, err)
			_, _ = disc.ServerVersion()
			got := rec.take()
			require.NotEmpty(t, got, "discovery client sent no request")
			for _, agent := range got {
				assert.Equal(t, want, agent, "discovery client")
			}

			tc, err := TableClient(rc)
			require.NoError(t, err)
			_ = tc.Get().AbsPath("api", "v1", "pods").Do(t.Context()).Error()
			assert.Equal(t, []string{want}, rec.take(), "table client")

			cs, err := Clientset(rc)
			require.NoError(t, err)
			_, _ = cs.CoreV1().Namespaces().Get(t.Context(), "ns", metav1.GetOptions{})
			assert.Equal(t, []string{want}, rec.take(), "typed client")
		})
	}
}

// TestHTTPClientReplacesTheConfigUserAgent asserts that HTTPClient sends
// httpx.UserAgent() when the config it is given names another one, and
// leaves the caller's config unchanged. Not parallel: it sets the
// process-wide value.
func TestHTTPClientReplacesTheConfigUserAgent(t *testing.T) {
	const want = "cli.rancher.io/kube-test (test/test)"
	t.Cleanup(httpx.SetUserAgent(want))
	rec, srv := newAgentRecorder(t)
	rc := &rest.Config{Host: srv.URL, Transport: srv.Client().Transport, UserAgent: "caller/1.0"}

	hc, err := HTTPClient(rc)
	require.NoError(t, err)
	resp, err := hc.Get(srv.URL + "/version")
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.Equal(t, []string{want}, rec.take())
	assert.Equal(t, "caller/1.0", rc.UserAgent, "the caller's config must stay unchanged")
}

// TestUserAgentProductIsTheFieldManager asserts the User-Agent's product
// token is the field manager every Kubernetes write names, so an
// administrator matches audit entries and managed fields by one name.
func TestUserAgentProductIsTheFieldManager(t *testing.T) {
	t.Parallel()
	got := invocation.Detect("v1", func(string) string { return "" }).UserAgent()
	assert.True(t, strings.HasPrefix(got, FieldManager+"/"), "%q does not start with %q", got, FieldManager+"/")
}
