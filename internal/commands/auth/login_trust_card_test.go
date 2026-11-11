package auth

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
)

// TestTrustCardTextIsUnchanged asserts the trust prompt's card for a
// certificate with safe values and for one whose subject and issuer hold
// hidden characters, which the card shows with those characters removed.
func TestTrustCardTextIsUnchanged(t *testing.T) {
	t.Parallel()
	const tail = "the certificate is not Rancher's generated CA: it may come from your organization's CA, or from a proxy between you and the server that replaces certificates\n" +
		"confirm the fingerprint with the server's administrator, whatever the subject above says"
	safe := tlstrust.Details{
		Kind: tlstrust.KindUnknownAuthority, Subject: "CN=rancher.example", Issuer: "CN=ca", Fingerprint: "AA:BB",
		NotBefore: "2026-01-01T00:00:00Z", NotAfter: "2027-01-01T00:00:00Z",
	}
	require.Equal(t,
		"certificate authority: CN=rancher.example\n"+
			"issuer: CN=ca\n"+
			"fingerprint: AA:BB\n"+
			"valid: 2026-01-01T00:00:00Z to 2027-01-01T00:00:00Z\n"+tail,
		trustCard(safe, ""))
	require.Equal(t,
		"warning: this server previously presented a certificate with fingerprint OLD; it now presents a different one - the certificate may have been replaced, or another server may be answering in its place\n"+
			"certificate authority: CN=rancher.example\n"+
			"issuer: CN=ca\n"+
			"fingerprint: AA:BB\n"+
			"valid: 2026-01-01T00:00:00Z to 2027-01-01T00:00:00Z\n"+tail,
		trustCard(safe, "OLD"))

	unsafe := safe
	unsafe.Subject = "CN=rancher\u200b.example\x1b[2J"
	unsafe.Issuer = "CN=c\u202ea"
	require.Equal(t,
		"certificate authority: CN=rancher.example[2J\n"+
			"issuer: CN=ca\n"+
			"fingerprint: AA:BB\n"+
			"valid: 2026-01-01T00:00:00Z to 2027-01-01T00:00:00Z\n"+tail,
		trustCard(unsafe, ""))
}
