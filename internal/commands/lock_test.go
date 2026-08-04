package commands

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
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

// seedTwoServerConfig writes a config with servers "acme" and "acme2" (file
// credential store, currentContext "acme"). keyring.MockInit is called so
// drain operations never touch the real OS keychain.
func seedTwoServerConfig(t *testing.T) string {
	t.Helper()
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &config.Config{
		CredentialStore: "file",
		CurrentContext:  "acme",
		Servers: map[string]config.Server{
			"acme":  {URL: "https://acme.example"},
			"acme2": {URL: "https://acme2.example"},
		},
		Contexts: map[string]config.Context{
			"acme":  {Server: "acme"},
			"acme2": {Server: "acme2"},
		},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir
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

// runLogout runs `auth logout <name>`. Errors are reported via assert.
func runLogout(t *testing.T, dir, name string) {
	t.Helper()
	cmd := &logoutCmd{dir: dir}
	_, err := cmd.Execute(context.Background(), args{a: []string{name}}, ui.Discard())
	assert.NoError(t, err)
}

// runDeleteServer runs `config delete-server <name>` with the production drain.
// Errors are reported via assert.
func runDeleteServer(t *testing.T, dir, name string) {
	t.Helper()
	cmd := &configDeleteServerCmd{dir: dir, drain: config.DrainToken}
	_, err := cmd.Execute(context.Background(), args{a: []string{name}}, ui.Discard())
	assert.NoError(t, err)
}

// runSetContext runs `config set-context` with the given cluster and/or project
// on the current context. Errors are reported via assert.
func runSetContext(t *testing.T, dir, cluster, project string) {
	t.Helper()
	cmd := &contextSetCmd{dir: dir}
	a := args{s: map[string]string{}}
	if cluster != "" {
		a.s["cluster"] = cluster
	}
	if project != "" {
		a.s["project"] = project
	}
	_, err := cmd.Execute(context.Background(), a, ui.Discard())
	assert.NoError(t, err)
}

// runUseContext runs `config use-context <name>`. Errors are reported via assert.
func runUseContext(t *testing.T, dir, name string) {
	t.Helper()
	cmd := &contextUseCmd{dir: dir}
	_, err := cmd.Execute(context.Background(), args{a: []string{name}}, ui.Discard())
	assert.NoError(t, err)
}

// runUseServer runs `config use-server <name>`. Errors are reported via assert.
func runUseServer(t *testing.T, dir, name string) {
	t.Helper()
	cmd := &serverSwitchCmd{dir: dir}
	_, err := cmd.Execute(context.Background(), args{a: []string{name}}, ui.Discard())
	assert.NoError(t, err)
}

// runSetCompletion runs `config set-completion` with the given network and/or
// timeout flags. Errors are reported via assert.
func runSetCompletion(t *testing.T, dir, network, timeout string) {
	t.Helper()
	cmd := &setCompletionCmd{dir: dir}
	a := args{s: map[string]string{}}
	if network != "" {
		a.s["network"] = network
	}
	if timeout != "" {
		a.s["timeout"] = timeout
	}
	_, err := cmd.Execute(context.Background(), a, ui.Discard())
	assert.NoError(t, err)
}

func TestLogoutSerializedUnderConfigLock(t *testing.T) {
	dir := seedTwoServerConfig(t)
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "acme", URL: "https://acme.example"}, "tok-acme"))
	require.NoError(t, store.SetToken(config.ServerRef{Name: "acme2", URL: "https://acme2.example"}, "tok-acme2"))

	assertSerializedWithLoadUnderLock(t, dir,
		func() { runLogout(t, dir, "acme") },
		func() { writeSentinelServer(t, dir) },
		func() { runLogout(t, dir, "acme2") },
	)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers["sentinel"]
	assert.True(t, ok, "sentinel committed by mutateWhileHeld must survive both logouts")
	_, ok = cfg.Servers["acme"]
	assert.True(t, ok, "server config entry for acme must survive logout (only token is drained)")
	_, ok = cfg.Servers["acme2"]
	assert.True(t, ok, "server config entry for acme2 must survive logout (only token is drained)")
	_, err = store.GetToken(config.ServerRef{Name: "acme"})
	assert.ErrorIs(t, err, config.ErrNoToken, "acme token must be drained by logout")
	_, err = store.GetToken(config.ServerRef{Name: "acme2"})
	assert.ErrorIs(t, err, config.ErrNoToken, "acme2 token must be drained by logout")
}

