package telemetry

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/testfs"
)

func uploadPath(dir string, elem ...string) string {
	return filepath.Join(append([]string{dir, "telemetry", dirUpload}, elem...)...)
}

func readLock(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(uploadPath(dir, fileLock))
	require.NoError(t, err)
	return string(data)
}

// writeLock writes a lock file whose written time is at, with a token.
func writeLock(t *testing.T, dir string, at time.Time, token string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(uploadPath(dir), 0o700))
	content := at.UTC().Format(time.RFC3339Nano) + " " + token
	require.NoError(t, os.WriteFile(uploadPath(dir, fileLock), []byte(content), 0o600))
	return content
}

func uploadTree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, p := range testfs.Tree(t, dir) {
		if rest, ok := strings.CutPrefix(p, "telemetry/"+dirUpload+"/"); ok {
			out = append(out, rest)
		}
	}
	return out
}

// TestOneSenderPerDirectory asserts one process holds the upload lock of a
// directory at a time: a second taker is refused while the first holds it, a
// lock is replaced only when the time written in it is stale, a lost link
// leaves no temporary file, a release leaves a lock another holder took, a
// lock that cannot be read is reported as such and one whose read failed only
// because it vanished, changed or is open in another process is held.
func TestOneSenderPerDirectory(t *testing.T) {
	now := uploadNow()
	useClock(t, now)

	t.Run("a second taker", func(t *testing.T) {
		dir := telemetryDir(t)
		first, second := openStore(t, dir), openStore(t, dir)
		lock, err := first.TakeUploadLock()
		require.NoError(t, err)
		_, err = second.TakeUploadLock()
		require.ErrorIs(t, err, ErrUploadLockHeld)
		assert.Equal(t, []string{fileLock}, uploadTree(t, dir), "no temporary file is left")
		written, token, ok := parseLockContent(readLock(t, dir))
		require.True(t, ok)
		assert.True(t, written.Equal(now))
		assert.Equal(t, lock.token, token)

		require.NoError(t, lock.Release())
		again, err := second.TakeUploadLock()
		require.NoError(t, err)
		require.NoError(t, again.Release())
	})

	t.Run("many takers", func(t *testing.T) {
		dir := telemetryDir(t)
		const takers = 24
		start := make(chan struct{})
		var holders, held atomic.Int32
		var wg sync.WaitGroup
		for range takers {
			wg.Go(func() {
				s, err := OpenStore(dir)
				if err != nil {
					return
				}
				defer func() { _ = s.Close() }()
				<-start
				switch _, err := s.TakeUploadLock(); {
				case err == nil:
					holders.Add(1)
				case errors.Is(err, ErrUploadLockHeld):
					held.Add(1)
				}
			})
		}
		close(start)
		wg.Wait()
		assert.EqualValues(t, 1, holders.Load())
		assert.EqualValues(t, takers-1, held.Load())
		assert.Equal(t, []string{fileLock}, uploadTree(t, dir))
	})

	t.Run("a taker paused before its link", func(t *testing.T) {
		dir := telemetryDir(t)
		first, second := openStore(t, dir), openStore(t, dir)
		arrived, proceed := make(chan struct{}), make(chan struct{})
		var paused atomic.Bool
		useSeam(t, &pauseHook, func(stage string) {
			if stage == "lock-created" && paused.CompareAndSwap(false, true) {
				close(arrived)
				<-proceed
			}
		})
		var firstErr error
		var wg sync.WaitGroup
		wg.Go(func() { _, firstErr = first.TakeUploadLock() })
		<-arrived
		winner, err := second.TakeUploadLock()
		require.NoError(t, err)
		close(proceed)
		wg.Wait()
		require.ErrorIs(t, firstErr, ErrUploadLockHeld)
		assert.Equal(t, []string{fileLock}, uploadTree(t, dir), "the loser leaves no temporary file")
		_, token, _ := parseLockContent(readLock(t, dir))
		assert.Equal(t, winner.token, token)
	})

	t.Run("the lost link matches exists", func(t *testing.T) {
		dir := telemetryDir(t)
		s := openStore(t, dir)
		_, err := s.TakeUploadLock()
		require.NoError(t, err)
		_, lost, err := s.placeLock()
		require.ErrorIs(t, err, fs.ErrExist)
		assert.True(t, lost)
	})

	t.Run("the stale rule", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			content  func() string
			mtime    time.Time
			replaced bool
		}{
			{"exactly the stale age", func() string { return now.Add(-idStaleAfter).UTC().Format(time.RFC3339Nano) + " t" }, now, true},
			{"1 ns younger", func() string { return now.Add(-idStaleAfter+1).UTC().Format(time.RFC3339Nano) + " t" }, now.Add(-time.Hour), false},
			{"exactly the stale age ahead", func() string { return now.Add(idStaleAfter).UTC().Format(time.RFC3339Nano) + " t" }, now.Add(-time.Hour), false},
			{"1 ns more ahead", func() string { return now.Add(idStaleAfter+1).UTC().Format(time.RFC3339Nano) + " t" }, now, true},
			{"old time, fresh file time", func() string { return now.Add(-time.Hour).UTC().Format(time.RFC3339Nano) + " t" }, now, true},
			{"fresh time, old file time", func() string { return now.UTC().Format(time.RFC3339Nano) + " t" }, now.Add(-24 * time.Hour), false},
			{"fresh time with CRLF", func() string { return now.UTC().Format(time.RFC3339Nano) + " t\r\n" }, now, false},
			{"empty", func() string { return "" }, now, true},
			{"not a time", func() string { return "yesterday t" }, now, true},
			{"a time only", func() string { return now.UTC().Format(time.RFC3339Nano) }, now, true},
			{"three fields", func() string { return now.UTC().Format(time.RFC3339Nano) + " t u" }, now, true},
			{"oversized", func() string { return strings.Repeat("x", maxLockSize+1) }, now, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dir := telemetryDir(t)
				require.NoError(t, os.MkdirAll(uploadPath(dir), 0o700))
				content := tc.content()
				file := uploadPath(dir, fileLock)
				require.NoError(t, os.WriteFile(file, []byte(content), 0o600))
				require.NoError(t, os.Chtimes(file, tc.mtime, tc.mtime))
				s := openStore(t, dir)
				lock, err := s.TakeUploadLock()
				if tc.replaced {
					require.NoError(t, err)
					_, token, ok := parseLockContent(readLock(t, dir))
					require.True(t, ok)
					assert.Equal(t, lock.token, token, "the lock is the new holder's")
					return
				}
				require.ErrorIs(t, err, ErrUploadLockHeld)
				assert.Equal(t, content, readLock(t, dir), "the lock is left as it was")
			})
		}
	})

	t.Run("a lock that cannot be read", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			setup func(t *testing.T, dir string)
		}{
			{"a directory", func(t *testing.T, dir string) {
				t.Helper()
				testfs.DirectoryInPlaceOfFile(t, uploadPath(dir, fileLock))
			}},
			{"a symlink to nowhere", func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, os.MkdirAll(uploadPath(dir), 0o700))
				testfs.Symlink(t, filepath.Join(dir, "telemetry", "nowhere"), uploadPath(dir, fileLock))
			}},
			{"a file that may not be read", func(t *testing.T, dir string) {
				t.Helper()
				writeLock(t, dir, now.Add(-time.Hour), "old")
				testfs.RestrictAccess(t, uploadPath(dir, fileLock), 0o000)
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dir := telemetryDir(t)
				tc.setup(t, dir)
				before := testfs.Tree(t, dir)
				s := openStore(t, dir)
				_, err := s.TakeUploadLock()
				require.ErrorIs(t, err, ErrUploadLockUnreadable)
				require.NotErrorIs(t, err, ErrUploadLockHeld)
				assert.Equal(t, before, testfs.Tree(t, dir), "nothing was removed or created")

				require.NoError(t, s.DeleteUploadState(), "off and reset remove it")
				assert.NoDirExists(t, uploadPath(dir))
			})
		}
	})

	t.Run("a lock read that fails but the lock is fine", func(t *testing.T) {
		lockName := path.Join(dirUpload, fileLock)
		for _, tc := range []struct {
			name    string
			busy    bool
			removed bool
			hook    func(dir string) func(string) error
		}{
			{"the lock is gone again", false, true, func(dir string) func(string) error {
				return func(name string) error {
					if name == lockName {
						_ = os.Remove(uploadPath(dir, fileLock))
						return fs.ErrNotExist
					}
					return nil
				}
			}},
			{"the lock was replaced after the read", false, false, func(dir string) func(string) error {
				return func(name string) error {
					if name == lockName {
						return fs.ErrNotExist
					}
					return nil
				}
			}},
			{"another process has it open", true, false, func(dir string) func(string) error {
				return func(name string) error {
					if name == lockName {
						return errors.New("in use")
					}
					return nil
				}
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dir := telemetryDir(t)
				content := writeLock(t, dir, now, "holder")
				useSeam(t, &retryDelay, time.Millisecond)
				useSeam(t, &isBusy, func(error) bool { return tc.busy })
				useSeam(t, &readHook, tc.hook(dir))
				s := openStore(t, dir)
				_, err := s.TakeUploadLock()
				require.ErrorIs(t, err, ErrUploadLockHeld)
				require.NotErrorIs(t, err, ErrUploadLockUnreadable)
				if tc.removed {
					assert.NoFileExists(t, uploadPath(dir, fileLock), "the hook removed the lock")
					assert.Empty(t, uploadTree(t, dir), "the take created nothing")
					return
				}
				assert.Equal(t, content, readLock(t, dir), "the lock is left as it was")
			})
		}
	})

	t.Run("a busy read of a lock that is not a regular file", func(t *testing.T) {
		dir := telemetryDir(t)
		testfs.DirectoryInPlaceOfFile(t, uploadPath(dir, fileLock))
		before := testfs.Tree(t, dir)
		useSeam(t, &retryDelay, time.Millisecond)
		useSeam(t, &isBusy, func(error) bool { return true })
		useSeam(t, &readHook, func(name string) error {
			if name == path.Join(dirUpload, fileLock) {
				return errors.New("in use")
			}
			return nil
		})
		s := openStore(t, dir)
		_, err := s.TakeUploadLock()
		require.ErrorIs(t, err, ErrUploadLockUnreadable)
		require.NotErrorIs(t, err, ErrUploadLockHeld)
		assert.Equal(t, before, testfs.Tree(t, dir))
	})

	t.Run("a stale lock that cannot be removed", func(t *testing.T) {
		dir := telemetryDir(t)
		content := writeLock(t, dir, now.Add(-time.Hour), "old")
		links := 0
		useSeam(t, &linkHook, func(string, string) error { links++; return nil })
		useSeam(t, &removeFileHook, func(name string) error {
			if name == path.Join(dirUpload, fileLock) {
				return os.ErrPermission
			}
			return nil
		})
		s := openStore(t, dir)
		_, err := s.TakeUploadLock()
		require.ErrorIs(t, err, ErrUploadLockHeld)
		assert.Equal(t, content, readLock(t, dir))
		assert.Equal(t, 1, links, "no second link is tried after a failed removal")
	})

	t.Run("the lock changes between the two reads", func(t *testing.T) {
		dir := telemetryDir(t)
		writeLock(t, dir, now.Add(-time.Hour), "old")
		var changed string
		useSeam(t, &pauseHook, func(stage string) {
			if stage == "lock-judged" {
				changed = writeLock(t, dir, now, "new")
			}
		})
		s := openStore(t, dir)
		_, err := s.TakeUploadLock()
		require.ErrorIs(t, err, ErrUploadLockHeld)
		assert.Equal(t, changed, readLock(t, dir), "the changed lock is not removed")
	})

	t.Run("release", func(t *testing.T) {
		dir := telemetryDir(t)
		s := openStore(t, dir)
		lock, err := s.TakeUploadLock()
		require.NoError(t, err)

		replaced := writeLock(t, dir, now, "another-holder")
		require.NoError(t, lock.Release())
		assert.Equal(t, replaced, readLock(t, dir), "a lock another holder took stays")

		require.NoError(t, os.Remove(uploadPath(dir, fileLock)))
		other, err := s.TakeUploadLock()
		require.NoError(t, err)
		require.NoError(t, other.Release())
		require.NoError(t, other.Release(), "a release after a release is no error")
		assert.Empty(t, uploadTree(t, dir))

		last, err := s.TakeUploadLock()
		require.NoError(t, err)
		require.NoError(t, s.Close())
		assert.NotPanics(t, func() { assert.Error(t, last.Release()) })
		assert.NotPanics(t, func() { assert.Error(t, last.WriteState(UploadState{})) })
	})

	t.Run("failures", func(t *testing.T) {
		t.Run("a failed write", func(t *testing.T) {
			dir := telemetryDir(t)
			useSeam(t, &writeHook, func(name string, _ int) error {
				if strings.Contains(name, lockTempPrefix) {
					return errors.New("disk full")
				}
				return nil
			})
			s := openStore(t, dir)
			_, err := s.TakeUploadLock()
			require.ErrorContains(t, err, "disk full")
			assert.Empty(t, uploadTree(t, dir), "no lock and no temporary file")
		})

		t.Run("another link error", func(t *testing.T) {
			dir := telemetryDir(t)
			noLinks := errors.New("operation not supported")
			useSeam(t, &linkHook, func(string, string) error { return noLinks })
			s := openStore(t, dir)
			_, err := s.TakeUploadLock()
			require.ErrorIs(t, err, noLinks)
			require.NotErrorIs(t, err, ErrUploadLockHeld)
			assert.Empty(t, uploadTree(t, dir), "no lock was created another way, no temporary file")
		})

		t.Run("another link error over a stale lock", func(t *testing.T) {
			dir := telemetryDir(t)
			writeLock(t, dir, now.Add(-time.Hour), "old")
			calls := 0
			noLinks := errors.New("operation not permitted")
			useSeam(t, &linkHook, func(string, string) error {
				calls++
				if calls == 2 {
					return noLinks
				}
				return nil
			})
			s := openStore(t, dir)
			_, err := s.TakeUploadLock()
			require.ErrorIs(t, err, noLinks)
			assert.Empty(t, uploadTree(t, dir), "the stale lock was removed and no temporary file is left")
		})
	})

	t.Run("leftover temporary files", func(t *testing.T) {
		dir := telemetryDir(t)
		require.NoError(t, os.MkdirAll(uploadPath(dir), 0o700))
		put := func(name, content string) {
			require.NoError(t, os.WriteFile(uploadPath(dir, name), []byte(content), 0o600))
		}
		put(".lock-old", now.Add(-idStaleAfter).UTC().Format(time.RFC3339Nano)+" a")
		put(".lock-young", now.Add(-idStaleAfter+1).UTC().Format(time.RFC3339Nano)+" b")
		put(".lock-junk", "half")
		put("other", "keep")
		s := openStore(t, dir)
		_, err := s.TakeUploadLock()
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{fileLock, ".lock-young", ".lock-junk", "other"}, uploadTree(t, dir))
	})
}

