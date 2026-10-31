package configcmd

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
)

func newUpdateServerCmd(dir string) *configUpdateServerCmd {
	return &configUpdateServerCmd{dir: dir}
}

func addServerForUpdateTests(t *testing.T, dir, name, url string) {
	t.Helper()
	c := newAddServerCmd(dir)
	_, err := c.Execute(context.Background(), cmdtest.Input{Str: map[string]string{"url": url}, Pos: []string{name}}, nil)
	require.NoError(t, err)
}

// TestUpdateServerWriteFailureAfterPurgeNamesClearedCredentials: purge-then-
// write is the safe ordering (a purge failure never leaves config.yaml
// pointed at the new, credential-less address), but the reverse - a
// successful purge followed by a write failure - must say so plainly: the
// entry still names the OLD URL, but its token is already gone, so a
// generic write-error message would look like nothing happened.
func TestUpdateServerWriteFailureAfterPurgeNamesClearedCredentials(t *testing.T) {
	dir, store := cmdtest.FilePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://old.example.com")
	require.NoError(t, store.SetToken(config.ServerRef{Name: "qa1", URL: "https://old.example.com"}, "tok"))

	// Fail the entry write that follows the purge, and only that one: the
	// cache binding id advances in a write of its own before the purge,
	// and failing that one instead would stop the command before any
	// credential was touched, which is the other test below.
	restore := config.SetPersistWriteHookForTest(func(step string) error {
		if step == "update" {
			return errors.New("disk full")
		}
		return nil
	})
	t.Cleanup(restore)

	c := newUpdateServerCmd(dir)
	_, err := c.Execute(context.Background(), cmdtest.Input{Str: map[string]string{"url": "https://new.example.com"}, Pos: []string{"qa1"}}, nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "credentials", "the write failure must name the credentials as already cleared")
	require.ErrorContains(t, err, `"qa1"`)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.Rancher+" auth login 'qa1'", ce.Hint)

	_, terr := store.GetToken(config.ServerRef{Name: "qa1", URL: "https://old.example.com"})
	require.ErrorIs(t, terr, config.ErrNoToken, "the purge that ran before the failed write is not undone")
	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.Equal(t, config.PauseLocked, cfg.Servers["qa1"].CacheWritesPaused, "the pause stays for the next cache miss to clear")
}

// TestUpdateServerConflictNeverPurgesCredentials exercises the CONFLICT path
// at the command level (not just commitUpdate directly): the injected
// tlsPreflight rotates the entry's CA file's bytes between
// register.PrepareUpdateSource's read and the commit's re-check - a real command
// Execute() call reaching a genuine two-phase conflict without any actual
// concurrency. Purge only ever runs after register.ValidateUpdate succeeds, so a
// conflict here must leave the stored token and config.yaml untouched.
func TestUpdateServerConflictNeverPurgesCredentials(t *testing.T) {
	path := cmdtest.WriteCAFile(t, cmdtest.CAPem1)
	dir, store := cmdtest.FilePolicyDir(t)
	addCmd := &configAddServerCmd{dir: dir, tlsPreflight: func(context.Context, string, string) (string, error) { return "", nil }}
	_, err := addCmd.Execute(context.Background(),
		cmdtest.Input{Str: map[string]string{"url": "https://old.example.com", "cacert": path}, Pos: []string{"qa1"}}, nil)
	require.NoError(t, err)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "qa1", URL: "https://old.example.com"}, "tok"))

	c := &configUpdateServerCmd{dir: dir, tlsPreflight: func(context.Context, string, string) (string, error) {
		require.NoError(t, os.WriteFile(path, []byte(cmdtest.CAPem2), 0o600))
		return "", nil
	}}
	_, err = c.Execute(context.Background(), cmdtest.Input{Str: map[string]string{"url": "https://new.example.com"}, Pos: []string{"qa1"}}, nil)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)

	tok, terr := store.GetToken(config.ServerRef{Name: "qa1", URL: "https://old.example.com"})
	require.NoError(t, terr, "conflict must never purge the old URL's credential")
	assert.Equal(t, "tok", tok)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "https://old.example.com", cfg.Servers["qa1"].URL, "conflict must never write config.yaml")
}

