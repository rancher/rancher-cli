//go:build !windows

package kubeconfig

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// TestExecEntryEnvWinsOverKubectlsEnvironment asserts, through client-go's
// own exec credential provider, that the plugin kubectl starts runs with
// the recorded directory and the recorded (empty) store variable, whatever
// kubectl's environment names: a real exec of the file's command.
func TestExecEntryEnvWinsOverKubectlsEnvironment(t *testing.T) {
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvCredentialStore, "")
	dir := writeTestConfig(t, "")
	writeLoggedIn(t, dir)

	out := filepath.Join(t.TempDir(), "plugin-env")
	plugin := filepath.Join(t.TempDir(), "plugin")
	script := "#!/bin/sh\nprintf '%s\\n%s\\n' \"$RANCHER_CLI_CONFIG_DIR\" \"${RANCHER_CLI_CREDENTIAL_STORE-unset}\" > " + out + "\n" +
		`echo '{"apiVersion":"client.authentication.k8s.io/v1beta1","kind":"ExecCredential","status":{"token":"t"}}'` + "\n"
	require.NoError(t, os.WriteFile(plugin, []byte(script), 0o755))

	res, err := newTestCommand(dir, config.NewFileStore(dir), stubFactory{}).(runtime.ReadCommand).
		Execute(t.Context(), execInput{s: map[string]string{"cluster": "c-1", "exec-command": plugin}}, nil)
	require.NoError(t, err)
	kc := loadKubeconfig(t, res)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer t", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	kc.Clusters["prod-c-1"].Server = srv.URL
	kc.Clusters["prod-c-1"].CertificateAuthorityData = nil
	kc.Clusters["prod-c-1"].InsecureSkipTLSVerify = true

	// kubectl's own environment names another directory and store.
	t.Setenv(config.EnvConfigDir, filepath.Join(t.TempDir(), "kubectls"))
	t.Setenv(config.EnvCredentialStore, "keychain")

	rc, err := clientcmd.NewDefaultClientConfig(*kc, &clientcmd.ConfigOverrides{}).ClientConfig()
	require.NoError(t, err)
	client, err := rest.HTTPClientFor(rc)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/version", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	got, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, []string{config.CanonicalDir(dir), ""}, strings.Split(strings.TrimSuffix(string(got), "\n"), "\n"))
}
