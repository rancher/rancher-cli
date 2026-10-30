package cli

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/telemetry"
	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/rancher/rancher-cli/internal/testtransport"
)

const signInLoginToken = "login-token-1"

// signInFixture is a recorded run on the file credential store against a
// facts server, for the function runArgv registers for a sign-in to call.
type signInFixture struct {
	dir       string
	srv       *factsServer
	canonical string
}

// newSignInFixture sets the environment of a recorded run with the server
// as the current server, and the hook state recording on with the directory
// present.
func newSignInFixture(t *testing.T) *signInFixture {
	t.Helper()
	resetFactsRecords(t)
	f := &signInFixture{dir: recordingEnv(t), srv: newFactsServer(t)}
	f.canonical = f.srv.canonical(t)
	f.setConfig(t, f.srv.URL)
	noteRecording(f.dir, telemetry.ConfigUnset)
	return f
}

// setConfig makes the stored config name server as the current server, or
// no server when it is empty.
func (f *signInFixture) setConfig(t *testing.T, server string) {
	t.Helper()
	cfg := &config.Config{Servers: map[string]config.Server{}}
	if server != "" {
		cfg.CurrentServer = "prod"
		cfg.Servers["prod"] = config.Server{URL: server}
	}
	setStoredConfigForTest(t, cfg, f.dir)
}

// line parses argv as the root parses a typed command line.
func (f *signInFixture) line(t *testing.T, argv ...string) keptLine {
	t.Helper()
	return keptLine{line: parseLine(newRoot(t), argv), parsed: true}
}

// getToken is the plugin's line with the server and a cluster.
func (f *signInFixture) getToken(t *testing.T) keptLine {
	t.Helper()
	return f.line(t, "auth", "get-token", "--server", f.srv.URL, "--cluster", "c-1")
}

// client is a client that reaches the server with its CA, and records the
// host of each request it sends.
func (f *signInFixture) client(t *testing.T) *http.Client {
	t.Helper()
	c, err := httpx.New(httpx.Options{CACert: f.srv.ca})
	require.NoError(t, err)
	return c
}

// answer makes one request so the server's host counts as one that
// answered, as the sign-in's own requests make it.
func (f *signInFixture) answer(t *testing.T) {
	t.Helper()
	resp, err := f.client(t).Get(f.srv.URL + "/seed")
	require.NoError(t, err)
	_ = resp.Body.Close()
	f.srv.clearHits()
}

// fetch is the install id request the sign-in hands the function.
func (f *signInFixture) fetch(t *testing.T) httpx.InstallIDFunc {
	t.Helper()
	return provider.InstallIDFetch(f.client(t), f.srv.URL, signInLoginToken)
}

// send calls the function of a sign-in with the server as the origin.
func (f *signInFixture) send(t *testing.T, k keptLine) {
	t.Helper()
	requestFactsDuringSignIn(t.Context(), k, f.srv.URL, f.fetch(t))
}

func (f *signInFixture) assertNothingSentOrHeld(t *testing.T) {
	t.Helper()
	assert.Empty(t, f.srv.paths(), "no request goes out")
	_, held := heldFactsFor(f.canonical)
	assert.False(t, held, "nothing is held")
}

func (f *signInFixture) seedInstallIDFailure(t *testing.T, kind telemetry.FailureKind, ago time.Duration) {
	t.Helper()
	seedFacts(t, f.dir, f.canonical, func(fc *telemetry.Facts) { fc.FailInstallID(kind, time.Now().Add(-ago)) })
}

