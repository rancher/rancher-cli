package token

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/remedytest"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// holdsMarkerRune reports whether s holds one of the five characters a
// remedy marker is made of, U+2060 to U+2064.
func holdsMarkerRune(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r >= '\u2060' && r <= '\u2064' })
}

// TestAReportedCertificateFailureKeepsTheMarkerOfItsCommands asserts the
// message of a not-revoked token whose entry stays, the joined text details
// of the report and the joined text lines of the deletion open each command
// with the marker; the invocation that prints them names its config
// directory in them, and hold no bare U+2063. Mutation: expand the marker
// away in tlsMessage.
func TestAReportedCertificateFailureKeepsTheMarkerOfItsCommands(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	login := x.login("prod", "alice")
	cert := tlsCert(t, "private-ca")
	d, rep := x.runSeeded(x.req(DeleteForLogout, "prod"), "prod", false, tlsFailure(unknownAuthorityFailure(cert)), tlsFailure(unknownAuthorityFailure(cert)), nil)
	n := notRevoked(t, rep, login.Ref.Name)
	assert.Contains(t, clierr.ExpandRemedies(n.Error, ""), "rancher config update-server 'prod' --cacert <file>")
	for name, text := range map[string]string{
		"the message":      n.Error,
		"the text details": strings.Join(remedytest.DetailTexts(rep.TextDetails()), "\n"),
		"the text lines":   strings.Join(d.TextLines(), "\n"),
	} {
		assert.True(t, clierr.HasRemedy(text), "%s holds a marked command: %q", name, text)
		assert.NotContains(t, strings.ReplaceAll(text, clierr.Rancher, ""), "\u2063", name)
	}
}

// TestACertificateFailureOfARevokeNamesItsCommandsWithTheMarker asserts, for
// every row of what revokes the token and whether RANCHER_CLI_CACERT
// supplied the CA, that the message of a certificate failure in the revoke
// report, and the text lines of the report, open each command with the
// marker, and that the expanded command with a config directory names it.
// Mutation: expand the marker away in tlsMessage.
func TestACertificateFailureOfARevokeNamesItsCommandsWithTheMarker(t *testing.T) {
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
				text := strings.Join(remedytest.DetailTexts(rep.TextDetails()), "\n")
				if strings.Contains(want, "rancher config update-server") {
					assert.Contains(t, clierr.ExpandRemedies(n.Error, ""), "rancher config update-server", "the message holds the command")
					assert.Contains(t, clierr.ExpandRemedies(text, " --config '/d'"), "rancher --config '/d' config update-server", "the report text holds the command")
					assert.True(t, clierr.HasRemedy(n.Error))
					assert.True(t, clierr.HasRemedy(text))
				}
			})
		}
	}
}
