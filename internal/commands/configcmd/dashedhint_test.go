package configcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestDeleteServerCommandPutsDoubleDashBeforeADashedName asserts the command
// that removes an entry, and the refusal of a URL change on the plugin's own
// login entry, for a name that starts with a dash.
func TestDeleteServerCommandPutsDoubleDashBeforeADashedName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, clierr.Rancher+" config delete-server 'prod'", deleteServerCommand("prod").String())
	assert.Equal(t, clierr.Rancher+" config delete-server -- '-h'", deleteServerCommand("-h").String())
	assert.Equal(t, clierr.Rancher+" config delete-server -- '--1-8443'", deleteServerCommand("--1-8443").String())
	err := execLoginURLChangeRefused("-x")
	assert.ErrorContains(t, err, "log it out first if it holds a login: "+clierr.Rancher+" auth logout -- '-x'; then delete it: "+clierr.Rancher+" config delete-server -- '-x'")
}

// TestEditRestrictionOffensesAreCompleteCommands asserts the text of each
// restriction of config edit: the explanation first and one complete command
// last.
func TestEditRestrictionOffensesAreCompleteCommands(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "credentialStore set; <name> is the entry to log in; log out the other entries first when there are any; use: "+
		clierr.Rancher+" auth login <name> --credential-store 'file'", credentialStoreOffense("", "file"))
}