// TestTheSignInRequestStopsAtTheFirstGateThatSaysNo asserts, one row per
// check, that the function a sign-in calls sends no request and holds
// nothing when the context is done, the line was not parsed, recording is
// off, the directory is missing and the command may not create it, the
// command is excluded, the line has no recorded server, the origin is
// another server, the facts file cannot be read, or the install id was read
// less than a day ago.
func TestTheSignInRequestStopsAtTheFirstGateThatSaysNo(t *testing.T) {
	rows := []struct {
		name  string
		setup func(t *testing.T, f *signInFixture) (ctx context.Context, k keptLine, origin string)
	}{
		{"the context is done", func(t *testing.T, f *signInFixture) (context.Context, keptLine, string) {
			t.Helper()
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			return ctx, f.getToken(t), f.srv.URL
		}},
		{"the line was not parsed", func(t *testing.T, f *signInFixture) (context.Context, keptLine, string) {
			t.Helper()
			return t.Context(), keptLine{}, f.srv.URL
		}},
		{"recording is off", func(t *testing.T, f *signInFixture) (context.Context, keptLine, string) {
			t.Helper()
			t.Setenv(telemetry.EnvTelemetry, "false")
			noteRecording(f.dir, telemetry.ConfigUnset)
			require.False(t, recordingStateNow().on)
			require.False(t, recordingStateNow().dirExisted, "recording off never checks the directory")
			return t.Context(), f.getToken(t), f.srv.URL
		}},
		{"the directory is missing and the command may not create it", func(t *testing.T, f *signInFixture) (context.Context, keptLine, string) {
			t.Helper()
			noteRecording(filepath.Join(f.dir, "absent"), telemetry.ConfigUnset)
			require.False(t, recordingStateNow().dirExisted)
			return t.Context(), f.line(t, "access", "clusters"), f.srv.URL
		}},
		{"the command is excluded", func(t *testing.T, f *signInFixture) (context.Context, keptLine, string) {
			t.Helper()
			return t.Context(), f.line(t, "telemetry", "status"), f.srv.URL
		}},
		{"the line has no recorded server", func(t *testing.T, f *signInFixture) (context.Context, keptLine, string) {
			t.Helper()
			f.setConfig(t, "")
			return t.Context(), f.line(t, "auth", "get-token", "--cluster", "c-1"), f.srv.URL
		}},
		{"the origin is another server", func(t *testing.T, f *signInFixture) (context.Context, keptLine, string) {
			t.Helper()
			return t.Context(), f.getToken(t), newFactsServer(t).URL
		}},
		{"the facts file cannot be read", func(t *testing.T, f *signInFixture) (context.Context, keptLine, string) {
			t.Helper()
			testfs.DirectoryInPlaceOfFile(t, filepath.Join(f.dir, "telemetry", "servers", telemetry.FactsFileName(f.canonical)))
			return t.Context(), f.getToken(t), f.srv.URL
		}},
		{"the install id was read an hour ago", func(t *testing.T, f *signInFixture) (context.Context, keptLine, string) {
			t.Helper()
			seedFacts(t, f.dir, f.canonical, func(fc *telemetry.Facts) { fc.SetInstallID(goodUUID, time.Now().Add(-time.Hour)) })
			return t.Context(), f.getToken(t), f.srv.URL
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := newSignInFixture(t)
			f.answer(t)
			ctx, k, origin := row.setup(t, f)
			requestFactsDuringSignIn(ctx, k, origin, f.fetch(t))
			f.assertNothingSentOrHeld(t)
		})
	}
}

// TestTheSignInRequestAsksForTheInstallIDWheneverItIsDue asserts the
// function sends the install id request after a stored failure of any kind,
// and after a value read more than a day ago, with the version request at
// the same time when the version is due and a host answered, holds both
// answers, and writes no file.
func TestTheSignInRequestAsksForTheInstallIDWheneverItIsDue(t *testing.T) {
	rows := []struct {
		name string
		seed func(t *testing.T, f *signInFixture)
	}{
		{"nothing is stored", func(*testing.T, *signInFixture) {}},
		{"a forbidden answer an hour ago", func(t *testing.T, f *signInFixture) {
			t.Helper()
			f.seedInstallIDFailure(t, telemetry.FailureForbidden, time.Hour)
		}},
		{"a network failure ten minutes ago", func(t *testing.T, f *signInFixture) {
			t.Helper()
			f.seedInstallIDFailure(t, telemetry.FailureNetwork, 10*time.Minute)
		}},
		{"a value read 25 hours ago", func(t *testing.T, f *signInFixture) {
			t.Helper()
			seedFacts(t, f.dir, f.canonical, func(fc *telemetry.Facts) { fc.SetInstallID(goodUUID, time.Now().Add(-25*time.Hour)) })
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := newSignInFixture(t)
			row.seed(t, f)
			f.answer(t)
			before := time.Now()
			f.send(t, f.getToken(t))

			assert.ElementsMatch(t, []string{versionPath, installIDPath}, f.srv.paths())
			hit := f.srv.hitsOn(installIDPath)[0]
			assert.Equal(t, "Bearer "+signInLoginToken, hit.header.Get("Authorization"))
			assert.Empty(t, f.srv.hitsOn(versionPath)[0].header.Values("Authorization"), "the version request carries no credential")
			held, ok := heldFactsFor(f.canonical)
			require.True(t, ok)
			assert.False(t, held.start.Before(before))
			require.NotNil(t, held.installID)
			assert.Equal(t, goodUUID, held.installID.Value)
			require.NotNil(t, held.version)
			assert.Equal(t, "v2.15.2", held.version.Value)
		})
	}
}

