package telemetrysend

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/telemetry"
	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/rancher/rancher-cli/internal/testtransport"
)

// skipIfHourChanged skips the test when the current hour is not hour: an event
// seeded in the current hour then belongs to a finished hour.
func skipIfHourChanged(t *testing.T, hour time.Time) {
	t.Helper()
	if !hourAgo(0).Equal(hour) {
		t.Skip("the hour changed while the test ran")
	}
}

// failEncoder fails to encode.
type failEncoder struct{}

func (failEncoder) Encode([]telemetry.StoredEvent) (Request, error) {
	return Request{}, errors.New("encoder failure")
}
func (failEncoder) RefusedCount([]byte) (int, bool) { return 0, false }

// lateEncoder reads each answer, then lets the time run out.
type lateEncoder struct {
	Encoder
	pass func()
}

func (e lateEncoder) RefusedCount(answer []byte) (int, bool) {
	e.pass()
	return e.Encoder.RefusedCount(answer)
}

// putTwoBatches writes two events of different contract versions into one
// finished hour. The sender cuts a version into its own request: the two
// events are two requests, with a store as small as a store can be.
func putTwoBatches(t *testing.T, dir string) []string {
	t.Helper()
	hour := hourAgo(3)
	var paths []string
	for i, version := range []string{"1.0.0", "1.0.1"} {
		fields := eventFields(t, i+1, hour)
		fields["schema_version"] = version
		paths = append(paths, putSlot(t, dir, hour, i+1, fields))
	}
	return paths
}

// requireNoLock asserts the lock of dir is gone.
func requireNoLock(t *testing.T, dir string) {
	t.Helper()
	assert.NotContains(t, uploadFiles(t, dir), "lock")
}

// TestUploadCommandSendsTheCurrentHour asserts that the command entry sends an
// event of the current hour and deletes its slot after the 2xx while Send leaves
// it, and that a covered file of the current hour whose removal fails keeps its
// id in the state and is deleted by the next run with no request.
func TestUploadCommandSendsTheCurrentHour(t *testing.T) {
	hour := hourAgo(0)

	t.Run("a slot of the current hour", func(t *testing.T) {
		dir := uploadDir(t)
		slot := putSlot(t, dir, hour, 1, eventFields(t, 1, hour))
		srv := newTestServer(t, nil)
		Send(t.Context(), input(dir, srv.URL))
		assert.Empty(t, srv.seenRequests(), "the automatic attempt leaves the current hour")
		assert.True(t, exists(dir, slot))

		res := SendNow(t.Context(), input(dir, srv.URL))
		skipIfHourChanged(t, hour)
		reqs := srv.seenRequests()
		require.Len(t, reqs, 1)
		assert.Equal(t, []string{uidOf(1)}, reqs[0].ids(t))
		assert.False(t, exists(dir, slot))
		assert.Equal(t, 1, res.Sent)
	})

	t.Run("a covered file that cannot be removed", func(t *testing.T) {
		dir := uploadDir(t)
		file := putCompactedRange(t, dir, hour, 1, 2)
		hourPath := filepath.Join(dir, "telemetry", "events", hourDir(hour))
		srv := newTestServer(t, nil)
		t.Run("the removal fails", func(t *testing.T) {
			if testfs.OnWindows() {
				f, err := os.Open(filepath.Join(dir, "telemetry", filepath.FromSlash(file)))
				require.NoError(t, err)
				t.Cleanup(func() { _ = f.Close() })
			} else {
				testfs.RestrictAccess(t, hourPath, 0o500)
			}
			SendNow(t.Context(), input(dir, srv.URL))
			skipIfHourChanged(t, hour)
			require.Len(t, srv.seenRequests(), 1)
			assert.True(t, exists(dir, file), "the removal failed")
			st, ok := attemptState(t, dir)
			require.True(t, ok)
			assert.Equal(t, []string{uidOf(1), uidOf(2)}, st.CoveredIDs, "the ids of the files still stored stay in the state")
		})

		res := SendNow(t.Context(), input(dir, srv.URL))
		skipIfHourChanged(t, hour)
		assert.Len(t, srv.seenRequests(), 1, "the covered events are not sent again")
		assert.False(t, exists(dir, file))
		assert.Equal(t, telemetry.SkippedNothingWaiting, res.Skipped)
		after, ok := attemptState(t, dir)
		require.True(t, ok)
		assert.Empty(t, after.CoveredIDs)
	})
}

