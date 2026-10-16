package auth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/token"
)

// TestLogoutAllFailureDetailsPrintTheCertificateFailure asserts the text
// details of auth logout --all's partial failure print the message and the
// fingerprint of a certificate failure under the token. Mutation: keep
// TextLine in LogoutAllPartialFailure.TextDetails.
func TestLogoutAllFailureDetailsPrintTheCertificateFailure(t *testing.T) {
	cert := tlstrust.Details{Kind: tlstrust.KindUnknownAuthority, Subject: "CN=private-ca", Fingerprint: "ab:cd"}
	p := LogoutAllPartialFailure{
		LoggedOut: []string{"prod"},
		NotRevoked: []token.NotRevokedToken{{
			Ref: &token.TokenRef{Kind: token.TokenKindExt, Name: "token-a"}, Source: "cache", Server: "https://r.example", Reason: "failed",
			Code: clierr.CodeTLSTrust, Error: "the server presented a certificate signed by an unknown authority", Certificate: &cert, Retry: "nextLogin",
		}},
	}
	text := strings.Join(p.TextDetails(), "\n")
	assert.Contains(t, text, "not revoked: ext/token-a on https://r.example (failed, TLS_TRUST, retry nextLogin)")
	assert.Contains(t, text, "  the server presented a certificate signed by an unknown authority")
	assert.Contains(t, text, "  fingerprint: ab:cd")
}
