package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// pausedFixture is an `auto` directory pinned to pin whose two entries,
// prod and staging, both have their cache writes paused as pause says.
func pausedFixture(t *testing.T, pin, pause string) string {
	t.Helper()
	t.Setenv(EnvCredentialStore, "")
	t.Setenv(EnvConfigNoLock, "")
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &Config{CredentialStore: "auto", Servers: map[string]Server{
		"prod":    {URL: "https://r.ex", CacheBindingID: "p1", CacheWritesPaused: pause},
		"staging": {URL: "https://s.ex", CacheBindingID: "s1", CacheWritesPaused: pause},
	}}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())
	if pin != "" {
		require.NoError(t, WritePin(dir, pin))
	}
	return dir
}

// pausesOnDisk is each entry's pause as config.yaml holds it.
func pausesOnDisk(t *testing.T, dir string) map[string]string {
	t.Helper()
	cfg, err := Load(FilePath(dir))
	require.NoError(t, err)
	out := map[string]string{}
	for n, s := range cfg.Servers {
		out[n] = s.CacheWritesPaused
	}
	return out
}

// seedTransitionFile writes a manifest with these steps and names it in
// config.yaml when named is true.
func seedTransitionFile(t *testing.T, dir string, named bool, steps ...transitionStep) {
	t.Helper()
	id, err := newTransitionID()
	require.NoError(t, err)
	require.NoError(t, writeTransition(dir, transitionManifest{Version: transitionVersion, ID: id, Steps: steps}))
	if named {
		cfg, lerr := Load(FilePath(dir))
		require.NoError(t, lerr)
		cfg.CredentialTransitionID = id
		require.NoError(t, cfg.Write())
	}
}

// TestAPauseResumesOnlyWhenNothingLeftCanMoveReaders asserts the resume
// rule, one row per state the store change record can be in: none,
// finished, abandoned, active with only delete steps, and active with a
// pin step whose store readers already resolve all let a resumer holding
// the lock clear every pause; a pin step that would move readers, and
// every record that cannot be read, keep all of them.
func TestAPauseResumesOnlyWhenNothingLeftCanMoveReaders(t *testing.T) {
	pinTo := func(target string) transitionStep { return transitionStep{Kind: transitionPin, Target: target} }
	for _, tc := range []struct {
		name   string
		pin    string
		setup  func(t *testing.T, dir string)
		resume bool
		reason string
	}{
		{name: "no change recorded", pin: backendFile, resume: true},
		{name: "a finished change", pin: backendFile, resume: true, setup: func(t *testing.T, dir string) {
			t.Helper()
			id, err := newTransitionID()
			require.NoError(t, err)
			cfg, lerr := Load(FilePath(dir))
			require.NoError(t, lerr)
			cfg.CredentialTransitionID = id
			require.NoError(t, cfg.Write())
		}},
		{name: "an abandoned change, pin step included", pin: backendFile, resume: true, setup: func(t *testing.T, dir string) {
			t.Helper()
			seedTransitionFile(t, dir, false, pinTo(backendKeychain))
		}},
		{name: "an active change with only a delete step", pin: backendFile, resume: true, setup: func(t *testing.T, dir string) {
			t.Helper()
			seedActiveDelete(t, dir, "prod")
		}},
		{name: "an active change whose pin names the store readers resolve", pin: backendKeychain, resume: true, setup: func(t *testing.T, dir string) {
			t.Helper()
			seedTransitionFile(t, dir, true, pinTo(backendKeychain))
		}},
		{name: "an active change whose pin would move readers", pin: backendFile, reason: PauseReasonPinStep, setup: func(t *testing.T, dir string) {
			t.Helper()
			seedTransitionFile(t, dir, true, pinTo(backendKeychain))
		}},
		{name: "an unpinned directory whose pin step would move readers off the file store", reason: PauseReasonPinStep, setup: func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "staging", URL: "https://s.ex"}, "tok"))
			seedTransitionFile(t, dir, true, pinTo(backendKeychain))
		}},
		{name: "an unpinned directory whose store cannot be resolved, with a pin step left", reason: PauseReasonPinStep, setup: func(t *testing.T, dir string) {
			t.Helper()
			// Credentials in both stores and no pin: readers resolve no
			// store at all, so the pin step would move every one of them.
			require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "staging", URL: "https://s.ex"}, "tok"))
			seedKeychainCredential(t, dir, "prod", "tok")
			seedTransitionFile(t, dir, true, pinTo(backendKeychain))
		}},
		{name: "a record that cannot be parsed", pin: backendFile, reason: PauseReasonChangeRecordUnreadable, setup: func(t *testing.T, dir string) {
			t.Helper()
			seedTransitionFile(t, dir, true, pinTo(backendKeychain))
			require.NoError(t, os.WriteFile(TransitionPath(dir), []byte("not: [yaml"), 0o600))
		}},
		{name: "a record that is a symlink", pin: backendFile, reason: PauseReasonChangeRecordUnreadable, setup: func(t *testing.T, dir string) {
			t.Helper()
			if runtime.GOOS == "windows" {
				t.Skip("an unprivileged runner cannot create a symlink")
			}
			seedTransitionFile(t, dir, true, pinTo(backendKeychain))
			target := filepath.Join(t.TempDir(), "elsewhere")
			require.NoError(t, os.Rename(TransitionPath(dir), target))
			require.NoError(t, os.Symlink(target, TransitionPath(dir)))
		}},
		{name: "a record that is not the one named", pin: backendFile, reason: PauseReasonChangeRecordUnreadable, setup: func(t *testing.T, dir string) {
			t.Helper()
			seedTransitionFile(t, dir, true, pinTo(backendKeychain))
			cfg, lerr := Load(FilePath(dir))
			require.NoError(t, lerr)
			other, err := newTransitionID()
			require.NoError(t, err)
			cfg.CredentialTransitionID = other
			require.NoError(t, cfg.Write())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := pausedFixture(t, tc.pin, PauseLocked)
			if tc.setup != nil {
				tc.setup(t, dir)
			}
			cfg, err := Load(FilePath(dir))
			require.NoError(t, err)
			if !tc.resume {
				assert.Equal(t, []PausedEntry{{Entry: "prod", Reason: tc.reason}, {Entry: "staging", Reason: tc.reason}},
					PausedEntries(dir, cfg), "each entry names what holds it")
			}
			assert.Equal(t, tc.resume, ResumeCacheWrites(dir, cfg))
			for name, s := range cfg.Servers {
				assert.Equal(t, !tc.resume, s.Paused(), "%s", name)
			}
		})
	}
}

