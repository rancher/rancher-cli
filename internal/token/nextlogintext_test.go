package token

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// ownerDeletion is textSafetyDeletion with a plain value at every path but the
// owner, which is id.
func ownerDeletion(id string) *Deletion {
	return textSafetyDeletion(func(path string) string {
		if path == "TokenUserID" {
			return id
		}
		return "t-" + path
	})
}

// lineWith is the one line of lines that holds text.
func lineWith(t *testing.T, lines []string, text string) string {
	t.Helper()
	var found []string
	for _, l := range lines {
		if strings.Contains(l, text) {
			found = append(found, l)
		}
	}
	require.Len(t, found, 1, "the lines holding %q in %q", text, lines)
	return found[0]
}

// TestTheTokenLineWritesTheOwnerOfTheNextLoginAsItAlwaysHas asserts the line of
// a token that the next login of its entry revokes names the owner as the
// success text always has: an id with no hidden rune and valid UTF-8 as it is,
// an id with a hidden rune as a double-quoted string, an invalid UTF-8 byte
// left for output.Printable to write as U+FFFD, and an id with a single quote
// and a hidden rune quoted whole.
func TestTheTokenLineWritesTheOwnerOfTheNextLoginAsItAlwaysHas(t *testing.T) {
	for _, tc := range []struct {
		name, id, want string
	}{
		{"plain", "u-abc12", "u-abc12"},
		{"hidden rune", "u-a\u200bb", `"u-a\u200bb"`},
		{"invalid byte", "u-a\xffb", "u-a\ufffdb"},
		{"single quote and hidden rune", "o'b\u200b", `"o'b\u200b"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := ownerDeletion(tc.id).TextLines()
			assert.Equal(t,
				"  not revoked ext/t-nextLoginRef (failed: the server refused; recorded on prod: the next login of prod as "+tc.want+" revokes it)",
				lineWith(t, lines, "ext/t-nextLoginRef"))
		})
	}
}

// TestTheCertificateLinesOfARevokeHoldNoControlCharacters asserts the lines
// of a certificate whose subject holds an escape and a newline, as they print:
// the escape byte and the newline are removed, and each line sits two spaces
// deeper than the token line.
func TestTheCertificateLinesOfARevokeHoldNoControlCharacters(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	cert := tlsCert(t, "evil\x1b[31m\nfake line: forged")
	d, _ := x.runSeeded(x.req(DeleteForLogout, "prod"), "prod", false, tlsFailure(unknownAuthorityFailure(cert)), tlsFailure(unknownAuthorityFailure(cert)), nil)
	lines := d.TextLines()
	var at int
	for i, l := range lines {
		if strings.HasPrefix(l, "  not revoked") {
			at = i
		}
	}
	require.NotZero(t, at, "%q", lines)
	require.GreaterOrEqual(t, len(lines), at+5, "%q", lines)
	assert.Equal(t, "    serving certificate: CN=evil[31mfake line: forged", lines[at+2])
	assert.Equal(t, "    issuer: CN=evil[31mfake line: forged", lines[at+3])
	assert.Equal(t, "    fingerprint: "+tlstrust.Fingerprint(cert), lines[at+4])
}