func TestUpdateServerURLChangeClearsCredentials(t *testing.T) {
	dir, store := cmdtest.FilePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://old.example.com")
	require.NoError(t, store.SetToken(config.ServerRef{Name: "qa1", URL: "https://old.example.com"}, "tok"))

	c := newUpdateServerCmd(dir)
	res, err := c.Execute(context.Background(), cmdtest.Input{Str: map[string]string{"url": "https://new.example.com"}, Pos: []string{"qa1"}}, nil)
	require.NoError(t, err)
	data := res.Data.(map[string]any)
	assert.Equal(t, []string{"url"}, data["changed"])
	assert.Equal(t, true, data["credentialsCleared"])
	// Same single-server shape as config delete-server (map keyed
	// "backend"), not a flat list - both report the local login delete's
	// output and should look alike on the wire.
	assert.Equal(t, map[string][]string{"backend": {"file"}}, data["deletedFrom"])
	assert.Equal(t, "server qa1 updated\n"+
		"  url: https://new.example.com (changed)\n"+
		"  credentials cleared - log in again: "+clierr.Rancher+" auth login 'qa1'\n"+
		"  tokens on https://old.example.com:\n"+
		"    not revoked: the pasted login token, which is not the CLI's to revoke\n", res.Text)

	_, gerr := store.GetToken(config.ServerRef{Name: "qa1", URL: "https://new.example.com"})
	require.ErrorIs(t, gerr, config.ErrNoToken)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "https://new.example.com", cfg.Servers["qa1"].URL)
}

func TestUpdateServerCAOnlyKeepsCredentials(t *testing.T) {
	dir, store := cmdtest.FilePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://qa1.example.com")
	require.NoError(t, store.SetToken(config.ServerRef{Name: "qa1", URL: "https://qa1.example.com"}, "tok"))

	c := newUpdateServerCmd(dir)
	res, err := c.Execute(context.Background(), cmdtest.Input{Str: map[string]string{"cacert": cmdtest.CAPem1}, Pos: []string{"qa1"}}, nil)
	require.NoError(t, err)
	data := res.Data.(map[string]any)
	assert.Equal(t, []string{"ca"}, data["changed"])
	assert.Equal(t, false, data["credentialsCleared"])
	assert.Equal(t, "server qa1 updated\n  ca: pinned (changed)\n", res.Text)

	tok, gerr := store.GetToken(config.ServerRef{Name: "qa1", URL: "https://qa1.example.com"})
	require.NoError(t, gerr)
	assert.Equal(t, "tok", tok)
}

