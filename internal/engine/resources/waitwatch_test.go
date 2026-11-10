package resources

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// parityStep is one answer of a wait script: an object, or an error.
type parityStep struct {
	obj *unstructured.Unstructured
	err error
}

func obj(o *unstructured.Unstructured) parityStep { return parityStep{obj: o} }
func fail(err error) parityStep                   { return parityStep{err: err} }

// parityWatch serves a poll script of Get answers as a watch. The first
// object answer is the List item; a first NOT_FOUND is an empty List followed
// by a Get that returns it; each later object answer is a Changed event on
// the open stream; a later NOT_FOUND is a Deleted event, an empty List and a
// Get that returns it; a later error is the end of the stream followed by a
// List that returns it. The last answer repeats, as in a poll script.
type parityWatch struct {
	seqClient
	steps   []parityStep
	cursor  int
	pending error
	lists   int
	opens   int
}

func (p *parityWatch) peek() parityStep { return p.steps[min(p.cursor, len(p.steps)-1)] }

func (p *parityWatch) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	if p.pending != nil {
		return nil, p.pending
	}
	st := p.peek()
	p.cursor++
	return st.obj, st.err
}

func (p *parityWatch) ListObject(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, string, error) {
	p.lists++
	st := p.peek()
	switch {
	case st.err == nil:
		p.cursor++
		return st.obj, "rv", nil
	case clierr.IsNotFound(st.err):
		p.pending = st.err
		return nil, "rv", nil
	}
	p.cursor++
	return nil, "", st.err
}

func (p *parityWatch) WatchObject(ctx context.Context, _ schema.GroupVersionResource, _, _, _ string, _ time.Duration) (<-chan engine.ObjectEvent, error) {
	p.opens++
	var events []engine.ObjectEvent
	hold := true
	for p.cursor < len(p.steps) {
		st := p.steps[p.cursor]
		if st.err == nil {
			events = append(events, engine.ObjectEvent{Type: engine.ObjectChanged, Object: st.obj})
			p.cursor++
			continue
		}
		hold = false
		if clierr.IsNotFound(st.err) {
			events = append(events, engine.ObjectEvent{Type: engine.ObjectDeleted, Object: stateObj("gone", "")})
		}
		break
	}
	ch := make(chan engine.ObjectEvent)
	go func() {
		defer close(ch)
		for _, ev := range events {
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return
			}
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
		if hold {
			<-ctx.Done()
		}
	}()
	return ch, nil
}

// outcome is what a wait gave, in the form the two modes are compared in.
type outcome struct {
	obj       map[string]any
	plain     string
	code      string
	message   string
	hint      string
	retryable bool
	details   any
}

func outcomeOf(got *unstructured.Unstructured, err error) outcome {
	var out outcome
	if got != nil {
		out.obj = got.Object
	}
	if err == nil {
		return out
	}
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok {
		out.plain = err.Error()
		return out
	}
	out.code, out.message, out.hint, out.retryable, out.details = ce.Code, ce.Message, ce.Hint, ce.Retryable, ce.Details
	return out
}

// parityCase is one wait run in both modes: polled with the script as the
// answers of Get, and watched with the script mapped as parityWatch says.
type parityCase struct {
	name          string
	steps         []parityStep
	cond          waitCond
	failOnStalled bool
	timeout       time.Duration
}

func (c parityCase) pollClient() *seqClient {
	s := &seqClient{}
	for _, st := range c.steps {
		s.objs = append(s.objs, st.obj)
		s.errs = append(s.errs, st.err)
	}
	return s
}

func (c parityCase) run(t *testing.T, client engine.ClusterClient) outcome {
	t.Helper()
	timeout := c.timeout
	if timeout == 0 {
		timeout = time.Minute
	}
	got, err := waitFor(t.Context(), client, schema.GroupVersionResource{}, "", "x", c.cond, timeout, stalledRule{fail: c.failOnStalled})
	return outcomeOf(got, err)
}

