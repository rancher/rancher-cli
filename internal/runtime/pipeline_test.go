package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

type mapInput struct {
	s  map[string]string
	b  map[string]bool
	a  []string
	sl map[string][]string
	is map[string]bool
}

func (m mapInput) String(k string) string    { return m.s[k] }
func (m mapInput) Bool(k string) bool        { return m.b[k] }
func (m mapInput) Strings(k string) []string { return m.sl[k] }
func (m mapInput) IsSet(k string) bool       { return m.is[k] }
func (m mapInput) Args() []string            { return m.a }

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
	t.Helper()
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

// refusingPlanMutating fails the test if Plan is ever invoked - used to
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
			_, ce := Run(t.Context(), cmd, mapInput{},
				IO{Out: &out, Err: &errBuf, Format: "json", ReadOnly: true, ReadOnlySources: tc.sources},
				safety.Binding{})
			require.NotNil(t, ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Contains(t, ce.Message, "read-only mode:")
			assert.Contains(t, ce.Message, "clusters scale")
			for _, s := range tc.sources {
				assert.Contains(t, ce.Message, s, "message must name every active read-only source")
			}
		})
	}
}

func TestRunRefusesMutatingDryRunUnderReadOnly(t *testing.T) {
	t.Parallel()
	cmd := refusingPlanMutating{d: mutatingDesc(), t: t}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
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
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", ReadOnly: true, ReadOnlySources: []string{"--read-only"}},
		safety.Binding{})
	require.Nil(t, ce)
}

func TestRunLocalCommandUnaffectedByReadOnlyMode(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{d: Descriptor{Name: "auth login", CommandType: Local}, res: Result{Kind: "LoginResult"}}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", ReadOnly: true, ReadOnlySources: []string{"RANCHER_CLI_READONLY"}},
		safety.Binding{})
	require.Nil(t, ce)
}

func TestRunMutatingUnaffectedWhenReadOnlyFalse(t *testing.T) {
	t.Parallel()
	called := 0
	cmd := fakeMutating{
		d:   mutatingDesc(),
		eff: safety.Effect{Action: safety.ActionUpdate, Target: "x", DisplayKind: "widget", DisplayName: "x"},
		exec: func(context.Context) (Result, error) {
			called++
			return Result{Kind: "ScaleResult"}, nil
		},
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", Apply: true}, // ReadOnly defaults false
		safety.Binding{})
	require.Nil(t, ce)
	assert.Equal(t, 1, called)
}

func TestRunRejectsInvalidAction(t *testing.T) {
	t.Parallel()
	cmd := fakeMutating{
		d:    mutatingDesc(),
		eff:  safety.Effect{Action: safety.Action("destroy"), Target: "x", DisplayKind: "widget", DisplayName: "x"},
		exec: unusedExec(t),
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", Apply: true}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Equal(t, "internal error: invalid action \"destroy\" in the dry-run preview", ce.Message)
}

func TestRunRejectsNonDestructiveDelete(t *testing.T) {
	t.Parallel()
	cmd := fakeMutating{
		d:    mutatingDesc(),
		eff:  safety.Effect{Action: safety.ActionDelete, Target: "x", DisplayKind: "widget", DisplayName: "x", Destructive: false},
		exec: unusedExec(t),
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", Apply: true, Yes: true}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Contains(t, ce.Message, "must be destructive")
}

func TestRunDeprecatedGateUsesBinding(t *testing.T) {
	t.Parallel()
	cmd := fakeMutating{
		d:    mutatingDesc(),
		eff:  safety.Effect{Action: safety.ActionUpdate, Target: "x", DisplayKind: "widget", DisplayName: "x"},
		exec: unusedExec(t),
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", Apply: true}, // no --allow-deprecated
		safety.Binding{Contract: safety.ContractPublic, Stability: safety.Deprecated})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeConfirmationRequired, ce.Code)
	assert.Contains(t, ce.Message, "--allow-deprecated")
}

// TestRenderEffectWarningsJSONOnce covers the case where an Effect.Warnings
// note rides the -o json dry-run envelope's top-level "warnings" exactly once -
// Effect.Warnings' json:"-" tag must not let it leak into env.data too.
func TestRenderEffectWarningsJSONOnce(t *testing.T) {
	t.Parallel()
	cmd := fakeMutating{
		d:    mutatingDesc(),
		eff:  safety.Effect{Action: safety.ActionUpdate, Target: "x", DisplayKind: "widget", DisplayName: "x", Warnings: []string{"note-a"}},
		exec: unusedExec(t),
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", DryRun: true},
		safety.Binding{Contract: safety.ContractPublic, Stability: safety.Stable})
	require.Nil(t, ce)
	env := decodeEnvelope(t, out.Bytes())
	warnings, ok := env["warnings"].([]any)
	require.True(t, ok, "effect envelope must carry warnings")
	require.Len(t, warnings, 1)
	assert.Equal(t, "note-a", warnings[0])
	assert.Equal(t, 1, strings.Count(out.String()+errBuf.String(), "note-a"),
		"note-a must appear exactly once across stdout+stderr")
}

// TestRenderEffectTableRendersDryRunTextWarningsToStderr asserts the pipeline-
// owned dry-run text renderer: table/default format renders the
// action/kind/name/gate text body on stdout, and Effect.Warnings reach the
// operator via the stderr "# warning:" channel - never duplicated into the
// text body itself.
func TestRenderEffectTableRendersDryRunTextWarningsToStderr(t *testing.T) {
	t.Parallel()
	cmd := fakeMutating{
		d: mutatingDesc(),
		eff: safety.Effect{
			Action: safety.ActionUpdate, Target: "x", DisplayKind: "widget", DisplayName: "x",
			Server: "prod", Cluster: "c-x", Warnings: []string{"note-a"},
		},
		exec: unusedExec(t),
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "", DryRun: true},
		safety.Binding{Contract: safety.ContractPublic, Stability: safety.Stable})
	require.Nil(t, ce)
	assert.Equal(t, "update widget/x\n"+
		"  server: prod\n"+
		"  cluster: c-x\n"+
		"  dry-run preview - re-run with --apply to apply the change\n", out.String())
	assert.Equal(t, "# warning: note-a\n", errBuf.String())
}

// TestRenderEffectWideMatchesTable asserts that -o wide renders the dry-run
// preview byte-identical to table/default - there is no separate wide body
// for an Effect preview.
func TestRenderEffectWideMatchesTable(t *testing.T) {
	t.Parallel()
	newCmd := func() fakeMutating {
		return fakeMutating{
			d:    mutatingDesc(),
			eff:  safety.Effect{Action: safety.ActionDelete, Target: "x", DisplayKind: "widget", DisplayName: "x", Destructive: true},
			exec: unusedExec(t),
		}
	}
	var tableOut, wideOut bytes.Buffer
	_, ce := Run(t.Context(), newCmd(), mapInput{},
		IO{Out: &tableOut, Err: io.Discard, Format: "table", DryRun: true}, safety.Binding{})
	require.Nil(t, ce)
	_, ce = Run(t.Context(), newCmd(), mapInput{},
		IO{Out: &wideOut, Err: io.Discard, Format: "wide", DryRun: true}, safety.Binding{})
	require.Nil(t, ce)
	assert.Equal(t, tableOut.String(), wideOut.String())
	assert.Contains(t, tableOut.String(), "dry-run preview - re-run with --apply --yes to apply the change")
}

// TestDryRunTextDiffLines asserts the change-summary content: each Effect.Diff
// entry becomes one line, in order.
func TestDryRunTextDiffLines(t *testing.T) {
	t.Parallel()
	cmd := fakeMutating{
		d: mutatingDesc(),
		eff: safety.Effect{
			Action: safety.ActionUpdate, Target: "x", DisplayKind: "deployment", DisplayName: "nginx",
			Diff: []safety.DiffEntry{{Path: "/spec/replicas", From: float64(1), To: float64(3)}},
		},
		exec: unusedExec(t),
	}
	var out bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: io.Discard, Format: "", DryRun: true}, safety.Binding{})
	require.Nil(t, ce)
	assert.Equal(t, "update deployment/nginx\n"+
		"  /spec/replicas: 1 -> 3\n"+
		"  dry-run preview - re-run with --apply to apply the change\n", out.String())
}

// TestDryRunTextDiffValues asserts a map or list value prints as compact JSON
// on its entry's one line (<, > and & as typed, a newline as the two
// characters \n), and a missing side prints as <nil>, inside the map or
// list as a null and as a side of the entry.
func TestDryRunTextDiffValues(t *testing.T) {
	t.Parallel()
	cmd := fakeMutating{
		d: mutatingDesc(),
		eff: safety.Effect{
			Action: safety.ActionCreate, Target: "x", DisplayKind: "project", DisplayName: "web",
			Diff: []safety.DiffEntry{
				{Path: "/spec/limit", To: map[string]any{"limitsCpu": "500m", "n": nil}},
				{Path: "/spec/uris", To: []any{"https://a/?x=1&y=<2>", nil}},
				{Path: "/spec/empty", To: map[string]any{}},
				{Path: "/spec/none", To: []any{}},
				{Path: "/spec/text", To: "a\nb"},
				{Path: "/spec/json", To: map[string]any{"k": "line1\nline2"}},
				{Path: "/spec/removed", From: []any{"a", "b"}},
			},
		},
		exec: unusedExec(t),
	}
	var out bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: io.Discard, Format: "", DryRun: true}, safety.Binding{})
	require.Nil(t, ce)
	assert.Equal(t, "create project/web\n"+
		"  /spec/limit: <nil> -> {\"limitsCpu\":\"500m\",\"n\":null}\n"+
		"  /spec/uris: <nil> -> [\"https://a/?x=1&y=<2>\",null]\n"+
		"  /spec/empty: <nil> -> {}\n"+
		"  /spec/none: <nil> -> []\n"+
		"  /spec/text: <nil> -> ab\n"+
		"  /spec/json: <nil> -> {\"k\":\"line1\\nline2\"}\n"+
		"  /spec/removed: [\"a\",\"b\"] -> <nil>\n"+
		"  dry-run preview - re-run with --apply to apply the change\n", out.String())
}

