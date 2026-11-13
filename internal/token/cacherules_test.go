package token

import (
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTheNextLoginOfTheEntryARecordIsOnRetiresItOrLeavesItThere asserts the
// step the rule for update-server relies on: update-server sends nothing and
// records the token of a cache record on the other entry at the server, for
// that entry's next login. A login the CLI created, as the same owner,
// retires the token with one DELETE and drops its record; a pasted login
// sends nothing and the token stays recorded on the entry, with its warning.
func TestTheNextLoginOfTheEntryARecordIsOnRetiresItOrLeavesItThere(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	x.entry("dev", config.Server{TokenUserID: "user-alice"})
	devs := x.token("alice")
	x.cache("server", devs, x.serverWide(), true)

	d, err := x.localDelete(x.req(DeleteForUpdateServer, "prod"))
	require.NoError(t, err)
	d.RevokeTokens(t.Context())
	require.Empty(t, d.Settle(t.Context()))
	n := notRevoked(t, d.Report(), devs.Ref.Name)
	assert.Equal(t, "notAttempted", n.Reason)
	assert.Equal(t, "dev", n.Entry)
	assert.Equal(t, "nextLogin", n.Retry)
	require.Len(t, x.load().Servers["dev"].SupersededTokens, 1)
	require.True(t, x.alive(devs), "update-server sent nothing")

	pasted := RetireSuperseded(t.Context(), x.dir, "dev", x.url, UserSupplied("pasted:secret"), x.load().Servers["dev"].SupersededTokens)
	assert.Len(t, pasted.Kept, 1, "a pasted login leaves the token recorded")
	assert.NotEmpty(t, pasted.Warnings)
	assert.True(t, x.alive(devs), "a pasted login sends nothing")

	fresh := x.token("alice")
	before := len(x.f.DeleteRequests())
	rep := RetireSuperseded(t.Context(), x.dir, "dev", x.url, fresh, x.load().Servers["dev"].SupersededTokens)
	assert.Len(t, rep.Retired, 1)
	assert.Len(t, x.f.DeleteRequests(), before+1, "one DELETE of the token")
	assert.False(t, x.alive(devs), "the login the CLI created retired it")
	assert.Empty(t, x.load().Servers["dev"].SupersededTokens, "its record is dropped")
}

// TestAKeptRecordMovedTwiceIsOneItemOfTheRevokePlan asserts that a record
// moved to its kept key twice, by two writes that replace the same token's
// record, leaves one record, so logout --all plans one item and sends one
// DELETE of the token.
func TestAKeptRecordMovedTwiceIsOneItemOfTheRevokePlan(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("y", "alice")
	x.entry("x", config.Server{TokenUserID: "user-alice"})
	ct := x.token("alice")
	const key = "server@c-1"
	write := func(token LoginCredential, named bool) {
		x.cache(key, token, x.serverWide(), named)
	}
	setX := func(path string) {
		x.edit(func(cfg *config.Config) {
			e := cfg.Servers["x"]
			e.URL = x.url + path
			cfg.Servers["x"] = e
		})
	}
	for range 2 {
		setX("")
		write(ct, true)
		setX("/p@x")
		write(x.token("alice"), true)
	}
	var kept int
	for _, k := range x.cacheKeys() {
		if strings.HasPrefix(k, config.KeptTokenKeyPrefix) {
			kept++
		}
	}
	require.Equal(t, 1, kept, "one record of the token is kept: %v", x.cacheKeys())

	before := len(x.f.DeleteRequests())
	_, rep, err := x.run(x.req(DeleteForLogoutAll))
	require.NoError(t, err)
	assert.Contains(t, revokedNames(rep), ct.Ref.Name)
	var sent int
	for _, name := range deletedNames(x.f, before) {
		if name == ct.Ref.Name {
			sent++
		}
	}
	assert.Equal(t, 1, sent, "one DELETE of the token")
}