// TestAPauseMadeWithoutTheLockIsClearedOnlyByItsOwnChange asserts the
// marker a change made with RANCHER_CLI_CONFIG_NO_LOCK leaves: a resumer
// holding the flock proves nothing about a change that never took it, so
// it leaves that pause; the change that made it clears it in its own last
// write; and a resumer without the lock clears nothing but its own.
func TestAPauseMadeWithoutTheLockIsClearedOnlyByItsOwnChange(t *testing.T) {
	t.Run("a resumer holding the lock leaves it", func(t *testing.T) {
		dir := pausedFixture(t, backendFile, PauseNoLock)
		cfg, err := Load(FilePath(dir))
		require.NoError(t, err)
		assert.Equal(t, []PausedEntry{{Entry: "prod", Reason: PauseReasonNoLock}, {Entry: "staging", Reason: PauseReasonNoLock}}, PausedEntries(dir, cfg))
		assert.False(t, ResumeCacheWrites(dir, cfg))
		assert.True(t, cfg.Servers["prod"].Paused())
	})
	t.Run("its own change clears it, and only it, under the variable", func(t *testing.T) {
		dir := pausedFixture(t, backendFile, PauseLocked)
		t.Setenv(EnvConfigNoLock, "1")
		cfg, err := Load(FilePath(dir))
		require.NoError(t, err)
		e := cfg.Servers["prod"]
		e.CacheWritesPaused = PauseNoLock
		cfg.Servers["prod"] = e
		assert.True(t, ResumeCacheWrites(dir, cfg, "prod"))
		assert.False(t, cfg.Servers["prod"].Paused(), "the setter clears its own")
		assert.True(t, cfg.Servers["staging"].Paused(), "and without the lock, nothing else")
	})
	t.Run("its own change keeps it while a pin step would move readers", func(t *testing.T) {
		dir := pausedFixture(t, backendFile, PauseNoLock)
		seedTransitionFile(t, dir, true, transitionStep{Kind: transitionPin, Target: backendKeychain})
		cfg, err := Load(FilePath(dir))
		require.NoError(t, err)
		assert.False(t, ResumeCacheWrites(dir, cfg, "prod"))
		assert.True(t, cfg.Servers["prod"].Paused())
	})
	t.Run("a value this release does not know is cleared as a locked one", func(t *testing.T) {
		dir := pausedFixture(t, backendFile, "someday")
		cfg, err := Load(FilePath(dir))
		require.NoError(t, err)
		assert.Equal(t, PauseReasonNone, PausedEntries(dir, cfg)[0].Reason)
		assert.True(t, ResumeCacheWrites(dir, cfg))
		assert.False(t, cfg.Servers["prod"].Paused())
	})
}

// TestAChangeStartsWithTheIdAdvancedAndCacheWritesPaused asserts the one
// change every credential change's first write makes to its entry.
func TestAChangeStartsWithTheIdAdvancedAndCacheWritesPaused(t *testing.T) {
	for _, tc := range []struct {
		noLock string
		want   string
	}{{"", PauseLocked}, {"1", PauseNoLock}} {
		t.Setenv(EnvConfigNoLock, tc.noLock)
		e := Server{URL: "https://r.ex", CacheBindingID: "before"}
		require.NoError(t, StartCredentialChange(&e))
		assert.NotEqual(t, "before", e.CacheBindingID)
		assert.Len(t, e.CacheBindingID, randomIDLen)
		assert.Equal(t, tc.want, e.CacheWritesPaused)
	}
}

