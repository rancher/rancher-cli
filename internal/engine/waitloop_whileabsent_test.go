package engine

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func notFoundErr() error { return clierr.New(clierr.CodeNotFound, "x not found") }

func whileAbsent(p *WaitParams) { p.WhileAbsent = true }

// TestWaitObjectWhileAbsentWaitsForAnObjectToAppear asserts that with
// WhileAbsent a NOT_FOUND Get keeps the wait going, at the usual spacing,
// without calling Observe, until the object appears and Observe sees it, with
// a polled and with a watched client.
func TestWaitObjectWhileAbsentWaitsForAnObjectToAppear(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLoop(t)
		f.gets = []answer{{err: notFoundErr()}, {err: notFoundErr()}, {state: "done"}}
		res := runLoop(t.Context(), getOnly{f: f}, time.Minute, whileAbsent)
		require.NoError(t, res.err)
		require.NotNil(t, res.done)
		assert.Equal(t, "get@0 get@5 get@10", f.sequence())
		assert.Equal(t, []WaitSource{SourceGet}, res.sources, "Observe sees only the object")
		f.requireInvariants(loopInterval)
	})
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLoop(t)
		f.lists = []answer{{none: true}, {none: true}, {state: "done"}}
		f.gets = []answer{{err: notFoundErr()}}
		res := runLoop(t.Context(), f, time.Minute, whileAbsent)
		require.NoError(t, res.err)
		require.NotNil(t, res.done)
		assert.Equal(t, "list@0 get@0 list@10 get@10 list@20", f.sequence())
		assert.Equal(t, []WaitSource{SourceList}, res.sources)
		f.requireInvariants(loopInterval)
	})
}

// TestWaitObjectWhileAbsentTimesOutWithNoLastObject asserts an object absent
// for the whole wait ends at the timeout, through OnTimeout with no last
// object and no last error, with a polled and with a watched client.
func TestWaitObjectWhileAbsentTimesOutWithNoLastObject(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		client func(f *fakeLoop) ClusterClient
	}{
		{"poll", func(f *fakeLoop) ClusterClient { return getOnly{f: f} }},
		{"watch", func(f *fakeLoop) ClusterClient { f.lists = []answer{{none: true}}; return f }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := newFakeLoop(t)
				f.gets = []answer{{err: notFoundErr()}}
				res := runLoop(t.Context(), tt.client(f), 30*time.Second, whileAbsent)
				var to *timedOut
				require.ErrorAs(t, res.err, &to)
				assert.Nil(t, to.last)
				assert.Nil(t, to.lastErr)
				assert.Empty(t, res.sources, "Observe is never called")
				assert.Equal(t, 30*time.Second, res.elapsed)
			})
		})
	}
}

// TestWaitObjectWhileAbsentClearsTheLastObjectAndError asserts an object
// seen, then a failed read, then a NOT_FOUND Get leave the timeout with no
// last object and no last error from before the NOT_FOUND: by polling, and
// by watching, where a Deleted event ends the stream and the next cycle's
// empty List and Get find the object gone.
func TestWaitObjectWhileAbsentClearsTheLastObjectAndError(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLoop(t)
		f.gets = []answer{{state: "pending"}, {err: statusErr(http.StatusInternalServerError)}, {err: notFoundErr()}}
		res := runLoop(t.Context(), getOnly{f: f}, 18*time.Second, whileAbsent)
		var to *timedOut
		require.ErrorAs(t, res.err, &to)
		assert.Nil(t, to.last, "the NOT_FOUND clears the object seen before it")
		assert.Nil(t, to.lastErr, "the NOT_FOUND clears the error before it")
		assert.Equal(t, "get@0 get@5 get@10 get@15", f.sequence())
	})
	synctest.Test(t, func(t *testing.T) {
		withStreamSeconds(t, 55)
		f := newFakeLoop(t)
		f.lists = []answer{{state: "pending"}, {err: statusErr(http.StatusInternalServerError)}, {none: true}}
		f.gets = []answer{{err: notFoundErr()}}
		f.streams = []streamScript{{steps: []streamStep{{wait: time.Second, ev: ObjectEvent{Type: ObjectDeleted, Object: loopObject("pending", "event-rv")}}}, hold: true}}
		res := runLoop(t.Context(), f, 28*time.Second, whileAbsent)
		var to *timedOut
		require.ErrorAs(t, res.err, &to)
		assert.Nil(t, to.last)
		assert.Nil(t, to.lastErr)
		assert.Equal(t, "list@0 watch@0 list@10 list@15 get@15 list@25 get@25", f.sequence())
		f.requireInvariants(loopInterval)
	})
}

