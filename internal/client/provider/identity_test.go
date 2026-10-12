package provider

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIdentityCreateNamesTheFieldManager asserts that the auth status identity
// probe, which writes through its own dynamic client rather than the
// cluster client, names the CLI as the field manager like every other
// create.
func TestIdentityCreateNamesTheFieldManager(t *testing.T) {
	t.Parallel()
	var gotManager, gotPath string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		gotPath, gotManager = r.URL.Path, r.URL.Query().Get("fieldManager")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"apiVersion":"ext.cattle.io/v1","kind":"SelfUser","metadata":{"name":"probe"},"status":{"userID":"u-1"}}`))
	}))
	t.Cleanup(srv.Close)

	id, err := Identity(t.Context(), config.Effective{URL: srv.URL, CA: serverCA(srv), Token: "id:secret"})
	require.NoError(t, err)
	assert.Equal(t, "u-1", id)
	assert.Equal(t, "/k8s/clusters/local/apis/ext.cattle.io/v1/selfusers", gotPath)
	assert.Equal(t, FieldManager, gotManager)
}
