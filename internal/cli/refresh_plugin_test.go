package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/telemetry"
)

// execInfoFor is the KUBERNETES_EXEC_INFO kubectl sets for a cluster entry
// with a direct endpoint and the CA it trusts.
func execInfoFor(t *testing.T, server, caPEM string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"apiVersion": "client.authentication.k8s.io/v1beta1", "kind": "ExecCredential",
		"spec": map[string]any{"interactive": false, "cluster": map[string]any{
			"server":                     server,
			"certificate-authority-data": base64.StdEncoding.EncodeToString([]byte(caPEM)),
		}},
	})
	require.NoError(t, err)
	return string(b)
}

// TestTheExecPluginsCachedCredentialSendsNoFactsRequest asserts each path
// that serves a cached credential sends no facts request and writes no
// facts file: the flag fast path, the resolved path without and with a direct
// endpoint on the Rancher host, and the legacy token command's pre-probe.
func TestTheExecPluginsCachedCredentialSendsNoFactsRequest(t *testing.T) {
	rows := []struct {
		name   string
		argv   func(srv *factsServer) []string
		direct bool
	}{
		{"the flag fast path", func(s *factsServer) []string {
			return []string{"auth", "get-token", "--server", s.URL, "--name", "prod", "--cluster", "c-1"}
		}, false},
		{"the resolved path", func(*factsServer) []string {
			return []string{"auth", "get-token", "--name", "prod", "--cluster", "c-1"}
		}, false},
		{"the resolved path with a direct endpoint on the Rancher host", func(*factsServer) []string {
			return []string{"auth", "get-token", "--name", "prod", "--cluster", "c-1"}
		}, true},
		{"the legacy token command", func(s *factsServer) []string {
			return []string{"token", "--server", s.URL, "--user", "kube", "--cluster", "c-1"}
		}, false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			resetFactsRecords(t)
			dir := recordingEnv(t)
			srv := newFactsServer(t)
			seedCachedHit(t, dir, srv.URL)
			if row.direct {
				t.Setenv("KUBERNETES_EXEC_INFO", execInfoFor(t, srv.URL+"/k8s/clusters/c-other", srv.ca))
			}
			got := runRecorded(t, newRoot(t), row.argv(srv)...)
			require.Zero(t, got.res.code, got.stderr)
			assert.Contains(t, got.stdout, "ExecCredential")
			version, install := srv.factsHits()
			assert.Zero(t, version)
			assert.Zero(t, install)
			assert.Empty(t, serverFiles(t, dir))
			if row.direct {
				assert.NotEmpty(t, srv.paths(), "the direct endpoint check reached the Rancher host")
			} else {
				assert.Empty(t, srv.paths())
			}
		})
	}
}

// TestACacheMissThatGotAnAnswerAsksForTheVersionOnly asserts an exec plugin
// call that finds no credential and asks the server for its auth providers
// is followed by one version request and no install id request.
func TestACacheMissThatGotAnAnswerAsksForTheVersionOnly(t *testing.T) {
	resetFactsRecords(t)
	dir := recordingEnv(t)
	srv := newFactsServer(t)
	got := runRecorded(t, newRoot(t), "auth", "get-token", "--server", srv.URL, "--cluster", "c-1", "--cacert", writePEM(t, srv))
	require.NotZero(t, got.res.code)
	paths := srv.paths()
	require.NotEmpty(t, paths)
	assert.Equal(t, "/v1-public/authproviders", paths[0], "the sign-in asks for the auth providers first")
	assert.Equal(t, versionPath, paths[len(paths)-1])
	version, install := srv.factsHits()
	assert.Equal(t, 1, version)
	assert.Zero(t, install)
	facts, ok := readFactsOf(t, dir, srv.canonical(t))
	require.True(t, ok)
	assert.Equal(t, "2.15.2", facts.Version.Value)
}

// TestRecordsDoNotCarryOverBetweenRuns asserts that on one process a run with
// recording off that got an answer from a server and built the root client
// leaves nothing for the next run: a recorded run against the same server
// that is refused before it connects sends no facts request.
func TestRecordsDoNotCarryOverBetweenRuns(t *testing.T) {
	dir, srv := factsEnv(t)
	t.Setenv(telemetry.EnvTelemetry, "false")
	accessClusters(t)
	require.NotEmpty(t, srv.paths(), "the first run reached the server")
	srv.clearHits()

	t.Setenv(telemetry.EnvTelemetry, "")
	t.Setenv("RANCHER_CLI_TOKEN", "")
	got := runRecorded(t, newRoot(t), "get", "cluster")
	require.NotZero(t, got.res.code)
	assert.Empty(t, srv.paths(), "the second run was refused before it connected")
	assert.Len(t, storedRuns(t, dir), 1, "and it recorded an event")
}

// stubTransport is a transport that sends nothing.
type stubTransport struct{}

func (stubTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, context.Canceled }
