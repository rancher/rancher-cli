package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const loopInterval = 5 * time.Second

// loopCall is one call the loop made on the fake client.
type loopCall struct {
	kind string // "list", "get" or "watch"
	at   time.Duration
	// rv is the version a list answered or a watch asked for.
	rv string
	// timeout is the length a watch asked for.
	timeout time.Duration
	// unsent marks a call that sent no request (ErrWatchUnavailable).
	unsent bool
}

// answer is what a fake List or Get gives: an object in the state, nothing
// (an empty list), or an error.
type answer struct {
	state string
	none  bool
	err   error
}

// streamStep is one event of a scripted stream, sent wait after the one
// before it.
type streamStep struct {
	wait time.Duration
	ev   ObjectEvent
}

// streamScript is what a fake WatchObject gives: an open error, or a stream
// that sends its steps, then stays open until its context ends (hold) or
// closes length after the last step.
type streamScript struct {
	err    error
	steps  []streamStep
	hold   bool
	length time.Duration
}

// fakeLoop is a ClusterClient and ObjectWatcher that answers from scripts,
// the last answer of each repeating, and logs every call with its time.
type fakeLoop struct {
	ClusterClient
	t       *testing.T
	start   time.Time
	calls   []loopCall
	lists   []answer
	gets    []answer
	streams []streamScript
	// getTakes, listTakes and watchTakes are how long a Get, a List and a watch
	// open take.
	getTakes   time.Duration
	listTakes  time.Duration
	watchTakes time.Duration
	opened     []context.Context
}

func newFakeLoop(t *testing.T) *fakeLoop {
	t.Helper()
	return &fakeLoop{t: t, start: time.Now(), lists: []answer{{state: "pending"}}, gets: []answer{{state: "pending"}},
		streams: []streamScript{{hold: true}}}
}

func (f *fakeLoop) since() time.Duration { return time.Since(f.start) }

func loopObject(state, rv string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "x", "resourceVersion": rv},
		"status":   map[string]any{"state": state},
	}}
}

func (f *fakeLoop) Get(_ context.Context, _ schema.GroupVersionResource, _, _ string) (*unstructured.Unstructured, error) {
	a := f.gets[min(f.countOf("get"), len(f.gets)-1)]
	f.calls = append(f.calls, loopCall{kind: "get", at: f.since()})
	if f.getTakes > 0 {
		time.Sleep(f.getTakes)
	}
	if a.err != nil {
		return nil, a.err
	}
	return loopObject(a.state, "item-rv"), nil
}

func (f *fakeLoop) ListObject(_ context.Context, _ schema.GroupVersionResource, _, _ string) (*unstructured.Unstructured, string, error) {
	n := f.countOf("list")
	a := f.lists[min(n, len(f.lists)-1)]
	rv := fmt.Sprintf("list-%d", n)
	f.calls = append(f.calls, loopCall{kind: "list", at: f.since(), rv: rv, unsent: errors.Is(a.err, ErrWatchUnavailable)})
	if f.listTakes > 0 {
		time.Sleep(f.listTakes)
	}
	switch {
	case a.err != nil:
		return nil, "", a.err
	case a.none:
		return nil, rv, nil
	}
	return loopObject(a.state, "item-rv"), rv, nil
}

func (f *fakeLoop) WatchObject(ctx context.Context, _ schema.GroupVersionResource, _, _, rv string, timeout time.Duration) (<-chan ObjectEvent, error) {
	sc := f.streams[min(f.countOf("watch"), len(f.streams)-1)]
	f.calls = append(f.calls, loopCall{kind: "watch", at: f.since(), rv: rv, timeout: timeout, unsent: errors.Is(sc.err, ErrWatchUnavailable)})
	for i, c := range f.opened {
		require.Error(f.t, c.Err(), "stream %d is still open when the next stream opens", i)
	}
	if f.watchTakes > 0 {
		time.Sleep(f.watchTakes)
	}
	if sc.err != nil {
		return nil, sc.err
	}
	f.opened = append(f.opened, ctx)
	ch := make(chan ObjectEvent)
	go func() {
		defer close(ch)
		for _, step := range sc.steps {
			select {
			case <-time.After(step.wait):
			case <-ctx.Done():
				return
			}
			select {
			case ch <- step.ev:
			case <-ctx.Done():
				return
			}
		}
		if sc.hold {
			<-ctx.Done()
			return
		}
		select {
		case <-time.After(sc.length):
		case <-ctx.Done():
		}
	}()
	return ch, nil
}

func (f *fakeLoop) countOf(kind string) int {
	n := 0
	for _, c := range f.calls {
		if c.kind == kind {
			n++
		}
	}
	return n
}

// sequence is the calls as "kind@seconds", for a readable comparison.
func (f *fakeLoop) sequence() string {
	var out []string
	for _, c := range f.calls {
		out = append(out, fmt.Sprintf("%s@%g", c.kind, c.at.Seconds()))
	}
	return strings.Join(out, " ")
}

// requireInvariants asserts what holds for every run: the calls sent by any
// time T are at most floor(T / interval) + 2, and every watch starts from
// the version of the list just before it.
func (f *fakeLoop) requireInvariants(interval time.Duration) {
	f.t.Helper()
	sent := 0
	lastList := ""
	for _, c := range f.calls {
		if c.unsent {
			continue
		}
		sent++
		require.LessOrEqual(f.t, sent, int(c.at/interval)+2, "calls sent by %v: %s", c.at, f.sequence())
		switch c.kind {
		case "list":
			lastList = c.rv
		case "watch":
			require.Equal(f.t, lastList, c.rv, "a watch starts from the version of the list before it: %s", f.sequence())
		}
	}
}

