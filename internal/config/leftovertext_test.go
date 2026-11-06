package config

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestRecordTextsNameTheTokenTheCommandAndWhoRunsIt asserts the two texts for
// a token record whose server no command can name - no server is recorded, or
// the one recorded cannot be used - say which token, the command, and who
// runs it: the token's owner when the record names it, its owner or an
// administrator when it does not. The key of a row is the function that
// builds the text, as the guard in package token names it; each person is
// written here as the words of the text.
func TestRecordTextsNameTheTokenTheCommandAndWhoRunsIt(t *testing.T) {
	t.Parallel()
	ref := clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-abc12"}
	bad := errors.New("it is not a valid URL")
	for _, tc := range []struct {
		key    string
		name   string
		text   string
		person string
	}{
		{"config.noServerText", "a known owner", noServerText(ref, "u-abc12"), "user 'u-abc12'"},
		{"config.noServerText", "no known owner", noServerText(ref, ""), "its owner or an administrator"},
		{"config.recordServerText", "a known owner", recordServerText(ref, bad, "u-abc12"), "user 'u-abc12'"},
		{"config.recordServerText", "no known owner", recordServerText(ref, bad, ""), "its owner or an administrator"},
	} {
		t.Run(tc.key+", "+tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Contains(t, tc.text, "token 'token-abc12'")
			assert.Contains(t, tc.text, clierr.Rancher+" delete token 'token-abc12' --apply --yes")
			assert.Contains(t, tc.text, tc.person)
			assert.NotContains(t, tc.text, "only an administrator")
		})
	}
}
