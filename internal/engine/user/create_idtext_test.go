package user

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/remedytest"
)

// hiddenRunesName holds a direction override, a zero-width space, a bell, a
// newline and the start of a remedy marker followed by the word the marker
// ends with.
const hiddenRunesName = "u-\u202e\u200b\a\n\u2063rancher-x"

// TestACreatedUserNameWithHiddenRunesIsShownAsEscapes asserts that a
// server-assigned user name holding hidden runes reaches the recovery of a
// failed Secret create and of a failed binding with escapes, never with the
// runes removed, that no command names it, and that details.resourceName keeps
// the exact name.
func TestACreatedUserNameWithHiddenRunesIsShownAsEscapes(t *testing.T) {
	t.Parallel()
	roles := []*unstructured.Unstructured{defaultRole("alpha")}
	for _, tc := range []struct {
		name   string
		client *fakeUserClient
		want   string
	}{
		{"the Secret create fails", &fakeUserClient{userPreviewName: "u-preview1", userRealName: hiddenRunesName, roles: roles,
			secretCreateErr: clierr.New(clierr.CodeGeneric, "boom")},
			"the user " + clierr.ShellQuote(strconv.Quote(hiddenRunesName)) + " was created without a password; the CLI prints no command for an id it cannot quote; global role alpha was not bound"},
		{"the binding fails", &fakeUserClient{userPreviewName: "u-preview1", userRealName: hiddenRunesName, roles: roles,
			bindingRealErr: map[string]error{"alpha": clierr.New(clierr.CodeGeneric, "boom")}},
			"the user " + clierr.ShellQuote(strconv.Quote(hiddenRunesName)) + " was created; global role alpha was not bound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, exec, err := plan(t, tc.client, fakeSrc{body: []byte(testPassword)}, planInput("alice"))
			require.NoError(t, err)
			_, err = exec(t.Context())
			_, details := committedDetails(t, err)
			assert.Equal(t, tc.want, details["recovery"])
			assert.Equal(t, hiddenRunesName, details["resourceName"], "the data field keeps the exact name")
			recovery, _ := details["recovery"].(string)
			remedytest.AssertIDShownWithEscapes(t, recovery, hiddenRunesName)
		})
	}
}

// TestTheGrantPreviewOfAUserNameWithHiddenRunesIsLeftOut asserts the warnings
// of a create whose typed user name holds hidden runes show the name with
// escapes and print no grant command for it, and that a name the terminal
// shows unchanged, an email-style one among them, keeps its command.
func TestTheGrantPreviewOfAUserNameWithHiddenRunesIsLeftOut(t *testing.T) {
	t.Parallel()
	hidden := grantUserClause(hiddenRunesName)
	assert.Equal(t, "the username holds characters the CLI does not show ("+strconv.Quote(hiddenRunesName)+"); the CLI prints no command for it", hidden)
	remedytest.AssertIDShownWithEscapes(t, hidden, hiddenRunesName)
	assert.NotContains(t, hidden, clierr.Rancher)
	assert.Equal(t, "preview the grant with: "+clierr.Rancher+" grant 'alice@example.com' --global --role user --dry-run", grantUserClause("alice@example.com"))

	client := &fakeUserClient{userPreviewName: "u-preview1"}
	eff, _, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, planInput(hiddenRunesName))
	require.NoError(t, err)
	var found bool
	for _, w := range eff.Warnings {
		if strings.Contains(w, "the username holds characters the CLI does not show") {
			found = true
			assert.Contains(t, w, strconv.Quote(hiddenRunesName))
			remedytest.AssertIDShownWithEscapes(t, w, hiddenRunesName)
		}
	}
	assert.True(t, found, "a warning says no grant command is printed: %v", eff.Warnings)
	for _, w := range eff.Warnings {
		assert.NotContains(t, strings.ReplaceAll(w, clierr.Rancher, "rancher"), "rancher grant", "no grant command for the name")
	}
}