// TestUploadCommandIgnoresTheStoredWait asserts that the command entry sends
// with a next attempt an hour or two ahead in the state file, that it writes the
// state of its own attempt, and that Send leaves the same events.
func TestUploadCommandIgnoresTheStoredWait(t *testing.T) {
	for name, wait := range map[string]time.Duration{"an hour": time.Hour, "two hours and failures": 2 * time.Hour} {
		t.Run(name, func(t *testing.T) {
			dir := uploadDir(t)
			putSlotRange(t, dir, hourAgo(3), 1, 2)
			writeState(t, dir, telemetry.UploadState{LastAttempt: time.Now(), HTTPStatus: 429, Failures: 3, NextAttempt: time.Now().Add(wait)})
			srv := newTestServer(t, nil)
			Send(t.Context(), input(dir, srv.URL))
			assert.Empty(t, srv.seenRequests(), "the automatic attempt waits")

			start := time.Now()
			res := SendNow(t.Context(), input(dir, srv.URL))
			require.Len(t, srv.seenRequests(), 1)
			assert.Equal(t, 2, res.Sent)
			st, ok := attemptState(t, dir)
			require.True(t, ok)
			assert.Zero(t, st.Failures)
			assert.Equal(t, 200, st.HTTPStatus)
			assert.GreaterOrEqual(t, st.NextAttempt.Sub(start), time.Hour)
			assert.Less(t, st.NextAttempt.Sub(start), time.Hour+maxJitter+time.Second)
			assert.True(t, res.NextAttempt.Equal(st.NextAttempt), "the result carries the stored next attempt")
		})
	}
}

