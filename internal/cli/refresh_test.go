package cli

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/telemetry"
	"github.com/rancher/rancher-cli/internal/testtransport"
)

// accessClusters runs access clusters, a command that sends a kube request
// and builds the root client.
func accessClusters(t *testing.T) recordedRun {
	t.Helper()
	return runRecorded(t, newRoot(t), "access", "clusters")
}

// factsEnv is the environment of a recorded run against a new server: the
// recording environment and the server as the URL, with a token and its CA.
func factsEnv(t *testing.T) (dir string, srv *factsServer) {
	t.Helper()
	resetFactsRecords(t)
	dir = recordingEnv(t)
	srv = newFactsServer(t)
	srv.useServer(t)
	return dir, srv
}

// runOffThenOn runs argv with recording off and then with it on, in the same
// environment, and asserts the standard output, standard error and exit code
// are the same. It returns the run with recording on.
func runOffThenOn(t *testing.T, argv ...string) recordedRun {
	t.Helper()
	processOutput := captureProcessStreams(t)
	t.Setenv(telemetry.EnvTelemetry, "false")
	off := runRecorded(t, newRoot(t), argv...)
	t.Setenv(telemetry.EnvTelemetry, "")
	on := runRecorded(t, newRoot(t), argv...)
	assert.Equal(t, off.stdout, on.stdout, "standard output")
	assert.Equal(t, off.stderr, on.stderr, "standard error")
	assert.Equal(t, off.res.code, on.res.code, "exit code")
	assert.Empty(t, processOutput(), "nothing reaches the process's own output or error")
	return on
}

// captureProcessStreams swaps the process's standard output and error for a
// file until the test ends, and returns a function that reads what was
// written to either.
func captureProcessStreams(t *testing.T) func() string {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "streams")
	require.NoError(t, err)
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = out, out
	t.Cleanup(func() {
		os.Stdout, os.Stderr = oldOut, oldErr
		_ = out.Close()
	})
	return func() string {
		data, err := os.ReadFile(out.Name())
		require.NoError(t, err)
		return string(data)
	}
}

// accessEvent runs access clusters and returns the fields of the event that
// run stored.
func accessEvent(t *testing.T, dir string) map[string]any {
	t.Helper()
	seen := map[string]bool{}
	for _, ev := range storedRuns(t, dir) {
		seen[ev.UID] = true
	}
	accessClusters(t)
	for _, ev := range storedRuns(t, dir) {
		if !seen[ev.UID] {
			return ev.Fields
		}
	}
	require.FailNow(t, "the run stored no event")
	return nil
}

// TestACommandThatBuildsTheRootClientRefreshesBothRecords asserts a command
// that builds the root client and sends a kube request is followed by one
// version request and one install id request, in that order after the
// command's own, with the headers the rules give, and that the same run
// again sends none.
func TestACommandThatBuildsTheRootClientRefreshesBothRecords(t *testing.T) {
	dir, srv := factsEnv(t)
	accessClusters(t)

	paths := srv.paths()
	require.Len(t, paths, 4, paths)
	assert.ElementsMatch(t, []string{versionPath, installIDPath}, paths[2:], "the facts requests come after the command's own")
	assert.NotContains(t, paths[:2], versionPath)

	versionHit := srv.hitsOn(versionPath)[0]
	assert.Equal(t, httpx.UserAgent(), versionHit.header.Get("User-Agent"))
	assert.Empty(t, versionHit.header.Values("Authorization"))
	assert.Empty(t, versionHit.header.Values("Impersonate-User"))
	installHit := srv.hitsOn(installIDPath)[0]
	assert.Equal(t, "Bearer token-x:y", installHit.header.Get("Authorization"))
	assert.Equal(t, httpx.UserAgent(), installHit.header.Get("User-Agent"))
	assert.Equal(t, "application/json", installHit.header.Get("Accept"))
	assert.Empty(t, installHit.header.Values("Impersonate-User"))
	for _, p := range srv.paths()[2:] {
		assert.False(t, strings.HasPrefix(p, "/k8s/clusters/"), "the facts requests go to the host root: %s", p)
	}

	facts, ok := readFactsOf(t, dir, srv.canonical(t))
	require.True(t, ok)
	assert.Equal(t, "2.15.2", facts.Version.Value)
	assert.Equal(t, "community", facts.Version.Edition)
	assert.Equal(t, goodUUID, facts.InstallID.Value)

	srv.clearHits()
	accessClusters(t)
	version, install := srv.factsHits()
	assert.Zero(t, version, "a fresh record is not asked for again")
	assert.Zero(t, install)
}

