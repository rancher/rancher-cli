package oidcclient

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// scriptedWatchClient serves the poll script of scriptedPollClient as a
// watch. The first object answer is the List item; a first NOT_FOUND is an
// empty List followed by a Get that returns it; each later object answer is a
// Changed event on the open stream; a later NOT_FOUND is a Deleted event, then
// an empty List and a Get that returns it; a later error is the end of the
// stream followed by a List that returns it. Events are sent eventDelay after
// the one before. A List error that makes the wait poll is answered by the Get
// that follows with the same error, because the script's next step stays that
// error. openErr is what the watch returns instead of a stream, and failEvent
// is an error event the stream sends first.
type scriptedWatchClient struct {
	*scriptedPollClient
	pending    error
	lists      int
	opens      int
	eventDelay time.Duration
	openErr    error
	failEvent  error
}

func (w *scriptedWatchClient) Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	if gvr.Resource != "secrets" && w.pending != nil {
		w.record("get")
		return nil, w.pending
	}
	return w.scriptedPollClient.Get(ctx, gvr, ns, name)
}

// sameErrorForTheNextGet reports whether a wait that got err from its List goes
// on with a Get: the errors that mean the server will not list or watch the
// object for this caller or this resource.
func sameErrorForTheNextGet(err error) bool {
	if errors.Is(err, engine.ErrWatchUnavailable) || errors.Is(err, engine.ErrListUnusable) {
		return true
	}
	if ce := clierr.From(err); ce != nil && ce.Code == clierr.CodeRBAC {
		return true
	}
	status, ok := clierr.HTTPStatus(err)
	return ok && (status == http.StatusForbidden || status == http.StatusMethodNotAllowed || status == http.StatusBadRequest)
}

func (w *scriptedWatchClient) ListObject(ctx context.Context, _ schema.GroupVersionResource, _, _ string) (*unstructured.Unstructured, string, error) {
	c := w.scriptedPollClient
	w.lists++
	c.record("list")
	if err := c.take(ctx); err != nil {
		return nil, "", err
	}
	i := c.calls - c.skip
	if i >= len(c.script) {
		if c.blockAtEnd {
			return nil, "", c.block(ctx)
		}
		i = len(c.script) - 1
	}
	st := c.script[i]
	switch {
	case st.err == nil:
		c.calls++
		return st.obj, "rv", nil
	case clierr.IsNotFound(st.err):
		w.pending = st.err
		return nil, "rv", nil
	case sameErrorForTheNextGet(st.err):
		return nil, "", st.err
	}
	c.calls++
	return nil, "", st.err
}

