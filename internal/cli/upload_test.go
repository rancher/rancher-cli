package cli

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/invocation"
	"github.com/rancher/rancher-cli/internal/telemetry"
	"github.com/rancher/rancher-cli/internal/telemetrysend"
	"github.com/rancher/rancher-cli/internal/testtransport"
)

// TestTelemetryOffAlwaysWinsOverUpload asserts that with a valid endpoint, a
// development build and events waiting, no request is made when a control
// turns recording off (the test binary, the variable, the config key, and a
// config that cannot be loaded), and one is made when only the default
// decides; and that when the controls turn off between the request and the
// write, nothing is deleted and no state is written.
func TestTelemetryOffAlwaysWinsOverUpload(t *testing.T) {
	type setup struct {
		name  string
		off   bool
		apply func(t *testing.T, dir string)
	}
	setups := []setup{
		{"the default", false, func(*testing.T, string) {}},
		{"the variable", true, func(t *testing.T, _ string) { t.Helper(); t.Setenv(telemetry.EnvTelemetry, "false") }},
		{"the config key", true, func(t *testing.T, dir string) { t.Helper(); writeConfig(t, dir, "telemetry: \"false\"\n") }},
		{"an unreadable config", true, func(t *testing.T, dir string) { t.Helper(); writeConfig(t, dir, "telemetry: [a]\n") }},
	}
	require.Equal(t, []telemetry.Control{telemetry.ControlTestBinary, telemetry.ControlVariable, telemetry.ControlConfig, telemetry.ControlDefault},
		telemetry.Controls(), "a control was added: add a row for it")
	for _, tc := range setups {
		t.Run(tc.name, func(t *testing.T) {
			dir := uploadEnv(t)
			paths := seedFinished(t, dir, 3, 1, 2)
			srv := newUploadServer(t, nil)
			withEndpoint(t, srv.URL)
			tc.apply(t, dir)
			run := runBuilt(t, "dev", "config", "path")
			if tc.off {
				assert.Zero(t, srv.count())
				assert.True(t, anyExists(paths))
				assert.Empty(t, uploadFilesOf(t, dir))
				return
			}
			require.Equal(t, 0, run.code, run.stderr)
			assert.Equal(t, 1, srv.count())
		})
	}

	t.Run("the test binary", func(t *testing.T) {
		experimentEnv(t)
		dir := os.Getenv(config.EnvConfigDir)
		t.Setenv(telemetry.EnvTelemetry, "true")
		seedFinished(t, dir, 3, 1, 2)
		srv := newUploadServer(t, nil)
		withEndpoint(t, srv.URL)
		runBuilt(t, "dev", "config", "path")
		assert.Zero(t, srv.count(), "recording is off in a test binary unless a test turns it on")
	})

	t.Run("turned off between the request and the write", func(t *testing.T) {
		dir := uploadEnv(t)
		paths := seedFinished(t, dir, 3, 1, 2)
		srv := newUploadServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
			require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("telemetry: \"false\"\n"), 0o600))
			_, _ = w.Write([]byte("{}"))
		})
		withEndpoint(t, srv.URL)
		run := runBuilt(t, "dev", "config", "path")
		require.Equal(t, 0, run.code, run.stderr)
		require.Equal(t, 1, srv.count())
		assert.Len(t, filesAmong(paths), len(paths), "no deletion")
		assert.NotContains(t, uploadFilesOf(t, dir), "state.json", "no state write")
		assert.NotContains(t, uploadFilesOf(t, dir), "lock")
	})
}