// TestACommandWithNoRootClientAsksOnlyForTheVersion asserts a kube-only
// command, checked by its request list to build no root client, is followed
// by one version request and no install id request.
func TestACommandWithNoRootClientAsksOnlyForTheVersion(t *testing.T) {
	dir, srv := factsEnv(t)
	runRecorded(t, newRoot(t), "get", "cluster")
	for _, p := range srv.paths() {
		assert.False(t, strings.HasPrefix(p, "/v1/"), "get cluster builds no root client: %s", p)
	}
	version, install := srv.factsHits()
	assert.Equal(t, 1, version)
	assert.Zero(t, install)
	facts, ok := readFactsOf(t, dir, srv.canonical(t))
	require.True(t, ok)
	assert.Equal(t, "2.15.2", facts.Version.Value)
	assert.Empty(t, facts.InstallID.Value)
	assert.Nil(t, facts.InstallID.Failure)
}

// TestACommandThatUsesOnlyTheHTTPClientIsFollowedByTheVersionRequest asserts a
// command that sends its own version request through an httpx client gets
// the refresh's version request after it.
func TestACommandThatUsesOnlyTheHTTPClientIsFollowedByTheVersionRequest(t *testing.T) {
	dir := recordingEnv(t)
	resetFactsRecords(t)
	srv := newFactsServer(t)
	got := runRecorded(t, newRoot(t), "config", "add-server", "x", "--url", srv.URL, "--cacert", writePEM(t, srv))
	require.Zero(t, got.res.code, got.stderr)
	version, install := srv.factsHits()
	assert.Equal(t, 2, version, "the command's own request and the refresh's")
	assert.Zero(t, install)
	facts, ok := readFactsOf(t, dir, srv.canonical(t))
	require.True(t, ok)
	assert.Equal(t, "2.15.2", facts.Version.Value)
}

// answerCase is one answer to a facts request and what the record stores.
type answerCase struct {
	name    string
	handler func(other *factsServer) http.HandlerFunc
	// failure is the stored failure kind; "" means the answer's value is
	// stored.
	failure telemetry.FailureKind
	// crossOrigin says the answer redirects to another server, which must
	// see no request.
	crossOrigin bool
	// stalled says the server never answers.
	stalled bool
}

func redirectTo(location func(other *factsServer) string) func(*factsServer) http.HandlerFunc {
	return func(other *factsServer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, location(other), http.StatusFound) }
	}
}

func statusAnswer(code int) func(*factsServer) http.HandlerFunc {
	return func(*factsServer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }
	}
}

func bodyAnswer(body string) func(*factsServer) http.HandlerFunc {
	return func(*factsServer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }
	}
}

func stall(*factsServer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { testtransport.HangAndAbort(r) }
}

// brokenBody sends the headers of a long body and part of it, then drops the
// connection.
func brokenBody(*factsServer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short"))
		panic(http.ErrAbortHandler)
	}
}

var padding = strings.Repeat("a", httpx.FactsBodyLimit)

// TestEachAnswerOfTheVersionRequestIsStoredPerTheTable asserts the version
// answers of the outcome table, from a server, are stored as it says, the
// version request follows no redirect, and the run's output and exit code
// are the same as with recording off.
func TestEachAnswerOfTheVersionRequestIsStoredPerTheTable(t *testing.T) {
	cases := []answerCase{
		{name: "302 to the same origin", handler: redirectTo(func(*factsServer) string { return "/moved-version" }), failure: telemetry.FailureNetwork},
		{name: "302 to another origin", handler: redirectTo(func(o *factsServer) string { return o.URL + versionPath }), failure: telemetry.FailureNetwork, crossOrigin: true},
		{name: "401", handler: statusAnswer(401), failure: telemetry.FailureNetwork},
		{name: "403", handler: statusAnswer(403), failure: telemetry.FailureForbidden},
		{name: "404", handler: statusAnswer(404), failure: telemetry.FailureForbidden},
		{name: "500", handler: statusAnswer(500), failure: telemetry.FailureNetwork},
		{name: "204", handler: statusAnswer(204), failure: telemetry.FailureInvalid},
		{name: "a body that is not JSON", handler: bodyAnswer("<html>"), failure: telemetry.FailureInvalid},
		{name: "a valid answer padded past the limit", handler: bodyAnswer(`{"Version":"v2.15.2","GitCommit":"` + padding + `"}`), failure: telemetry.FailureInvalid},
		{name: "a body that stops short", handler: brokenBody, failure: telemetry.FailureNetwork},
		{name: "a stalled server", handler: stall, failure: telemetry.FailureNetwork, stalled: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, srv := factsEnv(t)
			other := newFactsServer(t)
			if c.stalled {
				shrinkBudget(t, 300*time.Millisecond)
			}
			srv.handle(versionPath, c.handler(other))
			srv.handle("/moved-version", bodyAnswer(goodVersion)(other))
			runOffThenOn(t, "access", "clusters")

			facts, ok := readFactsOf(t, dir, srv.canonical(t))
			require.True(t, ok)
			require.NotNil(t, facts.Version.Failure)
			assert.Equal(t, c.failure, facts.Version.Failure.Kind)
			assert.Empty(t, facts.Version.Value)
			assert.Equal(t, goodUUID, facts.InstallID.Value, "the other record is stored on its own")
			assert.Empty(t, srv.hitsOn("/moved-version"), "no redirect is followed")
			assert.Empty(t, other.paths(), "the other server saw no request")
		})
	}
}

