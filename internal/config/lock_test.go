package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithLockRunsFnAndCreatesLockFile(t *testing.T) {
	dir := t.TempDir()
	ran := false
	err := WithLock(t.Context(), dir, func() error {
		ran = true
		return nil
	})
	require.NoError(t, err)
	assert.True(t, ran)
	// lock file should exist after the call
	assert.FileExists(t, filepath.Join(dir, configLockName))
}

func TestWithLockPropagatesErrorAndReleases(t *testing.T) {
	dir := t.TempDir()
	sentinel := errors.New("sentinel error")
	err := WithLock(t.Context(), dir, func() error {
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)

	// the lock must have been released - a second WithLock must succeed
	err2 := WithLock(t.Context(), dir, func() error { return nil })
	require.NoError(t, err2)
}

func TestWithLockSerializesGoroutines(t *testing.T) {
	dir := t.TempDir()

	// A parks in the hook (before its fn runs); B's fn must not run until
	// A releases.
	aInHook := make(chan struct{})
	aCanLeave := make(chan struct{})
	aCounter := 0
	bCounter := 0

	restore := SetLockHookForTest(func() {
		close(aInHook)
		<-aCanLeave
	})
	defer restore()

	// Launch A.
	aErr := make(chan error, 1)
	go func() {
		aErr <- WithLock(t.Context(), dir, func() error {
			aCounter++
			return nil
		})
	}()

	// Wait until A is parked in the hook (mutex held, fn not yet called).
	select {
	case <-aInHook:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for A to enter hook")
	}

	// Remove the hook so B doesn't also block on it once it acquires.
	restore()

	// Launch B - it blocks on the process mutex (A still holds it via the
	// hook). The semaphore wait hook fires as B reaches the mutex, so the
	// test waits for that fact instead of a window of real time.
	bAtSem := make(chan struct{})
	var atSemOnce sync.Once
	restoreSem := SetSemWaitHookForTest(func() { atSemOnce.Do(func() { close(bAtSem) }) })
	defer restoreSem()
	bDone := make(chan struct{})
	go func() {
		_ = WithLock(t.Context(), dir, func() error {
			bCounter++
			close(bDone)
			return nil
		})
	}()

	// Wait (bounded) for B to reach the mutex.
	select {
	case <-bAtSem:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for B to reach the process mutex")
	}
	restoreSem()

	// A still holds the lock and B is at the mutex: bDone cannot have
	// fired. If it has, the serialization is broken.
	select {
	case <-bDone:
		t.Fatal("B completed before A released the lock")
	default:
	}
	assert.Equal(t, 0, aCounter, "A's fn ran before hook returned")
	assert.Equal(t, 0, bCounter, "B ran before A released")

	// Release A.
	close(aCanLeave)

	select {
	case <-bDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for B to complete")
	}
	require.NoError(t, <-aErr)
	assert.Equal(t, 1, aCounter)
	assert.Equal(t, 1, bCounter)
}

func TestWithLockTimeoutIsActionable(t *testing.T) {
	dir := t.TempDir()

	old := lockWaitTimeout
	lockWaitTimeout = 200 * time.Millisecond
	defer func() { lockWaitTimeout = old }()

	// Hold the flock externally so WithLock cannot acquire it.
	fl := flock.New(filepath.Join(dir, configLockName))
	locked, err := fl.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	defer func() { _ = fl.Unlock() }()

	err = WithLock(t.Context(), dir, func() error { return nil })
	require.Error(t, err)
	require.ErrorContains(t, err, "another rancher process")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeLockTimeout, ce.Code)
	assert.True(t, ce.Retryable, "a lock timeout is transient - the holder finishes")
	assert.Contains(t, ce.Hint, "RANCHER_CLI_CONFIG_NO_LOCK")
}

func TestWithLockEnvOptOutSkipsFlock(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvConfigNoLock, "1")

	// Hold the flock externally; WithLock must still run fn (flock skipped).
	fl := flock.New(filepath.Join(dir, configLockName))
	locked, err := fl.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	defer func() { _ = fl.Unlock() }()

	ran := false
	err = WithLock(t.Context(), dir, func() error {
		ran = true
		return nil
	})
	require.NoError(t, err)
	assert.True(t, ran)
}