// TestUploadCommandResultValues asserts the values of the result for each
// answer: a 2xx with and without a refused count, a 400, a 503, no answer, a 2xx
// followed by a 503, a 400, no answer and the time limit, and nothing waiting.
func TestUploadCommandResultValues(t *testing.T) {
	refusals := answerWith(202, `{"partialSuccess":{"rejectedLogRecords":"1"}}`)
	then := func(second respond) respond {
		return func(call int, w http.ResponseWriter, r *http.Request) {
			if call == 0 {
				_, _ = w.Write([]byte("{}"))
				return
			}
			second(call, w, r)
		}
	}
	const twoBatches = -2
	type want struct {
		sent, dropped, requests, status, accepted int
		refused                                   *int
		noAnswer, outcome                         string
		limit                                     bool
		failures                                  int
	}
	one := 1
	zero := 0
	rows := []struct {
		name   string
		events int
		answer respond
		hang   bool
		want   want
	}{
		{"a 2xx with a count", 3, refusals, false, want{sent: 3, requests: 1, status: 202, accepted: 202, refused: &one, outcome: telemetry.OutcomeAccepted}},
		{"a 2xx with no count", 3, answerWith(200, "<html>"), false, want{sent: 3, requests: 1, status: 200, accepted: 200, outcome: telemetry.OutcomeAccepted}},
		{"a 400", 3, status(400), false, want{dropped: 3, requests: 1, status: 400, outcome: telemetry.OutcomeDeleted}},
		{"a 503", 3, status(503), false, want{requests: 1, status: 503, failures: 1, outcome: telemetry.OutcomeKept}},
		{"no answer", 3, nil, true, want{requests: 1, noAnswer: "timeout", failures: 1, outcome: telemetry.OutcomeNoAnswer}},
		{"a 2xx then a 503", twoBatches, then(status(503)), false, want{sent: 1, requests: 2, status: 503, accepted: 200, refused: &zero, failures: 1, outcome: telemetry.OutcomeKept}},
		{"a 2xx then a 400", twoBatches, then(status(400)), false, want{sent: 1, dropped: 1, requests: 2, status: 400, accepted: 200, refused: &zero, outcome: telemetry.OutcomeDeleted}},
		{"a 400 then a 503", twoBatches, func(call int, w http.ResponseWriter, _ *http.Request) {
			if call == 0 {
				w.WriteHeader(400)
				return
			}
			w.WriteHeader(503)
		}, false, want{dropped: 1, requests: 2, status: 503, failures: 1, outcome: telemetry.OutcomeKept}},
		{"a 2xx then no answer", twoBatches, then(nil), true, want{sent: 1, requests: 2, noAnswer: "timeout", accepted: 200, refused: &zero, failures: 1, outcome: telemetry.OutcomeNoAnswer}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			dir := uploadDir(t)
			if row.events == twoBatches {
				putTwoBatches(t, dir)
			} else {
				putSlotRange(t, dir, hourAgo(3), 1, row.events)
			}
			answer := row.answer
			clock := stopClock(t)
			var in Input
			if row.hang {
				base := answer
				answer = func(call int, w http.ResponseWriter, r *http.Request) {
					if base != nil && call == 0 {
						_, _ = w.Write([]byte("{}"))
						return
					}
					clock.pass(in)
					testtransport.HangAndAbort(r)
				}
			}
			srv := newTestServer(t, answer)
			in = input(dir, srv.URL)
			res := SendNow(clock.context(), in)
			assert.Equal(t, row.want.sent, res.Sent)
			assert.Equal(t, row.want.dropped, res.Dropped)
			assert.Equal(t, row.want.requests, res.Requests)
			assert.Equal(t, row.want.status, res.HTTPStatus)
			assert.Equal(t, row.want.accepted, res.AcceptedStatus)
			assert.Equal(t, row.want.refused, res.Refused)
			assert.Equal(t, row.want.noAnswer, res.NoAnswer)
			assert.Equal(t, row.want.outcome, res.LastOutcome)
			assert.Equal(t, row.want.limit, res.TimeLimitReached)
			assert.Empty(t, res.Skipped)
			assert.Empty(t, res.Failure)
			st, ok := attemptState(t, dir)
			require.True(t, ok)
			assert.Equal(t, row.want.failures, st.Failures)
			assert.True(t, res.NextAttempt.Equal(st.NextAttempt), "the result carries the stored next attempt")
			requireNoLock(t, dir)
		})
	}

	t.Run("the time limit after the first request", func(t *testing.T) {
		dir := uploadDir(t)
		putTwoBatches(t, dir)
		srv := newTestServer(t, nil)
		clock := stopClock(t)
		in := input(dir, srv.URL)
		in.Encoder = lateEncoder{Encoder: NewOTLPEncoder(), pass: func() { clock.pass(in) }}
		res := SendNow(t.Context(), in)
		assert.Len(t, srv.seenRequests(), 1)
		assert.Equal(t, 1, res.Sent)
		assert.Equal(t, 1, res.Requests)
		assert.True(t, res.TimeLimitReached)
		assert.Empty(t, res.Skipped)
		assert.Empty(t, res.Failure)
		assert.Len(t, readyIDs(t, dir), 2, "the deadline passed before the deletion: every file stays")
	})

	t.Run("nothing waiting", func(t *testing.T) {
		dir := uploadDir(t)
		srv := newTestServer(t, nil)
		res := SendNow(t.Context(), input(dir, srv.URL))
		assert.Equal(t, telemetry.UploadResult{Skipped: telemetry.SkippedNothingWaiting}, res)
		assert.Empty(t, srv.seenRequests())
	})
}

// TestUploadCommandWithNoConfigDirectory asserts that a missing configuration
// directory, a configuration directory with no events and no telemetry/upload,
// and a telemetry/upload with nothing to send each report nothing waiting,
// make no request and create nothing; and that the last one writes the state of
// the attempt while the others leave a next attempt of null.
func TestUploadCommandWithNoConfigDirectory(t *testing.T) {
	srv := newTestServer(t, nil)

	t.Run("a missing configuration directory", func(t *testing.T) {
		parent := t.TempDir()
		in := input(filepath.Join(parent, "absent"), srv.URL)
		res := SendNow(t.Context(), in)
		assert.Equal(t, telemetry.UploadResult{Skipped: telemetry.SkippedNothingWaiting}, res)
		assert.Equal(t, []string{}, dirNames(t, parent))
	})

	t.Run("no events and no upload directory", func(t *testing.T) {
		dir := uploadDir(t)
		before := testfs.Snapshot(t, dir)
		res := SendNow(t.Context(), input(dir, srv.URL))
		assert.Equal(t, telemetry.UploadResult{Skipped: telemetry.SkippedNothingWaiting}, res)
		assert.Equal(t, before, testfs.Snapshot(t, dir))
	})

	t.Run("an upload directory and nothing to send", func(t *testing.T) {
		dir := uploadDir(t)
		writeState(t, dir, telemetry.UploadState{LastAttempt: time.Now().Add(-3 * time.Hour), HTTPStatus: 503, Failures: 2, NextAttempt: time.Now().Add(-2 * time.Hour)})
		start := time.Now()
		res := SendNow(t.Context(), input(dir, srv.URL))
		assert.Equal(t, telemetry.SkippedNothingWaiting, res.Skipped)
		st, ok := attemptState(t, dir)
		require.True(t, ok)
		assert.Zero(t, st.Failures)
		assert.GreaterOrEqual(t, st.NextAttempt.Sub(start), time.Hour)
		assert.Less(t, st.NextAttempt.Sub(start), time.Hour+maxJitter+time.Second)
		assert.True(t, res.NextAttempt.Equal(st.NextAttempt))
		requireNoLock(t, dir)
	})
	assert.Empty(t, srv.seenRequests())
}