// TestThePluginsResumeTriesTheLockOnce asserts the exec plugin's resume:
// with the lock free it clears every pause the rule allows in one write
// and reports what stays; with the lock held by another process or
// inside this one it waits for nothing and writes nothing; under
// RANCHER_CLI_CONFIG_NO_LOCK it does not try at all.
func TestThePluginsResumeTriesTheLockOnce(t *testing.T) {
	counted := func(t *testing.T) (writes *[]string, tries *int) {
		t.Helper()
		var w []string
		var n int
		t.Cleanup(SetPersistWriteHookForTest(func(step string) error { w = append(w, step); return nil }))
		t.Cleanup(SetIdentityObserverForTest(func(op string) {
			if op == "configlocktry" {
				n++
			}
		}))
		return &w, &n
	}

	t.Run("the lock is free", func(t *testing.T) {
		dir := pausedFixture(t, backendFile, PauseLocked)
		writes, tries := counted(t)
		res, err := TryResumeCacheWrites(dir, "prod")
		require.NoError(t, err)
		assert.Equal(t, ResumeResult{Tried: true}, res)
		assert.Equal(t, 1, *tries)
		assert.Equal(t, []string{"resume"}, *writes, "one config write clears them all")
		assert.Equal(t, map[string]string{"prod": "", "staging": ""}, pausesOnDisk(t, dir))
	})

	t.Run("a pin step would move readers", func(t *testing.T) {
		dir := pausedFixture(t, backendFile, PauseLocked)
		seedTransitionFile(t, dir, true, transitionStep{Kind: transitionPin, Target: backendKeychain})
		writes, _ := counted(t)
		res, err := TryResumeCacheWrites(dir, "prod")
		require.NoError(t, err)
		assert.Equal(t, ResumeResult{Tried: true, Stays: PauseReasonPinStep}, res)
		assert.Empty(t, *writes)
	})

	t.Run("another process holds the lock", func(t *testing.T) {
		dir := pausedFixture(t, backendFile, PauseLocked)
		other := flock.New(LockPath(dir))
		ok, err := other.TryLock()
		require.NoError(t, err)
		require.True(t, ok)
		t.Cleanup(func() { _ = other.Unlock() })
		writes, tries := counted(t)
		start := time.Now()
		res, err := TryResumeCacheWrites(dir, "prod")
		require.NoError(t, err)
		assert.Less(t, time.Since(start), time.Second, "it never waits")
		assert.Equal(t, ResumeResult{}, res)
		assert.Equal(t, 1, *tries)
		assert.Empty(t, *writes)
		assert.Equal(t, PauseLocked, pausesOnDisk(t, dir)["prod"])
	})

	t.Run("a pause made without the lock", func(t *testing.T) {
		dir := pausedFixture(t, backendFile, PauseNoLock)
		writes, _ := counted(t)
		res, err := TryResumeCacheWrites(dir, "prod")
		require.NoError(t, err)
		assert.Equal(t, ResumeResult{Tried: true, Stays: PauseReasonNoLock}, res, "the plugin is not the change that made it")
		assert.Empty(t, *writes)
		assert.Equal(t, PauseNoLock, pausesOnDisk(t, dir)["prod"])
	})

	t.Run("this process already holds the semaphore", func(t *testing.T) {
		dir := pausedFixture(t, backendFile, PauseLocked)
		writes, _ := counted(t)
		configLockSem <- struct{}{}
		res, err := TryResumeCacheWrites(dir, "prod")
		<-configLockSem
		require.NoError(t, err)
		assert.Equal(t, ResumeResult{}, res, "the flock is free, but this process is already inside the lock")
		assert.Empty(t, *writes)
	})

	t.Run("this process holds the lock", func(t *testing.T) {
		dir := pausedFixture(t, backendFile, PauseLocked)
		writes, _ := counted(t)
		require.NoError(t, WithLock(t.Context(), dir, func() error {
			res, err := TryResumeCacheWrites(dir, "prod")
			require.NoError(t, err)
			assert.Equal(t, ResumeResult{}, res)
			return nil
		}))
		assert.Empty(t, *writes)
	})

	t.Run("the lock is skipped", func(t *testing.T) {
		dir := pausedFixture(t, backendFile, PauseLocked)
		t.Setenv(EnvConfigNoLock, "1")
		writes, tries := counted(t)
		res, err := TryResumeCacheWrites(dir, "prod")
		require.NoError(t, err)
		assert.Equal(t, ResumeResult{}, res)
		assert.Zero(t, *tries, "a free lock proves nothing without the flock, so it is not tried")
		assert.Empty(t, *writes)
	})

	t.Run("its write fails", func(t *testing.T) {
		dir := pausedFixture(t, backendFile, PauseLocked)
		t.Cleanup(SetPersistWriteHookForTest(func(string) error { return os.ErrPermission }))
		_, err := TryResumeCacheWrites(dir, "prod")
		require.ErrorIs(t, err, os.ErrPermission)
		assert.Equal(t, PauseLocked, pausesOnDisk(t, dir)["prod"], "and the pause is where it was")
	})
}
