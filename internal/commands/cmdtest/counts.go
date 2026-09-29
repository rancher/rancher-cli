package cmdtest

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
)

// Counts is what one command run cost, operation by operation: the
// observed file operations by name, the config lock, every other lock's
// flock, the transition lock, and keychain calls.
type Counts struct {
	Ops             map[string]int
	ConfigLocks     int
	Flocks          int
	TransitionLocks int
	Keychain        int
}

// CountOperations runs fn with every operation seam observed and returns
// what it cost. The seams are package-level, so the calling test must not
// run in parallel.
func CountOperations(t *testing.T, fn func()) Counts {
	t.Helper()
	c := Counts{Ops: map[string]int{}}
	restoreOps := config.SetIdentityObserverForTest(func(op string) { c.Ops[op]++ })
	defer restoreOps()
	config.SetKeyringObserverForTest(func(string) { c.Keychain++ })
	defer config.SetKeyringObserverForTest(nil)
	restoreLock := config.SetLockHookForTest(func() { c.ConfigLocks++ })
	defer restoreLock()
	restoreFlock := config.SetWaitForFlockHookForTest(func() { c.Flocks++ })
	defer restoreFlock()
	restoreTransition := config.SetTransitionLockHookForTest(func() { c.TransitionLocks++ })
	defer restoreTransition()
	fn()
	return c
}