// TestUploadFilesNeverCreateTheDirectory asserts the ready list, the deletion
// of covered files, the state read, the removal of telemetry/upload and the
// lock take create nothing when there is no telemetry directory, the take
// says so with its own error, and the state write, the release, the deletion
// and the removal create nothing once telemetry/upload is gone.
func TestUploadFilesNeverCreateTheDirectory(t *testing.T) {
	now := uploadNow()
	useClock(t, now)

	t.Run("no telemetry directory", func(t *testing.T) {
		dir := t.TempDir()
		s := openStore(t, dir)
		before := testfs.Snapshot(t, dir)
		list, err := s.Ready(ReadyOptions{CurrentHour: true})
		require.NoError(t, err)
		assert.Empty(t, list.Events)
		_, ok := s.ReadUploadState()
		assert.False(t, ok)
		assert.Empty(t, s.DeleteCovered(Coverage{dir: s.configDir}).Deleted)
		require.NoError(t, s.DeleteUploadState())
		_, err = s.TakeUploadLock()
		require.ErrorIs(t, err, ErrNoTelemetryDirectory)
		require.NotErrorIs(t, err, ErrUploadLockHeld)
		assert.Equal(t, before, testfs.Snapshot(t, dir))
		assert.Empty(t, testfs.Tree(t, dir))
	})

	t.Run("a telemetry directory without events", func(t *testing.T) {
		dir := telemetryDir(t)
		s := openStore(t, dir)
		before := testfs.Snapshot(t, dir)
		list, err := s.Ready(ReadyOptions{})
		require.NoError(t, err)
		assert.Empty(t, list.Events)
		_, ok := s.ReadUploadState()
		assert.False(t, ok)
		assert.Equal(t, before, testfs.Snapshot(t, dir), "no upload and no events directory was created")
	})

	t.Run("the upload directory is removed under a held lock", func(t *testing.T) {
		dir := telemetryDir(t)
		s := openStore(t, dir)
		lock, err := s.TakeUploadLock()
		require.NoError(t, err)
		require.NoError(t, os.RemoveAll(uploadPath(dir)))
		ev := runEvent(hoursAgo(now, 3))
		putSlot(t, dir, 1, ev)
		slot := slotPath(ev.Hour(), 1)
		before := testfs.Tree(t, dir)
		require.Error(t, lock.WriteState(UploadState{Sent: 1}))
		require.NoError(t, lock.Release())
		res := s.DeleteCovered(mustReady(t, s, ReadyOptions{}).Cover(idSet(ev.UID())))
		assert.Equal(t, []string{slot}, res.Deleted, "a real covered file is removed")
		assert.Equal(t, without(before, slot), testfs.Tree(t, dir), "the deletion created nothing")
		require.NoError(t, s.DeleteUploadState())
		assert.Equal(t, without(before, slot), testfs.Tree(t, dir), "the removal created nothing")
		assert.NoDirExists(t, uploadPath(dir))
	})
}

