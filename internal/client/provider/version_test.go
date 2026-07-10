package provider

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const serverVersionPath = "/k8s/clusters/local/apis/management.cattle.io/v3/settings/server-version"

// newVersionFake starts a TLS fake standing in for Rancher, serving exactly
// the local-cluster proxy path (PS-8's exact-path requirement — a bare
// /apis/... GET must never be attempted, so any other path 404s here as it
// would against a real Rancher).
func newVersionFake(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func effFor(t *testing.T, srv *httptest.Server, cluster string) config.Effective {
	t.Helper()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	return config.Effective{URL: srv.URL, CA: string(certPEM), Token: "id:secret", Cluster: cluster}
}

func TestServerVersionSettingReturnsValue(t *testing.T) {
	t.Parallel()
	srv := newVersionFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != serverVersionPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"management.cattle.io/v3","kind":"Setting","metadata":{"name":"server-version"},"value":"v2.13.0"}`))
	})

	got, err := ServerVersionSetting(context.Background(), effFor(t, srv, "local"))
	require.NoError(t, err)
	assert.Equal(t, "v2.13.0", got)
}

// TestServerVersionSettingAlwaysTargetsLocalCluster pins PS-8's exact-path
// requirement: Settings are management-plane singletons under the LOCAL
// cluster's proxy, regardless of which cluster the caller's context
// currently points at.
func TestServerVersionSettingAlwaysTargetsLocalCluster(t *testing.T) {
	t.Parallel()
	srv := newVersionFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != serverVersionPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"management.cattle.io/v3","kind":"Setting","value":"v2.14.0"}`))
	})

	got, err := ServerVersionSetting(context.Background(), effFor(t, srv, "c-1"))
	require.NoError(t, err, "the request must land on /k8s/clusters/local, not /k8s/clusters/c-1")
	assert.Equal(t, "v2.14.0", got)
}

func TestServerVersionSettingRBACDenied(t *testing.T) {
	t.Parallel()
	srv := newVersionFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","status":"Failure","reason":"Forbidden","code":403}`))
	})

	_, err := ServerVersionSetting(context.Background(), effFor(t, srv, "local"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
}

func TestServerVersionSettingMissing(t *testing.T) {
	t.Parallel()
	srv := newVersionFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","status":"Failure","reason":"NotFound","code":404}`))
	})

	_, err := ServerVersionSetting(context.Background(), effFor(t, srv, "local"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
}

func TestServerVersionSettingNoValueFieldIsEmptyNotError(t *testing.T) {
	t.Parallel()
	srv := newVersionFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"management.cattle.io/v3","kind":"Setting","metadata":{"name":"server-version"}}`))
	})

	got, err := ServerVersionSetting(context.Background(), effFor(t, srv, "local"))
	require.NoError(t, err)
	assert.Empty(t, got)
}
