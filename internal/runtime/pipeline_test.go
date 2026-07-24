package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

type mapInput struct {
	s map[string]string
	b map[string]bool
	a []string
}

func (m mapInput) String(k string) string { return m.s[k] }
func (m mapInput) Bool(k string) bool     { return m.b[k] }
func (m mapInput) Args() []string         { return m.a }

type fakeRead struct {
	d   Descriptor
	res Result
	err error
}

func (f fakeRead) Describe() Descriptor                                     { return f.d }
func (f fakeRead) Execute(context.Context, Input, Prompter) (Result, error) { return f.res, f.err }

type fakeMutating struct {
	d    Descriptor
	eff  safety.Effect
	err  error
	exec ExecuteFunc
}

func (f fakeMutating) Describe() Descriptor { return f.d }
func (f fakeMutating) Plan(context.Context, Input) (safety.Effect, ExecuteFunc, error) {
	return f.eff, f.exec, f.err
}

type describeOnly struct{ d Descriptor }

func (x describeOnly) Describe() Descriptor { return x.d }

func mutatingDesc() Descriptor { return Descriptor{Name: "clusters scale", CommandType: Mutating} }

// unusedExec is a closure that fails the test if invoked; use it where an
// invariant must reject before execution.
func unusedExec(t *testing.T) ExecuteFunc {
	return func(context.Context) (Result, error) {
		t.Helper()
		t.Fatal("exec must not be called")
		return Result{}, nil
	}
}

func decodeEnvelope(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var env map[string]any
	require.NoError(t, json.Unmarshal(b, &env))
	return env
}

// refusingPlanMutating fails the test if Plan is ever invoked — used to
// prove the read-only gate refuses a mutating command BEFORE dispatch, since
// Plan performs real network calls unconditionally (including for
// --dry-run).
type refusingPlanMutating struct {
	d Descriptor
	t *testing.T
}

func (r refusingPlanMutating) Describe() Descriptor { return r.d }
func (r refusingPlanMutating) Plan(context.Context, Input) (safety.Effect, ExecuteFunc, error) {
	r.t.Helper()
	r.t.Fatal("Plan must not be called when read-only mode refuses a mutating command")
	return safety.Effect{}, nil, nil
}

func TestRunRefusesMutatingUnderReadOnly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		sources []string
	}{
		{"config only", []string{"config readOnly"}},
		{"env only", []string{"RANCHER_CLI_READONLY"}},
		{"flag only", []string{"--read-only"}},
		{"config and env", []string{"config readOnly", "RANCHER_CLI_READONLY"}},
		{"all three", []string{"config readOnly", "RANCHER_CLI_READONLY", "--read-only"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := refusingPlanMutating{d: mutatingDesc(), t: t}
			var out, errBuf bytes.Buffer
			ce := Run(context.Background(), cmd, mapInput{},
				IO{Out: &out, Err: &errBuf, Format: "json", ReadOnly: true, ReadOnlySources: tc.sources},
				safety.Binding{})
			require.NotNil(t, ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Contains(t, ce.Message, "read-only mode:")
			assert.Contains(t, ce.Message, "clusters scale")
			for _, s := range tc.sources {
				assert.Contains(t, ce.Hint, s, "hint must name every active read-only source")
			}
		})
	}
}

func TestRunRefusesMutatingDryRunUnderReadOnly(t *testing.T) {
	t.Parallel()
	cmd := refusingPlanMutating{d: mutatingDesc(), t: t}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", ReadOnly: true, ReadOnlySources: []string{"--read-only"}, DryRun: true},
		safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code,
		"read-only refusal must win over dry-run-not-applicable; a mutating --dry-run still sends write-verb admission requests")
}

func TestRunReadOnlyCommandUnaffectedByReadOnlyMode(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{d: Descriptor{Name: "get thing", CommandType: ReadOnly}, res: Result{Kind: "Thing"}}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", ReadOnly: true, ReadOnlySources: []string{"--read-only"}},
		safety.Binding{})
	require.Nil(t, ce)
}

func TestRunLocalCommandUnaffectedByReadOnlyMode(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{d: Descriptor{Name: "auth login", CommandType: Local}, res: Result{Kind: "LoginResult"}}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", ReadOnly: true, ReadOnlySources: []string{"RANCHER_CLI_READONLY"}},
		safety.Binding{})
	require.Nil(t, ce)
}

