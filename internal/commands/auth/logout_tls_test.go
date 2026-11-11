package auth

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/remedytest"
	"github.com/rancher/rancher-cli/internal/runtime"
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
	text := strings.Join(remedytest.DetailTexts(p.TextDetails()), "\n")
	assert.Contains(t, text, "not revoked: ext/token-a on https://r.example (failed, TLS_TRUST, retry nextLogin)")
	assert.Contains(t, text, "  the server presented a certificate signed by an unknown authority")
	assert.Contains(t, text, "  fingerprint: ab:cd")
}

// TestANotRevokedCertificateFailurePrintsItsCommandsUnquoted asserts the
// default-format error text of auth logout --all and auth delete-token, for a
// token that may still be live because its certificate failed verification:
// with a config directory the CLI can print, the not-revoked block holds the
// command written out, with no remedy marker and no quote around it; with one
// it cannot print, no command, no marker and no quote is left, and the text
// says why.
func TestANotRevokedCertificateFailurePrintsItsCommandsUnquoted(t *testing.T) {
	t.Parallel()
	message := "the server presented a certificate signed by an unknown authority; save it for entry 'prod' with: " +
		clierr.Rancher + " config update-server 'prod' --cacert <file>"
	notRevoked := []token.NotRevokedToken{{
		Ref: &token.TokenRef{Kind: token.TokenKindExt, Name: "token-a"}, Source: "cache", Server: "https://r.example", Reason: "failed",
		Code: clierr.CodeTLSTrust, Error: message,
		Certificate: &tlstrust.Details{Kind: tlstrust.KindUnknownAuthority, Subject: "CN=private-ca", Fingerprint: "ab:cd"},
		Retry:       "nextLogin",
	}}
	rerun := clierr.Rancher + " auth logout --all"
	payloads := map[string]clierr.TextDetails{
		"logout --all":      LogoutAllPartialFailure{LoggedOut: []string{"prod"}, NotRevoked: notRevoked, Rerun: rerun},
		"auth delete-token": token.DeleteTokenFailure{Deleted: []string{"k"}, NotRevoked: notRevoked},
	}
	render := func(t *testing.T, streams runtime.IO, d clierr.TextDetails) string {
		t.Helper()
		var out bytes.Buffer
		require.NoError(t, runtime.RenderError(&out, "", streams.ExpandError(clierr.New(clierr.CodeGeneric, "the command failed").WithDetails(d))))
		return out.String()
	}
	for name, d := range payloads {
		t.Run(name+" with a directory it can print", func(t *testing.T) {
			t.Parallel()
			streams := runtime.IO{RemedyDir: t.TempDir()}
			text := render(t, streams, d)
			assert.NotContains(t, text, clierr.RemedyMarker, "no remedy marker is left")
			assert.NotContains(t, text, `"`, "no quote is left around a command")
			assert.Contains(t, text, "\n    "+streams.ExpandText(message)+"\n", "the message line holds the expanded command as it is")
			if _, ok := d.(LogoutAllPartialFailure); ok {
				assert.Contains(t, text, "to finish, run: "+streams.ExpandText(rerun)+"\n")
			}
		})
		t.Run(name+" with a directory it cannot print", func(t *testing.T) {
			t.Parallel()
			text := render(t, runtime.IO{RemedyDir: "/d\x01"}, d)
			assert.NotContains(t, text, clierr.RemedyMarker, "no remedy marker is left")
			assert.NotContains(t, text, `"`, "no quoted stub is left")
			assert.NotContains(t, text, "config update-server", "no command is offered")
			assert.NotContains(t, text, "auth logout --all", "no command is offered")
			assert.Contains(t, text, runtime.UnprintableConfigDirNote)
		})
	}
}
