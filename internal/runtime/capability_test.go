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

// stubChecker is a CapabilityChecker test double: it returns a fixed
// decision and counts calls so tests can assert both the outcome handling and
// "zero checks for ordinary commands."
type stubChecker struct {
	dec   CapabilityDecision
	calls int
	got   CapabilityRequirement
}

func (s *stubChecker) Check(_ context.Context, req CapabilityRequirement) CapabilityDecision {
	s.calls++
	s.got = req
	return s.dec
}

func primeDesc(name string) Descriptor {
	return Descriptor{Name: name, CommandType: ReadOnly,
		Capability: CapabilityRequirement{Tier: CapabilityPrime, Feature: "widget-feature"}}
}

// refusingExecuteRead fails the test if Execute is ever invoked - the read
// counterpart of refusingPlanMutating, proving the capability stage refuses
// BEFORE any resource access.
type refusingExecuteRead struct {
	d Descriptor
	t *testing.T
}

func (r refusingExecuteRead) Describe() Descriptor { return r.d }
func (r refusingExecuteRead) Execute(context.Context, Input, Prompter) (Result, error) {
	r.t.Helper()
	r.t.Fatal("Execute must not be called when the capability stage refuses")
	return Result{}, nil
}

func TestRunCapabilityProceedExecutesNormally(t *testing.T) {
	t.Parallel()
	called := 0
	cmd := fakeRead{d: primeDesc("get widget"),
		res: Result{Kind: "Widget"}}
	checker := &stubChecker{dec: CapabilityDecision{Outcome: CapabilityProceed}}
	var out bytes.Buffer
	status, ce := Run(t.Context(), fakeReadCounting{cmd, &called},
		mapInput{}, IO{Out: &out, Err: &out, Format: "json", Capability: checker}, safety.Binding{})
	require.Nil(t, ce)
	assert.Equal(t, 0, status)
	assert.Equal(t, 1, called, "Execute must run when the outcome is proceed")
	assert.Equal(t, 1, checker.calls)
	assert.Equal(t, "widget-feature", checker.got.Feature)
}

// fakeReadCounting wraps fakeRead to count Execute invocations without
// changing its Describe/Execute contract.
type fakeReadCounting struct {
	fakeRead
	calls *int
}

func (f fakeReadCounting) Execute(ctx context.Context, in Input, p Prompter) (Result, error) {
	*f.calls++
	return f.fakeRead.Execute(ctx, in, p)
}

func TestRunCapabilityUnknownExecutesNormally(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{d: primeDesc("get widget"), res: Result{Kind: "Widget"}}
	checker := &stubChecker{dec: CapabilityDecision{Outcome: CapabilityUnknown}}
	var out bytes.Buffer
	status, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &out, Format: "json", Capability: checker}, safety.Binding{})
	require.Nil(t, ce, "unknown must never block; the target request executes normally")
	assert.Equal(t, 0, status)
}

// TestRunCapabilityNilCheckerFailsLoud asserts the mis-wiring guard: a
// requirement-bearing command registered without a checker is a harness
// defect, not an unknown signal - it must error, never silently skip the
// mandatory stage.
func TestRunCapabilityNilCheckerFailsLoud(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{d: primeDesc("get widget"), res: Result{Kind: "Widget"}}
	var out bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &out, Format: "json"}, safety.Binding{})
	require.NotNil(t, ce, "a requirement-bearing command with no wired checker must fail loudly")
	assert.Contains(t, ce.Message, "no capability checker is set up")
}

func TestRunCapabilityDisabledOnPrimeRefusesWithEnableHint(t *testing.T) {
	t.Parallel()
	cmd := refusingExecuteRead{d: primeDesc("get widget"), t: t}
	checker := &stubChecker{dec: CapabilityDecision{Outcome: CapabilityDisabled, EditionPrime: true}}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", Capability: checker}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "widget-feature is disabled; enable it on the connected server")
	assert.Empty(t, ce.Hint)
}

// TestRunCapabilityDisabledOnNonPrimeRefusesWithNeutralHint asserts the
// amended outcome 2: an explicit disabled reading refuses on every edition,
// but a server not known to be Prime gets a message that holds regardless
// of tier rather than the Prime-specific "enable feature" instruction.
func TestRunCapabilityDisabledOnNonPrimeRefusesWithNeutralHint(t *testing.T) {
	t.Parallel()
	cmd := refusingExecuteRead{d: primeDesc("get widget"), t: t}
	checker := &stubChecker{dec: CapabilityDecision{Outcome: CapabilityDisabled, EditionPrime: false}}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", Capability: checker}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "widget-feature is disabled on the connected server, which decides whether it is available - "+clierr.LearnMorePrimeURL, ce.Message)
	assert.Empty(t, ce.Hint)
}

