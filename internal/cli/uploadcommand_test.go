package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/invocation"
	"github.com/rancher/rancher-cli/internal/telemetry"
	"github.com/rancher/rancher-cli/internal/telemetrysend"
	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/rancher/rancher-cli/internal/testtransport"
)

var uploadArgv = []string{"telemetry", "upload", "-o", "json"}

// hangBound is the time limit of the rows whose request never answers or whose
// attempt runs to the limit. The store holds at most five events; the walk
// and the first request finish inside it on a slow runner.
const hangBound = 3 * time.Second

// twoBatches is the event count of a row whose store holds two events of
// different contract versions, which the sender sends as two requests.
const twoBatches = -2

// seedTwoBatches writes two events of different contract versions into one
// finished hour.
func seedTwoBatches(t *testing.T, dir string) {
	t.Helper()
	for i, version := range []string{"1.0.0", "1.0.1"} {
		f := exampleEvent(t)
		f["log.record.uid"] = recordUID(i + 1)
		f["schema_version"] = version
		f["time"] = hourStart(3).Format(time.RFC3339)
		seedEvent(t, dir, 3, i+1, f)
	}
}

// uploadData decodes the data of the envelope a successful run wrote.
func uploadData(t *testing.T, run experimentRun) map[string]any {
	t.Helper()
	require.Equal(t, 0, run.code, run.stderr)
	var env struct {
		Kind string         `json:"kind"`
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(run.stdout), &env), run.stdout)
	require.Equal(t, "TelemetryUpload", env.Kind)
	return env.Data
}

// uploadFailure decodes the error a failed run wrote to stderr and checks the
// exit code.
func uploadFailure(t *testing.T, run experimentRun, code int, name string) cliError {
	t.Helper()
	require.Equal(t, code, run.code, run.stderr)
	ce := renderedError(t, "json", run.stderr)
	require.Equal(t, name, ce.Code, run.stderr)
	return ce
}

// storedState reads telemetry/upload/state.json of dir.
func storedState(t *testing.T, dir string) (telemetry.UploadState, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "telemetry", "upload", "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return telemetry.UploadState{}, false
	}
	require.NoError(t, err)
	var st telemetry.UploadState
	require.NoError(t, json.Unmarshal(raw, &st))
	return st, true
}

// within asserts the next attempt of the state lies between an hour and an
// hour plus the jitter after start.
func within(t *testing.T, st telemetry.UploadState, start time.Time) {
	t.Helper()
	assert.GreaterOrEqual(t, st.NextAttempt.Sub(start), time.Hour)
	assert.Less(t, st.NextAttempt.Sub(start), time.Hour+10*time.Minute+time.Second)
}

// TestUploadCommandRefusals asserts that the command refuses with VALIDATION at
// exit 2, naming the reason and never the configured value, makes no request and
// changes nothing in the config directory, for each release version, for a
// missing or invalid endpoint, for each control that turns recording off and for
// an exec plugin run; that the refusals come in the order exec plugin, release
// build, endpoint, recording; and that a config that cannot be loaded stops the
// run with exit 1 before the command runs.
func TestUploadCommandRefusals(t *testing.T) {
	require.Equal(t, []telemetry.Control{telemetry.ControlTestBinary, telemetry.ControlVariable, telemetry.ControlConfig, telemetry.ControlDefault},
		telemetry.Controls(), "a control was added: add a row for it; the default never turns recording off")
	const (
		good   = "https://h.example"
		secret = "https://userMarker7x:passMarker7x@h.example"
	)
	type row struct {
		name      string
		version   string
		value     string
		hasValue  bool
		execPlug  bool
		telemetry string
		config    string
		binary    bool
		contains  string
		hint      string
	}
	rows := []row{
		{name: "no variable", version: "dev", contains: "no endpoint"},
		{name: "the bare name", version: "dev", value: "telemetryupload", hasValue: true, contains: "no endpoint"},
		{name: "an http origin", version: "dev", value: "telemetryupload=http://h.example", hasValue: true, contains: schemeReason},
		{name: "a user name and password", version: "dev", value: "telemetryupload=" + secret, hasValue: true, contains: userReason},
		{name: "a path", version: "dev", value: "telemetryupload=https://h.example/v1/logs", hasValue: true, contains: pathReason},
		{name: "a query", version: "dev", value: "telemetryupload=https://h.example/?q=1", hasValue: true, contains: queryReason},
		{name: "recording off by the variable", version: "dev", value: "telemetryupload=" + good, hasValue: true,
			telemetry: "false", contains: "RANCHER_CLI_TELEMETRY is false"},
		{name: "recording off by the config key", version: "dev", value: "telemetryupload=" + good, hasValue: true,
			config: "telemetry: \"false\"\n", contains: "the telemetry key of config.yaml is false", hint: "telemetry on"},
		{name: "recording off in a test binary", version: "dev", value: "telemetryupload=" + good, hasValue: true,
			binary: true, contains: "decided by test binary"},
		{name: "an exec plugin run", version: "dev", value: "telemetryupload=" + good, hasValue: true, execPlug: true, contains: "kubectl exec plugin"},
		{name: "an exec plugin run in a release build", version: "1.2.3", value: "telemetryupload=" + good, hasValue: true, execPlug: true,
			contains: "kubectl exec plugin"},
		{name: "an exec plugin run with recording off", version: "dev", value: "telemetryupload=" + good, hasValue: true, execPlug: true,
			telemetry: "false", contains: "kubectl exec plugin"},
		{name: "a release build with no endpoint", version: "1.2.3", contains: "a release build ignores RANCHER_CLI_EXPERIMENT"},
		{name: "no endpoint with recording off", version: "dev", telemetry: "false", contains: "no endpoint"},
		{name: "an invalid endpoint with recording off", version: "dev", value: "telemetryupload=http://h.example", hasValue: true,
			telemetry: "false", contains: schemeReason},
	}
	for _, version := range releaseVersions {
		rows = append(rows, row{name: "the release build " + version, version: version, value: "telemetryupload=" + good, hasValue: true,
			contains: "a release build ignores RANCHER_CLI_EXPERIMENT"})
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			var dir string
			if tc.binary {
				experimentEnv(t)
				dir = os.Getenv(config.EnvConfigDir)
				t.Setenv(telemetry.EnvTelemetry, "true")
			} else {
				dir = uploadEnv(t)
			}
			if tc.telemetry != "" {
				t.Setenv(telemetry.EnvTelemetry, tc.telemetry)
			}
			if tc.config != "" {
				writeConfig(t, dir, tc.config)
			}
			paths := seedFinished(t, dir, 3, 1, 2)
			srv := newUploadServer(t, nil)
			if tc.hasValue {
				value := strings.ReplaceAll(tc.value, good, srv.URL)
				if tc.value == "telemetryupload="+good {
					value = "telemetryupload=" + srv.URL
				}
				setExperiment(t, value, true)
			}
			if tc.execPlug {
				t.Cleanup(SetInvocation(invocation.Context{ExecPlugin: true}))
				t.Setenv("KUBERNETES_EXEC_INFO", "{}")
			}
			before := testfs.Snapshot(t, dir)
			run := runBuilt(t, tc.version, uploadArgv...)
			ce := uploadFailure(t, run, 2, clierr.CodeValidation)
			assert.Contains(t, ce.Message, tc.contains)
			if tc.hint == "" {
				assert.Empty(t, ce.Hint)
			} else {
				assert.Contains(t, ce.Hint, tc.hint)
			}
			for _, secret := range []string{"userMarker7x", "passMarker7x", "h.example", srv.URL, "/v1/logs"} {
				assert.NotContains(t, ce.Message, secret)
			}
			assert.Zero(t, srv.count())
			assert.Len(t, filesAmong(paths), len(paths), "nothing was deleted")
			assert.Equal(t, before, testfs.Snapshot(t, dir), "nothing was created or changed")
		})
	}

	t.Run("a config that cannot be loaded", func(t *testing.T) {
		dir := uploadEnv(t)
		writeConfig(t, dir, "telemetry: [a]\n")
		paths := seedFinished(t, dir, 3, 1, 2)
		srv := newUploadServer(t, nil)
		withEndpoint(t, srv.URL)
		before := testfs.Snapshot(t, dir)
		run := runBuilt(t, "dev", uploadArgv...)
		uploadFailure(t, run, 1, clierr.CodeGeneric)
		assert.Zero(t, srv.count())
		assert.Len(t, filesAmong(paths), len(paths))
		assert.Equal(t, before, testfs.Snapshot(t, dir))
	})
}