func filesAmong(paths []string) []string {
	var out []string
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// TestPassFileOfThisRunIsSent asserts that a recorded run whose hourly pass
// compacts a finished hour sends that hour from the file the pass just wrote
// and deletes it after the 2xx, and that a run whose pass removed its hour (one
// older than the age cap) sends nothing from it and reports no error.
func TestPassFileOfThisRunIsSent(t *testing.T) {
	old := func(t *testing.T, dir string) {
		t.Helper()
		past := time.Now().Add(-48 * time.Hour)
		require.NoError(t, os.Chtimes(filepath.Join(dir, "telemetry", "pruned"), past, past))
	}

	t.Run("the pass compacts a finished hour", func(t *testing.T) {
		dir := uploadEnv(t)
		slots := seedFinished(t, dir, 3, 1, 3)
		old(t, dir)
		srv := newUploadServer(t, nil)
		withEndpoint(t, srv.URL)
		run := runBuilt(t, "dev", "config", "path")
		require.Equal(t, 0, run.code, run.stderr)
		reqs := srv.requests()
		require.Len(t, reqs, 1)
		assert.ElementsMatch(t, []string{recordUID(1), recordUID(2), recordUID(3)}, reqs[0].ids(t))
		assert.False(t, anyExists(slots), "the slots are gone: the pass compacted them")
		hours, err := os.ReadDir(filepath.Join(dir, "telemetry", "events", hourName(hourStart(3))))
		require.NoError(t, err)
		assert.Empty(t, hours, "the compacted file is deleted after the 2xx")
	})

	t.Run("the pass removes an hour older than the age cap", func(t *testing.T) {
		dir := uploadEnv(t)
		slots := seedFinished(t, dir, 5*24, 1, 3)
		old(t, dir)
		srv := newUploadServer(t, nil)
		withEndpoint(t, srv.URL)
		run := runBuilt(t, "dev", "config", "path")
		assert.Equal(t, 0, run.code, run.stderr)
		assert.Empty(t, run.stderr)
		assert.Zero(t, srv.count())
		assert.False(t, anyExists(slots))
		_, err := os.Stat(filepath.Join(dir, "telemetry", "events", hourName(hourStart(5*24))))
		assert.ErrorIs(t, err, os.ErrNotExist, "the pass removed the hour")
	})
}

// TestUploadStaysInTheDeadline asserts that, with the refresh budget and the
// floor shrunk together, an endpoint that never answers gets exactly one
// request and the run ends after the deadline and before the deadline plus a
// margin, and that a store whose walk outlasts the budget causes no request
// and leaves no lock.
func TestUploadStaysInTheDeadline(t *testing.T) {
	t.Run("an endpoint that never answers", func(t *testing.T) {
		restoreBudget := factsBudget
		restoreFloor := uploadFloor
		t.Cleanup(func() { factsBudget, uploadFloor = restoreBudget, restoreFloor })
		factsBudget, uploadFloor = hangBound, 100*time.Millisecond
		dir := uploadEnv(t)
		seedFinished(t, dir, 3, 1, 2)
		release := make(chan struct{})
		srv := newUploadServer(t, func(_ int, _ http.ResponseWriter, r *http.Request) { testtransport.HangUntilReleased(r, release) })
		t.Cleanup(func() { close(release) })
		withEndpoint(t, srv.URL)
		start := time.Now()
		run := runBuilt(t, "dev", "config", "path")
		elapsed := time.Since(start)
		require.Equal(t, 0, run.code, run.stderr)
		assert.Equal(t, 1, srv.count())
		assert.GreaterOrEqual(t, elapsed, hangBound-100*time.Millisecond, "the request waited for the deadline")
		assert.Less(t, elapsed, hangBound+10*time.Second, "and no longer")
		assert.NotContains(t, uploadFilesOf(t, dir), "lock")
	})

	t.Run("a walk that outlasts the budget", func(t *testing.T) {
		restoreBudget := factsBudget
		restoreFloor := uploadFloor
		t.Cleanup(func() { factsBudget, uploadFloor = restoreBudget, restoreFloor })
		dir := uploadEnv(t)
		seedFinished(t, dir, 3, 1, 3)
		srv := newUploadServer(t, nil)
		withEndpoint(t, srv.URL)
		factsBudget, uploadFloor = time.Second, 100*time.Millisecond
		// The first walk of the sender's attempt takes until 50 ms are left of
		// the budget: less than the floor, and more than none.
		senderStarted, walks := false, 0
		t.Cleanup(telemetry.SetObserverForTest(func(op string) {
			switch {
			case op == "readfile upload/state.json":
				senderStarted = true
			case senderStarted && op == "readdir events":
				walks++
				if walks == 1 {
					time.Sleep(factsBudget - 50*time.Millisecond)
				}
			}
		}))
		run := runBuilt(t, "dev", "config", "path")
		require.Equal(t, 0, run.code, run.stderr)
		assert.Equal(t, 1, walks, "the first walk ran, and no second walk after it")
		assert.Zero(t, srv.count())
		_, err := os.Lstat(filepath.Join(dir, "telemetry", "upload"))
		assert.ErrorIs(t, err, os.ErrNotExist, "only the lock creates telemetry/upload: no lock was taken and no state written")
	})
}

// TestTheControlsFunctionTakesNoLock asserts that the function the run hands to
// the sender to resolve the recording controls takes no lock of any kind, since
// the sender calls it while it holds the upload lock.
func TestTheControlsFunctionTakesNoLock(t *testing.T) {
	dir := uploadEnv(t)
	seedFinished(t, dir, 3, 1, 2)
	srv := newUploadServer(t, nil)
	withEndpoint(t, srv.URL)
	locks := 0
	count := func() { locks++ }
	t.Cleanup(config.SetSemWaitHookForTest(count))
	t.Cleanup(config.SetWaitForFlockHookForTest(count))
	t.Cleanup(config.SetLockHookForTest(count))
	calls := 0
	orig := sendStoredEvents
	t.Cleanup(func() { sendStoredEvents = orig })
	sendStoredEvents = func(ctx context.Context, in telemetrysend.Input) {
		resolve := in.RecordingOn
		in.RecordingOn = func() bool {
			calls++
			before := locks
			on := resolve()
			assert.Equal(t, before, locks, "the controls function took a lock")
			return on
		}
		orig(ctx, in)
	}
	run := runBuilt(t, "dev", "config", "path")
	require.Equal(t, 0, run.code, run.stderr)
	assert.Equal(t, 1, srv.count())
	assert.Equal(t, 2, calls, "after the lock was taken and before the deletion")
}

// uploadRows are the answers a run must not show anywhere: each makes the
// sender keep or drop files, or fail to read an answer.
func uploadRows(t *testing.T) map[string]uploadAnswer {
	t.Helper()
	return map[string]uploadAnswer{
		"a 500": replyStatus(500),
		"a 400": replyStatus(400),
		"a 200": nil,
		"a closed connection": func(_ int, w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		},
		"a streaming answer over the limit": func(_ int, w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(200)
			chunk := bytes.Repeat([]byte("x"), 32<<10)
			for range 20 {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		},
		"a 200 whose body is cut short": func(_ int, w http.ResponseWriter, _ *http.Request) {
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nabc")
			_ = buf.Flush()
			_ = conn.Close()
		},
		"a redirect": func(_ int, w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		},
	}
}

// brokenWriter fails every write.
type brokenWriter struct{ writes int }

func (w *brokenWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, errors.New("write on a closed pipe")
}

// TestUploadNeverChangesTheRun asserts that for each failure of the endpoint
// (500, 400, a closed connection, a streaming answer over the limit, a 200
// whose body is cut short, a redirect) and for a panic in the sender, the exit
// code, standard output and standard error equal those of the same run with the
// experiment unset; and that with --debug and a debug writer that fails on
// every write, the exit code and standard output are the same.
func TestUploadNeverChangesTheRun(t *testing.T) {
	dir := uploadEnv(t)
	control := runBuilt(t, "dev", "config", "path")
	require.Equal(t, 0, control.code, control.stderr)
	require.NotEmpty(t, control.stdout)
	controlDebug := runBuilt(t, "dev", "--debug", "config", "path")
	require.Equal(t, 0, controlDebug.code, controlDebug.stderr)

	rows := uploadRows(t)
	rows["a panic in the sender"] = nil
	for name, answer := range rows {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, os.RemoveAll(filepath.Join(dir, "telemetry", "upload")))
			seedFinished(t, dir, 3, 1, 2)
			srv := newUploadServer(t, answer)
			withEndpoint(t, srv.URL)
			if name == "a panic in the sender" {
				orig := sendStoredEvents
				sendStoredEvents = func(_ context.Context, _ telemetrysend.Input) { panic("sender failure") }
				t.Cleanup(func() { sendStoredEvents = orig })
			}
			run := runBuilt(t, "dev", "config", "path")
			assert.Equal(t, control.code, run.code)
			assert.Equal(t, control.stdout, run.stdout)
			assert.Equal(t, control.stderr, run.stderr)
			if name != "a panic in the sender" {
				assert.Positive(t, srv.count(), "the sender made a request")
			}
		})
	}

	t.Run("a debug writer that fails", func(t *testing.T) {
		require.NoError(t, os.RemoveAll(filepath.Join(dir, "telemetry", "upload")))
		seedFinished(t, dir, 3, 11, 2)
		srv := newUploadServer(t, nil)
		withEndpoint(t, srv.URL)
		root, err := BuildRoot("dev")
		require.NoError(t, err)
		var out bytes.Buffer
		w := &brokenWriter{}
		root.Writer, root.ErrWriter, root.Reader = &out, w, strings.NewReader("")
		code := Run(t.Context(), root, []string{"rancher", "--debug", "config", "path"})
		assert.Equal(t, controlDebug.code, code)
		assert.Equal(t, controlDebug.stdout, out.String())
		assert.Equal(t, 1, srv.count())
		assert.Equal(t, 2, w.writes, "both debug lines were tried")
	})
}