// TestRenderEffectRawFormatsWarningsToStderr asserts that raw-output formats
// (-o name, -o template=...) carry only the target field on stdout - the Effect
// envelope's "warnings" key is never emitted - so renderEffect must mirror each
// warning to stderr as "# warning: <w>" so they are not silently dropped (same
// contract as renderResult under raw formats).
func TestRenderEffectRawFormatsWarningsToStderr(t *testing.T) {
	t.Parallel()
	formats := []string{"name", "template={{.target}}"}
	for _, format := range formats {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			cmd := fakeMutating{
				d:    mutatingDesc(),
				eff:  safety.Effect{Action: safety.ActionUpdate, Target: "x", DisplayKind: "widget", DisplayName: "x", Warnings: []string{"cascade-warning"}},
				exec: unusedExec(t),
			}
			var out, errBuf bytes.Buffer
			_, ce := Run(t.Context(), cmd, mapInput{},
				IO{Out: &out, Err: &errBuf, Format: format, DryRun: true},
				safety.Binding{Contract: safety.ContractPublic, Stability: safety.Stable})
			require.Nil(t, ce)
			assert.Contains(t, errBuf.String(), "# warning: cascade-warning\n")
			assert.NotContains(t, out.String(), "cascade-warning")
			assert.Equal(t, 1, strings.Count(out.String()+errBuf.String(), "cascade-warning"),
				"cascade-warning must appear exactly once total across both streams")
		})
	}
}

// TestRenderEffectWarningsCombineDryRun covers case (c): two Effect.Warnings
// notes ride together, each appearing exactly once, in -o json dry-run
// output, in the original order. The default/table dry-run body has no
// dedicated text renderer yet (see
// TestRenderEffectWarningsTableStderrOnlyDryRunTableHasNoTextRendererYet) -
// only json/yaml and the raw formats are exercised here.
func TestRenderEffectWarningsCombineDryRun(t *testing.T) {
	newCmd := func() fakeMutating {
		return fakeMutating{
			d:    mutatingDesc(),
			eff:  safety.Effect{Action: safety.ActionUpdate, Target: "x", DisplayKind: "widget", DisplayName: "x", Warnings: []string{"note-a", "note-b"}},
			exec: unusedExec(t),
		}
	}

	t.Run("json", func(t *testing.T) {
		t.Parallel()
		var out, errBuf bytes.Buffer
		_, ce := Run(t.Context(), newCmd(), mapInput{},
			IO{Out: &out, Err: &errBuf, Format: "json", DryRun: true},
			safety.Binding{Contract: safety.ContractPublic, Stability: safety.Stable})
		require.Nil(t, ce)
		env := decodeEnvelope(t, out.Bytes())
		warnings, ok := env["warnings"].([]any)
		require.True(t, ok)
		require.Len(t, warnings, 2)
		assert.Equal(t, "note-a", warnings[0])
		assert.Equal(t, "note-b", warnings[1])
	})
}

// TestRunApplyRendersEffectWarnings asserts that an --apply run carries every
// Effect.Warnings entry (cascade-delete, quorum-impact, version-mismatch)
// onto the applied Result, exactly once, not only into the dry-run preview.
func TestRunApplyRendersEffectWarnings(t *testing.T) {
	t.Parallel()
	cmd := fakeMutating{
		d:   mutatingDesc(),
		eff: safety.Effect{Action: safety.ActionUpdate, Target: "x", DisplayKind: "widget", DisplayName: "x", Warnings: []string{"note-a"}},
		exec: func(context.Context) (Result, error) {
			return Result{Kind: "ScaleResult"}, nil
		},
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", Apply: true, Yes: true},
		safety.Binding{Contract: safety.ContractPublic, Stability: safety.Stable})
	require.Nil(t, ce)
	env := decodeEnvelope(t, out.Bytes())
	warnings, ok := env["warnings"].([]any)
	require.True(t, ok, "applied Result envelope must carry the effect's warnings")
	require.Len(t, warnings, 1)
	assert.Equal(t, "note-a", warnings[0])
	assert.Equal(t, 1, strings.Count(out.String(), "note-a"))
}

// TestRunApplyCombinesEffectWarnings covers ordering: multiple Effect.Warnings
// notes ride together on the applied Result in the original order, each
// appearing exactly once.
func TestRunApplyCombinesEffectWarnings(t *testing.T) {
	t.Parallel()
	cmd := fakeMutating{
		d:   mutatingDesc(),
		eff: safety.Effect{Action: safety.ActionUpdate, Target: "x", DisplayKind: "widget", DisplayName: "x", Warnings: []string{"note-a", "note-b"}},
		exec: func(context.Context) (Result, error) {
			return Result{Kind: "UpdateResult"}, nil
		},
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", Apply: true, Yes: true},
		safety.Binding{Contract: safety.ContractInternal, Stability: safety.Stable})
	require.Nil(t, ce)
	env := decodeEnvelope(t, out.Bytes())
	warnings, ok := env["warnings"].([]any)
	require.True(t, ok)
	require.Len(t, warnings, 2)
	assert.Equal(t, "note-a", warnings[0])
	assert.Equal(t, "note-b", warnings[1])
}

func TestRunExecClosureResultRendered(t *testing.T) {
	t.Parallel()
	called := 0
	cmd := fakeMutating{
		d:   mutatingDesc(),
		eff: safety.Effect{Action: safety.ActionCreate, Target: "x", DisplayKind: "widget", DisplayName: "x"},
		exec: func(context.Context) (Result, error) {
			called++
			return Result{Kind: "X", Data: map[string]string{"k": "v"}}, nil
		},
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
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
		eff:  safety.Effect{Action: safety.ActionCreate, Cluster: "c-x", DisplayKind: "widget", DisplayName: "x"},
		exec: func(context.Context) (Result, error) { return Result{Kind: "Widget"}, nil },
	}
	var out bytes.Buffer
	io := IO{Out: &out, Err: &out, DryRun: true, Format: "json",
		ServerIdentity: func() (string, error) { return "prod", nil }}
	_, ce := Run(t.Context(), cmd, mapInput{}, io, safety.Binding{Contract: safety.ContractPublic, Stability: safety.Stable})
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
	_, ce := Run(t.Context(), cmd, mapInput{}, io, safety.Binding{})
	if ce == nil || ce.Code != clierr.CodeGeneric {
		t.Fatalf("nil executor: got %v, want GENERIC internal error (not a panic)", ce)
	}
}

func TestRunReadCommandDryRunRejected(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{d: Descriptor{Name: "get thing", CommandType: ReadOnly}, res: Result{Kind: "Thing"}}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", DryRun: true}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeDryRunNotApplicable, ce.Code)
	assert.Equal(t, "get thing is a read-only command; re-run without --dry-run", ce.Message)
}

func TestRunUnimplementedContractRejected(t *testing.T) {
	t.Parallel()
	cmd := describeOnly{d: Descriptor{Name: "broken", CommandType: ReadOnly}}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
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
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.Nil(t, ce)
	env := decodeEnvelope(t, out.Bytes())
	assert.Equal(t, "LoginResult", env["kind"])
}

// textTableProbe records the Input a read command's Execute receives.
type textTableProbe struct {
	d   Descriptor
	got *Input
}

func (f textTableProbe) Describe() Descriptor { return f.d }
func (f textTableProbe) Execute(_ context.Context, in Input, _ Prompter) (Result, error) {
	*f.got = in
	return Result{Kind: "Thing", Text: "ok\n", Data: map[string]string{"a": "b"}}, nil
}

// TestReadCommandLearnsWhetherATextTableRenders asserts the Input a read
// command receives reports a text table for the default, table and wide
// formats and none for every other format, and still answers the command's
// own flag values.
func TestReadCommandLearnsWhetherATextTableRenders(t *testing.T) {
	t.Parallel()
	for format, want := range map[string]bool{
		"": true, "table": true, "wide": true,
		"json": false, "yaml": false, "name": false, "template=x{{ . }}": false,
	} {
		t.Run("format "+format, func(t *testing.T) {
			t.Parallel()
			var got Input
			cmd := textTableProbe{d: Descriptor{Name: "get thing", CommandType: ReadOnly, Shape: ShapeText}, got: &got}
			var out, errBuf bytes.Buffer
			_, ce := Run(t.Context(), cmd, mapInput{s: map[string]string{"selector": "a=b"}},
				IO{Out: &out, Err: &errBuf, Format: format}, safety.Binding{})
			require.Nil(t, ce)
			tt, ok := got.(TextTableInput)
			require.True(t, ok, "the pipeline hands a command a TextTableInput")
			assert.Equal(t, want, tt.TextTable())
			assert.Equal(t, "a=b", tt.String("selector"))
		})
	}
}