// TestUploadCommandIgnoresTheHourlyLimit asserts that the command sends after
// an automatic attempt in the same hour and after a 429 with a Retry-After of
// two hours, writes the state of its own attempt, and that a recorded run right
// after it makes no request.
func TestUploadCommandIgnoresTheHourlyLimit(t *testing.T) {
	t.Run("after an automatic attempt in the same hour", func(t *testing.T) {
		dir := uploadEnv(t)
		seedFinished(t, dir, 3, 1, 2)
		srv := newUploadServer(t, nil)
		withEndpoint(t, srv.URL)
		require.Equal(t, 0, runBuilt(t, "dev", "config", "path").code)
		require.Equal(t, 1, srv.count())
		stored, ok := storedState(t, dir)
		require.True(t, ok)
		assert.GreaterOrEqual(t, time.Until(stored.NextAttempt), 59*time.Minute, "the attempt is not due")

		seedFinished(t, dir, 3, 11, 2)
		start := time.Now()
		data := uploadData(t, runBuilt(t, "dev", uploadArgv...))
		assert.Equal(t, 2, srv.count())
		assert.EqualValues(t, 3, data["sent"], "the two seeded events and the event of the recorded run of this hour")
		st, ok := storedState(t, dir)
		require.True(t, ok)
		assert.True(t, st.LastAttempt.After(stored.LastAttempt), "the state holds the attempt of the command")
		within(t, st, start)
		assert.Zero(t, st.Failures)

		seedFinished(t, dir, 3, 21, 2)
		require.Equal(t, 0, runBuilt(t, "dev", "config", "path").code)
		assert.Equal(t, 2, srv.count(), "a recorded run right after the command is not due")
	})

	t.Run("after a 429 with a Retry-After", func(t *testing.T) {
		dir := uploadEnv(t)
		seedFinished(t, dir, 3, 1, 2)
		srv := newUploadServer(t, func(call int, w http.ResponseWriter, _ *http.Request) {
			if call == 0 {
				w.Header().Set("Retry-After", "7200")
				w.WriteHeader(429)
				return
			}
			_, _ = w.Write([]byte("{}"))
		})
		withEndpoint(t, srv.URL)
		require.Equal(t, 0, runBuilt(t, "dev", "config", "path").code)
		stored, ok := storedState(t, dir)
		require.True(t, ok)
		assert.GreaterOrEqual(t, time.Until(stored.NextAttempt), 119*time.Minute)
		assert.Equal(t, 1, stored.Failures)

		start := time.Now()
		data := uploadData(t, runBuilt(t, "dev", uploadArgv...))
		assert.Equal(t, 2, srv.count())
		assert.EqualValues(t, 3, data["sent"], "the two seeded events and the event of the recorded run of this hour")
		st, ok := storedState(t, dir)
		require.True(t, ok)
		within(t, st, start)
		assert.Zero(t, st.Failures)

		seedFinished(t, dir, 3, 21, 2)
		require.Equal(t, 0, runBuilt(t, "dev", "config", "path").code)
		assert.Equal(t, 2, srv.count())
	})
}

