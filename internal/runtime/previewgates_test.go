package runtime

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gateRows are every combination of a destructive change and a stability, with
// the flags a real run of it needs, written out. The three flags always come in
// the order --apply, --yes, --allow-deprecated.
var gateRows = []struct {
	row         string
	destructive bool
	stability   safety.Stability
	want        []string
}{
	{"nonDestructiveStable", false, safety.Stable, []string{"--apply"}},
	{"nonDestructiveBeta", false, safety.Beta, []string{"--apply"}},
	{"nonDestructiveDeprecated", false, safety.Deprecated, []string{"--apply", "--allow-deprecated"}},
	{"destructiveStable", true, safety.Stable, []string{"--apply", "--yes"}},
	{"destructiveBeta", true, safety.Beta, []string{"--apply", "--yes"}},
	{"destructiveDeprecated", true, safety.Deprecated, []string{"--apply", "--yes", "--allow-deprecated"}},
}

// gateEffect is the Effect a Plan returns for a gate row: a delete when the
// change is destructive, an update otherwise.
func gateEffect(destructive bool) safety.Effect {
	eff := safety.Effect{PreviewSource: safety.PreviewServerDryRun, Action: safety.ActionUpdate, Target: "x", DisplayKind: "widget", DisplayName: "x"}
	if destructive {
		eff.Action = safety.ActionDelete
		eff.Destructive = true
	}
	return eff
}

// TestPreviewNamesEveryGateTheApplyNeeds asserts, for every combination of a
// destructive change and a stability, that the text preview, the Effect of the
// JSON preview and the refusal of a run with no flag name the same flags, that
// they are the ones the row lists, and that a run given all three flags
// applies. The preview names every flag, whichever flags the dry run was given.
func TestPreviewNamesEveryGateTheApplyNeeds(t *testing.T) {
	t.Parallel()
	for _, tc := range gateRows {
		t.Run(tc.row, func(t *testing.T) {
			t.Parallel()
			binding := safety.Binding{Contract: safety.ContractPublic, Stability: tc.stability}
			applied := 0
			cmd := fakeMutating{d: mutatingDesc(), eff: gateEffect(tc.destructive), exec: func(context.Context) (Result, error) {
				applied++
				return Result{Kind: "ScaleResult"}, nil
			}}
			flags := strings.Join(tc.want, " ")

			for _, given := range []IO{{}, {Apply: true, Yes: true, AllowDeprecated: true}} {
				var out, errBuf bytes.Buffer
				streams := given
				streams.Out, streams.Err, streams.DryRun = &out, &errBuf, true
				_, ce := Run(t.Context(), cmd, mapInput{}, streams, binding)
				require.Nil(t, ce)
				lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
				assert.Equal(t, "  dry-run preview - re-run with "+flags+" to apply the change", lines[len(lines)-1])

				out.Reset()
				streams.Format = "json"
				_, ce = Run(t.Context(), cmd, mapInput{}, streams, binding)
				require.Nil(t, ce)
				data, ok := decodeEnvelope(t, out.Bytes())["data"].(map[string]any)
				require.True(t, ok, "the envelope has no data object")
				assert.Equal(t, anyStrings(tc.want), data["gates"])
			}
			assert.Zero(t, applied, "a dry run applies nothing")

			var out, errBuf bytes.Buffer
			_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, binding)
			require.NotNil(t, ce)
			assert.Equal(t, clierr.CodeConfirmationRequired, ce.Code)
			assert.True(t, strings.HasSuffix(ce.Message, "re-run with "+flags+" after reading the preview"), ce.Message)
			assert.Zero(t, applied, "a refused run applies nothing")

			out.Reset()
			_, ce = Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json", Apply: true, Yes: true, AllowDeprecated: true}, binding)
			require.Nil(t, ce)
			assert.Equal(t, 1, applied, "a run given every flag applies")
		})
	}
}

// TestSchemaGatesMatchTheRun asserts the gates the schema manifest
// lists for a mutating command are the flags of the same row: a destructive
// value of "true" adds --yes, "dependsOnInput" adds nothing beyond what the
// stability needs, and a command that does not mutate has none.
func TestSchemaGatesMatchTheRun(t *testing.T) {
	t.Parallel()
	for _, tc := range gateRows {
		t.Run(tc.row, func(t *testing.T) {
			t.Parallel()
			destructive := "false"
			if tc.destructive {
				destructive = "true"
			}
			assert.Equal(t, tc.want, Gates(Mutating, destructive, string(tc.stability)))
			assert.Equal(t, safety.RequiredGates(tc.destructive, tc.stability), Gates(Mutating, destructive, string(tc.stability)))
		})
	}
	assert.Equal(t, []string{"--apply"}, Gates(Mutating, "dependsOnInput", "stable"))
	assert.Equal(t, []string{"--apply", "--allow-deprecated"}, Gates(Mutating, "dependsOnInput", "deprecated"))
	assert.Nil(t, Gates(ReadOnly, "true", "deprecated"))
	assert.Nil(t, Gates(Local, "true", "deprecated"))
}