func authErr() error {
	return clierr.New(clierr.CodeAuth, "the server rejected the credential (401)")
}

func statusErr(status int) error {
	return clierr.New(clierr.CodeGeneric, fmt.Sprintf("status %d", status)).WithCause(clierr.ResponseError(http.MethodGet, status, nil))
}

func rbacErr() error { return clierr.New(clierr.CodeRBAC, "forbidden (403)") }

func changed(state string) ObjectEvent {
	return ObjectEvent{Type: ObjectChanged, Object: loopObject(state, "event-rv")}
}

func failedEvent(err error) ObjectEvent { return ObjectEvent{Type: ObjectFailed, Err: err} }

// timedOut is what the test OnTimeout returns.
type timedOut struct {
	last    *unstructured.Unstructured
	lastErr *clierr.Error
}

func (*timedOut) Error() string { return "timed out" }

var errTerminal = errors.New("terminal state")

// loopResult is what one run of the loop gave.
type loopResult struct {
	err     error
	done    *unstructured.Unstructured
	sources []WaitSource
	elapsed time.Duration
}

// runLoop runs WaitObject with a test Observe: state "done" ends the wait,
// "close" closes the stream when it comes from an event, "bad" is a terminal
// error, anything else continues.
func runLoop(ctx context.Context, client ClusterClient, timeout time.Duration, mod func(*WaitParams)) loopResult {
	var res loopResult
	p := WaitParams{
		GVR: schema.GroupVersionResource{Resource: "things"}, Name: "x",
		Timeout: timeout, Interval: loopInterval, SpacingFrom: SpacingFromReturn,
		Observe: func(_ context.Context, obj *unstructured.Unstructured, source WaitSource) (WaitOutcome, error) {
			res.sources = append(res.sources, source)
			state, _, _ := unstructured.NestedString(obj.Object, "status", "state")
			switch {
			case state == "done":
				res.done = obj
				return WaitDone, nil
			case state == "close" && source == SourceEvent:
				return WaitClose, nil
			case state == "bad":
				return WaitContinue, errTerminal
			}
			return WaitContinue, nil
		},
		RecordErr: func(ctx context.Context, _ error) bool { return ctx.Err() == nil },
		OnAbort:   func(err error) error { return err },
		OnCancel:  func(ctx context.Context, _ *unstructured.Unstructured, _ *clierr.Error) error { return ctx.Err() },
		OnTimeout: func(last *unstructured.Unstructured, lastErr *clierr.Error) error {
			return &timedOut{last: last, lastErr: lastErr}
		},
	}
	if mod != nil {
		mod(&p)
	}
	start := time.Now()
	res.err = WaitObject(ctx, client, p)
	res.elapsed = time.Since(start)
	return res
}

// withStreamSeconds fixes the length of every watch to secs seconds.
func withStreamSeconds(t *testing.T, secs int) {
	t.Helper()
	t.Cleanup(SetStreamSecondsForTest(secs, secs))
}

// getOnly is a ClusterClient that has only Get: it does not implement
// ObjectWatcher, so the loop polls.
type getOnly struct {
	ClusterClient
	f *fakeLoop
}

func (g getOnly) Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	return g.f.Get(ctx, gvr, ns, name)
}

// TestWaitObjectPollsWithAClientThatCannotWatch asserts a client without
// ObjectWatcher is polled with one Get a cycle, Interval apart.
func TestWaitObjectPollsWithAClientThatCannotWatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLoop(t)
		f.gets = []answer{{state: "pending"}, {state: "pending"}, {state: "done"}}
		res := runLoop(t.Context(), getOnly{f: f}, time.Minute, nil)
		require.NoError(t, res.err)
		assert.Equal(t, "get@0 get@5 get@10", f.sequence())
		assert.Equal(t, []WaitSource{SourceGet, SourceGet, SourceGet}, res.sources)
		f.requireInvariants(loopInterval)
	})
}

// TestWaitObjectSpacesCyclesFromTheReturnOrTheStart asserts SpacingFrom
// decides whether a slow request delays the next cycle.
func TestWaitObjectSpacesCyclesFromTheReturnOrTheStart(t *testing.T) {
	for _, tt := range []struct {
		name string
		from SpacingFrom
		want string
	}{
		{"from the return", SpacingFromReturn, "get@0 get@8 get@16"},
		{"from the start", SpacingFromStart, "get@0 get@5 get@10 get@15"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFakeLoop(t)
				f.getTakes = 3 * time.Second
				res := runLoop(t.Context(), getOnly{f: f}, 17*time.Second, func(p *WaitParams) { p.SpacingFrom = tt.from })
				var to *timedOut
				require.ErrorAs(t, res.err, &to)
				assert.Equal(t, tt.want, f.sequence())
			})
		})
	}
}

// TestWaitObjectListItemThatSatisfiesEndsWithoutAWatch asserts the item of
// the first List goes to Observe and a satisfied one returns with no watch.
func TestWaitObjectListItemThatSatisfiesEndsWithoutAWatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLoop(t)
		f.lists = []answer{{state: "done"}}
		res := runLoop(t.Context(), f, time.Minute, nil)
		require.NoError(t, res.err)
		assert.Equal(t, "list@0", f.sequence())
		assert.Equal(t, []WaitSource{SourceList}, res.sources)
		assert.Zero(t, res.elapsed)
	})
}

