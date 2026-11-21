package auth

import (
	"bytes"
	"crypto/x509"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// TestAFailedReverifyPrintsTheCertificateNamesWithHiddenCharactersRemoved asserts the error of a
// certificate that did not re-verify prints the DNS names crypto/x509 lists with each hidden character
// removed, as a TLS_TRUST error prints the certificate facts in its message.
func TestAFailedReverifyPrintsTheCertificateNamesWithHiddenCharactersRemoved(t *testing.T) {
	t.Parallel()
	cause := x509.HostnameError{Certificate: &x509.Certificate{DNSNames: []string{"a\u200bb.example"}}, Host: "other.example"}
	ce := reverifyFailedError(tlstrust.Details{Fingerprint: "ab:cd"}, cause)
	var out bytes.Buffer
	require.NoError(t, runtime.RenderError(&out, "", ce))
	assert.Equal(t, "error (TLS_TRUST): the accepted certificate (fingerprint ab:cd) could not be confirmed against the "+
		"server: x509: certificate is valid for ab.example, not other.example; the certificate was not stored - try again\n", out.String())
}