// TestMissingFlagsNamesTheGatesTheRunLacks asserts missingFlags returns, in
// order, the flags the change needs that the run was not given, whatever the
// Effect has in Gates.
func TestMissingFlagsNamesTheGatesTheRunLacks(t *testing.T) {
	t.Parallel()
	eff := safety.Effect{Destructive: true}
	deprecated := safety.Binding{Contract: safety.ContractPublic, Stability: safety.Deprecated}
	assert.Equal(t, []string{"--apply", "--yes", "--allow-deprecated"}, missingFlags(IO{}, eff, deprecated))
	assert.Equal(t, []string{"--yes", "--allow-deprecated"}, missingFlags(IO{Apply: true}, eff, deprecated))
	assert.Equal(t, []string{"--apply", "--allow-deprecated"}, missingFlags(IO{Yes: true}, eff, deprecated))
	assert.Equal(t, []string{"--apply", "--yes"}, missingFlags(IO{AllowDeprecated: true}, eff, deprecated))
	assert.Nil(t, missingFlags(IO{Apply: true, Yes: true, AllowDeprecated: true}, eff, deprecated))
	eff.Gates = []string{"--apply"}
	assert.Equal(t, []string{"--yes", "--allow-deprecated"}, missingFlags(IO{Apply: true}, eff, deprecated),
		"a list in Gates does not change what the change needs")
	assert.False(t, IO{Apply: true, Yes: true, AllowDeprecated: true}.givenGate("--unknown"),
		"a gate this package does not know always counts as missing")
}

// TestAPlanCannotWeakenTheGates asserts a Plan that returns its own Gates (no
// list, an empty list, only --apply, or a flag no run is given) changes neither
// the preview nor the refusal. For a destructive change under a deprecated
// binding the text and the JSON preview name all three flags, a run with no
// flag is refused for all three, a run given only --apply is refused for --yes
// and --allow-deprecated, and neither applies.
func TestAPlanCannotWeakenTheGates(t *testing.T) {
	t.Parallel()
	binding := safety.Binding{Contract: safety.ContractPublic, Stability: safety.Deprecated}
	all := []string{"--apply", "--yes", "--allow-deprecated"}
	for _, tc := range []struct {
		row     string
		planned []string
	}{
		{"noList", nil},
		{"emptyList", []string{}},
		{"onlyApply", []string{"--apply"}},
		{"unknownFlag", []string{"--unknown"}},
	} {
		t.Run(tc.row, func(t *testing.T) {
			t.Parallel()
			applied := 0
			eff := gateEffect(true)
			eff.Gates = tc.planned
			cmd := fakeMutating{d: mutatingDesc(), eff: eff, exec: func(context.Context) (Result, error) {
				applied++
				return Result{Kind: "ScaleResult"}, nil
			}}

			var out, errBuf bytes.Buffer
			_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, DryRun: true}, binding)
			require.Nil(t, ce)
			lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
			assert.Equal(t, "  dry-run preview - re-run with --apply --yes --allow-deprecated to apply the change", lines[len(lines)-1])

			out.Reset()
			_, ce = Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json", DryRun: true}, binding)
			require.Nil(t, ce)
			data, ok := decodeEnvelope(t, out.Bytes())["data"].(map[string]any)
			require.True(t, ok, "the envelope has no data object")
			assert.Equal(t, anyStrings(all), data["gates"])

			_, ce = Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, binding)
			require.NotNil(t, ce)
			assert.Equal(t, clierr.CodeConfirmationRequired, ce.Code)
			assert.True(t, strings.HasSuffix(ce.Message, "re-run with --apply --yes --allow-deprecated after reading the preview"), ce.Message)

			_, ce = Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json", Apply: true}, binding)
			require.NotNil(t, ce)
			assert.Equal(t, clierr.CodeConfirmationRequired, ce.Code)
			assert.Equal(t, "confirmation required for this mutation; re-run with --yes --allow-deprecated", ce.Message)
			assert.Zero(t, applied, "a refused run applies nothing")
		})
	}
}

// anyStrings converts a string list to the []any a JSON decode produces.
func anyStrings(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}
