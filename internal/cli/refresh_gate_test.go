package cli

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

// storeConfigWithServer makes the config snapshot the root Before hook keeps
// name url as its current server until the test ends.
func storeConfigWithServer(t *testing.T, serverURL string) {
	t.Helper()
	cfg := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{"prod": {URL: serverURL}}}
	setStoredConfig(cfg, t.TempDir())
	t.Cleanup(func() { setStoredConfig(nil, "") })
}

// TestTheGateSaysYesOnlyForARecordedCommandWithAServer asserts, with recording
// on and the directory present, the gate says no for a line that was not
// parsed, a group with no command word and each command in the exclusion
// table, and yes for a chosen command, with the canonical server it targeted.
func TestTheGateSaysYesOnlyForARecordedCommandWithAServer(t *testing.T) {
	storeConfigWithServer(t, "https://Rancher.Example.com:443/prefix")
	root := newRoot(t)
	on := recordingState{on: true, dirExisted: true}

	_, _, ok := recordingTarget(keptLine{}, on)
	assert.False(t, ok, "a line that was not parsed (a completion request)")
	_, _, ok = recordingTarget(keptLine{line: parseLine(root, []string{"get", "cluster"})}, on)
	assert.False(t, ok, "a parse the run did not keep")
	_, _, ok = recordingTarget(keptLine{line: parseLine(root, []string{"get"}), parsed: true}, on)
	assert.False(t, ok, "a group with no command word")
	for name := range telemetryExcluded {
		_, _, ok = recordingTarget(keptLine{line: parseLine(root, strings.Fields(name)), parsed: true}, on)
		assert.False(t, ok, name)
	}
	for _, off := range []recordingState{{}, {on: true}, {dirExisted: true}} {
		_, _, ok = recordingTarget(keptLine{line: parseLine(root, []string{"get", "cluster"}), parsed: true}, off)
		assert.False(t, ok)
	}

	d, server, ok := recordingTarget(keptLine{line: parseLine(root, []string{"get", "cluster"}), parsed: true}, on)
	require.True(t, ok)
	assert.Equal(t, "get cluster", d.Name)
	assert.Equal(t, "https://rancher.example.com", server)
}

// TestAHostMatchesTheRecordedServerByItsCanonicalForm asserts the host that
// answered is matched to the server through the canonical form, the one
// recorded last wins among several matches, a host that does not match is no
// target, and the fetcher counts only when it targets the same server.
func TestAHostMatchesTheRecordedServerByItsCanonicalForm(t *testing.T) {
	resetFactsRecords(t)
	first, last := &stubTransport{}, &stubTransport{}
	httpx.RecordAnswered(&url.URL{Scheme: "https", Host: "H:443"}, first)
	httpx.RecordAnswered(&url.URL{Scheme: "https", Host: "other.example.com"}, &stubTransport{})
	target, ok := refreshTargetFor("https://h")
	require.True(t, ok)
	assert.Equal(t, "https://H:443", target.origin)
	assert.Same(t, first, target.transport)
	assert.False(t, target.hasFetcher)

	httpx.RecordAnswered(&url.URL{Scheme: "https", Host: "h"}, last)
	target, ok = refreshTargetFor("https://h")
	require.True(t, ok)
	assert.Equal(t, "https://h", target.origin, "of two matches, the one recorded last")
	assert.Same(t, last, target.transport)

	_, ok = refreshTargetFor("https://unseen.example.com")
	assert.False(t, ok)

	fetch := func(context.Context) (string, int, error) { return goodUUID, 200, nil }
	httpx.RegisterInstallIDFetcher("https://b.example.com", fetch)
	target, _ = refreshTargetFor("https://h")
	assert.False(t, target.hasFetcher, "a fetcher for another server serves none")
	httpx.RegisterInstallIDFetcher("https://H:443", fetch)
	target, _ = refreshTargetFor("https://h")
	assert.True(t, target.hasFetcher, "the same server in another form")
}

// TestARecordNeedsWhatItsRequestSendsThrough asserts the per-record step
// returns a request only when the record is due and the request has what it
// needs: a transport for the version, a fetcher for the install id.
func TestARecordNeedsWhatItsRequestSendsThrough(t *testing.T) {
	var fresh telemetry.Facts
	fresh.SetVersion("v2.15.2", telemetry.EditionCommunity, time.Now())
	fresh.SetInstallID(goodUUID, time.Now())
	var empty telemetry.Facts
	full := refreshTarget{transport: &stubTransport{}, fetcher: httpx.InstallIDFetcher{Origin: "https://h"}, hasFetcher: true}
	now := time.Now()
	ctx := t.Context()

	for _, rec := range []refreshRecord{versionRecord, installIDRecord} {
		assert.Nil(t, requestFor(ctx, rec, fresh, now, full), "not due")
		assert.NotNil(t, requestFor(ctx, rec, empty, now, full), "due with what it needs")
	}
	assert.Nil(t, requestFor(ctx, versionRecord, empty, now, refreshTarget{hasFetcher: true}), "no transport")
	assert.Nil(t, requestFor(ctx, installIDRecord, empty, now, refreshTarget{transport: &stubTransport{}}), "no fetcher")
}