// TestUploadSendsNoCredential asserts that no request carries an
// Authorization or Cookie header, that each carries the CLI's User-Agent, and
// that a token in the environment and a stored login change neither.
func TestUploadSendsNoCredential(t *testing.T) {
	dir := uploadEnv(t)
	t.Setenv(config.EnvToken, "token-marker-456")
	t.Setenv(config.EnvURL, "https://rancher.invalid")
	cfg := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{"prod": {URL: "https://rancher.invalid"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	seedFinished(t, dir, 3, 1, 2)
	srv := newUploadServer(t, nil)
	withEndpoint(t, srv.URL)
	run := runBuilt(t, "dev", "config", "path")
	require.Equal(t, 0, run.code, run.stderr)
	reqs := srv.requests()
	require.Len(t, reqs, 1)
	h := reqs[0].header
	assert.Empty(t, h.Values("Authorization"))
	assert.Empty(t, h.Values("Cookie"))
	assert.True(t, strings.HasPrefix(h.Get("User-Agent"), "cli.rancher.io/"), h.Get("User-Agent"))
	for name, values := range h {
		for _, v := range values {
			assert.NotContains(t, v, "token-marker-456", name)
		}
	}
}

// TestDebugLinesShowOnlyCountsOriginAndAnswer asserts that with --debug the
// error output holds exactly the lines for each request (the event count, the
// contract version, the canonical origin, the status and the refused count, or
// that the count is unknown, or that there was no answer) and no stored value,
// record id, configured text or path; that without --debug and with
// --debug=false there is no debug line; and that a stored version holding a
// control character prints as unrecognized.
func TestDebugLinesShowOnlyCountsOriginAndAnswer(t *testing.T) {
	seed := func(t *testing.T, dir string, version string) (markers []string) {
		t.Helper()
		for i := range 3 {
			f := exampleEvent(t)
			for k, v := range f {
				if _, ok := v.(string); ok && k != "schema_version" && k != "event.name" && k != "time" {
					marker := "MARKER-" + strings.ReplaceAll(k, ".", "-") + "-" + string(rune('a'+i))
					f[k] = marker
					markers = append(markers, marker)
				}
			}
			f["log.record.uid"] = recordUID(i + 1)
			f["schema_version"] = version
			f["time"] = hourStart(3).Format(time.RFC3339)
			markers = append(markers, recordUID(i+1))
			seedEvent(t, dir, 3, i+1, f)
		}
		return markers
	}

	t.Run("a count and an answer without one", func(t *testing.T) {
		dir := uploadEnv(t)
		markers := seed(t, dir, "1.0.0")
		srv := newUploadServer(t, func(call int, w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"partialSuccess":{"rejectedLogRecords":"2"}}`))
		})
		port := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
		configured := "http://LocalHost:0" + port + "/"
		setExperiment(t, "telemetryupload="+configured, true)
		run := runBuilt(t, "dev", "--debug", "config", "path")
		require.Equal(t, 0, run.code, run.stderr)
		origin := "http://localhost:" + port
		assert.Equal(t, "debug: telemetry upload: sending 3 events (contract 1.0.0) to "+origin+"\n"+
			"debug: telemetry upload: answer HTTP 200, 2 refused\n", run.stderr)
		for _, m := range markers {
			assert.NotContains(t, run.stderr, m)
		}
		assert.NotContains(t, run.stderr, configured)
		assert.NotContains(t, run.stderr, "/v1/logs")
	})

	t.Run("an answer with no count and no answer", func(t *testing.T) {
		restoreBudget, restoreFloor := factsBudget, uploadFloor
		t.Cleanup(func() { factsBudget, uploadFloor = restoreBudget, restoreFloor })
		dir := uploadEnv(t)
		seed(t, dir, "1.0.0")
		srv := newUploadServer(t, replyStatus(503))
		withEndpoint(t, srv.URL)
		run := runBuilt(t, "dev", "--debug", "config", "path")
		assert.Equal(t, "debug: telemetry upload: sending 3 events (contract 1.0.0) to "+srv.URL+"\n"+
			"debug: telemetry upload: answer HTTP 503, refused count unknown\n", run.stderr)

		dir2 := uploadEnv(t)
		seed(t, dir2, "1.0.0")
		factsBudget, uploadFloor = hangBound, 100*time.Millisecond
		hang := newUploadServer(t, func(_ int, _ http.ResponseWriter, r *http.Request) { testtransport.HangAndAbort(r) })
		withEndpoint(t, hang.URL)
		start := time.Now()
		run = runBuilt(t, "dev", "--debug", "config", "path")
		assert.Equal(t, "debug: telemetry upload: sending 3 events (contract 1.0.0) to "+hang.URL+"\n"+
			"debug: telemetry upload: no answer: timeout\n", run.stderr)
		assert.Less(t, time.Since(start), hangBound+10*time.Second)
	})

	t.Run("a stored version with a control character", func(t *testing.T) {
		dir := uploadEnv(t)
		seed(t, dir, "1.0.0\x1b[31m")
		srv := newUploadServer(t, nil)
		withEndpoint(t, srv.URL)
		run := runBuilt(t, "dev", "--debug", "config", "path")
		assert.Contains(t, run.stderr, "(contract unrecognized)")
		assert.NotContains(t, run.stderr, "\x1b")
	})

	t.Run("no debug line without the flag", func(t *testing.T) {
		for name, argv := range map[string][]string{"no flag": {"config", "path"}, "--debug=false": {"--debug=false", "config", "path"}} {
			dir := uploadEnv(t)
			seed(t, dir, "1.0.0")
			srv := newUploadServer(t, nil)
			withEndpoint(t, srv.URL)
			run := runBuilt(t, "dev", argv...)
			assert.Equal(t, 1, srv.count(), name)
			assert.NotContains(t, run.stderr, "debug:", name)
			assert.Empty(t, run.stderr, name)
		}
	})
}

// TestARecordedRunWithTheExperimentReadsTheStateOnce asserts that on a
// development root with a valid endpoint and an attempt that is not due, a
// recorded run makes the file operations of the same run with no experiment and
// then the store open and one read of the state file, and nothing else.
func TestARecordedRunWithTheExperimentReadsTheStateOnce(t *testing.T) {
	dir := uploadEnv(t)
	base := func(argv ...string) []string {
		root, err := BuildRoot("dev")
		require.NoError(t, err)
		ops := observeFiles(t)
		got := runRecorded(t, root, argv...)
		require.Zero(t, got.res.code, got.stderr)
		var out []string
		for _, op := range *ops {
			op = strings.ReplaceAll(op, dir, "DIR")
			out = append(out, slotPath.ReplaceAllString(op, "events/HOUR/SLOT.json"))
		}
		return out
	}
	base("config", "path")
	want := []string{
		"stat DIR", "openroot .", "lstat telemetry", "openroot telemetry", "readfile config-directory-id", "stat pruned",
		"openfile events/HOUR/SLOT.json", "write events/HOUR/SLOT.json", "close events/HOUR/SLOT.json",
	}
	require.Equal(t, want, base("config", "path"), "the list of a recorded run with no experiment")

	srv := newUploadServer(t, nil)
	withEndpoint(t, srv.URL)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "telemetry", "upload"), 0o700))
	state := `{"lastAttempt":"` + time.Now().UTC().Format(time.RFC3339) + `","nextAttempt":"` +
		time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "telemetry", "upload", "state.json"), []byte(state), 0o600))
	got := base("config", "path")
	assert.Equal(t, append(slices.Clone(want), "openroot .", "lstat telemetry", "openroot telemetry", "readfile upload/state.json"), got)
	assert.Zero(t, srv.count())
}

// releaseRequestRows runs the request half of the release build guard.
func releaseRequestRows(t *testing.T) {
	t.Helper()
	for _, version := range releaseVersions {
		t.Run("a request "+version, func(t *testing.T) {
			dir := uploadEnv(t)
			paths := seedFinished(t, dir, 3, 1, 2)
			srv := newUploadServer(t, nil)
			withEndpoint(t, srv.URL)
			run := runBuilt(t, version, "config", "path")
			require.Equal(t, 0, run.code, run.stderr)
			assert.Zero(t, srv.count())
			assert.Len(t, filesAmong(paths), len(paths))
			assert.Empty(t, uploadFilesOf(t, dir))

			direct := func(v, origin string) {
				in := telemetrysend.Input{
					ConfigDir: dir, Version: v, Origin: origin, Encoder: telemetrysend.NewOTLPEncoder(),
					Deadline: time.Now().Add(10 * time.Second), Floor: time.Second, RecordingOn: func() bool { return true },
				}
				telemetrysend.Send(t.Context(), in)
			}
			direct(version, srv.URL)
			assert.Zero(t, srv.count(), "the entry refuses a release version")
			assert.Empty(t, uploadFilesOf(t, dir))
			for _, origin := range []string{"http://h.example", "https://h.example/v1/logs", srv.URL + "/", strings.ToUpper(srv.URL)} {
				direct("dev", origin)
			}
			assert.Zero(t, srv.count(), "the entry refuses an origin the check refuses or changes")
			assert.Empty(t, uploadFilesOf(t, dir))
		})
	}
}

// devRequestRows runs the request half of the development build guard.
func devRequestRows(t *testing.T) {
	t.Helper()
	for _, version := range developmentVersions {
		t.Run("a request "+version, func(t *testing.T) {
			dir := uploadEnv(t)
			seedFinished(t, dir, 3, 1, 2)
			srv := newUploadServer(t, nil)
			withEndpoint(t, srv.URL)
			run := runBuilt(t, version, "config", "path")
			require.Equal(t, 0, run.code, run.stderr)
			assert.Equal(t, 1, srv.count())
		})
	}
}

// noEndpointRequestRows runs the request half of the no endpoint guard: the
// sender is never called when the variable is absent or names no endpoint.
func noEndpointRequestRows(t *testing.T) {
	t.Helper()
	for name, value := range map[string]string{"the variable absent": "", "the bare name": "telemetryupload"} {
		t.Run("no call with "+name, func(t *testing.T) {
			dir := uploadEnv(t)
			seedFinished(t, dir, 3, 1, 2)
			calls := 0
			orig := sendStoredEvents
			sendStoredEvents = func(ctx context.Context, in telemetrysend.Input) { calls++; orig(ctx, in) }
			t.Cleanup(func() { sendStoredEvents = orig })
			setExperiment(t, value, value != "")
			run := runBuilt(t, "dev", "config", "path")
			require.Equal(t, 0, run.code, run.stderr)
			assert.Zero(t, calls)
			assert.Empty(t, uploadFilesOf(t, dir))
		})
	}
}

// execPluginRequestRows runs the request half of the exec plugin guard.
func execPluginRequestRows(t *testing.T) {
	t.Helper()
	type row struct {
		name       string
		execPlugin bool
		argv       []string
		sends      bool
	}
	rows := []row{{"an ordinary run", false, []string{"config", "path"}, true},
		{"an ordinary command in an exec plugin run", true, []string{"config", "path"}, false}}
	for _, name := range slices.Sorted(maps.Keys(uploadExcluded)) {
		for _, execPlugin := range []bool{false, true} {
			rows = append(rows, row{name + " exec plugin " + strconv.FormatBool(execPlugin), execPlugin, strings.Fields(name), false})
		}
	}
	for _, tc := range rows {
		t.Run("a request: "+tc.name, func(t *testing.T) {
			dir := uploadEnv(t)
			seedFinished(t, dir, 3, 1, 2)
			srv := newUploadServer(t, nil)
			withEndpoint(t, srv.URL)
			t.Cleanup(SetInvocation(invocation.Context{ExecPlugin: tc.execPlugin}))
			if tc.execPlugin {
				t.Setenv("KUBERNETES_EXEC_INFO", "{}")
			}
			runBuilt(t, "dev", tc.argv...)
			if tc.sends {
				assert.Equal(t, 1, srv.count())
				return
			}
			assert.Zero(t, srv.count())
			assert.Empty(t, uploadFilesOf(t, dir), "no telemetry/upload is created")
		})
	}
}

// urlRequestRows runs the request half of the URL rules guard.
func urlRequestRows(t *testing.T) {
	t.Helper()
	post := func(t *testing.T, addr string, value func(srv *uploadServer) string) {
		t.Helper()
		dir := uploadEnv(t)
		seedFinished(t, dir, 3, 1, 2)
		srv := serveUpload(t, addr, nil)
		withEndpoint(t, value(srv))
		run := runBuilt(t, "dev", "config", "path")
		require.Equal(t, 0, run.code, run.stderr)
		reqs := srv.requests()
		require.Len(t, reqs, 1)
		assert.Equal(t, "/v1/logs", reqs[0].path, "the canonical origin plus the encoder's path")
	}
	for name, tc := range map[string]struct {
		addr  string
		value func(srv *uploadServer) string
	}{
		"a loopback address":       {"127.0.0.1:0", func(s *uploadServer) string { return s.URL }},
		"a loopback address and /": {"127.0.0.1:0", func(s *uploadServer) string { return s.URL + "/" }},
		"localhost":                {"127.0.0.1:0", func(s *uploadServer) string { return strings.Replace(s.URL, "127.0.0.1", "localhost", 1) }},
		"::1":                      {"[::1]:0", func(s *uploadServer) string { return s.URL }},
		"127.0.0.2":                {"127.0.0.2:0", func(s *uploadServer) string { return s.URL }},
	} {
		t.Run("a request to "+name, func(t *testing.T) { post(t, tc.addr, tc.value) })
	}
}

// TestUploadExcludedNamesRegisteredCommands asserts every name in
// uploadExcluded is the registered name of a command, so a misspelled name
// cannot exclude nothing.
func TestUploadExcludedNamesRegisteredCommands(t *testing.T) {
	t.Parallel()
	wr, err := buildWiredRegistry("v-test", config.ReadOnlyOverride)
	require.NoError(t, err)
	registered := map[string]bool{}
	for _, c := range wr.reg.Commands() {
		registered[c.Describe().Name] = true
	}
	for name := range uploadExcluded {
		assert.True(t, registered[name], "%q is not the name of a registered command", name)
	}
}