func TestDeleteServerSerializedUnderConfigLock(t *testing.T) {
	dir := seedTwoServerConfig(t)
	assertSerializedWithLoadUnderLock(t, dir,
		func() { runDeleteServer(t, dir, "acme") },
		func() { writeSentinelServer(t, dir) },
		func() { runDeleteServer(t, dir, "acme2") },
	)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers["sentinel"]
	assert.True(t, ok, "sentinel committed by mutateWhileHeld must survive both deletes")
	_, ok = cfg.Servers["acme"]
	assert.False(t, ok, "acme must be absent after delete-server")
	_, ok = cfg.Servers["acme2"]
	assert.False(t, ok, "acme2 must be absent after delete-server")
}

func TestSetContextSerializedUnderConfigLock(t *testing.T) {
	dir := seedTwoServerConfig(t)
	// first sets cluster=c-1; second sets project=p-1. Because the second
	// command loads the config written by the first (serialization), both
	// fields must be present in the final current context.
	assertSerializedWithLoadUnderLock(t, dir,
		func() { runSetContext(t, dir, "c-1", "") },
		func() { writeSentinelServer(t, dir) },
		func() { runSetContext(t, dir, "", "p-1") },
	)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers["sentinel"]
	assert.True(t, ok, "sentinel committed by mutateWhileHeld must survive")
	ctx := cfg.Contexts[cfg.CurrentContext]
	assert.Equal(t, "c-1", ctx.Cluster, "cluster set by first command must survive second command's write")
	assert.Equal(t, "p-1", ctx.Project, "project set by second command must be present")
}

func TestUseContextSerializedUnderConfigLock(t *testing.T) {
	dir := seedTwoServerConfig(t)
	// first switches to acme2, second switches to acme; final must be acme
	// (order proof: the second's write always wins).
	assertSerializedWithLoadUnderLock(t, dir,
		func() { runUseContext(t, dir, "acme2") },
		func() { writeSentinelServer(t, dir) },
		func() { runUseContext(t, dir, "acme") },
	)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers["sentinel"]
	assert.True(t, ok, "sentinel committed by mutateWhileHeld must survive")
	assert.Equal(t, "acme", cfg.CurrentContext, "current context must be the second command's target")
}

func TestUseServerSerializedUnderConfigLock(t *testing.T) {
	dir := seedTwoServerConfig(t)
	// first switches to acme, second switches to acme2; final current server
	// must be acme2 (order proof: the second's write always wins).
	assertSerializedWithLoadUnderLock(t, dir,
		func() { runUseServer(t, dir, "acme") },
		func() { writeSentinelServer(t, dir) },
		func() { runUseServer(t, dir, "acme2") },
	)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers["sentinel"]
	assert.True(t, ok, "sentinel committed by mutateWhileHeld must survive")
	assert.Equal(t, "acme2", cfg.CurrentContext, "current context must be the second command's target")
}

func TestSetCompletionSerializedUnderConfigLock(t *testing.T) {
	dir := seedTwoServerConfig(t)
	// first enables network, second sets timeout; both fields must survive
	// (non-clobbering proof: second loads first's write and preserves network).
	assertSerializedWithLoadUnderLock(t, dir,
		func() { runSetCompletion(t, dir, "true", "") },
		func() { writeSentinelServer(t, dir) },
		func() { runSetCompletion(t, dir, "", "1s") },
	)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers["sentinel"]
	assert.True(t, ok, "sentinel committed by mutateWhileHeld must survive")
	assert.True(t, cfg.Completion.Network, "network enabled by first command must survive second command's write")
	assert.Equal(t, "1s", cfg.Completion.Timeout, "timeout set by second command must be present")
}
