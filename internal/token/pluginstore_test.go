package token

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// unsupportedStore is a credential store that cannot store a login credential.
type unsupportedStore struct{ config.CredStore }

func (unsupportedStore) SetTokenWithOutcome(config.ServerRef, string, config.PriorCredential) (config.CommitOutcome, error) {
	return config.CommitOutcome{State: clierr.CredentialNotWritten}, fmt.Errorf("helper: %w", config.ErrTokenStoreUnsupported)
}

// TestAPluginLoginInAStoreThatCannotHoldItNamesTheCommandThatTurnsReuseOff
// asserts the exec plugin's sign-in, with a credential store that cannot store
// a login credential, fails with a message whose last words are the command
// that turns token reuse off, which ends the message and names the config
// directory of the invocation.
func TestAPluginLoginInAStoreThatCannotHoldItNamesTheCommandThatTurnsReuseOff(t *testing.T) {
	x := newPluginFixture(t, true, testrancher.Options{})
	store := unsupportedStore{CredStore: x.store}
	cmd := x.command(store)
	cmd.openStore = func(string, string) (config.CredStore, string, error) { return store, "", nil }

	_, err := cmd.Execute(t.Context(), x.input("c-test", "n"), localPrompter())

	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Contains(t, ce.Message, "cannot store a login credential")
	expanded := runtime.IO{RemedyDir: "/d"}.ExpandText(ce.Message)
	assert.Contains(t, expanded, "fix the store, or turn the setting off with: rancher --config '/d' config set-token-reuse off")
	assert.True(t, strings.HasSuffix(expanded, "set-token-reuse off"), "the command ends the message: %q", expanded)
	assertCommandsEndTheirSentence(t, expanded)
}
