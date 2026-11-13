package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// conflictEnvelope is the error of a run, decoded from the JSON envelope.
type conflictEnvelope struct {
	Data struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	} `json:"data"`
}

// conflictServer responds to a read with a Pod that has a resourceVersion and
// to every write with a Status 409 from an admission webhook.
func conflictServer(t *testing.T) {
	t.Helper()
	extraArgsEnv(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"web","namespace":"default","resourceVersion":"5"}}`))
			return
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Conflict","code":409,` +
			`"message":"admission webhook \"x.example.io\" denied the request: duplicate not allowed"}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv(config.EnvURL, srv.URL)
	t.Setenv(config.EnvCACert, string(pemFromServer(t, srv)))
}

// TestCreateResourceWebhookConflictIsNotRetryable asserts a create whose
// dry run an admission webhook refuses with a 409 is CONFLICT at exit 5,
// not retryable, and shows the webhook's message: the request sent no
// resourceVersion. A 409 on it does not mean the resourceVersion is out of
// date. Not parallel: it sets the process environment and the captured command
// line.
func TestCreateResourceWebhookConflictIsNotRetryable(t *testing.T) {
	restoreRawArgv(t)
	conflictServer(t)
	manifest := filepath.Join(t.TempDir(), "cm.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm1\n  namespace: default\n"), 0o600))
	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	_, stderr, code := runBounded(t, root, []string{"create", "resource", "-f", manifest, "--cluster", "local", "-o", "json", "--dry-run"})

	assert.Equal(t, 5, code, stderr)
	var env conflictEnvelope
	require.NoError(t, json.Unmarshal([]byte(stderr), &env), stderr)
	assert.Equal(t, "CONFLICT", env.Data.Error.Code)
	assert.False(t, env.Data.Error.Retryable)
	assert.Contains(t, env.Data.Error.Message, "duplicate not allowed")
}

// TestPatchConflictIsRetryable asserts a patch whose write gets a 409 is the
// retryable CONFLICT with the message that the resource changed since it was
// last read: a patch writes the object it read with its resourceVersion. Not
// parallel: it sets the process environment and the captured command line.
func TestPatchConflictIsRetryable(t *testing.T) {
	restoreRawArgv(t)
	conflictServer(t)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	_, stderr, code := runBounded(t, root, []string{
		"patch", "pod", "web", "--type", "merge", "-p", `{"metadata":{"labels":{"a":"b"}}}`,
		"--cluster", "local", "--namespace", "default", "-o", "json", "--dry-run",
	})

	assert.Equal(t, 5, code, stderr)
	var env conflictEnvelope
	require.NoError(t, json.Unmarshal([]byte(stderr), &env), stderr)
	assert.Equal(t, "CONFLICT", env.Data.Error.Code)
	assert.True(t, env.Data.Error.Retryable)
	assert.Equal(t, "the resource changed on the server since it was last read; retry the command", env.Data.Error.Message)
}