// TestUploadCommandSharesTheLock asserts the result for each state of the upload
// lock and of telemetry/upload: a held lock, a stale lock that is taken over, an
// unreadable lock, a plain file or a symlink in place of the directory and a lock
// that cannot be created. Each of the last four is a failure that sends nothing,
// and none reads as nothing waiting.
func TestUploadCommandSharesTheLock(t *testing.T) {
	seed := func(t *testing.T) (dir string, srv *testServer) {
		t.Helper()
		dir = uploadDir(t)
		putSlotRange(t, dir, hourAgo(3), 1, 2)
		return dir, newTestServer(t, nil)
	}

	t.Run("a live lock", func(t *testing.T) {
		dir, srv := seed(t)
		other, err := telemetry.OpenStore(dir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = other.Close() })
		lock, err := other.TakeUploadLock()
		require.NoError(t, err)
		t.Cleanup(func() { _ = lock.Release() })
		res := SendNow(t.Context(), input(dir, srv.URL))
		assert.Equal(t, telemetry.SkippedLockHeld, res.Skipped)
		assert.Empty(t, res.Failure)
		assert.Empty(t, srv.seenRequests())
	})

	t.Run("a stale lock", func(t *testing.T) {
		dir, srv := seed(t)
		lock := filepath.Join(dir, "telemetry", "upload", "lock")
		require.NoError(t, os.MkdirAll(filepath.Dir(lock), 0o700))
		stale := time.Now().Add(-2*time.Minute).UTC().Format(time.RFC3339Nano) + " token"
		require.NoError(t, os.WriteFile(lock, []byte(stale), 0o600))
		res := SendNow(t.Context(), input(dir, srv.URL))
		assert.Empty(t, res.Skipped)
		assert.Equal(t, 2, res.Sent)
		requireNoLock(t, dir)
	})

	t.Run("an unreadable lock", func(t *testing.T) {
		dir, srv := seed(t)
		lock := filepath.Join(dir, "telemetry", "upload", "lock")
		testfs.DirectoryInPlaceOfFile(t, lock)
		start := time.Now()
		res := SendNow(t.Context(), input(dir, srv.URL))
		assert.Equal(t, telemetry.FailureLockUnreadable, res.Failure)
		assert.Equal(t, lock, res.FailurePath)
		assert.Empty(t, res.Skipped)
		assert.Empty(t, srv.seenRequests())
		st, ok := attemptState(t, dir)
		require.True(t, ok, "the backoff state is written")
		assert.Equal(t, "lock unavailable", st.NoAnswer)
		assert.GreaterOrEqual(t, st.NextAttempt.Sub(start), time.Hour)
	})

	t.Run("an unreadable lock with a wait stored", func(t *testing.T) {
		dir, srv := seed(t)
		writeState(t, dir, telemetry.UploadState{LastAttempt: time.Now(), HTTPStatus: 429, Failures: 1, NextAttempt: time.Now().Add(2 * time.Hour)})
		before, _ := attemptState(t, dir)
		testfs.DirectoryInPlaceOfFile(t, filepath.Join(dir, "telemetry", "upload", "lock"))
		res := SendNow(t.Context(), input(dir, srv.URL))
		assert.Equal(t, telemetry.FailureLockUnreadable, res.Failure)
		after, ok := attemptState(t, dir)
		require.True(t, ok)
		assert.Equal(t, before, after, "a lock error is not an answer: the stored wait stays")
	})

	t.Run("a plain file in place of the upload directory", func(t *testing.T) {
		dir, srv := seed(t)
		upload := filepath.Join(dir, "telemetry", "upload")
		require.NoError(t, os.WriteFile(upload, []byte("x"), 0o600))
		res := SendNow(t.Context(), input(dir, srv.URL))
		assert.Equal(t, telemetry.UploadResult{Failure: telemetry.FailureUploadDirectoryUnsafe, FailurePath: upload}, res)
		assert.Empty(t, srv.seenRequests())
		info, err := os.Lstat(upload)
		require.NoError(t, err)
		assert.True(t, info.Mode().IsRegular(), "nothing replaced it")
	})

	t.Run("a symlink in place of the upload directory", func(t *testing.T) {
		dir, srv := seed(t)
		upload := filepath.Join(dir, "telemetry", "upload")
		testfs.Symlink(t, t.TempDir(), upload)
		res := SendNow(t.Context(), input(dir, srv.URL))
		assert.Equal(t, telemetry.UploadResult{Failure: telemetry.FailureUploadDirectoryUnsafe, FailurePath: upload}, res)
		assert.Empty(t, srv.seenRequests())
	})

	for name, tc := range map[string]struct {
		take error
		want string
	}{
		"a link the file system refuses":      {&os.LinkError{Op: "link", Old: "a", New: "b", Err: errors.ErrUnsupported}, telemetry.FailureLockUnavailable},
		"a link answered with EPERM":          {&os.LinkError{Op: "link", Old: "a", New: "b", Err: syscall.EPERM}, telemetry.FailureLockUnavailable},
		"a link answered with EACCES":         {&os.LinkError{Op: "link", Old: "a", New: "b", Err: syscall.EACCES}, telemetry.FailureLockPermissionDenied},
		"a link answered with a full disk":    {&os.LinkError{Op: "link", Old: "a", New: "b", Err: syscall.ENOSPC}, telemetry.FailureLockCreateFailed},
		"a link answered with too many links": {&os.LinkError{Op: "link", Old: "a", New: "b", Err: syscall.EMLINK}, telemetry.FailureLockCreateFailed},
		"a permission error":                  {fs.ErrPermission, telemetry.FailureLockPermissionDenied},
		"another error":                       {errors.New("no space left on device"), telemetry.FailureLockCreateFailed},
	} {
		t.Run("a lock that cannot be created: "+name, func(t *testing.T) {
			dir, srv := seed(t)
			t.Cleanup(func() { afterTake = nil })
			afterTake = func(err error) error {
				if err != nil {
					return err
				}
				return tc.take
			}
			res := SendNow(t.Context(), input(dir, srv.URL))
			assert.Equal(t, tc.want, res.Failure)
			assert.Equal(t, filepath.Join(dir, "telemetry", "upload", "lock"), res.FailurePath)
			assert.Empty(t, res.Skipped)
			assert.Empty(t, srv.seenRequests())
			requireNoLock(t, dir)
			assert.Len(t, readyIDs(t, dir), 2, "no file was deleted")
		})
	}

	t.Run("a lock that cannot be created: an upload directory this user cannot write", func(t *testing.T) {
		dir, srv := seed(t)
		upload := filepath.Join(dir, "telemetry", "upload")
		require.NoError(t, os.MkdirAll(upload, 0o700))
		testfs.RestrictAccess(t, upload, 0o500)
		res := SendNow(t.Context(), input(dir, srv.URL))
		assert.Equal(t, telemetry.FailureLockPermissionDenied, res.Failure)
		assert.Empty(t, srv.seenRequests())
	})
}