// TestUploadCommandSendsTheCurrentHour asserts that, through Run, the command
// sends an event of the current hour and deletes its slot after the 2xx, and
// that a recorded run leaves it.
func TestUploadCommandSendsTheCurrentHour(t *testing.T) {
	hour := hourStart(0)
	dir := uploadEnv(t)
	f := exampleEvent(t)
	f["log.record.uid"] = recordUID(1)
	f["time"] = hour.Format(time.RFC3339)
	slot := seedEvent(t, dir, 0, 1, f)
	srv := newUploadServer(t, nil)
	withEndpoint(t, srv.URL)
	require.Equal(t, 0, runBuilt(t, "dev", "config", "path").code)
	assert.Zero(t, srv.count(), "the automatic attempt leaves the current hour")

	data := uploadData(t, runBuilt(t, "dev", uploadArgv...))
	if !hourStart(0).Equal(hour) {
		t.Skip("the hour changed while the test ran")
	}
	assert.EqualValues(t, 2, data["sent"], "the seeded event and the event of the recorded run of this hour")
	require.Equal(t, 1, srv.count())
	assert.Contains(t, srv.requests()[0].ids(t), recordUID(1))
	assert.False(t, anyExists([]string{slot}))
}

// TestUploadCommandWithNoConfigDirectory asserts that the command, with a valid
// endpoint, exits 0 with nothing waiting and makes no request for a config
// directory that does not exist (which stays missing) and for one with no
// events (which gains no telemetry/upload); that an upload directory with a
// state file and nothing to send gets the state of the attempt; and that the
// next attempt is null with no state file.
func TestUploadCommandWithNoConfigDirectory(t *testing.T) {
	t.Run("a missing config directory", func(t *testing.T) {
		uploadEnv(t)
		dir := filepath.Join(t.TempDir(), "absent")
		t.Setenv(config.EnvConfigDir, dir)
		srv := newUploadServer(t, nil)
		withEndpoint(t, srv.URL)
		data := uploadData(t, runBuilt(t, "dev", uploadArgv...))
		assert.Equal(t, "nothingWaiting", data["skipped"])
		assert.Nil(t, data["nextAttempt"])
		assert.Zero(t, srv.count())
		_, err := os.Lstat(dir)
		assert.ErrorIs(t, err, os.ErrNotExist)
	})

	t.Run("no events", func(t *testing.T) {
		dir := uploadEnv(t)
		srv := newUploadServer(t, nil)
		withEndpoint(t, srv.URL)
		before := testfs.Snapshot(t, dir)
		data := uploadData(t, runBuilt(t, "dev", uploadArgv...))
		assert.Equal(t, "nothingWaiting", data["skipped"])
		assert.Nil(t, data["nextAttempt"])
		assert.Zero(t, srv.count())
		assert.Equal(t, before, testfs.Snapshot(t, dir))
		assert.Empty(t, uploadFilesOf(t, dir))
	})

	t.Run("an upload directory, a state file and nothing to send", func(t *testing.T) {
		dir := uploadEnv(t)
		state := filepath.Join(dir, "telemetry", "upload", "state.json")
		require.NoError(t, os.MkdirAll(filepath.Dir(state), 0o700))
		old := `{"lastAttempt":"2026-01-01T00:00:00Z","httpStatus":503,"failures":2,"nextAttempt":"2026-01-01T01:00:00Z"}`
		require.NoError(t, os.WriteFile(state, []byte(old), 0o600))
		srv := newUploadServer(t, nil)
		withEndpoint(t, srv.URL)
		start := time.Now()
		data := uploadData(t, runBuilt(t, "dev", uploadArgv...))
		assert.Equal(t, "nothingWaiting", data["skipped"])
		assert.Zero(t, srv.count())
		st, ok := storedState(t, dir)
		require.True(t, ok)
		assert.False(t, st.LastAttempt.Before(start.Add(-time.Second)))
		assert.Zero(t, st.Failures)
		within(t, st, start)
		assert.Equal(t, st.NextAttempt.UTC().Format(time.RFC3339), data["nextAttempt"])
		assert.NotContains(t, uploadFilesOf(t, dir), "lock")
	})
}