// TestEachAnswerOfTheInstallIDRequestIsStoredPerTheTable asserts the install
// id answers of the outcome table, from a server, are stored as it says, the
// request follows a redirect to the same origin and refuses one to another
// origin, and the run's output and exit code are the same as with recording
// off.
func TestEachAnswerOfTheInstallIDRequestIsStoredPerTheTable(t *testing.T) {
	cases := []answerCase{
		{name: "302 to the same origin", handler: redirectTo(func(*factsServer) string { return "/moved-install" })},
		{name: "302 to another origin", handler: redirectTo(func(o *factsServer) string { return o.URL + installIDPath }), failure: telemetry.FailureNetwork, crossOrigin: true},
		{name: "401", handler: statusAnswer(401), failure: telemetry.FailureNetwork},
		{name: "403", handler: statusAnswer(403), failure: telemetry.FailureForbidden},
		{name: "404", handler: statusAnswer(404), failure: telemetry.FailureForbidden},
		{name: "500", handler: statusAnswer(500), failure: telemetry.FailureNetwork},
		{name: "204", handler: statusAnswer(204), failure: telemetry.FailureInvalid},
		{name: "a value that is not a UUID", handler: bodyAnswer(`{"value":"not-a-uuid"}`), failure: telemetry.FailureInvalid},
		{name: "a valid answer padded past the limit", handler: bodyAnswer(`{"value":"` + goodUUID + `","pad":"` + padding + `"}`), failure: telemetry.FailureInvalid},
		{name: "a body that stops short", handler: brokenBody, failure: telemetry.FailureNetwork},
		{name: "a stalled server", handler: stall, failure: telemetry.FailureNetwork, stalled: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, srv := factsEnv(t)
			other := newFactsServer(t)
			if c.stalled {
				shrinkBudget(t, 300*time.Millisecond)
			}
			srv.handle(installIDPath, c.handler(other))
			srv.handle("/moved-install", bodyAnswer(`{"value":"`+goodUUID+`"}`)(other))
			runOffThenOn(t, "access", "clusters")

			facts, ok := readFactsOf(t, dir, srv.canonical(t))
			require.True(t, ok)
			if c.failure == "" {
				assert.Equal(t, goodUUID, facts.InstallID.Value)
				assert.Nil(t, facts.InstallID.Failure)
				assert.Len(t, srv.hitsOn("/moved-install"), 1, "the same-origin redirect is followed")
			} else {
				require.NotNil(t, facts.InstallID.Failure)
				assert.Equal(t, c.failure, facts.InstallID.Failure.Kind)
				assert.Empty(t, facts.InstallID.Value)
			}
			assert.Equal(t, "2.15.2", facts.Version.Value, "the other record is stored on its own")
			assert.Empty(t, other.paths(), "the other server saw no request")
		})
	}
}