// TestTheSignInRequestAsksForTheVersionOnlyWhenItIsDueAndAHostAnswered
// asserts the version request is left out when the version was read an hour
// ago and when no host answered, and the install id request goes out
// either way.
func TestTheSignInRequestAsksForTheVersionOnlyWhenItIsDueAndAHostAnswered(t *testing.T) {
	t.Run("the version was read an hour ago", func(t *testing.T) {
		f := newSignInFixture(t)
		seedFacts(t, f.dir, f.canonical, func(fc *telemetry.Facts) {
			fc.SetVersion("v2.15.2", telemetry.EditionCommunity, time.Now().Add(-time.Hour))
		})
		f.answer(t)
		f.send(t, f.getToken(t))
		assert.Equal(t, []string{installIDPath}, f.srv.paths())
		held, ok := heldFactsFor(f.canonical)
		require.True(t, ok)
		assert.NotNil(t, held.installID)
		assert.Nil(t, held.version, "the sign-in did not ask")
	})
	t.Run("no host answered", func(t *testing.T) {
		f := newSignInFixture(t)
		f.send(t, f.getToken(t))
		assert.Equal(t, []string{installIDPath}, f.srv.paths())
		held, ok := heldFactsFor(f.canonical)
		require.True(t, ok)
		assert.NotNil(t, held.installID)
		assert.Nil(t, held.version)
	})
}

// TestTheSignInRequestOnAMissingDirectoryReadsNoFile asserts a first run of
// a command that may create the config directory asks for both facts with
// no file operation, and holds the answers.
func TestTheSignInRequestOnAMissingDirectoryReadsNoFile(t *testing.T) {
	f := newSignInFixture(t)
	f.answer(t)
	noteRecording(filepath.Join(f.dir, "absent"), telemetry.ConfigUnset)
	require.False(t, recordingStateNow().dirExisted)
	ops := observeFiles(t)
	f.send(t, f.getToken(t))
	assert.Empty(t, *ops, "the facts file is not read: nothing is stored")
	assert.ElementsMatch(t, []string{versionPath, installIDPath}, f.srv.paths())
	held, ok := heldFactsFor(f.canonical)
	require.True(t, ok)
	assert.NotNil(t, held.installID)
	assert.NotNil(t, held.version)
	assert.NoDirExists(t, filepath.Join(f.dir, "absent"))
}

// TestTheSignInRequestWritesNothingBeforeTheRefresh asserts the function
// leaves no facts file: the refresh after the event is the only writer.
func TestTheSignInRequestWritesNothingBeforeTheRefresh(t *testing.T) {
	f := newSignInFixture(t)
	f.answer(t)
	f.send(t, f.getToken(t))
	assert.Empty(t, serverFiles(t, f.dir))
	assert.NoDirExists(t, filepath.Join(f.dir, "telemetry"))
}