// TestUploadCommandSharesTheLock asserts the result of the command for a live
// lock, a stale lock, an unreadable lock, a plain file or a symlink in place of
// telemetry/upload and a lock that cannot be created; that none of the failures
// sends a request or reads as nothing waiting; and that a lock error leaves a
// stored wait as it was.
func TestUploadCommandSharesTheLock(t *testing.T) {
	setup := func(t *testing.T) (dir string, srv *uploadServer) {
		t.Helper()
		dir = uploadEnv(t)
		seedFinished(t, dir, 3, 1, 2)
		srv = newUploadServer(t, nil)
		withEndpoint(t, srv.URL)
		return dir, srv
	}

	t.Run("a live lock", func(t *testing.T) {
		dir, srv := setup(t)
		other, err := telemetry.OpenStore(dir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = other.Close() })
		lock, err := other.TakeUploadLock()
		require.NoError(t, err)
		t.Cleanup(func() { _ = lock.Release() })
		run := runBuilt(t, "dev", "telemetry", "upload")
		require.Equal(t, 0, run.code, run.stderr)
		assert.Equal(t, "nothing sent: another upload is running\n", run.stdout)
		assert.Zero(t, srv.count())

		data := uploadData(t, runBuilt(t, "dev", uploadArgv...))
		assert.Equal(t, "lockHeld", data["skipped"])
	})

	t.Run("a stale lock", func(t *testing.T) {
		dir, srv := setup(t)
		lock := filepath.Join(dir, "telemetry", "upload", "lock")
		require.NoError(t, os.MkdirAll(filepath.Dir(lock), 0o700))
		stale := time.Now().Add(-2*time.Minute).UTC().Format(time.RFC3339Nano) + " token"
		require.NoError(t, os.WriteFile(lock, []byte(stale), 0o600))
		if testfs.OnWindows() {
			f, err := os.Open(lock)
			require.NoError(t, err)
			t.Cleanup(func() { _ = f.Close() })
			data := uploadData(t, runBuilt(t, "dev", uploadArgv...))
			assert.Equal(t, "lockHeld", data["skipped"], "a stale lock another program holds open reads as held")
			assert.Zero(t, srv.count())
			return
		}
		data := uploadData(t, runBuilt(t, "dev", uploadArgv...))
		assert.Equal(t, "", data["skipped"])
		assert.EqualValues(t, 2, data["sent"])
		assert.Equal(t, 1, srv.count())
		assert.NotContains(t, uploadFilesOf(t, dir), "lock")
	})

	t.Run("an unreadable lock", func(t *testing.T) {
		dir, srv := setup(t)
		lock := filepath.Join(dir, "telemetry", "upload", "lock")
		testfs.DirectoryInPlaceOfFile(t, lock)
		run := runBuilt(t, "dev", uploadArgv...)
		ce := uploadFailure(t, run, 1, clierr.CodeGeneric)
		assert.Contains(t, ce.Message, lock)
		assert.Contains(t, ce.Message, "cannot be read")
		assert.Contains(t, ce.Message, "telemetry reset")
		assert.Zero(t, srv.count())
		st, ok := storedState(t, dir)
		require.True(t, ok, "the backoff state is written")
		assert.Equal(t, "lock unavailable", st.NoAnswer)
	})

	t.Run("an unreadable lock with a wait stored", func(t *testing.T) {
		dir, srv := setup(t)
		upload := filepath.Join(dir, "telemetry", "upload")
		require.NoError(t, os.MkdirAll(upload, 0o700))
		next := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
		stored := `{"lastAttempt":"` + time.Now().UTC().Format(time.RFC3339) + `","httpStatus":429,"failures":1,"nextAttempt":"` + next + `"}`
		require.NoError(t, os.WriteFile(filepath.Join(upload, "state.json"), []byte(stored), 0o600))
		testfs.DirectoryInPlaceOfFile(t, filepath.Join(upload, "lock"))
		run := runBuilt(t, "dev", uploadArgv...)
		uploadFailure(t, run, 1, clierr.CodeGeneric)
		assert.Zero(t, srv.count())
		raw, err := os.ReadFile(filepath.Join(upload, "state.json"))
		require.NoError(t, err)
		assert.Equal(t, stored, string(raw), "a lock error is not an answer: the stored wait stays")
	})

	t.Run("a plain file in place of the upload directory", func(t *testing.T) {
		dir, srv := setup(t)
		upload := filepath.Join(dir, "telemetry", "upload")
		require.NoError(t, os.WriteFile(upload, []byte("x"), 0o600))
		ce := uploadFailure(t, runBuilt(t, "dev", uploadArgv...), 1, clierr.CodeGeneric)
		assert.Contains(t, ce.Message, upload)
		assert.Contains(t, ce.Message, "symlink, not a directory")
		assert.Contains(t, ce.Message, "telemetry reset")
		assert.Zero(t, srv.count())
		info, err := os.Lstat(upload)
		require.NoError(t, err)
		assert.True(t, info.Mode().IsRegular(), "nothing replaced it")
	})

	t.Run("a symlink in place of the upload directory", func(t *testing.T) {
		dir, srv := setup(t)
		upload := filepath.Join(dir, "telemetry", "upload")
		target := t.TempDir()
		testfs.Symlink(t, target, upload)
		ce := uploadFailure(t, runBuilt(t, "dev", uploadArgv...), 1, clierr.CodeGeneric)
		assert.Contains(t, ce.Message, upload)
		assert.Zero(t, srv.count())
		assert.Empty(t, testfs.Tree(t, target), "nothing was written through the link")
	})

	t.Run("a lock that cannot be created", func(t *testing.T) {
		dir, srv := setup(t)
		upload := filepath.Join(dir, "telemetry", "upload")
		require.NoError(t, os.MkdirAll(upload, 0o700))
		testfs.RestrictAccess(t, upload, 0o500)
		ce := uploadFailure(t, runBuilt(t, "dev", uploadArgv...), 1, clierr.CodeGeneric)
		assert.Contains(t, ce.Message, "cannot be created: permission denied")
		assert.Contains(t, ce.Message, "write access to "+upload)
		assert.Contains(t, ce.Message, "telemetry reset")
		assert.NotContains(t, ce.Message, "hard link", "the hard link remedy is for a refused link only")
		assert.Zero(t, srv.count())
	})
}

// TestUploadCommandBoundIsTheTelemetryBound asserts that the limit the command
// runs under, when no test has shrunk it, is the bound the telemetry package
// keeps below the age at which another process takes over the upload lock.
func TestUploadCommandBoundIsTheTelemetryBound(t *testing.T) {
	assert.Equal(t, telemetry.UploadCommandBound, uploadCommandBound)
}

