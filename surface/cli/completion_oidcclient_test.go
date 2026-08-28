package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
)

// oidcClientServer serves the management OIDCClient list, counting hits. One
// name carries shell metacharacters to exercise the AC-21 sanitize boundary.
func oidcClientServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/oidcclients", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"items":[
			{"metadata":{"name":"my-app"},"status":{"clientID":"client-abc"}},
			{"metadata":{"name":"other-app"},"status":{"clientID":"client-def"}},
			{"metadata":{"name":"evil;rm -rf"},"status":{"clientID":"client-ghi"}}
		]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestLivePositionalOIDCClientCandidates pins the generic Completable NSNone
// management-kind fallback branch (round-8 finding 2's resolution): `get
// oidcclient <TAB>` lists v3 OIDCClients from the local (management) cluster
// through the same generic path nodedriver uses — no per-kind case needed —
// and hostile names are dropped by the sanitize charset.
func TestLivePositionalOIDCClientCandidates(t *testing.T) {
	t.Parallel()
	srv, hits := oidcClientServer(t)
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "get oidcclient"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", nil)
	assert.Equal(t, []string{"my-app", "other-app"}, got)
	assert.Equal(t, int32(1), hits.Load(), "exactly one management list request")
}

// TestLivePositionalOIDCClientTokenBoundary pins the management-token
// boundary: a missing token or an exec-cred-only (IDOnly) capability cannot
// authorize the management cluster list — nil, with ZERO requests.
func TestLivePositionalOIDCClientTokenBoundary(t *testing.T) {
	t.Parallel()
	srv, hits := oidcClientServer(t)
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "get oidcclient"}
	for _, cap := range []TokenCapability{
		{ServerURL: srv.URL},
		{Token: "downstream", IDOnly: true, ServerURL: srv.URL},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", nil)
		cancel()
		assert.Nil(t, got)
	}
	assert.Zero(t, hits.Load(), "token boundary must return nil WITHOUT a request")
}