// TestTheRefreshStopsAtEachGate asserts the refresh sends nothing and touches
// no file when it was handed no recorded run, no server, facts the event could
// not read, a context that is done, or a server no host that answered matches;
// and that with every gate open it sends the one due request.
func TestTheRefreshStopsAtEachGate(t *testing.T) {
	resetFactsRecords(t)
	restoreEnabled := telemetry.SetEnabledForTest()
	t.Cleanup(restoreEnabled)
	var sent atomic.Int32
	origVersion := sendVersionRequest
	sendVersionRequest = func(context.Context, http.RoundTripper, string) (httpx.VersionResponse, error) {
		sent.Add(1)
		return httpx.VersionResponse{Status: 200, Body: []byte(goodVersion)}, nil
	}
	t.Cleanup(func() { sendVersionRequest = origVersion })
	ops := observeFiles(t)

	dir := t.TempDir()
	const server = "https://h.example.com"
	httpx.RecordAnswered(&url.URL{Scheme: "https", Host: "h.example.com"}, &stubTransport{})
	good := refreshInput{
		server: server,
		opts:   telemetry.Options{ConfigDir: dir, DirExisted: true},
		start:  telemetry.FactsRead{Usable: true},
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	noServer, unreadable, elsewhere := good, good, good
	noServer.server = ""
	unreadable.start = telemetry.FactsRead{}
	elsewhere.server = "https://elsewhere.example.com"

	refreshServerFacts(t.Context(), refreshInput{})
	refreshServerFacts(t.Context(), noServer)
	refreshServerFacts(t.Context(), unreadable)
	refreshServerFacts(cancelled, good)
	refreshServerFacts(t.Context(), elsewhere)
	assert.Zero(t, sent.Load())
	assert.Empty(t, *ops)

	refreshServerFacts(t.Context(), good)
	assert.Equal(t, int32(1), sent.Load())
	facts, ok := readFactsOf(t, dir, server)
	require.True(t, ok)
	assert.Equal(t, "2.15.2", facts.Version.Value)
}

// TestTheRootClientRegistersTheInstallIDFetcherWhenItIsBuilt asserts building
// the root client registers a fetcher for the server's scheme and host, and
// a factory whose root client cannot be built registers nothing.
func TestTheRootClientRegistersTheInstallIDFetcherWhenItIsBuilt(t *testing.T) {
	resetFactsRecords(t)
	t.Run("a root client that cannot be built", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvConfigDir, dir)
		t.Setenv(config.EnvURL, "")
		t.Setenv(config.EnvToken, "")
		_, err := newClientFactory(dir, config.NewFileStore(dir)).Root()
		require.Error(t, err)
		_, ok := httpx.RegisteredInstallIDFetcher()
		assert.False(t, ok)
	})
	t.Run("a root client that is built", func(t *testing.T) {
		srv := newFactsServer(t)
		t.Setenv(config.EnvURL, "")
		t.Setenv(config.EnvToken, "")
		_, err := seedRootConfig(t, srv.Server).Root()
		require.NoError(t, err)
		f, ok := httpx.RegisteredInstallIDFetcher()
		require.True(t, ok)
		assert.Equal(t, srv.URL, f.Origin)
		value, status, err := f.Fetch(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 200, status)
		assert.Equal(t, goodUUID, value)
		assert.Equal(t, "Bearer token-abc:secret", srv.hitsOn(installIDPath)[0].header.Get("Authorization"))
	})
}

// TestARegisteredFetcherSendsNothingWhenNoHostAnswered asserts a fetcher
// registered for the recorded server sends no request when no host that
// answered matches it: the install id request needs the same host the
// version request does.
func TestARegisteredFetcherSendsNothingWhenNoHostAnswered(t *testing.T) {
	resetFactsRecords(t)
	restoreEnabled := telemetry.SetEnabledForTest()
	t.Cleanup(restoreEnabled)
	var sent atomic.Int32
	const server = "https://h.example.com"
	httpx.RegisterInstallIDFetcher(server, func(context.Context) (string, int, error) {
		sent.Add(1)
		return goodUUID, 200, nil
	})
	dir := t.TempDir()
	ops := observeFiles(t)
	refreshServerFacts(t.Context(), refreshInput{
		server: server,
		opts:   telemetry.Options{ConfigDir: dir, DirExisted: true},
		start:  telemetry.FactsRead{Usable: true},
	})
	assert.Zero(t, sent.Load())
	assert.Empty(t, *ops)
	assert.Empty(t, serverFiles(t, dir))
}
