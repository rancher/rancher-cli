package telemetrysend

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/telemetry"
	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/rancher/rancher-cli/internal/testtransport"
)

// TestSlotEventsOfAFinishedHourAreSent asserts an event still in a slot of a
// finished hour is sent and its slot deleted after the 2xx, and an event in
// both a slot and a compacted file is sent once and both files are deleted.
func TestSlotEventsOfAFinishedHourAreSent(t *testing.T) {
	dir := uploadDir(t)
	hour := hourAgo(3)
	only := putSlot(t, dir, hour, 1, eventFields(t, 1, hour))
	both := putSlot(t, dir, hour, 2, eventFields(t, 2, hour))
	file := putCompactedFile(t, dir, hour, hourDir(hour)+".jsonl", time.Now().Add(-2*time.Minute), eventFields(t, 2, hour), eventFields(t, 3, hour))
	srv := newTestServer(t, nil)
	Send(t.Context(), input(dir, srv.URL))
	reqs := srv.seenRequests()
	require.Len(t, reqs, 1)
	assert.Equal(t, []string{uidOf(2), uidOf(3), uidOf(1)}, reqs[0].ids(t), "compacted files first, each event once")
	for _, p := range []string{only, both, file} {
		assert.False(t, exists(dir, p), p)
	}
}

// TestEveryStoredEventIsSent asserts events stored before the experiment was
// set and events a release build stored are all sent: the sender filters by
// neither.
func TestEveryStoredEventIsSent(t *testing.T) {
	dir := uploadDir(t)
	for i, version := range []string{"dev", "1.2.3", "0.1.0-rc.1", "dev-abc123"} {
		f := eventFields(t, i+1, hourAgo(3+i))
		f["service.version"] = version
		putSlot(t, dir, hourAgo(3+i), 1, f)
	}
	srv := newTestServer(t, nil)
	Send(t.Context(), input(dir, srv.URL))
	reqs := srv.seenRequests()
	require.Len(t, reqs, 1)
	assert.Len(t, reqs[0].ids(t), 4)
	versions := map[any]bool{}
	for _, f := range reqs[0].flat(t) {
		versions[f["service.version"]] = true
	}
	assert.Len(t, versions, 4)
}

// TestUploadAgeLimits asserts hours 2, 4 and 29 days old are sent, an hour 31
// days old is in no request and its files stay, and a directory of only such
// hours causes no request, no telemetry/upload and no write.
func TestUploadAgeLimits(t *testing.T) {
	day := 24
	dir := uploadDir(t)
	sent := []string{
		putCompactedFile(t, dir, hourAgo(2*day), hourDir(hourAgo(2*day))+".jsonl", time.Now().Add(-time.Hour), eventFields(t, 1, hourAgo(2*day))),
		putSlot(t, dir, hourAgo(4*day), 1, eventFields(t, 2, hourAgo(4*day))),
		putSlot(t, dir, hourAgo(29*day), 1, eventFields(t, 3, hourAgo(29*day))),
	}
	old := putSlot(t, dir, hourAgo(31*day), 1, eventFields(t, 4, hourAgo(31*day)))
	srv := newTestServer(t, nil)
	Send(t.Context(), input(dir, srv.URL))
	reqs := srv.seenRequests()
	require.Len(t, reqs, 1)
	assert.ElementsMatch(t, []string{uidOf(1), uidOf(2), uidOf(3)}, reqs[0].ids(t))
	assert.Empty(t, filesWaiting(dir, sent))
	assert.True(t, exists(dir, old), "the files of a skipped hour stay")
	st, _ := attemptState(t, dir)
	assert.Equal(t, 3, st.Sent)
	assert.Zero(t, st.Dropped)

	only := uploadDir(t)
	putSlot(t, only, hourAgo(31*day), 1, eventFields(t, 5, hourAgo(31*day)))
	putSlot(t, only, hourAgo(40*day), 1, eventFields(t, 6, hourAgo(40*day)))
	before := testfs.Snapshot(t, only)
	other := newTestServer(t, nil)
	Send(t.Context(), input(only, other.URL))
	assert.Empty(t, other.seenRequests())
	assert.Equal(t, before, testfs.Snapshot(t, only), "no telemetry/upload, nothing written")
}