// TestWaitObjectReturnsOnAnEventThatSatisfies asserts an event that
// satisfies ends the wait at once, with no further call, and that the stream
// is cancelled.
func TestWaitObjectReturnsOnAnEventThatSatisfies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		withStreamSeconds(t, 55)
		f := newFakeLoop(t)
		f.streams = []streamScript{{steps: []streamStep{{wait: 2 * time.Second, ev: changed("pending")}, {wait: time.Second, ev: changed("done")}}, hold: true}}
		res := runLoop(t.Context(), f, time.Minute, nil)
		require.NoError(t, res.err)
		assert.Equal(t, "list@0 watch@0", f.sequence())
		assert.Equal(t, 3*time.Second, res.elapsed)
		assert.Equal(t, []WaitSource{SourceList, SourceEvent, SourceEvent}, res.sources)
		require.Len(t, f.opened, 1)
		synctest.Wait()
		require.Error(t, f.opened[0].Err(), "the stream context is cancelled when the wait returns")
		f.requireInvariants(loopInterval)
	})
}

// TestWaitObjectTerminalErrorFromAnObjectEndsTheWait asserts an error from
// Observe ends the wait with it, from a List item, a Get answer and an event.
func TestWaitObjectTerminalErrorFromAnObjectEndsTheWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLoop(t)
		f.lists = []answer{{state: "bad"}}
		require.ErrorIs(t, runLoop(t.Context(), f, time.Minute, nil).err, errTerminal)
		assert.Equal(t, "list@0", f.sequence())
	})
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLoop(t)
		f.streams = []streamScript{{steps: []streamStep{{ev: changed("bad")}}, hold: true}}
		require.ErrorIs(t, runLoop(t.Context(), f, time.Minute, nil).err, errTerminal)
		assert.Equal(t, "list@0 watch@0", f.sequence())
	})
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLoop(t)
		f.gets = []answer{{state: "bad"}}
		assert.ErrorIs(t, runLoop(t.Context(), getOnly{f: f}, time.Minute, nil).err, errTerminal)
	})
}

// loopSpacing is one set of the loop's timing parameters: the interval and
// where its cycles are measured from.
type loopSpacing struct {
	name     string
	interval time.Duration
	from     SpacingFrom
	// timeout is the whole wait. With 2 second cycles it is 37 seconds, not 38:
	// a call due at the deadline instant races the cancellation of the
	// wait's context, so the calls would differ from run to run.
	timeout time.Duration
}

// loopSpacings are the parameters of the generated wait and of the OIDC client
// waits.
var loopSpacings = []loopSpacing{
	{"5 seconds from the return", loopInterval, SpacingFromReturn, 38 * time.Second},
	{"2 seconds from the start", 2 * time.Second, SpacingFromStart, 37 * time.Second},
}

func (s loopSpacing) apply(p *WaitParams) {
	p.Interval, p.SpacingFrom = s.interval, s.from
}

// callsEvery is "kind@from kind@from+step ..." up to and including to, in seconds.
func callsEvery(kind string, from, step, to int) string {
	var out []string
	for at := from; at <= to; at += step {
		out = append(out, fmt.Sprintf("%s@%d", kind, at))
	}
	return strings.Join(out, " ")
}

// cycles is a list and a watch at each of from, from+step ... up to to.
func cycles(from, step, to int) string {
	var out []string
	for at := from; at <= to; at += step {
		out = append(out, fmt.Sprintf("list@%d watch@%d", at, at))
	}
	return strings.Join(out, " ")
}

