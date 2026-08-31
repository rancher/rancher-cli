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

// semWaitHook is a test-only synchronization point, invoked (when non-nil)
// immediately before a WithLock caller attempts to acquire the in-process
// semaphore. Lets a test wait for "this goroutine is about to contend for
// the semaphore" instead of sleeping. Nil in production.
var semWaitHook func()

// waitForFlockHook is a test-only synchronization point, invoked (when
// non-nil) immediately before a flock acquisition attempt begins polling.
// Shared by every waitForFlock caller (the config lock, the token-cache
// lock, the helper-records lock), so a test can wait for "this goroutine's
// flock wait has started" instead of sleeping. Nil in production.
var waitForFlockHook func()

// SetLockWaitTimeoutForTest shortens the lock wait for the current test
// run; call the returned restore in cleanup. Exported for the same reason
// as SetLockHookForTest: other packages' tests (internal/commands) pin
// lock-timeout behavior they cannot otherwise reach. Not for production.
func SetLockWaitTimeoutForTest(d time.Duration) (restore func()) {
	old := lockWaitTimeout
	lockWaitTimeout = d
	return func() { lockWaitTimeout = old }
}

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

// SetSemWaitHookForTest sets semWaitHook for the current test run; call the
// returned restore in cleanup.
//
// The calling test must not use t.Parallel(): the hook is a package-level
// seam shared by every WithLock call.
func SetSemWaitHookForTest(fn func()) (restore func()) {
	old := semWaitHook
	semWaitHook = fn
	return func() { semWaitHook = old }
}

// SetWaitForFlockHookForTest sets waitForFlockHook for the current test
// run; call the returned restore in cleanup.
//
// The calling test must not use t.Parallel(): the hook is a package-level
// seam shared by every waitForFlock call (the config lock, the token-cache
// lock, the helper-records lock).
func SetWaitForFlockHookForTest(fn func()) (restore func()) {
	old := waitForFlockHook
	waitForFlockHook = fn
	return func() { waitForFlockHook = old }
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

	if semWaitHook != nil {
		semWaitHook()
	}
	select {
	case configLockSem <- struct{}{}:
	case <-waitCtx.Done():
		return lockWaitError(waitCtx.Err(), lockWaitInProcess)
	}
	defer func() { <-configLockSem }()
	// Both select arms can be ready when the caller's context is already
	// canceled, and Go may pick the semaphore send. Re-check before ANY
	// filesystem work or the callback: a canceled command must never
	// mutate — the env opt-out path below has no later context check at
	// all, and even the flock path would create the config directory first.
	if err := waitCtx.Err(); err != nil {
		return lockWaitError(err, lockWaitInProcess)
	}

	if os.Getenv(EnvConfigNoLock) == "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		release, lockErr := waitForFlock(waitCtx, filepath.Join(dir, configLockName), 100*time.Millisecond)
		if lockErr != nil {
			return lockWaitError(lockErr, lockWaitCrossProcess)
		}
		defer release()
	}
	if lockHeldHook != nil {
		lockHeldHook()
	}
	return fn()
}

// lockWaitStage says which of the two waits timed out, so the error can
// point at the right suspect: an exhausted IN-PROCESS wait means this very
// process holds the lock — a concurrent goroutine or a nested WithLock call
// (a bug) — while a cross-process wait points at another rancher process.
type lockWaitStage int

const (
	lockWaitInProcess lockWaitStage = iota
	lockWaitCrossProcess
)

// lockWaitError classifies a failed lock wait: the caller's own
// cancellation (Ctrl-C) passes through as-is — it is not a lock problem —
// while a deadline or contention failure gets a stage-specific actionable
// message and the escape-hatch hint.
func lockWaitError(err error, stage lockWaitStage) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	msg := "locking the configuration directory (another rancher process may be writing): "
	if stage == lockWaitInProcess {
		msg = "waiting for the configuration lock held by this same process (a concurrent goroutine, or a nested WithLock call — a bug): "
	}
	return clierr.Retryable(clierr.CodeLockTimeout,
		msg+err.Error()+" — on a filesystem with broken file locking, skip the flock with RANCHER_CLI_CONFIG_NO_LOCK=1").
		WithCause(err).
		WithHint("export RANCHER_CLI_CONFIG_NO_LOCK=1")
}

// waitForFlock acquires the flock at path, polling every poll interval until
// ctx's deadline, and returns the release. One implementation behind every
// config-directory lock (the config lock, the token-cache lock, the
// helper-records lock) so the wait mechanics cannot drift; each caller keeps
// its own timeout policy and error wording. A (false, nil) result from the
// flock library must never let a caller proceed unlocked, so it is mapped to
// a wrappable timeout identity here.
func waitForFlock(ctx context.Context, path string, poll time.Duration) (release func(), err error) {
	if waitForFlockHook != nil {
		waitForFlockHook()
	}
	fl := flock.New(path)
	ok, lockErr := fl.TryLockContext(ctx, poll)
	if lockErr == nil && !ok {
		lockErr = context.DeadlineExceeded
	}
	if lockErr != nil {
		return nil, lockErr
	}
	return func() { _ = fl.Unlock() }, nil
}
