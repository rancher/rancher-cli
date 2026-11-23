package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// TestCloudCredentialHarvesterCreateBody asserts the harvester create, run
// through the real entry against the test facade, sends the one request the
// facade records with the cloud credential type, the display name, the driver
// annotation Rancher's UI writes and the config keys that were typed, and with
// the kubeconfig as the one multi-line value, trimmed. Not parallel: it sets
// the environment.
func TestCloudCredentialHarvesterCreateBody(t *testing.T) {
	restoreRawArgv(t)
	keyring.MockInit()
	var posts []string
	stub := ccStub(ccNorman{}, &posts, false)
	rancher := testrancher.New(testrancher.Options{
		SeedToken:          secretBearer,
		SelfSubjectReviews: true,
		ClusterVisibility: func(b string) ([]testrancher.ClusterFixture, bool) {
			return []testrancher.ClusterFixture{{ID: "c-x", DisplayName: "c-x"}}, b == secretBearer
		},
		ProjectVisibility: func(context.Context, string, string) ([]testrancher.ProjectFixture, bool, error) {
			return nil, true, nil
		},
		NamespaceVisibility: func(string, string) ([]testrancher.NamespaceFixture, bool) { return nil, true },
		Fallthrough: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !stub(w, r) {
				writeKubeJSON(w, http.StatusOK, `{"items":[]}`)
			}
		}),
	})
	srv := httptest.NewTLSServer(rancher)
	t.Cleanup(srv.Close)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv(config.EnvConfigDir, t.TempDir())
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	t.Setenv(config.EnvURL, srv.URL)
	t.Setenv(config.EnvCACert, string(pemFromServer(t, srv)))
	t.Setenv(config.EnvToken, secretBearer)
	t.Setenv("EDITOR", "rancher-test-no-such-editor")
	t.Setenv("VISUAL", "rancher-test-no-such-editor")
	t.Chdir(t.TempDir())

	stdout, stderr, code := runWithStdin(t, []string{"create", "cloud-credential", "harvester", "h", "--set", "clusterId=c-x", "--from-file", "kubeconfigContent=-", "--apply", "-o", "json"}, ccKube)
	require.Zero(t, code, stderr)
	var envelope struct {
		Kind string `json:"kind"`
		Data any    `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &envelope), stdout)
	assert.Equal(t, "CloudCredential", envelope.Kind)
	assert.True(t, slices.ContainsFunc(ccStringLeaves(envelope.Data), func(leaf string) bool { return strings.HasPrefix(leaf, "cc-") }),
		"the data of the envelope names the new credential: %s", stdout)

	records := rancher.NormanCloudCredentialRequests()
	require.Len(t, records, 1)
	assert.Equal(t, http.MethodPost, records[0].Method)
	assert.Equal(t, secretBearer, records[0].Bearer)
	body := records[0].Body
	assert.Equal(t, "cloudCredential", body["type"])
	assert.Equal(t, "h", body["name"])
	assert.Equal(t, map[string]any{"provisioning.cattle.io/driver": "harvester"}, body["annotations"])
	assert.Equal(t, map[string]any{"clusterId": "c-x", "kubeconfigContent": strings.TrimSpace(ccKube)}, body["harvestercredentialConfig"])
}