// TestARecordIsAskedForOnlyWhenItIsDue asserts, for both records, the waits
// after a success, a 403 or 404, an answer the CLI cannot use and a network
// failure, with times a little either side of each boundary.
func TestARecordIsAskedForOnlyWhenItIsDue(t *testing.T) {
	rows := []struct {
		name string
		set  func(f *telemetry.Facts, at time.Time)
		ago  time.Duration
		asks bool
	}{
		{"forbidden 5 hours ago", failBoth(telemetry.FailureForbidden), 5 * time.Hour, false},
		{"forbidden 7 hours ago", failBoth(telemetry.FailureForbidden), 7 * time.Hour, true},
		{"network 59 minutes ago", failBoth(telemetry.FailureNetwork), 59 * time.Minute, false},
		{"network 61 minutes ago", failBoth(telemetry.FailureNetwork), 61 * time.Minute, true},
		{"invalid 23 hours ago", failBoth(telemetry.FailureInvalid), 23 * time.Hour, false},
		{"invalid 25 hours ago", failBoth(telemetry.FailureInvalid), 25 * time.Hour, true},
		{"success 23 hours ago", succeedBoth, 23 * time.Hour, false},
		{"success 25 hours ago", succeedBoth, 25 * time.Hour, true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			dir, srv := factsEnv(t)
			seedFacts(t, dir, srv.canonical(t), func(f *telemetry.Facts) { row.set(f, time.Now().Add(-row.ago)) })
			accessClusters(t)
			version, install := srv.factsHits()
			want := 0
			if row.asks {
				want = 1
			}
			assert.Equal(t, want, version)
			assert.Equal(t, want, install)
		})
	}
}

func failBoth(kind telemetry.FailureKind) func(*telemetry.Facts, time.Time) {
	return func(f *telemetry.Facts, at time.Time) {
		f.FailVersion(kind, at)
		f.FailInstallID(kind, at)
	}
}

func succeedBoth(f *telemetry.Facts, at time.Time) {
	f.SetVersion("v2.14.1", telemetry.EditionCommunity, at)
	f.SetInstallID(goodUUID, at)
}

// TestBothRequestsShareOneDeadline asserts two stalled requests end together
// at the budget, not one after the other: with a budget of 500 ms the run
// returns no sooner than 500 ms and, as two deadlines in a row would take
// 1 s, no later than 800 ms after the command's own request ends, and both
// records store a network failure.
func TestBothRequestsShareOneDeadline(t *testing.T) {
	dir, srv := factsEnv(t)
	shrinkBudget(t, 500*time.Millisecond)
	srv.handle(versionPath, stall(srv))
	srv.handle(installIDPath, stall(srv))

	runOffThenOn(t, "access", "clusters")
	end := time.Now()
	srv.mu.Lock()
	own := srv.lastOwn
	srv.mu.Unlock()
	elapsed := end.Sub(own)
	assert.GreaterOrEqual(t, elapsed, 500*time.Millisecond, "the stall is real")
	assert.LessOrEqual(t, elapsed, 800*time.Millisecond, "one deadline for both requests")

	facts, ok := readFactsOf(t, dir, srv.canonical(t))
	require.True(t, ok)
	require.NotNil(t, facts.Version.Failure)
	require.NotNil(t, facts.InstallID.Failure)
	assert.Equal(t, telemetry.FailureNetwork, facts.Version.Failure.Kind)
	assert.Equal(t, telemetry.FailureNetwork, facts.InstallID.Failure.Kind)
}

// TestAServerThatDidNotAnswerGetsNoFactsRequestAndNoFile asserts a refused
// port and a TLS server whose CA was not given send no facts request and
// write no facts file, for a command whose first request is a kube request
// and for one that builds the root client (and so registers the install id
// request) before its first request.
func TestAServerThatDidNotAnswerGetsNoFactsRequestAndNoFile(t *testing.T) {
	commands := map[string][]string{
		"a kube request first":        {"access", "clusters"},
		"the root client built first": {"inspect", "cluster", "local"},
	}
	for name, argv := range commands {
		t.Run(name+" against a refused port", func(t *testing.T) {
			resetFactsRecords(t)
			dir := recordingEnv(t)
			t.Setenv(config.EnvURL, "https://127.0.0.1:1")
			t.Setenv(config.EnvToken, "token-x:y")
			t.Setenv(config.EnvCACert, otherCAPEM(t))
			got := runRecorded(t, newRoot(t), argv...)
			require.NotZero(t, got.res.code)
			assert.Empty(t, serverFiles(t, dir))
		})
		t.Run(name+" against a certificate that is not trusted", func(t *testing.T) {
			resetFactsRecords(t)
			dir := recordingEnv(t)
			srv := newFactsServer(t)
			t.Setenv(config.EnvURL, srv.URL)
			t.Setenv(config.EnvToken, "token-x:y")
			t.Setenv(config.EnvCACert, otherCAPEM(t))
			got := runRecorded(t, newRoot(t), argv...)
			require.NotZero(t, got.res.code)
			assert.Empty(t, srv.paths(), "the handshake failed before any request")
			assert.Empty(t, serverFiles(t, dir))
		})
	}
}

