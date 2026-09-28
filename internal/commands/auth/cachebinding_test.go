package auth

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bindingIDs reads each entry's cache binding id from the config on disk.
func bindingIDs(t *testing.T, dir string, names ...string) map[string]string {
	t.Helper()
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	out := make(map[string]string, len(names))
	for _, n := range names {
		out[n] = cfg.Servers[n].CacheBindingID
	}
	return out
}

// TestLogoutAdvancesTheCacheBindingBeforeDeleting asserts the order the
// logout does its two jobs in. A get-token that read the entry's old
// binding id and its old credential a moment earlier is still running:
// if the purge went first, that call could write a fresh cache record
// after the purge and under an id the entry still accepts, and the next
// kubectl call would be served a credential for a login that is gone.
// Advancing the id first means such a record is a miss whenever it lands,
// and it also means a delete that then fails leaves no stale record
// behind even though the login credential may still work.
func TestLogoutAdvancesTheCacheBindingBeforeDeleting(t *testing.T) {
	dir := cmdtest.SeedTwoServerConfig(t)
	store := config.NewFileStore(dir)
	ref := config.ServerRef{Name: "acme", URL: "https://acme.example"}
	require.NoError(t, store.SetToken(ref, "tok-acme"))

	seed, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	entry := seed.Servers["acme"]
	require.NoError(t, config.AdvanceCacheBinding(&entry))
	seed.Servers["acme"] = entry
	seed.SetPath(config.FilePath(dir))
	require.NoError(t, seed.Write())
	before := bindingIDs(t, dir, "acme")

	// The hook fires before each config write. At the binding write the
	// credential must still be there: that is what "before deleting"
	// means.
	var credentialAtBindingWrite string
	restore := config.SetPersistWriteHookForTest(func(step string) error {
		if step == "cachebinding" {
			tok, gerr := store.GetToken(ref)
			if gerr == nil {
				credentialAtBindingWrite = tok
			}
		}
		return nil
	})
	t.Cleanup(restore)

	// And once that write lands the entry's cache writes are paused, so a
	// get-token that loads now writes nothing until the logout's last
	// write resumes them.
	var pausedAfterFirstWrite string
	restoreAfter := config.SetAfterWriteHookForTest(func(what string) {
		if what == "config.yaml" && pausedAfterFirstWrite == "" {
			cfg, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			pausedAfterFirstWrite = cfg.Servers["acme"].CacheWritesPaused + "."
		}
	})
	t.Cleanup(restoreAfter)

	runLogout(t, dir, "acme")

	assert.Equal(t, "tok-acme", credentialAtBindingWrite, "the id advances while the credential is still there")
	assert.Equal(t, config.PauseLocked+".", pausedAfterFirstWrite, "in the same write that pauses the entry's cache writes")
	after := bindingIDs(t, dir, "acme")
	assert.NotEqual(t, before["acme"], after["acme"])
	assert.NotEmpty(t, after["acme"])
	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.False(t, cfg.Servers["acme"].Paused(), "and the last write resumes them")
	_, gerr := store.GetToken(ref)
	require.ErrorIs(t, gerr, config.ErrNoToken, "and the credential is gone afterwards")
}

// TestLogoutAllAdvancesEveryTargetsCacheBinding asserts the same for the
// sweep: every entry it logs out gets a fresh binding, so no entry keeps
// serving records from the login just removed.
func TestLogoutAllAdvancesEveryTargetsCacheBinding(t *testing.T) {
	dir := cmdtest.SeedTwoServerConfig(t)
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "acme", URL: "https://acme.example"}, "tok-acme"))
	require.NoError(t, store.SetToken(config.ServerRef{Name: "acme2", URL: "https://acme2.example"}, "tok-acme2"))
	before := bindingIDs(t, dir, "acme", "acme2")

	cmd := &logoutCmd{dir: dir}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{Flag: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)

	after := bindingIDs(t, dir, "acme", "acme2")
	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	for _, n := range []string{"acme", "acme2"} {
		assert.NotEmpty(t, after[n], "%s has a binding", n)
		assert.NotEqual(t, before[n], after[n], "%s advanced", n)
		assert.False(t, cfg.Servers[n].Paused(), "%s resumed by the last write", n)
	}
}