// TestUploadStateRoundTrip asserts a state written under the lock reads back
// equal, a second write replaces the first, the keys are optional and
// unknown keys are ignored, a file that cannot be used reads as the zero value,
// and a write uses a temporary file and a rename.
func TestUploadStateRoundTrip(t *testing.T) {
	now := uploadNow()
	useClock(t, now)
	dir := telemetryDir(t)
	s := openStore(t, dir)
	lock, err := s.TakeUploadLock()
	require.NoError(t, err)
	refused := 2
	first := UploadState{
		LastAttempt: now, HTTPStatus: 200, Sent: 5, Refused: &refused, Dropped: 1,
		NextAttempt: now.Add(time.Hour), Failures: 3,
	}

	_, ok := s.ReadUploadState()
	assert.False(t, ok, "no file yet")
	require.NoError(t, lock.WriteState(first))
	got, ok := s.ReadUploadState()
	require.True(t, ok)
	assert.Equal(t, first, got)

	second := UploadState{LastAttempt: now.Add(time.Minute), NoAnswer: "timeout", Failures: 4}
	require.NoError(t, lock.WriteState(second))
	got, ok = s.ReadUploadState()
	require.True(t, ok)
	assert.Equal(t, second, got)
	assert.Nil(t, got.Refused, "an unknown count reads as nil")
	assert.Equal(t, []string{fileLock, fileState}, uploadTree(t, dir), "no temporary file is left")

	file := uploadPath(dir, fileState)
	for name, content := range map[string]string{
		"null refused and unknown keys": `{"sent": 3, "refused": null, "future": {"a": 1}}`,
		"CRLF and a trailing newline":   "{\r\n  \"sent\": 3\r\n}\r\n",
	} {
		require.NoError(t, os.WriteFile(file, []byte(content), 0o600))
		got, ok = s.ReadUploadState()
		assert.True(t, ok, name)
		assert.Equal(t, 3, got.Sent, name)
		assert.Nil(t, got.Refused, name)
	}

	for name, content := range map[string]string{
		"not json": "sent", "empty": "", "an array": "[1]", "null": "null",
		"a wrong type": `{"sent": "many"}`, "a bad time": `{"nextAttempt": "soon"}`,
		"over the cap": `{"sent": 3, "padding": "` + strings.Repeat("x", maxStateSize) + `"}`,
	} {
		require.NoError(t, os.WriteFile(file, []byte(content), 0o600))
		got, ok = s.ReadUploadState()
		assert.False(t, ok, name)
		assert.Equal(t, UploadState{}, got, name)
	}

	require.NoError(t, os.Remove(file))
	testfs.DirectoryInPlaceOfFile(t, file)
	got, ok = s.ReadUploadState()
	assert.False(t, ok, "a directory in place of the file")
	assert.Equal(t, UploadState{}, got)
	require.NoError(t, os.Remove(file))

	t.Run("a leftover temporary name that is a symlink", func(t *testing.T) {
		keep := filepath.Join(dir, "telemetry", "keep")
		require.NoError(t, os.WriteFile(keep, []byte("untouched"), 0o600))
		testfs.Symlink(t, keep, uploadPath(dir, stateTempName))
		require.NoError(t, lock.WriteState(first))
		data, err := os.ReadFile(keep)
		require.NoError(t, err)
		assert.Equal(t, "untouched", string(data))
		got, ok := s.ReadUploadState()
		require.True(t, ok)
		assert.Equal(t, first, got)
	})

	t.Run("the operations of a write", func(t *testing.T) {
		ops := observeOps(t)
		require.NoError(t, lock.WriteState(second))
		tmp := path.Join(dirUpload, stateTempName)
		assert.Equal(t, []string{
			"remove " + tmp, "openfile " + tmp, "write " + tmp, "close " + tmp, "rename " + tmp,
		}, ops.take())
	})
}

