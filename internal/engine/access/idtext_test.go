package access

import (
	"errors"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/remedytest"
)

// hiddenRunesName holds a direction override, a zero-width space, a bell, a
// newline and the start of a remedy marker followed by the word the marker
// ends with.
const hiddenRunesName = "u-\u202e\u200b\a\n\u2063rancher-x"

// TestATypedNameWithHiddenRunesIsShownAsEscapesInUserErrors asserts the
// not-found and ambiguous-match errors for a typed user name show it with
// escapes, never with the runes removed.
func TestATypedNameWithHiddenRunesIsShownAsEscapesInUserErrors(t *testing.T) {
	t.Parallel()
	quoted := strconv.Quote(hiddenRunesName)

	ce := findUserError(t, usersClient(numberedUsers(2, "member")...), hiddenRunesName)
	assert.Equal(t, "user "+quoted+" not found", ce.Message)
	remedytest.AssertIDShownWithEscapes(t, ce.Message, hiddenRunesName)

	dup := numberedUsers(3, hiddenRunesName)
	ce = findUserError(t, usersClient(dup...), hiddenRunesName)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Equal(t, "3 users match "+quoted+"; pass the user id", ce.Message)
	remedytest.AssertIDShownWithEscapes(t, ce.Message, hiddenRunesName)
}

// TestTheProxyIdentityMismatchShowsNamesWithHiddenRunesAsEscapes asserts the
// error for a proxy that answered as another user shows both names with
// escapes, never with the runes removed.
func TestTheProxyIdentityMismatchShowsNamesWithHiddenRunesAsEscapes(t *testing.T) {
	t.Parallel()
	out := classifyProbe("c-1", hiddenRunesName, callerIdentity{Username: hiddenRunesName + "-other"}, reviewResult{kind: outcomeAnswer}, 1, 3)
	require.Error(t, out.terminal)
	ce, ok := errors.AsType[*clierr.Error](out.terminal)
	require.True(t, ok)
	assert.Equal(t, "the proxy for cluster c-1 answered as "+strconv.Quote(hiddenRunesName+"-other")+", not "+strconv.Quote(hiddenRunesName)+
		"; impersonation was not applied (evaluated 1 of 3 clusters)", ce.Message)
	remedytest.AssertIDShownWithEscapes(t, ce.Message, hiddenRunesName)
	remedytest.AssertIDShownWithEscapes(t, ce.Message, hiddenRunesName+"-other")
}

// TestNoCommandNamesAGroupOrServiceAccountWithHiddenRunes asserts the clause
// that suggests a command for a typed group or service account name leaves
// the command out when the name holds hidden runes, shows the name with
// escapes, and keeps the command for a name the terminal shows unchanged.
func TestNoCommandNamesAGroupOrServiceAccountWithHiddenRunes(t *testing.T) {
	t.Parallel()
	for name, clause := range map[string]string{
		"group":           groupPointQuestion(hiddenRunesName),
		"service account": serviceAccountList(hiddenRunesName),
		"group message":   groupListMessage(hiddenRunesName),
	} {
		assert.Contains(t, clause, "holds characters the CLI does not show ("+strconv.Quote(hiddenRunesName)+"); the CLI prints no command for it", name)
		assert.NotContains(t, clause, "--as", name)
		remedytest.AssertIDShownWithEscapes(t, clause, hiddenRunesName, name)
	}
	assert.Equal(t, pointQuestionPlaceholders+"; ask a point question with: "+groupAccessCan("github_org://eng").String(), groupPointQuestion("github_org://eng"))
	assert.Equal(t, "use "+clierr.Rancher+" access list --cluster <cluster> --as 'system:serviceaccount:ns:sa'", serviceAccountList("system:serviceaccount:ns:sa"))
}
