package kubeconfig

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// authProviderArg is the --auth-provider the exec entry's arguments carry
// right after --user-id and its value, "" when none follows them.
func authProviderArg(t *testing.T, args []string) string {
	t.Helper()
	i := slices.Index(args, "--user-id")
	if i < 0 {
		assert.NotContains(t, args, "--auth-provider", "--auth-provider is written beside --user-id only")
		return ""
	}
	if i+3 < len(args) && args[i+2] == "--auth-provider" {
		return args[i+3]
	}
	assert.NotContains(t, args, "--auth-provider", "--auth-provider, when written, follows --user-id")
	return ""
}

// TestKubeconfigWritesTheAnsweringLoginsAuthConfig asserts the exec entry
// names, with --auth-provider right after --user-id, the auth config the
// login that answers the plugin's lookup recorded, and only then: not for a
// login that recorded none, not when the recorded value is not an auth
// config name, and not without --user-id, which a login that records no
// owner leaves out. None of these prints a notice about the auth config.
func TestKubeconfigWritesTheAnsweringLoginsAuthConfig(t *testing.T) {
	keyring.MockInit()
	for _, tc := range []struct {
		name         string
		setup        recordsSetup
		user, config string
		// lines is how many notices the run prints, the machine line included.
		lines int
	}{
		{name: "a login that recorded an auth config", setup: recordsSetup{cfg: prodCurrent(nil), loggedIn: []string{"prod"}, authConfig: "corp-ldap"}, user: "u-prod", config: "corp-ldap", lines: 1},
		{name: "a login that recorded none", setup: recordsSetup{cfg: prodCurrent(nil), loggedIn: []string{"prod"}}, user: "u-prod", lines: 1},
		{name: "a recorded value that is no auth config name", setup: recordsSetup{cfg: prodCurrent(nil), loggedIn: []string{"prod"}, authConfig: "Corp LDAP"}, user: "u-prod", lines: 1},
		{name: "a login that recorded an auth config and no owner", setup: recordsSetup{cfg: prodCurrent(nil), loggedIn: []string{"prod"}, unowned: true, authConfig: "corp-ldap"}, lines: 2},
		{name: "an entry named <current>-<id> answers with its own", setup: recordsSetup{
			cfg: prodCurrent(map[string]config.Server{"prod-c-1": {URL: recordsURL, TokenAuthConfig: "openldap"}}), loggedIn: []string{"prod-c-1"}, authConfig: "openldap"},
			user: "u-prod-c-1", config: "openldap", lines: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, lines, _, err := runRecords(t, tc.setup)
			require.NoError(t, err)
			assert.Equal(t, tc.user, userIDArg(t, e.Args))
			assert.Equal(t, tc.config, authProviderArg(t, e.Args))
			assert.Len(t, lines, tc.lines, "the run prints no notice about the auth config: %v", lines)
			for _, l := range lines {
				assert.NotContains(t, l, "auth config")
				assert.NotContains(t, l, clierr.Rancher+" auth login", "no remedy is offered for it")
			}
		})
	}
}
