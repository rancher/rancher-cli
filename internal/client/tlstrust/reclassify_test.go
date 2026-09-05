package tlstrust

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// untypedVerifyError builds the error shape a platform verifier (macOS
// Security.framework, Windows CryptoAPI) produces: a
// tls.CertificateVerificationError whose Err is a bare string, carrying the
// captured chain. Observed live 2026-08-25 against a dynamiclistener-default
// Rancher server dialed with system roots on macOS.
func untypedVerifyError(chain []*x509.Certificate) error {
	return &tls.CertificateVerificationError{
		UnverifiedCertificates: chain,
		Err:                    errors.New("x509: certificate is not trusted"),
	}
}

func leafOf(t *testing.T, c tls.Certificate) *x509.Certificate {
	t.Helper()
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	require.NoError(t, err)
	return leaf
}

// TestClassifyUntypedUnknownAuthorityLeafOnly is the live-incident repro: a
// leaf issued by an absent CA, presented WITHOUT that CA (dynamiclistener's
// leaf-only shape), failing with an untyped platform error. It must
// classify unknownAuthority with the --cacert guidance - not invalidChain
// with contact-the-operator.
func TestClassifyUntypedUnknownAuthorityLeafOnly(t *testing.T) {
	t.Parallel()
	ca, caKey := genCA(t, "dynamiclistener-ca@test")
	leafTLS := genLeaf(t, ca, caKey, "rancher.test", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	leaf := leafOf(t, leafTLS)

	ce := Classify(untypedVerifyError([]*x509.Certificate{leaf}), "rancher.test:443", time.Now())
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	assert.Contains(t, ce.Message, "unknown authority")
	assert.Contains(t, ce.Message, "--cacert")
	details, ok := ce.Details.(Details)
	require.True(t, ok)
	assert.Equal(t, KindUnknownAuthority, details.Kind)
	assert.Equal(t, Fingerprint(leaf), details.Fingerprint,
		"the candidate must be the presented leaf - the only pinnable certificate observed")
}

// TestClassifyUntypedUnknownAuthorityFullChain: the same untrusted server
// presenting leaf+CA must classify unknownAuthority with the CA as the
// pinnable candidate (the anchor walk finds it).
func TestClassifyUntypedUnknownAuthorityFullChain(t *testing.T) {
	t.Parallel()
	ca, caKey := genCA(t, "private-ca")
	leafTLS := genLeaf(t, ca, caKey, "rancher.test", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	leaf := leafOf(t, leafTLS)

	ce := Classify(untypedVerifyError([]*x509.Certificate{leaf, ca}), "rancher.test:443", time.Now())
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.Equal(t, KindUnknownAuthority, details.Kind)
	assert.Equal(t, Fingerprint(ca), details.Fingerprint,
		"the candidate must be the chain's own anchor, the pinnable CA")
}

// TestClassifyUntypedExpired: the validity-window check answers from the
// chain alone, before any hostname or trust reasoning.
func TestClassifyUntypedExpired(t *testing.T) {
	t.Parallel()
	ca, caKey := genCA(t, "some-ca")
	leafTLS := genLeaf(t, ca, caKey, "rancher.test", time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
	leaf := leafOf(t, leafTLS)

	ce := Classify(untypedVerifyError([]*x509.Certificate{leaf, ca}), "rancher.test:443", time.Now())
	require.NotNil(t, ce)
	assert.Contains(t, ce.Message, "expired")
	assert.Equal(t, KindExpired, ce.Details.(Details).Kind)
}

// TestClassifyUntypedNotYetValid covers the other clock direction.
func TestClassifyUntypedNotYetValid(t *testing.T) {
	t.Parallel()
	ca, caKey := genCA(t, "some-ca")
	leafTLS := genLeaf(t, ca, caKey, "rancher.test", time.Now().Add(time.Hour), time.Now().Add(2*time.Hour))
	leaf := leafOf(t, leafTLS)

	ce := Classify(untypedVerifyError([]*x509.Certificate{leaf, ca}), "rancher.test:443", time.Now())
	require.NotNil(t, ce)
	assert.Contains(t, ce.Message, "not yet valid")
	assert.Equal(t, KindNotYetValid, ce.Details.(Details).Kind)
}

// TestClassifyUntypedHostnameMismatch: an in-window chain for a DIFFERENT
// name must classify hostnameMismatch (the platform reports this untyped
// too); the port on the compared host must not defeat the match.
func TestClassifyUntypedHostnameMismatch(t *testing.T) {
	t.Parallel()
	ca, caKey := genCA(t, "some-ca")
	leafTLS := genLeaf(t, ca, caKey, "good.test", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	leaf := leafOf(t, leafTLS)

	ce := Classify(untypedVerifyError([]*x509.Certificate{leaf, ca}), "other.test:443", time.Now())
	require.NotNil(t, ce)
	assert.Contains(t, ce.Message, "not valid for other.test:443")
	assert.Equal(t, KindHostnameMismatch, ce.Details.(Details).Kind)
}

// TestClassifyUntypedMatchingHostIsNotMismatch: a chain valid for the
// dialed host (port stripped) must NOT read as hostnameMismatch - it falls
// through to the trust reasoning and classifies unknownAuthority. This
// deliberately covers platform-only distrust of a sound chain (revoked
// root, CT policy): the --cacert remedy is mechanically real there - a
// pinned pool uses Go's verifier - and the details fingerprint supports the
// out-of-band check before anyone pins.
func TestClassifyUntypedMatchingHostIsNotMismatch(t *testing.T) {
	t.Parallel()
	ca, caKey := genCA(t, "some-ca")
	leafTLS := genLeaf(t, ca, caKey, "rancher.test", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	leaf := leafOf(t, leafTLS)

	ce := Classify(untypedVerifyError([]*x509.Certificate{leaf, ca}), "rancher.test:443", time.Now())
	require.NotNil(t, ce)
	assert.Equal(t, KindUnknownAuthority, ce.Details.(Details).Kind)
}

// TestClassifyUntypedEmptyHost: a Classify caller with no host still gets
// the trust classification (the hostname check is simply skipped), matching
// Go's taxonomy where an unresolvable path is unknown authority regardless
// of name checking.
func TestClassifyUntypedEmptyHost(t *testing.T) {
	t.Parallel()
	ca, caKey := genCA(t, "real-issuer")
	leafTLS := genLeaf(t, ca, caKey, "rancher.test", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	leaf := leafOf(t, leafTLS)

	ce := Classify(untypedVerifyError([]*x509.Certificate{leaf}), "", time.Now())
	require.NotNil(t, ce)
	assert.Equal(t, KindUnknownAuthority, ce.Details.(Details).Kind)
}

// TestClassifyUntypedExtraneousStaleCertDoesNotReadAsExpired: a server
// bundling an unrelated expired certificate beside an in-window untrusted
// leaf must classify unknownAuthority - the stale bystander is not on the
// leaf's signing path and its dates say nothing about why verification
// failed.
func TestClassifyUntypedExtraneousStaleCertDoesNotReadAsExpired(t *testing.T) {
	t.Parallel()
	ca, caKey := genCA(t, "private-ca")
	leafTLS := genLeaf(t, ca, caKey, "rancher.test", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	leaf := leafOf(t, leafTLS)
	staleCA, staleKey := genCA(t, "old-forgotten-root")
	staleTLS := genLeaf(t, staleCA, staleKey, "stale.test", time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
	stale := leafOf(t, staleTLS)

	ce := Classify(untypedVerifyError([]*x509.Certificate{leaf, stale}), "rancher.test:443", time.Now())
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.Equal(t, KindUnknownAuthority, details.Kind)
	assert.Equal(t, Fingerprint(leaf), details.Fingerprint,
		"the candidate must be the leaf, never the stale bystander")
}

// TestClassifyUntypedEmptyChain: an untyped failure with no captured chain
// stays the nullable-details invalidChain case; reclassification must not
// panic or fabricate metadata.
func TestClassifyUntypedEmptyChain(t *testing.T) {
	t.Parallel()
	ce := Classify(untypedVerifyError(nil), "rancher.test:443", time.Now())
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.Equal(t, KindInvalidChain, details.Kind)
	assert.Empty(t, details.Fingerprint)
}

// TestClassifyUntypedBracketedIPv6HostWithoutPortIsNotMismatch asserts that
// the bracketed default-port spelling a caller's host[:port] label produces
// for an IPv6 server, "[::1]", is unbracketed before the hostname check: a
// certificate whose IP SAN covers ::1 must classify as unknown authority
// (the --cacert remedy), never as a hostname mismatch. The explicit-port
// spelling is asserted alongside as the control.
func TestClassifyUntypedBracketedIPv6HostWithoutPortIsNotMismatch(t *testing.T) {
	t.Parallel()
	_, leaf := selfSignedLeaf(t, "::1")
	for _, host := range []string{"[::1]", "[::1]:8443"} {
		ce := Classify(untypedVerifyError([]*x509.Certificate{leaf}), host, time.Now())
		require.NotNil(t, ce, host)
		assert.Equal(t, KindUnknownAuthority, ce.Details.(Details).Kind, host)
	}
	_, other := selfSignedLeaf(t, "::2")
	ce := Classify(untypedVerifyError([]*x509.Certificate{other}), "[::1]", time.Now())
	require.NotNil(t, ce)
	assert.Equal(t, KindHostnameMismatch, ce.Details.(Details).Kind, "a SAN for another address is still a mismatch")
	assert.Contains(t, ce.Message, "not valid for [::1]")
}
