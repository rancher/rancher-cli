package register

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
)

// TestValidateUpdatePlansARewriteWhenOnlyTheUserinfoDiffers asserts that a
// stored URL that holds a user name or password and names the same server as
// the new URL is rewritten without it, as a change of no field and no
// credential, and that every other difference is a change of the URL as
// before.
func TestValidateUpdatePlansARewriteWhenOnlyTheUserinfoDiffers(t *testing.T) {
	for _, tc := range []struct {
		name, stored, typed string
		rewrite             bool
	}{
		{"userinfo", "https://u:pwzq7@host.example", "https://host.example", true},
		{"a user name alone", "https://userzq7@host.example", "https://host.example", true},
		{"userinfo and an uppercase host", "HTTPS://u:pwzq7@Host.Example:443", "https://host.example", true},
		{"userinfo and another host", "https://u:pwzq7@host.example", "https://other.example", false},
		{"userinfo and a bad port", "https://u:pwzq7@host.example:abc", "https://host.example", false},
		{"an uppercase host and no userinfo", "HTTPS://Host.Example", "https://host.example", false},
		{"no scheme", "u:pwzq7@host.example:1", "https://host.example:1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Servers: map[string]config.Server{"x": {URL: tc.stored, CAFingerprint: "fp", CAFromEnvironment: true}}}
			plan, err := ValidateUpdate(cfg, "x", UpdateSource{CanonicalURL: tc.stored}, Probe{CanonicalURL: tc.typed})
			require.NoError(t, err)
			assert.Equal(t, tc.rewrite, plan.RewriteURL)
			assert.Equal(t, !tc.rewrite, plan.CredentialsCleared)
			if tc.rewrite {
				assert.Empty(t, plan.ChangedFields, "the server did not change")
			} else {
				assert.Equal(t, []string{"url"}, plan.ChangedFields)
			}
		})
	}
}

// TestApplyUpdateRewritesTheURLAndKeepsTheRest asserts that a rewrite that
// removes the user information writes the canonical URL, keeps the login's
// records, the CA fingerprint and the CA's origin, and reports a change that
// is not a server change.
func TestApplyUpdateRewritesTheURLAndKeepsTheRest(t *testing.T) {
	cfg := &config.Config{CurrentServer: "x", Servers: map[string]config.Server{"x": {
		URL: "https://u:pwzq7@host.example", CAFingerprint: "fp", CAFromEnvironment: true, CacheBindingID: "b1",
		SupersededTokens: []config.SupersededToken{{Server: "https://host.example"}},
	}}}
	dir := t.TempDir()
	cfg.SetPath(config.FilePath(dir))
	plan, err := ValidateUpdate(cfg, "x", UpdateSource{CanonicalURL: "https://u:pwzq7@host.example"}, Probe{CanonicalURL: "https://host.example"})
	require.NoError(t, err)
	out, err := ApplyUpdate(cfg, plan)
	require.NoError(t, err)
	assert.False(t, out.Unchanged)
	assert.True(t, out.UserinfoRemoved)
	assert.False(t, out.CredentialsCleared)
	assert.Empty(t, out.ChangedFields)
	e := cfg.Servers["x"]
	assert.Equal(t, "https://host.example", e.URL)
	assert.Equal(t, "fp", e.CAFingerprint)
	assert.True(t, e.CAFromEnvironment)
	assert.Equal(t, "b1", e.CacheBindingID)
	assert.Len(t, e.SupersededTokens, 1)
	reread, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "https://host.example", reread.Servers["x"].URL, "the rewrite is written")
}