func (w *scriptedWatchClient) WatchObject(ctx context.Context, _ schema.GroupVersionResource, _, _, _ string, _ time.Duration) (<-chan engine.ObjectEvent, error) {
	c := w.scriptedPollClient
	w.opens++
	c.record("watch")
	if w.openErr != nil {
		return nil, w.openErr
	}
	var events []engine.ObjectEvent
	hold := true
	for i := c.calls - c.skip; i >= 0 && i < len(c.script); i = c.calls - c.skip {
		st := c.script[i]
		if st.err == nil {
			events = append(events, engine.ObjectEvent{Type: engine.ObjectChanged, Object: st.obj})
			c.calls++
			continue
		}
		hold = false
		if clierr.IsNotFound(st.err) {
			events = append(events, engine.ObjectEvent{Type: engine.ObjectDeleted, Object: oidcClientObj("my-app", "", nil)})
		}
		break
	}
	if w.failEvent != nil {
		events = []engine.ObjectEvent{{Type: engine.ObjectFailed, Err: w.failEvent}}
		hold = true
	}
	ch := make(chan engine.ObjectEvent)
	go func() {
		defer close(ch)
		for _, ev := range events {
			select {
			case <-time.After(w.eventDelay):
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

// setPollInterval sets the wait's interval for the test.
func setPollInterval(t *testing.T, d time.Duration) {
	t.Helper()
	orig := pollInterval
	pollInterval = d
	t.Cleanup(func() { pollInterval = orig })
}

// server500 is an error that names a status the server answered.
func server500() error {
	return clierr.New(clierr.CodeGeneric, "status 500").WithCause(clierr.ResponseError(http.MethodGet, http.StatusInternalServerError, nil))
}

func forbidden() error { return clierr.New(clierr.CodeRBAC, "forbidden (403)") }

// assertCommitted asserts err is a *clierr.Error with the code that carries
// the verb's committed details.
func assertCommitted(t *testing.T, v waitVerb, err error) *clierr.Error {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	details, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok, "%v", err)
	assert.True(t, details.Committed)
	assert.Equal(t, "OIDCClient", details.ResourceKind)
	assert.Equal(t, "my-app", details.ResourceName)
	return ce
}

// TestOIDCVerbWaitWatchedEndsOnTheEventThatFinishes asserts that for create,
// add-secret and remove-secret a watched wait whose List item is unfinished
// ends on the Changed event that finishes it, with one List, one watch and the
// result the polled wait gives; and that a List item that already finishes
// ends the wait with no watch.
func TestOIDCVerbWaitWatchedEndsOnTheEventThatFinishes(t *testing.T) {
	for _, v := range waitVerbsWithTimeout("30s") {
		if v.finished == nil {
			continue
		}
		t.Run(v.name+" event", func(t *testing.T) {
			withFastPoll(t)
			setup := waitSetup{script: []pollStep{{obj: v.unfinished}, {obj: v.finished}}}
			polled := v.exec(t.Context(), t, pollMode, setup)
			watched := v.exec(t.Context(), t, watchMode, setup)
			require.NoError(t, polled.err)
			require.NoError(t, watched.err)
			assert.Equal(t, v.finishedKind, watched.res.Kind)
			assert.Equal(t, polled.res.Kind, watched.res.Kind)
			assert.Equal(t, polled.res.Text, watched.res.Text)
			assert.Equal(t, polled.res.Data, watched.res.Data)
			assert.Equal(t, polled.res.Warnings, watched.res.Warnings)
			assert.Equal(t, 1, watched.watch.lists)
			assert.Equal(t, 1, watched.watch.opens)
		})
		t.Run(v.name+" List item", func(t *testing.T) {
			withFastPoll(t)
			setup := waitSetup{script: []pollStep{{obj: v.finished}}}
			polled := v.exec(t.Context(), t, pollMode, setup)
			watched := v.exec(t.Context(), t, watchMode, setup)
			require.NoError(t, polled.err)
			require.NoError(t, watched.err)
			assert.Equal(t, polled.res.Kind, watched.res.Kind)
			assert.Equal(t, polled.res.Text, watched.res.Text)
			assert.Equal(t, polled.res.Data, watched.res.Data)
			assert.Equal(t, 1, watched.watch.lists)
			assert.Zero(t, watched.watch.opens, "the List item ends the wait, no watch")
		})
	}
}

// TestOIDCVerbWaitCancelIsATimeout asserts that for every verb, polling or
// watching, a cancel in the middle of the wait returns the verb's timeout
// error with the committed details and the last error, never
// context.Canceled.
func TestOIDCVerbWaitCancelIsATimeout(t *testing.T) {
	for _, v := range waitVerbsWithTimeout("60s") {
		for _, m := range waitModes {
			t.Run(v.name+" "+string(m), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					setPollInterval(t, 2*time.Second)
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					go func() {
						time.Sleep(3 * time.Second)
						cancel()
					}()
					r := v.exec(ctx, t, m, waitSetup{script: []pollStep{{err: forbidden()}}, blockAtEnd: true})
					require.Error(t, r.err)
					require.NotErrorIs(t, r.err, context.Canceled)
					ce := assertCommitted(t, v, r.err)
					assert.Equal(t, clierr.CodeGeneric, ce.Code)
					assert.Equal(t, v.retryableTimeout, ce.Retryable)
					assert.Contains(t, ce.Message, "timed out waiting for oidc-client my-app")
					assert.Contains(t, ce.Message, "; last error: RBAC: forbidden (403)")
				})
			})
		}
	}
}