func TestRunMutatingUnaffectedWhenReadOnlyFalse(t *testing.T) {
	t.Parallel()
	called := 0
	cmd := fakeMutating{
		d:   mutatingDesc(),
		eff: safety.Effect{Action: safety.ActionUpdate, Target: "x"},
		exec: func(context.Context) (Result, error) {
			called++
			return Result{Kind: "ScaleResult"}, nil
		},
	}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", Apply: true}, // ReadOnly defaults false
		safety.Binding{})
	require.Nil(t, ce)
	assert.Equal(t, 1, called)
}

func TestRunRejectsInvalidAction(t *testing.T) {
	t.Parallel()
	cmd := fakeMutating{
		d:    mutatingDesc(),
		eff:  safety.Effect{Action: safety.Action("destroy"), Target: "x"},
		exec: unusedExec(t),
	}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", Apply: true}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Contains(t, ce.Message, "invalid effect action")
}

func TestRunRejectsNonDestructiveDelete(t *testing.T) {
	t.Parallel()
	cmd := fakeMutating{
		d:    mutatingDesc(),
		eff:  safety.Effect{Action: safety.ActionDelete, Target: "x", Destructive: false},
		exec: unusedExec(t),
	}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", Apply: true, Yes: true}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Contains(t, ce.Message, "must be destructive")
}

func TestRunStampsTierStabilityFromBinding(t *testing.T) {
	t.Parallel()
	cmd := fakeMutating{
		d: mutatingDesc(),
		// Command spoofs a tier; the pipeline must overwrite it from the binding.
		eff:  safety.Effect{Action: safety.ActionUpdate, Target: "x", Tier: safety.Tier("spoofed"), Stability: safety.Stability("spoofed")},
		exec: unusedExec(t),
	}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", DryRun: true},
		safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta})
	require.Nil(t, ce)
	env := decodeEnvelope(t, out.Bytes())
	assert.Equal(t, "crd", env["tier"])
	assert.Equal(t, "beta", env["stability"])
	data := env["data"].(map[string]any)
	assert.Equal(t, "crd", data["tier"])
	assert.Equal(t, "beta", data["stability"])
}

func TestRunDeprecatedGateUsesBinding(t *testing.T) {
	t.Parallel()
	cmd := fakeMutating{
		d:    mutatingDesc(),
		eff:  safety.Effect{Action: safety.ActionUpdate, Target: "x"},
		exec: unusedExec(t),
	}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", Apply: true}, // no --allow-deprecated
		safety.Binding{Tier: safety.TierPublic, Stability: safety.Deprecated})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeConfirmationRequired, ce.Code)
	assert.Contains(t, ce.Hint, "--allow-deprecated")
}

func TestRunNormanWarningOnApply(t *testing.T) {
	t.Parallel()
	called := 0
	cmd := fakeMutating{
		d:   mutatingDesc(),
		eff: safety.Effect{Action: safety.ActionUpdate, Target: "x", ConflictDetection: false},
		exec: func(context.Context) (Result, error) {
			called++
			return Result{Kind: "ScaleResult", Data: map[string]string{"ok": "true"}}, nil
		},
	}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", Apply: true},
		safety.Binding{Tier: safety.TierNorman, Stability: safety.Stable})
	require.Nil(t, ce)
	assert.Equal(t, 1, called)
	env := decodeEnvelope(t, out.Bytes())
	warnings, ok := env["warnings"].([]any)
	require.True(t, ok, "envelope must carry warnings")
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0].(string), "no conflict detection")
}

// TestRunNormanWarningOnDryRun covers the dry-run half of the
// ARCHITECTURE.md promise that the norman-tier/no-conflict-detection
// warning rides BOTH the dry-run Effect and the applied Result — the
// applied half is TestRunNormanWarningOnApply above; this half had zero
// coverage (DR-014: deleting renderEffect's warning append kept the
// suite green).
func TestRunNormanWarningOnDryRun(t *testing.T) {
	t.Parallel()
	cmd := fakeMutating{
		d:    mutatingDesc(),
		eff:  safety.Effect{Action: safety.ActionUpdate, Target: "x", ConflictDetection: false},
		exec: unusedExec(t),
	}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", DryRun: true},
		safety.Binding{Tier: safety.TierNorman, Stability: safety.Stable})
	require.Nil(t, ce)
	env := decodeEnvelope(t, out.Bytes())
	warnings, ok := env["warnings"].([]any)
	require.True(t, ok, "effect envelope must carry warnings")
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0].(string), "no conflict detection")
}