func TestRawResultBypassesEnvelope(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{
		d: Descriptor{Name: "token", CommandType: Local},
		res: Result{Raw: true, Kind: "ExecCredential",
			Data: map[string]any{"apiVersion": "client.authentication.k8s.io/v1beta1", "kind": "ExecCredential"}},
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.Nil(t, ce)
	env := decodeEnvelope(t, out.Bytes())
	assert.Equal(t, "client.authentication.k8s.io/v1beta1", env["apiVersion"])
	assert.NotContains(t, out.String(), "cli.rancher.io/v1")
}

// TestEnvelopeHasNoContractOrStability verifies that contract/stability are
// NOT serialized into the output envelope - the schema manifest is the single
// published source; per-result stamping was removed in the contract redesign.
func TestEnvelopeHasNoContractOrStability(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{d: Descriptor{Name: "get thing", CommandType: ReadOnly}, res: Result{Kind: "ClusterList", Data: map[string]string{"a": "b"}}}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json"},
		safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta})
	require.Nil(t, ce)
	env := decodeEnvelope(t, out.Bytes())
	assert.NotContains(t, env, "tier")
	assert.NotContains(t, env, "contract")
	assert.NotContains(t, env, "stability")
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
		_, ce := Run(t.Context(), flaggedReadCmd(), mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
		require.NotNil(t, ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
	})
	t.Run("bad enum", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		in := mapInput{s: map[string]string{"token": "id:sec", "auth-flow": "bogus"}}
		_, ce := Run(t.Context(), flaggedReadCmd(), in, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
		require.NotNil(t, ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
	})
	// Positional args get the same Enum check Flags already had - a command
	// like `completion <shell>` can rely on this generic path instead of
	// hand-rolling its own.
	t.Run("bad positional enum", func(t *testing.T) {
		cmd := fakeRead{d: Descriptor{Name: "x", CommandType: Local, Args: []ArgSpec{
			{Name: "shell", Type: "string", Required: true, Enum: []string{"bash", "zsh"}},
		}}, res: Result{Kind: "OK"}}
		var out, errBuf bytes.Buffer
		_, ce := Run(t.Context(), cmd, mapInput{a: []string{"powershell-classic"}}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
		require.NotNil(t, ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Contains(t, ce.Message, "powershell-classic")
	})
	t.Run("good positional enum passes through", func(t *testing.T) {
		cmd := fakeRead{d: Descriptor{Name: "x", CommandType: Local, Args: []ArgSpec{
			{Name: "shell", Type: "string", Required: true, Enum: []string{"bash", "zsh"}},
		}}, res: Result{Kind: "OK"}}
		var out, errBuf bytes.Buffer
		_, ce := Run(t.Context(), cmd, mapInput{a: []string{"bash"}}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
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
	_, ce := Run(t.Context(), cmd, mapInput{}, streams, safety.Binding{})
	require.Nil(t, ce)
	assert.Equal(t, payload, out.Bytes()) // no envelope, no JSON quoting, no trailing mutation
}

func TestRunRejectsResultWithBothRawFields(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{
		d:   Descriptor{Name: "kubeconfig", CommandType: Local},
		res: Result{Raw: true, RawBytes: []byte("x")},
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
}

// TestQuietErrorStillRendersText asserts that a quiet stdout format (-o name,
// whose stdout is bare identifiers) still surfaces an error on stderr - as
// text, since name is not json/yaml.
func TestQuietErrorStillRendersText(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), flaggedReadCmd(), mapInput{}, IO{Out: &out, Err: &errBuf, Format: "name"}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.NotEmpty(t, errBuf.String(), "error must not be silent under -o name")
	assert.Contains(t, errBuf.String(), "error (VALIDATION):")
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
		_, ce := Run(t.Context(), newCmd(), mapInput{}, IO{Out: &out, Err: &errBuf, Format: ""}, safety.Binding{})
		require.Nil(t, ce)
		assert.Equal(t, text, out.String())
		assert.Contains(t, errBuf.String(), "# warning: pod-3: rbac denied\n")
		assert.NotContains(t, out.String(), "apiVersion")
	})

	t.Run("table format identical to default", func(t *testing.T) {
		t.Parallel()
		var out, errBuf bytes.Buffer
		_, ce := Run(t.Context(), newCmd(), mapInput{}, IO{Out: &out, Err: &errBuf, Format: "table"}, safety.Binding{})
		require.Nil(t, ce)
		assert.Equal(t, text, out.String())
		assert.Contains(t, errBuf.String(), "# warning: pod-3: rbac denied\n")
	})

	t.Run("json format renders envelope over Data, not raw text", func(t *testing.T) {
		t.Parallel()
		var out, errBuf bytes.Buffer
		_, ce := Run(t.Context(), newCmd(), mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
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
		_, ce := Run(t.Context(), newCmd(), mapInput{}, IO{Out: &out, Err: &errBuf, Format: "yaml"}, safety.Binding{})
		require.Nil(t, ce)
		var env map[string]any
		require.NoError(t, yaml.Unmarshal(out.Bytes(), &env))
		assert.Equal(t, "LogsResult", env["kind"])
		assert.NotContains(t, out.String(), text)
	})

	t.Run("template renders over JSON view of Data", func(t *testing.T) {
		t.Parallel()
		var out, errBuf bytes.Buffer
		_, ce := Run(t.Context(), newCmd(), mapInput{}, IO{Out: &out, Err: &errBuf, Format: "template={{index .pods 0}}"}, safety.Binding{})
		require.Nil(t, ce)
		assert.Equal(t, "pod-1\n", out.String())
	})

	t.Run("-o name is a documented no-op for non-Tabular Data", func(t *testing.T) {
		t.Parallel()
		var out, errBuf bytes.Buffer
		_, ce := Run(t.Context(), newCmd(), mapInput{}, IO{Out: &out, Err: &errBuf, Format: "name"}, safety.Binding{})
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
	_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
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
	_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
}

func TestRenderResultRejectsSummaryAndRawBytes(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{
		d:   Descriptor{Name: "x", CommandType: ReadOnly},
		res: Result{Summary: "done", RawBytes: []byte("y")},
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Contains(t, ce.Message, "internal error")
}

func TestRenderResultRejectsTextAndSummary(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{
		d:   Descriptor{Name: "x", CommandType: ReadOnly},
		res: Result{Text: "hello\n", Summary: "done"},
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Contains(t, ce.Message, "internal error")
}

func TestRenderResultRejectsSummaryAndRaw(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{
		d:   Descriptor{Name: "x", CommandType: ReadOnly},
		res: Result{Summary: "done", Raw: true, Data: map[string]string{"k": "v"}},
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Contains(t, ce.Message, "internal error")
}

// TestRenderResultShapeMismatchIsInternalError asserts the runtime enforcement of a
// Descriptor's declared OutputShape: a command whose Result does not carry
// the payload its own Shape promises is an internal error, not a silent
// fallback.
func TestRenderResultShapeMismatchIsInternalError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		d      Descriptor
		res    Result
		format string
	}{
		{
			name:   "ShapeTabular with non-Tabular Data",
			d:      Descriptor{Name: "get thing", CommandType: ReadOnly, Shape: ShapeTabular},
			res:    Result{Kind: "Thing", Data: map[string]string{"a": "b"}},
			format: "json", // ShapeTabular's check is format-independent: Data must implement Tabular under every format.
		},
		{
			name:   "ShapeText with no Text under table format",
			d:      Descriptor{Name: "patch thing", CommandType: ReadOnly, Shape: ShapeText},
			res:    Result{Kind: "Thing", Data: map[string]string{"a": "b"}},
			format: "", // Text is only consumed by the table-family render path; see the json/yaml regression test below.
		},
		{
			name:   "ShapeBytes with no RawBytes",
			d:      Descriptor{Name: "kubeconfig", CommandType: ReadOnly, Shape: ShapeBytes},
			res:    Result{},
			format: "json", // RawBytes is written verbatim regardless of format.
		},
		{
			name:   "ShapeExecJSON with Raw unset",
			d:      Descriptor{Name: "auth get-token", CommandType: ReadOnly, Shape: ShapeExecJSON},
			res:    Result{Kind: "ExecCredential", Data: map[string]string{"a": "b"}},
			format: "json", // the Raw branch fires regardless of format.
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := fakeRead{d: tc.d, res: tc.res}
			var out, errBuf bytes.Buffer
			_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: tc.format}, safety.Binding{})
			require.NotNil(t, ce)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.Contains(t, ce.Message, "internal error")
			assert.Empty(t, out.String(), "a shape-mismatch result must never partially render")
			assert.NotEmpty(t, errBuf.String(), "a shape-mismatch error must still be rendered to stderr")
		})
	}
}

// TestMutatingShapeViolationStillEmitsDiagnosticsAfterCommit asserts a
// pipeline-wide guarantee: a mutating command whose executor already
// committed state, but whose returned Result fails shape validation, must
// still exit non-zero with a rendered diagnostic on stderr - never a silent
// exit 1 with zero bytes on both streams after the change has already
// landed server-side. Any future render path that returns an error from
// renderResult without rendering it first will fail this matrix.
func TestMutatingShapeViolationStillEmitsDiagnosticsAfterCommit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		shape  OutputShape
		res    Result
		format string
	}{
		{name: "ShapeText no Text", shape: ShapeText, res: Result{Kind: "Thing"}, format: ""},
		{name: "ShapeTabular non-Tabular Data", shape: ShapeTabular, res: Result{Kind: "Thing", Data: map[string]string{"a": "b"}}, format: "json"},
		{name: "ShapeBytes no RawBytes", shape: ShapeBytes, res: Result{}, format: "json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var committed bool
			cmd := fakeMutating{
				d:   Descriptor{Name: "patch thing", CommandType: Mutating, Shape: tc.shape},
				eff: safety.Effect{Action: safety.ActionUpdate, DisplayKind: "thing", DisplayName: "t1"},
				exec: func(context.Context) (Result, error) {
					committed = true
					return tc.res, nil
				},
			}
			var out, errBuf bytes.Buffer
			status, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: tc.format, Apply: true}, safety.Binding{})
			require.True(t, committed, "the mutation must have already executed before the shape check runs")
			require.NotNil(t, ce)
			assert.Equal(t, 0, status)
			assert.NotEmpty(t, errBuf.String(), "a post-commit render failure must still be reported on stderr")
		})
	}
}

// TestRenderResultShapeTextUntouchedUnderJSONAndYAML asserts the coexistence
// rule: a ShapeText command whose Text is not yet populated must still
// render its Data envelope unchanged under -o json/-o yaml (and under -o
// name/-o template, which read Data the same way) - only the table-family
// render path (table/default/wide) actually reads Text, so an empty Text
// is not a shape violation anywhere else. This is the exact contract that
// lets a not-yet-implemented ShapeText command's -o json keep working
// exactly as it did before Shape was declared.
func TestRenderResultShapeTextUntouchedUnderJSONAndYAML(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"json", "yaml", "name", "template={{.a}}"} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			cmd := fakeRead{
				d:   Descriptor{Name: "patch thing", CommandType: ReadOnly, Shape: ShapeText},
				res: Result{Kind: "Thing", Data: map[string]string{"a": "b"}},
			}
			var out, errBuf bytes.Buffer
			_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: format}, safety.Binding{})
			require.Nil(t, ce, "an unpopulated Text must not fail render under %s", format)
			assert.Empty(t, errBuf.String())
		})
	}
}

// TestRenderResultShapeMatchSucceeds is the passing counterpart: a Result
// that carries exactly the payload its Descriptor's Shape declares renders
// normally, with no shape-mismatch error.
func TestRenderResultShapeMatchSucceeds(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{
		d:   Descriptor{Name: "patch thing", CommandType: ReadOnly, Shape: ShapeText},
		res: Result{Kind: "Thing", Text: "thing/x patched\n"},
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: ""}, safety.Binding{})
	require.Nil(t, ce)
	assert.Equal(t, "thing/x patched\n", out.String())
	assert.Empty(t, errBuf.String())
}

// TestRenderResultShapeTextRendersUnderWide asserts the wide-is-table-family
// rule for ShapeText: -o wide renders exactly the Text, with no separate
// wide body and no envelope fallback. Without this, a ShapeText command's
// once-only secret would commit the mutation and then fail to render under
// -o wide.
func TestRenderResultShapeTextRendersUnderWide(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{
		d:   Descriptor{Name: "create token", CommandType: ReadOnly, Shape: ShapeText},
		res: Result{Kind: "Token", Text: "token/x created\n  bearer token: abc\n"},
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "wide"}, safety.Binding{})
	require.Nil(t, ce)
	assert.Equal(t, "token/x created\n  bearer token: abc\n", out.String())
}

// TestRenderResultUsesWideTextOverride asserts WideText replaces Text
// under -o wide alone: the default and table formats keep Text, and a
// result without a WideText renders Text under wide as every command did
// before.
func TestRenderResultUsesWideTextOverride(t *testing.T) {
	t.Parallel()
	const text = "grouped\n"
	const wide = "NAMESPACE  ROW\nteam-a     x\n"
	cases := []struct {
		name   string
		result Result
		format string
		want   string
	}{
		{"wide picks WideText", Result{Kind: "Thing", Text: text, WideText: wide}, "wide", wide},
		{"table keeps Text", Result{Kind: "Thing", Text: text, WideText: wide}, "table", text},
		{"default keeps Text", Result{Kind: "Thing", Text: text, WideText: wide}, "", text},
		{"no WideText falls back", Result{Kind: "Thing", Text: text}, "wide", text},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := fakeRead{d: Descriptor{Name: "access list", CommandType: ReadOnly, Shape: ShapeText}, res: c.result}
			var out, errBuf bytes.Buffer
			_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: c.format}, safety.Binding{})
			require.Nil(t, ce)
			assert.Equal(t, c.want, out.String())
		})
	}
}

