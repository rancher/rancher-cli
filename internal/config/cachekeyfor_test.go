package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestCacheKeyForCarriesTheUserID asserts the two key forms: a call
// without --user-id keys as CacheKey does, and one with it carries the
// user id ahead of that key under the reserved prefix, so two user ids,
// or a user id and none, never share a key.
func TestCacheKeyForCarriesTheUserID(t *testing.T) {
	t.Parallel()
	const srv, cluster = "https://r.ex", "c-1"
	assert.Equal(t, "prod-c-1@https://r.ex/c-1", CacheKeyFor("prod-c-1", srv, cluster, ""))
	assert.Equal(t, CacheKey("prod-c-1", srv, cluster), CacheKeyFor("prod-c-1", srv, cluster, ""))
	assert.Equal(t, "user-id:u-abc12/prod-c-1@https://r.ex/c-1", CacheKeyFor("prod-c-1", srv, cluster, "u-abc12"))
	assert.NotEqual(t, CacheKeyFor("prod-c-1", srv, cluster, "u-a"), CacheKeyFor("prod-c-1", srv, cluster, "u-b"))
	assert.NotEqual(t, CacheKeyFor("prod-c-1", srv, cluster, "u-a"), CacheKeyFor("prod-c-1", srv, cluster, ""))
	// The user id ends at the first "/" only because a user id holds
	// none: auth get-token refuses one that is not an object name, and
	// without that check these two calls would share a key.
	assert.Equal(t, CacheKeyFor("b/prod", srv, cluster, "u-a"), CacheKeyFor("prod", srv, cluster, "u-a/b"))
	for _, name := range []string{"prod", "", "legacy:prod", "user-idx", "user-id", "exec-login-r.ex"} {
		assert.False(t, strings.HasPrefix(CacheKeyFor(name, srv, cluster, ""), UserIDCachePrefix),
			"a key without a user id begins with its name %q, never with the reserved prefix", name)
	}
	assert.True(t, IsUserIDCacheSlot("user-id:prod"))
	assert.False(t, IsUserIDCacheSlot("user-id"))
	assert.False(t, IsUserIDCacheSlot("prod"))
}

// TestValidateEntryNameRefusesTheUserIDPrefix asserts a server entry name
// cannot open the reserved namespace of the exec plugin's --user-id
// cache slots, beside the two other reserved namespaces.
func TestValidateEntryNameRefusesTheUserIDPrefix(t *testing.T) {
	t.Parallel()
	ce := ValidateEntryName("user-id:prod")
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, `server names starting with "user-id:" are reserved for the exec plugin's cache slots of calls with --user-id; use a different name`, ce.Message)
	assert.Nil(t, ValidateEntryName("user-id"))
	assert.Nil(t, ValidateEntryName("prod"))
	assert.NotNil(t, ValidateEntryName(LegacyCachePrefix+"prod"))
	assert.NotNil(t, ValidateEntryName(ExecLoginPrefix+"r.ex"))
}