// TestTheSignInRequestSharesOneDeadlineCountedFromItsCall asserts a budget
// that is gone before any request leaves holds the start time and no answer,
// and one that the facts read uses up holds the same: the sign-in sends
// nothing, and the refresh after it sends no version request and writes no
// facts file. An install id server that never answers is cut at the budget,
// counted from the call even when the facts read took most of it: the
// function returns by the end of the budget and holds a network failure.
func TestTheSignInRequestSharesOneDeadlineCountedFromItsCall(t *testing.T) {
	t.Run("a budget of zero", func(t *testing.T) {
		f := newSignInFixture(t)
		f.answer(t)
		shrinkBudget(t, 0)
		f.send(t, f.getToken(t))
		assert.Empty(t, f.srv.paths(), "no request reaches the server")
		held, ok := heldFactsFor(f.canonical)
		require.True(t, ok, "the start time is held")
		assert.Nil(t, held.installID)
		assert.Nil(t, held.version)
	})
	t.Run("a facts read that uses the budget", func(t *testing.T) {
		f := newSignInFixture(t)
		require.NoError(t, os.MkdirAll(filepath.Join(f.dir, "telemetry"), 0o700))
		f.answer(t)
		shrinkBudget(t, 200*time.Millisecond)
		restore := telemetry.SetObserverForTest(func(op string) {
			if strings.HasPrefix(op, "readfile") {
				time.Sleep(300 * time.Millisecond)
			}
		})
		f.send(t, f.getToken(t))
		restore()
		assert.Empty(t, f.srv.paths(), "no request is sent after the budget")
		held, ok := heldFactsFor(f.canonical)
		require.True(t, ok)
		assert.Nil(t, held.installID)
		assert.Nil(t, held.version)

		refreshServerFacts(t.Context(), refreshInput{
			server: f.canonical,
			opts:   telemetry.Options{ConfigDir: f.dir, DirExisted: true, Getenv: os.Getenv},
			start:  telemetry.FactsRead{Usable: true},
		})
		assert.Empty(t, f.srv.paths(), "the refresh counts from the held start and sends nothing")
		assert.Empty(t, serverFiles(t, f.dir))
	})
	t.Run("an install id request that stalls", func(t *testing.T) {
		f := stalledInstallIDFixture(t)
		shrinkBudget(t, 300*time.Millisecond)
		f.sendWithin(t, time.Second)
		f.assertStalledRequestCut(t)
	})
	t.Run("an install id request that stalls after a slow facts read", func(t *testing.T) {
		f := stalledInstallIDFixture(t)
		shrinkBudget(t, time.Second)
		restore := telemetry.SetObserverForTest(func(op string) {
			if strings.HasPrefix(op, "readfile") {
				time.Sleep(700 * time.Millisecond)
			}
		})
		defer restore()
		f.sendWithin(t, 1400*time.Millisecond)
		f.assertStalledRequestCut(t)
	})
}

// stalledInstallIDFixture is a sign-in fixture whose install id is due and
// whose version is not, with a host that answered and an install id route
// that never answers.
func stalledInstallIDFixture(t *testing.T) *signInFixture {
	t.Helper()
	f := newSignInFixture(t)
	seedFacts(t, f.dir, f.canonical, func(fc *telemetry.Facts) {
		fc.SetVersion("v2.15.2", telemetry.EditionCommunity, time.Now().Add(-time.Hour))
	})
	f.answer(t)
	f.srv.handle(installIDPath, func(_ http.ResponseWriter, r *http.Request) { testtransport.HangAndAbort(r) })
	return f
}

// sendWithin calls the function of a sign-in and fails the test when it has
// not returned after limit, counted from the call. A function that runs on
// past the limit is cancelled and waited for first.
func (f *signInFixture) sendWithin(t *testing.T, limit time.Duration) {
	t.Helper()
	k, fetch := f.getToken(t), f.fetch(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		requestFactsDuringSignIn(ctx, k, f.srv.URL, fetch)
	}()
	select {
	case <-done:
	case <-time.After(limit):
		cancel()
		<-done
		require.FailNowf(t, "the function ran past its budget", "it had not returned after %v", limit)
	}
	assert.Less(t, time.Since(start), limit)
}

// assertStalledRequestCut asserts the install id request went out and the
// budget cut it: a network failure is held, not a cancelled run, and no
// version request was sent.
func (f *signInFixture) assertStalledRequestCut(t *testing.T) {
	t.Helper()
	assert.Len(t, f.srv.hitsOn(installIDPath), 1, "the request went out")
	held, ok := heldFactsFor(f.canonical)
	require.True(t, ok)
	require.NotNil(t, held.installID)
	assert.Equal(t, telemetry.Answer{}, *held.installID, "a request the budget cut is a network failure, not a cancelled run")
	assert.Nil(t, held.version)
}