// TestBusyFilesAreRetriedOnce asserts the removal of telemetry/upload, the
// rename of the state file and the removal of the lock are tried once more
// when the first try fails with a busy error, and no more than once.
func TestBusyFilesAreRetriedOnce(t *testing.T) {
	now := uploadNow()
	useClock(t, now)
	useSeam(t, &isBusy, func(error) bool { return true })
	useSeam(t, &retryDelay, time.Millisecond)
	busy := errors.New("in use")

	failing := func(t *testing.T, seam *func(string) error, match string, times int) *int {
		t.Helper()
		calls := 0
		useSeam(t, seam, func(name string) error {
			if name != match {
				return nil
			}
			calls++
			if calls <= times {
				return busy
			}
			return nil
		})
		return &calls
	}

	t.Run("removing the upload directory", func(t *testing.T) {
		for _, times := range []int{0, 1, 2} {
			dir := telemetryDir(t)
			require.NoError(t, os.MkdirAll(uploadPath(dir), 0o700))
			calls := failing(t, &removeAllHook, dirUpload, times)
			err := openStore(t, dir).DeleteUploadState()
			if times == 2 {
				require.ErrorIs(t, err, busy)
				assert.Equal(t, 2, *calls, "tried twice, no more")
				assert.DirExists(t, uploadPath(dir))
				continue
			}
			require.NoError(t, err)
			assert.Equal(t, times+1, *calls)
			assert.NoDirExists(t, uploadPath(dir))
		}
	})

	t.Run("renaming the state file", func(t *testing.T) {
		for _, times := range []int{1, 2} {
			dir := telemetryDir(t)
			s := openStore(t, dir)
			lock, err := s.TakeUploadLock()
			require.NoError(t, err)
			calls := 0
			useSeam(t, &renameHook, func(oldname, _ string) error {
				calls++
				if calls <= times {
					return busy
				}
				return nil
			})
			err = lock.WriteState(UploadState{Sent: 9})
			if times == 2 {
				require.ErrorIs(t, err, busy)
				assert.Equal(t, 2, calls)
				assert.Equal(t, []string{fileLock}, uploadTree(t, dir), "no temporary file is left")
				continue
			}
			require.NoError(t, err)
			assert.Equal(t, 2, calls)
			got, ok := s.ReadUploadState()
			require.True(t, ok)
			assert.Equal(t, 9, got.Sent)
		}
	})

	t.Run("releasing the lock", func(t *testing.T) {
		for _, times := range []int{1, 2} {
			dir := telemetryDir(t)
			s := openStore(t, dir)
			lock, err := s.TakeUploadLock()
			require.NoError(t, err)
			calls := failing(t, &removeFileHook, path.Join(dirUpload, fileLock), times)
			err = lock.Release()
			if times == 2 {
				require.ErrorIs(t, err, busy)
				assert.Equal(t, 2, *calls)
				assert.Equal(t, []string{fileLock}, uploadTree(t, dir), "the lock stays")
				continue
			}
			require.NoError(t, err)
			assert.Equal(t, 2, *calls)
			assert.Empty(t, uploadTree(t, dir))
		}
	})
}

