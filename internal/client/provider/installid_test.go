package provider

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/httpx"
)

const installIDRequestPath = "/v1/management.cattle.io.settings/install-uuid"

func installIDClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	client, err := httpx.New(httpx.Options{CACert: ca})
	require.NoError(t, err)
	return client
}

// TestTheInstallIDRequestAsksTheRootAPIWithTheBearer asserts the request goes
// to the setting at the host root with the bearer, the Accept header and the
// User-Agent and no impersonation, and returns the setting's value.
func TestTheInstallIDRequestAsksTheRootAPIWithTheBearer(t *testing.T) {
	defer httpx.SetUserAgent("cli.rancher.io/test")()
	var mu sync.Mutex
	var path string
	var header http.Header
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		path, header = r.URL.Path, r.Header.Clone()
		mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"install-uuid","value":"d3b07384-d9a0-4e7a-9c2b-5f6a7b8c9d0e","default":""}`))
	}))
	defer srv.Close()

	fetch := InstallIDFetch(installIDClient(t, srv), srv.URL, "id:secret")
	value, status, err := fetch(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 200, status)
	assert.Equal(t, "d3b07384-d9a0-4e7a-9c2b-5f6a7b8c9d0e", value)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, installIDRequestPath, path)
	assert.Equal(t, "Bearer id:secret", header.Get("Authorization"))
	assert.Equal(t, "application/json", header.Get("Accept"))
	assert.Equal(t, "cli.rancher.io/test", header.Get("User-Agent"))
	assert.Empty(t, header.Values("Impersonate-User"))
}

// TestTheInstallIDRequestReportsWhatCameBack asserts a 2xx body that does not
// decode, has no value or is over the limit gives no value with its status,
// and an error status gives its status with no value and no error.
func TestTheInstallIDRequestReportsWhatCameBack(t *testing.T) {
	rows := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"a value", 200, `{"value":"x"}`, "x"},
		{"an empty value", 200, `{"value":""}`, ""},
		{"no value field", 200, `{"id":"install-uuid"}`, ""},
		{"not JSON", 200, `<html>`, ""},
		{"204 with no body", 204, ``, ""},
		{"a value padded past the limit", 200, `{"value":"x","pad":"` + strings.Repeat("a", httpx.FactsBodyLimit) + `"}`, ""},
		{"401", 401, `{"value":"x"}`, ""},
		{"403", 403, `{"value":"x"}`, ""},
		{"404", 404, ``, ""},
		{"500", 500, `{"value":"x"}`, ""},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(row.status)
				_, _ = w.Write([]byte(row.body))
			}))
			defer srv.Close()
			value, status, err := InstallIDFetch(installIDClient(t, srv), srv.URL, "t")(t.Context())
			require.NoError(t, err)
			assert.Equal(t, row.status, status)
			assert.Equal(t, row.want, value)
		})
	}

	t.Run("no response", func(t *testing.T) {
		value, status, err := InstallIDFetch(&http.Client{Transport: &http.Transport{}}, "https://127.0.0.1:1", "t")(t.Context())
		require.Error(t, err)
		assert.Zero(t, status)
		assert.Empty(t, value)
	})
}

// TestTheInstallIDRequestFollowsOnlySameOriginRedirects asserts a redirect to
// the same origin is followed and a redirect to another origin is refused,
// with no request reaching the other origin.
func TestTheInstallIDRequestFollowsOnlySameOriginRedirects(t *testing.T) {
	var otherHits atomic.Int32
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		_, _ = w.Write([]byte(`{"value":"elsewhere"}`))
	}))
	defer other.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == installIDRequestPath {
			http.Redirect(w, r, "/moved", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`{"value":"moved-here"}`))
	}))
	defer srv.Close()

	value, status, err := InstallIDFetch(installIDClient(t, srv), srv.URL, "t")(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 200, status)
	assert.Equal(t, "moved-here", value)

	cross := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+installIDRequestPath, http.StatusFound)
	}))
	defer cross.Close()
	value, status, err = InstallIDFetch(installIDClient(t, cross), cross.URL, "t")(t.Context())
	assert.Empty(t, value)
	assert.NotEqual(t, 200, status)
	if err == nil {
		assert.Equal(t, http.StatusFound, status)
	}
	assert.Zero(t, otherHits.Load(), "the other origin saw no request")
}

// TestDecodeServerInfoReadsTheVersionAndTheEdition asserts the exported
// decode gives what GetServerInfo gives from the same body, and an error for
// a body that is not JSON.
func TestDecodeServerInfoReadsTheVersionAndTheEdition(t *testing.T) {
	rows := []struct {
		body string
		want ServerInfo
	}{
		{`{"Version":"v2.15.2","GitCommit":"x","RancherPrime":"false"}`, ServerInfo{Version: "v2.15.2", Edition: EditionCommunity}},
		{`{"Version":"v2.15.2","RancherPrime":"true"}`, ServerInfo{Version: "v2.15.2", Edition: EditionPrime}},
		{`{"Version":"v2.15.2","RancherPrime":"maybe"}`, ServerInfo{Version: "v2.15.2", Edition: EditionUnknown}},
		{`{}`, ServerInfo{}},
	}
	for _, row := range rows {
		got, err := DecodeServerInfo([]byte(row.body))
		require.NoError(t, err, row.body)
		assert.Equal(t, row.want, got, row.body)
	}
	_, err := DecodeServerInfo([]byte(`<html>`))
	assert.Error(t, err)
}
