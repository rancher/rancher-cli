package config

import (
	"context"
	"errors"
	"path/filepath"
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
	err := WithLock(dir, func() error {
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
	err := WithLock(dir, func() error {
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)

	// the lock must have been released — a second WithLock must succeed
	err2 := WithLock(dir, func() error { return nil })
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
		aErr <- WithLock(dir, func() error {
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

	// Launch B — it closes bStarted immediately before calling WithLock,
	// then blocks on the process mutex (A still holds it via the hook).
	bStarted := make(chan struct{})
	bDone := make(chan struct{})
	go func() {
		close(bStarted)
		_ = WithLock(dir, func() error {
			bCounter++
			close(bDone)
			return nil
		})
	}()

	// Wait (bounded) for B to reach its WithLock call.
	select {
	case <-bStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for B to start")
	}

	// A still holds the lock. bDone must NOT fire — prove B is blocked, not
	// just unscheduled. If it fires here the serialization is broken.
	select {
	case <-bDone:
		t.Fatal("B completed before A released the lock")
	case <-time.After(100 * time.Millisecond):
		// expected: B is blocked on the mutex
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

	err = WithLock(dir, func() error { return nil })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "another rancher process")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
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
	err = WithLock(dir, func() error {
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

	err := WithLock(dir, func() error { return nil })
	require.NoError(t, err)
	assert.True(t, fired)
}
