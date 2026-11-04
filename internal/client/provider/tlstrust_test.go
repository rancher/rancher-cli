package provider

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var providerSlotRemedy = tlstrust.Remedy{CA: "; CA-SLOT", URL: "URL-SLOT", Fix: "FIX-SLOT"}

// untrustedEffective is a connection to a TLS server whose certificate the
// saved CA does not match, so every request fails the certificate check.
func untrustedEffective(t *testing.T) config.Effective {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	t.Cleanup(srv.Close)
	_, wrongCA, err := testrancher.LoopbackTLSCertificate()
	require.NoError(t, err)
	return config.Effective{URL: srv.URL, CA: wrongCA, Token: "id:secret", ServerName: "acme", TLSRemedy: providerSlotRemedy}
}

func requireClassified(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	assert.Equal(t, 10, ce.ExitCode())
	assert.False(t, ce.Retryable)
	assert.Contains(t, ce.Message, "; CA-SLOT;")
}

// TestMappersPassATLSTrustErrorThrough asserts the status mappers return
// the TLS_TRUST error of the wrapped transport unchanged: the connectivity
// preflight (retryable GENERIC for any other no-answer error) and the
// request mapper. Mutation: map any error without a status to GENERIC.
func TestMappersPassATLSTrustErrorThrough(t *testing.T) {
	trust := clierr.New(clierr.CodeTLSTrust, "untrusted")
	wrapped := &url.Error{Op: "Get", URL: "https://rancher.example/version", Err: trust}

	assert.Same(t, trust, mapPreflightErr(t.Context(), http.MethodGet, config.Effective{}, wrapped))
	assert.Same(t, trust, mapRequestErr(t.Context(), http.MethodGet, wrapped, &clierr.Request{Cluster: "c-1"}))
}

// TestLocalProbesClassifyACertificateFailure asserts the requests auth
// status and the login verify step make against the local cluster - the
// version preflight, the owner read, the identity read - return TLS_TRUST
// worded by the connection's remedy. Mutation: leave kube.RESTConfig
// without its wrapper.
func TestLocalProbesClassifyACertificateFailure(t *testing.T) {
	eff := untrustedEffective(t)
	t.Run("Preflight", func(t *testing.T) {
		_, err := Preflight(t.Context(), eff)
		requireClassified(t, err)
	})
	t.Run("Owner", func(t *testing.T) {
		_, err := Owner(t.Context(), eff)
		requireClassified(t, err)
	})
	t.Run("Identity", func(t *testing.T) {
		_, err := Identity(t.Context(), eff)
		requireClassified(t, err)
	})
}

// TestPostReviewKeepsItsRetryableDegrade asserts a certificate failure of a
// review post is still the retryable GENERIC error a fan-out degrades on,
// whose text carries the classified message. Mutation: return the
// TLS_TRUST error from PostReview.
func TestPostReviewKeepsItsRetryableDegrade(t *testing.T) {
	eff := untrustedEffective(t)
	rc, err := kube.RESTConfig(eff, "c-1")
	require.NoError(t, err)
	c, err := NewClusterClient(rc, "c-1")
	require.NoError(t, err)
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "authorization.k8s.io/v1", "kind": "SelfSubjectAccessReview"}}
	_, err = c.PostReview(t.Context(), engine.ReviewRequest{Kind: engine.ReviewSelfSubjectAccess, Object: obj})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.True(t, ce.Retryable)
	assert.Contains(t, ce.Message, "no response from the cluster: ")
	assert.Contains(t, ce.Message, "; CA-SLOT;", "the classified message is in the text")
}
