package auth

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/remedytest"
)

// assertCAFileMeaningOnce asserts msg names --cacert <file> and says what
// <file> is once: before the command that holds the placeholder when
// commandHoldsFile is set, after the clause that names it otherwise.
func assertCAFileMeaningOnce(t *testing.T, msg string, commandHoldsFile bool) {
	t.Helper()
	remedytest.AssertClauseOnce(t, msg, config.CAFileMeaning, config.CAFileArg("--cacert"), commandHoldsFile)
}

// TestLoginMessagesThatNameTheCAFileSayWhatTheFileIs asserts each message of
// auth login that suggests --cacert <file> says what <file> is once, in the
// place the wording needs: the remedy of a variable CA on the --url path (a
// clause) and on the plain path (before the command), the remedy of a saved
// CA, both branches of the warning about a variable CA that is not saved, and
// the refusal of --cacert with no --url and no name.
func TestLoginMessagesThatNameTheCAFileSayWhatTheFileIs(t *testing.T) {
	t.Parallel()
	t.Run("a variable CA on the --url path", func(t *testing.T) {
		assertCAFileMeaningOnce(t, loginVariableRemedy(true, "prod"), false)
	})
	t.Run("a variable CA on the plain path", func(t *testing.T) {
		assertCAFileMeaningOnce(t, loginVariableRemedy(false, "prod"), true)
	})
	t.Run("a saved CA", func(t *testing.T) {
		assertCAFileMeaningOnce(t, loginSaveCARemedy("prod"), true)
	})
	t.Run("the warning for an entry with no saved CA", func(t *testing.T) {
		assertCAFileMeaningOnce(t, variableNotice("prod", ""), true)
	})
	t.Run("the warning for an entry with another saved CA", func(t *testing.T) {
		assertCAFileMeaningOnce(t, variableNotice("prod", "ca.pem"), true)
	})
	t.Run("--cacert with no --url and no name", func(t *testing.T) {
		_, err := resolveLoginTarget(&config.Config{}, "", "", "ca.pem")
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "got %v", err)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assertCAFileMeaningOnce(t, ce.Message, true)
	})
}
