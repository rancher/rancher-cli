package cli

import (
	"context"
	"slices"
	"testing"

	"github.com/rancher/rancher-cli/internal/commands/auth"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCredentialStoreOffersTheStoresThatCanBeNamed asserts the flag's
// completion: the three stores a value can name on its own, in both the
// space and the equals form, each one a value the command accepts
// verbatim. A credential helper is typed rather than offered, because no
// helper name is known without asking the machine and a suggestion that
// the command would reject is worse than none.
func TestCredentialStoreOffersTheStoresThatCanBeNamed(t *testing.T) {
	d := auth.NewLogin(t.TempDir()).Describe()
	i := slices.IndexFunc(d.Flags, func(a runtime.ArgSpec) bool { return a.Name == "credential-store" })
	require.GreaterOrEqual(t, i, 0, "auth login carries the flag its completion is for")
	require.Nil(t, d.Flags[i].Enum,
		"no enum: a typed exec:<name> has to be accepted, and an enum would reject it")

	site := CompletionSite{Kind: SiteFlagValue, FlagName: "credential-store", FlagSpelling: "--credential-store"}
	got := dispatchSite(context.Background(), site, d, nil, nil, []string{"--credential-store"})
	assert.Equal(t, []string{"auto", "file", "keychain"}, got)
	for _, v := range got {
		require.NoError(t, config.CheckStorePolicy(v), "every suggestion is a value the command accepts")
	}

	equals := CompletionSite{Kind: SiteFlagValue, FlagName: "credential-store", FlagSpelling: "--credential-store", EqualsForm: true}
	assert.Equal(t, []string{"--credential-store=auto", "--credential-store=file", "--credential-store=keychain"},
		dispatchSite(context.Background(), equals, d, nil, nil, []string{"--credential-store="}))

	// A command without the flag owns nothing here.
	other := CompletionSite{Kind: SiteFlagValue, FlagName: "credential-store", FlagSpelling: "--credential-store"}
	assert.False(t, credentialStoreSite(other, canDescriptor()))
}