// TestWaitObjectStreamEndings asserts the calls of a watched wait against the
// ways a stream or an open can end, each with both sets of timing parameters
// and a timeout of 38 seconds, or 37 for the 2 second interval: the request
// budget keeps the calls within the bound, and the third failed watch in a row
// makes the wait poll.
// The expected sequences follow from the budget rule: the next cycle starts
// the interval times the calls of the previous cycle after the first call of
// that cycle returned or started.
func TestWaitObjectStreamEndings(t *testing.T) {
	const (
		threeShort5 = "list@0 watch@0 list@10 watch@10 list@20 watch@20 get@30 get@35"
		threeShort2 = "list@0 watch@0 list@4 watch@4 list@8 watch@8 get@12 get@14 get@16 get@18 get@20 get@22 get@24 get@26 get@28 get@30 get@32 get@34 get@36"
	)
	openRefused5 := "list@0 watch@0 get@10 get@15 get@20 get@25 get@30 get@35"
	openRefused2 := "list@0 watch@0 " + callsEvery("get", 4, 2, 36)
	listRefused5 := "list@0 get@0 get@10 get@15 get@20 get@25 get@30 get@35"
	listRefused2 := "list@0 get@0 " + callsEvery("get", 4, 2, 36)
	tests := []struct {
		name         string
		client       func(f *fakeLoop)
		want5, want2 string
	}{
		{"200, one ERROR, close", func(f *fakeLoop) {
			f.streams = []streamScript{{steps: []streamStep{{ev: failedEvent(statusErr(http.StatusGone))}}, hold: true}}
		}, threeShort5, threeShort2},
		{"200, one bookmark, close", func(f *fakeLoop) {
			f.streams = []streamScript{{steps: []streamStep{{ev: ObjectEvent{Type: ObjectBookmark}}}}}
		}, threeShort5, threeShort2},
		{"200, close at once", func(f *fakeLoop) {
			f.streams = []streamScript{{}}
		}, threeShort5, threeShort2},
		{"open 500", func(f *fakeLoop) {
			f.streams = []streamScript{{err: statusErr(http.StatusInternalServerError)}}
		}, threeShort5, threeShort2},
		{"open with no answer", func(f *fakeLoop) {
			f.streams = []streamScript{{err: clierr.Retryable(clierr.CodeGeneric, "context deadline exceeded")}}
		}, threeShort5, threeShort2},
		{"open 403", func(f *fakeLoop) {
			f.streams = []streamScript{{err: rbacErr()}}
		}, openRefused5, openRefused2},
		{"open 405", func(f *fakeLoop) {
			f.streams = []streamScript{{err: statusErr(http.StatusMethodNotAllowed)}}
		}, openRefused5, openRefused2},
		{"open 400", func(f *fakeLoop) {
			f.streams = []streamScript{{err: statusErr(http.StatusBadRequest)}}
		}, openRefused5, openRefused2},
		{"open unavailable", func(f *fakeLoop) {
			f.streams = []streamScript{{err: ErrWatchUnavailable}}
		}, "list@0 watch@0 get@5 get@10 get@15 get@20 get@25 get@30 get@35", "list@0 watch@0 " + callsEvery("get", 2, 2, 36)},
		{"List unavailable", func(f *fakeLoop) {
			f.lists = []answer{{err: ErrWatchUnavailable}}
		}, "list@0 get@0 get@5 get@10 get@15 get@20 get@25 get@30 get@35", "list@0 get@0 " + callsEvery("get", 2, 2, 36)},
		{"List 400, the server refused the field selector", func(f *fakeLoop) {
			f.lists = []answer{{err: statusErr(http.StatusBadRequest)}}
		}, listRefused5, listRefused2},
		{"List unusable (no resourceVersion or several items)", func(f *fakeLoop) {
			f.lists = []answer{{err: fmt.Errorf("%w: the server listed 2 objects", ErrListUnusable)}}
		}, listRefused5, listRefused2},
		{"List 405", func(f *fakeLoop) {
			f.lists = []answer{{err: statusErr(http.StatusMethodNotAllowed)}}
		}, listRefused5, listRefused2},
		{"List 403, the Get follows at once", func(f *fakeLoop) {
			f.lists = []answer{{err: rbacErr()}}
		}, listRefused5, listRefused2},
		{"List 500", func(f *fakeLoop) {
			f.lists = []answer{{err: statusErr(http.StatusInternalServerError)}}
		}, "list@0 list@5 list@10 list@15 list@20 list@25 list@30 list@35", callsEvery("list", 0, 2, 36)},
		{"empty List, the Get finds the object", func(f *fakeLoop) {
			f.lists = []answer{{none: true}, {state: "pending"}}
		}, "list@0 get@0 list@10 watch@10", "list@0 get@0 list@4 watch@4"},
		{"List 404, the Get finds the object", func(f *fakeLoop) {
			f.lists = []answer{{err: clierr.New(clierr.CodeNotFound, "gone")}, {state: "pending"}}
		}, "list@0 get@0 list@10 watch@10", "list@0 get@0 list@4 watch@4"},
		{"events, then close", func(f *fakeLoop) {
			f.streams = []streamScript{{steps: []streamStep{{wait: time.Second, ev: changed("pending")}}, length: time.Second}}
		}, "list@0 watch@0 list@10 watch@10 list@20 watch@20 list@30 watch@30", cycles(0, 4, 36)},
		{"Deleted, then the object comes back", func(f *fakeLoop) {
			f.streams = []streamScript{{steps: []streamStep{{ev: ObjectEvent{Type: ObjectDeleted, Object: loopObject("pending", "event-rv")}}}, hold: true}}
		}, "list@0 watch@0 list@10 watch@10 list@20 watch@20 list@30 watch@30", cycles(0, 4, 36)},
	}
	for _, sp := range loopSpacings {
		for _, tt := range tests {
			t.Run(sp.name+"/"+tt.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					withStreamSeconds(t, 55)
					f := newFakeLoop(t)
					tt.client(f)
					res := runLoop(t.Context(), f, sp.timeout, sp.apply)
					var to *timedOut
					require.ErrorAs(t, res.err, &to)
					want := tt.want5
					if sp.interval != loopInterval {
						want = tt.want2
					}
					assert.Equal(t, want, f.sequence())
					f.requireInvariants(sp.interval)
				})
			})
		}
	}
}

// TestWaitObjectSpacesWatchedCyclesFromTheReturnOrTheStart asserts that with a
// List that takes 1.5 seconds and a watch that cannot open, a watched wait
// with a 2 second interval measures its cycles from the start of the List or
// from its return: the next cycle starts 4 seconds after the first call
// started, or 4 seconds after it returned.
func TestWaitObjectSpacesWatchedCyclesFromTheReturnOrTheStart(t *testing.T) {
	for _, tt := range []struct {
		name string
		from SpacingFrom
		want string
	}{
		{"from the start", SpacingFromStart, "list@0 watch@1.5 list@4 watch@5.5 list@8 watch@9.5"},
		{"from the return", SpacingFromReturn, "list@0 watch@1.5 list@5.5 watch@7"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFakeLoop(t)
				f.listTakes = 1500 * time.Millisecond
				f.streams = []streamScript{{err: statusErr(http.StatusInternalServerError)}}
				res := runLoop(t.Context(), f, 10*time.Second, func(p *WaitParams) {
					p.Interval, p.SpacingFrom = 2*time.Second, tt.from
				})
				var to *timedOut
				require.ErrorAs(t, res.err, &to)
				assert.Equal(t, tt.want, f.sequence())
			})
		})
	}
}

// TestWaitObjectFailedWatchesNeedToBeInARow asserts a stream that delivered an
// event resets the count of failed watches, so alternating short and
// delivering streams never make the wait poll.
func TestWaitObjectFailedWatchesNeedToBeInARow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		withStreamSeconds(t, 55)
		f := newFakeLoop(t)
		short := streamScript{}
		delivers := streamScript{steps: []streamStep{{ev: changed("pending")}}}
		f.streams = []streamScript{short, short, delivers, short, short, delivers}
		res := runLoop(t.Context(), f, 58*time.Second, nil)
		var to *timedOut
		require.ErrorAs(t, res.err, &to)
		assert.Equal(t, "list@0 watch@0 list@10 watch@10 list@20 watch@20 list@30 watch@30 list@40 watch@40 list@50 watch@50", f.sequence())
		f.requireInvariants(loopInterval)
	})
}

