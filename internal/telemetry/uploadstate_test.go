package telemetry

import (
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/testfs"
)

// TestUploadStateKeepsTheCoveredIDs asserts the covered record ids are written
// as the coveredIds key, read back in order, and left out of the file when
// there are none.
func TestUploadStateKeepsTheCoveredIDs(t *testing.T) {
	now := uploadNow()
	useClock(t, now)
	dir := telemetryDir(t)
	s := openStore(t, dir)
	lock, err := s.TakeUploadLock()
	require.NoError(t, err)

	ids := []string{testUUID(), testUUID(), testUUID()}
	require.NoError(t, lock.WriteState(UploadState{LastAttempt: now, CoveredIDs: ids}))
	got, ok := s.ReadUploadState()
	require.True(t, ok)
	assert.Equal(t, ids, got.CoveredIDs)

	require.NoError(t, lock.WriteState(UploadState{LastAttempt: now}))
	data, err := os.ReadFile(uploadPath(dir, fileState))
	require.NoError(t, err)
	assert.NotContains(t, string(data), "coveredIds")
	got, ok = s.ReadUploadState()
	require.True(t, ok)
	assert.Empty(t, got.CoveredIDs)
}

// TestWriteStateWithoutLock asserts the write with no lock held replaces the
// state file through a temporary file and a rename like the write under the
// lock, creates no lock, and creates nothing when telemetry/upload is missing.
func TestWriteStateWithoutLock(t *testing.T) {
	now := uploadNow()
	useClock(t, now)
	st := UploadState{LastAttempt: now, NoAnswer: "lock unavailable", Failures: 1, NextAttempt: now.Add(time.Hour)}

	t.Run("with telemetry upload", func(t *testing.T) {
		dir := telemetryDir(t)
		require.NoError(t, os.MkdirAll(uploadPath(dir), 0o700))
		s := openStore(t, dir)
		ops := observeOps(t)
		require.NoError(t, s.WriteStateWithoutLock(st))
		tmp := path.Join(dirUpload, stateTempName)
		assert.Equal(t, []string{
			"remove " + tmp, "openfile " + tmp, "write " + tmp, "close " + tmp, "rename " + tmp,
		}, ops.take())
		got, ok := s.ReadUploadState()
		require.True(t, ok)
		assert.Equal(t, st, got)
		assert.Equal(t, []string{fileState}, uploadTree(t, dir), "no lock and no temporary file")
	})

	t.Run("without telemetry upload", func(t *testing.T) {
		dir := telemetryDir(t)
		s := openStore(t, dir)
		before := testfs.Snapshot(t, dir)
		require.Error(t, s.WriteStateWithoutLock(st))
		assert.Equal(t, before, testfs.Snapshot(t, dir))
	})

	t.Run("without a telemetry directory", func(t *testing.T) {
		dir := t.TempDir()
		s := openStore(t, dir)
		require.ErrorIs(t, s.WriteStateWithoutLock(st), ErrNoTelemetryDirectory)
		assert.Empty(t, testfs.Tree(t, dir))
	})
}

// TestUploadLockStatus asserts the read-only check reports no telemetry
// directory, no upload directory, no lock and a stale lock as free, a fresh
// lock as held, a directory in place of the lock as unreadable, and a plain
// file or a symlink in place of telemetry/upload as unsafe, and that it
// creates and removes nothing.
func TestUploadLockStatus(t *testing.T) {
	now := uploadNow()
	useClock(t, now)

	check := func(t *testing.T, dir string, wantExists bool, wantErr error) {
		t.Helper()
		s := openStore(t, dir)
		before := testfs.Snapshot(t, dir)
		exists, err := s.UploadLockStatus()
		assert.Equal(t, wantExists, exists)
		if wantErr == nil {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, wantErr)
		}
		assert.Equal(t, before, testfs.Snapshot(t, dir), "the check changes nothing")
	}

	t.Run("no telemetry directory", func(t *testing.T) {
		check(t, t.TempDir(), false, nil)
	})
	t.Run("no upload directory", func(t *testing.T) {
		check(t, telemetryDir(t), false, nil)
	})
	t.Run("no lock", func(t *testing.T) {
		dir := telemetryDir(t)
		require.NoError(t, os.MkdirAll(uploadPath(dir), 0o700))
		check(t, dir, true, nil)
	})
	t.Run("a fresh lock", func(t *testing.T) {
		dir := telemetryDir(t)
		writeLock(t, dir, now.Add(-10*time.Second), "tok")
		check(t, dir, true, ErrUploadLockHeld)
	})
	t.Run("a stale lock", func(t *testing.T) {
		dir := telemetryDir(t)
		writeLock(t, dir, now.Add(-2*time.Minute), "tok")
		check(t, dir, true, nil)
	})
	t.Run("a lock that does not parse", func(t *testing.T) {
		dir := telemetryDir(t)
		require.NoError(t, os.MkdirAll(uploadPath(dir), 0o700))
		require.NoError(t, os.WriteFile(uploadPath(dir, fileLock), []byte("garbage"), 0o600))
		check(t, dir, true, nil)
	})
	t.Run("a directory in place of the lock", func(t *testing.T) {
		dir := telemetryDir(t)
		testfs.DirectoryInPlaceOfFile(t, uploadPath(dir, fileLock))
		check(t, dir, true, ErrUploadLockUnreadable)
	})
	t.Run("a plain file in place of telemetry upload", func(t *testing.T) {
		dir := telemetryDir(t)
		require.NoError(t, os.WriteFile(uploadPath(dir), []byte("x"), 0o600))
		check(t, dir, true, ErrUnsafeUploadDir)
	})
	t.Run("a telemetry directory that cannot be searched", func(t *testing.T) {
		dir := telemetryDir(t)
		require.NoError(t, os.MkdirAll(uploadPath(dir), 0o700))
		s := openStore(t, dir)
		testfs.RestrictAccess(t, filepath.Join(dir, "telemetry"), 0o600)
		exists, err := s.UploadLockStatus()
		assert.True(t, exists)
		require.ErrorIs(t, err, ErrUnsafeUploadDir, "the failure is about the directory")
		assert.NotErrorIs(t, err, ErrUploadLockUnreadable)
	})
	t.Run("a symlink in place of telemetry upload", func(t *testing.T) {
		dir := telemetryDir(t)
		target := filepath.Join(dir, "elsewhere")
		require.NoError(t, os.MkdirAll(target, 0o700))
		testfs.Symlink(t, target, uploadPath(dir))
		check(t, dir, true, ErrUnsafeUploadDir)
	})
}

// TestAttemptDue asserts the state allows an attempt when no next attempt is
// stored, when it is not after now, and when it lies further ahead than any
// wait the upload stage writes, and not otherwise.
func TestAttemptDue(t *testing.T) {
	now := uploadNow()
	for name, tc := range map[string]struct {
		next time.Time
		due  bool
	}{
		"none stored":       {time.Time{}, true},
		"in the past":       {now.Add(-time.Minute), true},
		"now":               {now, true},
		"a second ahead":    {now.Add(time.Second), false},
		"an hour ahead":     {now.Add(time.Hour), false},
		"a day ahead":       {now.Add(MaxAttemptWait), false},
		"the largest wait":  {now.Add(MaxAttemptWait + nextAttemptSlack), false},
		"past the largest":  {now.Add(MaxAttemptWait + nextAttemptSlack + time.Second), true},
		"far in the future": {now.Add(1000 * time.Hour), true},
	} {
		assert.Equal(t, tc.due, UploadState{NextAttempt: tc.next}.AttemptDue(now), name)
	}
}
