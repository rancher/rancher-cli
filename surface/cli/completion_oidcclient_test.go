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
// management-kind fallback branch: `get
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

// oidcClientSecretsServer serves ONE named OIDCClient's status.clientSecrets,
// for the dependent secret-name positional.
func oidcClientSecretsServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/oidcclients/my-app", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"status":{"clientSecrets":{
			"client-secret-1":{},"client-secret-2":{},"evil;rm -rf":{}
		}}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestLivePositionalBespokeOIDCClientOpClientName pins the data-driven
// bespoke-op completion seam: the client positional on
// all three lifecycle verbs reuses the SAME generic listing path get/patch/
// delete oidcclient use, keyed off the op name rather than
// kindForDescriptor's last-token derivation.
func TestLivePositionalBespokeOIDCClientOpClientName(t *testing.T) {
	t.Parallel()
	for _, opName := range []string{"oidc-client add-secret", "oidc-client rotate-secret", "oidc-client remove-secret"} {
		t.Run(opName, func(t *testing.T) {
			t.Parallel()
			srv, hits := oidcClientServer(t)
			cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
			site := CompletionSite{Kind: SitePositional, Index: 0}
			d := runtime.Descriptor{Name: opName}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", nil)
			assert.Equal(t, []string{"my-app", "other-app"}, got)
			assert.Equal(t, int32(1), hits.Load())
		})
	}
}

// TestLivePositionalBespokeOIDCClientSecretName pins the DEPENDENT
// secret-name positional (index 1) on rotate-secret/remove-secret: a
// client-scoped GET of the already-typed first positional's
// status.clientSecrets keys, hostile names sanitized.
func TestLivePositionalBespokeOIDCClientSecretName(t *testing.T) {
	t.Parallel()
	for _, opName := range []string{"oidc-client rotate-secret", "oidc-client remove-secret"} {
		t.Run(opName, func(t *testing.T) {
			t.Parallel()
			srv, hits := oidcClientSecretsServer(t)
			cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
			site := CompletionSite{Kind: SitePositional, Index: 1}
			d := runtime.Descriptor{Name: opName}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", []string{"my-app"})
			assert.Equal(t, []string{"client-secret-1", "client-secret-2"}, got)
			assert.Equal(t, int32(1), hits.Load())
		})
	}
}

// TestLivePositionalBespokeOIDCClientSecretNameNoFirstPositional pins the
// dependent-positional gate: a missing or unresolvable first positional
// completes to nothing — never an error into the TAB stream, zero requests.
func TestLivePositionalBespokeOIDCClientSecretNameNoFirstPositional(t *testing.T) {
	t.Parallel()
	srv, hits := oidcClientSecretsServer(t)
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	site := CompletionSite{Kind: SitePositional, Index: 1}
	d := runtime.Descriptor{Name: "oidc-client rotate-secret"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", nil)
	assert.Nil(t, got)
	assert.Zero(t, hits.Load())
}
