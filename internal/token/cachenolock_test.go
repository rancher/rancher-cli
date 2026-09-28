package token

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAMissLeavesAPauseMadeWithoutTheLock asserts the exec plugin's side
// of the marker a change made with RANCHER_CLI_CONFIG_NO_LOCK leaves: the
// plugin takes the free lock, but it is not the change that paused the
// entry, and that change may still be running without the lock, so the
// pause stays and the skipped write says so, pointing at config status,
// which names why.
func TestAMissLeavesAPauseMadeWithoutTheLock(t *testing.T) {
	m := newMadeFrom(t)
	dir := pausedPlugin(t, m.URL)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	e := cfg.Servers["prod"]
	e.CacheWritesPaused = config.PauseNoLock
	cfg.Servers["prod"] = e
	require.NoError(t, cfg.Write())

	var notices bytes.Buffer
	tok, err := kubectl(t, dir, m.URL, ui.New(&notices, strings.NewReader(""), true, false), nil)
	require.NoError(t, err)
	assert.Equal(t, testrancher.MadeFromToken("alice"), tok, "the credential is returned")
	assert.Empty(t, served(t, dir, m.URL), "and no record was written for it")
	after, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, config.PauseNoLock, after.Servers["prod"].CacheWritesPaused, "the pause stays for the change that made it")
	assert.Contains(t, notices.String(), "not caching: a credential change for prod has not finished (see `rancher config status`)",
		"config status names the reason and what clears it")
}