// TestWaitObjectHealthyStreamsRunTheirLength asserts a stream that lasts its
// timeoutSeconds is followed by the next cycle at once, and that the last
// stream asks for the seconds the wait has left.
func TestWaitObjectHealthyStreamsRunTheirLength(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		withStreamSeconds(t, 55)
		f := newFakeLoop(t)
		f.streams = []streamScript{{length: 55 * time.Second}}
		res := runLoop(t.Context(), f, 120*time.Second, nil)
		var to *timedOut
		require.ErrorAs(t, res.err, &to)
		assert.Equal(t, "list@0 watch@0 list@55 watch@55 list@110 watch@110", f.sequence())
		var asked []time.Duration
		for _, c := range f.calls {
			if c.kind == "watch" {
				asked = append(asked, c.timeout)
			}
		}
		assert.Equal(t, []time.Duration{55 * time.Second, 55 * time.Second, 10 * time.Second}, asked)
		f.requireInvariants(loopInterval)
	})
}

// TestWaitObjectStreamLengthIsRandomAndBounded asserts the seconds a watch
// asks for come from 50 to 60 and never exceed the seconds left, rounded up,
// with 1 second at least.
func TestWaitObjectStreamLengthIsRandomAndBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLoop(t)
		f.streams = []streamScript{{length: 51 * time.Second}}
		runLoop(t.Context(), f, 600*time.Second, nil)
		seen := map[time.Duration]bool{}
		for _, c := range f.calls {
			if c.kind == "watch" {
				if 600*time.Second-c.at >= 60*time.Second {
					assert.GreaterOrEqual(t, c.timeout, 50*time.Second)
				}
				assert.LessOrEqual(t, c.timeout, 60*time.Second)
				seen[c.timeout] = true
			}
		}
		assert.Greater(t, len(seen), 1, "the length is drawn at random")
	})
	for _, tt := range []struct {
		timeout time.Duration
		want    time.Duration
	}{
		{20 * time.Second, 20 * time.Second},
		{2500 * time.Millisecond, 3 * time.Second},
		{100 * time.Millisecond, time.Second},
	} {
		t.Run(tt.timeout.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFakeLoop(t)
				runLoop(t.Context(), f, tt.timeout, nil)
				require.Len(t, f.calls, 2)
				assert.Equal(t, tt.want, f.calls[1].timeout)
			})
		})
	}
}

// TestWaitObjectEmptyListThenNotFoundReturnsTheGetError asserts an empty List
// is followed by a Get in the same cycle, and a NOT_FOUND from it is the
// error the wait returns, as it is.
func TestWaitObjectEmptyListThenNotFoundReturnsTheGetError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLoop(t)
		notFound := clierr.New(clierr.CodeNotFound, "x not found")
		f.lists = []answer{{none: true}}
		f.gets = []answer{{err: notFound}}
		res := runLoop(t.Context(), f, time.Minute, nil)
		assert.Same(t, notFound, res.err)
		assert.Equal(t, "list@0 get@0", f.sequence())
	})
}

// TestWaitObjectDeletedThenEmptyListThenNotFound asserts a Deleted event
// ends the stream, and the next cycle's empty List and Get decide: NOT_FOUND
// ends the wait up to two intervals after the List.
func TestWaitObjectDeletedThenEmptyListThenNotFound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		withStreamSeconds(t, 55)
		f := newFakeLoop(t)
		notFound := clierr.New(clierr.CodeNotFound, "x not found")
		f.lists = []answer{{state: "pending"}, {none: true}}
		f.gets = []answer{{err: notFound}}
		f.streams = []streamScript{{steps: []streamStep{{wait: time.Second, ev: ObjectEvent{Type: ObjectDeleted, Object: loopObject("pending", "event-rv")}}}, hold: true}}
		res := runLoop(t.Context(), f, time.Minute, nil)
		assert.Same(t, notFound, res.err)
		assert.Equal(t, "list@0 watch@0 list@10 get@10", f.sequence())
	})
}

// TestWaitObjectAnEventWithAFailFastStateClosesTheStream asserts that
// WaitClose closes the stream without ending the wait, and that the next
// cycle's List starts no sooner than one interval after the event, even when
// the budget would allow it sooner, and that its item decides.
func TestWaitObjectAnEventWithAFailFastStateClosesTheStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		withStreamSeconds(t, 55)
		f := newFakeLoop(t)
		f.lists = []answer{{state: "pending"}, {state: "done"}}
		f.streams = []streamScript{{steps: []streamStep{{wait: 8 * time.Second, ev: changed("close")}}, hold: true}}
		res := runLoop(t.Context(), f, time.Minute, nil)
		require.NoError(t, res.err)
		assert.Equal(t, "list@0 watch@0 list@13", f.sequence())
		assert.Equal(t, []WaitSource{SourceList, SourceEvent, SourceList}, res.sources)
		f.requireInvariants(loopInterval)
	})
	synctest.Test(t, func(t *testing.T) {
		withStreamSeconds(t, 55)
		f := newFakeLoop(t)
		f.lists = []answer{{state: "pending"}, {state: "done"}}
		f.streams = []streamScript{{steps: []streamStep{{wait: time.Second, ev: changed("close")}}, hold: true}}
		res := runLoop(t.Context(), f, time.Minute, nil)
		require.NoError(t, res.err)
		assert.Equal(t, "list@0 watch@0 list@10", f.sequence(), "the budget alone sets the next List when it is later")
	})
}

