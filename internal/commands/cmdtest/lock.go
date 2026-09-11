package cmdtest

import (
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/config"
)

// AssertSerializedWithLoadUnderLock proves two properties for a wrapped
// command: (1) its deciding config load happens INSIDE the lock - a
// change committed while the command was waiting is visible to it; and
// (2) a second locked operation cannot start until the first finishes.
func AssertSerializedWithLoadUnderLock(t *testing.T, dir string, first func(), mutateWhileHeld func(), second func()) {
	t.Helper()
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	restore := config.SetLockHookForTest(func() {
		enteredOnce.Do(func() { close(entered) }) // first holder only
		<-release
	})
	defer restore()
	// Guard release on all exit paths so a t.Fatal cannot leave the first
	// goroutine parked holding the process mutex and hang the whole package.
	t.Cleanup(closeRelease)

	firstDone := make(chan struct{})
	go func() { defer close(firstDone); first() }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first command never acquired the config lock")
	}
	// The first command holds the lock and has NOT loaded yet (the hook
	// fires before fn): a mutation committed now must be visible to it.
	mutateWhileHeld()

	// The second command blocks on the config lock's process mutex; the
	// semaphore wait hook fires as it gets there, so the test waits for
	// that fact instead of a window of real time.
	secondAtSem := make(chan struct{})
	var atSemOnce sync.Once
	restoreSem := config.SetSemWaitHookForTest(func() { atSemOnce.Do(func() { close(secondAtSem) }) })
	t.Cleanup(restoreSem)
	secondDone := make(chan struct{})
	go func() { defer close(secondDone); second() }()
	select {
	case <-secondAtSem:
	case <-time.After(10 * time.Second):
		t.Fatal("second command never reached the config lock's process mutex")
	}
	restoreSem()
	select {
	case <-secondDone:
		t.Fatal("second command completed while the first held the lock")
	default:
	}
	closeRelease()
	<-firstDone
	<-secondDone
}

// WriteSentinelServer appends a server entry named "sentinel" to the
// config file in dir using os.WriteFile of pre-marshaled bytes - never
// through WithLock (deadlock) or cfg.Write (same risk while a lock hook
// is parked).
func WriteSentinelServer(t *testing.T, dir string) {
	t.Helper()
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.Servers["sentinel"] = config.Server{URL: "https://sentinel.example"}
	b, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(config.FilePath(dir), b, 0600))
}

// SeedTwoServerConfig writes a config with servers "acme" and "acme2" (file
// credential store, currentServer "acme"). keyring.MockInit is called so
// delete operations never touch the real OS keychain.
func SeedTwoServerConfig(t *testing.T) string {
	t.Helper()
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &config.Config{
		CredentialStore: "file",
		CurrentServer:   "acme",
		Servers: map[string]config.Server{
			"acme":  {URL: "https://acme.example"},
			"acme2": {URL: "https://acme2.example"},
		},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir
}
