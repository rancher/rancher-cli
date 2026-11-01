package extres

import (
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
