package kube

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// TestTableClientClassifiesCertificateFailuresOfTheServer asserts a request
// through the table client returns a TLS_TRUST error worded by the
// effective connection's remedy, like every client built from a RESTConfig.
// Mutation: build the client's transport from a config that drops
// WrapTransport.
func TestTableClientClassifiesCertificateFailuresOfTheServer(t *testing.T) {
	srv := untrustedServer(t)
	eff := config.Effective{URL: srv.URL, Token: "id:secret", TLSRemedy: distinctRemedy}
	rc, err := RESTConfig(eff, "c-1")
	require.NoError(t, err)
	rc.Transport = &http.Transport{}
	t.Cleanup(rc.Transport.(*http.Transport).CloseIdleConnections)

	c, err := TableClient(rc)
	require.NoError(t, err)
	err = c.Get().AbsPath("api", "v1", "pods").Do(t.Context()).Error()
	assertTrustError(t, err)
}

// TestTableClientDecodesATableAndSendsTheConfiguredCredentials asserts the
// client reads a meta.k8s.io Table from the cluster proxy's path with the
// config's bearer token and the CLI's User-Agent.
func TestTableClientDecodesATableAndSendsTheConfiguredCredentials(t *testing.T) {
	t.Parallel()
	var path, auth, agent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth, agent = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"Table","apiVersion":"meta.k8s.io/v1","columnDefinitions":[{"name":"Name","type":"string"}],"rows":[{"cells":["a"]}]}`))
	}))
	t.Cleanup(srv.Close)
	rc := &rest.Config{Host: srv.URL + "/k8s/clusters/c-1", BearerToken: "tok", Transport: srv.Client().Transport}

	c, err := TableClient(rc)
	require.NoError(t, err)
	obj, err := c.Get().AbsPath("api", "v1", "pods").Do(t.Context()).Get()
	require.NoError(t, err)
	table, ok := obj.(*metav1.Table)
	require.True(t, ok, "got %T", obj)
	require.Len(t, table.Rows, 1)
	assert.Equal(t, []any{"a"}, table.Rows[0].Cells)
	assert.Equal(t, "/k8s/clusters/c-1/api/v1/pods", path)
	assert.Equal(t, "Bearer tok", auth)
	assert.Equal(t, httpx.UserAgent(), agent)
}
