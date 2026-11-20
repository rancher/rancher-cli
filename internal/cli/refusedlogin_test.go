package cli

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
)

// seedStoredLogin points a factory at srv with one entry, acme, that holds a
// stored token with the given provenance lines of config.yaml.
func seedStoredLogin(t *testing.T, srv *httptest.Server, entryLines string) *clientFactory {
	t.Helper()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(certPath, pemFromServer(t, srv), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(
		"currentServer: acme\nservers:\n  acme:\n    url: "+srv.URL+"\n    ca: "+certPath+"\n"+entryLines), 0o600))
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "acme"}, "token-abc:secret"))
	return newClientFactory(dir, store)
}

// TestRefusedStoredTokenNamesTheLoginCommandOnEveryClient asserts a 401 to
// the token of a stored entry names the login command for that entry as its
// hint, through the root client, the action client, the node action client and the cluster clients
// (a read, a pod log read, and a read acting as another user): a pasted token
// names --token-stdin, a login through an auth config names --auth-provider
// with that auth config, and the exec plugin's own login entry names none.
func TestRefusedStoredTokenNamesTheLoginCommandOnEveryClient(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvURL, "")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name  string
		entry string
		want  string
	}{
		{"a pasted token", "    provenance: userSupplied\n", clierr.Rancher + " auth login 'acme' --token-stdin"},
		{"a login through an auth config", "    provenance: derived\n    tokenAuthConfig: corp-ldap\n", clierr.Rancher + " auth login 'acme' --auth-provider 'corp-ldap'"},
		{"a login with no recorded auth config", "    provenance: derived\n", clierr.Rancher + " auth login 'acme' --token-stdin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := seedStoredLogin(t, srv, tc.entry)
			expect := func(label string, err error) {
				t.Helper()
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok, "%s: got %v", label, err)
				assert.Equal(t, clierr.CodeAuth, ce.Code, label)
				assert.Equal(t, tc.want, ce.Hint, label)
			}

			root, err := f.Root()
			require.NoError(t, err)
			_, err = root.GetJSON(t.Context(), "/x")
			expect("root client", err)

			action, err := f.Action()
			require.NoError(t, err)
			_, err = action.PostAction(t.Context(), "/v1/x?action=y", []byte(`{}`))
			var refused *engine.ActionRefused
			require.ErrorAs(t, err, &refused)
			expect("action client", refused.Err)
			require.NotNil(t, refused.Request)
			assert.Equal(t, tc.want, refused.Request.Login.Field(), "the refused action's request carries the login command")

			nodeActions, err := f.NodeActions()
			require.NoError(t, err)
			_, err = nodeActions.GetNode(t.Context(), "c-1", "m-1")
			expect("node action client read", err)
			err = nodeActions.PostNodeAction(t.Context(), "c-1", "m-1", engine.NodeActionDrain, []byte(`{}`))
			var refusedNode *engine.ActionRefused
			require.ErrorAs(t, err, &refusedNode)
			expect("node action client write", refusedNode.Err)
			require.NotNil(t, refusedNode.Request)
			assert.Equal(t, tc.want, refusedNode.Request.Login.Field(), "the refused node action's request carries the login command")

			cluster, err := f.Cluster(t.Context(), "local")
			require.NoError(t, err)
			gvr := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
			_, err = cluster.List(t.Context(), gvr, "", "")
			expect("cluster client read", err)
			_, err = cluster.Logs(t.Context(), "default", "x", engine.LogOptions{})
			expect("cluster client pod log read", err)

			as, err := f.ImpersonatedCluster(t.Context(), "local", engine.Impersonation{User: "u-abc"})
			require.NoError(t, err)
			_, err = as.List(t.Context(), gvr, "", "")
			expect("cluster client read acting as another user", err)
		})
	}

	t.Run("the exec plugin's own login entry", func(t *testing.T) {
		dir := t.TempDir()
		certPath := filepath.Join(dir, "ca.pem")
		require.NoError(t, os.WriteFile(certPath, pemFromServer(t, srv), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(
			"currentServer: exec-login-r.ex\nservers:\n  exec-login-r.ex:\n    url: "+srv.URL+"\n    ca: "+certPath+"\n    provenance: execLogin\n"), 0o600))
		store := config.NewFileStore(dir)
		require.NoError(t, store.SetToken(config.ServerRef{Name: "exec-login-r.ex"}, "token-abc:secret"))
		root, err := newClientFactory(dir, store).Root()
		require.NoError(t, err)
		_, err = root.GetJSON(t.Context(), "/x")
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "got %v", err)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.Empty(t, ce.Hint)
	})

	t.Run("RANCHER_CLI_TOKEN", func(t *testing.T) {
		f := seedStoredLogin(t, srv, "    provenance: userSupplied\n")
		t.Setenv(config.EnvToken, "env-id:env-secret")
		root, err := f.Root()
		require.NoError(t, err)
		_, err = root.GetJSON(t.Context(), "/x")
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "got %v", err)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.Empty(t, ce.Hint)
	})
}