// TestYoungCompactedFileWaits asserts a 2xx for slot events does not delete a
// compacted file another process wrote 30 seconds ago that holds the same
// events, and that at two minutes old the file is sent and deleted.
func TestYoungCompactedFileWaits(t *testing.T) {
	dir := uploadDir(t)
	hour := hourAgo(3)
	slots := putSlotRange(t, dir, hour, 1, 3)
	events := []map[string]any{eventFields(t, 1, hour), eventFields(t, 2, hour), eventFields(t, 3, hour)}
	file := putCompactedFile(t, dir, hour, hourDir(hour)+".jsonl", time.Now().Add(-30*time.Second), events...)
	srv := newTestServer(t, nil)
	Send(t.Context(), input(dir, srv.URL))
	require.Len(t, srv.seenRequests(), 1)
	assert.Empty(t, filesWaiting(dir, slots))
	assert.True(t, exists(dir, file), "a young file is not listed and not deleted")

	old := time.Now().Add(-2 * time.Minute)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "telemetry", filepath.FromSlash(file)), old, old))
	st, _ := attemptState(t, dir)
	st.NextAttempt = time.Now().Add(-time.Second)
	writeState(t, dir, st)
	Send(t.Context(), input(dir, srv.URL))
	reqs := srv.seenRequests()
	require.Len(t, reqs, 2)
	assert.Len(t, reqs[1].ids(t), 3)
	assert.False(t, exists(dir, file))
}

// TestAttemptLooksThenLocksThenListsAgain asserts an attempt on a directory
// with no telemetry/events (and one with no telemetry directory) creates
// nothing and sends nothing, and that events another process sent and deleted
// between the first look and the lock are not sent. It holds for the
// automatic attempt and for the command entry.
func TestAttemptLooksThenLocksThenListsAgain(t *testing.T) {
	entries := map[string]func(t *testing.T, in Input){
		"the automatic attempt": func(t *testing.T, in Input) { t.Helper(); Send(t.Context(), in) },
		"the command":           func(t *testing.T, in Input) { t.Helper(); SendNow(t.Context(), in) },
	}
	for name, send := range entries {
		t.Run(name, func(t *testing.T) { lookThenLock(t, send) })
	}
}

func lookThenLock(t *testing.T, send func(t *testing.T, in Input)) {
	t.Helper()
	srv := newTestServer(t, nil)
	for name, dir := range map[string]string{"no events": uploadDir(t), "no telemetry": t.TempDir()} {
		t.Run(name, func(t *testing.T) {
			before := testfs.Snapshot(t, dir)
			send(t, input(dir, srv.URL))
			assert.Empty(t, srv.seenRequests())
			assert.Equal(t, before, testfs.Snapshot(t, dir))
		})
	}

	t.Run("a file sent and deleted between the look and the lock", func(t *testing.T) {
		dir := uploadDir(t)
		paths := putSlotRange(t, dir, hourAgo(3), 1, 3)
		t.Cleanup(func() { afterLook = nil })
		afterLook = func() {
			require.NoError(t, os.Remove(filepath.Join(dir, "telemetry", filepath.FromSlash(paths[1]))))
		}
		srv := newTestServer(t, nil)
		send(t, input(dir, srv.URL))
		reqs := srv.seenRequests()
		require.Len(t, reqs, 1)
		assert.Equal(t, []string{uidOf(1), uidOf(3)}, reqs[0].ids(t), "the second list is sent")

		again := uploadDir(t)
		only := putSlotRange(t, again, hourAgo(3), 1, 1)
		afterLook = func() { require.NoError(t, os.Remove(filepath.Join(again, "telemetry", filepath.FromSlash(only[0])))) }
		none := newTestServer(t, nil)
		send(t, input(again, none.URL))
		assert.Empty(t, none.seenRequests(), "nothing is left to send after the lock")
	})
}

// TestSenderAndPassInterleave asserts that when the hourly pass compacts an
// hour between two attempts, and leaves a slot of it behind, no request holds a
// record id twice and every event is sent over the two attempts.
func TestSenderAndPassInterleave(t *testing.T) {
	dir := uploadDir(t)
	hour := hourAgo(3)
	slots := putSlotRange(t, dir, hour, 1, 4)
	srv := newTestServer(t, status(503))
	Send(t.Context(), input(dir, srv.URL))
	require.Len(t, srv.seenRequests(), 1)

	events := []map[string]any{eventFields(t, 1, hour), eventFields(t, 2, hour), eventFields(t, 3, hour), eventFields(t, 4, hour)}
	file := putCompactedFile(t, dir, hour, hourDir(hour)+".jsonl", time.Now().Add(-2*time.Minute), events...)
	require.NoError(t, os.Remove(filepath.Join(dir, "telemetry", filepath.FromSlash(slots[0]))))
	require.NoError(t, os.Remove(filepath.Join(dir, "telemetry", filepath.FromSlash(slots[1]))))
	st, _ := attemptState(t, dir)
	st.NextAttempt = time.Now().Add(-time.Second)
	writeState(t, dir, st)

	ok := newTestServer(t, nil)
	Send(t.Context(), input(dir, ok.URL))
	reqs := ok.seenRequests()
	require.Len(t, reqs, 1)
	ids := reqs[0].ids(t)
	assert.Equal(t, []string{uidOf(1), uidOf(2), uidOf(3), uidOf(4)}, ids, "each id once")
	assert.False(t, exists(dir, file))
	assert.Empty(t, filesWaiting(dir, slots))
}

