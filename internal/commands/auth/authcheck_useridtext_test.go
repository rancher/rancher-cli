package auth

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/remedytest"
)

// TestAuthStatusShowsAnOwnerWithHiddenRunesAsEscapes asserts auth status text
// shows the identity the server reports and the owner a stored login records
// with escapes when they hold hidden runes, never with the runes removed, and
// a normal id as it is, while the data keeps the owner exactly.
func TestAuthStatusShowsAnOwnerWithHiddenRunesAsEscapes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, id, shown string }{
		{"a normal id", "u-abc12", "u-abc12"},
		{"hidden runes", "u-abc" + string(rune(0x200b)) + "12" + string(rune(0x202e)) + "\a\n", strconv.Quote("u-abc" + string(rune(0x200b)) + "12" + string(rune(0x202e)) + "\a\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := config.Server{Provenance: clierr.ProvenanceDerived, TokenUserID: tc.id}
			data, line := credentialReport(s)
			assert.Equal(t, tc.id, data["userId"], "the data keeps the owner as it is")
			assert.Equal(t, "credential: derived (user "+tc.shown+")", line)

			text := renderAuthCheckText(true, "default", "v1", nil, nil, map[string]any{"userID": tc.id}, nil, storeReport{policy: "auto"}, line)
			assert.Contains(t, text, "  userID: "+tc.shown+"\n")
			if tc.id != tc.shown {
				remedytest.AssertIDShownWithEscapes(t, text, tc.id)
				remedytest.AssertIDShownWithEscapes(t, line, tc.id)
			}
		})
	}
}
