package token

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/remedytest"
)

// assertCAFileMeaningOnce asserts msg names flag with the <file> placeholder
// and says what <file> is once: before the command that holds the
// placeholder when commandHoldsFile is set, after the clause that names it
// otherwise.
func assertCAFileMeaningOnce(t *testing.T, msg, flag string, commandHoldsFile bool) {
	t.Helper()
	remedytest.AssertClauseOnce(t, msg, config.CAFileMeaning, config.CAFileArg(flag), commandHoldsFile)
}

// TestGetTokenTLSRemedyWithTheVariableSaysWhatTheFileIs asserts the clause of
// the legacy token command and auth get-token for a CA that came from
// RANCHER_CLI_CACERT names its own CA flag with <file> and says what the
// file is once, after that clause.
func TestGetTokenTLSRemedyWithTheVariableSaysWhatTheFileIs(t *testing.T) {
	t.Parallel()
	variable := config.CASource{Kind: config.CASourceVariable}
	assertCAFileMeaningOnce(t, tlsRemedyFor(variable, false).CA, "--cacert", false)
	assertCAFileMeaningOnce(t, tlsRemedyFor(variable, true).CA, "--cacerts", false)
}

// TestLoginRemedyPrintsItsFactsWithItsCommand asserts a print of a
// LoginRemedy value (%v, %+v, %s) is its instruction with no lead: the clause
// that says what <file> is comes once, before the command that holds it, for
// a login whose CA is inline, and the text holds no field of the struct.
func TestLoginRemedyPrintsItsFactsWithItsCommand(t *testing.T) {
	t.Parallel()
	const pem = "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n"
	l := NoTerminalLogin(nil, "", "c-1", "https://r.example.com", pem, config.CASource{Kind: config.CASourceVariable}, false)
	require.True(t, l.InlineCA)
	want := l.Instruction("")
	for _, verb := range []string{"%v", "%+v", "%s"} {
		got := fmt.Sprintf(verb, l)
		assert.Equal(t, want, got, verb)
		remedytest.AssertClauseOnce(t, got, config.CAFileMeaning, config.CAFileArg("--cacert"), true)
	}
	assert.Equal(t, want, fmt.Sprint(&l), "a pointer prints the same")
}