// TestARequestThatPanicsInTheSignInRequestIsHeldAsAFailure asserts a panic in
// either request ends neither the process nor the function: the function
// returns, the panicking record holds a network failure and the other one
// holds its answer.
func TestARequestThatPanicsInTheSignInRequestIsHeldAsAFailure(t *testing.T) {
	t.Run("the install id request", func(t *testing.T) {
		f := newSignInFixture(t)
		f.answer(t)
		old := sendInstallIDRequest
		sendInstallIDRequest = func(context.Context, httpx.InstallIDFetcher) (string, int, error) { panic("boom") }
		t.Cleanup(func() { sendInstallIDRequest = old })
		f.send(t, f.getToken(t))
		held, ok := heldFactsFor(f.canonical)
		require.True(t, ok)
		require.NotNil(t, held.installID)
		assert.Equal(t, telemetry.Answer{}, *held.installID)
		require.NotNil(t, held.version)
		assert.Equal(t, "v2.15.2", held.version.Value)
	})
	t.Run("the version request", func(t *testing.T) {
		f := newSignInFixture(t)
		f.answer(t)
		old := sendVersionRequest
		sendVersionRequest = func(context.Context, http.RoundTripper, string) (httpx.VersionResponse, error) { panic("boom") }
		t.Cleanup(func() { sendVersionRequest = old })
		f.send(t, f.getToken(t))
		held, ok := heldFactsFor(f.canonical)
		require.True(t, ok)
		require.NotNil(t, held.version)
		assert.Equal(t, telemetry.Answer{}, *held.version)
		require.NotNil(t, held.installID)
		assert.Equal(t, goodUUID, held.installID.Value)
	})
}

// refreshFor is the input a recorded run of the fixture's server hands the
// refresh when nothing is stored.
func (f *signInFixture) refreshFor() refreshInput {
	return refreshInput{
		server: f.canonical,
		opts:   telemetry.Options{ConfigDir: f.dir, DirExisted: true, Getenv: os.Getenv},
		start:  telemetry.FactsRead{Usable: true},
	}
}

func goodAnswers() (installID, version telemetry.Answer) {
	return telemetry.Answer{Status: 200, Value: goodUUID}, telemetry.Answer{Status: 200, Value: "v2.15.2", Edition: telemetry.EditionCommunity}
}

// TestTheRefreshWritesTheAnswersTheSignInHeldAndAsksAgainForNothing asserts
// the answers a sign-in held for the refresh's server are written with one
// write and no further request, and answers held for another server are
// not used.
func TestTheRefreshWritesTheAnswersTheSignInHeldAndAsksAgainForNothing(t *testing.T) {
	t.Run("the same server", func(t *testing.T) {
		f := newSignInFixture(t)
		f.answer(t)
		id, ver := goodAnswers()
		holdFacts(heldFacts{server: f.canonical, start: time.Now(), installID: &id, version: &ver})
		refreshServerFacts(t.Context(), f.refreshFor())
		assert.Empty(t, f.srv.paths(), "no request goes out")
		facts, ok := readFactsOf(t, f.dir, f.canonical)
		require.True(t, ok)
		assert.Equal(t, goodUUID, facts.InstallID.Value)
		assert.Equal(t, "2.15.2", facts.Version.Value)
	})
	t.Run("another server", func(t *testing.T) {
		f := newSignInFixture(t)
		f.answer(t)
		id, ver := goodAnswers()
		holdFacts(heldFacts{server: "https://other.example.com", start: time.Now(), installID: &id, version: &ver})
		refreshServerFacts(t.Context(), f.refreshFor())
		assert.Equal(t, []string{versionPath}, f.srv.paths(), "the refresh does what it does with nothing held")
		facts, ok := readFactsOf(t, f.dir, f.canonical)
		require.True(t, ok)
		assert.Empty(t, facts.InstallID.Value, "the held answer of another server is not written")
	})
	t.Run("a cancelled answer", func(t *testing.T) {
		f := newSignInFixture(t)
		f.answer(t)
		id := telemetry.Answer{Cancelled: true}
		_, ver := goodAnswers()
		holdFacts(heldFacts{server: f.canonical, start: time.Now(), installID: &id, version: &ver})
		refreshServerFacts(t.Context(), f.refreshFor())
		facts, ok := readFactsOf(t, f.dir, f.canonical)
		require.True(t, ok)
		assert.Equal(t, "2.15.2", facts.Version.Value)
		assert.Empty(t, facts.InstallID.Value)
		assert.Nil(t, facts.InstallID.Failure, "a cancelled request stores nothing")
	})
	t.Run("a held failure over a good value keeps the value", func(t *testing.T) {
		f := newSignInFixture(t)
		f.answer(t)
		seedFacts(t, f.dir, f.canonical, func(fc *telemetry.Facts) { fc.SetInstallID(goodUUID, time.Now().Add(-25*time.Hour)) })
		_, ver := goodAnswers()
		failed := telemetry.Answer{Status: 403}
		holdFacts(heldFacts{server: f.canonical, start: time.Now(), installID: &failed, version: &ver})
		in := f.refreshFor()
		in.start = telemetry.ReadServerFacts(f.canonical, in.opts)
		refreshServerFacts(t.Context(), in)
		facts, ok := readFactsOf(t, f.dir, f.canonical)
		require.True(t, ok)
		assert.Equal(t, goodUUID, facts.InstallID.Value)
		require.NotNil(t, facts.InstallID.Failure)
		assert.Equal(t, telemetry.FailureForbidden, facts.InstallID.Failure.Kind)
	})
}

