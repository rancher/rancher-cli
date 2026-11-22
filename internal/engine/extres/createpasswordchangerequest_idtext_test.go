package extres

import (
	"context"
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
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"sigs.k8s.io/yaml"
)

// allowCapability is a capability checker that lets every command proceed.
type allowCapability struct{}

func (allowCapability) Check(context.Context, runtime.CapabilityRequirement) runtime.CapabilityDecision {
	return runtime.CapabilityDecision{Outcome: runtime.CapabilityProceed}
}

// passwordChangePreview runs a dry run of a password change for id through the
// pipeline in format and returns what it wrote to standard output.
func passwordChangePreview(t *testing.T, id, format string) string {
	t.Helper()
	client := &fakeClient{status: passwordUpdatedStatus()}
	cmd := NewCreatePasswordChangeRequest(stubFactory{c: client}, fakeFileSource{body: []byte("aVeryLongPassword123\n")})
	var out, errBuf strings.Builder
	_, ce := runtime.Run(t.Context(), cmd, pcrInput{args: []string{id}, bools: map[string]bool{"password-stdin": true}},
		runtime.IO{Out: &out, Err: &errBuf, Format: format, DryRun: true, Capability: allowCapability{}}, safety.Binding{})
	require.Nil(t, ce)
	return out.String()
}

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
// message of an unconfirmed password change and the text preview as a
// double-quoted string with the byte as an escape, and that the preview's
// name and the details keep the exact id.
func TestPasswordChangeShowsAnIdWithAnInvalidByteAsAnEscape(t *testing.T) {
	t.Parallel()
	const id = "u-a\xffb"
	client := &fakeClient{status: map[string]any{"conditions": []any{}}}
	cmd := NewCreatePasswordChangeRequest(stubFactory{c: client}, fakeFileSource{body: []byte("aVeryLongPassword123\n")})
	eff, exec, err := cmd.(runtime.MutatingCommand).Plan(t.Context(), pcrInput{args: []string{id}, bools: map[string]bool{"password-stdin": true}})
	require.NoError(t, err)
	assert.Equal(t, id, eff.DisplayName, "the preview's name keeps the exact id")
	preview := passwordChangePreview(t, id, "")
	assert.Contains(t, preview, "create password-change-request/"+strconv.Quote(id)+"\n")
	remedytest.AssertIDShownWithEscapes(t, preview, id)

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
// the name in the text preview and in the completed line shows a typed id
// holding hidden runes with escapes, never with the runes removed, and that
// the preview's name and target and the data keep the exact id.
func TestPasswordChangePreviewAndResultShowAnIdWithHiddenRunesAsEscapes(t *testing.T) {
	t.Parallel()
	const id = "u-\u202e\u200b\a\n\u2063rancher-x"
	client := &fakeClient{status: passwordUpdatedStatus()}
	cmd := NewCreatePasswordChangeRequest(stubFactory{c: client}, fakeFileSource{body: []byte("aVeryLongPassword123\n")})
	eff, exec, err := cmd.(runtime.MutatingCommand).Plan(t.Context(), pcrInput{args: []string{id}, bools: map[string]bool{"password-stdin": true}})
	require.NoError(t, err)
	assert.Equal(t, id, eff.DisplayName, "the preview's name keeps the exact id")
	assert.Equal(t, "passwordchangerequests/"+id, eff.Target, "the target keeps the exact id")
	remedytest.AssertIDShownWithEscapes(t, passwordChangePreview(t, id, ""), id)

	res, err := exec(t.Context())
	require.NoError(t, err)
	remedytest.AssertIDShownWithEscapes(t, res.Text, id)
	assert.Equal(t, id, res.Data.(map[string]any)["userID"], "the data keeps the exact id")
}

// TestPasswordChangePreviewInJSONAndYAMLCarriesTheIdAsItIs asserts the json
// and yaml preview of a password change carry a typed id that has hidden
// characters unchanged, in displayName and in target.
func TestPasswordChangePreviewInJSONAndYAMLCarriesTheIdAsItIs(t *testing.T) {
	t.Parallel()
	const id = "u-\u202e\u200b\ab"
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			raw := []byte(passwordChangePreview(t, id, format))
			if format == "yaml" {
				var err error
				raw, err = yaml.YAMLToJSON(raw)
				require.NoError(t, err)
			}
			var got struct {
				Data struct {
					DisplayName string `json:"displayName"`
					Target      string `json:"target"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(raw, &got))
			assert.Equal(t, id, got.Data.DisplayName)
			assert.Equal(t, "passwordchangerequests/"+id, got.Data.Target)
		})
	}
}