// TestUploadCommandReportsFailures asserts the result for each failure inside
// an allowed attempt, one row per failure value: none reads as nothing waiting,
// none sends a request that the failure came before, and a cancelled context
// stops the requests without deleting a file that no 2xx covered.
func TestUploadCommandReportsFailures(t *testing.T) {
	t.Run("a telemetry path that is a plain file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "telemetry")
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
		srv := newTestServer(t, nil)
		res := SendNow(t.Context(), input(dir, srv.URL))
		assert.Equal(t, telemetry.UploadResult{Failure: telemetry.FailureStoreUnavailable, FailurePath: path}, res)
		assert.Empty(t, srv.seenRequests())
	})

	t.Run("a telemetry path that is a symlink", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "telemetry")
		testfs.Symlink(t, t.TempDir(), path)
		srv := newTestServer(t, nil)
		res := SendNow(t.Context(), input(dir, srv.URL))
		assert.Equal(t, telemetry.UploadResult{Failure: telemetry.FailureStoreUnavailable, FailurePath: path}, res)
		assert.Empty(t, srv.seenRequests())
	})

	t.Run("a walk that fails", func(t *testing.T) {
		dir := uploadDir(t)
		putSlotRange(t, dir, hourAgo(3), 1, 2)
		testfs.RestrictAccess(t, filepath.Join(dir, "telemetry", "events"), 0)
		srv := newTestServer(t, nil)
		res := SendNow(t.Context(), input(dir, srv.URL))
		assert.Equal(t, telemetry.UploadResult{Failure: telemetry.FailureReadFailed}, res)
		assert.Empty(t, srv.seenRequests())
	})

	t.Run("an encoder that fails", func(t *testing.T) {
		dir := uploadDir(t)
		paths := putSlotRange(t, dir, hourAgo(3), 1, 2)
		srv := newTestServer(t, nil)
		in := input(dir, srv.URL)
		in.Encoder = failEncoder{}
		res := SendNow(t.Context(), in)
		assert.Equal(t, telemetry.FailureRequestFailed, res.Failure)
		assert.Empty(t, res.Skipped)
		assert.Zero(t, res.Requests)
		assert.Empty(t, srv.seenRequests())
		assert.Len(t, filesWaiting(dir, paths), 2)
		requireNoLock(t, dir)
	})

	t.Run("a panic", func(t *testing.T) {
		dir := uploadDir(t)
		putSlotRange(t, dir, hourAgo(3), 1, 2)
		srv := newTestServer(t, nil)
		in := input(dir, srv.URL)
		in.Encoder = panicEncoder{}
		var res telemetry.UploadResult
		require.NotPanics(t, func() { res = SendNow(t.Context(), in) })
		assert.Equal(t, telemetry.UploadResult{Failure: telemetry.FailureSenderFailed}, res)
		requireNoLock(t, dir)
	})

	t.Run("a panic after a request", func(t *testing.T) {
		dir := uploadDir(t)
		putTwoBatches(t, dir)
		srv := newTestServer(t, nil)
		in := input(dir, srv.URL)
		in.Encoder = hookEncoder{Encoder: NewOTLPEncoder(), onEncode: func() {
			if len(srv.seenRequests()) == 1 {
				panic("encoder failure")
			}
		}}
		res := SendNow(t.Context(), in)
		assert.Equal(t, telemetry.FailureSenderFailed, res.Failure)
		assert.Equal(t, 1, res.Requests, "the request sent before the panic is counted")
		requireNoLock(t, dir)
	})

	t.Run("recording turned off after the lock", func(t *testing.T) {
		dir := uploadDir(t)
		paths := putSlotRange(t, dir, hourAgo(3), 1, 2)
		srv := newTestServer(t, nil)
		in := input(dir, srv.URL)
		in.RecordingOn = func() bool { return false }
		res := SendNow(t.Context(), in)
		assert.Equal(t, telemetry.UploadResult{Failure: telemetry.FailureRecordingTurnedOff}, res)
		assert.Empty(t, srv.seenRequests())
		assert.Len(t, filesWaiting(dir, paths), 2)
		assert.Empty(t, uploadFiles(t, dir), "the directory holds nothing of this attempt")
	})

	t.Run("recording turned off before the deletion", func(t *testing.T) {
		dir := uploadDir(t)
		paths := putSlotRange(t, dir, hourAgo(3), 1, 2)
		srv := newTestServer(t, nil)
		in := input(dir, srv.URL)
		calls := 0
		in.RecordingOn = func() bool { calls++; return calls < 2 }
		res := SendNow(t.Context(), in)
		assert.Equal(t, telemetry.FailureRecordingTurnedOff, res.Failure)
		assert.Equal(t, 1, res.Requests)
		assert.Len(t, srv.seenRequests(), 1)
		assert.Len(t, filesWaiting(dir, paths), 2, "nothing was deleted")
		assert.NotContains(t, uploadFiles(t, dir), "state.json")
		requireNoLock(t, dir)
	})

	t.Run("a cancelled context during a request", func(t *testing.T) {
		dir := uploadDir(t)
		paths := putSlotRange(t, dir, hourAgo(3), 1, 2)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		srv := newTestServer(t, func(_ int, _ http.ResponseWriter, r *http.Request) {
			cancel()
			testtransport.HangAndAbort(r)
		})
		res := SendNow(ctx, input(dir, srv.URL))
		assert.Len(t, srv.seenRequests(), 1)
		assert.Equal(t, 1, res.Requests)
		assert.Empty(t, res.Failure)
		assert.Len(t, filesWaiting(dir, paths), 2, "no 2xx covered a file")
		requireNoLock(t, dir)
	})

	t.Run("a cancelled context before the run", func(t *testing.T) {
		dir := uploadDir(t)
		putSlotRange(t, dir, hourAgo(3), 1, 2)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		srv := newTestServer(t, nil)
		before := testfs.Snapshot(t, dir)
		res := SendNow(ctx, input(dir, srv.URL))
		assert.Equal(t, telemetry.UploadResult{}, res)
		assert.Empty(t, srv.seenRequests())
		assert.Equal(t, before, testfs.Snapshot(t, dir))
	})
}