// TestUploadCommandIsBounded asserts that, with the limit and the floor shrunk
// together, an endpoint that never answers gets one request and the run ends
// after the limit and before the limit plus a margin, leaving no lock and a
// state with the reason; that a limit that passes before the first request is
// the skipped value timeUp with its line and no request; and that a limit that
// passes after the first request stops the rest.
func TestUploadCommandIsBounded(t *testing.T) {
	shrink := func(t *testing.T, bound, floor time.Duration) {
		t.Helper()
		oldBound, oldFloor := uploadCommandBound, uploadFloor
		t.Cleanup(func() { uploadCommandBound, uploadFloor = oldBound, oldFloor })
		uploadCommandBound, uploadFloor = bound, floor
	}

	t.Run("an endpoint that never answers", func(t *testing.T) {
		shrink(t, hangBound, 100*time.Millisecond)
		dir := uploadEnv(t)
		seedFinished(t, dir, 3, 1, 2)
		release := make(chan struct{})
		srv := newUploadServer(t, func(_ int, _ http.ResponseWriter, r *http.Request) { testtransport.HangUntilReleased(r, release) })
		t.Cleanup(func() { close(release) })
		withEndpoint(t, srv.URL)
		start := time.Now()
		run := runBuilt(t, "dev", uploadArgv...)
		elapsed := time.Since(start)
		data := uploadData(t, run)
		assert.Equal(t, 1, srv.count())
		assert.GreaterOrEqual(t, elapsed, hangBound-100*time.Millisecond, "the request waited for the limit")
		assert.Less(t, elapsed, hangBound+10*time.Second, "and no longer")
		assert.Equal(t, "timeout", data["noAnswer"])
		assert.Nil(t, data["httpStatus"])
		assert.NotContains(t, uploadFilesOf(t, dir), "lock")
		st, ok := storedState(t, dir)
		require.True(t, ok)
		assert.Equal(t, "timeout", st.NoAnswer)
	})

	t.Run("the limit passes before the first request", func(t *testing.T) {
		shrink(t, time.Second, 100*time.Millisecond)
		dir := uploadEnv(t)
		paths := seedFinished(t, dir, 3, 1, 2)
		srv := newUploadServer(t, nil)
		withEndpoint(t, srv.URL)
		// The first walk of the sender's attempt takes until 50 ms are left of
		// the limit: less than the floor, and more than none.
		senderStarted, walks := false, 0
		t.Cleanup(telemetry.SetObserverForTest(func(op string) {
			switch {
			case op == "readfile upload/state.json":
				senderStarted = true
			case senderStarted && op == "readdir events":
				walks++
				if walks == 1 {
					time.Sleep(time.Second - 50*time.Millisecond)
				}
			}
		}))
		run := runBuilt(t, "dev", "telemetry", "upload")
		require.Equal(t, 0, run.code, run.stderr)
		assert.Equal(t, "nothing sent: the 30-second limit passed before the first request\n", run.stdout)
		assert.Zero(t, srv.count())
		assert.Len(t, filesAmong(paths), len(paths))
		assert.Empty(t, uploadFilesOf(t, dir), "no lock was taken")
	})

	t.Run("the limit passes before the sender starts", func(t *testing.T) {
		shrink(t, 600*time.Millisecond, 100*time.Millisecond)
		dir := uploadEnv(t)
		seedFinished(t, dir, 3, 1, 2)
		srv := newUploadServer(t, nil)
		withEndpoint(t, srv.URL)
		orig := sendNow
		t.Cleanup(func() { sendNow = orig })
		sendNow = func(ctx context.Context, in telemetrysend.Input) telemetry.UploadResult {
			time.Sleep(time.Until(in.Deadline) + 20*time.Millisecond)
			return orig(ctx, in)
		}
		data := uploadData(t, runBuilt(t, "dev", uploadArgv...))
		assert.Equal(t, "timeUp", data["skipped"])
		assert.Equal(t, true, data["timeLimitReached"])
		assert.Zero(t, srv.count())
		assert.Empty(t, uploadFilesOf(t, dir))
	})

	t.Run("the limit passes after the first request", func(t *testing.T) {
		clock := stopSenderClock(t)
		dir := uploadEnv(t)
		seedTwoBatches(t, dir)
		srv := newUploadServer(t, nil)
		withEndpoint(t, srv.URL)
		clock.install(t, true)
		run := runBuilt(t, "dev", "telemetry", "upload")
		require.Equal(t, 0, run.code, run.stderr)
		assert.Equal(t, 1, srv.count())
		assert.Contains(t, run.stdout, "sent 1 events (HTTP 200, 0 refused), then stopped at the 30-second limit, the rest kept; next automatic attempt after ")
	})
}

