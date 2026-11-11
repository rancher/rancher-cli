package runtime

import (
	"bytes"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfirmationRefusalText asserts the code and the exact message of the
// refusal for each set of missing confirmation flags: a refusal that lacks
// --apply names --dry-run as the preview, and a refusal with --apply given
// only names the flags still missing.
func TestConfirmationRefusalText(t *testing.T) {
	t.Parallel()
	update := safety.Effect{PreviewSource: safety.PreviewServerDryRun, Action: safety.ActionUpdate, Target: "x", DisplayKind: "widget", DisplayName: "x"}
	destructive := safety.Effect{PreviewSource: safety.PreviewServerDryRun, Action: safety.ActionDelete, Target: "x", DisplayKind: "widget", DisplayName: "x", Destructive: true}
	deprecated := safety.Binding{Contract: safety.ContractPublic, Stability: safety.Deprecated}
	for _, tc := range []struct {
		row     string
		eff     safety.Effect
		binding safety.Binding
		streams IO
		want    string
	}{
		{"applyMissingOnANonDestructiveEffect", update, safety.Binding{}, IO{},
			"confirmation required for this mutation; preview it with --dry-run, then re-run with --apply after reading the preview"},
		{"applyAndYesMissingOnADestructiveEffect", destructive, safety.Binding{}, IO{},
			"confirmation required for this mutation; preview it with --dry-run, then re-run with --apply --yes after reading the preview"},
		{"applyAndAllowDeprecatedMissing", update, deprecated, IO{},
			"confirmation required for this mutation; preview it with --dry-run, then re-run with --apply --allow-deprecated after reading the preview"},
		{"applyYesAndAllowDeprecatedMissing", destructive, deprecated, IO{},
			"confirmation required for this mutation; preview it with --dry-run, then re-run with --apply --yes --allow-deprecated after reading the preview"},
		{"onlyYesMissing", destructive, safety.Binding{}, IO{Apply: true},
			"confirmation required for this mutation; re-run with --yes"},
		{"onlyAllowDeprecatedMissing", update, deprecated, IO{Apply: true},
			"confirmation required for this mutation; re-run with --allow-deprecated"},
		{"yesAndAllowDeprecatedMissing", destructive, deprecated, IO{Apply: true},
			"confirmation required for this mutation; re-run with --yes --allow-deprecated"},
	} {
		t.Run(tc.row, func(t *testing.T) {
			t.Parallel()
			cmd := fakeMutating{d: mutatingDesc(), eff: tc.eff, exec: unusedExec(t)}
			var out, errBuf bytes.Buffer
			streams := tc.streams
			streams.Out, streams.Err, streams.Format = &out, &errBuf, "json"
			_, ce := Run(t.Context(), cmd, mapInput{}, streams, tc.binding)
			require.NotNil(t, ce)
			assert.Equal(t, clierr.CodeConfirmationRequired, ce.Code)
			assert.Equal(t, tc.want, ce.Message)

			env := decodeEnvelope(t, errBuf.Bytes())
			data, ok := env["data"].(map[string]any)
			require.True(t, ok, "the envelope has no data object")
			detail, ok := data["error"].(map[string]any)
			require.True(t, ok, "the envelope has no error object")
			assert.Equal(t, string(clierr.CodeConfirmationRequired), detail["code"])
			assert.Equal(t, tc.want, detail["message"])
		})
	}
}