func TestUpdateServerClearCacertReturnsSystemRoots(t *testing.T) {
	dir, _ := cmdtest.FilePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://qa1.example.com")
	c := newUpdateServerCmd(dir)
	_, err := c.Execute(context.Background(), cmdtest.Input{Str: map[string]string{"cacert": cmdtest.CAPem1}, Pos: []string{"qa1"}}, nil)
	require.NoError(t, err)

	res, err := c.Execute(context.Background(), cmdtest.Input{Flag: map[string]bool{"clear-cacert": true}, Pos: []string{"qa1"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"ca"}, res.Data.(map[string]any)["changed"])
	assert.Equal(t, "server qa1 updated\n  ca: cleared (changed)\n", res.Text)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Empty(t, cfg.Servers["qa1"].CA)
}

func TestUpdateServerSameValueIsNoOpNoDeletion(t *testing.T) {
	dir, store := cmdtest.FilePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://qa1.example.com")
	require.NoError(t, store.SetToken(config.ServerRef{Name: "qa1", URL: "https://qa1.example.com"}, "tok"))

	c := newUpdateServerCmd(dir)
	res, err := c.Execute(context.Background(), cmdtest.Input{Str: map[string]string{"url": "https://qa1.example.com"}, Pos: []string{"qa1"}}, nil)
	require.NoError(t, err)
	data := res.Data.(map[string]any)
	assert.Equal(t, true, data["unchanged"])
	assert.Equal(t, []string{}, data["changed"])
	assert.Equal(t, false, data["credentialsCleared"])
	assert.Equal(t, "server qa1 unchanged\n", res.Text)

	tok, gerr := store.GetToken(config.ServerRef{Name: "qa1", URL: "https://qa1.example.com"})
	require.NoError(t, gerr)
	assert.Equal(t, "tok", tok)
}

func TestUpdateServerNoFlagsIsValidation(t *testing.T) {
	dir, _ := cmdtest.FilePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://qa1.example.com")
	c := newUpdateServerCmd(dir)
	_, err := c.Execute(context.Background(), cmdtest.Input{Pos: []string{"qa1"}}, nil)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

// TestUpdateServerRejectsUnsupportedScheme: same scheme gate as
// add-server/login --url, reached through --url on an existing entry.
func TestUpdateServerRejectsUnsupportedScheme(t *testing.T) {
	dir, _ := cmdtest.FilePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://qa1.example.com")
	c := newUpdateServerCmd(dir)
	_, err := c.Execute(context.Background(), cmdtest.Input{Str: map[string]string{"url": "ftp://host"}, Pos: []string{"qa1"}}, nil)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)

	cfg, cerr := config.Load(config.FilePath(dir))
	require.NoError(t, cerr)
	assert.Equal(t, "https://qa1.example.com", cfg.Servers["qa1"].URL, "no write on an unsupported scheme")
}

func TestUpdateServerUnknownNameIsNotFound(t *testing.T) {
	dir, _ := cmdtest.FilePolicyDir(t)
	c := newUpdateServerCmd(dir)
	_, err := c.Execute(context.Background(), cmdtest.Input{Str: map[string]string{"url": "https://qa1.example.com"}, Pos: []string{"ghost"}}, nil)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
}

func TestUpdateServerMutuallyExclusiveCacertFlags(t *testing.T) {
	dir, _ := cmdtest.FilePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://qa1.example.com")
	c := newUpdateServerCmd(dir)
	_, err := c.Execute(context.Background(), cmdtest.Input{Str: map[string]string{"cacert": cmdtest.CAPem1}, Flag: map[string]bool{"clear-cacert": true}, Pos: []string{"qa1"}}, nil)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestUpdateServerCompletesServerNames(t *testing.T) {
	dir, _ := cmdtest.FilePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://qa1.example.com")
	view := cmdtest.View{Dir: dir, Servers: []string{"qa1"}}
	c := newUpdateServerCmd(dir)
	assert.Equal(t, []string{"qa1"}, c.CompleteArgs(t.Context(), view, nil))
}

// TestUpdateServerBindingWriteFailureClearsNothing asserts the order the
// two writes happen in. The entry's cache binding id advances before the
// purge, so a get-token that read the old id and the old credential
// cannot write a record the purge will not see and the entry will still
// accept. When that write fails, nothing has been deleted yet and the
// command stops with the credential and the address both untouched.
func TestUpdateServerBindingWriteFailureClearsNothing(t *testing.T) {
	dir, store := cmdtest.FilePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://old.example.com")
	require.NoError(t, store.SetToken(config.ServerRef{Name: "qa1", URL: "https://old.example.com"}, "tok"))

	restore := config.SetPersistWriteHookForTest(func(step string) error {
		if step == "cachebinding" {
			return errors.New("disk full")
		}
		return nil
	})
	t.Cleanup(restore)

	c := newUpdateServerCmd(dir)
	_, err := c.Execute(context.Background(), cmdtest.Input{Str: map[string]string{"url": "https://new.example.com"}, Pos: []string{"qa1"}}, nil)
	require.ErrorContains(t, err, "disk full")

	tok, terr := store.GetToken(config.ServerRef{Name: "qa1", URL: "https://old.example.com"})
	require.NoError(t, terr, "nothing was purged")
	assert.Equal(t, "tok", tok)
	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.Equal(t, "https://old.example.com", cfg.Servers["qa1"].URL, "the address is untouched")
	assert.False(t, cfg.Servers["qa1"].Paused(), "and nothing is paused: the first write never landed")
}

// TestUpdateServerAdvancesTheCacheBindingBeforeThePurge asserts the id the
// entry keeps after a successful URL change is a new one: the clearing
// invalidated every exec-credential cache record the old login left, and
// the entry write that follows must not throw that away by writing a
// fresh value over it.
func TestUpdateServerAdvancesTheCacheBindingBeforeThePurge(t *testing.T) {
	dir, store := cmdtest.FilePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://old.example.com")
	require.NoError(t, store.SetToken(config.ServerRef{Name: "qa1", URL: "https://old.example.com"}, "tok"))

	before, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	seed := before.Servers["qa1"]
	require.NoError(t, config.AdvanceCacheBinding(&seed))
	before.Servers["qa1"] = seed
	before.SetPath(config.FilePath(dir))
	require.NoError(t, before.Write())

	var atPurge config.Server
	restore := config.SetAfterWriteHookForTest(func(what string) {
		if what == "credentials.yaml" {
			cfg, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			atPurge = cfg.Servers["qa1"]
		}
	})
	t.Cleanup(restore)

	c := newUpdateServerCmd(dir)
	_, uerr := c.Execute(context.Background(), cmdtest.Input{Str: map[string]string{"url": "https://new.example.com"}, Pos: []string{"qa1"}}, nil)
	require.NoError(t, uerr)

	after, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.Equal(t, "https://new.example.com", after.Servers["qa1"].URL)
	assert.NotEmpty(t, after.Servers["qa1"].CacheBindingID, "the entry keeps a binding after the write")
	assert.NotEqual(t, seed.CacheBindingID, after.Servers["qa1"].CacheBindingID, "and it is a new one")
	assert.Equal(t, config.PauseLocked, atPurge.CacheWritesPaused, "cache writes are paused while the credential is deleted")
	assert.Equal(t, after.Servers["qa1"].CacheBindingID, atPurge.CacheBindingID, "under the id the first write advanced")
	assert.False(t, after.Servers["qa1"].Paused(), "and the entry write resumes them")
}

// TestAURLChangeWhoseDeleteFailsLeavesTheEntryPaused asserts the stop
// between the first write and the last: the credential could not be
// cleared, the address is unchanged, and the entry's cache writes stay
// paused under the advanced id until the next cache miss or change.
func TestAURLChangeWhoseDeleteFailsLeavesTheEntryPaused(t *testing.T) {
	dir, store := cmdtest.FilePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://old.example.com")
	require.NoError(t, store.SetToken(config.ServerRef{Name: "qa1", URL: "https://old.example.com"}, "tok"))
	before, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	require.NoError(t, os.Remove(config.CredentialsFilePath(dir)))
	require.NoError(t, os.Mkdir(config.CredentialsFilePath(dir), 0o700))

	_, err = newUpdateServerCmd(dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://new.example.com"}, Pos: []string{"qa1"}}, nil)
	require.ErrorContains(t, err, "its cache binding id has already been advanced and its cache writes paused")
	after, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.Equal(t, "https://old.example.com", after.Servers["qa1"].URL)
	assert.NotEqual(t, before.Servers["qa1"].CacheBindingID, after.Servers["qa1"].CacheBindingID)
	assert.Equal(t, config.PauseLocked, after.Servers["qa1"].CacheWritesPaused)
}

// TestAURLChangeWithoutTheLockClearsThePauseItMade asserts the setter's
// side of the marker a change made with RANCHER_CLI_CONFIG_NO_LOCK
// leaves: no other process may clear it, so the entry write that ends the
// URL change has to.
func TestAURLChangeWithoutTheLockClearsThePauseItMade(t *testing.T) {
	dir, store := cmdtest.FilePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://old.example.com")
	require.NoError(t, store.SetToken(config.ServerRef{Name: "qa1", URL: "https://old.example.com"}, "tok"))
	t.Setenv(config.EnvConfigNoLock, "1")
	var atUpdate string
	t.Cleanup(config.SetPersistWriteHookForTest(func(step string) error {
		if step == "update" {
			cfg, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			atUpdate = cfg.Servers["qa1"].CacheWritesPaused
		}
		return nil
	}))

	_, err := newUpdateServerCmd(dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://new.example.com"}, Pos: []string{"qa1"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, config.PauseNoLock, atUpdate, "the first write marked the pause as made without the lock")
	after, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.False(t, after.Servers["qa1"].Paused())
}

// TestUpdateServerUnknownNameListsServers asserts an unknown name is
// NOT_FOUND and the error lists every configured server in name order.
func TestUpdateServerUnknownNameListsServers(t *testing.T) {
	dir, _ := cmdtest.FilePolicyDir(t)
	want := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"}
	for _, n := range want {
		addServerForUpdateTests(t, dir, n, "https://"+n+".example.com")
	}
	c := newUpdateServerCmd(dir)
	_, err := c.Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://qa1.example.com"}, Pos: []string{"ghost"}}, nil)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, want, candidateIDs(ce.Candidates))
}