// TestOpenFilesOnWindows asserts what an open handle does to the upload
// files. The store opens its files with delete sharing and removes them with
// delete semantics, so only a handle opened without delete sharing (os.Open,
// other programs) blocks a removal or a replacement, and only on Windows;
// everywhere else the file is removed.
func TestOpenFilesOnWindows(t *testing.T) {
	now := uploadNow()
	useClock(t, now)
	useSeam(t, &retryDelay, time.Millisecond)
	hold := func(t *testing.T, file string) {
		t.Helper()
		f, err := os.Open(file)
		require.NoError(t, err)
		t.Cleanup(func() { _ = f.Close() })
	}

	t.Run("a covered slot", func(t *testing.T) {
		dir := t.TempDir()
		h := hoursAgo(now, 3)
		ev := runEvent(h)
		file := putSlot(t, dir, 1, ev)
		s := openStore(t, dir)
		cov := mustReady(t, s, ReadyOptions{}).Cover(idSet(ev.UID()))
		hold(t, file)
		res := s.DeleteCovered(cov)
		if testfs.OnWindows() {
			assert.Equal(t, []string{slotPath(h, 1)}, res.Waiting)
			assert.Empty(t, res.Deleted)
			assert.FileExists(t, file)
			return
		}
		assert.Equal(t, []string{slotPath(h, 1)}, res.Deleted)
		assert.NoFileExists(t, file)
	})

	t.Run("the state file", func(t *testing.T) {
		dir := telemetryDir(t)
		s := openStore(t, dir)
		lock, err := s.TakeUploadLock()
		require.NoError(t, err)
		require.NoError(t, lock.WriteState(UploadState{Sent: 1}))
		hold(t, uploadPath(dir, fileState))
		err = lock.WriteState(UploadState{Sent: 2})
		if testfs.OnWindows() {
			require.Error(t, err)
			assert.Equal(t, []string{fileLock, fileState}, uploadTree(t, dir), "no temporary file is left")
			return
		}
		require.NoError(t, err)
	})

	t.Run("a stale lock", func(t *testing.T) {
		dir := telemetryDir(t)
		content := writeLock(t, dir, now.Add(-time.Hour), "old")
		hold(t, uploadPath(dir, fileLock))
		s := openStore(t, dir)
		lock, err := s.TakeUploadLock()
		if testfs.OnWindows() {
			require.ErrorIs(t, err, ErrUploadLockHeld)
			assert.Equal(t, content, readLock(t, dir))
			return
		}
		require.NoError(t, err)
		assert.Equal(t, lock.token, strings.Fields(readLock(t, dir))[1])
	})

	t.Run("releasing a lock that is open", func(t *testing.T) {
		dir := telemetryDir(t)
		s := openStore(t, dir)
		lock, err := s.TakeUploadLock()
		require.NoError(t, err)
		hold(t, uploadPath(dir, fileLock))
		err = lock.Release()
		if testfs.OnWindows() {
			require.Error(t, err)
			assert.FileExists(t, uploadPath(dir, fileLock))
			return
		}
		require.NoError(t, err)
		assert.NoFileExists(t, uploadPath(dir, fileLock))
	})
}

