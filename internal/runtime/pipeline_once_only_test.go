package runtime

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// partialWriteErr always fails, returning n bytes written before the error —
// n = 0 simulates a write that reports nothing accepted; n > 0 simulates a
// partial write, so a caller can tell whether a once-only value may already
// be on the wire.
type partialWriteErr struct{ n int }

func (w partialWriteErr) Write(p []byte) (int, error) {
	return min(w.n, len(p)), errors.New("disk full")
}

func onceOnlyDesc() Descriptor {
	return Descriptor{
		Name:        "create widget",
		CommandType: Mutating,
		Delivery:    DeliveryOnceOnly,
	}
}

func onceOnlyExec(res Result) ExecuteFunc {
	return func(context.Context) (Result, error) { return res, nil }
}

func runOnceOnly(t *testing.T, out, errW *bytes.Buffer, res Result, format string) (int, *clierr.Error) {
	t.Helper()
	cmd := fakeMutating{d: onceOnlyDesc(), eff: safety.Effect{Action: safety.ActionCreate}, exec: onceOnlyExec(res)}
	return Run(t.Context(), cmd, mapInput{}, IO{Out: out, Err: errW, Format: format, Apply: true},
		safety.Binding{})
}

// TestFinishResultRequiresCommittedContextForDeliveryOnceOnly pins the
// command-bug guard: a DeliveryOnceOnly executor that returns a Result with
// no CommittedContext must never render — the absence is a structured
// command-bug error, never a silent skip.
func TestFinishResultRequiresCommittedContextForDeliveryOnceOnly(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	_, ce := runOnceOnly(t, &out, &errBuf, Result{Kind: "WidgetCreated", Data: map[string]string{"name": "w1"}}, "json")
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Contains(t, ce.Message, "command bug")
	assert.Contains(t, ce.Message, "CommittedContext")
	assert.Empty(t, out.String(), "no bytes may reach stdout without committed context")
}

// TestFinishOnceOnlyResultBuffersAndRenders pins the success path: a
// DeliveryOnceOnly Result with CommittedContext set renders exactly like an
// ordinary result on a working writer.
func TestFinishOnceOnlyResultBuffersAndRenders(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	res := Result{
		Kind: "WidgetCreated",
		Data: map[string]string{"name": "w1", "secret": "shown-once"},
		CommittedContext: &CommittedContext{
			ResourceKind: "Widget", ResourceName: "w1", Recovery: "delete widget w1, then create again",
		},
	}
	status, ce := runOnceOnly(t, &out, &errBuf, res, "json")
	require.Nil(t, ce)
	assert.Zero(t, status)
	env := decodeEnvelope(t, out.Bytes())
	assert.Equal(t, "WidgetCreated", env["kind"])
	assert.Empty(t, errBuf.String())
}

// TestFinishOnceOnlyResultFailingStdoutWriter proves 3b's render-failure
// emission: when the single buffered write to stdout fails, the committed
// resource/recovery context is emitted as a structured error envelope to
// stderr — a working stderr channel exposes the full envelope — and
// partialOutput reflects whether the failing write reported n > 0.
func TestFinishOnceOnlyResultFailingStdoutWriter(t *testing.T) {
	t.Parallel()
	cc := &CommittedContext{ResourceKind: "Widget", ResourceName: "w1", SecretName: "widget-secret-1", Recovery: "get widget w1 -o json"}

	cases := []struct {
		name         string
		out          *partialWriteErr
		wantPartial  bool
		wantExitCode int
	}{
		{"zero bytes accepted", &partialWriteErr{n: 0}, false, 1},
		{"partial bytes accepted", &partialWriteErr{n: 5}, true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var errBuf bytes.Buffer
			res := Result{Kind: "WidgetCreated", Data: map[string]string{"name": "w1"}, CommittedContext: cc}
			cmd := fakeMutating{d: onceOnlyDesc(), eff: safety.Effect{Action: safety.ActionCreate}, exec: onceOnlyExec(res)}
			status, ce := Run(t.Context(), cmd, mapInput{},
				IO{Out: tc.out, Err: &errBuf, Format: "json", Apply: true}, safety.Binding{})
			require.NotNil(t, ce)
			assert.Zero(t, status)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.False(t, ce.Retryable, "a post-commit render failure must never look retryable")
			details, ok := ce.Details.(clierr.CommittedContextDetails)
			require.True(t, ok, "details must be clierr.CommittedContextDetails, got %T", ce.Details)
			assert.True(t, details.Committed)
			assert.Equal(t, "Widget", details.ResourceKind)
			assert.Equal(t, "w1", details.ResourceName)
			assert.Equal(t, "widget-secret-1", details.SecretName)
			assert.Equal(t, tc.wantPartial, details.PartialOutput)
			assert.Equal(t, "get widget w1 -o json", details.Recovery)

			env := decodeEnvelope(t, errBuf.Bytes())
			assert.Equal(t, "Error", env["kind"])
		})
	}
}

