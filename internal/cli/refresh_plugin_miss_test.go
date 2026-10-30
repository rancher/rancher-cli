package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/telemetry"
	"github.com/rancher/rancher-cli/internal/token"
)

const (
	storedLoginToken  = "stored-login-token"
	createdClusterTok = "ext/created:secret"
)

// storedLoginEnv is a recorded run of the exec plugin against a facts server
// that holds one stored login under the entry prod, a Rancher that creates
// the cluster token from it (or, when ext is false, one without ext tokens)
// and the facts paths. expiresAt is the expiry the create answers with.
func storedLoginEnv(t *testing.T, ext bool, expiresAt string) (dir string, srv *factsServer) {
	t.Helper()
	resetFactsRecords(t)
	dir = recordingEnv(t)
	srv = newFactsServer(t)
	t.Setenv(config.EnvCACert, srv.ca)
	cfg := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{"prod": {URL: srv.URL, CacheBindingID: "b1"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "prod", URL: srv.URL}, storedLoginToken))
	srv.handle(token.ExtTokensPath, func(w http.ResponseWriter, r *http.Request) {
		if !ext {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		body := `{"metadata":{"name":"created"},"status":{"bearerToken":"` + createdClusterTok + `"`
		if expiresAt != "" {
			body += `,"expiresAt":"` + expiresAt + `"`
		}
		_, _ = w.Write([]byte(body + `}}`))
	})
	return dir, srv
}

func getTokenLine(srv *factsServer) []string {
	return []string{"auth", "get-token", "--server", srv.URL, "--name", "prod", "--cluster", "local"}
}

