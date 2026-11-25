package auth

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/ui"
)

// TestTrustCardNoticeIsUnchanged asserts the trust card, whose subject and
// issuer have hidden characters, prints through Prompter.Notice as the card
// itself and nothing more: the card writes certificate text with those
// characters removed, and no line about removed characters follows it.
func TestTrustCardNoticeIsUnchanged(t *testing.T) {
	t.Parallel()
	d := tlstrust.Details{
		Kind: tlstrust.KindUnknownAuthority, Subject: "CN=rancher\u200b.example\x1b[2J", Issuer: "CN=c\u202ea", Fingerprint: "AA:BB",
		NotBefore: "2026-01-01T00:00:00Z", NotAfter: "2027-01-01T00:00:00Z",
	}
	var errBuf bytes.Buffer
	ui.New(&errBuf, strings.NewReader(""), true, false).Notice(trustCard(d, ""))
	assert.Equal(t, trustCard(d, "")+"\n", errBuf.String())
}

// TestLogoutPrintsNoFilterNote asserts auth logout, whose text has the
// lines of a certificate failure with their hidden characters removed,
// prints no line about removed characters.
func TestLogoutPrintsNoFilterNote(t *testing.T) {
	t.Parallel()
	assert.True(t, logoutCmd{}.Describe().NoFilterNote)
}
