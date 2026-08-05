package config

import (
	"context"
	"os"
	"path/filepath"
	"sync"
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

// configLockMu serializes same-process WithLock callers so goroutines in
// one process never contend on the flock.
var configLockMu sync.Mutex

// lockHeldHook is a test-only synchronization point, invoked (when
// non-nil) while BOTH the process mutex and the flock are held, before fn
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
// exclusive flock on <dir>/config.lock plus the process-level mutex.
// Every load→modify→write of config.yaml or credentials.yaml runs inside
// it, so the read that decides is the read that wins. NOT re-entrant — a
// nested WithLock call deadlocks on the process mutex by design (it is a
// bug, and a loud one).
//
// Lock ordering: the config lock is always OUTERMOST. Code holding the
// token-cache lock or the helper-records lock must never acquire it.
//
// RANCHER_CLI_CONFIG_NO_LOCK (any non-empty value) skips the flock —
// escape hatch for filesystems with broken advisory locking — while the
// process mutex still serializes in-process callers.
func WithLock(dir string, fn func() error) error {
	configLockMu.Lock()
	defer configLockMu.Unlock()
	if os.Getenv(EnvConfigNoLock) == "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		fl := flock.New(filepath.Join(dir, configLockName))
		ctx, cancel := context.WithTimeout(context.Background(), lockWaitTimeout)
		defer cancel()
		ok, lockErr := fl.TryLockContext(ctx, 100*time.Millisecond)
		if lockErr == nil && !ok {
			// TryLockContext can report plain failure; keep a wrappable
			// timeout identity for errors.Is.
			lockErr = context.DeadlineExceeded
		}
		if lockErr != nil {
			return clierr.New(clierr.CodeGeneric,
				"locking the configuration directory (another rancher process may be writing): "+lockErr.Error()).
				WithCause(lockErr).
				WithHint("set RANCHER_CLI_CONFIG_NO_LOCK=1 to skip the flock on filesystems with broken file locking")
		}
		defer func() { _ = fl.Unlock() }()
	}
	if lockHeldHook != nil {
		lockHeldHook()
	}
	return fn()
}
