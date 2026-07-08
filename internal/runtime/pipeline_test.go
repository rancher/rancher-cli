package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mapInput struct {
	s map[string]string
	b map[string]bool
}

func (m mapInput) String(k string) string { return m.s[k] }
func (m mapInput) Bool(k string) bool     { return m.b[k] }
func (m mapInput) Args() []string         { return nil }

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
