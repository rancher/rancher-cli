package kube

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/config"
)

// headerServer is a TLS server that keeps the headers and paths of the
// requests it saw.
type headerServer struct {
	*httptest.Server
	ca      string
	mu      sync.Mutex
	headers []http.Header
	paths   []string
}

func newHeaderServer(t *testing.T) *headerServer {
	t.Helper()
	h := &headerServer{}
	h.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.headers = append(h.headers, r.Header.Clone())
		h.paths = append(h.paths, r.URL.Path)
		h.mu.Unlock()
		_, _ = w.Write([]byte(`{"Version":"v2.15.2"}`))
	}))
	t.Cleanup(h.Close)
	h.ca = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: h.Certificate().Raw}))
	return h
}

func resetRecords(t *testing.T) {
	t.Helper()
	httpx.ClearRecords()
	t.Cleanup(httpx.ClearRecords)
}

// TestARequestThroughAnImpersonatingClientRecordsItsHost asserts a request
// sent through a client built from RESTConfigImpersonating, with its bearer
// and Impersonate-User header, records the host that answered, and that a
// version request sent through the recorded transport carries neither
// header. Mutation: move the wrapper into HTTPClient, or set WrapTransport
// again after it.
func TestARequestThroughAnImpersonatingClientRecordsItsHost(t *testing.T) {
	resetRecords(t)
	defer httpx.SetUserAgent("cli.rancher.io/test")()
	srv := newHeaderServer(t)
	rc, err := RESTConfigImpersonating(config.Effective{URL: srv.URL, Token: "id:secret", CA: srv.ca}, "c-1", "u-1")
	require.NoError(t, err)
	hc, err := HTTPClient(rc)
	require.NoError(t, err)
	resp, err := hc.Get(rc.Host + "/version")
	require.NoError(t, err)
	_ = resp.Body.Close()

	srv.mu.Lock()
	require.Equal(t, []string{"/k8s/clusters/c-1/version"}, srv.paths)
	assert.Equal(t, "Bearer id:secret", srv.headers[0].Get("Authorization"))
	assert.Equal(t, "u-1", srv.headers[0].Get("Impersonate-User"))
	srv.headers, srv.paths = nil, nil
	srv.mu.Unlock()

	hosts := httpx.AnsweredHosts()
	require.Len(t, hosts, 1)
	assert.Equal(t, BaseURL(srv.URL), hosts[0].Origin)

	got, err := httpx.RequestVersion(t.Context(), hosts[0].Transport, hosts[0].Origin)
	require.NoError(t, err)
	assert.Equal(t, 200, got.Status)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Equal(t, []string{"/rancherversion"}, srv.paths)
	assert.Empty(t, srv.headers[0].Values("Authorization"))
	assert.Empty(t, srv.headers[0].Values("Impersonate-User"))
	assert.Equal(t, "cli.rancher.io/test", srv.headers[0].Get("User-Agent"))
	assert.Len(t, httpx.AnsweredHosts(), 1, "the version request through the transport records no second host")
}

// TestAnAnswerOfAnyStatusRecordsTheHostAndAFailureRecordsNothing asserts a
// 404 or 500 answer records the host and a refused port or a failed TLS
// handshake records nothing.
func TestAnAnswerOfAnyStatusRecordsTheHostAndAFailureRecordsNothing(t *testing.T) {
	t.Run("an error status", func(t *testing.T) {
		resetRecords(t)
		srv := httptest.NewTLSServer(http.NotFoundHandler())
		defer srv.Close()
		ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
		rc, err := RESTConfig(config.Effective{URL: srv.URL, Token: "t", CA: ca}, "local")
		require.NoError(t, err)
		hc, err := HTTPClient(rc)
		require.NoError(t, err)
		resp, err := hc.Get(rc.Host + "/version")
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
		assert.Len(t, httpx.AnsweredHosts(), 1)
	})
	t.Run("a refused port", func(t *testing.T) {
		resetRecords(t)
		rc, err := RESTConfig(config.Effective{URL: "https://127.0.0.1:1", Token: "t"}, "local")
		require.NoError(t, err)
		rc.Transport = &http.Transport{}
		t.Cleanup(rc.Transport.(*http.Transport).CloseIdleConnections)
		hc, err := HTTPClient(rc)
		require.NoError(t, err)
		_, err = hc.Get(rc.Host + "/version")
		require.Error(t, err)
		assert.Empty(t, httpx.AnsweredHosts())
	})
	t.Run("a failed TLS handshake", func(t *testing.T) {
		resetRecords(t)
		srv := untrustedServer(t)
		rc, err := RESTConfig(config.Effective{URL: srv.URL, Token: "t"}, "local")
		require.NoError(t, err)
		rc.Transport = &http.Transport{}
		t.Cleanup(rc.Transport.(*http.Transport).CloseIdleConnections)
		hc, err := HTTPClient(rc)
		require.NoError(t, err)
		_, err = hc.Get(rc.Host + "/version")
		require.Error(t, err)
		assert.Empty(t, httpx.AnsweredHosts())
	})
}