func readyCond() waitCond { return waitCond{kind: "condition", name: "Ready", status: "True"} }

func unsettledObj(ready string, generation, observed int64) *unstructured.Unstructured {
	o := multiConditionObj([3]string{"Ready", ready, ""})
	o.SetGeneration(generation)
	_ = unstructured.SetNestedField(o.Object, observed, "status", "observedGeneration")
	return o
}

// parityCases are the cases of the existing waitFor tests, as scripts.
func parityCases() []parityCase {
	active := waitCond{kind: "state", name: "active"}
	stalled := multiConditionObj([3]string{"Ready", "True", ""}, [3]string{"Stalled", "True", "authentication required: Repository not found."})
	tlsErr := clierr.New(clierr.CodeTLSTrust, "the server certificate is not trusted")
	return []parityCase{
		{name: "polls until state", steps: []parityStep{obj(stateObj("provisioning", "yes")), obj(stateObj("active", "no"))}, cond: active},
		{name: "pod phase timeout", steps: []parityStep{obj(phaseObj("Pending"))}, cond: waitCond{kind: "state", name: "running"}, timeout: 30 * time.Second},
		{name: "transitioning error fails fast", steps: []parityStep{obj(stateObj("provisioning", "yes")), obj(errorObj("quota exceeded"))}, cond: active},
		{name: "condition status", steps: []parityStep{obj(conditionObj("Ready", "False")), obj(conditionObj("Ready", "True"))}, cond: readyCond()},
		{name: "timeout carries the last state", steps: []parityStep{obj(stateObj("provisioning", "yes"))}, cond: active, timeout: 30 * time.Second},
		{name: "not found fails at once", steps: []parityStep{fail(clierr.New(clierr.CodeNotFound, "x not found").WithHint("rancher get cluster"))}, cond: active},
		{name: "retries a transient error", steps: []parityStep{obj(stateObj("provisioning", "yes")), fail(errors.New("transient network blip")), obj(stateObj("active", "no"))}, cond: active},
		{name: "timeout with no object", steps: []parityStep{fail(errors.New("down"))}, cond: active, timeout: 20 * time.Second},
		{name: "stalled fails fast", steps: []parityStep{obj(stalled)}, cond: readyCond(), failOnStalled: true},
		{name: "stalled false does not trip", steps: []parityStep{obj(multiConditionObj([3]string{"Ready", "True", ""}, [3]string{"Stalled", "False", ""}))}, cond: readyCond(), failOnStalled: true},
		{name: "stalled as the target", steps: []parityStep{obj(multiConditionObj([3]string{"Stalled", "True", "boom"}))}, cond: waitCond{kind: "condition", name: "Stalled", status: "True"}, failOnStalled: true},
		{name: "stalled with no message", steps: []parityStep{obj(multiConditionObj([3]string{"Ready", "True", ""}, [3]string{"Stalled", "True", ""}))}, cond: readyCond(), failOnStalled: true},
		{name: "skips an unsettled generation", steps: []parityStep{obj(unsettledObj("True", 2, 1)), obj(unsettledObj("True", 2, 2))}, cond: readyCond(), failOnStalled: true},
		{name: "skips a stale stalled", steps: []parityStep{obj(multiConditionObj([3]string{"Stalled", "True", "old spec"})), obj(multiConditionObj([3]string{"Ready", "True", ""}))}, cond: readyCond(), failOnStalled: true},
		{name: "timeout names an unsettled generation", steps: []parityStep{obj(unsettledObj("False", 2, 1))}, cond: readyCond(), failOnStalled: true, timeout: 30 * time.Second},
		{name: "unsettled object of a kind without stalled succeeds", steps: []parityStep{obj(unsettledObj("True", 2, 1))}, cond: readyCond()},
		{name: "auth ends at once", steps: []parityStep{fail(clierr.New(clierr.CodeAuth, "the server rejected the credential (401)").WithHint("rancher auth status"))}, cond: active},
		{name: "tls trust ends at once", steps: []parityStep{fail(tlsErr)}, cond: active},
		{name: "rbac is retried to the timeout", steps: []parityStep{fail(clierr.New(clierr.CodeRBAC, "forbidden (403)"))}, cond: active, timeout: 20 * time.Second},
		{name: "server error is retried to the timeout", steps: []parityStep{fail(clierr.Retryable(clierr.CodeGeneric, "server error (500)"))}, cond: active, timeout: 20 * time.Second},
		{name: "slow server is retried to the timeout", steps: []parityStep{fail(clierr.Retryable(clierr.CodeGeneric, "context deadline exceeded - the server may be slow"))}, cond: active, timeout: 20 * time.Second},
		{name: "raw error is retried to the timeout", steps: []parityStep{fail(errors.New("connection refused"))}, cond: active, timeout: 20 * time.Second},
		{name: "an object after an error clears the error", steps: []parityStep{fail(clierr.New(clierr.CodeRBAC, "forbidden (403)")), obj(stateObj("provisioning", "yes"))}, cond: active, timeout: 20 * time.Second},
		{name: "succeeds after a forbidden answer", steps: []parityStep{fail(clierr.New(clierr.CodeRBAC, "forbidden (403)")), obj(stateObj("active", "no"))}, cond: active},
	}
}