// deleteRecorded runs the steps `telemetry off` takes on the stored data: the
// events and the upload state go.
func deleteRecorded(t *testing.T, dir string) {
	t.Helper()
	s, err := telemetry.OpenStore(dir)
	require.NoError(t, err)
	require.NoError(t, s.DeleteEvents())
	require.NoError(t, s.DeleteUploadState())
	require.NoError(t, s.Close())
}

// TestUploadAfterOffWritesNothing asserts that `telemetry off` during a
// request (the events and the upload state deleted, the key set to false)
// leaves no state file and no telemetry/upload, whether the controls read as
// off or, in a race, still read as on; that recording turned off before the
// requests keeps the files and leaves no request; and that `telemetry off`
// between the first look and the lock, with the events still there or already
// deleted, leaves no request and no telemetry/upload that the lock created,
// including when the directory held a state file at the first look and `off`
// removed it before the lock.
func TestUploadAfterOffWritesNothing(t *testing.T) {
	for name, stillOn := range map[string]bool{"controls off": false, "controls still on": true} {
		t.Run(name, func(t *testing.T) {
			dir := uploadDir(t)
			putSlotRange(t, dir, hourAgo(3), 1, 2)
			var off atomic.Bool
			srv := newTestServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				deleteRecorded(t, dir)
				off.Store(!stillOn)
				_, _ = w.Write([]byte("{}"))
			})
			in := input(dir, srv.URL)
			in.RecordingOn = func() bool { return !off.Load() }
			Send(t.Context(), in)
			require.Len(t, srv.seenRequests(), 1)
			assert.Empty(t, uploadFiles(t, dir))
			_, err := os.Lstat(filepath.Join(dir, "telemetry", "upload"))
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}

	t.Run("off before the deletion keeps the files", func(t *testing.T) {
		dir := uploadDir(t)
		paths := putSlotRange(t, dir, hourAgo(3), 1, 2)
		var off atomic.Bool
		srv := newTestServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
			off.Store(true)
			_, _ = w.Write([]byte("{}"))
		})
		in := input(dir, srv.URL)
		in.RecordingOn = func() bool { return !off.Load() }
		Send(t.Context(), in)
		require.Len(t, srv.seenRequests(), 1)
		assert.Equal(t, paths, filesWaiting(dir, paths), "no deletion")
		assert.NotContains(t, uploadFiles(t, dir), "state.json", "no state write")
		assert.NotContains(t, uploadFiles(t, dir), "lock", "the lock is released")
	})

	for name, offSteps := range map[string]func(t *testing.T, dir string){
		"off with the events still there": func(*testing.T, string) {},
		"off with the events deleted":     deleteRecorded,
	} {
		t.Run("between the look and the lock, "+name, func(t *testing.T) {
			dir := uploadDir(t)
			paths := putSlotRange(t, dir, hourAgo(3), 1, 2)
			var off atomic.Bool
			t.Cleanup(func() { afterLook = nil })
			afterLook = func() {
				off.Store(true)
				offSteps(t, dir)
			}
			srv := newTestServer(t, nil)
			in := input(dir, srv.URL)
			in.RecordingOn = func() bool { return !off.Load() }
			Send(t.Context(), in)
			assert.Empty(t, srv.seenRequests(), "recording was off when the lock was taken")
			_, err := os.Lstat(filepath.Join(dir, "telemetry", "upload"))
			require.ErrorIs(t, err, os.ErrNotExist, "the directory the lock created is removed")
			if name == "off with the events still there" {
				assert.Equal(t, paths, filesWaiting(dir, paths), "no deletion")
			}
		})
	}

	t.Run("between the look and the lock, off with the events deleted and the directory there at the look", func(t *testing.T) {
		dir := uploadDir(t)
		putSlotRange(t, dir, hourAgo(3), 1, 2)
		writeState(t, dir, telemetry.UploadState{LastAttempt: time.Now().Add(-2 * time.Hour), HTTPStatus: 503, NextAttempt: time.Now().Add(-time.Hour)})
		var off atomic.Bool
		t.Cleanup(func() { afterLook = nil })
		afterLook = func() {
			off.Store(true)
			deleteRecorded(t, dir)
		}
		srv := newTestServer(t, nil)
		in := input(dir, srv.URL)
		in.RecordingOn = func() bool { return !off.Load() }
		Send(t.Context(), in)
		assert.Empty(t, srv.seenRequests(), "recording was off when the lock was taken")
		_, err := os.Lstat(filepath.Join(dir, "telemetry", "upload"))
		assert.ErrorIs(t, err, os.ErrNotExist, "no state file is left, so the directory holds only the lock")
	})

	t.Run("off at the lock leaves a directory that was there", func(t *testing.T) {
		dir := uploadDir(t)
		putSlotRange(t, dir, hourAgo(3), 1, 2)
		writeState(t, dir, telemetry.UploadState{LastAttempt: time.Now().Add(-2 * time.Hour), HTTPStatus: 503, NextAttempt: time.Now().Add(-time.Hour)})
		srv := newTestServer(t, nil)
		in := input(dir, srv.URL)
		in.RecordingOn = func() bool { return false }
		Send(t.Context(), in)
		assert.Empty(t, srv.seenRequests())
		assert.Equal(t, []string{"state.json"}, uploadFiles(t, dir), "only the lock is gone")
	})
}