// TestWaitObjectAbortsAtOnceOnAuthAndTLSFailures asserts a 401 or a
// certificate failure from a List, a Get, a watch open and an ERROR event
// ends the wait at once through OnAbort, with no further call, with both sets
// of timing parameters.
func TestWaitObjectAbortsAtOnceOnAuthAndTLSFailures(t *testing.T) {
	for _, bad := range []error{authErr(), clierr.New(clierr.CodeTLSTrust, "the server certificate is not trusted")} {
		tests := []struct {
			name   string
			client func(f *fakeLoop) ClusterClient
			want   string
		}{
			{"List", func(f *fakeLoop) ClusterClient { f.lists = []answer{{err: bad}}; return f }, "list@0"},
			{"Get", func(f *fakeLoop) ClusterClient { f.gets = []answer{{err: bad}}; return getOnly{f: f} }, "get@0"},
			{"Get after an empty List", func(f *fakeLoop) ClusterClient {
				f.lists, f.gets = []answer{{none: true}}, []answer{{err: bad}}
				return f
			}, "list@0 get@0"},
			{"open", func(f *fakeLoop) ClusterClient { f.streams = []streamScript{{err: bad}}; return f }, "list@0 watch@0"},
			{"ERROR event", func(f *fakeLoop) ClusterClient {
				f.streams = []streamScript{{steps: []streamStep{{ev: failedEvent(bad)}}, hold: true}}
				return f
			}, "list@0 watch@0"},
		}
		for _, sp := range loopSpacings {
			for _, tt := range tests {
				t.Run(sp.name+"/"+clierr.From(bad).Code+" "+tt.name, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						f := newFakeLoop(t)
						c := tt.client(f)
						aborted := false
						res := runLoop(t.Context(), c, time.Minute, func(p *WaitParams) {
							sp.apply(p)
							p.OnAbort = func(err error) error { aborted = true; return err }
						})
						assert.Same(t, bad, res.err)
						assert.True(t, aborted)
						assert.Equal(t, tt.want, f.sequence())
						assert.Zero(t, res.elapsed, "the wait ends at once")
					})
				})
			}
		}
	}
}

// TestWaitObjectTimeoutCarriesTheLastObjectAndError asserts OnTimeout gets
// the last object from a List, a Get or an event, and the last List or Get
// error that RecordErr kept; an object clears the error.
func TestWaitObjectTimeoutCarriesTheLastObjectAndError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		withStreamSeconds(t, 55)
		f := newFakeLoop(t)
		f.lists = []answer{{err: statusErr(http.StatusInternalServerError)}, {state: "pending"}}
		f.streams = []streamScript{{steps: []streamStep{{wait: time.Second, ev: changed("later")}}, hold: true}}
		res := runLoop(t.Context(), f, 30*time.Second, nil)
		var to *timedOut
		require.ErrorAs(t, res.err, &to)
		require.NotNil(t, to.last)
		state, _, _ := unstructured.NestedString(to.last.Object, "status", "state")
		assert.Equal(t, "later", state, "the last object is the event's")
		assert.Nil(t, to.lastErr, "an object clears the last error")
	})
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLoop(t)
		f.lists = []answer{{err: statusErr(http.StatusInternalServerError)}}
		res := runLoop(t.Context(), f, 7*time.Second, nil)
		var to *timedOut
		require.ErrorAs(t, res.err, &to)
		assert.Nil(t, to.last)
		require.NotNil(t, to.lastErr)
		assert.Equal(t, clierr.CodeGeneric, to.lastErr.Code)
	})
}

// TestWaitObjectRecordErrDecidesTheLastError asserts an error RecordErr
// refuses is not carried to the timeout.
func TestWaitObjectRecordErrDecidesTheLastError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLoop(t)
		f.lists = []answer{{err: statusErr(http.StatusInternalServerError)}}
		res := runLoop(t.Context(), f, 7*time.Second, func(p *WaitParams) {
			p.RecordErr = func(context.Context, error) bool { return false }
		})
		var to *timedOut
		require.ErrorAs(t, res.err, &to)
		assert.Nil(t, to.lastErr)
	})
}

// TestWaitObjectCancelAndTimeoutUseTheirOwnHooks asserts a cancelled
// context goes to OnCancel and a deadline to OnTimeout, in a stream and in
// a wait between cycles.
func TestWaitObjectCancelAndTimeoutUseTheirOwnHooks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLoop(t)
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			time.Sleep(3 * time.Second)
			cancel()
		}()
		res := runLoop(ctx, f, time.Minute, nil)
		require.ErrorIs(t, res.err, context.Canceled)
		assert.Equal(t, 3*time.Second, res.elapsed)
		synctest.Wait()
	})
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLoop(t)
		f.streams = []streamScript{{err: statusErr(http.StatusInternalServerError)}}
		res := runLoop(t.Context(), f, 4*time.Second, nil)
		var to *timedOut
		require.ErrorAs(t, res.err, &to)
		assert.Equal(t, 4*time.Second, res.elapsed)
	})
}

// blockingList is a fake whose List blocks until its context ends.
type blockingList struct{ fakeLoop }

func (b *blockingList) ListObject(ctx context.Context, _ schema.GroupVersionResource, _, _ string) (*unstructured.Unstructured, string, error) {
	<-ctx.Done()
	return nil, "", clierr.Retryable(clierr.CodeGeneric, "context deadline exceeded")
}

