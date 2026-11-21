package auth

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/ui"
)

// TestTrustPromptLabelPrintsUnchangedBesideAHiddenCertificateCharacter
// asserts the trust prompt's label is written byte for byte as the code spells
// it when the certificate's subject has a hidden character. The label is not in
// double quotes, and the output has neither the character nor its escape,
// because the card removes it.
func TestTrustPromptLabelPrintsUnchangedBesideAHiddenCertificateCharacter(t *testing.T) {
	t.Parallel()
	d := tlstrust.Details{
		Kind: tlstrust.KindUnknownAuthority, Subject: "CN=rancher\u200b.example", Issuer: "CN=ca", Fingerprint: "AA:BB",
		NotBefore: "2026-01-01T00:00:00Z", NotAfter: "2027-01-01T00:00:00Z",
	}
	var errw bytes.Buffer
	confirmTrust(t.Context(), ui.New(&errw, newLineReader("n"), false, true), d, "")
	got := errw.String()
	assert.True(t, strings.HasSuffix(got, "\ntrust this certificate for this server entry? [y/N]: "), got)
	assert.NotContains(t, got, `"`)
	assert.NotContains(t, got, "\u200b")
	assert.NotContains(t, got, `\u200b`)
}