// TestARunWithRecordingOffOrNoDirectoryAsksForNothing asserts each control
// that turns recording off, and a missing config directory, send no facts
// request and write no facts file.
func TestARunWithRecordingOffOrNoDirectoryAsksForNothing(t *testing.T) {
	t.Run("the variable", func(t *testing.T) {
		dir, srv := factsEnv(t)
		t.Setenv(telemetry.EnvTelemetry, "false")
		accessClusters(t)
		assertNothingAsked(t, dir, srv)
	})
	t.Run("the config key", func(t *testing.T) {
		dir, srv := factsEnv(t)
		writeConfig(t, dir, "telemetry: \"false\"\n")
		accessClusters(t)
		assertNothingAsked(t, dir, srv)
	})
	t.Run("no config directory", func(t *testing.T) {
		_, srv := factsEnv(t)
		missing := filepath.Join(t.TempDir(), "absent")
		t.Setenv(config.EnvConfigDir, missing)
		accessClusters(t)
		assertNothingAsked(t, missing, srv)
		_, err := os.Stat(missing)
		assert.ErrorIs(t, err, os.ErrNotExist, "the directory is never created")
	})
}

func assertNothingAsked(t *testing.T, dir string, srv *factsServer) {
	t.Helper()
	version, install := srv.factsHits()
	assert.Zero(t, version)
	assert.Zero(t, install)
	assert.Empty(t, serverFiles(t, dir))
}

// cancelOn returns a handler that tells the test its request arrived, then
// waits until the request's context ends; the test cancels the run when it
// arrives.
func cancelOn(arrived chan<- struct{}) http.HandlerFunc {
	var once sync.Once
	return func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(arrived) })
		testtransport.HangAndAbort(r)
	}
}

// cancelWhen cancels the run's context when arrived is closed, or after a
// bound. It returns a function that gives the time of the cancel, once the
// run has ended.
func cancelWhen(arrived <-chan struct{}, cancel func()) (cancelledAt func() time.Time) {
	var at atomic.Int64
	go func() {
		select {
		case <-arrived:
		case <-time.After(30 * time.Second):
		}
		at.Store(time.Now().UnixNano())
		cancel()
	}()
	return func() time.Time { return time.Unix(0, at.Load()) }
}

// TestARunCancelledBeforeTheRefreshAsksForNothing asserts a run whose
// context ended during the command's own request sends no facts request, and
// a run cancelled while the version request is out leaves the facts file as
// it was.
func TestARunCancelledBeforeTheRefreshAsksForNothing(t *testing.T) {
	t.Run("during the command's own request", func(t *testing.T) {
		dir, srv := factsEnv(t)
		arrived := make(chan struct{})
		srv.handle("/v1/management.cattle.io.clusters", cancelOn(arrived))
		ctx, cancel := contextWithCancel(t)
		cancelWhen(arrived, cancel)
		got := runWithContext(t, ctx, newRoot(t), "access", "clusters")
		assert.Equal(t, 130, got.res.code, got.stderr)
		assertNothingAsked(t, dir, srv)
	})
	t.Run("during the version request", func(t *testing.T) {
		dir, srv := factsEnv(t)
		seedFacts(t, dir, srv.canonical(t), func(f *telemetry.Facts) { f.SetInstallID(goodUUID, time.Now()) })
		file := filepath.Join(dir, "telemetry", "servers", telemetry.FactsFileName(srv.canonical(t)))
		before, err := os.ReadFile(file)
		require.NoError(t, err)
		ops := observeFiles(t)
		arrived := make(chan struct{})
		srv.handle(versionPath, cancelOn(arrived))
		ctx, cancel := contextWithCancel(t)
		cancelledAt := cancelWhen(arrived, cancel)
		runWithContext(t, ctx, newRoot(t), "access", "clusters")
		ended := time.Now()
		assert.Len(t, srv.hitsOn(versionPath), 1)
		after, err := os.ReadFile(file)
		require.NoError(t, err)
		assert.Equal(t, string(before), string(after), "a cancelled request stores nothing")
		for _, op := range *ops {
			assert.False(t, strings.HasPrefix(op, "rename servers/"), "a cancelled refresh writes nothing: %s", op)
		}
		assert.Less(t, ended.Sub(cancelledAt()), time.Second, "the run stops at the cancel, not at the 2 second budget")
	})
	t.Run("during a body read that fails", func(t *testing.T) {
		dir, srv := factsEnv(t)
		seedFacts(t, dir, srv.canonical(t), func(f *telemetry.Facts) { f.SetInstallID(goodUUID, time.Now()) })
		file := filepath.Join(dir, "telemetry", "servers", telemetry.FactsFileName(srv.canonical(t)))
		before, err := os.ReadFile(file)
		require.NoError(t, err)
		arrived := make(chan struct{})
		var once sync.Once
		srv.handle(versionPath, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte("short"))
			w.(http.Flusher).Flush()
			once.Do(func() { close(arrived) })
			testtransport.HangAndAbort(r)
		})
		ctx, cancel := contextWithCancel(t)
		cancelWhen(arrived, cancel)
		runWithContext(t, ctx, newRoot(t), "access", "clusters")
		after, err := os.ReadFile(file)
		require.NoError(t, err)
		assert.Equal(t, string(before), string(after), "a read that failed because the run ended stores nothing")
	})
}

