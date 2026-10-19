package token

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// TestPluginLoginEntryIsNamedForTheServerAndTheUser asserts the plugin
// login entry the plugin-created login makes is named for the server and,
// for a call with --user-id, the user, so one server holds one such entry
// for each user; a call without --user-id keeps the name of the server
// alone.
func TestPluginLoginEntryIsNamedForTheServerAndTheUser(t *testing.T) {
	x := newOwnerFixture(t, true, testrancher.Options{})
	x.entry("prod", "", "", false)
	_, err := x.call("prod-c-test", "user-carol", "carol", nil)
	require.NoError(t, err)
	_, err = x.call("prod-c-test", "user-alice", "alice", nil)
	require.NoError(t, err)
	cfg, err := config.Load(config.FilePath(x.dir))
	require.NoError(t, err)
	for user, entry := range map[string]string{"user-carol": x.pluginEntryOf("user-carol"), "user-alice": x.pluginEntryOf("user-alice")} {
		assert.True(t, strings.HasPrefix(entry, config.ExecLoginPrefix), "%s is in the plugin's namespace", entry)
		assert.Equal(t, user, cfg.Servers[entry].TokenUserID, entry)
		assert.True(t, strings.HasSuffix(entry, "-"+user), "%s is named for the user", entry)
		assert.Equal(t, clierr.ProvenanceExecLogin, cfg.Servers[entry].Provenance)
	}
	assert.NotContains(t, cfg.Servers, x.pluginEntry(), "no entry has the name of the server alone")

	y := newOwnerFixture(t, true, testrancher.Options{})
	y.entry("prod", "", "", false)
	_, err = y.call("prod-c-test", "", "carol", nil)
	require.NoError(t, err)
	_, host := y.stored(y.pluginEntry())
	assert.Equal(t, "user-carol", host.TokenUserID, "a call without --user-id makes the entry named for the server alone")
}

// TestUserIDPluginCreatedLoginWritesInPlaceOnlyOverItsOwnUser asserts the
// plugin login entry named for the server and the user is written in
// place when it records that user or none, and is never written over when
// it records another user: the call signs in interactively, with no
// disclosure for a login that does not happen, and the entry stays as it
// was.
func TestUserIDPluginCreatedLoginWritesInPlaceOnlyOverItsOwnUser(t *testing.T) {
	for _, recorded := range []string{"user-carol", ""} {
		t.Run("recording "+recorded, func(t *testing.T) {
			x := newOwnerFixture(t, true, testrancher.Options{})
			name := x.pluginEntryOf("user-carol")
			x.entry(name, "", recorded, true)
			got, err := x.call("kube-c-test", "user-carol", "carol", nil)
			require.NoError(t, err)
			assert.Equal(t, "user-carol", got)
			login, s := x.stored(name)
			assert.NotEmpty(t, login, "the entry is logged in in place")
			assert.Equal(t, "user-carol", s.TokenUserID)
			cfg, err := config.Load(config.FilePath(x.dir))
			require.NoError(t, err)
			assert.Len(t, cfg.Servers, 1, "no second entry is made")
		})
	}
	for _, holds := range []bool{false, true} {
		t.Run(map[bool]string{false: "recording another user, no login", true: "recording another user, holding a login"}[holds], func(t *testing.T) {
			x := newOwnerFixture(t, true, testrancher.Options{})
			name := x.pluginEntryOf("user-carol")
			user := ""
			if holds {
				user = "bob"
			}
			x.entry(name, user, "user-bob", true)
			beforeLogin, before := x.stored(name)
			var notices bytes.Buffer
			got, err := x.call("kube-c-test", "user-carol", "carol", &notices)
			require.NoError(t, err)
			assert.Equal(t, "user-carol", got, "the call signs in interactively")
			afterLogin, after := x.stored(name)
			assert.Equal(t, beforeLogin, afterLogin, "the other user's login is not written over")
			assert.Equal(t, before, after)
			assert.NotContains(t, notices.String(), "plaintext file")
			assert.True(t, x.cached("kube-c-test", "user-carol"), "the sign-in's token is cached")
		})
	}
}

// TestPluginLoginEntryNameLengthIsBounded asserts a plugin-created login
// whose entry name is longer than the credential store accepts is refused
// before the sign-in, on every operating system, with the login of an
// entry of a shorter name as the remedy; one name at the limit is written.
func TestPluginLoginEntryNameLengthIsBounded(t *testing.T) {
	require.Equal(t, 467, config.MaxEntryNameLength, "500 less the 32-character account id and the colon")
	for _, tc := range []struct {
		length  int
		refused bool
	}{
		{config.MaxEntryNameLength, false},
		{config.MaxEntryNameLength + 1, true},
	} {
		t.Run(map[bool]string{false: "at the limit", true: "one over"}[tc.refused], func(t *testing.T) {
			x := newOwnerFixture(t, true, testrancher.Options{})
			name := config.ExecLoginPrefix + strings.Repeat("x", tc.length-len(config.ExecLoginPrefix))
			require.Len(t, name, tc.length)
			x.entry(name, "", "user-carol", true)
			var notices bytes.Buffer
			got, err := x.call(name+"-c-test", "user-carol", "carol", &notices)
			if !tc.refused {
				require.NoError(t, err)
				assert.Equal(t, "user-carol", got)
				login, _ := x.stored(name)
				assert.NotEmpty(t, login)
				return
			}
			ce, msg := refusal(t, err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, "the exec plugin's login entry for this server would have a name of 468 characters, and the credential store accepts at most 467; "+
				"to log in under a shorter name, pipe an API token of 'user-carol' to rancher auth login <name> --url '"+x.serverID()+"' --token-stdin, with a name of your choice for <name>", msg)
			assert.NotContains(t, notices.String(), "Enter username", "nothing is signed in")
			login, _ := x.stored(name)
			assert.Empty(t, login, "nothing is stored")
			assert.Empty(t, x.f.CreateRequests(), "and nothing is created")
		})
	}
}