// TestTheRefreshCountsItsBudgetFromTheSignInRequest asserts a record the
// sign-in did not ask for is not sent when the sign-in's start is older than
// the budget, and stores nothing, while the held answers are written.
func TestTheRefreshCountsItsBudgetFromTheSignInRequest(t *testing.T) {
	f := newSignInFixture(t)
	f.answer(t)
	shrinkBudget(t, 100*time.Millisecond)
	id, _ := goodAnswers()
	holdFacts(heldFacts{server: f.canonical, start: time.Now().Add(-time.Second), installID: &id})
	refreshServerFacts(t.Context(), f.refreshFor())
	assert.Empty(t, f.srv.paths(), "the version request would start after the deadline")
	facts, ok := readFactsOf(t, f.dir, f.canonical)
	require.True(t, ok)
	assert.Equal(t, goodUUID, facts.InstallID.Value)
	assert.Empty(t, facts.Version.Value)
	assert.Nil(t, facts.Version.Failure, "a request not sent stores nothing")
}

// TestTheRefreshWritesHeldAnswersOnlyUnderItsGates asserts held answers are
// not written when the event's read of the facts file was not usable, when
// no host answered, when the run's context is done, and when the run has no
// server.
func TestTheRefreshWritesHeldAnswersOnlyUnderItsGates(t *testing.T) {
	id, ver := goodAnswers()
	rows := []struct {
		name  string
		setup func(t *testing.T, f *signInFixture) (context.Context, refreshInput)
	}{
		{"the read was not usable", func(t *testing.T, f *signInFixture) (context.Context, refreshInput) {
			t.Helper()
			f.answer(t)
			in := f.refreshFor()
			in.start = telemetry.FactsRead{}
			return t.Context(), in
		}},
		{"no host answered", func(t *testing.T, f *signInFixture) (context.Context, refreshInput) {
			t.Helper()
			return t.Context(), f.refreshFor()
		}},
		{"the context is done", func(t *testing.T, f *signInFixture) (context.Context, refreshInput) {
			t.Helper()
			f.answer(t)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			return ctx, f.refreshFor()
		}},
		{"the run has no server", func(t *testing.T, f *signInFixture) (context.Context, refreshInput) {
			t.Helper()
			f.answer(t)
			in := f.refreshFor()
			in.server = ""
			return t.Context(), in
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := newSignInFixture(t)
			ctx, in := row.setup(t, f)
			holdFacts(heldFacts{server: f.canonical, start: time.Now(), installID: &id, version: &ver})
			refreshServerFacts(ctx, in)
			assert.Empty(t, serverFiles(t, f.dir), "nothing is written")
			assert.Empty(t, f.srv.paths())
		})
	}
}

// TestARecordedRunThatFailedWritesTheAnswersTheSignInHeld asserts a run with
// the config directory present and a non-zero exit code hands the refresh its
// input as a successful one: the held answers are written.
func TestARecordedRunThatFailedWritesTheAnswersTheSignInHeld(t *testing.T) {
	f := newSignInFixture(t)
	f.answer(t)
	id, ver := goodAnswers()
	holdFacts(heldFacts{server: f.canonical, start: time.Now(), installID: &id, version: &ver})
	in := recordRun(newRoot(t), f.getToken(t), runResult{code: 1}, time.Now())
	require.Equal(t, f.canonical, in.server)
	refreshServerFacts(t.Context(), in)
	assert.Empty(t, f.srv.paths())
	facts, ok := readFactsOf(t, f.dir, f.canonical)
	require.True(t, ok)
	assert.Equal(t, goodUUID, facts.InstallID.Value)
	assert.Equal(t, "2.15.2", facts.Version.Value)
}