func TestSetLockHookForTestFiresWhileHeld(t *testing.T) {
	dir := t.TempDir()
	fired := false
	restore := SetLockHookForTest(func() {
		fired = true
	})
	defer restore()

	err := WithLock(t.Context(), dir, func() error { return nil })
	require.NoError(t, err)
	assert.True(t, fired)
}

// TestWithLockCancellationUnblocksWaits asserts that Ctrl-C
// (context cancellation) must abort BOTH waits promptly - the in-process
// semaphore wait and the cross-process flock wait - and surface as the
// caller's own cancellation, not as a lock timeout.
// No t.Parallel(): holds the package-level semaphore and the flock.
func TestWithLockCancellationUnblocksWaits(t *testing.T) {
	t.Run("cancel while waiting on the flock", func(t *testing.T) {
		dir := t.TempDir()
		fl := flock.New(filepath.Join(dir, configLockName))
		locked, err := fl.TryLock()
		require.NoError(t, err)
		require.True(t, locked)
		defer func() { _ = fl.Unlock() }()

		ctx, cancel := context.WithCancel(t.Context())
		flockWaiting := make(chan struct{})
		restore := SetWaitForFlockHookForTest(func() { close(flockWaiting) })
		defer restore()
		done := make(chan error, 1)
		go func() { done <- WithLock(ctx, dir, func() error { return nil }) }()
		<-flockWaiting // let the waiter reach TryLockContext
		cancel()
		select {
		case werr := <-done:
			require.Error(t, werr)
			require.ErrorIs(t, werr, context.Canceled, "cancellation must pass through, not be dressed as a lock timeout")
		case <-time.After(5 * time.Second):
			t.Fatal("cancellation did not unblock the flock wait")
		}
	})

	t.Run("cancel while waiting on the in-process semaphore", func(t *testing.T) {
		dir := t.TempDir()
		holderParked := make(chan struct{})
		holderRelease := make(chan struct{})
		holderDone := make(chan error, 1)
		var once sync.Once
		restore := SetLockHookForTest(func() {
			once.Do(func() { close(holderParked) })
			<-holderRelease
		})
		go func() { holderDone <- WithLock(t.Context(), dir, func() error { return nil }) }()

		// Wait until the holder is provably inside the hook (semaphore held);
		// the channel gives the happens-before edge that makes removing the
		// hook race-free. The waiter below must not park in it.
		select {
		case <-holderParked:
		case <-time.After(10 * time.Second):
			t.Fatal("holder never reached the lock hook")
		}
		restore()

		ctx, cancel := context.WithCancel(t.Context())
		semWaiting := make(chan struct{})
		restoreSemHook := SetSemWaitHookForTest(func() { close(semWaiting) })
		defer restoreSemHook()
		waiterDone := make(chan error, 1)
		go func() { waiterDone <- WithLock(ctx, dir, func() error { return nil }) }()
		<-semWaiting // let the waiter reach the semaphore acquisition
		cancel()
		select {
		case werr := <-waiterDone:
			require.Error(t, werr)
			require.ErrorIs(t, werr, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("cancellation did not unblock the semaphore wait")
		}

		close(holderRelease)
		require.NoError(t, <-holderDone)
	})
}

// TestWithLockPreCanceledContextNeverRunsFn asserts that when the
// caller's context is already canceled, both acquisition select arms are
// ready and Go may pick the semaphore send - fn must still never run, with
// or without the flock (the env opt-out path has no later context check),
// and no filesystem work may happen first.
// No t.Parallel(): t.Setenv in the second case.
func TestWithLockPreCanceledContextNeverRunsFn(t *testing.T) {
	run := func(t *testing.T) {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "never-created")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		ran := false
		// The two-ready-arms choice is random: iterate so a lucky Done pick
		// cannot hide the bug.
		for range 50 {
			err := WithLock(ctx, dir, func() error { ran = true; return nil })
			require.Error(t, err)
			require.ErrorIs(t, err, context.Canceled)
		}
		assert.False(t, ran, "fn must never run under a canceled context")
		_, statErr := os.Stat(dir)
		assert.True(t, os.IsNotExist(statErr), "no filesystem work under a canceled context")
	}
	t.Run("flock path", run)
	t.Run("env opt-out path", func(t *testing.T) {
		t.Setenv(EnvConfigNoLock, "1")
		run(t)
	})
}