// TestRenderEffectWarningsJSONOnce covers case (a) from the plan (D-10b,
// review R2-6): an Effect.Warnings note rides the -o json dry-run envelope's
// top-level "warnings" exactly once — Effect.Warnings' json:"-" tag must not
// let it leak into env.data too.
func TestRenderEffectWarningsJSONOnce(t *testing.T) {
	t.Parallel()
	cmd := fakeMutating{
		d:    mutatingDesc(),
		eff:  safety.Effect{Action: safety.ActionUpdate, Target: "x", Warnings: []string{"note-a"}},
		exec: unusedExec(t),
	}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", DryRun: true},
		safety.Binding{Tier: safety.TierPublic, Stability: safety.Stable})
	require.Nil(t, ce)
	env := decodeEnvelope(t, out.Bytes())
	warnings, ok := env["warnings"].([]any)
	require.True(t, ok, "effect envelope must carry warnings")
	require.Len(t, warnings, 1)
	assert.Equal(t, "note-a", warnings[0])
	assert.Equal(t, 1, strings.Count(out.String()+errBuf.String(), "note-a"),
		"note-a must appear exactly once across stdout+stderr")
}

// TestRenderEffectWarningsTableStderrOnly covers case (b): table output
// (render.go) encodes env.Data alone, so an Effect.Warnings note would
// otherwise vanish for a user looking at default/table dry-run output.
// renderEffect must write it to stderr as "# warning: <w>" — the same
// convention Result documents for table mode (command.go) — and it must
// NOT also appear on stdout.
func TestRenderEffectWarningsTableStderrOnly(t *testing.T) {
	t.Parallel()
	cmd := fakeMutating{
		d:    mutatingDesc(),
		eff:  safety.Effect{Action: safety.ActionUpdate, Target: "x", Warnings: []string{"note-a"}},
		exec: unusedExec(t),
	}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "", DryRun: true},
		safety.Binding{Tier: safety.TierPublic, Stability: safety.Stable})
	require.Nil(t, ce)
	assert.Contains(t, errBuf.String(), "# warning: note-a\n")
	assert.NotContains(t, out.String(), "note-a")
	assert.Equal(t, 1, strings.Count(out.String()+errBuf.String(), "note-a"),
		"note-a must appear exactly once total across both streams")
}

// TestRenderEffectWarningsCombineWithTierWarning covers case (c): an
// Effect.Warnings note and the existing TierNorman/no-conflict-detection
// warning ride together, each appearing exactly once, in both -o json and
// default/table dry-run output, with the Effect warning ordered BEFORE the
// tier warning.
func TestRenderEffectWarningsCombineWithTierWarning(t *testing.T) {
	newCmd := func() fakeMutating {
		return fakeMutating{
			d:    mutatingDesc(),
			eff:  safety.Effect{Action: safety.ActionUpdate, Target: "x", ConflictDetection: false, Warnings: []string{"note-a"}},
			exec: unusedExec(t),
		}
	}

	t.Run("json", func(t *testing.T) {
		t.Parallel()
		var out, errBuf bytes.Buffer
		ce := Run(context.Background(), newCmd(), mapInput{},
			IO{Out: &out, Err: &errBuf, Format: "json", DryRun: true},
			safety.Binding{Tier: safety.TierNorman, Stability: safety.Stable})
		require.Nil(t, ce)
		env := decodeEnvelope(t, out.Bytes())
		warnings, ok := env["warnings"].([]any)
		require.True(t, ok)
		require.Len(t, warnings, 2)
		assert.Equal(t, "note-a", warnings[0], "effect warnings must precede the tier warning")
		assert.Contains(t, warnings[1], "no conflict detection")
		assert.Equal(t, 1, strings.Count(out.String(), "note-a"))
		assert.Equal(t, 1, strings.Count(out.String(), "no conflict detection"))
	})

	t.Run("table", func(t *testing.T) {
		t.Parallel()
		var out, errBuf bytes.Buffer
		ce := Run(context.Background(), newCmd(), mapInput{},
			IO{Out: &out, Err: &errBuf, Format: "", DryRun: true},
			safety.Binding{Tier: safety.TierNorman, Stability: safety.Stable})
		require.Nil(t, ce)
		assert.Contains(t, errBuf.String(), "# warning: note-a\n")
		assert.Contains(t, errBuf.String(), "# warning: served via norman tier: no conflict detection\n")
		combined := out.String() + errBuf.String()
		assert.Equal(t, 1, strings.Count(combined, "note-a"))
		assert.Equal(t, 1, strings.Count(combined, "no conflict detection"))
	})
}

