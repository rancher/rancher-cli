package telemetrysend

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/telemetry"
	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/rancher/rancher-cli/internal/testtransport"
)

// TestUploadOncePerHour asserts two attempts inside an hour with files waiting
// send once, that the next attempt time written is between 1 hour and 1 hour 10
// minutes after the attempt, that a next attempt more than a day ahead is due,
// that less than the floor left means no attempt and no write, that an attempt
// with nothing to send writes the next attempt time when telemetry/upload
// exists and the next run then reads only the state file, and that with
// telemetry/upload missing and nothing to send nothing is created.
func TestUploadOncePerHour(t *testing.T) {
	dir := uploadDir(t)
	hour := hourAgo(3)
	putSlotRange(t, dir, hour, 1, 3)
	srv := newTestServer(t, nil)
	in := input(dir, srv.URL)

	start := time.Now()
	Send(t.Context(), in)
	require.Len(t, srv.seenRequests(), 1)
	st, ok := attemptState(t, dir)
	require.True(t, ok)
	assert.GreaterOrEqual(t, st.NextAttempt.Sub(start), time.Hour)
	assert.Less(t, st.NextAttempt.Sub(start), time.Hour+maxJitter+time.Second)
	assert.Equal(t, 3, st.Sent)
	assert.Empty(t, readyIDs(t, dir))

	putSlotRange(t, dir, hour, 10, 2)
	Send(t.Context(), in)
	assert.Len(t, srv.seenRequests(), 1, "the second attempt inside the hour sends nothing")
	again, _ := attemptState(t, dir)
	assert.Equal(t, st, again, "an attempt that is not due leaves the state")

	st.NextAttempt = time.Now().Add(25 * time.Hour)
	writeState(t, dir, st)
	Send(t.Context(), in)
	assert.Len(t, srv.seenRequests(), 2, "a next attempt more than a day ahead is due")

	t.Run("less than the floor left", func(t *testing.T) {
		dir := uploadDir(t)
		putSlotRange(t, dir, hour, 1, 1)
		short := input(dir, srv.URL)
		short.Deadline = time.Now().Add(500 * time.Millisecond)
		before := len(srv.seenRequests())
		Send(t.Context(), short)
		assert.Len(t, srv.seenRequests(), before)
		assert.Empty(t, uploadFiles(t, dir))
		assert.Equal(t, []string{"events"}, dirNames(t, filepath.Join(dir, "telemetry")))
	})

	t.Run("nothing to send with telemetry upload present", func(t *testing.T) {
		dir := uploadDir(t)
		writeState(t, dir, telemetry.UploadState{LastAttempt: time.Now().Add(-2 * time.Hour), HTTPStatus: 503, Failures: 2, NextAttempt: time.Now().Add(-time.Hour)})
		before := len(srv.seenRequests())
		start := time.Now()
		Send(t.Context(), input(dir, srv.URL))
		assert.Len(t, srv.seenRequests(), before)
		st, ok := attemptState(t, dir)
		require.True(t, ok)
		assert.GreaterOrEqual(t, st.NextAttempt.Sub(start), time.Hour)
		assert.Less(t, st.NextAttempt.Sub(start), time.Hour+maxJitter+time.Second)
		assert.Equal(t, 0, st.Failures)
		assert.Equal(t, 503, st.HTTPStatus, "the last answer stays what status shows")
		assert.NotContains(t, uploadFiles(t, dir), "lock")

		var ops []string
		t.Cleanup(telemetry.SetObserverForTest(func(op string) { ops = append(ops, op) }))
		Send(t.Context(), input(dir, srv.URL))
		assert.Equal(t, []string{"openroot .", "lstat telemetry", "openroot telemetry", "readfile upload/state.json"}, ops,
			"a run before the next attempt reads the state and walks nothing")
	})

	t.Run("nothing to send with telemetry upload missing", func(t *testing.T) {
		dir := uploadDir(t)
		before := len(srv.seenRequests())
		Send(t.Context(), input(dir, srv.URL))
		assert.Len(t, srv.seenRequests(), before)
		assert.Equal(t, []string{}, dirNames(t, filepath.Join(dir, "telemetry")))

		missing := t.TempDir()
		Send(t.Context(), input(missing, srv.URL))
		assert.Equal(t, []string{}, dirNames(t, missing), "no telemetry directory: nothing is created")
	})
}

// TestWalkAfterTheDeletionRunsOnlyWithTimeLeft asserts an attempt that sends
// and deletes lists the stored events before the lock, again after it, then
// once more after the deletion to drop the ids of the deleted files; and that an
// attempt with less than the floor left after the deletion, or past the
// deadline, does not list them a third time and keeps the ids for the next
// attempt to drop.
func TestWalkAfterTheDeletionRunsOnlyWithTimeLeft(t *testing.T) {
	for name, tc := range map[string]struct {
		// leave is the time left before the deadline once the first file is
		// removed; a negative value is past the deadline. Zero leaves the
		// attempt alone.
		leave      time.Duration
		walks      int
		coveredIDs int
	}{
		"time left after the deletion":                {0, 3, 0},
		"the deadline passes in the deletion":         {-20 * time.Millisecond, 2, 2},
		"less than the floor left after the deletion": {25 * time.Millisecond, 2, 2},
	} {
		t.Run(name, func(t *testing.T) {
			dir := uploadDir(t)
			paths := putSlotRange(t, dir, hourAgo(3), 1, 2)
			srv := newTestServer(t, nil)
			in := input(dir, srv.URL)
			in.Floor = 50 * time.Millisecond
			clock := stopClock(t)
			walks := 0
			shrunk := false
			t.Cleanup(telemetry.SetObserverForTest(func(op string) {
				switch {
				case op == "readdir events":
					walks++
				case tc.leave != 0 && !shrunk && strings.HasPrefix(op, "remove events/"):
					shrunk = true
					clock.leave(in, tc.leave)
				}
			}))
			Send(t.Context(), in)
			assert.Len(t, srv.seenRequests(), 1)
			assert.Equal(t, tc.walks, walks)
			assert.Empty(t, filesWaiting(dir, paths), "both files are deleted")
			st, ok := attemptState(t, dir)
			require.True(t, ok)
			assert.Len(t, st.CoveredIDs, tc.coveredIDs)
		})
	}
}