// TestUploadSendsNoCredential asserts no request has an Authorization or
// Cookie header, the CLI's User-Agent is on each, and a token in the
// environment appears in no header.
func TestUploadSendsNoCredential(t *testing.T) {
	t.Setenv("RANCHER_CLI_TOKEN", "token-marker-123")
	dir := uploadDir(t)
	putSlotRange(t, dir, hourAgo(3), 1, 2)
	srv := newTestServer(t, nil)
	Send(t.Context(), input(dir, srv.URL))
	reqs := srv.seenRequests()
	require.Len(t, reqs, 1)
	h := reqs[0].header
	assert.Empty(t, h.Values("Authorization"))
	assert.Empty(t, h.Values("Cookie"))
	assert.Equal(t, httpx.UserAgent(), h.Get("User-Agent"))
	for name, values := range h {
		for _, v := range values {
			assert.NotContains(t, v, "token-marker-123", name)
		}
	}
}

// TestSendRefusesWhatItMust asserts a release version, an origin the check
// refuses or changes, a nil encoder, a nil controls function, a done context
// and less than the floor left each send nothing and create nothing.
func TestSendRefusesWhatItMust(t *testing.T) {
	srv := newTestServer(t, nil)
	cases := map[string]func(in *Input){
		"release 1.2.3":              func(in *Input) { in.Version = "1.2.3" },
		"release v0.1.0":             func(in *Input) { in.Version = "v0.1.0" },
		"release 0.1.0-rc.1":         func(in *Input) { in.Version = "0.1.0-rc.1" },
		"unknown":                    func(in *Input) { in.Version = "unknown" },
		"_OTHER":                     func(in *Input) { in.Version = "_OTHER" },
		"empty":                      func(in *Input) { in.Version = "" },
		"a non-loopback http origin": func(in *Input) { in.Origin = "http://h.example" },
		"an origin with a path":      func(in *Input) { in.Origin = "https://h.example/v1/logs" },
		"an origin with a slash":     func(in *Input) { in.Origin = srv.URL + "/" },
		"an upper case origin":       func(in *Input) { in.Origin = strings.ToUpper(srv.URL) },
		"an origin with a user":      func(in *Input) { in.Origin = "http://user:pass@" + strings.TrimPrefix(srv.URL, "http://") },
		"no encoder":                 func(in *Input) { in.Encoder = nil },
		"no controls function":       func(in *Input) { in.RecordingOn = nil },
		"under the floor":            func(in *Input) { in.Deadline = time.Now().Add(100 * time.Millisecond) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dir := uploadDir(t)
			putSlotRange(t, dir, hourAgo(3), 1, 2)
			in := input(dir, srv.URL)
			mutate(&in)
			before := testfs.Snapshot(t, dir)
			Send(t.Context(), in)
			assert.Empty(t, srv.seenRequests())
			assert.Equal(t, before, testfs.Snapshot(t, dir))
		})
	}

	t.Run("a done context", func(t *testing.T) {
		dir := uploadDir(t)
		putSlotRange(t, dir, hourAgo(3), 1, 2)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		before := testfs.Snapshot(t, dir)
		Send(ctx, input(dir, srv.URL))
		assert.Empty(t, srv.seenRequests())
		assert.Equal(t, before, testfs.Snapshot(t, dir))
	})
}

// panicEncoder panics when asked to encode.
type panicEncoder struct{}

