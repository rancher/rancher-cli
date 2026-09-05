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

// deferredDesc builds a Descriptor carrying no static requirement - the
// create-resource shape, whose requirement depends on the manifest and is
// supplied only through CapabilityResolver.
func deferredDesc(name string) Descriptor {
	return Descriptor{Name: name, CommandType: ReadOnly}
}

// resolverRead is a ReadCommand that also implements CapabilityResolver, for
// pipeline-seam tests of the deferred capability path.
type resolverRead struct {
	d          Descriptor
	res        Result
	resolved   CapabilityRequirement
	resolveErr error
	calls      *int
}

func (r resolverRead) Describe() Descriptor { return r.d }
func (r resolverRead) Execute(context.Context, Input, Prompter) (Result, error) {
	return r.res, nil
}
func (r resolverRead) ResolveCapability(context.Context, Input) (CapabilityRequirement, error) {
	if r.calls != nil {
		*r.calls++
	}
	return r.resolved, r.resolveErr
}

// refusingExecuteResolverRead fails the test if Execute is ever invoked -
// the resolver counterpart of refusingExecuteRead, proving the deferred
// capability stage refuses BEFORE any resource access.
type refusingExecuteResolverRead struct {
	d          Descriptor
	resolved   CapabilityRequirement
	resolveErr error
	t          *testing.T
}

func (r refusingExecuteResolverRead) Describe() Descriptor { return r.d }
func (r refusingExecuteResolverRead) Execute(context.Context, Input, Prompter) (Result, error) {
	r.t.Helper()
	r.t.Fatal("Execute must not be called when the deferred capability stage refuses")
	return Result{}, nil
}
func (r refusingExecuteResolverRead) ResolveCapability(context.Context, Input) (CapabilityRequirement, error) {
	return r.resolved, r.resolveErr
}

// refusingPlanResolverMutating fails the test if Plan is ever invoked,
// proving the deferred capability stage runs before Plan - the mode-1
// ordering invariant, same as refusingPlanMutating asserts for the static path.
type refusingPlanResolverMutating struct {
	d        Descriptor
	resolved CapabilityRequirement
	t        *testing.T
}

func (r refusingPlanResolverMutating) Describe() Descriptor { return r.d }
func (r refusingPlanResolverMutating) Plan(context.Context, Input) (safety.Effect, ExecuteFunc, error) {
	r.t.Helper()
	r.t.Fatal("Plan must not be called when the deferred capability stage refuses")
	return safety.Effect{}, nil, nil
}
func (r refusingPlanResolverMutating) ResolveCapability(context.Context, Input) (CapabilityRequirement, error) {
	return r.resolved, nil
}

// staticAndResolverRead implements both a static requirement AND
// CapabilityResolver, to assert Run's own static-wins precedence directly
// (independent of the registry's mutual-exclusion startup rejection).
type staticAndResolverRead struct {
	d            Descriptor
	res          Result
	resolveCalls *int
}

func (s staticAndResolverRead) Describe() Descriptor { return s.d }
func (s staticAndResolverRead) Execute(context.Context, Input, Prompter) (Result, error) {
	return s.res, nil
}
func (s staticAndResolverRead) ResolveCapability(context.Context, Input) (CapabilityRequirement, error) {
	if s.resolveCalls != nil {
		*s.resolveCalls++
	}
	return CapabilityRequirement{}, nil
}

func TestRunResolverZeroRequirementSkipsChecker(t *testing.T) {
	t.Parallel()
	calls := 0
	cmd := resolverRead{d: deferredDesc("create resource"), res: Result{Kind: "Widget"}, calls: &calls}
	checker := &stubChecker{dec: CapabilityDecision{Outcome: CapabilityProceed}}
	var out bytes.Buffer
	status, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &out, Format: "json", Capability: checker}, safety.Binding{})
	require.Nil(t, ce)
	assert.Equal(t, 0, status)
	assert.Equal(t, 1, calls, "the resolver must be consulted when the static descriptor carries no requirement")
	assert.Equal(t, 0, checker.calls, "a zero requirement must never reach the checker")
}

func TestRunResolverSetRequirementRunsCheckerAndProceeds(t *testing.T) {
	t.Parallel()
	calls := 0
	cmd := resolverRead{d: deferredDesc("create resource"), res: Result{Kind: "Widget"},
		resolved: CapabilityRequirement{Tier: CapabilityPrime, Feature: "widget-feature"}, calls: &calls}
	checker := &stubChecker{dec: CapabilityDecision{Outcome: CapabilityProceed}}
	var out bytes.Buffer
	status, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &out, Format: "json", Capability: checker}, safety.Binding{})
	require.Nil(t, ce)
	assert.Equal(t, 0, status)
	assert.Equal(t, 1, calls)
	assert.Equal(t, 1, checker.calls)
	assert.Equal(t, "widget-feature", checker.got.Feature)
}

func TestRunResolverPrimeRequiredBlocksExecute(t *testing.T) {
	t.Parallel()
	cmd := refusingExecuteResolverRead{d: deferredDesc("create resource"),
		resolved: CapabilityRequirement{Tier: CapabilityPrime, Feature: "widget-feature"}, t: t}
	checker := &stubChecker{dec: CapabilityDecision{Outcome: CapabilityPrimeRequired, Server: "https://rancher.example"}}
	var out, errBuf bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", Capability: checker}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodePrimeRequired, ce.Code)
	details, ok := ce.Details.(clierr.PrimeRequiredDetails)
	require.True(t, ok, "Details must carry the structured PrimeRequiredDetails shape")
	assert.Equal(t, "widget-feature", details.Feature)
}