// TestUploadCommandOutput asserts, through Run in text and in JSON, the line and
// the data for each answer of the endpoint, and that every row exits 0.
func TestUploadCommandOutput(t *testing.T) {
	const next = "; next automatic attempt after "
	oneRefused := map[string]any{"sent": 5, "refused": 3, "dropped": 0, "httpStatus": 200, "noAnswer": "", "skipped": "", "timeLimitReached": false}
	rows := []struct {
		name   string
		events int
		answer uploadAnswer
		hang   bool
		late   bool
		line   string
		want   map[string]any
	}{
		{"a 200 with refusals", 5, func(_ int, w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"partialSuccess":{"rejectedLogRecords":"3"}}`))
		}, false, false, "sent 5 events (HTTP 200, 3 refused)" + next, oneRefused},
		{"a 200 with an unparsable body", 5, func(_ int, w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }, false, false,
			"sent 5 events (HTTP 200, refused count unknown)" + next,
			map[string]any{"sent": 5, "refused": nil, "dropped": 0, "httpStatus": 200, "noAnswer": "", "skipped": "", "timeLimitReached": false}},
		{"a 503", 5, replyStatus(503), false, false, "sent 0 events: HTTP 503, files kept" + next,
			map[string]any{"sent": 0, "refused": nil, "dropped": 0, "httpStatus": 503, "noAnswer": "", "skipped": "", "timeLimitReached": false}},
		{"a 400", 5, replyStatus(400), false, false, "sent 0 events: HTTP 400, 5 events deleted" + next,
			map[string]any{"sent": 0, "refused": nil, "dropped": 5, "httpStatus": 400, "noAnswer": "", "skipped": "", "timeLimitReached": false}},
		{"a timeout", 5, func(_ int, _ http.ResponseWriter, r *http.Request) { testtransport.HangAndAbort(r) }, true, false,
			"sent 0 events: no answer (timeout), files kept" + next,
			map[string]any{"sent": 0, "refused": nil, "dropped": 0, "httpStatus": nil, "noAnswer": "timeout", "skipped": "", "timeLimitReached": false}},
		{"a 2xx then a timeout", twoBatches, func(call int, w http.ResponseWriter, r *http.Request) {
			if call == 0 {
				_, _ = w.Write([]byte("{}"))
				return
			}
			testtransport.HangAndAbort(r)
		}, true, false, "sent 1 events (HTTP 200, 0 refused), then no answer (timeout), files kept" + next,
			map[string]any{"sent": 1, "refused": 0, "dropped": 0, "httpStatus": nil, "noAnswer": "timeout", "skipped": "", "timeLimitReached": false}},
		{"a 400 then a 503", twoBatches, func(call int, w http.ResponseWriter, _ *http.Request) {
			if call == 0 {
				w.WriteHeader(400)
				return
			}
			w.WriteHeader(503)
		}, false, false, "sent 0 events: HTTP 503, files kept, 1 events deleted" + next,
			map[string]any{"sent": 0, "refused": nil, "dropped": 1, "httpStatus": 503, "noAnswer": "", "skipped": "", "timeLimitReached": false}},
		{"a 2xx then a 400", twoBatches, func(call int, w http.ResponseWriter, _ *http.Request) {
			if call == 0 {
				_, _ = w.Write([]byte("{}"))
				return
			}
			w.WriteHeader(400)
		}, false, false, "sent 1 events (HTTP 200, 0 refused), then HTTP 400, 1 events deleted" + next,
			map[string]any{"sent": 1, "refused": 0, "dropped": 1, "httpStatus": 400, "noAnswer": "", "skipped": "", "timeLimitReached": false}},
		{"the limit after the first request", twoBatches, nil, false, true,
			"sent 1 events (HTTP 200, 0 refused), then stopped at the 30-second limit, the rest kept" + next,
			map[string]any{"sent": 1, "refused": 0, "dropped": 0, "httpStatus": 200, "noAnswer": "", "skipped": "", "timeLimitReached": true}},
		{"nothing waiting", 0, nil, false, false, "nothing waiting",
			map[string]any{"sent": 0, "refused": nil, "dropped": 0, "httpStatus": nil, "noAnswer": "", "skipped": "nothingWaiting", "timeLimitReached": false}},
	}
	for _, row := range rows {
		for _, format := range []string{"json", "text", "yaml"} {
			if format == "yaml" && row.name != "a 200 with refusals" {
				continue
			}
			t.Run(row.name+" "+format, func(t *testing.T) {
				var clock *senderClock
				if row.hang || row.late {
					clock = stopSenderClock(t)
				}
				dir := uploadEnv(t)
				if row.events == twoBatches {
					seedTwoBatches(t, dir)
				} else {
					seedFinished(t, dir, 3, 1, row.events)
				}
				answer := row.answer
				if row.hang {
					base := answer
					hangAt := 0
					if row.events == twoBatches {
						hangAt = 1
					}
					answer = func(call int, w http.ResponseWriter, r *http.Request) {
						if call == hangAt {
							clock.pass()
						}
						base(call, w, r)
					}
				}
				srv := newUploadServer(t, answer)
				withEndpoint(t, srv.URL)
				if clock != nil {
					clock.install(t, row.late)
				}
				argv := []string{"telemetry", "upload"}
				if format != "text" {
					argv = append(argv, "-o", format)
				}
				run := runBuilt(t, "dev", argv...)
				require.Equal(t, 0, run.code, run.stderr)
				if format == "text" {
					assert.True(t, strings.HasPrefix(run.stdout, row.line), run.stdout)
					if strings.HasSuffix(row.line, next) {
						assert.Regexp(t, `after 20\d\d-\d\d-\d\dT\d\d:\d\d:\d\dZ\n$`, run.stdout)
					} else {
						assert.Equal(t, row.line+"\n", run.stdout)
					}
					return
				}
				var env struct {
					Kind string         `json:"kind"`
					Data map[string]any `json:"data"`
				}
				if format == "json" {
					require.NoError(t, json.Unmarshal([]byte(run.stdout), &env), run.stdout)
				} else {
					require.NoError(t, yaml.Unmarshal([]byte(run.stdout), &env), run.stdout)
				}
				data := env.Data
				require.NotNil(t, data, run.stdout)
				nextAttempt := data["nextAttempt"]
				delete(data, "nextAttempt")
				got := map[string]any{}
				for k, v := range data {
					if f, ok := v.(float64); ok {
						v = int(f)
					}
					got[k] = v
				}
				assert.Equal(t, row.want, got)
				if row.name == "nothing waiting" {
					assert.Nil(t, nextAttempt)
				} else {
					assert.NotNil(t, nextAttempt)
				}
			})
		}
	}
}

// TestDebugLinesOfTheUploadCommand asserts that `rancher --debug telemetry
// upload` and `rancher telemetry upload --debug` write exactly the debug lines
// of the sender to the error output and the result alone to standard output,
// with no stored value, record id, configured text or path; and that
// --debug=false and no flag write no line.
func TestDebugLinesOfTheUploadCommand(t *testing.T) {
	seed := func(t *testing.T, dir string) (markers []string) {
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
			f["schema_version"] = "1.0.0"
			f["time"] = hourStart(3).Format(time.RFC3339)
			markers = append(markers, recordUID(i+1))
			seedEvent(t, dir, 3, i+1, f)
		}
		return markers
	}
	for name, argv := range map[string][]string{
		"typed before the command": {"--debug", "telemetry", "upload"},
		"typed after the command":  {"telemetry", "upload", "--debug"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := uploadEnv(t)
			markers := seed(t, dir)
			srv := newUploadServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"partialSuccess":{"rejectedLogRecords":"2"}}`))
			})
			port := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
			configured := "http://LocalHost:0" + port + "/"
			setExperiment(t, "telemetryupload="+configured, true)
			run := runBuilt(t, "dev", argv...)
			require.Equal(t, 0, run.code, run.stderr)
			origin := "http://localhost:" + port
			assert.Equal(t, "debug: telemetry upload: sending 3 events (contract 1.0.0) to "+origin+"\n"+
				"debug: telemetry upload: answer HTTP 200, 2 refused\n", run.stderr)
			assert.True(t, strings.HasPrefix(run.stdout, "sent 3 events (HTTP 200, 2 refused); next automatic attempt after "), run.stdout)
			assert.Equal(t, 1, strings.Count(run.stdout, "\n"), "the result alone")
			for _, m := range markers {
				assert.NotContains(t, run.stderr+run.stdout, m)
			}
			assert.NotContains(t, run.stderr+run.stdout, configured)
			assert.NotContains(t, run.stderr+run.stdout, "/v1/logs")
		})
	}

	t.Run("no debug line without the flag", func(t *testing.T) {
		for name, argv := range map[string][]string{
			"no flag":            {"telemetry", "upload"},
			"--debug=false":      {"--debug=false", "telemetry", "upload"},
			"--debug=false last": {"telemetry", "upload", "--debug=false"},
		} {
			dir := uploadEnv(t)
			seed(t, dir)
			srv := newUploadServer(t, nil)
			withEndpoint(t, srv.URL)
			run := runBuilt(t, "dev", argv...)
			require.Equal(t, 0, run.code, name+run.stderr)
			assert.Equal(t, 1, srv.count(), name)
			assert.Empty(t, run.stderr, name)
		}
	})
}

// TestUploadCommandRecordsNothing asserts that a run of the command stores no
// event, takes no part in the automatic attempt (one request in all, from the
// command) and leaves the events of the current hour out of the store.
func TestUploadCommandRecordsNothing(t *testing.T) {
	dir := uploadEnv(t)
	seedFinished(t, dir, 3, 1, 2)
	srv := newUploadServer(t, nil)
	withEndpoint(t, srv.URL)
	calls := 0
	orig := sendStoredEvents
	t.Cleanup(func() { sendStoredEvents = orig })
	sendStoredEvents = func(ctx context.Context, in telemetrysend.Input) { calls++; orig(ctx, in) }
	uploadData(t, runBuilt(t, "dev", uploadArgv...))
	assert.Equal(t, 1, srv.count())
	assert.Zero(t, calls, "the run makes no automatic attempt")
	for _, p := range testfs.Tree(t, filepath.Join(dir, "telemetry", "events")) {
		assert.False(t, strings.HasPrefix(p, hourName(hourStart(0))), "an event of this run was recorded: %s", p)
	}
}

// TestUploadCommandSendsNoCredential asserts that no request of the command
// carries an Authorization or Cookie header, that each carries the CLI's
// User-Agent, and that a token in the environment and a stored login change
// neither.
func TestUploadCommandSendsNoCredential(t *testing.T) {
	dir := uploadEnv(t)
	t.Setenv(config.EnvToken, "token-marker-789")
	t.Setenv(config.EnvURL, "https://rancher.invalid")
	cfg := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{"prod": {URL: "https://rancher.invalid"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	seedFinished(t, dir, 3, 1, 2)
	srv := newUploadServer(t, nil)
	withEndpoint(t, srv.URL)
	uploadData(t, runBuilt(t, "dev", uploadArgv...))
	reqs := srv.requests()
	require.Len(t, reqs, 1)
	h := reqs[0].header
	assert.Empty(t, h.Values("Authorization"))
	assert.Empty(t, h.Values("Cookie"))
	assert.True(t, strings.HasPrefix(h.Get("User-Agent"), "cli.rancher.io/"), h.Get("User-Agent"))
	for name, values := range h {
		for _, v := range values {
			assert.NotContains(t, v, "token-marker-789", name)
		}
	}
}

// TestUploadCommandTakesNoConfigLock asserts that, at the time the command
// calls the sender, the config lock is free: a probe that takes it from inside
// the sender seam gets it at once. A command that held the config lock across
// the sender would make the probe time out.
func TestUploadCommandTakesNoConfigLock(t *testing.T) {
	dir := uploadEnv(t)
	seedFinished(t, dir, 3, 1, 2)
	srv := newUploadServer(t, nil)
	withEndpoint(t, srv.URL)
	probed := false
	orig := sendNow
	t.Cleanup(func() { sendNow = orig })
	sendNow = func(ctx context.Context, in telemetrysend.Input) telemetry.UploadResult {
		probe, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		err := config.WithLock(probe, dir, func() error { probed = true; return nil })
		require.NoError(t, err, "the config lock is held while the sender runs")
		return orig(ctx, in)
	}
	data := uploadData(t, runBuilt(t, "dev", uploadArgv...))
	assert.True(t, probed, "the probe ran and took the lock")
	assert.EqualValues(t, 2, data["sent"])
}

// failingEncoder fails to encode.
type failingEncoder struct{}

func (failingEncoder) Encode([]telemetry.StoredEvent) (telemetrysend.Request, error) {
	return telemetrysend.Request{}, errors.New("encoder failure")
}
func (failingEncoder) RefusedCount([]byte) (int, bool) { return 0, false }

// panickingEncoder panics when asked to encode.
type panickingEncoder struct{}

func (panickingEncoder) Encode([]telemetry.StoredEvent) (telemetrysend.Request, error) {
	panic("encoder failure")
}
func (panickingEncoder) RefusedCount([]byte) (int, bool) { return 0, false }

// TestUploadCommandReportsFailures asserts that each failure inside an allowed
// attempt exits 1 with the generic code and a message that names the failed step
// and the requests already sent, that recording turned off during the attempt
// exits 2 with the validation code and deletes nothing, and that Ctrl-C during a
// request exits 130 with the interrupted code; and that none exits 0.
func TestUploadCommandReportsFailures(t *testing.T) {
	replaceEncoder := func(t *testing.T, enc telemetrysend.Encoder) {
		t.Helper()
		orig := sendNow
		t.Cleanup(func() { sendNow = orig })
		sendNow = func(ctx context.Context, in telemetrysend.Input) telemetry.UploadResult {
			in.Encoder = enc
			return orig(ctx, in)
		}
	}
	setup := func(t *testing.T) (dir string, paths []string, srv *uploadServer) {
		t.Helper()
		dir = uploadEnv(t)
		paths = seedFinished(t, dir, 3, 1, 2)
		srv = newUploadServer(t, nil)
		withEndpoint(t, srv.URL)
		return dir, paths, srv
	}

	t.Run("a telemetry path that is a plain file", func(t *testing.T) {
		dir := uploadEnv(t)
		path := filepath.Join(dir, "telemetry")
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
		srv := newUploadServer(t, nil)
		withEndpoint(t, srv.URL)
		ce := uploadFailure(t, runBuilt(t, "dev", uploadArgv...), 1, clierr.CodeGeneric)
		assert.Contains(t, ce.Message, path)
		assert.Contains(t, ce.Message, "requests sent: 0")
		assert.Zero(t, srv.count())
	})

	t.Run("a telemetry path that is a symlink", func(t *testing.T) {
		dir := uploadEnv(t)
		path := filepath.Join(dir, "telemetry")
		testfs.Symlink(t, t.TempDir(), path)
		srv := newUploadServer(t, nil)
		withEndpoint(t, srv.URL)
		ce := uploadFailure(t, runBuilt(t, "dev", uploadArgv...), 1, clierr.CodeGeneric)
		assert.Contains(t, ce.Message, path)
		assert.Zero(t, srv.count())
	})

	t.Run("a walk that fails", func(t *testing.T) {
		dir, paths, srv := setup(t)
		t.Run("the walk", func(t *testing.T) {
			testfs.RestrictAccess(t, filepath.Join(dir, "telemetry", "events"), 0)
			ce := uploadFailure(t, runBuilt(t, "dev", uploadArgv...), 1, clierr.CodeGeneric)
			assert.Contains(t, ce.Message, "cannot be listed")
			assert.Contains(t, ce.Message, "requests sent: 0")
			assert.Zero(t, srv.count())
		})
		assert.Len(t, filesAmong(paths), len(paths))
	})

	t.Run("an encoder that fails", func(t *testing.T) {
		dir, paths, srv := setup(t)
		replaceEncoder(t, failingEncoder{})
		ce := uploadFailure(t, runBuilt(t, "dev", uploadArgv...), 1, clierr.CodeGeneric)
		assert.Contains(t, ce.Message, "a request could not be built")
		assert.Contains(t, ce.Message, "requests sent: 0")
		assert.Zero(t, srv.count())
		assert.Len(t, filesAmong(paths), len(paths))
		assert.NotContains(t, uploadFilesOf(t, dir), "lock")
	})

	t.Run("a sender that panics", func(t *testing.T) {
		dir, paths, srv := setup(t)
		replaceEncoder(t, panickingEncoder{})
		ce := uploadFailure(t, runBuilt(t, "dev", uploadArgv...), 1, clierr.CodeGeneric)
		assert.Contains(t, ce.Message, "unexpected error")
		assert.Zero(t, srv.count())
		assert.Len(t, filesAmong(paths), len(paths))
		assert.NotContains(t, uploadFilesOf(t, dir), "lock")
	})

	t.Run("recording turned off during the attempt", func(t *testing.T) {
		dir := uploadEnv(t)
		paths := seedFinished(t, dir, 3, 1, 2)
		srv := newUploadServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
			require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("telemetry: \"false\"\n"), 0o600))
			_, _ = w.Write([]byte("{}"))
		})
		withEndpoint(t, srv.URL)
		ce := uploadFailure(t, runBuilt(t, "dev", uploadArgv...), 2, clierr.CodeValidation)
		assert.Contains(t, ce.Message, "nothing was deleted")
		assert.Contains(t, ce.Message, "requests sent: 1")
		assert.Equal(t, 1, srv.count())
		assert.Len(t, filesAmong(paths), len(paths))
		assert.NotContains(t, uploadFilesOf(t, dir), "state.json")
	})

	t.Run("Ctrl-C during a request", func(t *testing.T) {
		dir := uploadEnv(t)
		paths := seedFinished(t, dir, 3, 1, 2)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		srv := newUploadServer(t, func(_ int, _ http.ResponseWriter, r *http.Request) {
			cancel()
			testtransport.HangAndAbort(r)
		})
		withEndpoint(t, srv.URL)
		root, err := BuildRoot("dev")
		require.NoError(t, err)
		var out, errOut strings.Builder
		root.Writer, root.ErrWriter, root.Reader = &out, &errOut, strings.NewReader("")
		code := Run(ctx, root, append([]string{"rancher"}, uploadArgv...))
		run := experimentRun{stdout: out.String(), stderr: errOut.String(), code: code}
		ce := uploadFailure(t, run, 130, clierr.CodeInterrupted)
		assert.Contains(t, ce.Message, "requests sent: 1")
		assert.Equal(t, 1, srv.count())
		assert.Len(t, filesAmong(paths), len(paths), "no 2xx covered a file")
		assert.NotContains(t, uploadFilesOf(t, dir), "lock")
	})
}

// TestUploadCommandUnderReadOnly asserts that the command runs under the
// read-only source of the environment and of config.yaml (exit 0, one request),
// and that a typed --read-only is refused as for every local command.
func TestUploadCommandUnderReadOnly(t *testing.T) {
	for _, source := range []string{"environment", "config"} {
		t.Run(source, func(t *testing.T) {
			dir := uploadEnv(t)
			seedFinished(t, dir, 3, 1, 2)
			srv := newUploadServer(t, nil)
			withEndpoint(t, srv.URL)
			if source == "environment" {
				t.Setenv(config.EnvReadOnly, "1")
			} else {
				writeConfig(t, dir, "readOnly: true\n")
			}
			data := uploadData(t, runBuilt(t, "dev", uploadArgv...))
			assert.EqualValues(t, 2, data["sent"])
			assert.Equal(t, 1, srv.count())
		})
	}

	t.Run("the typed flag", func(t *testing.T) {
		dir := uploadEnv(t)
		seedFinished(t, dir, 3, 1, 2)
		srv := newUploadServer(t, nil)
		withEndpoint(t, srv.URL)
		ce := uploadFailure(t, runBuilt(t, "dev", "telemetry", "upload", "--read-only", "-o", "json"), 2, clierr.CodeValidation)
		assert.Contains(t, ce.Message, "telemetry upload is a local command and --read-only does not apply to it")
		assert.Empty(t, ce.Hint)
		assert.Zero(t, srv.count())
	})
}