// TestWaitObjectEndsWhenARequestOutlastsTheDeadline asserts a List that
// blocks until the wait's deadline ends the wait at the deadline, with no
// error recorded for the cut call.
func TestWaitObjectEndsWhenARequestOutlastsTheDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := &blockingList{fakeLoop: *newFakeLoop(t)}
		res := runLoop(t.Context(), b, 9*time.Second, nil)
		var to *timedOut
		require.ErrorAs(t, res.err, &to)
		assert.Equal(t, 9*time.Second, res.elapsed)
		assert.Nil(t, to.lastErr)
	})
}

// TestWaitObjectForbiddenListThenSatisfiedGetReturnsAtOnce asserts a user
// with get only, whose List answers 400, 403 or 405, is unusable or sends nothing, gets the
// first Get in the same cycle: a satisfied object returns with no delay,
// also with a timeout shorter than one interval.
func TestWaitObjectForbiddenListThenSatisfiedGetReturnsAtOnce(t *testing.T) {
	for name, err := range map[string]error{
		"400":         statusErr(http.StatusBadRequest),
		"unusable":    fmt.Errorf("%w: the server listed 2 objects", ErrListUnusable),
		"403":         rbacErr(),
		"405":         statusErr(http.StatusMethodNotAllowed),
		"unavailable": ErrWatchUnavailable,
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFakeLoop(t)
				f.lists = []answer{{err: err}}
				f.gets = []answer{{state: "done"}}
				res := runLoop(t.Context(), f, time.Second, nil)
				require.NoError(t, res.err)
				assert.Equal(t, "list@0 get@0", f.sequence())
				assert.Zero(t, res.elapsed)
				assert.Equal(t, []WaitSource{SourceGet}, res.sources)
			})
		})
	}
}

// observation is one call of Observe: where the object came from and when the
// call started.
type observation struct {
	source WaitSource
	at     time.Duration
}

func (o observation) String() string {
	name := map[WaitSource]string{SourceList: "list", SourceGet: "get", SourceEvent: "event", SourceCheckAgain: "again"}[o.source]
	return fmt.Sprintf("%s@%g", name, o.at.Seconds())
}

// checkAgainCase is one wait whose Observe asks to be called again. The
// object's state decides what a List item, a Get answer and an event give:
// "again" is WaitCheckAgain, "done" is WaitDone, "bad" is an error and
// anything else WaitContinue. A call with SourceCheckAgain is answered by
// again; when it is nil the call asks again.
type checkAgainCase struct {
	name         string
	client       func(f *fakeLoop)
	getOnly      bool
	timeout      time.Duration
	again        func(nth int, at time.Duration) (WaitOutcome, error)
	takes        time.Duration
	wantObserved string
	wantCalls    string
	// wantEnd is "done", "timeout" or "error".
	wantEnd string
}

func (c checkAgainCase) run(t *testing.T) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		const interval = 2 * time.Second
		withStreamSeconds(t, 55)
		f := newFakeLoop(t)
		f.lists = []answer{{state: "again"}}
		if c.client != nil {
			c.client(f)
		}
		var client ClusterClient = f
		if c.getOnly {
			client = getOnly{f: f}
		}
		var seen []observation
		nth := 0
		res := runLoop(t.Context(), client, c.timeout, func(p *WaitParams) {
			p.Interval, p.SpacingFrom = interval, SpacingFromReturn
			p.Observe = func(_ context.Context, obj *unstructured.Unstructured, source WaitSource) (WaitOutcome, error) {
				at := f.since()
				seen = append(seen, observation{source, at})
				if source == SourceCheckAgain {
					nth++
					if c.takes > 0 {
						time.Sleep(c.takes)
					}
					if c.again != nil {
						return c.again(nth, at)
					}
					return WaitCheckAgain, nil
				}
				state, _, _ := unstructured.NestedString(obj.Object, "status", "state")
				switch state {
				case "again":
					return WaitCheckAgain, nil
				case "done":
					return WaitDone, nil
				case "bad":
					return WaitContinue, errTerminal
				}
				return WaitContinue, nil
			}
		})
		var got []string
		again := 0
		var previous time.Duration
		for _, o := range seen {
			got = append(got, o.String())
			if o.source == SourceCheckAgain {
				if again > 0 {
					assert.GreaterOrEqual(t, o.at-previous, interval, "calls of Observe that check again are at least one interval apart: %v", got)
				}
				previous = o.at
				again++
			}
		}
		assert.Equal(t, c.wantObserved, strings.Join(got, " "))
		assert.Equal(t, c.wantCalls, f.sequence())
		assert.LessOrEqual(t, again, int(res.elapsed/interval), "at most one Observe call that checks again per interval")
		switch c.wantEnd {
		case "done":
			require.NoError(t, res.err)
		case "timeout":
			var to *timedOut
			require.ErrorAs(t, res.err, &to)
			assert.NotNil(t, to.last, "the timeout carries the last object")
		case "error":
			require.ErrorIs(t, res.err, errTerminal)
		}
		if !c.getOnly {
			f.requireInvariants(interval)
		}
	})
}

func doneAt(nth int) func(int, time.Duration) (WaitOutcome, error) {
	return func(n int, _ time.Duration) (WaitOutcome, error) {
		if n >= nth {
			return WaitDone, nil
		}
		return WaitCheckAgain, nil
	}
}

