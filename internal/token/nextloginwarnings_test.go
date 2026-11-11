package token

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestTheWarningWritesTheOwnerOfTheNextLoginAsEscapes
// asserts the warning for the tokens the next login of an entry revokes names
// the owner as clierr.IDText writes it: an id with no hidden rune and valid
// UTF-8 as it is, an invalid UTF-8 byte as an escape, and a value with a
// single quote and a hidden rune quoted only in the piece that holds the rune.
func TestTheWarningWritesTheOwnerOfTheNextLoginAsEscapes(t *testing.T) {
	for _, tc := range []struct {
		name, id, want string
	}{
		{"plain", "u-abc12", "u-abc12"},
		{"hidden rune", "u-a\u200bb", `"u-a\u200bb"`},
		{"invalid byte", "u-a\xffb", `"u-a\xffb"`},
		{"single quote and hidden rune", "o'b\u200b", `o'"b\u200b"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lines []string
			for _, w := range ownerDeletion(tc.id).Warnings() {
				lines = append(lines, strings.ReplaceAll(w, clierr.Rancher, "rancher"))
			}
			assert.Equal(t,
				"1 token(s) recorded on prod are revoked by the next login of prod as "+tc.want+"; run it with: rancher auth login 'prod'",
				lineWith(t, lines, "recorded on prod"))
		})
	}
}
