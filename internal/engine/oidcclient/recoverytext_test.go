package oidcclient

import (
	"strings"
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

// TestRecoveryTextsLeaveOutACommandForAnUnprintableName asserts that a name a
// shell would misread leaves the command out of each recovery text and puts
// the note in once.
func TestRecoveryTextsLeaveOutACommandForAnUnprintableName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"a'b", "a\u2019b", "a\u200bb", `a\\b`} {
		assert.Equal(t, clierr.LeftOutNote, recoveryHint(name), "%q", name)

		again := rotateAgainRecovery(name, "client-secret-1")
		assert.NotContains(t, again, "rotate-secret", "%q", name)
		assert.NotContains(t, again, "the command previews it", "%q", name)
		assert.NotContains(t, again, "--apply", "%q", name)
		assert.Equal(t, 1, strings.Count(again, clierr.LeftOutNote), "%q", name)
		assert.True(t, strings.HasPrefix(again, "once the controller is healthy, rotate the same secret again"), "%q", name)

		check := checkRecovery(recoveryCommand(name), "if a new key landed")
		assert.NotContains(t, check, "get oidcclient", "%q", name)
		assert.Equal(t, "if a new key landed; "+clierr.LeftOutNote, check, "%q", name)
	}
}