// TestOIDCVerbWaitAnErrorAfterTheDeadlineIsNotTheLastError asserts that for
// every verb, polling or watching, a request that blocks to the deadline and
// then returns an error that names an answered status (a 500) leaves no last
// error in the timeout message.
func TestOIDCVerbWaitAnErrorAfterTheDeadlineIsNotTheLastError(t *testing.T) {
	for _, v := range waitVerbsWithTimeout("5s") {
		for _, m := range waitModes {
			t.Run(v.name+" "+string(m), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					setPollInterval(t, 2*time.Second)
					r := v.exec(t.Context(), t, m, waitSetup{blockAtEnd: true, blockErr: server500()})
					ce := assertCommitted(t, v, r.err)
					assert.Contains(t, ce.Message, "timed out waiting for oidc-client my-app")
					assert.NotContains(t, ce.Message, "last error")
					assert.NotContains(t, ce.Message, "500")
				})
			})
		}
	}
}

// TestOIDCVerbWaitRequestsStartAnIntervalApartFromTheStart asserts that for
// every verb, with requests that take 1.5 seconds and an interval of 2
// seconds, the requests of a polled wait start at 0, 2 and 4 seconds, and a
// watched wait lists at 0, opens its watch at 1.5 when the List returns and
// lists again at 4, so a slow request does not delay the next cycle.
func TestOIDCVerbWaitRequestsStartAnIntervalApartFromTheStart(t *testing.T) {
	for _, v := range waitVerbsWithTimeout("5s") {
		for _, tt := range []struct {
			mode waitMode
			want []string
		}{
			{pollMode, []string{"get@0", "get@2", "get@4"}},
			{watchMode, []string{"list@0", "watch@1.5", "list@4"}},
		} {
			t.Run(v.name+" "+string(tt.mode), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					setPollInterval(t, 2*time.Second)
					r := v.exec(t.Context(), t, tt.mode, waitSetup{
						script: []pollStep{{obj: v.unfinished}, {err: clierr.Retryable(clierr.CodeGeneric, "server error (500)")}},
						takes:  1500 * time.Millisecond,
					})
					require.Error(t, r.err)
					assert.Equal(t, tt.want, r.client.log)
				})
			})
		}
	}
}

// TestOIDCVerbWaitWatchedAbortsAtOnceOnAuthAndTLSFailures asserts that for
// every verb an authentication or certificate failure from the List, from the
// watch open and from an error event of the stream ends the wait through the
// verb's abort, with the committed details and no further request.
func TestOIDCVerbWaitWatchedAbortsAtOnceOnAuthAndTLSFailures(t *testing.T) {
	bad := []error{
		clierr.New(clierr.CodeAuth, "the server rejected the credential (401)"),
		clierr.New(clierr.CodeTLSTrust, "the server certificate is not trusted"),
	}
	for _, v := range waitVerbsWithTimeout("30s") {
		for _, err := range bad {
			code := clierr.From(err).Code
			for _, tt := range []struct {
				name  string
				setup waitSetup
				want  []string
			}{
				{"List", waitSetup{script: []pollStep{{err: err}}}, []string{"list@0"}},
				{"open", waitSetup{script: []pollStep{{obj: v.unfinished}}, watch: func(w *scriptedWatchClient) { w.openErr = err }}, []string{"list@0", "watch@0"}},
				{"error event", waitSetup{script: []pollStep{{obj: v.unfinished}}, watch: func(w *scriptedWatchClient) { w.failEvent = err }}, []string{"list@0", "watch@0"}},
			} {
				t.Run(v.name+" "+code+" "+tt.name, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						setPollInterval(t, 2*time.Second)
						r := v.exec(t.Context(), t, watchMode, tt.setup)
						ce := assertCommitted(t, v, r.err)
						assert.Equal(t, code, ce.Code)
						assert.False(t, ce.Retryable)
						details, _ := ce.Details.(clierr.CommittedContextDetails)
						assert.Equal(t, v.abortSecretName, details.SecretName)
						assert.Equal(t, v.abortRecovery, details.Recovery)
						assert.Equal(t, tt.want, r.client.log)
					})
				})
			}
		}
	}
}