// TestWaitForGivesTheSameOutcomeWatchedAndPolled asserts every case of the
// existing waitFor tests gives the same outcome when the client can watch as
// when it can only Get: the same object, or the same error code, message,
// hint, details and retryable flag.
func TestWaitForGivesTheSameOutcomeWatchedAndPolled(t *testing.T) {
	for _, c := range parityCases() {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				polled := c.run(t, c.pollClient())
				watched := c.run(t, &parityWatch{steps: c.steps})
				assert.Equal(t, polled, watched)
			})
		})
	}
}

// TestWaitForWatchedNotFoundIsTheGetError asserts a NOT_FOUND from the Get
// that follows an empty List is the error the wait returns, as it is, for a
// first answer and for a delete during the stream: the empty List does not
// make an error of its own.
func TestWaitForWatchedNotFoundIsTheGetError(t *testing.T) {
	notFound := clierr.New(clierr.CodeNotFound, "cluster not found: x").WithHint("rancher get cluster")
	for name, steps := range map[string][]parityStep{
		"first answer": {fail(notFound)},
		"after a delete": {obj(stateObj("provisioning", "yes")),
			fail(notFound)},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := &parityWatch{steps: steps}
				_, err := waitFor(t.Context(), c, schema.GroupVersionResource{}, "", "x", waitCond{kind: "state", name: "active"}, time.Minute, stalledRule{})
				assert.Same(t, notFound, err)
				assert.Equal(t, clierr.CodeNotFound, clierr.From(err).Code)
			})
		})
	}
}

// TestWaitForWatchSeesAStateThatPollingSkips asserts a satisfied state that
// lasts less than one interval ends a watched wait with success, where the
// polled wait, which samples the object once an interval, times out.
func TestWaitForWatchSeesAStateThatPollingSkips(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cond := waitCond{kind: "state", name: "active"}
		watched := &parityWatch{steps: []parityStep{obj(stateObj("provisioning", "yes")), obj(stateObj("active", "no")), obj(stateObj("provisioning", "yes"))}}
		got, err := waitFor(t.Context(), watched, schema.GroupVersionResource{}, "", "x", cond, time.Minute, stalledRule{})
		require.NoError(t, err)
		assert.Equal(t, "active", mustState(got))

		sampled := &seqClient{objs: []*unstructured.Unstructured{stateObj("provisioning", "yes")}}
		_, err = waitFor(t.Context(), sampled, schema.GroupVersionResource{}, "", "x", cond, time.Minute, stalledRule{})
		require.ErrorContains(t, err, "timed out")
	})
}

