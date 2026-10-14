package rbac

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeleteUserRefusesWithTLSTrustWhenTheCertificateFails asserts the
// self-delete guard, whose identity probe fails the certificate check,
// still refuses the delete and now reports TLS_TRUST (exit 10) instead of a
// retryable GENERIC refusal, and that no DELETE request reaches the server.
// Mutation: leave the cluster client unwrapped (GENERIC, retryable).
func TestDeleteUserRefusesWithTLSTrustWhenTheCertificateFails(t *testing.T) {
	t.Parallel()
	var deletes atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	_, wrongCA, err := testrancher.LoopbackTLSCertificate()
	require.NoError(t, err)
	rc, err := kube.RESTConfig(config.Effective{URL: srv.URL, CA: wrongCA, Token: "id:secret"}, engine.LocalCluster)
	require.NoError(t, err)
	client, err := provider.NewClusterClient(rc, engine.LocalCluster)
	require.NoError(t, err)

	_, err = planDeleteUser(t, NewDeleteUser(userKind(t), fakeFactory{c: client}), stubInput{args: []string{"u-other"}})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	assert.Equal(t, 10, ce.ExitCode())
	assert.False(t, ce.Retryable)
	assert.Zero(t, deletes.Load(), "the refused delete sends no DELETE")
}
