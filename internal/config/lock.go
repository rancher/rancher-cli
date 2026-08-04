package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

const configLockName = "config.lock"

// lockWaitTimeout bounds how long WithLock waits for another process to
// release the config lock. A var so tests shorten it.
var lockWaitTimeout = 10 * time.Second

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
			return fmt.Errorf("locking the configuration directory (another rancher process may be writing): %w", lockErr)
		}
		defer func() { _ = fl.Unlock() }()
	}
	if lockHeldHook != nil {
		lockHeldHook()
	}
	return fn()
}