// TestCheckResultShapeRejectsWideTextWithoutShapeText asserts WideText is
// a projection of a ShapeText Text, never a payload of its own: any other
// shape, or an empty Text, is an internal error.
func TestCheckResultShapeRejectsWideTextWithoutShapeText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		d    Descriptor
		res  Result
	}{
		{"tabular shape", Descriptor{Name: "get thing", Shape: ShapeTabular}, Result{Text: "x\n", WideText: "y\n"}},
		{"no text", Descriptor{Name: "access list", Shape: ShapeText}, Result{WideText: "y\n"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ce := checkResultShape(c.d, c.res, "wide")
			require.NotNil(t, ce)
			assert.Contains(t, ce.Error(), "returned WideText without a ShapeText Text")
		})
	}
}

// TestCheckResultShapeRejectsWideAndStyledText asserts the two text
// projections never combine: StyledText styles Text, and no builder styles
// a wide view, so a result carrying both is an internal error.
func TestCheckResultShapeRejectsWideAndStyledText(t *testing.T) {
	t.Parallel()
	d := Descriptor{Name: "access list", Shape: ShapeText}
	res := Result{Text: "x\n", WideText: "y\n", StyledText: func(style.Style) string { return "x\n" }}
	ce := checkResultShape(d, res, "wide")
	require.NotNil(t, ce)
	assert.Contains(t, ce.Error(), "returned both WideText and StyledText")
}

// TestRenderResultEnvelopeWarningsTableStderr asserts that table/default mode
// writes each Result warning to stderr as "# warning: <w>": renderTable
// (output/render.go) reads only env.Data, so the table on stdout never
// carries it, and each warning appears exactly once across both streams.
func TestRenderResultEnvelopeWarningsTableStderr(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{
		d:   Descriptor{Name: "get thing", CommandType: ReadOnly, Shape: ShapeTabular},
		res: Result{Kind: "Thing", Data: nodeMetricsRows{{"name": "node-1"}}, Warnings: []string{"note-a"}},
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: ""}, safety.Binding{})
	require.Nil(t, ce)
	assert.Contains(t, errBuf.String(), "# warning: note-a\n")
	assert.NotContains(t, out.String(), "note-a")
	assert.Equal(t, 1, strings.Count(out.String()+errBuf.String(), "note-a"),
		"note-a must appear exactly once total across both streams")
}

// TestRenderResultEnvelopeWarningsJSONEnvelopeOnly covers the -o json/-o yaml
// half: warnings stay inside the envelope only, with no
// duplicate stderr "# warning:" write.
func TestRenderResultEnvelopeWarningsJSONEnvelopeOnly(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{
		d:   Descriptor{Name: "get thing", CommandType: ReadOnly},
		res: Result{Kind: "Thing", Data: map[string]string{"a": "b"}, Warnings: []string{"note-a"}},
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.Nil(t, ce)
	env := decodeEnvelope(t, out.Bytes())
	warnings, ok := env["warnings"].([]any)
	require.True(t, ok)
	require.Len(t, warnings, 1)
	assert.Equal(t, "note-a", warnings[0])
	assert.NotContains(t, errBuf.String(), "# warning:", "json mode must not also print warnings to stderr")
	assert.Empty(t, errBuf.String())
}

// nodeMetricsRows is an output.Tabular payload of {"name"} maps that renders
// NAME/CPU/MEMORY with "<none>" for CPU and MEMORY, as a degraded
// node-metrics row reads.
type nodeMetricsRows []map[string]string

func (n nodeMetricsRows) Columns() []string { return []string{"NAME", "CPU", "MEMORY"} }

func (n nodeMetricsRows) Rows() [][]string {
	out := make([][]string, 0, len(n))
	for _, row := range n {
		out = append(out, []string{row["name"], "<none>", "<none>"})
	}
	return out
}

// fooTabularData is a minimal Tabular fixture that also carries a JSON field
// (Foo), so the same value renders under every format: table/wide (via
// Columns/Rows), and template/json/yaml (via the Foo field its JSON view
// exposes).
type fooTabularData struct {
	Foo string `json:"foo"`
}

func (f fooTabularData) Columns() []string { return []string{"NAME"} }
func (f fooTabularData) Rows() [][]string  { return [][]string{{f.Foo}} }

// TestTopNodeShapedResultWarningsSurviveTableOutput asserts, with a Result
// shaped like a node-metrics read - Kind "NodeMetrics", an output.Tabular Data
// payload, and one metrics-unavailable warning - that the warning reaches
// stderr in table/default format while the table still renders: without it a
// user cannot tell that "<none>" CPU/MEMORY columns mean metrics were
// unreachable, not that usage is zero.
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
	_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "table"}, safety.Binding{})
	require.Nil(t, ce)
	assert.Contains(t, out.String(), "node-1", "the table must still render the node rows")
	assert.Contains(t, errBuf.String(), "# warning: "+warning+"\n")
	assert.NotContains(t, out.String(), "metrics.k8s.io unavailable")
	assert.Equal(t, 1, strings.Count(out.String()+errBuf.String(), warning))
}

// TestRenderResultRawFormatsWarningsToStderr asserts that raw-output
// formats (-o name, -o template=...) carry only the named field on
// stdout - the envelope's warnings key is never emitted - so renderResult must
// mirror each warning to stderr as "# warning: <w>" so they are not silently
// dropped.
func TestRenderResultRawFormatsWarningsToStderr(t *testing.T) {
	t.Parallel()
	formats := []string{"name", "template={{.name}}"}
	for _, format := range formats {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			cmd := fakeRead{
				d: Descriptor{Name: "get cluster", CommandType: ReadOnly},
				res: Result{
					Kind:     "Cluster",
					Data:     map[string]string{"name": "c-abc"},
					Warnings: []string{"cert-expiry: 3 days"},
				},
			}
			var out, errBuf bytes.Buffer
			_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: format}, safety.Binding{})
			require.Nil(t, ce)
			assert.Contains(t, errBuf.String(), "# warning: cert-expiry: 3 days\n")
			assert.NotContains(t, out.String(), "cert-expiry")
			assert.Equal(t, 1, strings.Count(out.String()+errBuf.String(), "cert-expiry"),
				"warning must appear exactly once total across both streams")
		})
	}
}

// TestRunValidatesOutputFormat asserts the -o contract: the closed set of
// literals plus template=<go-template>, validated BEFORE the command runs.
// Anything else is a structured VALIDATION error - never Go template source.
func TestRunValidatesOutputFormat(t *testing.T) {
	t.Parallel()

	valid := []string{"", "table", "wide", "json", "yaml", "name", "template={{.foo}}"}
	for _, format := range valid {
		t.Run("valid format="+format, func(t *testing.T) {
			t.Parallel()
			cmd := fakeRead{
				d:   Descriptor{Name: "get thing", CommandType: ReadOnly, Shape: ShapeTabular},
				res: Result{Kind: "Thing", Data: fooTabularData{Foo: "bar"}},
			}
			var out, errBuf bytes.Buffer
			_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: format}, safety.Binding{})
			require.Nil(t, ce)
		})
	}

	invalid := []struct {
		name    string
		format  string
		message string
	}{
		{
			name:    "unknown value",
			format:  "csv",
			message: `--output="csv" is not one of table|wide|json|yaml|name|template=<go-template>`,
		},
		{
			name:    "bare template",
			format:  "template",
			message: `--output="template" is not one of table|wide|json|yaml|name|template=<go-template>`,
		},
		{
			name:    "empty template",
			format:  "template=",
			message: "--output template is empty; pass -o template='{{ .field }}'",
		},
		{
			name:    "template parse error",
			format:  "template={{",
			message: "--output template does not parse:",
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
			_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: tc.format}, safety.Binding{})
			require.NotNil(t, ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Contains(t, ce.Message, tc.message)
			assert.Empty(t, ce.Hint, "an unsupported -o value carries its guidance in the message, not a hint")
			assert.Empty(t, out.String(), "nothing may reach stdout for an invalid -o")

			// The invalid value itself is untrusted, so it can never select
			// envelope rendering: every case here renders as text, the error
			// that complains about the format IS the format.
			assert.Contains(t, errBuf.String(), "error (VALIDATION): "+ce.Message)
			assert.NotContains(t, errBuf.String(), "hint:", "no hint line when the guidance already lives in the message")
		})
	}
}

// TestRunValidatesColor asserts the --color contract: "" (omitted), "auto",
// "always", and "never" are accepted; anything else is a structured
// VALIDATION error, never a silent fall-through to auto.
func TestRunValidatesColor(t *testing.T) {
	t.Parallel()

	valid := []string{"", "auto", "always", "never"}
	for _, color := range valid {
		t.Run("valid color="+color, func(t *testing.T) {
			t.Parallel()
			cmd := fakeRead{
				d:   Descriptor{Name: "get thing", CommandType: ReadOnly, Shape: ShapeTabular},
				res: Result{Kind: "Thing", Data: fooTabularData{Foo: "bar"}},
			}
			var out, errBuf bytes.Buffer
			_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Color: color}, safety.Binding{})
			require.Nil(t, ce)
		})
	}

	cmd := fakeRead{
		d:   Descriptor{Name: "get thing", CommandType: ReadOnly},
		res: Result{Kind: "Thing", Data: map[string]string{"foo": "bar"}},
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Color: "rainbow"}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, `--color="rainbow" is not one of auto|always|never`)
	assert.Empty(t, ce.Hint, "the allowed values already live in the message")
	assert.Empty(t, out.String(), "nothing may reach stdout for an invalid --color")
}

// TestRunInvalidOutputFormatRefusesMutatingBeforePlan proves the pre-execution
// guarantee: a mutation must never execute and then fail at render, so an
// invalid -o refuses the command before Plan is ever invoked (Plan sends real
// write-verb requests unconditionally).
func TestRunInvalidOutputFormatRefusesMutatingBeforePlan(t *testing.T) {
	t.Parallel()
	cmd := refusingPlanMutating{d: mutatingDesc(), t: t}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "csv", Apply: true},
		safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, `--output="csv"`)
}

