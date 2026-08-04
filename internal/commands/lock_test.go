package commands

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertSerializedWithLoadUnderLock proves two properties for a wrapped
// command: (1) its deciding config load happens INSIDE the lock — a
// change committed while the command was waiting is visible to it; and
// (2) a second locked operation cannot start until the first finishes.
func assertSerializedWithLoadUnderLock(t *testing.T, dir string, first func(), mutateWhileHeld func(), second func()) {
	t.Helper()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	restore := config.SetLockHookForTest(func() {
		once.Do(func() { close(entered) }) // first holder only
		<-release
	})
	defer restore()

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

	secondDone := make(chan struct{})
	go func() { defer close(secondDone); second() }()
	select {
	case <-secondDone:
		t.Fatal("second command completed while the first held the lock")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	<-firstDone
	<-secondDone
}

// writeSentinelServer appends a server entry named "sentinel" to the
// config file in dir using os.WriteFile of pre-marshaled bytes — never
// through WithLock (deadlock) or cfg.Write (same risk while a lock hook
// is parked).
func writeSentinelServer(t *testing.T, dir string) {
	t.Helper()
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.Servers["sentinel"] = config.Server{URL: "https://sentinel.example"}
	b, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(config.FilePath(dir), b, 0600))
}

// runLogin runs `auth login` for the given server/name with --token-stdin
// using an in-memory file store. Errors are reported via assert (safe from
// goroutines).
func runLogin(t *testing.T, dir, name, server string) {
	t.Helper()
	store := config.NewFileStore(dir)
	cmd := &loginCmd{
		dir:            dir,
		newStore:       func(string, string) (config.CredStore, error) { return store, nil },
		rancherVersion: stubRancherVersion,
	}
	_, err := cmd.Execute(context.Background(),
		args{s: map[string]string{"server": server, "name": name}, b: map[string]bool{"token-stdin": true}},
		stdinPrompter("id:sec\n"))
	assert.NoError(t, err)
}
