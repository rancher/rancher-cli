package token

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
)

// TestEntryOnOtherServerErrorNamesTheRefusalOfAnUnusableURL asserts that when
// the entry a call names holds a URL that cannot be used (an @ after the
// host, userinfo or a bad port), the conflict names the entry's refusal and
// repair, suggests no login or kubeconfig from the URL, and leaves a plugin
// login entry as it is; and that the route that regenerates the kubeconfig
// from an entry registered at the call's URL stays when one exists.
func TestEntryOnOtherServerErrorNamesTheRefusalOfAnUnusableURL(t *testing.T) {
	t.Parallel()
	const rawAt = "it holds an @ after the host, which usually means a password with a /, ? or # in it"
	const route = "; to use https://r.example.com, regenerate it from an entry registered there with: rancher config use-server <entry>; then rancher kubeconfig --cluster 'c-1'"
	for _, tc := range []struct {
		name, entryName, url, reason string
		here, plugin                 bool
	}{
		{"an at sign after the host", "prod", "https://admin:1234/pwzq7@x", rawAt, false, false},
		{"an at sign after the host, an entry at the call's URL", "prod", "https://admin:1234/pwzq7@x", rawAt, true, false},
		{"userinfo", "prod", "https://u:pwzq7@hostzq7", "it holds a user name or password", false, false},
		{"a bad port", "prod", "https://hostzq7:abc", "it is not a valid URL", false, false},
		{"a plugin login entry with an at sign after the host", "exec-login-o", "https://admin:1234/pwzq7@x", rawAt, false, true},
		{"a plugin login entry with an at sign after the host, an entry at the call's URL", "exec-login-o", "https://admin:1234/pwzq7@x", rawAt, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{Servers: map[string]config.Server{tc.entryName: {URL: tc.url}}, CurrentServer: tc.entryName}
			if tc.here {
				cfg.Servers["staging"] = config.Server{URL: "https://r.example.com"}
			}
			ce := entryOnOtherServerError(cfg, tc.entryName, "https://r.example.com", "c-1")
			assert.Equal(t, clierr.CodeConflict, ce.Code)
			msg := clierr.ExpandRemedies(ce.Message, "")
			assert.Contains(t, msg, "the call names server entry '"+tc.entryName+"' and runs for https://r.example.com; the server URL in server entry '"+tc.entryName+"' cannot be used: "+tc.reason)
			for _, part := range []string{"admin", "1234", "pwzq7", "hostzq7", "<unusable URL>", "registered at", "auth login", "kubeconfig --cluster 'c-1' --"} {
				assert.NotContains(t, msg, part)
			}
			if tc.plugin {
				assert.NotContains(t, msg, "auth logout", "a plugin login entry is left as it is")
				assert.NotContains(t, msg, "delete-server", "a plugin login entry is left as it is")
				assert.NotContains(t, msg, "update-server")
			} else {
				assert.Contains(t, msg, "change the entry's URL with rancher config update-server '"+tc.entryName+"' --url <url>")
			}
			if tc.here {
				assert.Contains(t, msg, clierr.ExpandRemedies(route+useServerNote(tc.entryName), ""))
			} else {
				assert.NotContains(t, msg, "regenerate it from an entry")
			}
			assert.Empty(t, ce.Hint)
		})
	}
}