// TestRenderErrCandidatesTotal asserts the text renderer says how many of
// the full list a cut shows, the line is unchanged without a total, and
// the envelope carries candidatesTotal only when the list was cut.
func TestRenderErrCandidatesTotal(t *testing.T) {
	t.Parallel()
	full := make([]clierr.Candidate, 37)
	for i := range full {
		full[i] = clierr.Candidate{Name: fmt.Sprintf("n%02d", i), ID: fmt.Sprintf("u-%02d", i)}
	}
	shown := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprintf("n%02d (u-%02d)", i, i)
		}
		return strings.Join(parts, ", ")
	}

	t.Run("text with a total", func(t *testing.T) {
		t.Parallel()
		ce := clierr.New(clierr.CodeNotFound, "user not found: x").WithCandidatesCut(full, 10)
		var errBuf bytes.Buffer
		_ = renderErr(IO{Err: &errBuf}, ce)
		assert.Equal(t, "error (NOT_FOUND): user not found: x\n"+
			"  candidates (10 of 37): "+shown(10)+"\n", errBuf.String())
	})

	t.Run("text without a total", func(t *testing.T) {
		t.Parallel()
		ce := clierr.New(clierr.CodeNotFound, "user not found: x").WithCandidatesCut(full[:10], 10)
		var errBuf bytes.Buffer
		_ = renderErr(IO{Err: &errBuf}, ce)
		assert.Equal(t, "error (NOT_FOUND): user not found: x\n"+
			"  candidates: "+shown(10)+"\n", errBuf.String())
	})

	for _, tc := range []struct {
		name      string
		full      []clierr.Candidate
		wantTotal bool
	}{
		{name: "envelope of a cut list", full: full, wantTotal: true},
		{name: "envelope of a list at the limit", full: full[:10], wantTotal: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ce := clierr.New(clierr.CodeNotFound, "user not found: x").WithCandidatesCut(tc.full, 10)
			var errBuf bytes.Buffer
			_ = renderErr(IO{Err: &errBuf, Format: "json"}, ce)
			var env struct {
				Data struct {
					Error struct {
						Candidates      []clierr.Candidate `json:"candidates"`
						CandidatesTotal *int               `json:"candidatesTotal"`
					} `json:"error"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(errBuf.Bytes(), &env))
			assert.Len(t, env.Data.Error.Candidates, 10)
			if tc.wantTotal {
				require.NotNil(t, env.Data.Error.CandidatesTotal)
				assert.Equal(t, 37, *env.Data.Error.CandidatesTotal)
			} else {
				assert.Nil(t, env.Data.Error.CandidatesTotal)
			}
		})
	}
}

// TestRenderErrFormatAware asserts renderErr's format switch: default, table,
// wide, name, and template all render the short operator-facing text block
// to stderr; json and yaml render the structured envelope, each in its own
// encoding (yaml as YAML, not JSON dressed up as one).
func TestRenderErrFormatAware(t *testing.T) {
	t.Parallel()
	ce := clierr.New(clierr.CodeNotFound, `cluster "web-prd" not found`).
		WithHint("run: rancher get cluster").
		WithCandidates([]clierr.Candidate{{Name: "web-prod", ID: "c-1"}, {Name: "web-prd2", ID: "c-2"}})
	wantText := "error (NOT_FOUND): cluster \"web-prd\" not found\n" +
		"  hint: run: rancher get cluster\n" +
		"  candidates: web-prod (c-1), web-prd2 (c-2)\n"

	for _, format := range []string{"", "table", "wide", "name", "template={{.error.code}}"} {
		t.Run("text format="+format, func(t *testing.T) {
			t.Parallel()
			var errBuf bytes.Buffer
			_ = renderErr(IO{Err: &errBuf, Format: format}, ce)
			assert.Equal(t, wantText, errBuf.String())
		})
	}

	for _, format := range []string{"json", "yaml"} {
		t.Run("envelope format="+format, func(t *testing.T) {
			t.Parallel()
			var errBuf bytes.Buffer
			_ = renderErr(IO{Err: &errBuf, Format: format}, ce)
			var env map[string]any
			if format == "json" {
				require.NoError(t, json.Unmarshal(errBuf.Bytes(), &env))
			} else {
				require.NoError(t, yaml.Unmarshal(errBuf.Bytes(), &env))
			}
			assert.Equal(t, "Error", env["kind"])
			data, ok := env["data"].(map[string]any)
			require.True(t, ok)
			errObj, ok := data["error"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeNotFound, errObj["code"])
			assert.Equal(t, "run: rancher get cluster", errObj["hint"])
		})
	}
}

// TestRenderErrRetryableHeader asserts the header's retryable marker: a
// retryable error's header carries ", retryable" after the code; a
// non-retryable error's does not.
func TestRenderErrRetryableHeader(t *testing.T) {
	t.Parallel()
	var errBuf bytes.Buffer
	_ = renderErr(IO{Err: &errBuf, Format: ""}, clierr.Retryable(clierr.CodeLockTimeout, "locked"))
	assert.Equal(t, "error (LOCK_TIMEOUT, retryable): locked\n", errBuf.String())

	errBuf.Reset()
	_ = renderErr(IO{Err: &errBuf, Format: ""}, clierr.New(clierr.CodeLockTimeout, "locked"))
	assert.Equal(t, "error (LOCK_TIMEOUT): locked\n", errBuf.String())
}

// stubTextDetails is a minimal clierr.TextDetails implementation for
// exercising the details block in isolation from any real producer's shape.
type stubTextDetails struct{ lines []string }

func (s stubTextDetails) TextDetails() []string { return s.lines }

// TestRenderErrDetailsBlock asserts that a details payload implementing
// clierr.TextDetails renders its projected lines after hint/candidates, and
// that no details payload at all adds no lines.
func TestRenderErrDetailsBlock(t *testing.T) {
	t.Parallel()
	ce := clierr.New(clierr.CodeConflict, "boom").
		WithHint("retry").
		WithDetails(stubTextDetails{lines: []string{"committed: Widget w1 - the change may already be on the server", "recover: get widget w1 -o json"}})
	var errBuf bytes.Buffer
	_ = renderErr(IO{Err: &errBuf, Format: ""}, ce)
	assert.Equal(t, "error (CONFLICT): boom\n"+
		"  hint: retry\n"+
		"  committed: Widget w1 - the change may already be on the server\n"+
		"  recover: get widget w1 -o json\n", errBuf.String())

	errBuf.Reset()
	_ = renderErr(IO{Err: &errBuf, Format: ""}, clierr.New(clierr.CodeConflict, "boom"))
	assert.Equal(t, "error (CONFLICT): boom\n", errBuf.String())
}

// TestRenderErrFallsBackForUnprojectedDetails asserts the defense against an internal error:
// a Details value that reached the error through some path other than
// WithDetails (an Error literal setting the field directly, bypassing the
// typed setter) renders the neutral fallback line instead of nothing, or a
// panic, or leaking an unprojected internal shape onto stderr.
func TestRenderErrFallsBackForUnprojectedDetails(t *testing.T) {
	t.Parallel()
	ce := &clierr.Error{Code: clierr.CodeGeneric, Message: "boom", Details: "raw value bypassing WithDetails"}
	var errBuf bytes.Buffer
	_ = renderErr(IO{Err: &errBuf, Format: ""}, ce)
	assert.Equal(t, "the error has more details - re-run with -o json to see them", detailsFallbackLine)
	assert.Equal(t, "error (GENERIC): boom\n  "+detailsFallbackLine+"\n", errBuf.String())
}

// TestRenderErrWarningsDefaultFormat asserts the default-format merge: the
// error's warning ride prints as "# warning:" lines ahead of the error text,
// in the order they were added, never inside the error block itself.
func TestRenderErrWarningsDefaultFormat(t *testing.T) {
	t.Parallel()
	ce := clierr.New(clierr.CodeGeneric, "boom").WithWarnings("first", "second")
	var errBuf bytes.Buffer
	_ = renderErr(IO{Err: &errBuf, Format: ""}, ce)
	assert.Equal(t, "# warning: first\n# warning: second\nerror (GENERIC): boom\n", errBuf.String())
}

// TestRenderErrWarningsEnvelope asserts the json/yaml merge: the warning ride
// lands at the envelope's top-level "warnings" field, never under
// data.error, so a scripted caller sees it exactly where a success result's
// Warnings would be.
func TestRenderErrWarningsEnvelope(t *testing.T) {
	t.Parallel()
	ce := clierr.New(clierr.CodeGeneric, "boom").WithWarnings("first", "second")
	var errBuf bytes.Buffer
	_ = renderErr(IO{Err: &errBuf, Format: "json"}, ce)
	var env struct {
		Warnings []string `json:"warnings"`
		Data     struct {
			Error map[string]any `json:"error"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(errBuf.Bytes(), &env))
	assert.Equal(t, []string{"first", "second"}, env.Warnings)
	assert.NotContains(t, env.Data.Error, "warnings")
}

// TestRenderErrNoWarningsAddsNoLines asserts the zero-warnings case: no
// warning ride means no "# warning:" lines and no envelope "warnings" key.
func TestRenderErrNoWarningsAddsNoLines(t *testing.T) {
	t.Parallel()
	ce := clierr.New(clierr.CodeGeneric, "boom")

	var errBuf bytes.Buffer
	_ = renderErr(IO{Err: &errBuf, Format: ""}, ce)
	assert.Equal(t, "error (GENERIC): boom\n", errBuf.String())

	errBuf.Reset()
	_ = renderErr(IO{Err: &errBuf, Format: "json"}, ce)
	env := decodeEnvelope(t, errBuf.Bytes())
	_, hasWarnings := env["warnings"]
	assert.False(t, hasWarnings)
}

// stubNoticeSource is a test NoticeSource: DrainNotices returns the queued
// notices once and empties thereafter, mirroring the real drain-once
// contract without any store behind it.
type stubNoticeSource struct{ notices []string }

func (s *stubNoticeSource) DrainNotices() []string {
	out := s.notices
	s.notices = nil
	return out
}

// TestRenderErrMergesNoticeSourceAheadOfWarnings asserts the merge order: a
// drained NoticeSource notice (recorded earliest) leads the error's own
// WithWarnings ride, each in its own record order - under both the default
// text form and the json/yaml envelope.
func TestRenderErrMergesNoticeSourceAheadOfWarnings(t *testing.T) {
	t.Parallel()
	ce := clierr.New(clierr.CodeGeneric, "boom").WithWarnings("error-carried-1", "error-carried-2")

	t.Run("default", func(t *testing.T) {
		t.Parallel()
		notices := &stubNoticeSource{notices: []string{"notice-1", "notice-2"}}
		var errBuf bytes.Buffer
		_ = renderErr(IO{Err: &errBuf, Format: "", Notices: notices}, ce)
		assert.Equal(t,
			"# warning: notice-1\n# warning: notice-2\n# warning: error-carried-1\n# warning: error-carried-2\nerror (GENERIC): boom\n",
			errBuf.String())
	})

	t.Run("json", func(t *testing.T) {
		t.Parallel()
		notices := &stubNoticeSource{notices: []string{"notice-1", "notice-2"}}
		var errBuf bytes.Buffer
		_ = renderErr(IO{Err: &errBuf, Format: "json", Notices: notices}, ce)
		var env struct {
			Warnings []string `json:"warnings"`
		}
		require.NoError(t, json.Unmarshal(errBuf.Bytes(), &env))
		assert.Equal(t, []string{"notice-1", "notice-2", "error-carried-1", "error-carried-2"}, env.Warnings)
	})
}

// TestRenderErrDrainsNoticeSourceWithNoErrorWarnings asserts that a
// NoticeSource notice reaches the error path on its own, with no
// error-carried WithWarnings involved, at the renderErr seam.
func TestRenderErrDrainsNoticeSourceWithNoErrorWarnings(t *testing.T) {
	t.Parallel()
	notices := &stubNoticeSource{notices: []string{"keychain unavailable; credentials will be stored in plaintext"}}
	ce := clierr.New(clierr.CodeAuth, "not logged in")
	var errBuf bytes.Buffer
	_ = renderErr(IO{Err: &errBuf, Format: "json", Notices: notices}, ce)
	env := decodeEnvelope(t, errBuf.Bytes())
	warnings, ok := env["warnings"].([]any)
	require.True(t, ok)
	assert.Equal(t, []any{"keychain unavailable; credentials will be stored in plaintext"}, warnings)
	assert.Empty(t, notices.notices, "drain must be destructive - nothing left to drain a second time")
}

// TestRunOrdinaryCommandDrainsNoticeSourceOnSuccess asserts the success-path
// drain for a plain (non-once-only) command: a notice recorded on the
// invocation's NoticeSource leads Result.Warnings - under the default
// format it mirrors to stderr ahead of any Result.Warnings entry, and under
// -o json it lands inside the same envelope's warnings field, in that
// order. Draining is destructive.
func TestRunOrdinaryCommandDrainsNoticeSourceOnSuccess(t *testing.T) {
	t.Parallel()
	t.Run("default", func(t *testing.T) {
		t.Parallel()
		notices := &stubNoticeSource{notices: []string{"notice-1"}}
		cmd := fakeRead{
			d:   Descriptor{Name: "get thing", CommandType: ReadOnly, Shape: ShapeText},
			res: Result{Kind: "Thing", Text: "thing rendered\n", Warnings: []string{"result-warning"}},
		}
		var out, errBuf bytes.Buffer
		_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "", Notices: notices}, safety.Binding{})
		require.Nil(t, ce)
		assert.Equal(t, "# warning: notice-1\n# warning: result-warning\n", errBuf.String())
		assert.Empty(t, notices.notices)
	})
	t.Run("json", func(t *testing.T) {
		t.Parallel()
		notices := &stubNoticeSource{notices: []string{"notice-1"}}
		cmd := fakeRead{
			d:   Descriptor{Name: "get thing", CommandType: ReadOnly, Shape: ShapeTabular},
			res: Result{Kind: "Thing", Data: fooTabularData{Foo: "bar"}, Warnings: []string{"result-warning"}},
		}
		var out, errBuf bytes.Buffer
		_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json", Notices: notices}, safety.Binding{})
		require.Nil(t, ce)
		assert.Empty(t, errBuf.String())
		env := decodeEnvelope(t, out.Bytes())
		warnings, ok := env["warnings"].([]any)
		require.True(t, ok)
		assert.Equal(t, []any{"notice-1", "result-warning"}, warnings)
		assert.Empty(t, notices.notices)
	})
}

