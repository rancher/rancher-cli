package oidcclient

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestRecoveryTextsPutDoubleDashBeforeADashedName asserts the commands the
// recovery texts of this package name, for an ordinary name and for names
// that start with a dash.
func TestRecoveryTextsPutDoubleDashBeforeADashedName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, clierr.Rancher+" get oidcclient 'my-app' -o json", recoveryHint("my-app"))
	assert.Equal(t, clierr.Rancher+" get oidcclient -o json -- '-x'", recoveryHint("-x"))
	const lead = "once the controller is healthy, rotate the same secret again (the command previews it; add --apply --yes after reading the preview): "
	assert.Equal(t, lead+clierr.Rancher+" oidc-client rotate-secret 'my-app' 'client-secret-1' --dry-run", rotateAgainRecovery("my-app", "client-secret-1"))
	assert.Equal(t, lead+clierr.Rancher+" oidc-client rotate-secret --dry-run -- '-x' 'client-secret-1'", rotateAgainRecovery("-x", "client-secret-1"))
	assert.Equal(t, lead+clierr.Rancher+" oidc-client rotate-secret --dry-run -- 'my-app' '-s'", rotateAgainRecovery("my-app", "-s"))
}
