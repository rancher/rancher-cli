package extres

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestRecoveryTextsKeepTheGatesAndPutDoubleDashBeforeADashedName asserts the
// text that says how to create a token or a kubeconfig again: the command that
// deletes the one the server created keeps its gates, and a name that starts
// with a dash follows the flags and "--".
func TestRecoveryTextsKeepTheGatesAndPutDoubleDashBeforeADashedName(t *testing.T) {
	t.Parallel()
	const lead = "to create it again, delete this one first: "
	assert.Equal(t, lead+clierr.Rancher+" delete token 'token-abc12' --apply --yes", tokenRecovery("token-abc12"))
	assert.Equal(t, lead+clierr.Rancher+" delete token --apply --yes -- '-x'", tokenRecovery("-x"))
	assert.Equal(t, lead+clierr.Rancher+" delete kubeconfig 'kubeconfig-abc12' --apply --yes", kubeconfigRecovery("kubeconfig-abc12"))
	assert.Equal(t, lead+clierr.Rancher+" delete kubeconfig --apply --yes -- '-x'", kubeconfigRecovery("-x"))
}

// TestRecoveryTextsLeaveOutTheDeleteForANameAShellMisreads asserts that a name
// holding a quote, a curly quote, a zero-width space or two backslashes in a
// row leaves the delete command out of the recovery text, which then holds the
// command that lists them and the note once.
func TestRecoveryTextsLeaveOutTheDeleteForANameAShellMisreads(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"a'b", "a\u2019b", "a\u200bb", `a\\b`} {
		token := tokenRecovery(name)
		assert.NotContains(t, token, "delete token", "%q", name)
		assert.Contains(t, token, clierr.Rancher+" get tokens", "%q", name)
		assert.Equal(t, 1, strings.Count(token, clierr.LeftOutNote), "%q", name)

		kubeconfig := kubeconfigRecovery(name)
		assert.NotContains(t, kubeconfig, "delete kubeconfig", "%q", name)
		assert.Contains(t, kubeconfig, clierr.Rancher+" get kubeconfigs", "%q", name)
		assert.Equal(t, 1, strings.Count(kubeconfig, clierr.LeftOutNote), "%q", name)
	}
}