// TestWaitForWatchedFailFastStateNeedsToPersist asserts that an event with
// status.transitioning=error closes the stream and does not end the wait: the
// next List, no sooner than one interval later, decides. A state the
// controller has cleared by then does not fail the wait, and one that
// persists does, with today's error.
func TestWaitForWatchedFailFastStateNeedsToPersist(t *testing.T) {
	cond := waitCond{kind: "state", name: "active"}
	t.Run("clears", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := &parityWatch{steps: []parityStep{obj(stateObj("provisioning", "yes")), obj(errorObj("quota exceeded")), obj(stateObj("active", "no"))}}
			start := time.Now()
			got, err := waitFor(t.Context(), c, schema.GroupVersionResource{}, "", "x", cond, time.Minute, stalledRule{})
			require.NoError(t, err)
			assert.Equal(t, "active", mustState(got))
			assert.GreaterOrEqual(t, time.Since(start), pollInterval, "the List that decides comes one interval after the event")
		})
	})
	t.Run("persists", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := &parityWatch{steps: []parityStep{obj(stateObj("provisioning", "yes")), obj(errorObj("quota exceeded"))}}
			_, err := waitFor(t.Context(), c, schema.GroupVersionResource{}, "", "x", cond, time.Minute, stalledRule{})
			require.ErrorContains(t, err, "x failed: quota exceeded")
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.False(t, ce.Retryable)
			assert.Equal(t, 1, c.opens, "no second stream opens: the List after the event fails the wait")
			assert.Equal(t, 2, c.lists)
		})
	})
}

// TestWaitForWatchedEventSatisfiedWhileStalled asserts an event that reads
// satisfied while Stalled=True does not return success, for a kind with
// WaitFailsOnStalled: it closes the stream, and the confirming List fails the
// wait with the Stalled error, as the Get script does.
func TestWaitForWatchedEventSatisfiedWhileStalled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stalled := multiConditionObj([3]string{"Ready", "True", ""}, [3]string{"Stalled", "True", "clone failed"})
		steps := []parityStep{obj(multiConditionObj([3]string{"Ready", "False", ""})), obj(stalled)}
		c := parityCase{steps: steps, cond: readyCond(), failOnStalled: true}
		polled := c.run(t, c.pollClient())
		watched := c.run(t, &parityWatch{steps: steps})
		assert.Equal(t, polled, watched)
		assert.Equal(t, "GENERIC", watched.code)
	})
}

// TestWaitForWatchedEventFromAnUnsettledGenerationIsIgnored asserts an event
// of a kind with WaitFailsOnStalled whose generation is not settled does not
// satisfy the wait, even when it reads satisfied.
func TestWaitForWatchedEventFromAnUnsettledGenerationIsIgnored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &parityWatch{steps: []parityStep{obj(unsettledObj("False", 2, 1)), obj(unsettledObj("True", 2, 1))}}
		_, err := waitFor(t.Context(), c, schema.GroupVersionResource{}, "", "x", readyCond(), 30*time.Second, stalledRule{fail: true})
		require.ErrorContains(t, err, "timed out")
		assert.ErrorContains(t, err, "not yet reconciled")
	})
}

// blockingWatch is a watching client whose List answers the scripted errors
// and then blocks until the context ends, returning the error the provider
// returns for a request the deadline cut.
type blockingWatch struct {
	seqClient
	scripted []error
	lists    int
	answered bool
}

func (b *blockingWatch) ListObject(ctx context.Context, _ schema.GroupVersionResource, _, _ string) (*unstructured.Unstructured, string, error) {
	i := b.lists
	b.lists++
	if i < len(b.scripted) {
		return nil, "", b.scripted[i]
	}
	<-ctx.Done()
	if b.answered {
		return nil, "", clierr.Retryable(clierr.CodeGeneric, "https://rancher.example.com answered HTTP 429 Too Many Requests and the request ran out of time (30s) - retry later").
			WithCause(clierr.ResponseError("GET", 429, nil))
	}
	return nil, "", clierr.Retryable(clierr.CodeGeneric, "context deadline exceeded - the server may be slow")
}

