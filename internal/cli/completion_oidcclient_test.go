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

// TestLivePositionalBespokeOIDCClientSecretName asserts the DEPENDENT
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
			got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", []string{"my-app"})
			assert.Equal(t, []string{"client-secret-1", "client-secret-2"}, got)
			assert.Equal(t, int32(1), hits.Load())
		})
	}
}

// TestLivePositionalBespokeOIDCClientSecretNameNoFirstPositional asserts the
// dependent-positional gate: a missing or unresolvable first positional
// completes to nothing - never an error into the TAB stream, zero requests.
func TestLivePositionalBespokeOIDCClientSecretNameNoFirstPositional(t *testing.T) {
	t.Parallel()
	srv, hits := oidcClientSecretsServer(t)
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	site := CompletionSite{Kind: SitePositional, Index: 1}
	d := runtime.Descriptor{Name: "oidc-client rotate-secret"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", nil)
	assert.Nil(t, got)
	assert.Zero(t, hits.Load())
}
