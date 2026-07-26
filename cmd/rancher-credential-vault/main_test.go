package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVersionVerbNeedsNoVault: version must work with no Vault env at all —
// it dispatches before the Vault handler is constructed.
func TestVersionVerbNeedsNoVault(t *testing.T) {
	t.Setenv("VAULT_ADDR", "")
	t.Setenv("VAULT_TOKEN", "")
	var out bytes.Buffer
	code := run([]string{"version"}, &out)
	require.Equal(t, 0, code)
	assert.Equal(t, VERSION+"\n", out.String())
}

func TestNonVersionVerbsFallThrough(t *testing.T) {
	var out bytes.Buffer
	assert.Equal(t, -1, run([]string{"get"}, &out))
	assert.Equal(t, -1, run([]string{}, &out))
	assert.Empty(t, out.String())
}