// TestRunRawBytesAndExecJSONWarningsMirrorUnderEveryFormat asserts that
// warnings on the raw-output shapes reach stderr under every format:
// ShapeBytes (RawBytes) and ShapeExecJSON (Raw) results have no envelope to
// carry Warnings, so both branches mirror them to stderr unconditionally
// rather than returning before the mirror loop.
func TestRunRawBytesAndExecJSONWarningsMirrorUnderEveryFormat(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"", "table", "json"} {
		t.Run("RawBytes format="+format, func(t *testing.T) {
			t.Parallel()
			cmd := fakeRead{
				d:   Descriptor{Name: "kubeconfig", CommandType: Local, Shape: ShapeBytes},
				res: Result{RawBytes: []byte("apiVersion: v1\n"), Warnings: []string{"server URL is not https"}},
			}
			var out, errBuf bytes.Buffer
			_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: format}, safety.Binding{})
			require.Nil(t, ce)
			assert.Equal(t, "apiVersion: v1\n", out.String(), "RawBytes stays verbatim regardless of -o")
			assert.Contains(t, errBuf.String(), "# warning: server URL is not https\n")
		})
		t.Run("Raw format="+format, func(t *testing.T) {
			t.Parallel()
			cmd := fakeRead{
				d:   Descriptor{Name: "auth get-token", CommandType: Local, Shape: ShapeExecJSON},
				res: Result{Raw: true, Kind: "ExecCredential", Data: map[string]string{"token": "t"}, Warnings: []string{"cache read failed: boom"}},
			}
			var out, errBuf bytes.Buffer
			_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: format}, safety.Binding{})
			require.Nil(t, ce)
			assert.NotContains(t, out.String(), "warning", "the bare ExecCredential JSON never carries a warnings key")
			assert.Contains(t, errBuf.String(), "# warning: cache read failed: boom\n")
		})
	}
}

// TestValidateInputMissingRequiredFlagUsesDeclaredMessage asserts that a
// required flag which declares MissingMessage and MissingHint is refused
// with exactly those, hint included, while a flag without them keeps the
// generic "--<name> is required" refusal and no hint.
func TestValidateInputMissingRequiredFlagUsesDeclaredMessage(t *testing.T) {
	t.Parallel()
	d := Descriptor{Name: "x", Flags: []ArgSpec{
		{Name: "cluster", Type: "string", Required: true, MissingMessage: "--cluster is required; pass --cluster local", MissingHint: "rancher get cluster"},
	}}
	ce := validateInput(d, mapInput{}, false)
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "--cluster is required; pass --cluster local", ce.Message)
	assert.Equal(t, "rancher get cluster", ce.Hint)

	plain := Descriptor{Name: "x", Flags: []ArgSpec{{Name: "mode", Type: "string", Required: true}}}
	ce = validateInput(plain, mapInput{}, false)
	require.NotNil(t, ce)
	assert.Equal(t, "--mode is required", ce.Message)
	assert.Empty(t, ce.Hint)
}

// TestValidateInputMissingInputListsAllowedValues asserts that a missing
// required argument or flag lists its allowed values when it declares them,
// that one without values keeps the short text, that a declared
// MissingMessage wins over the list, and that the invalid-value texts keep
// their form.
func TestValidateInputMissingInputListsAllowedValues(t *testing.T) {
	t.Parallel()
	d := Descriptor{Name: "x",
		Args: []ArgSpec{
			{Name: "kind", Type: "string", Required: true, Enum: []string{"a", "b"}},
			{Name: "name", Type: "string", Required: true},
		},
		Flags: []ArgSpec{
			{Name: "mode", Type: "string", Required: true, Enum: []string{"c", "d"}},
			{Name: "tags", Type: "stringSlice", Required: true, Enum: []string{"e", "f"}},
			{Name: "free", Type: "string", Required: true},
			{Name: "declared", Type: "string", Required: true, Enum: []string{"g", "h"}, MissingMessage: "--declared is required; see the docs"},
		},
	}
	assert.Equal(t, "a|b", OneOf([]string{"a", "b"}))

	full := mapInput{
		a:  []string{"a", "n"},
		s:  map[string]string{"mode": "c", "free": "v", "declared": "g"},
		sl: map[string][]string{"tags": {"e"}},
	}
	require.Nil(t, validateInput(d, full, false))

	for _, tc := range []struct {
		name string
		in   func(mapInput) mapInput
		want string
	}{
		{"argument with values", func(in mapInput) mapInput { in.a = []string{"", "n"}; return in }, "argument <kind> is required; one of a|b"},
		{"argument without values", func(in mapInput) mapInput { in.a = []string{"a"}; return in }, "argument <name> is required"},
		{"flag with values", func(in mapInput) mapInput { in.s = map[string]string{"free": "v", "declared": "g"}; return in }, "--mode is required; one of c|d"},
		{"list flag with values", func(in mapInput) mapInput { in.sl = nil; return in }, "--tags is required; one of e|f"},
		{"flag without values", func(in mapInput) mapInput { in.s = map[string]string{"mode": "c", "declared": "g"}; return in }, "--free is required"},
		{"declared message", func(in mapInput) mapInput { in.s = map[string]string{"mode": "c", "free": "v"}; return in }, "--declared is required; see the docs"},
		{"invalid argument", func(in mapInput) mapInput { in.a = []string{"z", "n"}; return in }, `argument <kind>="z" is not one of a|b`},
		{"invalid flag", func(in mapInput) mapInput {
			in.s = map[string]string{"mode": "z", "free": "v", "declared": "g"}
			return in
		}, `--mode="z" is not one of c|d`},
		{"invalid list entry", func(in mapInput) mapInput { in.sl = map[string][]string{"tags": {"e", "z"}}; return in }, `--tags="z" is not one of e|f`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ce := validateInput(d, tc.in(full), false)
			require.NotNil(t, ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, tc.want, ce.Message)
		})
	}
}

// TestValidateInputStringSlicePerEntry asserts validateInput's
// semantics: Required means at least one occurrence; Enum membership is
// checked on every entry.
func TestValidateInputStringSlicePerEntry(t *testing.T) {
	t.Parallel()
	d := Descriptor{
		Name: "x",
		Flags: []ArgSpec{
			{Name: "mode", Type: "stringSlice", Required: true, Enum: []string{"a", "b"}},
		},
	}

	t.Run("required with zero occurrences fails", func(t *testing.T) {
		t.Parallel()
		ce := validateInput(d, mapInput{}, false)
		require.NotNil(t, ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Contains(t, ce.Message, "--mode is required")
	})

	t.Run("enum checked per entry", func(t *testing.T) {
		t.Parallel()
		ce := validateInput(d, mapInput{sl: map[string][]string{"mode": {"a", "c"}}}, false)
		require.NotNil(t, ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Contains(t, ce.Message, `"c"`)
	})

	t.Run("all entries valid passes", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, validateInput(d, mapInput{sl: map[string][]string{"mode": {"a", "b"}}}, false))
	})
}