// dirNames lists the entries of dir.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestOneSenderPerDirectory asserts that two attempts on one directory, with
// separate stores and the server holding the first request until the second
// has tried, send once and that the other sends nothing, that a stale lock
// does not stop the upload, that an attempt that took the lock after another
// ended one sends nothing, and that an attempt whose take failed keeps the
// state another process wrote after the first look.
func TestOneSenderPerDirectory(t *testing.T) {
	dir := uploadDir(t)
	putSlotRange(t, dir, hourAgo(3), 1, 2)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := newTestServer(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		testtransport.HangUntilReleased(r, release)
		_, _ = w.Write([]byte("{}"))
	})

	var wg sync.WaitGroup
	wg.Go(func() { Send(t.Context(), input(dir, srv.URL)) })
	<-entered
	Send(t.Context(), input(dir, srv.URL))
	assert.Len(t, srv.seenRequests(), 1, "the second attempt found the lock held and sent nothing")
	close(release)
	wg.Wait()
	assert.Len(t, srv.seenRequests(), 1)
	assert.Empty(t, readyIDs(t, dir))
	assert.NotContains(t, uploadFiles(t, dir), "lock", "the lock is released")

	t.Run("a stale lock", func(t *testing.T) {
		dir := uploadDir(t)
		putSlotRange(t, dir, hourAgo(3), 1, 1)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "telemetry", "upload"), 0o700))
		stale := time.Now().Add(-5*time.Minute).UTC().Format(time.RFC3339Nano) + " 0123456789abcdef"
		require.NoError(t, os.WriteFile(filepath.Join(dir, "telemetry", "upload", "lock"), []byte(stale), 0o600))
		srv := newTestServer(t, nil)
		Send(t.Context(), input(dir, srv.URL))
		assert.Len(t, srv.seenRequests(), 1)
	})

	t.Run("an attempt that ended between the look and the lock", func(t *testing.T) {
		dir := uploadDir(t)
		paths := putSlotRange(t, dir, hourAgo(3), 1, 2)
		srv := newTestServer(t, answerWith(http.StatusTooManyRequests, "", "Retry-After", "7200"))
		t.Cleanup(func() { afterLook = nil })
		afterLook = func() {
			afterLook = nil
			Send(t.Context(), input(dir, srv.URL))
		}
		start := time.Now()
		Send(t.Context(), input(dir, srv.URL))
		assert.Len(t, srv.seenRequests(), 1, "the attempt that took the lock later found the state of the first and sent nothing")
		st, ok := attemptState(t, dir)
		require.True(t, ok)
		assert.Equal(t, http.StatusTooManyRequests, st.HTTPStatus)
		assert.Equal(t, 1, st.Failures)
		assert.InDelta(t, float64(2*time.Hour), float64(st.NextAttempt.Sub(start)), float64(3*time.Second), "the wait the first attempt wrote")
		assert.Equal(t, paths, filesWaiting(dir, paths))
		assert.NotContains(t, uploadFiles(t, dir), "lock")
	})

	t.Run("an attempt that ended between the look and a failed take", func(t *testing.T) {
		dir := uploadDir(t)
		paths := putSlotRange(t, dir, hourAgo(3), 1, 2)
		other := telemetry.UploadState{
			LastAttempt: time.Now(), HTTPStatus: http.StatusTooManyRequests, Failures: 3,
			NextAttempt: time.Now().Add(6 * time.Hour), CoveredIDs: []string{uidOf(1)},
		}
		t.Cleanup(func() { afterLook = nil })
		afterLook = func() {
			afterLook = nil
			writeState(t, dir, other)
			testfs.DirectoryInPlaceOfFile(t, filepath.Join(dir, "telemetry", "upload", "lock"))
		}
		srv := newTestServer(t, nil)
		Send(t.Context(), input(dir, srv.URL))
		assert.Empty(t, srv.seenRequests())
		st, ok := attemptState(t, dir)
		require.True(t, ok)
		assert.Zero(t, st.NoAnswer, "the state the other process wrote stays")
		assert.Equal(t, 3, st.Failures)
		assert.Equal(t, []string{uidOf(1)}, st.CoveredIDs)
		assert.WithinDuration(t, other.NextAttempt, st.NextAttempt, time.Second)
		assert.Equal(t, paths, filesWaiting(dir, paths))
	})

	t.Run("a fresh lock", func(t *testing.T) {
		dir := uploadDir(t)
		putSlotRange(t, dir, hourAgo(3), 1, 1)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "telemetry", "upload"), 0o700))
		fresh := time.Now().UTC().Format(time.RFC3339Nano) + " 0123456789abcdef"
		require.NoError(t, os.WriteFile(filepath.Join(dir, "telemetry", "upload", "lock"), []byte(fresh), 0o600))
		srv := newTestServer(t, nil)
		Send(t.Context(), input(dir, srv.URL))
		assert.Empty(t, srv.seenRequests())
		assert.Equal(t, []string{"lock"}, uploadFiles(t, dir), "nothing written while another process holds the lock")
	})
}
