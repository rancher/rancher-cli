package token

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// TestASignInRecoveryNamesTheUserIDEscapedOnce asserts a user id that holds
// hidden characters appears in the recover line of the default-format error
// text of a failed sign-in with its escapes, inside the quotes of the command
// line, in each place the text names the user, never escaped a second time by
// the renderer: the line is the recovery text as the sign-in builds it, and
// json carries the same text.
func TestASignInRecoveryNamesTheUserIDEscapedOnce(t *testing.T) {
	t.Parallel()
	login := &clierr.TokenFate{
		Outcome: clierr.CreateKnown, Ref: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-a"},
		Cleanup: &clierr.RevokeOutcome{Result: clierr.ResultFailed, Error: "denied"},
	}
	ce := failSignIn(clierr.New(clierr.CodeGeneric, "the sign-in failed"), clierr.StageClusterToken, "m1", login, nil, hostileUserID, "", "")

	var text bytes.Buffer
	require.NoError(t, runtime.RenderError(&text, "", ce))
	var line string
	for l := range strings.SplitSeq(text.String(), "\n") {
		if rest, ok := strings.CutPrefix(l, "  recover: "); ok {
			line = rest
		}
	}
	require.NotEmpty(t, line, "%q", text.String())
	recovery := clierr.ExpandRemedies(signInRecovery(login, nil, "m1", hostileUserID, "", ""), "")
	assert.Contains(t, line, strconv.Quote(hostileUserID))
	assert.Equal(t, recovery, line, "the renderer leaves the text of the recovery as the sign-in builds it")

	var out bytes.Buffer
	require.NoError(t, runtime.RenderError(&out, "json", ce))
	var env struct {
		Data struct {
			Error struct {
				Details struct {
					Recovery string `json:"recovery"`
				} `json:"details"`
			} `json:"error"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	assert.Equal(t, recovery, env.Data.Error.Details.Recovery)
}