// hookEncoder runs a function before each encode.
type hookEncoder struct {
	Encoder
	onEncode func()
}

func (e hookEncoder) Encode(events []telemetry.StoredEvent) (Request, error) {
	e.onEncode()
	return e.Encoder.Encode(events)
}

// TestUploadCommandIsBounded asserts the time limit passing before the first
// request, after the lock, is a timeUp result that sends nothing, and a limit
// that is already under the floor at the entry is the same.
func TestUploadCommandIsBounded(t *testing.T) {
	t.Run("the limit passes between the look and the lock", func(t *testing.T) {
		dir := uploadDir(t)
		paths := putSlotRange(t, dir, hourAgo(3), 1, 2)
		srv := newTestServer(t, nil)
		in := input(dir, srv.URL)
		in.Deadline = time.Now().Add(600 * time.Millisecond)
		in.Floor = 50 * time.Millisecond
		t.Cleanup(func() { afterLook = nil })
		afterLook = func() { time.Sleep(time.Until(in.Deadline) + 20*time.Millisecond) }
		res := SendNow(t.Context(), in)
		assert.Equal(t, telemetry.SkippedTimeUp, res.Skipped)
		assert.True(t, res.TimeLimitReached)
		assert.Empty(t, res.Failure)
		assert.Empty(t, srv.seenRequests())
		assert.Len(t, filesWaiting(dir, paths), 2)
		requireNoLock(t, dir)
	})

	t.Run("less than the floor left at the entry", func(t *testing.T) {
		dir := uploadDir(t)
		putSlotRange(t, dir, hourAgo(3), 1, 2)
		srv := newTestServer(t, nil)
		in := input(dir, srv.URL)
		in.Deadline = time.Now().Add(100 * time.Millisecond)
		before := testfs.Snapshot(t, dir)
		res := SendNow(t.Context(), in)
		assert.Equal(t, telemetry.UploadResult{Skipped: telemetry.SkippedTimeUp, TimeLimitReached: true}, res)
		assert.Equal(t, before, testfs.Snapshot(t, dir))
	})
}

