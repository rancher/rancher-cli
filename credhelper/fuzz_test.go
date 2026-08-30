package credhelper

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FuzzCanonicalServerURL fuzzes the credential-identity function against
// arbitrary server URLs. Never call t.Parallel() here — it panics inside
// f.Fuzz.
func FuzzCanonicalServerURL(f *testing.F) {
	f.Add("https://example.com")
	f.Add("HTTPS://EXAMPLE.com:443")
	f.Add("http://user:pass@example.com/path?x=1#f")
	f.Add("https://[::1]:8443")
	f.Add("https://[fe80::1%25eth0]:443") // known-bug shape: zone id
	f.Add("http://a%25b")                 // known-bug minimal shape
	f.Add("http://[::%25%200]")           // escaped space alongside the zone escape
	f.Add("ftp://example.com")
	f.Add("")
	f.Add("://nope")
	f.Add("https://")
	f.Add("https://:443")
	f.Add("example.com")

	f.Fuzz(func(t *testing.T, raw string) {
		result, err := CanonicalServerURL(raw) // (a): no panic

		// (b) error/output exclusivity — mechanical shape only.
		assert.Equal(t, err == nil, result != "", "raw=%q result=%q err=%v", raw, result, err)

		if err != nil {
			return
		}

		// (d) userinfo never survives into the output.
		assert.NotContains(t, result, "@")

		// (e) an http/https input keeps its scheme, case-insensitively.
		if u, perr := url.Parse(raw); perr == nil {
			switch strings.ToLower(u.Scheme) {
			case "http":
				assert.True(t, strings.HasPrefix(result, "http://"), "result=%q", result)
			case "https":
				assert.True(t, strings.HasPrefix(result, "https://"), "result=%q", result)
			}
		}

		// (b2) the output re-parses.
		_, rerr := url.Parse(result)
		require.NoError(t, rerr, "result=%q", result)

		// (c) canonicalization is idempotent.
		again, aerr := CanonicalServerURL(result)
		require.NoError(t, aerr, "result=%q", result)
		assert.Equal(t, result, again)
	})
}