// TestFinishOnceOnlyResultFailingStdoutAndStderr proves the failure is
// best-effort: even when stderr ALSO fails to accept the emitted envelope,
// the pipeline still returns the correct classified error and exit code —
// no channel left, but no panic and no silent success either.
func TestFinishOnceOnlyResultFailingStdoutAndStderr(t *testing.T) {
	t.Parallel()
	cc := &CommittedContext{ResourceKind: "Widget", ResourceName: "w1", Recovery: "get widget w1 -o json"}
	res := Result{Kind: "WidgetCreated", Data: map[string]string{"name": "w1"}, CommittedContext: cc}
	cmd := fakeMutating{d: onceOnlyDesc(), eff: safety.Effect{Action: safety.ActionCreate}, exec: onceOnlyExec(res)}

	failOut := &partialWriteErr{n: 3}
	failErr := &partialWriteErr{n: 0}
	status, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: failOut, Err: failErr, Format: "json", Apply: true}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Zero(t, status)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Equal(t, 1, ce.ExitCode())
	details, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok)
	assert.True(t, details.PartialOutput)
}

// restrictiveOnceOnlyDesc mirrors the four structured once-only descriptors:
// DeliveryOnceOnly plus an OutputPolicy excluding "name".
func restrictiveOnceOnlyDesc() Descriptor {
	d := onceOnlyDesc()
	d.OutputPolicy = OutputPolicy{AllowedFormats: []string{"table", "wide", "json", "yaml", "template"}}
	return d
}

// refusingOnceOnlyMutating fails the test if Plan is ever invoked — proves
// the output-policy gate rejects BEFORE Plan (zero total requests), not
// merely before Execute.
type refusingOnceOnlyMutating struct {
	d Descriptor
	t *testing.T
}

func (r refusingOnceOnlyMutating) Describe() Descriptor { return r.d }
func (r refusingOnceOnlyMutating) Plan(context.Context, Input) (safety.Effect, ExecuteFunc, error) {
	r.t.Helper()
	r.t.Fatal("Plan must not be called when the output policy rejects the format")
	return safety.Effect{}, nil, nil
}

// TestRunOutputPolicyRejectsNameBeforePlan pins that a
// once-only command's OutputPolicy rejects "name" (and any format outside
// its allow-list) with VALIDATION before Plan runs at all — zero total
// requests, not merely zero mutation requests.
func TestRunOutputPolicyRejectsNameBeforePlan(t *testing.T) {
	t.Parallel()
	cmd := refusingOnceOnlyMutating{d: restrictiveOnceOnlyDesc(), t: t}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "name", Apply: true, Yes: true},
		safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "--output=name")
	assert.Empty(t, out.String())
}

// TestRunOutputPolicyAcceptsEveryAllowedFormat proves every declared format
// still carries the value through to render — the policy is a rejection
// list, not an accidental narrowing of the accepted set.
func TestRunOutputPolicyAcceptsEveryAllowedFormat(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"", "table", "wide", "json", "yaml", "template={{.Name}}"} {
		t.Run("format "+format, func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			res := Result{
				Kind: "WidgetCreated",
				Data: widgetTable{Name: "w1"},
				CommittedContext: &CommittedContext{
					ResourceKind: "Widget", ResourceName: "w1", Recovery: "get widget w1 -o json",
				},
			}
			cmd := fakeMutating{d: restrictiveOnceOnlyDesc(), eff: safety.Effect{Action: safety.ActionCreate}, exec: onceOnlyExec(res)}
			status, ce := Run(t.Context(), cmd, mapInput{},
				IO{Out: &out, Err: &errBuf, Format: format, Apply: true, Yes: true}, safety.Binding{})
			require.Nil(t, ce)
			assert.Zero(t, status)
			assert.Contains(t, out.String(), "w1")
		})
	}
}

// widgetTable is a minimal output.Tabular implementation for exercising
// table/wide/name-adjacent rendering paths.
type widgetTable struct{ Name string }

func (w widgetTable) Columns() []string { return []string{"NAME"} }
func (w widgetTable) Rows() [][]string  { return [][]string{{w.Name}} }