func (b *blockingWatch) WatchObject(context.Context, schema.GroupVersionResource, string, string, string, time.Duration) (<-chan engine.ObjectEvent, error) {
	return nil, engine.ErrWatchUnavailable
}

// TestWaitForWatchedTimeoutDuringARequestNamesNoCutError asserts a List that
// the wait's own deadline cuts is not recorded as the last error, an error
// from an earlier List stays named, and a cut that names an answered status
// is recorded, with the same messages as for a Get.
func TestWaitForWatchedTimeoutDuringARequestNamesNoCutError(t *testing.T) {
	const retryAdvice = "retry with a longer --timeout or inspect the resource to see why the wait condition is not met"
	tests := []struct {
		name    string
		client  *blockingWatch
		timeout time.Duration
		want    string
	}{
		{"no earlier error", &blockingWatch{}, 50 * time.Millisecond,
			"timed out waiting for x; last state: <no state observed>; " + retryAdvice},
		{"earlier server error", &blockingWatch{scripted: []error{clierr.New(clierr.CodeGeneric, "server error (500)")}}, 50 * time.Millisecond,
			"timed out waiting for x; last state: <no state observed>; last error: GENERIC: server error (500); " + "fix that error, or retry with a longer --timeout"},
		{"answered", &blockingWatch{answered: true}, 50 * time.Millisecond,
			"timed out waiting for x; last state: <no state observed>; last error: GENERIC: " +
				"https://rancher.example.com answered HTTP 429 Too Many Requests and the request ran out of time (30s) - retry later; " + retryAdvice},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, err := waitFor(t.Context(), tt.client, schema.GroupVersionResource{}, "", "x", waitCond{kind: "state", name: "active"}, tt.timeout, stalledRule{})
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				assert.Equal(t, tt.want, ce.Message)
			})
		})
	}
}

// TestWaitForWatchedCancelReturnsTheContextError asserts a cancelled context
// ends a watched wait with the context's own error, never the retryable
// timeout message.
func TestWaitForWatchedCancelReturnsTheContextError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &parityWatch{steps: []parityStep{obj(phaseObj("Pending"))}}
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			time.Sleep(3 * time.Second)
			cancel()
		}()
		_, err := waitFor(ctx, c, schema.GroupVersionResource{}, "", "x", waitCond{kind: "state", name: "running"}, time.Minute, stalledRule{})
		require.ErrorIs(t, err, context.Canceled)
		assert.NotContains(t, err.Error(), "timed out")
		_, structured := errors.AsType[*clierr.Error](err)
		assert.False(t, structured)
	})
}

// TestGetWaitWatchedGivesTheSummaryAndWarningsOfAPolledWait asserts, at the
// command boundary, that a wait on a watching client and the same wait on a
// polling client give the same summary apart from the elapsed time, the same
// warnings and the same object.
func TestGetWaitWatchedGivesTheSummaryAndWarningsOfAPolledWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		k := mustKind(t, "cluster")
		steps := []parityStep{obj(conditionObj("Ready", "False")), obj(conditionObj("Ready", "True"))}
		c := parityCase{steps: steps}
		run := func(client engine.ClusterClient) runtime.Result {
			cmd := NewGet(k, &recFactory{client: client}).(runtime.ReadCommand)
			res, err := cmd.Execute(t.Context(), input{args: []string{"c-abc"}, b: map[string]bool{"wait": true}}, nil)
			require.NoError(t, err)
			return res
		}
		polled := run(c.pollClient())
		watched := run(&parityWatch{steps: steps})
		assert.Equal(t, polled.Kind, watched.Kind)
		assert.Equal(t, polled.Data, watched.Data)
		assert.Equal(t, polled.Warnings, watched.Warnings)
		const prefix = "wait: condition=Ready satisfied after "
		require.Contains(t, polled.Summary, prefix)
		require.Contains(t, watched.Summary, prefix)
	})
}
