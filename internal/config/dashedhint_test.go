package config

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestLoginAndLogoutHintsPutDoubleDashBeforeADashedName asserts the login and
// logout hints for a name that starts with a dash, and that an ordinary name
// keeps its text.
func TestLoginAndLogoutHintsPutDoubleDashBeforeADashedName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, clierr.Rancher+" auth login -- '-x'", LoginHintFor("-x"))
	assert.Equal(t, clierr.Rancher+" auth logout -- '-x'", LogoutHintFor("-x"))
	assert.Equal(t, clierr.Rancher+" auth login 'prod'", LoginHintFor("prod"))
	assert.Equal(t, clierr.Rancher+" auth logout 'prod'", LogoutHintFor("prod"))
}

// TestRemedyTextsPutDoubleDashBeforeADashedName asserts every command with
// a name or token in the texts of this package: the flags come first and the
// name follows "--".
func TestRemedyTextsPutDoubleDashBeforeADashedName(t *testing.T) {
	t.Parallel()
	ref := clierr.TokenRef{Name: "-x"}
	assert.Equal(t, "no server is recorded for token '-x'; delete it while signed in to the server that issued it: "+
		clierr.Rancher+" delete token --apply --yes -- '-x'", noServerText(ref))
	assert.Contains(t, recordServerText(ref, errors.New("bad")), "delete it there while signed in to that server: "+
		clierr.Rancher+" delete token --apply --yes -- '-x'")
	assert.Contains(t, EntryOrigin("-x").Text(ErrServerURLScheme), "change the entry's URL with "+
		clierr.Rancher+" config update-server --url <url> -- '-x'")
	assert.Equal(t, ", or keep its credential in a plaintext file on purpose with: "+clierr.Rancher+" auth login --credential-store file -- '-x'",
		fileStoreAlternative(&Config{Servers: map[string]Server{"-x": {URL: "https://x.example.com"}}}, "-x"))
	cfg := &Config{CurrentServer: "-x", Servers: map[string]Server{"-x": {URL: "https://x.example.com"}}}
	assert.Equal(t, "; <file> is a file that holds the server's CA certificate; save it for this entry with: "+
		clierr.Rancher+" config update-server --cacert <file> -- '-x'", cfg.EffectiveConnection().TLSRemedy.CA)
}