// TestOIDCVerbWaitNotFoundIsReturnedAsItIs asserts that for every verb a
// NOT_FOUND from a Get ends the wait with that error, unclassified: from the
// first Get of a polled wait, from the Get that follows an empty List, and
// from the Get that follows the empty List after a Deleted event.
func TestOIDCVerbWaitNotFoundIsReturnedAsItIs(t *testing.T) {
	for _, v := range waitVerbsWithTimeout("30s") {
		notFound := clierr.New(clierr.CodeNotFound, "oidcclient my-app not found").WithHint("rancher get oidcclient")
		for _, tt := range []struct {
			name   string
			mode   waitMode
			script []pollStep
		}{
			{"polled", pollMode, []pollStep{{err: notFound}}},
			{"empty List", watchMode, []pollStep{{err: notFound}}},
			{"Deleted event", watchMode, []pollStep{{obj: v.unfinished}, {err: notFound}}},
		} {
			t.Run(v.name+" "+tt.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					setPollInterval(t, 2*time.Second)
					r := v.exec(t.Context(), t, tt.mode, waitSetup{script: tt.script})
					assert.Same(t, notFound, r.err)
				})
			})
		}
	}
}

// TestOIDCVerbWaitForbiddenListThenPollsFromTheSecondRequest asserts that for
// every verb a List that answers 403 is followed by a Get in the same cycle,
// the wait polls from then on, the second Get comes 4 seconds after the first
// request (twice the 2 second interval) and later ones every 2 seconds.
func TestOIDCVerbWaitForbiddenListThenPollsFromTheSecondRequest(t *testing.T) {
	for _, v := range waitVerbsWithTimeout("7s") {
		t.Run(v.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				setPollInterval(t, 2*time.Second)
				r := v.exec(t.Context(), t, watchMode, waitSetup{script: []pollStep{{err: forbidden()}}})
				require.Error(t, r.err)
				assert.Equal(t, []string{"list@0", "get@0", "get@4", "get@6"}, r.client.log)
			})
		})
	}
}

// TestWaitForOIDCClientResultsAreNotTypedNils asserts that the error a wait
// returns is nil on success, the verb's own terminal error when check gives one,
// and never a nil *clierr.Error inside an error.
func TestWaitForOIDCClientResultsAreNotTypedNils(t *testing.T) {
	gvr := schema.GroupVersionResource{Resource: "oidcclients"}
	client := &fakeOIDCClient{pollResponses: []*unstructured.Unstructured{oidcClientObj("my-app", "", nil)}}
	onTimeout := func(*unstructured.Unstructured) *clierr.Error { return clierr.New(clierr.CodeGeneric, "timed out") }
	onAbort := func(err error) *clierr.Error { return clierr.New(clierr.CodeAuth, err.Error()) }

	value, err := waitForOIDCClient(t.Context(), client, gvr, "my-app", time.Minute,
		func(context.Context, *unstructured.Unstructured) waitOutcome {
			return waitOutcome{done: true, value: "v"}
		},
		onTimeout, onAbort)
	require.NoError(t, err)
	assert.Equal(t, "v", value)

	terminal := clierr.New(clierr.CodeGeneric, "impossible shape")
	_, err = waitForOIDCClient(t.Context(), client, gvr, "my-app", time.Minute,
		func(context.Context, *unstructured.Unstructured) waitOutcome { return waitOutcome{terminal: terminal} },
		onTimeout, onAbort)
	assert.Same(t, terminal, err)
}
