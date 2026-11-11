package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// PluginLoginLockPath is the per-server flock the plugin-created login
// holds across its interactive wait: <dir>/exec-login-<sha256[:16]>.lock,
// keyed on the canonical server URL so every spelling of one server
// serializes on one file and the name never carries URL characters.
func PluginLoginLockPath(dir, canonicalURL string) string {
	sum := sha256.Sum256([]byte(canonicalURL))
	return filepath.Join(dir, "exec-login-"+hex.EncodeToString(sum[:8])+".lock")
}

// WithPluginLoginLock runs fn while holding the plugin-login flock for
// canonicalURL. It is the ONE lock taken outside the config lock: the
// plugin login holds it across acquisition, the cluster-token create, and
// the persistence primitive (which takes the config lock inside), and
// releases it before the retirement pass. Code holding the config lock
// must never call this.
//
// Waiters block for at most lockWaitTimeout, or until ctx ends, whichever
// comes first; kubectl supplies no deadline, so the bound is what stops a
// waiter behind an abandoned interactive login whose process is alive. On
// expiry the error is LOCK_TIMEOUT, retryable, naming the contention, and
// deliberately without the RANCHER_CLI_CONFIG_NO_LOCK hint: bypassing this
// lock would start the second interactive login it exists to prevent. The
// variable itself is still honored for filesystems with broken locking,
// as WithLock honors it. The flock releases with the process; no stale-lock
// cleanup is needed.
func WithPluginLoginLock(ctx context.Context, dir, canonicalURL string, fn func() error) error {
	if os.Getenv(EnvConfigNoLock) != "" {
		return fn()
	}
	if err := EnsureDir(dir); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, lockWaitTimeout)
	defer cancel()
	release, err := waitForFlock(waitCtx, PluginLoginLockPath(dir, canonicalURL), 100*time.Millisecond)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		return clierr.Retryable(clierr.CodeLockTimeout,
			"another rancher process is logging in to "+ServerHost(canonicalURL)+
				"; waited "+lockWaitTimeout.String()+" for it to finish: "+err.Error()+
				"; retry once it completes").
			WithCause(err)
	}
	defer release()
	return fn()
}
