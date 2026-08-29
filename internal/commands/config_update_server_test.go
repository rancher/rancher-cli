package commands

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newUpdateServerCmd(dir string) *configUpdateServerCmd {
	return &configUpdateServerCmd{dir: dir}
}

func addServerForUpdateTests(t *testing.T, dir, name, url string) {
	t.Helper()
	c := newAddServerCmd(dir)
	_, err := c.Execute(context.Background(), args{s: map[string]string{"url": url}, a: []string{name}}, nil)
	require.NoError(t, err)
}

// TestUpdateServerWriteFailureAfterPurgeNamesClearedCredentials: purge-then-
// write is the safe ordering (a purge failure never leaves config.yaml
// pointed at the new, credential-less address), but the reverse — a
// successful purge followed by a write failure — must say so plainly: the
// entry still names the OLD URL, but its token is already gone, so a
// generic write-error message would look like nothing happened.
func TestUpdateServerWriteFailureAfterPurgeNamesClearedCredentials(t *testing.T) {
	dir, store := filePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://old.example.com")
	require.NoError(t, store.SetToken(config.ServerRef{Name: "qa1", URL: "https://old.example.com"}, "tok"))

	// Turn config.yaml into a symlink to a copy of itself: reads still
	// resolve normally (Load follows the symlink), but atomicWriteFile
	// explicitly refuses to write through a symlink — a deterministic,
	// single-file write failure that never touches credentials.yaml (a
	// separate file in the same dir), so the purge below succeeds for real.
	realPath := config.FilePath(dir)
	data, rerr := os.ReadFile(realPath)
	require.NoError(t, rerr)
	backing := filepath.Join(dir, "config.yaml.real")
	require.NoError(t, os.WriteFile(backing, data, 0o600))
	require.NoError(t, os.Remove(realPath))
	require.NoError(t, os.Symlink(backing, realPath))

	c := newUpdateServerCmd(dir)
	_, err := c.Execute(context.Background(), args{s: map[string]string{"url": "https://new.example.com"}, a: []string{"qa1"}}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "credentials", "the write failure must name the credentials as already cleared")
	assert.Contains(t, err.Error(), `"qa1"`)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, "rancher auth login 'qa1'", ce.Hint)

	_, terr := store.GetToken(config.ServerRef{Name: "qa1", URL: "https://old.example.com"})
	require.ErrorIs(t, terr, config.ErrNoToken, "the purge that ran before the failed write is not undone")
}

// TestUpdateServerConflictNeverPurgesCredentials exercises the CONFLICT path
// at the command level (not just commitUpdate directly): the injected
// tlsPreflight rotates the entry's CA file's bytes between
// prepareUpdateSource's read and the commit's re-check — a real command
// Execute() call reaching a genuine two-phase conflict without any actual
// concurrency. Purge only ever runs after validateUpdate succeeds, so a
// conflict here must leave the stored token and config.yaml untouched.
func TestUpdateServerConflictNeverPurgesCredentials(t *testing.T) {
	path := writeCAFile(t, testCAPem1)
	dir, store := filePolicyDir(t)
	addCmd := &configAddServerCmd{dir: dir, tlsPreflight: func(context.Context, string, string) (string, error) { return "", nil }}
	_, err := addCmd.Execute(context.Background(),
		args{s: map[string]string{"url": "https://old.example.com", "cacert": path}, a: []string{"qa1"}}, nil)
	require.NoError(t, err)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "qa1", URL: "https://old.example.com"}, "tok"))

	c := &configUpdateServerCmd{dir: dir, tlsPreflight: func(context.Context, string, string) (string, error) {
		require.NoError(t, os.WriteFile(path, []byte(testCAPem2), 0o600))
		return "", nil
	}}
	_, err = c.Execute(context.Background(), args{s: map[string]string{"url": "https://new.example.com"}, a: []string{"qa1"}}, nil)
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
	dir, store := filePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://old.example.com")
	require.NoError(t, store.SetToken(config.ServerRef{Name: "qa1", URL: "https://old.example.com"}, "tok"))

	c := newUpdateServerCmd(dir)
	res, err := c.Execute(context.Background(), args{s: map[string]string{"url": "https://new.example.com"}, a: []string{"qa1"}}, nil)
	require.NoError(t, err)
	data := res.Data.(map[string]any)
	assert.Equal(t, []string{"url"}, data["changed"])
	assert.Equal(t, true, data["credentialsCleared"])
	// Same single-server shape as config delete-server (map keyed
	// "backend"), not a flat list — both report DeleteTokenEverywhere's
	// output and should look alike on the wire.
	assert.Equal(t, map[string][]string{"backend": {"file"}}, data["deletedFrom"])
	assert.Equal(t, "server qa1 updated\n"+
		"  url: https://new.example.com (changed)\n"+
		"  credentials cleared — log in again: rancher auth login 'qa1'\n", res.Text)

	_, gerr := store.GetToken(config.ServerRef{Name: "qa1", URL: "https://new.example.com"})
	require.ErrorIs(t, gerr, config.ErrNoToken)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "https://new.example.com", cfg.Servers["qa1"].URL)
}