func (panicEncoder) Encode([]telemetry.StoredEvent) (Request, error) { panic("encoder failure") }
func (panicEncoder) RefusedCount([]byte) (int, bool)                 { return 0, false }

// TestSendRecoversAPanic asserts a panic below the entry does not reach the
// caller and leaves no lock behind.
func TestSendRecoversAPanic(t *testing.T) {
	dir := uploadDir(t)
	putSlotRange(t, dir, hourAgo(3), 1, 2)
	srv := newTestServer(t, nil)
	in := input(dir, srv.URL)
	in.Encoder = panicEncoder{}
	assert.NotPanics(t, func() { Send(t.Context(), in) })
	assert.Empty(t, srv.seenRequests())
	assert.NotContains(t, uploadFiles(t, dir), "lock")
}

// failingWriter fails every write.
type failingWriter struct{ writes int }

func (w *failingWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, errors.New("closed pipe")
}

// TestSendWritesDebugLines asserts the lines before and after each request,
// for an answer with a count, an answer with none, no answer and a stored
// version that is not three numbers; that they hold the canonical origin and
// no path, no stored value and no record id; that a writer that fails on every
// write changes nothing; and that no writer prints nothing.
func TestSendWritesDebugLines(t *testing.T) {
	build := func(t *testing.T, version string) string {
		t.Helper()
		dir := uploadDir(t)
		hour := hourAgo(3)
		for i := range 3 {
			f := eventFields(t, i+1, hour)
			f["schema_version"] = version
			putSlot(t, dir, hour, i+1, f)
		}
		return dir
	}
	for name, tc := range map[string]struct {
		answer  respond
		version string
		want    string
	}{
		"a count": {answerWith(200, `{"partialSuccess":{"rejectedLogRecords":"2"}}`), "1.0.0",
			"debug: telemetry upload: sending 3 events (contract 1.0.0) to ORIGIN\ndebug: telemetry upload: answer HTTP 200, 2 refused\n"},
		"no count": {answerWith(200, "not json"), "1.0.0",
			"debug: telemetry upload: sending 3 events (contract 1.0.0) to ORIGIN\ndebug: telemetry upload: answer HTTP 200, refused count unknown\n"},
		"a 503": {status(503), "1.0.0",
			"debug: telemetry upload: sending 3 events (contract 1.0.0) to ORIGIN\ndebug: telemetry upload: answer HTTP 503, refused count unknown\n"},
		"a version with a control character": {status(400), "1.0.0\x1b[31m",
			"debug: telemetry upload: sending 3 events (contract unrecognized) to ORIGIN\ndebug: telemetry upload: answer HTTP 400, refused count unknown\n"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := build(t, tc.version)
			srv := newTestServer(t, tc.answer)
			var out bytes.Buffer
			in := input(dir, srv.URL)
			in.Debug = &out
			Send(t.Context(), in)
			assert.Equal(t, strings.ReplaceAll(tc.want, "ORIGIN", srv.URL), out.String())
			assert.NotContains(t, out.String(), "/v1/logs")
			assert.NotContains(t, out.String(), uidOf(1))
		})
	}

	t.Run("no answer", func(t *testing.T) {
		dir := build(t, "1.0.0")
		clock := stopClock(t)
		in := input(dir, "")
		srv := newTestServer(t, func(_ int, _ http.ResponseWriter, r *http.Request) {
			clock.pass(in)
			testtransport.HangAndAbort(r)
		})
		var out bytes.Buffer
		in.Origin = srv.URL
		in.Debug = &out
		Send(clock.context(), in)
		assert.Equal(t, "debug: telemetry upload: sending 3 events (contract 1.0.0) to "+srv.URL+"\ndebug: telemetry upload: no answer: timeout\n", out.String())
	})

	t.Run("a writer that fails changes nothing", func(t *testing.T) {
		dirA, dirB := build(t, "1.0.0"), build(t, "1.0.0")
		srv := newTestServer(t, nil)
		w := &failingWriter{}
		in := input(dirA, srv.URL)
		in.Debug = w
		Send(t.Context(), in)
		Send(t.Context(), input(dirB, srv.URL))
		assert.Equal(t, 2, w.writes, "both lines were tried")
		a, _ := attemptState(t, dirA)
		b, _ := attemptState(t, dirB)
		assert.Equal(t, a.Sent, b.Sent)
		assert.Empty(t, readyIDs(t, dirA))
	})

	t.Run("no writer prints nothing", func(t *testing.T) {
		dir := build(t, "1.0.0")
		srv := newTestServer(t, nil)
		assert.NotPanics(t, func() { Send(t.Context(), input(dir, srv.URL)) })
		assert.Len(t, srv.seenRequests(), 1)
	})
}