// TestResultSummaryAppearsInJSONEnvelope proves that a non-empty Result.Summary
// lands in the envelope's top-level "summary" key under -o json.
func TestResultSummaryAppearsInJSONEnvelope(t *testing.T) {
	t.Parallel()
	summary := "wait: condition=Ready satisfied after 53s"
	cmd := fakeRead{
		d:   Descriptor{Name: "get cluster", CommandType: ReadOnly},
		res: Result{Kind: "Cluster", Data: widgetTable{Name: "c-abc"}, Summary: summary},
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.Nil(t, ce)
	env := decodeEnvelope(t, out.Bytes())
	assert.Equal(t, summary, env["summary"])
}

// TestResultSummaryRequiresTabularData asserts the runtime rule that Summary
// rides only over Tabular Data - the JSON-fallback-era pattern of a Summary
// line papering over non-Tabular Data is an internal error now that the
// fallback itself is gone.
func TestResultSummaryRequiresTabularData(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{
		d:   Descriptor{Name: "get cluster", CommandType: ReadOnly},
		res: Result{Kind: "Cluster", Data: map[string]string{"name": "c-abc"}, Summary: "wait: satisfied"},
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Contains(t, ce.Message, "Summary requires Tabular Data")
}

// TestResultSummaryPrintsUnderWideToo asserts that the Summary annotation line
// prints over the table under both table/default AND wide, not table only.
func TestResultSummaryPrintsUnderWideToo(t *testing.T) {
	t.Parallel()
	summary := "wait: condition=Ready satisfied after 53s"
	newCmd := func() fakeRead {
		return fakeRead{
			d:   Descriptor{Name: "get cluster", CommandType: ReadOnly},
			res: Result{Kind: "Cluster", Data: nodeMetricsRows{{"name": "node-1"}}, Summary: summary},
		}
	}
	for _, format := range []string{"table", "wide"} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			_, ce := Run(t.Context(), newCmd(), mapInput{}, IO{Out: &out, Err: &errBuf, Format: format}, safety.Binding{})
			require.Nil(t, ce)
			assert.Contains(t, out.String(), summary)
		})
	}
}

// TestResultEmptySummaryAbsentFromJSONEnvelope proves that an empty
// Result.Summary emits NO "summary" key - the envelope must be byte-identical
// to a non-wait result (additive, not always-present).
func TestResultEmptySummaryAbsentFromJSONEnvelope(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{
		d:   Descriptor{Name: "get cluster", CommandType: ReadOnly},
		res: Result{Kind: "Cluster", Data: map[string]string{"name": "c-abc"}},
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.Nil(t, ce)
	env := decodeEnvelope(t, out.Bytes())
	_, hasSummary := env["summary"]
	assert.False(t, hasSummary, "empty Summary must emit no summary key in the envelope")
}

// TestResultSummaryPrintsBeforeTableInDefaultMode proves that a non-empty
// Result.Summary is written to stdout as its own line ahead of the rendered
// table under format ""/table.
func TestResultSummaryPrintsBeforeTableInDefaultMode(t *testing.T) {
	t.Parallel()
	summary := "wait: condition=Ready satisfied after 53s"
	cmd := fakeRead{
		d: Descriptor{Name: "get cluster", CommandType: ReadOnly},
		res: Result{
			Kind:    "Cluster",
			Data:    nodeMetricsRows{{"name": "node-1"}},
			Summary: summary,
		},
	}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: ""}, safety.Binding{})
	require.Nil(t, ce)
	outStr := out.String()
	assert.Contains(t, outStr, summary)
	summaryPos := strings.Index(outStr, summary)
	namePos := strings.Index(outStr, "NAME")
	assert.Less(t, summaryPos, namePos, "summary must appear before the table")
}

// TestCancelledContextRendersInterrupted asserts that a command ending with
// an error after its context was cancelled is reported as INTERRUPTED (exit
// 130) with the command's error as the cause: a bare cancellation says
// "interrupted" alone, any other surfaced error keeps its message and
// details after it. A live context and an expired deadline both leave the
// error's own classification alone.
func TestCancelledContextRendersInterrupted(t *testing.T) {
	t.Parallel()
	binding := safety.Binding{Contract: safety.ContractPublic, Stability: safety.Stable}
	run := func(t *testing.T, ctx context.Context, err error) (*clierr.Error, map[string]any) {
		t.Helper()
		cmd := fakeRead{d: Descriptor{Name: "get widgets", CommandType: ReadOnly}, err: err}
		var out, errBuf bytes.Buffer
		_, ce := Run(ctx, cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "json"}, binding)
		require.NotNil(t, ce)
		env := decodeEnvelope(t, errBuf.Bytes())
		data, ok := env["data"].(map[string]any)
		require.True(t, ok, "envelope data: %v", env)
		errObj, ok := data["error"].(map[string]any)
		require.True(t, ok, "envelope error: %v", data)
		if w, ok := env["warnings"]; ok {
			errObj["envelopeWarnings"] = w
		}
		return ce, errObj
	}
	cancelled := func(t *testing.T) context.Context {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		return ctx
	}
	cause := clierr.Retryable(clierr.CodeGeneric, "cannot reach server: connection refused").
		WithDetails(clierr.PrimeRequiredDetails{Feature: "f", Server: "s"}).
		WithWarnings("the stamp was written before the failure")

	t.Run("cancelled context with a surfaced error keeps its message, details and warnings", func(t *testing.T) {
		t.Parallel()
		ce, errObj := run(t, cancelled(t), cause)
		assert.Equal(t, clierr.CodeInterrupted, ce.Code)
		assert.Equal(t, 130, ce.ExitCode())
		assert.False(t, ce.Retryable)
		require.ErrorIs(t, ce, cause)
		assert.Equal(t, "INTERRUPTED", errObj["code"])
		assert.Equal(t, "interrupted: cannot reach server: connection refused", errObj["message"])
		assert.Equal(t, false, errObj["retryable"])
		assert.Equal(t, map[string]any{"feature": "f", "server": "s"}, errObj["details"])
		assert.Equal(t, []string{"the stamp was written before the failure"}, ce.Warnings())
		assert.Equal(t, []any{"the stamp was written before the failure"}, errObj["envelopeWarnings"])
	})
	t.Run("bare cancellation says interrupted alone", func(t *testing.T) {
		t.Parallel()
		ce, errObj := run(t, cancelled(t), fmt.Errorf("waiting: %w", context.Canceled))
		assert.Equal(t, clierr.CodeInterrupted, ce.Code)
		assert.Equal(t, "interrupted", errObj["message"])
		_, hasDetails := errObj["details"]
		assert.False(t, hasDetails)
	})
	t.Run("live context leaves the classification alone", func(t *testing.T) {
		t.Parallel()
		ce, _ := run(t, t.Context(), cause)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.True(t, ce.Retryable)
	})
	t.Run("expired deadline is not an interrupt", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer cancel()
		require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
		ce, _ := run(t, ctx, cause)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.True(t, ce.Retryable)
	})
}

// TestValidateFormatLeavesTheValueOutForASecretInput asserts that the
// refusal of an unknown -o value, and of a template that does not parse,
// shows the value and the parser's message for an ordinary command, and
// neither for a command that takes a secret input, whose -o can have taken
// the word typed after it, a secret among them.
func TestValidateFormatLeavesTheValueOutForASecretInput(t *testing.T) {
	t.Parallel()
	const oneOf = "table|wide|json|yaml|name|template=<go-template>"
	for _, tc := range []struct {
		name, format, ordinary, secret string
	}{
		{"an unknown value", "s3cretcanary9", `--output="s3cretcanary9" is not one of ` + oneOf, "--output value is not one of " + oneOf},
		{"a flag typed as the value", "--from-literal=k=s3cretcanary9", `--output="--from-literal=k=s3cretcanary9" is not one of ` + oneOf, "--output value is not one of " + oneOf},
		{"a bare template", "template", `--output="template" is not one of ` + oneOf, "--output value is not one of " + oneOf},
		{"a template that does not parse", "template={{s3cretcanary9", "", "--output template does not parse; must be valid Go text/template syntax"},
		{"a template with no source", "template=", "--output template is empty; pass -o template='{{ .field }}'", "--output template is empty; pass -o template='{{ .field }}'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ordinary := validateFormat(tc.format, false)
			require.NotNil(t, ordinary)
			if tc.ordinary != "" {
				assert.Equal(t, tc.ordinary, ordinary.Message)
			} else {
				assert.Contains(t, ordinary.Message, "--output template does not parse: ")
				assert.Contains(t, ordinary.Message, "s3cretcanary9", "an ordinary command shows the parser's message")
			}
			secret := validateFormat(tc.format, true)
			require.NotNil(t, secret)
			assert.Equal(t, clierr.CodeValidation, secret.Code)
			assert.Equal(t, tc.secret, secret.Message)
			assert.NotContains(t, secret.Message, "s3cretcanary9")
		})
	}
	assert.Nil(t, validateFormat("json", true))
	assert.Nil(t, validateFormat("template={{.x}}", true))
	assert.Nil(t, validateFormat("", true))
}

// TestValidateColorLeavesTheValueOutForASecretInput asserts that the refusal
// of an unknown --color value shows the value for an ordinary command and
// leaves it out for a command that takes a secret input.
func TestValidateColorLeavesTheValueOutForASecretInput(t *testing.T) {
	t.Parallel()
	ordinary := validateColor("s3cretcanary9", false)
	require.NotNil(t, ordinary)
	assert.Equal(t, `--color="s3cretcanary9" is not one of auto|always|never`, ordinary.Message)
	secret := validateColor("s3cretcanary9", true)
	require.NotNil(t, secret)
	assert.Equal(t, clierr.CodeValidation, secret.Code)
	assert.Equal(t, "--color value is not one of auto|always|never", secret.Message)
	assert.Nil(t, validateColor("never", true))
	assert.Nil(t, validateColor("", true))
}

// TestValidateOutputPolicyLeavesTheTemplateSourceOutForASecretInput asserts
// that the refusal of a template format by a command that restricts formats
// shows the source for an ordinary command and "template" alone for a command
// that takes a secret input, and that a plain format literal is shown by both.
func TestValidateOutputPolicyLeavesTheTemplateSourceOutForASecretInput(t *testing.T) {
	t.Parallel()
	d := Descriptor{Name: "x", OutputPolicy: OutputPolicy{AllowedFormats: []string{"json"}}}
	secret := d
	secret.SecretInput = true

	ce := validateOutputPolicy(d, "template={{s3cretcanary9}}")
	require.NotNil(t, ce)
	assert.Equal(t, "x does not support --output=template={{s3cretcanary9}}; allowed: json", ce.Message)
	ce = validateOutputPolicy(secret, "template={{s3cretcanary9}}")
	require.NotNil(t, ce)
	assert.Equal(t, "x does not support --output=template; allowed: json", ce.Message)
	for _, d := range []Descriptor{d, secret} {
		ce = validateOutputPolicy(d, "name")
		require.NotNil(t, ce)
		assert.Equal(t, "x does not support --output=name; allowed: json", ce.Message)
	}
}