// TestWaitObjectCheckAgain asserts the rules of WaitCheckAgain with a 2 second
// interval: Observe is called again with the same object once an interval
// while the stream is open, the call is outside the request budget, and the
// run of calls ends with the stream, with an event that has its own outcome,
// with an error or with the deadline.
func TestWaitObjectCheckAgain(t *testing.T) {
	for _, c := range []checkAgainCase{
		{
			name: "calls every interval while the stream is open and ends with done", timeout: time.Minute,
			again:        doneAt(3),
			wantObserved: "list@0 again@2 again@4 again@6", wantCalls: "list@0 watch@0", wantEnd: "done",
		},
		{
			name: "stops when the stream closes", timeout: 12 * time.Second,
			client: func(f *fakeLoop) {
				f.lists = []answer{{state: "again"}, {state: "pending"}}
				f.streams = []streamScript{{length: 5 * time.Second}}
			},
			wantObserved: "list@0 again@2 again@4 list@5 list@10",
			wantCalls:    "list@0 watch@0 list@5 watch@5 list@10 watch@10", wantEnd: "timeout",
		},
		{
			name: "an event that continues ends it and an event that asks again starts a new one", timeout: time.Minute,
			client: func(f *fakeLoop) {
				f.streams = []streamScript{{steps: []streamStep{{wait: 3 * time.Second, ev: changed("pending")}, {wait: 4 * time.Second, ev: changed("again")}}, hold: true}}
			},
			again: func(_ int, at time.Duration) (WaitOutcome, error) {
				if at >= 9*time.Second {
					return WaitDone, nil
				}
				return WaitCheckAgain, nil
			},
			wantObserved: "list@0 again@2 event@3 event@7 again@9", wantCalls: "list@0 watch@0", wantEnd: "done",
		},
		{
			name: "the outcome of a List item starts it when the stream opens", timeout: time.Minute,
			client: func(f *fakeLoop) {
				f.listTakes, f.watchTakes = 1500*time.Millisecond, time.Second
			},
			again:        doneAt(1),
			wantObserved: "list@1.5 again@4.5", wantCalls: "list@0 watch@1.5", wantEnd: "done",
		},
		{
			name: "an error ends the wait with it", timeout: time.Minute,
			again: func(int, time.Duration) (WaitOutcome, error) {
				return WaitContinue, errTerminal
			},
			wantObserved: "list@0 again@2", wantCalls: "list@0 watch@0", wantEnd: "error",
		},
		{
			name: "a timeout between calls carries the last object", timeout: 5 * time.Second,
			wantObserved: "list@0 again@2 again@4", wantCalls: "list@0 watch@0", wantEnd: "timeout",
		},
		{
			name: "a client that cannot watch is polled and never called again", timeout: time.Minute, getOnly: true,
			client: func(f *fakeLoop) {
				f.gets = []answer{{state: "again"}, {state: "again"}, {state: "done"}}
			},
			wantObserved: "get@0 get@2 get@4", wantCalls: "get@0 get@2 get@4", wantEnd: "done",
		},
		{
			name: "a watch the server refuses leaves a wait that polls and never calls again", timeout: 9 * time.Second,
			client: func(f *fakeLoop) {
				f.streams = []streamScript{{err: rbacErr()}}
				f.gets = []answer{{state: "pending"}}
			},
			wantObserved: "list@0 get@4 get@6 get@8", wantCalls: "list@0 watch@0 get@4 get@6 get@8", wantEnd: "timeout",
		},
		{
			name: "a bookmark does not end it", timeout: time.Minute,
			client: func(f *fakeLoop) {
				f.streams = []streamScript{{steps: []streamStep{{wait: 3 * time.Second, ev: ObjectEvent{Type: ObjectBookmark}}}, hold: true}}
			},
			again:        doneAt(3),
			wantObserved: "list@0 again@2 again@4 again@6", wantCalls: "list@0 watch@0", wantEnd: "done",
		},
		{
			name: "a watch that fails to open starts none and the pending call is dropped at the next cycle", timeout: 10 * time.Second,
			client: func(f *fakeLoop) {
				f.lists = []answer{{state: "again"}, {state: "pending"}}
				f.streams = []streamScript{{err: statusErr(http.StatusInternalServerError)}, {hold: true}}
			},
			wantObserved: "list@0 list@4", wantCalls: "list@0 watch@0 list@4 watch@4", wantEnd: "timeout",
		},
		{
			name: "a Get answer starts none, no stream is open", timeout: 10 * time.Second,
			client: func(f *fakeLoop) {
				f.lists = []answer{{none: true}, {state: "pending"}}
				f.gets = []answer{{state: "again"}}
			},
			wantObserved: "get@0 list@4", wantCalls: "list@0 get@0 list@4 watch@4", wantEnd: "timeout",
		},
		{
			name: "the interval runs from the return of the previous call", timeout: time.Minute, takes: time.Second,
			again:        doneAt(2),
			wantObserved: "list@0 again@2 again@5", wantCalls: "list@0 watch@0", wantEnd: "done",
		},
	} {
		t.Run(c.name, c.run)
	}
}

// TestWaitObjectCheckAgainEndsWithTheStreamCancelled asserts that when the
// wait returns during a run of Observe calls that check again, the stream's
// context is cancelled.
func TestWaitObjectCheckAgainEndsWithTheStreamCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		withStreamSeconds(t, 55)
		f := newFakeLoop(t)
		f.lists = []answer{{state: "again"}}
		n := 0
		res := runLoop(t.Context(), f, time.Minute, func(p *WaitParams) {
			p.Interval = 2 * time.Second
			p.Observe = func(context.Context, *unstructured.Unstructured, WaitSource) (WaitOutcome, error) {
				n++
				if n == 3 {
					return WaitDone, nil
				}
				return WaitCheckAgain, nil
			}
		})
		require.NoError(t, res.err)
		require.Len(t, f.opened, 1)
		synctest.Wait()
		require.Error(t, f.opened[0].Err(), "the stream context is cancelled when the wait returns")
	})
}
