package token

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// holdsMarkerRune reports whether s holds one of the five characters a
// remedy marker is made of, U+2060 to U+2064.
func holdsMarkerRune(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r >= '\u2060' && r <= '\u2064' })
}

// TestAReportedCertificateFailureHoldsNoCharacterOfTheMarker asserts the
// message of a not-revoked token whose entry stays, the joined text details
// of the report and the joined text lines of the deletion hold none of the
// five characters of the remedy marker, U+2060 to U+2064, so no bare U+2063
// and no other copy of a marker remains in them. Mutation: drop
// ExpandRemedies in tlsMessage.
func TestAReportedCertificateFailureHoldsNoCharacterOfTheMarker(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	login := x.login("prod", "alice")
	cert := tlsCert(t, "private-ca")
	d, rep := x.runSeeded(x.req(DeleteForLogout, "prod"), "prod", false, tlsFailure(unknownAuthorityFailure(cert)), tlsFailure(unknownAuthorityFailure(cert)), nil)
	n := notRevoked(t, rep, login.Ref.Name)
	assert.Contains(t, n.Error, "rancher config update-server 'prod' --cacert <file>")
	for name, text := range map[string]string{
		"the message":      n.Error,
		"the text details": strings.Join(rep.TextDetails(), "\n"),
		"the text lines":   strings.Join(d.TextLines(), "\n"),
	} {
		assert.False(t, holdsMarkerRune(text), "%s holds a character of the remedy marker: %q", name, text)
		assert.NotContains(t, text, "\u2063", name)
	}
}

// TestACertificateFailureOfARevokeNamesItsCommandsPlain asserts, for every
// row of what revokes the token and whether RANCHER_CLI_CACERT supplied the
// CA, that the message of a certificate failure in the revoke report, and the
// text lines of the report, write each command as the plain command and hold
// no character of the remedy marker: the report is data that no invocation
// expands. Mutation: drop ExpandRemedies in tlsMessage.
func TestACertificateFailureOfARevokeNamesItsCommandsPlain(t *testing.T) {
	for _, row := range tlsRevokeRows() {
		for _, fromVariable := range []bool{false, true} {
			name, want := row.name+", the CA from the entry or none", row.caEntry
			if fromVariable {
				name, want = row.name+", the CA from the variable", row.caVariable
			}
			t.Run(name, func(t *testing.T) {
				x, req, token := row.setup(t)
				cert := tlsCert(t, "private-ca")
				_, rep := x.runSeeded(req, "prod", fromVariable, tlsFailure(unknownAuthorityFailure(cert)), tlsFailure(unknownAuthorityFailure(cert)), nil)
				n := notRevoked(t, rep, token)
				text := strings.Join(rep.TextDetails(), "\n")
				if strings.Contains(want, "rancher config update-server") {
					assert.Contains(t, n.Error, "rancher config update-server", "the message holds the command")
					assert.Contains(t, text, "rancher config update-server", "the report text holds the command")
				}
				assert.False(t, clierr.HasRemedy(n.Error))
				assert.False(t, clierr.HasRemedy(text))
				assert.False(t, holdsMarkerRune(n.Error), "the message holds a character of the remedy marker: %q", n.Error)
				assert.False(t, holdsMarkerRune(text), "the report text holds a character of the remedy marker: %q", text)
			})
		}
	}
}