// TestValidateInputEnumRefusalsLeaveTheValueOutForASecretInput asserts, at
// each of the three places that check an enum (a positional argument, a
// string flag and a string slice flag), that the refusal shows the value for
// an ordinary command and names only the argument or flag and the allowed
// values for a command that takes a secret input.
func TestValidateInputEnumRefusalsLeaveTheValueOutForASecretInput(t *testing.T) {
	t.Parallel()
	const word = "--from-literal=k=s3cretcanary9"
	for _, tc := range []struct {
		name             string
		d                Descriptor
		in               Input
		ordinary, secret string
	}{
		{
			name:     "a positional argument",
			d:        Descriptor{Name: "x", Args: []ArgSpec{{Name: "kind", Type: "string", Enum: []string{"a", "b"}}}},
			in:       mapInput{a: []string{word}},
			ordinary: `argument <kind>="` + word + `" is not one of a|b`,
			secret:   "argument <kind> value is not one of a|b",
		},
		{
			name:     "a string flag",
			d:        Descriptor{Name: "x", Flags: []ArgSpec{{Name: "type", Type: "string", Enum: []string{"json", "merge"}}}},
			in:       mapInput{s: map[string]string{"type": word}},
			ordinary: `--type="` + word + `" is not one of json|merge`,
			secret:   "--type value is not one of json|merge",
		},
		{
			name:     "a string slice flag",
			d:        Descriptor{Name: "x", Flags: []ArgSpec{{Name: "mode", Type: "stringSlice", Enum: []string{"a", "b"}}}},
			in:       mapInput{sl: map[string][]string{"mode": {"a", word}}},
			ordinary: `--mode="` + word + `" is not one of a|b`,
			secret:   "--mode value is not one of a|b",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ordinary := validateInput(tc.d, tc.in, false)
			require.NotNil(t, ordinary)
			assert.Equal(t, tc.ordinary, ordinary.Message)
			d := tc.d
			d.SecretInput = true
			secret := validateInput(d, tc.in, false)
			require.NotNil(t, secret)
			assert.Equal(t, clierr.CodeValidation, secret.Code)
			assert.Equal(t, tc.secret, secret.Message)
			assert.NotContains(t, secret.Message, "s3cretcanary9")
		})
	}
}

// TestInternalErrorMessagesStartWithThePrefix asserts the whole message of
// every defect the pipeline reports in a command or in the CLI's own setup:
// the GENERIC code and a text that starts with "internal error: ". A new
// message of that kind is covered once its row is added; the source scan in
// test/conformance holds the prefix for a message nobody added.
func TestInternalErrorMessagesStartWithThePrefix(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, cmd Command, streams IO) *clierr.Error {
		t.Helper()
		var out, errBuf bytes.Buffer
		streams.Out, streams.Err, streams.Format = &out, &errBuf, "json"
		_, ce := Run(t.Context(), cmd, mapInput{}, streams, safety.Binding{})
		require.NotNil(t, ce)
		return ce
	}
	capability := func(req CapabilityRequirement) func(t *testing.T) *clierr.Error {
		return func(t *testing.T) *clierr.Error {
			t.Helper()
			ce := ValidCapabilityShape(req, "get widget")
			require.NotNil(t, ce)
			return ce
		}
	}
	shape := func(d Descriptor, res Result, format string) func(t *testing.T) *clierr.Error {
		return func(t *testing.T) *clierr.Error {
			t.Helper()
			ce := checkResultShape(d, res, format)
			require.NotNil(t, ce)
			return ce
		}
	}
	plan := func(eff safety.Effect, exec ExecuteFunc) func(t *testing.T) *clierr.Error {
		return func(t *testing.T) *clierr.Error {
			t.Helper()
			return run(t, fakeMutating{d: mutatingDesc(), eff: eff, exec: exec}, IO{Apply: true, Yes: true})
		}
	}
	widget := safety.Effect{Action: safety.ActionCreate, Target: "x", DisplayKind: "widget", DisplayName: "x"}
	styled := func(style.Style) string { return "x\n" }

	for _, tc := range []struct {
		name string
		got  func(t *testing.T) *clierr.Error
		want string
	}{
		{"a command that is neither a read nor a mutating command", func(t *testing.T) *clierr.Error {
			t.Helper()
			return run(t, describeOnly{d: Descriptor{Name: "broken", CommandType: ReadOnly}}, IO{})
		}, "internal error: command implements neither ReadCommand nor MutatingCommand: broken"},
		{"a plan with no executor", plan(widget, nil),
			"internal error: the command returned nothing to run to apply the change"},
		{"a plan with an invalid action", plan(safety.Effect{Action: safety.Action("destroy"), Target: "x", DisplayKind: "widget", DisplayName: "x"}, unusedExec(t)),
			`internal error: invalid action "destroy" in the dry-run preview`},
		{"a plan with no display kind", plan(safety.Effect{Action: safety.ActionCreate, Target: "x", DisplayName: "x"}, unusedExec(t)),
			"internal error: the dry-run preview has an empty DisplayKind or DisplayName"},
		{"a delete that is not destructive", plan(safety.Effect{Action: safety.ActionDelete, Target: "x", DisplayKind: "widget", DisplayName: "x"}, unusedExec(t)),
			"internal error: a delete in the dry-run preview must be destructive"},
		{"a capability requirement with no checker", func(t *testing.T) *clierr.Error {
			t.Helper()
			return run(t, fakeRead{d: primeDesc("get widget")}, IO{})
		}, "internal error: get widget declares a capability requirement but no capability checker is set up; register the command through the standard server-command path"},
		{"a capability requirement with no tier", capability(CapabilityRequirement{Feature: "f"}),
			"internal error: get widget: capability requirement has Feature/Unsupported set with no Tier"},
		{"an unknown capability tier", capability(CapabilityRequirement{Tier: "bogus", Feature: "f"}),
			`internal error: get widget: unknown capability tier "bogus"`},
		{"a capability tier with no feature", capability(CapabilityRequirement{Tier: CapabilityPrime}),
			`internal error: get widget: capability tier "prime" requires a Feature name`},
		{"a prime capability tier with Unsupported", capability(CapabilityRequirement{Tier: CapabilityPrime, Feature: "f", Unsupported: "u"}),
			`internal error: get widget: capability tier "prime" does not use Unsupported`},
		{"a discovery capability tier with no Unsupported", capability(CapabilityRequirement{Tier: CapabilityExtDiscovery, Feature: "f"}),
			`internal error: get widget: capability tier "extDiscovery" requires Unsupported`},
		{"a notice on a command that is not once-only", shape(Descriptor{Name: "get thing"}, Result{OnceOnlyNotice: "n"}, "json"),
			"internal error: get thing is not DeliveryOnceOnly but returned OnceOnlyNotice"},
		{"styled text with no text", shape(Descriptor{Name: "do thing", Shape: ShapeText}, Result{StyledText: styled}, "table"),
			"internal error: do thing returned StyledText but no Text"},
		{"wide text on a tabular command", shape(Descriptor{Name: "get thing", Shape: ShapeTabular}, Result{Text: "x\n", WideText: "y\n"}, "wide"),
			"internal error: get thing returned WideText without a ShapeText Text"},
		{"wide text and styled text", shape(Descriptor{Name: "access list", Shape: ShapeText}, Result{Text: "x\n", WideText: "y\n", StyledText: styled}, "wide"),
			"internal error: access list returned both WideText and StyledText"},
		{"a tabular command with other data", shape(Descriptor{Name: "get thing", CommandType: ReadOnly, Shape: ShapeTabular}, Result{Data: map[string]string{"a": "b"}}, "json"),
			"internal error: get thing declares ShapeTabular but its Data does not implement output.Tabular"},
		{"a text command with no text", shape(Descriptor{Name: "patch thing", CommandType: ReadOnly, Shape: ShapeText}, Result{}, ""),
			"internal error: patch thing declares ShapeText but returned no Text"},
		{"a bytes command with no bytes", shape(Descriptor{Name: "kubeconfig", CommandType: ReadOnly, Shape: ShapeBytes}, Result{}, "json"),
			"internal error: kubeconfig declares ShapeBytes but returned no RawBytes"},
		{"an exec JSON command with Raw unset", shape(Descriptor{Name: "auth get-token", CommandType: ReadOnly, Shape: ShapeExecJSON}, Result{}, "json"),
			"internal error: auth get-token declares ShapeExecJSON but Result.Raw is not set"},
		{"text and raw output", func(t *testing.T) *clierr.Error {
			t.Helper()
			return run(t, fakeRead{d: Descriptor{Name: "x", CommandType: ReadOnly}, res: Result{Text: "x", Raw: true}}, IO{})
		}, "internal error: only one of Text, Raw and RawBytes can be set, because each uses the whole stdout; Summary cannot be set with any of them"},
		{"a summary with data that is not a table", func(t *testing.T) *clierr.Error {
			t.Helper()
			cmd := fakeRead{d: Descriptor{Name: "get cluster", CommandType: ReadOnly}, res: Result{Kind: "Cluster", Data: map[string]string{"name": "c-abc"}, Summary: "wait: satisfied"}}
			return run(t, cmd, IO{})
		}, "internal error: Summary requires Tabular Data"},
		{"an exit status the command does not declare", func(t *testing.T) *clierr.Error {
			t.Helper()
			return run(t, exitStatusCmd{exitDeclCmd{name: "probe"}, 7}, IO{})
		}, "internal error: probe returned undeclared exit status 7"},
		{"a bool value refusal with no bool value", func(t *testing.T) *clierr.Error {
			t.Helper()
			return BoolValueError(Descriptor{Name: "get thing"}, []string{"x"}, nil, "rancher get thing x")
		}, "internal error: get thing was refused for a bool value but no bool value was found"},
		{"a once-only command with no committed context", func(t *testing.T) *clierr.Error {
			t.Helper()
			var out, errBuf bytes.Buffer
			_, ce := runOnceOnly(t, &out, &errBuf, Result{Kind: "WidgetCreated", Data: map[string]string{"name": "w1"}}, "json")
			require.NotNil(t, ce)
			return ce
		}, "internal error: create widget is DeliveryOnceOnly but returned no CommittedContext"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ce := tc.got(t)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.True(t, strings.HasPrefix(ce.Message, "internal error: "), "message %q", ce.Message)
			assert.Equal(t, tc.want, ce.Message)
		})
	}
}