func TestRunResolverNilCheckerFailsLoud(t *testing.T) {
	t.Parallel()
	cmd := refusingExecuteResolverRead{d: deferredDesc("create resource"),
		resolved: CapabilityRequirement{Tier: CapabilityPrime, Feature: "widget-feature"}, t: t}
	var out bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &out, Format: "json"}, safety.Binding{})
	require.NotNil(t, ce, "a deferred requirement resolving under a nil checker must fail loudly")
	assert.Contains(t, ce.Message, "no capability checker is wired")
}

func TestRunResolverErrorRendersBeforeChecker(t *testing.T) {
	t.Parallel()
	resolveErr := clierr.New(clierr.CodeValidation, "unsupported resource Foo/Bar")
	cmd := refusingExecuteResolverRead{d: deferredDesc("create resource"), resolveErr: resolveErr, t: t}
	checker := &stubChecker{dec: CapabilityDecision{Outcome: CapabilityProceed}}
	var out bytes.Buffer
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &out, Format: "json", Capability: checker}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, 0, checker.calls, "a resolver error must never reach the checker")
}

func TestRunResolverCheckedBeforePlan(t *testing.T) {
	t.Parallel()
	cmd := refusingPlanResolverMutating{d: Descriptor{Name: "create resource", CommandType: Mutating},
		resolved: CapabilityRequirement{Tier: CapabilityPrime, Feature: "widget-feature"}, t: t}
	checker := &stubChecker{dec: CapabilityDecision{Outcome: CapabilityPrimeRequired}}
	var out, errBuf bytes.Buffer
	// DryRun is set to prove the ordering holds identically under --dry-run:
	// the deferred check still precedes Plan, so no dry-run Effect is ever
	// computed for a blocked outcome.
	_, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &errBuf, Format: "json", DryRun: true, Capability: checker}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodePrimeRequired, ce.Code)
}

// TestRunStaticCapabilityNeverConsultsResolver asserts static-wins precedence
// directly at Run: a command implementing both a static requirement and
// CapabilityResolver never has its resolver called - the two cannot
// legitimately coexist (the registry rejects it at startup), but Run's own
// precedence is asserted independently here.
func TestRunStaticCapabilityNeverConsultsResolver(t *testing.T) {
	t.Parallel()
	calls := 0
	cmd := staticAndResolverRead{d: primeDesc("get widget"), res: Result{Kind: "Widget"}, resolveCalls: &calls}
	checker := &stubChecker{dec: CapabilityDecision{Outcome: CapabilityProceed}}
	var out bytes.Buffer
	status, ce := Run(t.Context(), cmd, mapInput{},
		IO{Out: &out, Err: &out, Format: "json", Capability: checker}, safety.Binding{})
	require.Nil(t, ce)
	assert.Equal(t, 0, status)
	assert.Equal(t, 0, calls, "a static requirement must win; the resolver is never consulted")
	assert.Equal(t, 1, checker.calls)
}

// TestRunMalformedStaticCapabilityFailsBeforeResolver asserts the full-shape
// validator's first boundary: a malformed STATIC value fails before the
// resolver is ever called, for all three malformations.
func TestRunMalformedStaticCapabilityFailsBeforeResolver(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		req  CapabilityRequirement
	}{
		{"unknown tier", CapabilityRequirement{Tier: "bogus", Feature: "widget-feature"}},
		{"known tier empty feature", CapabilityRequirement{Tier: CapabilityPrime}},
		{"feature no tier", CapabilityRequirement{Feature: "widget-feature"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			d := Descriptor{Name: "create resource", CommandType: ReadOnly, Capability: tc.req}
			cmd := staticAndResolverRead{d: d, res: Result{Kind: "Widget"}, resolveCalls: &calls}
			var out bytes.Buffer
			_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &out, Format: "json"}, safety.Binding{})
			require.NotNil(t, ce)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.Equal(t, 0, calls, "a malformed static value must fail before the resolver is ever called")
		})
	}
}

// TestRunMalformedResolvedCapabilityFailsBeforeChecker asserts the full-shape
// validator's second boundary: a malformed value RETURNED by a resolver
// fails before the checker and Plan/Execute, for all three malformations.
func TestRunMalformedResolvedCapabilityFailsBeforeChecker(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		req  CapabilityRequirement
	}{
		{"unknown tier", CapabilityRequirement{Tier: "bogus", Feature: "widget-feature"}},
		{"known tier empty feature", CapabilityRequirement{Tier: CapabilityPrime}},
		{"feature no tier", CapabilityRequirement{Feature: "widget-feature"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := refusingExecuteResolverRead{d: deferredDesc("create resource"), resolved: tc.req, t: t}
			checker := &stubChecker{dec: CapabilityDecision{Outcome: CapabilityProceed}}
			var out bytes.Buffer
			_, ce := Run(t.Context(), cmd, mapInput{},
				IO{Out: &out, Err: &out, Format: "json", Capability: checker}, safety.Binding{})
			require.NotNil(t, ce)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.Equal(t, 0, checker.calls, "a malformed resolver return must fail before the checker")
		})
	}
}
