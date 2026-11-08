package auth

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/remedytest"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// Two user ids that hold a direction override, a zero-width space, a bell, a
// newline and the first rune of the remedy marker followed by the word the
// marker ends with.
const (
	hostileFirstID  = "user-a\u202e\u200b\a\n\u2063rancher-1"
	hostileSecondID = "user-b\u202e\u200b\a\n\u2063rancher-2"
)

// userChangeText is the warning that says who the entry is logged in as now,
// decoded from the JSON envelope.
func userChangeText(t *testing.T, warnings []string) string {
	t.Helper()
	var found []string
	for _, w := range warningsInJSON(t, warnings) {
		if strings.HasPrefix(w, "server entry prod is now logged in as") {
			found = append(found, w)
		}
	}
	require.Len(t, found, 1)
	return found[0]
}

// TestTheUserChangeWarningShowsAnIdWithHiddenRunesAsEscapes asserts the
// warning of a login that replaces another user's login holds no rune the
// terminal hides, whether the new owner comes from a sign-in's create response
// or from the verify request of a stored token, and shows each id with escapes
// in the JSON envelope: the exact id, never one with the runes removed.
func TestTheUserChangeWarningShowsAnIdWithHiddenRunesAsEscapes(t *testing.T) {
	t.Parallel()
	want := "server entry prod is now logged in as user " + strconv.Quote(hostileSecondID) + "; it was logged in as user " + strconv.Quote(hostileFirstID)
	newFixture := func(t *testing.T) *userChangeFixture {
		t.Helper()
		f := testrancher.New(testrancher.Options{Users: []testrancher.User{
			{Name: "alice", Password: "pw", UserID: hostileFirstID},
			{Name: "bob", Password: "pw", UserID: hostileSecondID},
		}})
		return newUserChangeFixtureOn(t, f)
	}

	t.Run("the owner a sign-in names", func(t *testing.T) {
		t.Parallel()
		x := newFixture(t)
		x.signIn("alice")
		got := userChangeText(t, x.signIn("bob"))
		assert.Equal(t, want, got)
		remedytest.AssertIDShownWithEscapes(t, got, hostileFirstID)
		remedytest.AssertIDShownWithEscapes(t, got, hostileSecondID)
	})

	t.Run("the owner a verify request names", func(t *testing.T) {
		t.Parallel()
		x := newFixture(t)
		x.signIn("alice")
		cmd := newPlainLoginCmd(x.dir, x.store)
		cmd.verify = func(context.Context, config.Effective) (string, error) { return hostileSecondID, nil }
		in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
		res, err := cmd.Execute(t.Context(), in, cmdtest.StdinPrompter("id:secret"))
		require.NoError(t, err)
		got := userChangeText(t, res.Warnings)
		assert.Equal(t, want, got)
		remedytest.AssertIDShownWithEscapes(t, got, hostileFirstID)
		remedytest.AssertIDShownWithEscapes(t, got, hostileSecondID)
	})
}
