package access

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestRunFanOutDerivesSourceAndHonoursTerminalOutcomes asserts the member
// policy: degradable failures become issues, a terminal outcome aborts
// with its error, and the result source is derived from member sources.
func TestRunFanOutDerivesSourceAndHonoursTerminalOutcomes(t *testing.T) {
	t.Parallel()
	answers := map[string]reviewResult{
		"a": {kind: outcomeAnswer},
		"b": {kind: outcomeDegradable, err: clierr.Retryable(clierr.CodeGeneric, "503")},
	}
	members, terminal := runFanOut(t.Context(), []string{"a", "b"}, func(_ context.Context, k string) (string, reviewResult) {
		return k, answers[k]
	})
	require.Nil(t, terminal)
	require.Len(t, members, 2)
	assert.Equal(t, MemberAuthorizer, members[0].Source)
	assert.Equal(t, MemberRequest, members[1].Source)
	require.NotNil(t, members[1].Issue)
	assert.True(t, members[1].Issue.Retryable)
	assert.Equal(t, SourceMixed, deriveSource(members))

	_, terminal = runFanOut(t.Context(), []string{"a", "b"}, func(_ context.Context, k string) (string, reviewResult) {
		if k == "b" {
			return k, reviewResult{kind: outcomeTerminal, err: clierr.New(clierr.CodeRBAC, "cannot create resource")}
		}
		return k, reviewResult{kind: outcomeAnswer}
	})
	require.NotNil(t, terminal)
	assert.Equal(t, outcomeTerminal, terminal.Kind)
	assert.Equal(t, clierr.CodeRBAC, terminal.Err.Code)
}

// TestRunFanOutMidRunClusterAccessLossIsTerminal asserts a member's
// cluster-access refusal after a successful preflight aborts the fan-out and keeps
// its own kind, so no caller can mistake it for a refused review.
func TestRunFanOutMidRunClusterAccessLossIsTerminal(t *testing.T) {
	t.Parallel()
	_, terminal := runFanOut(t.Context(), []string{"a", "b"}, func(_ context.Context, k string) (string, reviewResult) {
		if k == "b" {
			return k, reviewResult{kind: outcomeNoClusterAccess, resp: http403(""), err: clierr.New(clierr.CodeRBAC, "")}
		}
		return k, reviewResult{kind: outcomeAnswer}
	})
	require.NotNil(t, terminal)
	assert.Equal(t, outcomeNoClusterAccess, terminal.Kind)
	assert.Equal(t, 403, terminal.Status)
	assert.Equal(t, clierr.CodeRBAC, terminal.Err.Code)
	require.ErrorContains(t, terminal, "cluster access changed during the run")
}

// TestRunFanOutBudgetIsTerminal asserts an exhausted overall budget is an
// error carrying the evaluated count, never a rollup.
func TestRunFanOutBudgetIsTerminal(t *testing.T) {
	old := fanOutBudget
	fanOutBudget = 50 * time.Millisecond
	t.Cleanup(func() { fanOutBudget = old })
	ctx, cancel := fanOutContext(t.Context())
	defer cancel()
	_, terminal := runFanOut(ctx, []string{"a", "b", "c"}, func(ctx context.Context, k string) (string, reviewResult) {
		select {
		case <-ctx.Done():
			return k, reviewResult{kind: outcomeTerminal, err: clierr.New(clierr.CodeGeneric, ctx.Err().Error())}
		case <-time.After(time.Second):
			return k, reviewResult{kind: outcomeAnswer}
		}
	})
	require.NotNil(t, terminal)
	assert.Equal(t, outcomeTerminal, terminal.Kind)
	assert.Equal(t, 0, terminal.Status)
	require.ErrorContains(t, terminal, "fan-out budget of 50ms exhausted; evaluated 0 of 3")
}

// TestRunFanOutTerminalAbandonsQueuedMembers asserts that after a terminal
// member no queued member is launched, even with a dead context.
func TestRunFanOutTerminalAbandonsQueuedMembers(t *testing.T) {
	old := fanOutLimit
	fanOutLimit = 1
	t.Cleanup(func() { fanOutLimit = old })
	var calls atomic.Int32
	_, terminal := runFanOut(t.Context(), []string{"a", "b", "c"}, func(_ context.Context, k string) (string, reviewResult) {
		calls.Add(1)
		return k, reviewResult{kind: outcomeTerminal, err: clierr.New(clierr.CodeGeneric, "endpoint gone")}
	})
	require.NotNil(t, terminal)
	assert.Equal(t, int32(1), calls.Load(), "queued members are abandoned, never launched")
}

// TestRunFanOutNoMemberStartsAfterCancellation asserts, with two workers,
// that a member still queued when the first member turned terminal is
// never started: the second worker is held inside fn until the
// cancellation, and the third key never reaches fn.
func TestRunFanOutNoMemberStartsAfterCancellation(t *testing.T) {
	old := fanOutLimit
	fanOutLimit = 2
	t.Cleanup(func() { fanOutLimit = old })
	var mu sync.Mutex
	started := map[string]bool{}
	_, terminal := runFanOut(t.Context(), []string{"a", "b", "c"}, func(ctx context.Context, k string) (string, reviewResult) {
		mu.Lock()
		started[k] = true
		mu.Unlock()
		if k == "a" {
			return k, reviewResult{kind: outcomeTerminal, err: clierr.New(clierr.CodeGeneric, "endpoint gone")}
		}
		<-ctx.Done() // b is in flight when a turns terminal; it observes the cancellation
		return k, reviewResult{kind: outcomeTerminal, err: clierr.New(clierr.CodeGeneric, ctx.Err().Error())}
	})
	require.NotNil(t, terminal)
	require.ErrorContains(t, terminal, "endpoint gone")
	mu.Lock()
	defer mu.Unlock()
	assert.True(t, started["a"])
	assert.False(t, started["c"], "a queued member never starts after the cancellation")
}

// TestBudgetErrorNamesTheBudgetOnlyWhenItCancelled asserts the mapping.
func TestBudgetErrorNamesTheBudgetOnlyWhenItCancelled(t *testing.T) {
	old := fanOutBudget
	fanOutBudget = time.Millisecond
	t.Cleanup(func() { fanOutBudget = old })
	ctx, cancel := fanOutContext(t.Context())
	defer cancel()
	<-ctx.Done()
	err := budgetError(ctx, ctx.Err(), "namespace discovery")
	require.ErrorContains(t, err, "fan-out budget of 1ms exhausted during namespace discovery")
	plain := clierr.New(clierr.CodeRBAC, "forbidden")
	assert.Same(t, plain, budgetError(t.Context(), plain, "namespace discovery"))
}
