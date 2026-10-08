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

// countingLocal is a Local command that counts its executions.
type countingLocal struct {
	d     Descriptor
	calls *int
}

func (c countingLocal) Describe() Descriptor { return c.d }
func (c countingLocal) Execute(context.Context, Input, Prompter) (Result, error) {
	*c.calls++
	return Result{Kind: "LocalResult"}, nil
}

func localDesc() Descriptor { return Descriptor{Name: "config path", CommandType: Local} }

// TestRunRefusesTypedReadOnlyOnLocalCommand asserts that a typed
// --read-only on a Local command is refused with VALIDATION and no hint
// before the command runs, whether the flag reads true or false, and that
// the message names the command and the session-wide forms but never
// blames the environment or config as the cause.
func TestRunRefusesTypedReadOnlyOnLocalCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		in      mapInput
		streams IO
	}{
		{"typed true", mapInput{b: map[string]bool{FlagReadOnly: true}, is: map[string]bool{FlagReadOnly: true}},
			IO{ReadOnly: true, ReadOnlySources: []string{"--read-only"}}},
		{"typed false", mapInput{b: map[string]bool{FlagReadOnly: false}, is: map[string]bool{FlagReadOnly: true}}, IO{}},
		{"typed with the environment source", mapInput{b: map[string]bool{FlagReadOnly: true}, is: map[string]bool{FlagReadOnly: true}},
			IO{ReadOnly: true, ReadOnlySources: []string{"RANCHER_CLI_READONLY", "--read-only"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			var out, errBuf bytes.Buffer
			streams := tc.streams
			streams.Out, streams.Err, streams.Format = &out, &errBuf, "json"
			_, ce := Run(t.Context(), countingLocal{d: localDesc(), calls: &calls}, tc.in, streams, safety.Binding{})
			require.NotNil(t, ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Empty(t, ce.Hint)
			require.ErrorContains(t, ce, "config path is a local command")
			require.ErrorContains(t, ce, "--read-only does not govern it")
			require.ErrorContains(t, ce, "without --read-only")
			require.ErrorContains(t, ce, "and any value typed with it")
			require.ErrorContains(t, ce, "config readOnly")
			assert.NotContains(t, ce.Message, "enabled by")
			assert.NotContains(t, ce.Message, "read-only mode:")
			assert.Zero(t, calls, "the command must not run")
		})
	}
}

// TestRunLocalCommandUnderEnvOrConfigReadOnlyRuns asserts that the
// environment and config read-only sources, with no typed --read-only,
// leave a Local command usable.
func TestRunLocalCommandUnderEnvOrConfigReadOnlyRuns(t *testing.T) {
	t.Parallel()
	for _, sources := range [][]string{{"RANCHER_CLI_READONLY"}, {"config readOnly"}, {"config readOnly", "RANCHER_CLI_READONLY"}} {
		calls := 0
		var out, errBuf bytes.Buffer
		_, ce := Run(t.Context(), countingLocal{d: localDesc(), calls: &calls}, mapInput{},
			IO{Out: &out, Err: &errBuf, Format: "json", ReadOnly: true, ReadOnlySources: sources}, safety.Binding{})
		require.Nil(t, ce, sources)
		assert.Equal(t, 1, calls, sources)
	}
}

// TestRunReadOnlyCommandAcceptsTypedReadOnly asserts that a ReadOnly
// command runs with --read-only typed.
func TestRunReadOnlyCommandAcceptsTypedReadOnly(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{d: Descriptor{Name: "get thing", CommandType: ReadOnly}, res: Result{Kind: "Thing"}}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd,
		mapInput{b: map[string]bool{FlagReadOnly: true}, is: map[string]bool{FlagReadOnly: true}},
		IO{Out: &out, Err: &errBuf, Format: "json", ReadOnly: true, ReadOnlySources: []string{"--read-only"}},
		safety.Binding{})
	require.Nil(t, ce)
}

// TestRunTypedReadOnlyRefusalPrecedesDryRunRefusal asserts that with
// --read-only and --dry-run both typed on a Local command the --read-only
// refusal comes first, and that --dry-run alone keeps its own refusal.
func TestRunTypedReadOnlyRefusalPrecedesDryRunRefusal(t *testing.T) {
	t.Parallel()
	calls := 0
	cmd := countingLocal{d: localDesc(), calls: &calls}
	var out, errBuf bytes.Buffer

	_, ce := Run(t.Context(), cmd,
		mapInput{b: map[string]bool{FlagReadOnly: true}, is: map[string]bool{FlagReadOnly: true}},
		IO{Out: &out, Err: &errBuf, Format: "json", DryRun: true, ReadOnly: true, ReadOnlySources: []string{"--read-only"}},
		safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)

	_, ce = Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json", DryRun: true}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeDryRunNotApplicable, ce.Code)
	assert.Zero(t, calls)
}

// TestRunMutatingReadOnlyRefusalUnchangedByTypedFlag asserts that a
// Mutating command with --read-only typed keeps the read-only mode refusal
// that names its sources.
func TestRunMutatingReadOnlyRefusalUnchangedByTypedFlag(t *testing.T) {
	t.Parallel()
	cmd := refusingPlanMutating{d: mutatingDesc(), t: t}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd,
		mapInput{b: map[string]bool{FlagReadOnly: true}, is: map[string]bool{FlagReadOnly: true}},
		IO{Out: &out, Err: &errBuf, Format: "json", ReadOnly: true, ReadOnlySources: []string{"--read-only"}},
		safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, ce, "read-only mode: clusters scale is a mutating command; enabled by --read-only")
}