// TestARequestThatPanicsStoresANetworkFailure asserts a panic inside the
// version request or the install id request is recovered, stores a network
// failure for its record only, and leaves the output and exit code as they are
// with recording off.
func TestARequestThatPanicsStoresANetworkFailure(t *testing.T) {
	t.Run("the version request", func(t *testing.T) {
		dir, srv := factsEnv(t)
		orig := sendVersionRequest
		sendVersionRequest = func(context.Context, http.RoundTripper, string) (httpx.VersionResponse, error) {
			panic("a version request that panics")
		}
		t.Cleanup(func() { sendVersionRequest = orig })
		runOffThenOn(t, "access", "clusters")
		facts, ok := readFactsOf(t, dir, srv.canonical(t))
		require.True(t, ok)
		require.NotNil(t, facts.Version.Failure)
		assert.Equal(t, telemetry.FailureNetwork, facts.Version.Failure.Kind)
		assert.Equal(t, goodUUID, facts.InstallID.Value)
	})
	t.Run("the install id request", func(t *testing.T) {
		dir, srv := factsEnv(t)
		orig := sendInstallIDRequest
		sendInstallIDRequest = func(context.Context, httpx.InstallIDFetcher) (string, int, error) {
			panic("a fetcher that panics")
		}
		t.Cleanup(func() { sendInstallIDRequest = orig })
		runOffThenOn(t, "access", "clusters")
		facts, ok := readFactsOf(t, dir, srv.canonical(t))
		require.True(t, ok)
		require.NotNil(t, facts.InstallID.Failure)
		assert.Equal(t, telemetry.FailureNetwork, facts.InstallID.Failure.Kind)
		assert.Equal(t, "2.15.2", facts.Version.Value)
	})
}

// TestAFactsWriteThatFailsChangesNothingTheRunPrints asserts a facts file
// that cannot be written, and a telemetry servers path that is a file, leave
// the output and exit code as they are with recording off.
func TestAFactsWriteThatFailsChangesNothingTheRunPrints(t *testing.T) {
	t.Run("a regular file at the servers path", func(t *testing.T) {
		dir, _ := factsEnv(t)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "telemetry"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "telemetry", "servers"), nil, 0o600))
		runOffThenOn(t, "access", "clusters")
		assert.Equal(t, []string{"servers"}, namesIn(t, filepath.Join(dir, "telemetry"), "servers"))
	})
	t.Run("a write that panics", func(t *testing.T) {
		dir, srv := factsEnv(t)
		orig := saveServerFacts
		saveServerFacts = func(string, telemetry.Facts, telemetry.Options) bool { panic("a write that panics") }
		t.Cleanup(func() { saveServerFacts = orig })
		runOffThenOn(t, "access", "clusters")
		version, install := srv.factsHits()
		assert.Equal(t, 1, version)
		assert.Equal(t, 1, install)
		assert.Empty(t, serverFiles(t, dir))
	})
	t.Run("a servers directory that cannot be written", func(t *testing.T) {
		if os.Getuid() == 0 || os.PathSeparator == '\\' {
			t.Skip("directory permissions do not stop this user")
		}
		dir, srv := factsEnv(t)
		servers := filepath.Join(dir, "telemetry", "servers")
		require.NoError(t, os.MkdirAll(servers, 0o700))
		require.NoError(t, os.Chmod(servers, 0o500))
		t.Cleanup(func() { _ = os.Chmod(servers, 0o700) })
		runOffThenOn(t, "access", "clusters")
		version, install := srv.factsHits()
		assert.Equal(t, 1, version, "the requests went out; only the write failed")
		assert.Equal(t, 1, install)
		assert.Empty(t, serverFiles(t, dir))
	})
}

