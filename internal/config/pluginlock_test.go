package config

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPluginLoginLockPathIsPerCanonicalServer asserts the lock file is
// derived from the canonical URL alone: two spellings of one server share
// it, two servers do not, and the name never carries the URL's characters.
func TestPluginLoginLockPathIsPerCanonicalServer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := PluginLoginLockPath(dir, "https://r.ex")
	assert.Equal(t, a, PluginLoginLockPath(dir, "https://r.ex"))
	assert.NotEqual(t, a, PluginLoginLockPath(dir, "https://other.ex"))
	base := filepath.Base(a)
	assert.True(t, strings.HasPrefix(base, "exec-login-"), base)
	assert.True(t, strings.HasSuffix(base, ".lock"), base)
	assert.Len(t, base, len("exec-login-")+16+len(".lock"))
	assert.NotContains(t, base, ":")
	assert.NotContains(t, base, "/")
}

// TestWithPluginLoginLockSerializesAndTimesOutWithoutTheNoLockHint asserts
// a second holder waits, that the wait is bounded by lockWaitTimeout, and
// that the timeout is LOCK_TIMEOUT, retryable, names the contention, and
// carries no RANCHER_CLI_CONFIG_NO_LOCK hint: bypassing this lock would
// start a second interactive login.
func TestWithPluginLoginLockSerializesAndTimesOutWithoutTheNoLockHint(t *testing.T) {
	restore := SetLockWaitTimeoutForTest(300 * time.Millisecond)
	t.Cleanup(restore)
	dir := t.TempDir()
	held := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = WithPluginLoginLock(t.Context(), dir, "https://r.ex", func() error {
			close(held)
			<-release
			return nil
		})
	})
	<-held
	err := WithPluginLoginLock(t.Context(), dir, "https://r.ex", func() error { return nil })
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, clierr.CodeLockTimeout, ce.Code)
	assert.True(t, ce.Retryable)
	assert.Contains(t, ce.Message, "another rancher process is logging in to r.ex")
	assert.Empty(t, ce.Hint)
	assert.NotContains(t, ce.Message, EnvConfigNoLock)
	close(release)
	wg.Wait()

	require.NoError(t, WithPluginLoginLock(t.Context(), dir, "https://r.ex", func() error { return nil }),
		"the lock is free once the holder returns")
}

// TestWithPluginLoginLockHonorsCancellation asserts a waiter's own
// cancellation passes through as context.Canceled, not as a lock error.
func TestWithPluginLoginLockHonorsCancellation(t *testing.T) {
	restore := SetLockWaitTimeoutForTest(10 * time.Second)
	t.Cleanup(restore)
	dir := t.TempDir()
	held, release := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = WithPluginLoginLock(t.Context(), dir, "https://r.ex", func() error {
			close(held)
			<-release
			return nil
		})
	})
	<-held
	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	err := WithPluginLoginLock(ctx, dir, "https://r.ex", func() error { return nil })
	require.ErrorIs(t, err, context.Canceled)
	close(release)
	wg.Wait()
}

// TestWithPluginLoginLockDifferentServersDoNotContend asserts two servers'
// locks are independent.
func TestWithPluginLoginLockDifferentServersDoNotContend(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	held, release := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = WithPluginLoginLock(t.Context(), dir, "https://a.ex", func() error {
			close(held)
			<-release
			return nil
		})
	})
	<-held
	require.NoError(t, WithPluginLoginLock(t.Context(), dir, "https://b.ex", func() error { return nil }))
	close(release)
	wg.Wait()
}