// newMissingDirFixture is a signInFixture whose config directory did not
// exist when the hook ran; a test creates it, as the plugin's cache write
// does.
func newMissingDirFixture(t *testing.T) *signInFixture {
	t.Helper()
	resetFactsRecords(t)
	f := &signInFixture{dir: missingDir(t), srv: newFactsServer(t)}
	f.canonical = f.srv.canonical(t)
	f.setConfig(t, f.srv.URL)
	noteRecording(f.dir, telemetry.ConfigUnset)
	require.False(t, recordingStateNow().dirExisted)
	return f
}

// TestAFirstPluginRunThatCreatedTheDirectoryWritesOnlyTheFactsFile asserts a
// run of auth get-token that signed in on a missing config directory and
// created it writes the held answers into the server's facts file and nothing
// else under telemetry, and a failed one writes nothing and creates no
// telemetry directory.
func TestAFirstPluginRunThatCreatedTheDirectoryWritesOnlyTheFactsFile(t *testing.T) {
	t.Run("a successful run", func(t *testing.T) {
		f := newMissingDirFixture(t)
		f.answer(t)
		f.send(t, f.getToken(t))
		require.NoError(t, os.MkdirAll(f.dir, 0o700))
		in := recordRun(newRoot(t), f.getToken(t), runResult{}, time.Now())
		require.Equal(t, f.canonical, in.server)
		refreshServerFacts(t.Context(), in)
		var tree []string
		for _, p := range treeOf(t, f.dir) {
			if p == "telemetry" || strings.HasPrefix(p, "telemetry/") {
				tree = append(tree, p)
			}
		}
		file := telemetry.FactsFileName(f.canonical)
		assert.Equal(t, []string{"telemetry", "telemetry/servers", "telemetry/servers/" + file}, tree)
		facts, ok := readFactsOf(t, f.dir, f.canonical)
		require.True(t, ok)
		assert.Equal(t, goodUUID, facts.InstallID.Value)
		assert.Equal(t, "2.15.2", facts.Version.Value)
		assert.Empty(t, storedRuns(t, f.dir))
	})
	t.Run("a failed run", func(t *testing.T) {
		f := newMissingDirFixture(t)
		f.answer(t)
		f.send(t, f.getToken(t))
		require.NoError(t, os.MkdirAll(f.dir, 0o700))
		in := recordRun(newRoot(t), f.getToken(t), runResult{code: 1}, time.Now())
		assert.Equal(t, refreshInput{}, in)
		refreshServerFacts(t.Context(), in)
		assert.NoDirExists(t, filepath.Join(f.dir, "telemetry"))
	})
}

// TestTheLegacyCommandsFirstRunAsksAfterTheOutputWithTheLoginTokenItRegistered
// asserts a legacy token run that created the config directory, with a
// fetcher registered with the skip as its tail registers it, sends the
// install id request after the run and writes only the facts file.
func TestTheLegacyCommandsFirstRunAsksAfterTheOutputWithTheLoginTokenItRegistered(t *testing.T) {
	f := newMissingDirFixture(t)
	f.answer(t)
	skip := true
	httpx.RegisterInstallIDFetcher(f.srv.URL, f.fetch(t), skip)
	require.NoError(t, os.MkdirAll(f.dir, 0o700))
	in := recordRun(newRoot(t), f.line(t, "token", "--server", f.srv.URL), runResult{}, time.Now())
	require.Equal(t, f.canonical, in.server)
	refreshServerFacts(t.Context(), in)
	assert.ElementsMatch(t, []string{versionPath, installIDPath}, f.srv.paths())
	assert.Equal(t, "Bearer "+signInLoginToken, f.srv.hitsOn(installIDPath)[0].header.Get("Authorization"))
	var telemetryNames []string
	for _, p := range treeOf(t, f.dir) {
		if strings.HasPrefix(p, "telemetry/") {
			telemetryNames = append(telemetryNames, p)
		}
	}
	assert.Equal(t, []string{"telemetry/servers", "telemetry/servers/" + telemetry.FactsFileName(f.canonical)}, telemetryNames)
	assert.Empty(t, storedRuns(t, f.dir))
}