// TestRunApplyRendersEffectWarnings covers Finding 1 of the adjudicated
// review (pipeline.go:96): runMutating rendered eff.Warnings only inside the
// streams.DryRun branch (via renderEffect); the --apply path went
// exec(ctx) -> renderResult(streams, res), which never saw eff.Warnings at
// all. A real --apply --yes delete/patch/create must carry every advisory
// warning (cascade-delete, quorum-impact, version-mismatch) forward onto the
// applied Result, exactly once.
func TestRunApplyRendersEffectWarnings(t *testing.T) {
	t.Parallel()
	cmd := fakeMutating{
		d:   mutatingDesc(),
		eff: safety.Effect{Action: safety.ActionUpdate, Target: "x", Warnings: []string{"note-a"}},
		exec: func(context.Context) (Result, error) {
			return Result{Kind: "ScaleResult"}, nil
		},
	}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", Apply: true, Yes: true},
		safety.Binding{Tier: safety.TierPublic, Stability: safety.Stable})
	require.Nil(t, ce)
	env := decodeEnvelope(t, out.Bytes())
	warnings, ok := env["warnings"].([]any)
	require.True(t, ok, "applied Result envelope must carry the effect's warnings")
	require.Len(t, warnings, 1)
	assert.Equal(t, "note-a", warnings[0])
	assert.Equal(t, 1, strings.Count(out.String(), "note-a"))
}

// TestRunApplyCombinesEffectWarningAndTierWarning covers Finding 1's ordering
// requirement: an Effect.Warnings note and the existing
// TierNorman/no-conflict-detection warning ride together on the applied
// Result, each appearing exactly once, with the effect warning ordered
// BEFORE the tier warning.
func TestRunApplyCombinesEffectWarningAndTierWarning(t *testing.T) {
	t.Parallel()
	cmd := fakeMutating{
		d:   mutatingDesc(),
		eff: safety.Effect{Action: safety.ActionUpdate, Target: "x", ConflictDetection: false, Warnings: []string{"note-a"}},
		exec: func(context.Context) (Result, error) {
			return Result{Kind: "ScaleResult"}, nil
		},
	}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", Apply: true, Yes: true},
		safety.Binding{Tier: safety.TierNorman, Stability: safety.Stable})
	require.Nil(t, ce)
	env := decodeEnvelope(t, out.Bytes())
	warnings, ok := env["warnings"].([]any)
	require.True(t, ok)
	require.Len(t, warnings, 2)
	assert.Equal(t, "note-a", warnings[0], "effect warnings must precede the tier warning")
	assert.Contains(t, warnings[1], "no conflict detection")
	assert.Equal(t, 1, strings.Count(out.String(), "note-a"))
	assert.Equal(t, 1, strings.Count(out.String(), "no conflict detection"))
}

func TestRunExecClosureResultRendered(t *testing.T) {
	t.Parallel()
	called := 0
	cmd := fakeMutating{
		d:   mutatingDesc(),
		eff: safety.Effect{Action: safety.ActionCreate, Target: "x"},
		exec: func(context.Context) (Result, error) {
			called++
			return Result{Kind: "X", Data: map[string]string{"k": "v"}}, nil
		},
	}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", Apply: true}, safety.Binding{})
	require.Nil(t, ce)
	assert.Equal(t, 1, called, "exec must be called exactly once")
	env := decodeEnvelope(t, out.Bytes())
	assert.Equal(t, "X", env["kind"])
}

func TestRunStampsEffectServer(t *testing.T) {
	t.Parallel()
	cmd := fakeMutating{
		d:    Descriptor{Name: "create widget", CommandType: Mutating},
		eff:  safety.Effect{Action: safety.ActionCreate, Cluster: "c-x"},
		exec: func(context.Context) (Result, error) { return Result{Kind: "Widget"}, nil },
	}
	var out bytes.Buffer
	io := IO{Out: &out, Err: &out, DryRun: true, Format: "json",
		ServerIdentity: func() (string, error) { return "prod", nil }}
	ce := Run(context.Background(), cmd, mapInput{}, io, safety.Binding{Tier: safety.TierPublic, Stability: safety.Stable})
	require.Nil(t, ce)
	env := decodeEnvelope(t, out.Bytes())
	data := env["data"].(map[string]any)
	assert.Equal(t, "prod", data["server"])
}