// namesIn returns the entries of dir/telemetry that are named name, to tell a
// file from nothing.
func namesIn(t *testing.T, telemetryDir, name string) []string {
	t.Helper()
	entries, err := os.ReadDir(telemetryDir)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		if e.Name() == name && !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

var tempFactsName = regexp.MustCompile(`servers/\.[0-9a-f]{32}\.json-[0-9a-f]{8}`)

// TestARecordedRunReadsTheFactsFileOnce asserts the file operations of a
// run that got an answer: with nothing due the refresh adds none after the
// event's, and with a record due it adds exactly the open of the directory, the
// look at the telemetry directory, its open, the servers directory, then the
// open, write and close of one temporary file and its rename; the facts file
// is read once in the whole run.
func TestARecordedRunReadsTheFactsFileOnce(t *testing.T) {
	dir, srv := factsEnv(t)
	facts := "readfile servers/" + telemetry.FactsFileName(srv.canonical(t))
	accessClusters(t)
	count := func(ops []string, op string) int {
		n := 0
		for _, o := range ops {
			if o == op {
				n++
			}
		}
		return n
	}
	masked := func(ops []string) []string {
		var out []string
		for _, op := range ops {
			op = strings.ReplaceAll(op, dir, "DIR")
			op = slotPath.ReplaceAllString(op, "events/HOUR/SLOT.json")
			out = append(out, tempFactsName.ReplaceAllString(op, "servers/TEMP"))
		}
		return out
	}

	t.Run("nothing due", func(t *testing.T) {
		ops := observeFiles(t)
		accessClusters(t)
		got := masked(*ops)
		assert.Equal(t, 1, count(got, facts))
		require.NotEmpty(t, got)
		assert.Equal(t, "close events/HOUR/SLOT.json", got[len(got)-1], "the event's own write is the last operation")
	})
	t.Run("a record due", func(t *testing.T) {
		seedFacts(t, dir, srv.canonical(t), func(f *telemetry.Facts) {
			f.SetVersion("v2.14.1", telemetry.EditionCommunity, time.Now().Add(-25*time.Hour))
			f.SetInstallID(goodUUID, time.Now())
		})
		ops := observeFiles(t)
		accessClusters(t)
		got := masked(*ops)
		assert.Equal(t, 1, count(got, facts))
		require.Greater(t, len(got), 8)
		assert.Equal(t, []string{
			"openroot .",
			"lstat telemetry",
			"openroot telemetry",
			"mkdirall servers",
			"openfile servers/TEMP",
			"write servers/TEMP",
			"close servers/TEMP",
			"rename servers/TEMP",
		}, got[len(got)-8:])
		assert.Equal(t, "close events/HOUR/SLOT.json", got[len(got)-9], "after the event's own write")
	})
}

// TestTheEventJustWrittenNeverCarriesTheRefreshedValues asserts a first run
// against a server with nothing stored records no version and an install id
// that was not fetched, writes the facts, and the next run's event carries
// the version, the edition and the install id.
func TestTheEventJustWrittenNeverCarriesTheRefreshedValues(t *testing.T) {
	dir, srv := factsEnv(t)
	first := accessEvent(t, dir)
	assert.NotContains(t, first, "io.rancher.manager.version")
	assert.NotContains(t, first, "io.rancher.manager.install.id")
	assert.Equal(t, "not_fetched", first["io.rancher.cli.install_id.missing_reason"])
	_, ok := readFactsOf(t, dir, srv.canonical(t))
	require.True(t, ok)

	second := accessEvent(t, dir)
	assert.Equal(t, "2.15.2", second["io.rancher.manager.version"])
	assert.Equal(t, "community", second["io.rancher.manager.edition"])
	assert.Equal(t, goodUUID, second["io.rancher.manager.install.id"])
	assert.NotContains(t, second, "io.rancher.cli.install_id.missing_reason")
}

// TestHostileAnswersNeverReachAnEventAsGiven asserts a version with a
// customer name, 65 characters or a build suffix is stored and then reported
// as _OTHER, an install id that is not a UUID is reported as invalid, a good
// value stays and is used after a failing server, and a value 31 days old is
// not used.
func TestHostileAnswersNeverReachAnEventAsGiven(t *testing.T) {
	for _, version := range []string{
		"v2.15.2-acme-corp",
		"v2." + strings.Repeat("1", 63),
		"v2.15.2+build.7",
	} {
		t.Run(version, func(t *testing.T) {
			dir, srv := factsEnv(t)
			srv.handle(versionPath, bodyAnswer(`{"Version":"`+version+`","RancherPrime":"false"}`)(srv))
			srv.handle(installIDPath, bodyAnswer(`{"value":"acme-corp-install"}`)(srv))
			accessClusters(t)
			facts, ok := readFactsOf(t, dir, srv.canonical(t))
			require.True(t, ok)
			assert.Equal(t, telemetry.Other, facts.Version.Value)
			require.NotNil(t, facts.InstallID.Failure)
			assert.Equal(t, telemetry.FailureInvalid, facts.InstallID.Failure.Kind)

			fields := accessEvent(t, dir)
			assert.Equal(t, telemetry.Other, fields["io.rancher.manager.version"])
			assert.Equal(t, "invalid", fields["io.rancher.cli.install_id.missing_reason"])
			assert.NotContains(t, fields, "io.rancher.manager.install.id")
		})
	}
	t.Run("a good value then a failing server", func(t *testing.T) {
		dir, srv := factsEnv(t)
		seedFacts(t, dir, srv.canonical(t), func(f *telemetry.Facts) {
			f.SetVersion("v2.14.1", telemetry.EditionPrime, time.Now().Add(-48*time.Hour))
			f.SetInstallID(goodUUID, time.Now().Add(-48*time.Hour))
		})
		srv.handle(versionPath, statusAnswer(500)(srv))
		srv.handle(installIDPath, statusAnswer(500)(srv))
		accessClusters(t)
		facts, ok := readFactsOf(t, dir, srv.canonical(t))
		require.True(t, ok)
		assert.Equal(t, "2.14.1", facts.Version.Value)
		assert.Equal(t, goodUUID, facts.InstallID.Value)
		require.NotNil(t, facts.Version.Failure)
		fields := accessEvent(t, dir)
		assert.Equal(t, "2.14.1", fields["io.rancher.manager.version"])
		assert.Equal(t, "prime", fields["io.rancher.manager.edition"])
		assert.Equal(t, goodUUID, fields["io.rancher.manager.install.id"])
	})
	t.Run("a value 31 days old", func(t *testing.T) {
		dir, srv := factsEnv(t)
		seedFacts(t, dir, srv.canonical(t), func(f *telemetry.Facts) {
			f.SetVersion("v2.14.1", telemetry.EditionPrime, time.Now().Add(-31*24*time.Hour))
			f.SetInstallID(goodUUID, time.Now().Add(-31*24*time.Hour))
		})
		srv.handle(versionPath, statusAnswer(500)(srv))
		srv.handle(installIDPath, statusAnswer(500)(srv))
		fields := accessEvent(t, dir)
		assert.NotContains(t, fields, "io.rancher.manager.version")
		assert.NotContains(t, fields, "io.rancher.manager.install.id")
		assert.Equal(t, "not_fetched", fields["io.rancher.cli.install_id.missing_reason"])
	})
}

// TestTheFactsFileIsNamedByTheServerAlone asserts a server entry with an odd
// name and an exec plugin miss against the same server write only the file
// named by the hash of the canonical URL.
func TestTheFactsFileIsNamedByTheServerAlone(t *testing.T) {
	dir, srv := factsEnv(t)
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "token-x:y")
	writeConfig(t, dir, serverConfig("my prod..x", map[string]string{"my prod..x": srv.URL}))
	accessClusters(t)
	want := []string{telemetry.FactsFileName(srv.canonical(t))}
	require.Equal(t, want, serverFiles(t, dir))

	srv.clearHits()
	seedFacts(t, dir, srv.canonical(t), func(f *telemetry.Facts) {
		f.SetVersion("v2.14.1", telemetry.EditionCommunity, time.Now().Add(-48*time.Hour))
		f.SetInstallID(goodUUID, time.Now())
	})
	got := runRecorded(t, newRoot(t), "auth", "get-token", "--server", srv.URL, "--cluster", "c-1", "--cacert", writePEM(t, srv))
	require.NotZero(t, got.res.code)
	assert.Len(t, srv.hitsOn(versionPath), 1, "the plugin's miss refreshes the version")
	assert.Equal(t, want, serverFiles(t, dir))
}