// TestSendNowRefusesWhatItMust asserts a release version, an origin the check
// refuses or changes, a nil encoder and a nil controls function each make the
// entry fail with the sender failure and send nothing, and create nothing.
func TestSendNowRefusesWhatItMust(t *testing.T) {
	srv := newTestServer(t, nil)
	cases := map[string]func(in *Input){
		"release 1.2.3":              func(in *Input) { in.Version = "1.2.3" },
		"unknown":                    func(in *Input) { in.Version = "unknown" },
		"a non-loopback http origin": func(in *Input) { in.Origin = "http://h.example" },
		"an origin with a path":      func(in *Input) { in.Origin = "https://h.example/v1/logs" },
		"no encoder":                 func(in *Input) { in.Encoder = nil },
		"no controls function":       func(in *Input) { in.RecordingOn = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dir := uploadDir(t)
			putSlotRange(t, dir, hourAgo(3), 1, 2)
			in := input(dir, srv.URL)
			mutate(&in)
			before := testfs.Snapshot(t, dir)
			res := SendNow(t.Context(), in)
			assert.Equal(t, telemetry.UploadResult{Failure: telemetry.FailureSenderFailed}, res)
			assert.Empty(t, srv.seenRequests())
			assert.Equal(t, before, testfs.Snapshot(t, dir))
		})
	}
}