func TestRunRejectsNilExecutor(t *testing.T) {
	t.Parallel()
	cmd := fakeMutating{
		d:   Descriptor{Name: "create widget", CommandType: Mutating},
		eff: safety.Effect{Action: safety.ActionCreate},
		// exec deliberately nil: Plan returned (eff, nil, nil)
	}
	var out, errOut bytes.Buffer
	io := IO{Out: &out, Err: &errOut, Apply: true, Format: "json"}
	ce := Run(context.Background(), cmd, mapInput{}, io, safety.Binding{})
	if ce == nil || ce.Code != clierr.CodeGeneric {
		t.Fatalf("nil executor: got %v, want GENERIC command-bug error (not a panic)", ce)
	}
}

func TestRunReadCommandDryRunRejected(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{d: Descriptor{Name: "get thing", CommandType: ReadOnly}, res: Result{Kind: "Thing"}}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", DryRun: true}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeDryRunNotApplicable, ce.Code)
}

func TestRunUnimplementedContractRejected(t *testing.T) {
	t.Parallel()
	cmd := describeOnly{d: Descriptor{Name: "broken", CommandType: ReadOnly}}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Contains(t, ce.Message, "implements neither")
}

func TestReadCommandExecutes(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{
		d:   Descriptor{Name: "login", CommandType: Local},
		res: Result{Kind: "LoginResult", Data: map[string]string{"ok": "true"}},
	}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.Nil(t, ce)
	env := decodeEnvelope(t, out.Bytes())
	assert.Equal(t, "LoginResult", env["kind"])
}

func TestRawResultBypassesEnvelope(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{
		d: Descriptor{Name: "token", CommandType: Local},
		res: Result{Raw: true, Kind: "ExecCredential",
			Data: map[string]any{"apiVersion": "client.authentication.k8s.io/v1beta1", "kind": "ExecCredential"}},
	}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.Nil(t, ce)
	env := decodeEnvelope(t, out.Bytes())
	assert.Equal(t, "client.authentication.k8s.io/v1beta1", env["apiVersion"])
	assert.NotContains(t, out.String(), "cli.rancher.io/v1")
}

func TestResultTierStabilityReachEnvelope(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{d: Descriptor{Name: "get thing", CommandType: ReadOnly}, res: Result{Kind: "ClusterList", Data: map[string]string{"a": "b"}}}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json"},
		safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta})
	require.Nil(t, ce)
	env := decodeEnvelope(t, out.Bytes())
	assert.Equal(t, "crd", env["tier"])
	assert.Equal(t, "beta", env["stability"])
}

func flaggedReadCmd() fakeRead {
	return fakeRead{
		d: Descriptor{Name: "x", CommandType: Local, Flags: []ArgSpec{
			{Name: "token", Type: "string", Required: true},
			{Name: "auth-flow", Type: "string", Enum: []string{"devicecode", "authcode"}},
		}},
		res: Result{Kind: "OK"},
	}
}

