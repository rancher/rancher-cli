package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	"github.com/rancher/rancher-cli/internal/clierr"
)

const configLockName = "config.lock"

// lockWaitTimeout bounds how long WithLock waits for another process to
// release the config lock. A var so tests shorten it.
//
// 60 s exceeds the worst expected single hold: a credential-helper call inside
// a locked span is bounded at execHelperTimeout (30 s), but logout --all
// multiplies helper calls by server count, so a waiter may still time out in
// that pathological case. RANCHER_CLI_CONFIG_NO_LOCK is the escape hatch when
// that occurs.
var lockWaitTimeout = 60 * time.Second

// configLockSem serializes same-process WithLock callers (so goroutines in
// one process never contend on the flock) while staying cancellable: a
// channel semaphore can be waited on in a select with the caller's
// context, which a sync.Mutex cannot.
var configLockSem = make(chan struct{}, 1)

// lockHeldHook is a test-only synchronization point, invoked (when
// non-nil) while BOTH the process semaphore and the flock are held, before fn
// runs. Nil in production. Set via SetLockHookForTest.
var lockHeldHook func()

// SetLockHookForTest sets the config-lock hook for the current test run;
// call the returned restore in cleanup. Exported from non-test code so
// other packages' tests (internal/commands) can pin lock/load orderings.
//
// The calling test must not use t.Parallel(): the hook is a package-level
// seam shared by every WithLock call.
func SetLockHookForTest(fn func()) (restore func()) {
	old := lockHeldHook
	lockHeldHook = fn
	return func() { lockHeldHook = old }
}

// WithLock runs fn while holding the config-directory write lock: an
// exclusive flock on <dir>/config.lock plus the process-level semaphore.
// Every load→modify→write of config.yaml or credentials.yaml runs inside
// it, so the read that decides is the read that wins. NOT re-entrant — a
// nested WithLock call deadlocks by design until the wait deadline (it is
// a bug, and a loud one).
//
// ctx is the command's context: cancellation (Ctrl-C) aborts BOTH waits —
// the in-process semaphore and the cross-process flock — immediately; the
// lockWaitTimeout deadline covers the two waits together, not each.
//
// Lock ordering: the config lock is always OUTERMOST. Code holding the
// token-cache lock or the helper-records lock must never acquire it.
//
// RANCHER_CLI_CONFIG_NO_LOCK (any non-empty value) skips the flock —
// escape hatch for filesystems with broken advisory locking — while the
// process semaphore still serializes in-process callers.
func WithLock(ctx context.Context, dir string, fn func() error) error {
	waitCtx, cancel := context.WithTimeout(ctx, lockWaitTimeout)
	defer cancel()

	select {
	case configLockSem <- struct{}{}:
	case <-waitCtx.Done():
		return lockWaitError(waitCtx.Err())
	}
	defer func() { <-configLockSem }()
	// Both select arms can be ready when the caller's context is already
	// canceled, and Go may pick the semaphore send. Re-check before ANY
	// filesystem work or the callback: a canceled command must never
	// mutate — the env opt-out path below has no later context check at
	// all, and even the flock path would create the config directory first.
	if err := waitCtx.Err(); err != nil {
		return lockWaitError(err)
	}

	if os.Getenv(EnvConfigNoLock) == "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		fl := flock.New(filepath.Join(dir, configLockName))
		ok, lockErr := fl.TryLockContext(waitCtx, 100*time.Millisecond)
		if lockErr == nil && !ok {
			// TryLockContext can report plain failure; keep a wrappable
			// timeout identity for errors.Is.
			lockErr = context.DeadlineExceeded
		}
		if lockErr != nil {
			return lockWaitError(lockErr)
		}
		defer func() { _ = fl.Unlock() }()
	}
	if lockHeldHook != nil {
		lockHeldHook()
	}
	return fn()
}

// lockWaitError classifies a failed lock wait: the caller's own
// cancellation (Ctrl-C) passes through as-is — it is not a lock problem —
// while a deadline or contention failure gets the actionable message and
// the escape-hatch hint.
func lockWaitError(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	return clierr.New(clierr.CodeGeneric,
		"locking the configuration directory (another rancher process may be writing): "+err.Error()).
		WithCause(err).
		WithHint("set RANCHER_CLI_CONFIG_NO_LOCK=1 to skip the flock on filesystems with broken file locking")
}