// maskedOperations replaces the config directory, the hour and slot of an
// event file and the temporary facts file in each operation.
func maskedOperations(dir string, ops []string) []string {
	out := make([]string, len(ops))
	for i, op := range ops {
		op = strings.ReplaceAll(op, dir, "DIR")
		op = slotPath.ReplaceAllString(op, "events/HOUR/SLOT.json")
		out[i] = tempFactsName.ReplaceAllString(op, "servers/TEMP")
	}
	return out
}

// TestTheFileOperationsOfAPluginSignInAreTheListed asserts the file
// operations of a miss that signed in, with the config directory, its id and
// the hourly marker present and an install id due: the hook's stat, the
// sign-in's read of the facts file, the event's operations as a recorded
// run makes them, and the refresh's one write.
func TestTheFileOperationsOfAPluginSignInAreTheListed(t *testing.T) {
	f := newSignInFixture(t)
	runRecorded(t, newRoot(t), "config", "path")
	require.Len(t, storedRuns(t, f.dir), 1)
	seedFacts(t, f.dir, f.canonical, func(fc *telemetry.Facts) {
		fc.SetVersion("v2.15.2", telemetry.EditionCommunity, time.Now().Add(-time.Hour))
		fc.FailInstallID(telemetry.FailureForbidden, time.Now().Add(-time.Hour))
	})
	f.setConfig(t, f.srv.URL)
	f.answer(t)
	ops := observeFiles(t)
	noteRecording(f.dir, telemetry.ConfigUnset)
	k := f.getToken(t)
	f.send(t, k)
	in := recordRun(newRoot(t), k, runResult{}, time.Now())
	refreshServerFacts(t.Context(), in)

	facts := "readfile servers/" + telemetry.FactsFileName(f.canonical)
	assert.Equal(t, []string{
		"stat DIR",
		"openroot .",
		"lstat telemetry",
		"openroot telemetry",
		facts,
		"openroot .",
		"lstat telemetry",
		"openroot telemetry",
		"readfile config-directory-id",
		"stat pruned",
		facts,
		"openfile events/HOUR/SLOT.json",
		"write events/HOUR/SLOT.json",
		"close events/HOUR/SLOT.json",
		"openroot .",
		"lstat telemetry",
		"openroot telemetry",
		"mkdirall servers",
		"openfile servers/TEMP",
		"write servers/TEMP",
		"close servers/TEMP",
		"rename servers/TEMP",
	}, maskedOperations(f.dir, *ops))
	assert.Equal(t, []string{installIDPath}, f.srv.paths(), "the one request went out in the sign-in")
	stored, ok := readFactsOf(t, f.dir, f.canonical)
	require.True(t, ok)
	assert.Equal(t, goodUUID, stored.InstallID.Value)
}

// TestTheFileOperationsOfAFirstPluginRunAreTheListed asserts the file
// operations of a plugin run that signed in on a missing config directory
// and created it: the hook's stat, nothing in the sign-in, the stat at the
// end of the run, the read of the facts file, and the write that creates the
// directories and the file through one temporary file.
func TestTheFileOperationsOfAFirstPluginRunAreTheListed(t *testing.T) {
	f := newMissingDirFixture(t)
	f.answer(t)
	ops := observeFiles(t)
	builds := countBuilds(t)
	noteRecording(f.dir, telemetry.ConfigUnset)
	k := f.getToken(t)
	f.send(t, k)
	require.NoError(t, os.MkdirAll(f.dir, 0o700))
	in := recordRun(newRoot(t), k, runResult{}, time.Now())
	refreshServerFacts(t.Context(), in)

	assert.Zero(t, *builds, "no event is built")
	assert.Equal(t, []string{
		"stat DIR",
		"stat DIR",
		"openroot .",
		"lstat telemetry",
		"openroot .",
		"lstat telemetry",
		"mkdir telemetry",
		"openroot telemetry",
		"mkdirall servers",
		"openfile servers/TEMP",
		"write servers/TEMP",
		"close servers/TEMP",
		"rename servers/TEMP",
	}, maskedOperations(f.dir, *ops))
}