func TestUpdateServerCAOnlyKeepsCredentials(t *testing.T) {
	dir, store := filePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://qa1.example.com")
	require.NoError(t, store.SetToken(config.ServerRef{Name: "qa1", URL: "https://qa1.example.com"}, "tok"))

	c := newUpdateServerCmd(dir)
	res, err := c.Execute(context.Background(), args{s: map[string]string{"cacert": testCAPem1}, a: []string{"qa1"}}, nil)
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
	dir, _ := filePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://qa1.example.com")
	c := newUpdateServerCmd(dir)
	_, err := c.Execute(context.Background(), args{s: map[string]string{"cacert": testCAPem1}, a: []string{"qa1"}}, nil)
	require.NoError(t, err)

	res, err := c.Execute(context.Background(), args{b: map[string]bool{"clear-cacert": true}, a: []string{"qa1"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"ca"}, res.Data.(map[string]any)["changed"])
	assert.Equal(t, "server qa1 updated\n  ca: cleared (changed)\n", res.Text)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Empty(t, cfg.Servers["qa1"].CA)
}

func TestUpdateServerSameValueIsNoOpNoDeletion(t *testing.T) {
	dir, store := filePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://qa1.example.com")
	require.NoError(t, store.SetToken(config.ServerRef{Name: "qa1", URL: "https://qa1.example.com"}, "tok"))

	c := newUpdateServerCmd(dir)
	res, err := c.Execute(context.Background(), args{s: map[string]string{"url": "https://qa1.example.com"}, a: []string{"qa1"}}, nil)
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
	dir, _ := filePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://qa1.example.com")
	c := newUpdateServerCmd(dir)
	_, err := c.Execute(context.Background(), args{a: []string{"qa1"}}, nil)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

// TestUpdateServerRejectsUnsupportedScheme: same scheme gate as
// add-server/login --url, reached through --url on an existing entry.
func TestUpdateServerRejectsUnsupportedScheme(t *testing.T) {
	dir, _ := filePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://qa1.example.com")
	c := newUpdateServerCmd(dir)
	_, err := c.Execute(context.Background(), args{s: map[string]string{"url": "ftp://host"}, a: []string{"qa1"}}, nil)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)

	cfg, cerr := config.Load(config.FilePath(dir))
	require.NoError(t, cerr)
	assert.Equal(t, "https://qa1.example.com", cfg.Servers["qa1"].URL, "no write on an unsupported scheme")
}

func TestUpdateServerUnknownNameIsNotFound(t *testing.T) {
	dir, _ := filePolicyDir(t)
	c := newUpdateServerCmd(dir)
	_, err := c.Execute(context.Background(), args{s: map[string]string{"url": "https://qa1.example.com"}, a: []string{"ghost"}}, nil)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
}

func TestUpdateServerMutuallyExclusiveCacertFlags(t *testing.T) {
	dir, _ := filePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://qa1.example.com")
	c := newUpdateServerCmd(dir)
	_, err := c.Execute(context.Background(), args{s: map[string]string{"cacert": testCAPem1}, b: map[string]bool{"clear-cacert": true}, a: []string{"qa1"}}, nil)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestUpdateServerCompletesServerNames(t *testing.T) {
	dir, _ := filePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://qa1.example.com")
	view := testView{dir: dir, srvNames: []string{"qa1"}}
	c := newUpdateServerCmd(dir)
	assert.Equal(t, []string{"qa1"}, c.CompleteArgs(context.Background(), view))
}
