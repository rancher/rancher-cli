package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/remedytest"
)

// TestCAFileHelpersWriteThePlaceholderAndItsMeaning asserts the two helpers
// of every message that suggests a CA flag with a file: the flag with the
// placeholder after it, and the clause that says what the placeholder is.
func TestCAFileHelpersWriteThePlaceholderAndItsMeaning(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "--cacert <file>", CAFileArg("--cacert"))
	assert.Equal(t, "--cacerts <file>", CAFileArg("--cacerts"))
	assert.Equal(t, "<file> is a file that holds the server's CA certificate", CAFileMeaning)
}

// TestCAFileMeaningComesOnceBeforeTheCommandInTheMessagesOfConfig asserts the
// messages of this package that suggest --cacert <file> say what <file> is
// once, before the command that holds it: both branches of the wording of a
// certificate failure for a named entry, and the refusal that asks for the
// CA of the exec plugin's own login entry.
func TestCAFileMeaningComesOnceBeforeTheCommandInTheMessagesOfConfig(t *testing.T) {
	cfg := &Config{
		CurrentServer: "prod",
		Servers:       map[string]Server{"prod": {URL: "https://prod.example.com", CA: "PROD-CA"}},
	}
	arg := CAFileArg("--cacert")
	for _, tc := range []struct {
		name, variable string
	}{
		{"a named entry", ""},
		{"a named entry and the variable", "ENV-CA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvURL, "")
			t.Setenv(EnvCACert, tc.variable)
			msg := cfg.EffectiveConnection().TLSRemedy.CA
			remedytest.AssertClauseOnce(t, msg, CAFileMeaning, arg, true)
		})
	}

	t.Run("the plugin login entry with a saved CA", func(t *testing.T) {
		const entry = ExecLoginPrefix + "r.example.test"
		ce := ErrNotLoggedInFor(map[string]string{entry: "https://r.example.test"}, map[string]bool{entry: true})
		require.NotNil(t, ce)
		remedytest.AssertClauseOnce(t, ce.Message, CAFileMeaning, arg, true)
	})
}