// TestWaitObjectWhileAbsentDeletedEventClearsTheLastObject asserts a Deleted
// event clears the last object at once with WhileAbsent, so a wait that ends
// before the next cycle reads again sees the object absent; without
// WhileAbsent the last object stays.
func TestWaitObjectWhileAbsentDeletedEventClearsTheLastObject(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		mod     func(*WaitParams)
		cleared bool
	}{
		{"while absent", whileAbsent, true},
		{"default", nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				withStreamSeconds(t, 55)
				f := newFakeLoop(t)
				f.lists = []answer{{state: "pending"}}
				f.streams = []streamScript{{steps: []streamStep{{wait: 3 * time.Second, ev: ObjectEvent{Type: ObjectDeleted, Object: loopObject("pending", "event-rv")}}}, hold: true}}
				res := runLoop(t.Context(), f, 8*time.Second, tt.mod)
				var to *timedOut
				require.ErrorAs(t, res.err, &to)
				assert.Equal(t, "list@0 watch@0", f.sequence(), "the wait ends before the next cycle")
				if tt.cleared {
					assert.Nil(t, to.last)
				} else {
					assert.NotNil(t, to.last)
				}
			})
		})
	}
}

// TestWaitObjectOnDeletedDecidesAtTheEvent asserts OnDeleted decides on a
// Deleted event at once: WaitDone ends the wait with success and an error
// ends it with that error, both at the event; WaitContinue leaves the event
// to the next cycle, as a nil OnDeleted does.
func TestWaitObjectOnDeletedDecidesAtTheEvent(t *testing.T) {
	t.Parallel()
	errGone := errors.New("gone")
	for _, tt := range []struct {
		name    string
		out     WaitOutcome
		err     error
		want    error
		elapsed time.Duration
		seq     string
	}{
		{"done", WaitDone, nil, nil, time.Second, "list@0 watch@0"},
		{"error", WaitContinue, errGone, errGone, time.Second, "list@0 watch@0"},
		{"continue", WaitContinue, nil, nil, 10 * time.Second, "list@0 watch@0 list@10"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				withStreamSeconds(t, 55)
				f := newFakeLoop(t)
				f.lists = []answer{{state: "pending"}, {state: "done"}}
				f.streams = []streamScript{{steps: []streamStep{{wait: time.Second, ev: ObjectEvent{Type: ObjectDeleted, Object: loopObject("pending", "event-rv")}}}, hold: true}}
				calls := 0
				res := runLoop(t.Context(), f, time.Minute, func(p *WaitParams) {
					p.OnDeleted = func(context.Context) (WaitOutcome, error) {
						calls++
						return tt.out, tt.err
					}
				})
				assert.Equal(t, tt.want, res.err)
				assert.Equal(t, 1, calls)
				assert.Equal(t, tt.elapsed, res.elapsed)
				assert.Equal(t, tt.seq, f.sequence())
			})
		})
	}
}

// TestWaitObjectWhileAbsentKeepsTheRequestBound asserts a wait for an absent
// object sends at most floor(T / Interval) + 2 calls by any time T, with both
// spacings and both clients.
func TestWaitObjectWhileAbsentKeepsTheRequestBound(t *testing.T) {
	t.Parallel()
	for _, sp := range loopSpacings {
		for _, tt := range []struct {
			name   string
			client func(f *fakeLoop) ClusterClient
		}{
			{"poll", func(f *fakeLoop) ClusterClient { return getOnly{f: f} }},
			{"watch", func(f *fakeLoop) ClusterClient { f.lists = []answer{{none: true}}; return f }},
		} {
			t.Run(sp.name+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) {
					f := newFakeLoop(t)
					f.gets = []answer{{err: notFoundErr()}}
					f.getTakes = 2 * time.Second
					res := runLoop(t.Context(), tt.client(f), sp.timeout, func(p *WaitParams) {
						sp.apply(p)
						p.WhileAbsent = true
					})
					var to *timedOut
					require.ErrorAs(t, res.err, &to)
					require.NotZero(t, f.countOf("get"))
					f.requireInvariants(sp.interval)
				})
			})
		}
	}
}

// TestWaitObjectWithoutWhileAbsentEndsOnNotFound asserts the field's default:
// a NOT_FOUND Get ends the wait with the error as it is.
func TestWaitObjectWithoutWhileAbsentEndsOnNotFound(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLoop(t)
		nf := notFoundErr()
		f.gets = []answer{{state: "pending"}, {err: nf}}
		res := runLoop(t.Context(), getOnly{f: f}, time.Minute, nil)
		assert.Same(t, nf, res.err)
		assert.Equal(t, "get@0 get@5", f.sequence())
	})
}
