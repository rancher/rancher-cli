package runtime

import (
	"bytes"
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEffectSchemaListsWriteVia asserts the manifest's effect schema has the
// writeVia field and a closed writeVia enum that equals the two path constants
// of the safety package, in the order they are declared.
func TestEffectSchemaListsWriteVia(t *testing.T) {
	t.Parallel()
	es := effectSchema()
	assert.Contains(t, es.Fields, EffectField{Name: "writeVia", Type: "string"})
	assert.Equal(t, []string{string(safety.WriteViaKubernetesAPI), string(safety.WriteViaRancherAction)}, es.WriteVia)
	assert.Equal(t, []string{"kubernetesAPI", "rancherAction"}, es.WriteVia)
}

// TestRunChecksTheWriteViaOfAPlan asserts the pipeline refuses a Plan whose
// Effect sets a write path that is not one of the two, as an internal error
// before any gate, and accepts an empty one and each valid one.
func TestRunChecksTheWriteViaOfAPlan(t *testing.T) {
	t.Parallel()
	effect := func(via safety.WriteVia) safety.Effect {
		return safety.Effect{PreviewSource: safety.PreviewServerDryRun, Action: safety.ActionUpdate, Target: "x", DisplayKind: "widget", DisplayName: "x", WriteVia: via}
	}
	for _, via := range []safety.WriteVia{"", safety.WriteViaKubernetesAPI, safety.WriteViaRancherAction} {
		t.Run("accepts "+string(via), func(t *testing.T) {
			t.Parallel()
			called := 0
			cmd := fakeMutating{d: mutatingDesc(), eff: effect(via), exec: func(context.Context) (Result, error) {
				called++
				return Result{Kind: "ScaleResult"}, nil
			}}
			var out, errBuf bytes.Buffer
			_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json", Apply: true}, safety.Binding{})
			require.Nil(t, ce)
			assert.Equal(t, 1, called)
		})
	}
	for _, via := range []safety.WriteVia{"kubernetesApi", "norman", " rancherAction"} {
		t.Run("refuses "+string(via), func(t *testing.T) {
			t.Parallel()
			cmd := fakeMutating{d: mutatingDesc(), eff: effect(via), exec: unusedExec(t)}
			var out, errBuf bytes.Buffer
			_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json", Apply: true}, safety.Binding{})
			require.NotNil(t, ce)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.Equal(t, `internal error: invalid write path "`+string(via)+`" in the dry-run preview`, ce.Message)
		})
	}
}