// credentialOf decodes the ExecCredential a run printed and returns its
// status token and whether it carries an expiry.
func credentialOf(t *testing.T, stdout string) (tok string, hasExpiry bool) {
	t.Helper()
	var cred struct {
		Status struct {
			Token               string  `json:"token"`
			ExpirationTimestamp *string `json:"expirationTimestamp"`
		} `json:"status"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &cred), "stdout is one ExecCredential: %q", stdout)
	return cred.Status.Token, cred.Status.ExpirationTimestamp != nil
}

// offAndOnRuns is the same plugin line run once with recording off and once
// with it on, each against its own config directory and facts server.
type offAndOnRuns struct {
	off, on recordedRun
	offSrv  *factsServer
	onDir   string
	onSrv   *factsServer
}

// runOffAndOn runs the plugin line of storedLoginEnv(ext, expiresAt) with
// recording off in one config directory and with it on in another. prepare,
// when set, runs on each directory and server before its run. A run that
// cached the credential in one directory would make the other a hit, which
// is why the two runs do not share one.
func runOffAndOn(t *testing.T, ext bool, expiresAt string, prepare func(dir string, srv *factsServer)) offAndOnRuns {
	t.Helper()
	processOutput := captureProcessStreams(t)
	var r offAndOnRuns
	offDir, offSrv := storedLoginEnv(t, ext, expiresAt)
	if prepare != nil {
		prepare(offDir, offSrv)
	}
	t.Setenv(telemetry.EnvTelemetry, "false")
	r.off = runRecorded(t, newRoot(t), getTokenLine(offSrv)...)
	r.offSrv = offSrv
	r.onDir, r.onSrv = storedLoginEnv(t, ext, expiresAt)
	if prepare != nil {
		prepare(r.onDir, r.onSrv)
	}
	r.on = runRecorded(t, newRoot(t), getTokenLine(r.onSrv)...)
	assert.Empty(t, processOutput(), "nothing reaches the process's own output or error")
	return r
}

// assertSameOutput asserts the two runs wrote the same error output, ended
// with the same exit code and printed the same ExecCredential, with the
// expiry value left out and its presence compared, and that the run with
// recording off sent no facts request.
func (r offAndOnRuns) assertSameOutput(t *testing.T) {
	t.Helper()
	version, install := r.offSrv.factsHits()
	assert.Zero(t, version+install, "recording off sends no facts request")
	mask := func(run recordedRun, srv *factsServer) string {
		return strings.ReplaceAll(run.stderr, srv.URL, "<url>")
	}
	assert.Equal(t, mask(r.off, r.offSrv), mask(r.on, r.onSrv), "standard error")
	assert.Equal(t, r.off.res.code, r.on.res.code, "exit code")
	if r.off.stdout == "" || r.on.stdout == "" {
		assert.Equal(t, r.off.stdout, r.on.stdout, "standard output")
		return
	}
	off, offExpiry := credentialWithoutExpiry(t, r.off.stdout)
	on, onExpiry := credentialWithoutExpiry(t, r.on.stdout)
	assert.Equal(t, offExpiry, onExpiry, "the credential's expiry is present in both or neither")
	assert.Equal(t, off, on, "the ExecCredential")
}

// credentialWithoutExpiry decodes the ExecCredential a run printed and
// returns it with status.expirationTimestamp removed, and whether that field
// was present.
func credentialWithoutExpiry(t *testing.T, stdout string) (cred map[string]any, hadExpiry bool) {
	t.Helper()
	require.NoError(t, json.Unmarshal([]byte(stdout), &cred), "stdout is one ExecCredential: %q", stdout)
	status, _ := cred["status"].(map[string]any)
	_, hadExpiry = status["expirationTimestamp"]
	delete(status, "expirationTimestamp")
	return cred, hadExpiry
}

// TestAStoredLoginMissAsksForTheInstallIDWithTheStoredLogin asserts a call
// that creates the cluster token from the stored login asks for the version
// and the install id after its own requests, and the install id request
// carries the stored login and never the token the call prints; the run's
// output and exit code are those of the same run with recording off.
func TestAStoredLoginMissAsksForTheInstallIDWithTheStoredLogin(t *testing.T) {
	dir, srv := storedLoginEnv(t, true, "")
	got := runOffThenOn(t, getTokenLine(srv)...)
	require.Zero(t, got.res.code, got.stderr)
	printed, _ := credentialOf(t, got.stdout)
	assert.Equal(t, createdClusterTok, printed)
	assert.NotEqual(t, storedLoginToken, printed, "the call prints the cluster token, not the stored login")
	paths := srv.paths()
	require.Len(t, paths, 4, paths)
	assert.Equal(t, []string{token.ExtTokensPath, token.ExtTokensPath}, paths[:2], "the call's own requests: one with recording off and one with it on")
	assert.ElementsMatch(t, []string{versionPath, installIDPath}, paths[2:], "the facts requests come last")
	hit := srv.hitsOn(installIDPath)
	require.Len(t, hit, 1, "the run with recording off sent none")
	assert.Equal(t, "Bearer "+storedLoginToken, hit[0].header.Get("Authorization"))
	facts, ok := readFactsOf(t, dir, srv.canonical(t))
	require.True(t, ok)
	assert.Equal(t, goodUUID, facts.InstallID.Value)
	assert.Equal(t, "2.15.2", facts.Version.Value)
}

// TestAStoredLoginMissOnARancherWithoutExtTokensAsksWithTheStoredLogin
// asserts the call that prints the stored login itself asks for the install
// id with that login; its output and exit code are those of the same line
// run in another config directory with recording off.
func TestAStoredLoginMissOnARancherWithoutExtTokensAsksWithTheStoredLogin(t *testing.T) {
	runs := runOffAndOn(t, false, "", nil)
	dir, srv, got := runs.onDir, runs.onSrv, runs.on
	require.Zero(t, got.res.code, got.stderr)
	runs.assertSameOutput(t)
	printed, _ := credentialOf(t, got.stdout)
	assert.Equal(t, storedLoginToken, printed)
	assert.Contains(t, got.stderr, "this Rancher does not serve ext.cattle.io")
	paths := srv.paths()
	require.Len(t, paths, 3, paths)
	assert.Equal(t, token.ExtTokensPath, paths[0])
	assert.ElementsMatch(t, []string{versionPath, installIDPath}, paths[1:])
	assert.Equal(t, "Bearer "+storedLoginToken, srv.hitsOn(installIDPath)[0].header.Get("Authorization"))
	facts, ok := readFactsOf(t, dir, srv.canonical(t))
	require.True(t, ok)
	assert.Equal(t, goodUUID, facts.InstallID.Value)
}

// TestAStoredLoginMissAfterAForbiddenAnswerAsksForTheVersionOnly asserts a
// call that creates the cluster token from the stored login does not ask for
// the install id again an hour after the server refused it; its output and
// exit code are those of the same line run in another config directory with
// recording off.
func TestAStoredLoginMissAfterAForbiddenAnswerAsksForTheVersionOnly(t *testing.T) {
	runs := runOffAndOn(t, true, "", func(dir string, srv *factsServer) {
		seedFacts(t, dir, srv.canonical(t), func(f *telemetry.Facts) { f.FailInstallID(telemetry.FailureForbidden, time.Now().Add(-time.Hour)) })
	})
	require.Zero(t, runs.on.res.code, runs.on.stderr)
	runs.assertSameOutput(t)
	version, install := runs.onSrv.factsHits()
	assert.Equal(t, 1, version)
	assert.Zero(t, install)
}

// TestAStoredLoginMissThatFailsAfterTheCreateAsksForTheVersionOnly asserts a
// call that fails on the expiry the server answered with registers no
// install id request, and asks for the version as any recorded run does; its
// output and exit code are those of the same line run in another config
// directory with recording off.
func TestAStoredLoginMissThatFailsAfterTheCreateAsksForTheVersionOnly(t *testing.T) {
	runs := runOffAndOn(t, true, "not a time", nil)
	require.NotZero(t, runs.on.res.code)
	runs.assertSameOutput(t)
	version, install := runs.onSrv.factsHits()
	assert.Equal(t, 1, version)
	assert.Zero(t, install)
}