func TestValidateInput(t *testing.T) {
	t.Parallel()
	t.Run("required missing", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		ce := Run(context.Background(), flaggedReadCmd(), mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
		require.NotNil(t, ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
	})
	t.Run("bad enum", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		in := mapInput{s: map[string]string{"token": "id:sec", "auth-flow": "bogus"}}
		ce := Run(context.Background(), flaggedReadCmd(), in, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
		require.NotNil(t, ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
	})
	// Positional args get the same Enum check Flags already had (external
	// review finding 6) — a command like `completion <shell>` can rely on
	// this generic path instead of hand-rolling its own.
	t.Run("bad positional enum", func(t *testing.T) {
		cmd := fakeRead{d: Descriptor{Name: "x", CommandType: Local, Args: []ArgSpec{
			{Name: "shell", Type: "string", Required: true, Enum: []string{"bash", "zsh"}},
		}}, res: Result{Kind: "OK"}}
		var out, errBuf bytes.Buffer
		ce := Run(context.Background(), cmd, mapInput{a: []string{"powershell-classic"}}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
		require.NotNil(t, ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Contains(t, ce.Message, "powershell-classic")
	})
	t.Run("good positional enum passes through", func(t *testing.T) {
		cmd := fakeRead{d: Descriptor{Name: "x", CommandType: Local, Args: []ArgSpec{
			{Name: "shell", Type: "string", Required: true, Enum: []string{"bash", "zsh"}},
		}}, res: Result{Kind: "OK"}}
		var out, errBuf bytes.Buffer
		ce := Run(context.Background(), cmd, mapInput{a: []string{"bash"}}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
		assert.Nil(t, ce)
	})
}

func TestRunRawBytesWrittenVerbatim(t *testing.T) {
	t.Parallel()
	payload := []byte("apiVersion: v1\nkind: Config\n")
	cmd := fakeRead{
		d:   Descriptor{Name: "kubeconfig", CommandType: Local},
		res: Result{RawBytes: payload},
	}
	var out bytes.Buffer
	streams := IO{Out: &out, Err: &bytes.Buffer{}, Format: "json"} // format ignored for raw bytes
	require.Nil(t, Run(context.Background(), cmd, mapInput{}, streams, safety.Binding{}))
	assert.Equal(t, payload, out.Bytes()) // no envelope, no JSON quoting, no trailing mutation
}

func TestRunRejectsResultWithBothRawFields(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{
		d:   Descriptor{Name: "kubeconfig", CommandType: Local},
		res: Result{Raw: true, RawBytes: []byte("x")},
	}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
}

func TestQuietErrorStillRendersJSON(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), flaggedReadCmd(), mapInput{}, IO{Out: &out, Err: &errBuf, Format: "name"}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.NotEmpty(t, errBuf.String(), "error must not be silent under -o name")
	env := decodeEnvelope(t, errBuf.Bytes())
	assert.Equal(t, "Error", env["kind"])
	data := env["data"].(map[string]any)
	assert.NotEmpty(t, data["error"], "error envelope must contain error details")
}

func TestRenderResultTextMode(t *testing.T) {
	text := "server pod-1 ready\nserver pod-2 ready\n"
	data := map[string]any{"pods": []string{"pod-1", "pod-2"}}
	warnings := []string{"pod-3: rbac denied"}

	newCmd := func() fakeRead {
		return fakeRead{
			d:   Descriptor{Name: "logs", CommandType: ReadOnly},
			res: Result{Kind: "LogsResult", Text: text, Data: data, Warnings: warnings},
		}
	}

	t.Run("default format writes Text verbatim and warnings to stderr", func(t *testing.T) {
		t.Parallel()
		var out, errBuf bytes.Buffer
		ce := Run(context.Background(), newCmd(), mapInput{}, IO{Out: &out, Err: &errBuf, Format: ""}, safety.Binding{})
		require.Nil(t, ce)
		assert.Equal(t, text, out.String())
		assert.Contains(t, errBuf.String(), "# warning: pod-3: rbac denied\n")
		assert.NotContains(t, out.String(), "apiVersion")
	})

	t.Run("table format identical to default", func(t *testing.T) {
		t.Parallel()
		var out, errBuf bytes.Buffer
		ce := Run(context.Background(), newCmd(), mapInput{}, IO{Out: &out, Err: &errBuf, Format: "table"}, safety.Binding{})
		require.Nil(t, ce)
		assert.Equal(t, text, out.String())
		assert.Contains(t, errBuf.String(), "# warning: pod-3: rbac denied\n")
	})

	t.Run("json format renders envelope over Data, not raw text", func(t *testing.T) {
		t.Parallel()
		var out, errBuf bytes.Buffer
		ce := Run(context.Background(), newCmd(), mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
		require.Nil(t, ce)
		env := decodeEnvelope(t, out.Bytes())
		assert.Equal(t, "LogsResult", env["kind"])
		edata, ok := env["data"].(map[string]any)
		require.True(t, ok)
		assert.NotNil(t, edata["pods"])
		warn, ok := env["warnings"].([]any)
		require.True(t, ok)
		assert.Contains(t, warn[0], "rbac denied")
		assert.NotContains(t, out.String(), text)
	})

	t.Run("yaml format renders envelope over Data", func(t *testing.T) {
		t.Parallel()
		var out, errBuf bytes.Buffer
		ce := Run(context.Background(), newCmd(), mapInput{}, IO{Out: &out, Err: &errBuf, Format: "yaml"}, safety.Binding{})
		require.Nil(t, ce)
		var env map[string]any
		require.NoError(t, yaml.Unmarshal(out.Bytes(), &env))
		assert.Equal(t, "LogsResult", env["kind"])
		assert.NotContains(t, out.String(), text)
	})

	t.Run("template renders over JSON view of Data", func(t *testing.T) {
		t.Parallel()
		var out, errBuf bytes.Buffer
		ce := Run(context.Background(), newCmd(), mapInput{}, IO{Out: &out, Err: &errBuf, Format: "template={{index .pods 0}}"}, safety.Binding{})
		require.Nil(t, ce)
		assert.Equal(t, "pod-1\n", out.String())
	})

	t.Run("-o name is a documented no-op for non-Tabular Data", func(t *testing.T) {
		t.Parallel()
		var out, errBuf bytes.Buffer
		ce := Run(context.Background(), newCmd(), mapInput{}, IO{Out: &out, Err: &errBuf, Format: "name"}, safety.Binding{})
		require.Nil(t, ce)
		assert.Empty(t, out.String())
	})
}

func TestRenderResultRejectsTextAndRawBytes(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{
		d:   Descriptor{Name: "x", CommandType: ReadOnly},
		res: Result{Text: "x", RawBytes: []byte("y")},
	}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
}

func TestRenderResultRejectsTextAndRaw(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{
		d:   Descriptor{Name: "x", CommandType: ReadOnly},
		res: Result{Text: "x", Raw: true},
	}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
}

// TestRenderResultEnvelopeWarningsTableStderr covers Finding 4 of the
// adjudicated review (pipeline.go:251): renderResult's envelope path set
// env.Warnings but never wrote anything to stderr, and renderTable
// (output/render.go) only ever reads env.Data — so in default/table format
// every Result's warnings silently vanished (e.g. `top node`'s
// metrics-server-absent warning, `inspect` warnings). This is pre-existing
// and broader than provisioning. Table/default mode must now write each
// warning to stderr as "# warning: <w>", stdout must not carry it, and each
// warning must appear exactly once total.
func TestRenderResultEnvelopeWarningsTableStderr(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{
		d:   Descriptor{Name: "get thing", CommandType: ReadOnly},
		res: Result{Kind: "Thing", Data: map[string]string{"a": "b"}, Warnings: []string{"note-a"}},
	}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: ""}, safety.Binding{})
	require.Nil(t, ce)
	assert.Contains(t, errBuf.String(), "# warning: note-a\n")
	assert.NotContains(t, out.String(), "note-a")
	assert.Equal(t, 1, strings.Count(out.String()+errBuf.String(), "note-a"),
		"note-a must appear exactly once total across both streams")
}

// TestRenderResultEnvelopeWarningsJSONEnvelopeOnly covers the -o json/-o yaml
// half of Finding 4: warnings stay inside the envelope only, with no
// duplicate stderr "# warning:" write.
func TestRenderResultEnvelopeWarningsJSONEnvelopeOnly(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{
		d:   Descriptor{Name: "get thing", CommandType: ReadOnly},
		res: Result{Kind: "Thing", Data: map[string]string{"a": "b"}, Warnings: []string{"note-a"}},
	}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.Nil(t, ce)
	env := decodeEnvelope(t, out.Bytes())
	warnings, ok := env["warnings"].([]any)
	require.True(t, ok)
	require.Len(t, warnings, 1)
	assert.Equal(t, "note-a", warnings[0])
	assert.NotContains(t, errBuf.String(), "# warning:", "json mode must not also print warnings to stderr")
	assert.Empty(t, errBuf.String())
}

// nodeMetricsRows mirrors inspect.nodeRows (internal/engine/inspect/topnode.go):
// `top node`'s output.Tabular payload of {"node", "usage"} maps, rendering
// NAME/CPU/MEMORY with "<none>" for a degraded row.
type nodeMetricsRows []map[string]string

func (n nodeMetricsRows) Columns() []string { return []string{"NAME", "CPU", "MEMORY"} }

func (n nodeMetricsRows) Rows() [][]string {
	out := make([][]string, 0, len(n))
	for _, row := range n {
		out = append(out, []string{row["name"], "<none>", "<none>"})
	}
	return out
}

// TestTopNodeShapedResultWarningsSurviveTableOutput is the regression proof
// requested by the adjudication for Finding 4: it demonstrates the
// pre-existing bug (every command's warnings vanished in default/table
// output, not just provisioning's) is fixed, using a Result shaped exactly
// like `top node`'s real output — Kind "NodeMetrics", an output.Tabular Data
// payload, and the one metrics.k8s.io-unavailable warning
// (inspect.metricsUnavailableWarning) topnode.go emits when metrics-server is
// absent. Before the fix, this warning was silently dropped in table/default
// format; a user running `rancher-cli top node` with no metrics-server saw
// no indication their CPU/MEMORY columns were "<none>" because metrics were
// unreachable, not because usage was legitimately zero.
func TestTopNodeShapedResultWarningsSurviveTableOutput(t *testing.T) {
	t.Parallel()
	warning := "metrics.k8s.io unavailable on cluster c-abc123"
	cmd := fakeRead{
		d: Descriptor{Name: "top node", CommandType: ReadOnly},
		res: Result{
			Kind:     "NodeMetrics",
			Data:     nodeMetricsRows{{"name": "node-1"}, {"name": "node-2"}},
			Warnings: []string{warning},
		},
	}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "table"}, safety.Binding{})
	require.Nil(t, ce)
	assert.Contains(t, out.String(), "node-1", "the table must still render the node rows")
	assert.Contains(t, errBuf.String(), "# warning: "+warning+"\n")
	assert.NotContains(t, out.String(), "metrics.k8s.io unavailable")
	assert.Equal(t, 1, strings.Count(out.String()+errBuf.String(), warning))
}

// TestRunValidatesOutputFormat pins the -o contract: the closed set of
// literals plus template=<go-template>, validated BEFORE the command runs.
// Anything else is a structured VALIDATION error — never Go template source.
func TestRunValidatesOutputFormat(t *testing.T) {
	t.Parallel()

	valid := []string{"", "table", "json", "yaml", "name", "template={{.foo}}"}
	for _, format := range valid {
		t.Run("valid format="+format, func(t *testing.T) {
			t.Parallel()
			cmd := fakeRead{
				d:   Descriptor{Name: "get thing", CommandType: ReadOnly},
				res: Result{Kind: "Thing", Data: map[string]string{"foo": "bar"}},
			}
			var out, errBuf bytes.Buffer
			ce := Run(context.Background(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: format}, safety.Binding{})
			require.Nil(t, ce)
		})
	}

	invalid := []struct {
		name    string
		format  string
		message string
		hint    string
	}{
		{
			name:    "unknown value",
			format:  "wide",
			message: `--output="wide" is not one of table|json|yaml|name|template=<go-template>`,
			hint:    "allowed: table, json, yaml, name, template=<go-template>",
		},
		{
			name:    "bare template",
			format:  "template",
			message: `--output="template" is not one of table|json|yaml|name|template=<go-template>`,
			hint:    "allowed: table, json, yaml, name, template=<go-template>",
		},
		{
			name:    "empty template source",
			format:  "template=",
			message: "--output template source is empty",
			hint:    "pass -o template='{{ .field }}'",
		},
		{
			name:    "template parse error",
			format:  "template={{",
			message: "invalid template:",
			hint:    "template must be valid Go text/template syntax",
		},
	}
	for _, tc := range invalid {
		t.Run("invalid "+tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := fakeRead{
				d:   Descriptor{Name: "get thing", CommandType: ReadOnly},
				res: Result{Kind: "Thing", Data: map[string]string{"foo": "bar"}},
			}
			var out, errBuf bytes.Buffer
			ce := Run(context.Background(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: tc.format}, safety.Binding{})
			require.NotNil(t, ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Contains(t, ce.Message, tc.message)
			assert.Equal(t, tc.hint, ce.Hint)
			assert.Empty(t, out.String(), "nothing may reach stdout for an invalid -o")

			env := decodeEnvelope(t, errBuf.Bytes())
			assert.Equal(t, "Error", env["kind"])
			data, ok := env["data"].(map[string]any)
			require.True(t, ok)
			errObj, ok := data["error"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeValidation, errObj["code"])
		})
	}
}

// TestRunInvalidOutputFormatRefusesMutatingBeforePlan proves the pre-execution
// guarantee: a mutation must never execute and then fail at render, so an
// invalid -o refuses the command before Plan is ever invoked (Plan sends real
// write-verb requests unconditionally).
func TestRunInvalidOutputFormatRefusesMutatingBeforePlan(t *testing.T) {
	t.Parallel()
	cmd := refusingPlanMutating{d: mutatingDesc(), t: t}
	var out, errBuf bytes.Buffer
	ce := Run(context.Background(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "wide", Apply: true},
		safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, `--output="wide"`)
}

func TestRenderErrAlwaysJSON(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"", "json", "yaml", "name", "table", "template={{.error.code}}"} {
		t.Run("format="+format, func(t *testing.T) {
			var errBuf bytes.Buffer
			ce := clierr.New(clierr.CodeNotFound, "nope").WithHint("look elsewhere")
			_ = renderErr(IO{Err: &errBuf, Format: format}, ce)

			var env struct {
				Kind string `json:"kind"`
				Data struct {
					Error struct {
						Code string `json:"code"`
						Hint string `json:"hint"`
					} `json:"error"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(errBuf.Bytes(), &env),
				"stderr must be the JSON envelope regardless of -o (CW-1)")
			assert.Equal(t, "Error", env.Kind)
			assert.Equal(t, clierr.CodeNotFound, env.Data.Error.Code)
			assert.Equal(t, "look elsewhere", env.Data.Error.Hint)
		})
	}
}