// TestUploadFilesAreOwnerOnly asserts the upload directory, the lock and the
// state file can be reached by their owner only.
func TestUploadFilesAreOwnerOnly(t *testing.T) {
	enable(t)
	now := uploadNow()
	useClock(t, now)
	dir := t.TempDir()
	require.Equal(t, StatusRecorded, record(t, dir, runEvent(now)))
	s := openStore(t, dir)
	lock, err := s.TakeUploadLock()
	require.NoError(t, err)
	require.NoError(t, lock.WriteState(UploadState{Sent: 1}))
	for _, p := range []string{uploadPath(dir), uploadPath(dir, fileLock), uploadPath(dir, fileState)} {
		testfs.RequireOwnerOnly(t, p)
	}
}

// TestUploadPathOverAPlainFileOrSymlink asserts a telemetry/upload that is a
// plain file or a symlink is refused and left alone, and the removal of a
// symlinked one removes the link and not what it points at.
func TestUploadPathOverAPlainFileOrSymlink(t *testing.T) {
	now := uploadNow()
	useClock(t, now)

	t.Run("a plain file", func(t *testing.T) {
		dir := telemetryDir(t)
		require.NoError(t, os.WriteFile(uploadPath(dir), []byte("file"), 0o600))
		s := openStore(t, dir)
		before := testfs.Snapshot(t, dir)
		_, err := s.TakeUploadLock()
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrUploadLockHeld)
		assert.Equal(t, before, testfs.Snapshot(t, dir), "nothing was removed or created")
		assert.NotPanics(t, func() {
			assert.Error(t, (&UploadLock{s: s, token: "x"}).WriteState(UploadState{Sent: 1}))
		})
		assert.Equal(t, before, testfs.Snapshot(t, dir))
	})

	t.Run("a symlink", func(t *testing.T) {
		dir := telemetryDir(t)
		target := filepath.Join(dir, "telemetry", "elsewhere")
		require.NoError(t, os.Mkdir(target, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(target, "keep"), []byte("untouched"), 0o600))
		testfs.Symlink(t, target, uploadPath(dir))
		s := openStore(t, dir)
		before := testfs.Snapshot(t, target)

		_, err := s.TakeUploadLock()
		require.ErrorIs(t, err, ErrUnsafeUploadDir)
		assert.Equal(t, before, testfs.Snapshot(t, target), "the target is unchanged")

		require.NoError(t, s.DeleteUploadState())
		_, err = os.Lstat(uploadPath(dir))
		require.ErrorIs(t, err, fs.ErrNotExist, "the link is removed")
		assert.Equal(t, before, testfs.Snapshot(t, target), "what it pointed at stays")
	})
}