func TestRunCapabilityPrimeRequiredReturnsStructuredError(t *testing.T) {
	t.Parallel()
	cmd := refusingExecuteRead{d: primeDesc("get widget"), t: t}
	checker := &stubChecker{dec: CapabilityDecision{Outcome: CapabilityPrimeRequired, Server: "https://rancher.example"}}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", Capability: checker}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodePrimeRequired, ce.Code)
	assert.Equal(t, 9, ce.ExitCode())
	assert.Contains(t, ce.Message, clierr.LearnMorePrimeURL)
	assert.Empty(t, ce.Hint)
	details, ok := ce.Details.(clierr.PrimeRequiredDetails)
	require.True(t, ok, "Details must carry the structured PrimeRequiredDetails shape")
	assert.Equal(t, "widget-feature", details.Feature)
	assert.Equal(t, "https://rancher.example", details.Server)

	// The error envelope on stderr must carry the same code/exit machinery.
	var env map[string]any
	require.NoError(t, json.Unmarshal(errBuf.Bytes(), &env))
	data, ok := env["data"].(map[string]any)
	require.True(t, ok, "envelope must carry a data object")
	errField, ok := data["error"].(map[string]any)
	require.True(t, ok, "envelope data must carry an error object")
	assert.Equal(t, clierr.CodePrimeRequired, errField["code"])
}

// extDiscoveryDesc mirrors primeDesc for the ext.cattle.io-discovery tier.
func extDiscoveryDesc(name string) Descriptor {
	return Descriptor{Name: name, CommandType: ReadOnly,
		Capability: CapabilityRequirement{Tier: CapabilityExtDiscovery, Feature: "passwordchangerequests", Unsupported: "the user-password workflow"}}
}

// TestRunCapabilityUnsupportedReturnsStructuredError proves
// CapabilityExtDiscovery's negative outcome: a plain refusal naming the
// requirement's Unsupported description, CAPABILITY_UNSUPPORTED (not
// PRIME_REQUIRED), and the structured details an agent can decode.
func TestRunCapabilityUnsupportedReturnsStructuredError(t *testing.T) {
	t.Parallel()
	cmd := refusingExecuteRead{d: extDiscoveryDesc("get widget"), t: t}
	checker := &stubChecker{dec: CapabilityDecision{Outcome: CapabilityUnsupported, Server: "https://rancher.example"}}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", Capability: checker}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeCapabilityUnsupported, ce.Code)
	assert.Equal(t, 11, ce.ExitCode())
	assert.Equal(t, "the connected server does not support the user-password workflow; upgrade the server to a version that does", ce.Message)
	details, ok := ce.Details.(clierr.CapabilityUnsupportedDetails)
	require.True(t, ok, "Details must carry the structured CapabilityUnsupportedDetails shape")
	assert.Equal(t, "passwordchangerequests", details.Feature)
	assert.Equal(t, "https://rancher.example", details.Server)
}

func TestRunCapabilitySkippedForOrdinaryCommands(t *testing.T) {
	t.Parallel()
	cmd := fakeRead{d: Descriptor{Name: "get widget", CommandType: ReadOnly}, res: Result{Kind: "Widget"}}
	checker := &stubChecker{dec: CapabilityDecision{Outcome: CapabilityPrimeRequired}}
	var out bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &out, Format: "json", Capability: checker}, safety.Binding{})
	require.Nil(t, ce)
	assert.Equal(t, 0, checker.calls, "a command with no capability requirement must never call the checker")
}

func TestRunCapabilityCheckedBeforePlan(t *testing.T) {
	t.Parallel()
	d := Descriptor{Name: "delete widget", CommandType: Mutating,
		Capability: CapabilityRequirement{Tier: CapabilityPrime, Feature: "widget-feature"}}
	cmd := refusingPlanMutating{d: d, t: t}
	checker := &stubChecker{dec: CapabilityDecision{Outcome: CapabilityPrimeRequired}}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", Capability: checker}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodePrimeRequired, ce.Code)
}
