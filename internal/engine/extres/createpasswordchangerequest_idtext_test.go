package extres

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/remedytest"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// TestUnconfirmedPasswordChangeShowsAnIdWithHiddenRunesAsEscapes asserts that
// a typed user id holding a direction override, a zero-width space, a bell, a
// newline and the start of a remedy marker reaches the error message and the
// recovery of an unconfirmed password change with escapes, never with the
// runes removed; that no command names it; that the recovery says where the
// exact id is; and that details.resourceName keeps the exact id.
func TestUnconfirmedPasswordChangeShowsAnIdWithHiddenRunesAsEscapes(t *testing.T) {
	t.Parallel()
	const id = "u-\u202e\u200b\a\n\u2063rancher-x"
	client := &fakeClient{status: map[string]any{"conditions": []any{}}}
	cmd := NewCreatePasswordChangeRequest(stubFactory{c: client}, fakeFileSource{body: []byte("aVeryLongPassword123\n")})
	exec, err := planPCR(t, cmd, pcrInput{args: []string{id}, bools: map[string]bool{"password-stdin": true}})
	require.NoError(t, err)
	_, err = exec(t.Context())
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)

	var buf strings.Builder
	require.NoError(t, output.Render(&buf, "json", output.ErrorEnvelope(ce)))
	var got struct {
		Data struct {
			Error struct {
				Message string `json:"message"`
				Details struct {
					ResourceName string `json:"resourceName"`
					Recovery     string `json:"recovery"`
				} `json:"details"`
			} `json:"error"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(buf.String()), &got))
	e := got.Data.Error
	assert.Equal(t, id, e.Details.ResourceName, "the data field keeps the exact id")
	assert.Equal(t, "the server did not confirm the password change for user "+strconv.Quote(id), e.Message)
	recovery := strings.ReplaceAll(e.Details.Recovery, clierr.Rancher, "rancher")
	remedytest.AssertIDShownWithEscapes(t, recovery, id)
	remedytest.AssertIDShownWithEscapes(t, e.Message, id)
	assert.Contains(t, recovery, clierr.ShellQuote(strconv.Quote(id)))
	assert.Contains(t, recovery, "the CLI prints no command for it")
	assert.Contains(t, recovery, "details.resourceName")
	assert.NotContains(t, recovery, "rancher get user ", "no command names the id")
	assert.Equal(t, 1, strings.Count(recovery, "rancher "), "only the command that lists users")
}

// TestPasswordChangeShowsAnIdWithAnInvalidByteAsAnEscape asserts that a typed
// user id whose only problem is an invalid UTF-8 byte reaches the error
// message of an unconfirmed password change and the preview name as a
// double-quoted string with the byte as an escape, and that the details keep
// the exact id.
func TestPasswordChangeShowsAnIdWithAnInvalidByteAsAnEscape(t *testing.T) {
	t.Parallel()
	const id = "u-a\xffb"
	client := &fakeClient{status: map[string]any{"conditions": []any{}}}
	cmd := NewCreatePasswordChangeRequest(stubFactory{c: client}, fakeFileSource{body: []byte("aVeryLongPassword123\n")})
	eff, exec, err := cmd.(runtime.MutatingCommand).Plan(t.Context(), pcrInput{args: []string{id}, bools: map[string]bool{"password-stdin": true}})
	require.NoError(t, err)
	assert.Equal(t, strconv.Quote(id), eff.DisplayName)
	remedytest.AssertIDShownWithEscapes(t, eff.DisplayName, id)

	_, err = exec(t.Context())
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	details, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok)
	assert.Equal(t, id, details.ResourceName, "the data field keeps the exact id")
	assert.Equal(t, "the server did not confirm the password change for user "+strconv.Quote(id), ce.Message)
	remedytest.AssertIDShownWithEscapes(t, ce.Message, id)
}

// TestUnconfirmedPasswordChangeRecoveryNamesAUserIDAsBefore asserts a typed
// Rancher user id gets the command that shows the user, as before.
func TestUnconfirmedPasswordChangeRecoveryNamesAUserIDAsBefore(t *testing.T) {
	t.Parallel()
	assert.Equal(t, clierr.Rancher+" get user 'u-abc123' -o json", userLookupRecovery("u-abc123"))
	assert.Contains(t, userLookupRecovery("alice@example.com"), "'alice@example.com' is not a Rancher user id")
	assert.NotContains(t, userLookupRecovery("alice@example.com"), "get user ")
}

// TestPasswordChangePreviewAndResultShowAnIdWithHiddenRunesAsEscapes asserts
// the name in the dry-run preview and in the completed line shows a typed id
// holding hidden runes with escapes, never with the runes removed, and that
// the target and the data keep the exact id.
func TestPasswordChangePreviewAndResultShowAnIdWithHiddenRunesAsEscapes(t *testing.T) {
	t.Parallel()
	const id = "u-\u202e\u200b\a\n\u2063rancher-x"
	client := &fakeClient{status: passwordUpdatedStatus()}
	cmd := NewCreatePasswordChangeRequest(stubFactory{c: client}, fakeFileSource{body: []byte("aVeryLongPassword123\n")})
	eff, exec, err := cmd.(runtime.MutatingCommand).Plan(t.Context(), pcrInput{args: []string{id}, bools: map[string]bool{"password-stdin": true}})
	require.NoError(t, err)
	remedytest.AssertIDShownWithEscapes(t, eff.DisplayName, id)
	assert.Equal(t, "passwordchangerequests/"+id, eff.Target, "the target keeps the exact id")

	res, err := exec(t.Context())
	require.NoError(t, err)
	remedytest.AssertIDShownWithEscapes(t, res.Text, id)
	assert.Equal(t, id, res.Data.(map[string]any)["userID"], "the data keeps the exact id")
}
