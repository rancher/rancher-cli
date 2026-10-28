package token

import (
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCacheProbeKeyGivesNoKeyForAnUnusableServer asserts that the probe key
// of a server URL that carries userinfo or does not parse is none, so the
// probe misses and the run reaches connectionTarget, which refuses it: a hit
// and a miss with the same URL exit the same way.
func TestCacheProbeKeyGivesNoKeyForAnUnusableServer(t *testing.T) {
	t.Parallel()
	for _, server := range []string{
		"https://u:pwzq7@r.ex", "u:pwzq7@r.ex", "https://u@r.ex", "https://:pwzq7@r.ex",
		"https://u:pwzq7@r.ex:abc", "https://u:pwzq7@:443",
	} {
		key, ok := computeCacheProbeKey("prod", server, "c-1", "", "")
		assert.False(t, ok, server)
		assert.Empty(t, key, server)
	}
	key, ok := computeCacheProbeKey("prod", "r.ex", "c-1", "", "")
	require.True(t, ok)
	assert.Equal(t, config.CacheKeyFor("prod", "https://r.ex", "c-1", "", ""), key)
}

// TestGetTokenProbeMissesForAServerWithUserinfo asserts that a cached
// credential that the same URL without userinfo hits is not served for a
// --server that carries userinfo, on the exec plugin's probe and on the
// legacy token command's.
func TestGetTokenProbeMissesForAServerWithUserinfo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	identity := config.CanonicalServerIdentity("https://r.ex")
	exp := time.Now().Add(time.Hour)
	require.NoError(t, seedCache(t, dir, store, config.CacheKey("prod", identity, "c-1"), "cached-tok", exp, "prod", identity, "c-1"))
	shim := config.LegacyCachePrefix + "alice"
	require.NoError(t, seedCache(t, dir, store, config.CacheKey(shim, identity, "c-1"), "cached-tok", exp, shim, identity, "c-1"))

	cmd := tokenCmd{dir: dir, store: store, anchor: anchorFor(dir)}
	_, _, _, hit := cmd.probeFlagCache("prod", "https://r.ex", "c-1", "", "", ui.Discard())
	require.True(t, hit, "the same URL without userinfo is a hit")
	_, _, _, hit = cmd.probeFlagCache("prod", "https://u:pwzq7@r.ex", "c-1", "", "", ui.Discard())
	assert.False(t, hit, "a URL with userinfo is a miss")

	_, _, _, hit, _ = shimProbeFlagCache(dir, shim, "https://r.ex", "c-1", anchorFor(dir), ui.Discard())
	require.True(t, hit, "the same URL without userinfo is a hit")
	_, _, _, hit, _ = shimProbeFlagCache(dir, shim, "https://u:pwzq7@r.ex", "c-1", anchorFor(dir), ui.Discard())
	assert.False(t, hit, "a URL with userinfo is a miss")
}

// TestRecordedUserIDsForAnUnusableCurrentServer asserts that completion of
// --user-id gives no value when the server it would use, the current entry's
// URL, is refused, and the entry's recorded owner when it is usable.
func TestRecordedUserIDsForAnUnusableCurrentServer(t *testing.T) {
	t.Setenv(config.EnvURL, "")
	for _, tc := range []struct {
		url  string
		want []string
	}{
		{"https://r.ex", []string{"u-abc12"}},
		{"https://u:pwzq7@r.ex", nil},
		{"https://r.ex:abc", nil},
	} {
		cfg := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{"prod": {URL: tc.url, TokenUserID: "u-abc12"}}}
		assert.Equal(t, tc.want, RecordedUserIDs(cfg, "", "prod", "c-1"), tc.url)
	}
}
