package engine

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// WaitSource is where an object a wait looks at came from.
type WaitSource int

const (
	// SourceList is the item of a List by name.
	SourceList WaitSource = iota
	// SourceGet is the answer of a Get.
	SourceGet
	// SourceEvent is the object of a watch event.
	SourceEvent
	// SourceCheckAgain is the object of an earlier call, given to Observe again
	// because it returned WaitCheckAgain.
	SourceCheckAgain
)

// WaitOutcome is what a wait decided about one object.
type WaitOutcome int

const (
	// WaitContinue keeps waiting.
	WaitContinue WaitOutcome = iota
	// WaitDone ends the wait with success.
	WaitDone
	// WaitClose is for an event only: it closes the stream without ending the
	// wait, and the next cycle's List decides with every check.
	WaitClose
	// WaitCheckAgain keeps waiting, like WaitContinue, and asks the loop to call
	// Observe again with the same object every Interval while the current watch
	// stream is open: for a wait whose verdict depends on a request the watch
	// does not see.
	WaitCheckAgain
)

// SpacingFrom says where the loop measures the spacing of its cycles from.
type SpacingFrom int

const (
	// SpacingFromReturn measures from the return of a cycle's first request.
	SpacingFromReturn SpacingFrom = iota
	// SpacingFromStart measures from the start of a cycle's first request.
	SpacingFromStart
)

// WaitParams is what one wait gives WaitObject.
type WaitParams struct {
	GVR       schema.GroupVersionResource
	Namespace string
	Name      string
	// Timeout is the whole wait.
	Timeout time.Duration
	// Interval is the spacing of one call: the next cycle starts no sooner than
	// Interval times the calls the previous cycle sent, measured from the
	// return or the start of its first call (see SpacingFrom).
	Interval    time.Duration
	SpacingFrom SpacingFrom
	// Observe checks one object and stores the caller's result when it
	// returns WaitDone. A non-nil error ends the wait with it.
	Observe func(ctx context.Context, obj *unstructured.Unstructured, source WaitSource) (WaitOutcome, error)
	// RecordErr says whether a List or Get error becomes the last error the
	// timeout names.
	RecordErr func(ctx context.Context, err error) bool
	// OnAbort returns the error that ends the wait for an authentication or
	// certificate failure.
	OnAbort func(err error) error
	// OnCancel returns the error for a wait the caller cancelled.
	OnCancel func(ctx context.Context, last *unstructured.Unstructured, lastErr *clierr.Error) error
	// OnTimeout returns the error for a wait that ran out of time.
	OnTimeout func(last *unstructured.Unstructured, lastErr *clierr.Error) error
}

// failedWatchLimit is how many failed watches in a row make a wait poll.
const failedWatchLimit = 3

type streamRange struct{ lo, hi int }

var streamSeconds atomic.Pointer[streamRange]

// SetStreamSecondsForTest sets the range, in whole seconds, the length of a
// watch is drawn from, and returns a restore func. In non-test code so other
// packages' tests can reach it, as provider.SetRequestTimeoutForTest is.
func SetStreamSecondsForTest(lo, hi int) (restore func()) {
	old := streamSeconds.Swap(&streamRange{lo: lo, hi: max(lo, hi)})
	return func() { streamSeconds.Store(old) }
}

// streamLength is the timeoutSeconds of the next watch: a random whole number
// from 50 to 60, less when the wait has less time left (rounded up, at least
// 1), so processes started together do not reconnect together.
func streamLength(ctx context.Context) time.Duration {
	r := streamSeconds.Load()
	if r == nil {
		r = &streamRange{lo: 50, hi: 60}
	}
	secs := r.lo + rand.IntN(r.hi-r.lo+1)
	if deadline, ok := ctx.Deadline(); ok {
		left := int(math.Ceil(time.Until(deadline).Seconds()))
		secs = min(secs, max(left, 1))
	}
	return time.Duration(max(secs, 1)) * time.Second
}

// refusesWatch reports whether err says the server will not list or watch the
// object for this caller or this resource: a 403, a 405, a 400 (the server
// refused the field selector or a watch parameter), or a client that offers
// no watch at run time, or a List that holds no resourceVersion or several
// items (ErrListUnusable). Polling is the safe path for each.
func refusesWatch(err error) bool {
	if errors.Is(err, ErrWatchUnavailable) || errors.Is(err, ErrListUnusable) {
		return true
	}
	if ce := clierr.From(err); ce != nil && ce.Code == clierr.CodeRBAC {
		return true
	}
	status, ok := clierr.HTTPStatus(err)
	return ok && (status == http.StatusForbidden || status == http.StatusMethodNotAllowed || status == http.StatusBadRequest)
}

// The phases of the loop. Each pass of the loop sends at most one request or
// waits once.
type waitPhase int

const (
	phaseList waitPhase = iota
	phaseGet
	phaseOpen
	phaseStream
	phaseSleep
)

// WaitObject waits for one object until Observe returns WaitDone, Observe or
// an abort ends it with an error, a Get answers NOT_FOUND, or Timeout ends.
//
// A client that implements ObjectWatcher is watched: a cycle Lists the object
// by name and watches from the list's resourceVersion until the stream ends.
// Any other client, a 400, 403 or 405 on the List or the watch, a List with no
// resourceVersion or with several items, or 3 failed watches in a row make the
// wait poll with Get, one Get a cycle, for the rest of the call. Every object the loop sees goes to Observe.
//
// A cycle sends at most 2 calls, and the next cycle starts no sooner than
// Interval times the calls the previous one sent, measured from where
// SpacingFrom says, so by any time T the cycles have sent at most
// floor(T / Interval) + 2 calls whatever the server does.
//
// When Observe returns WaitCheckAgain for a List item or an event, the loop
// calls Observe again with that object once an Interval after the outcome or
// after the stream opened, and again after each call that returns
// WaitCheckAgain, while the stream is open. Those calls send no request of the
// loop and are outside the bound above. An event, the end of the stream or an
// outcome other than WaitCheckAgain ends them. A Get answer starts none, and
// a poll never makes one.
//
// The loop keeps every client call, the event receive, the timer and
// WaitShouldStop in the body of its one for statement, with one select, so the
// scan in test/conformance that every wait loop asks whether to stop sees
// them.
func WaitObject(ctx context.Context, client ClusterClient, p WaitParams) error {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	ow, watching := client.(ObjectWatcher)
	var (
		last    *unstructured.Unstructured
		lastErr *clierr.Error
		phase   waitPhase

		calls int       // calls the cycle has sent
		ref   time.Time // where the cycle's spacing is measured from

		listRV  string
		failed  int
		events  <-chan ObjectEvent
		timer   *time.Timer
		timerC  <-chan time.Time
		sleep   time.Duration // how long the next cycle waits to start
		started time.Time     // the stream's response headers
		got     bool          // the stream delivered a Changed or Deleted event
		notNext time.Time     // the next cycle starts no sooner than this

		again      *unstructured.Unstructured // the object Observe asked to be called with again
		againTimer *time.Timer
		againC     <-chan time.Time

		streamCancel context.CancelFunc = func() {}
	)
	defer func() {
		streamCancel()
		if timer != nil {
			timer.Stop()
		}
		if againTimer != nil {
			againTimer.Stop()
		}
	}()

	beginCycle := func() {
		calls, ref, notNext = 0, time.Time{}, time.Time{}
		phase = phaseGet
		if watching {
			phase = phaseList
		}
	}
	sent := func(before time.Time) {
		calls++
		if ref.IsZero() {
			ref = time.Now()
			if p.SpacingFrom == SpacingFromStart {
				ref = before
			}
		}
	}
	dropAgain := func() {
		again, againC = nil, nil
		if againTimer != nil {
			againTimer.Stop()
			againTimer = nil
		}
	}
	endCycle := func() {
		dropAgain()
		if ref.IsZero() {
			ref = time.Now()
		}
		next := ref.Add(time.Duration(calls) * p.Interval)
		if notNext.After(next) {
			next = notNext
		}
		if sleep = time.Until(next); sleep > 0 {
			phase = phaseSleep
			return
		}
		beginCycle()
	}
	closeStream := func() {
		streamCancel()
		events = nil
		if !got && time.Since(started) < 2*p.Interval {
			failed++
			if failed >= failedWatchLimit {
				watching = false
			}
		} else {
			failed = 0
		}
	}
	finish := func() error {
		if errors.Is(ctx.Err(), context.Canceled) {
			return p.OnCancel(ctx, last, lastErr)
		}
		return p.OnTimeout(last, lastErr)
	}

	beginCycle()
	for {
		if ctx.Err() != nil {
			return finish()
		}
		switch phase {
		case phaseList:
			before := time.Now()
			item, rv, err := ow.ListObject(ctx, p.GVR, p.Namespace, p.Name)
			if !errors.Is(err, ErrWatchUnavailable) {
				sent(before)
			}
			switch {
			case err == nil && item != nil:
				last, lastErr, listRV = item, nil, rv
				out, oerr := p.Observe(ctx, item, SourceList)
				if oerr != nil {
					return oerr
				}
				if out == WaitDone {
					return nil
				}
				if out == WaitCheckAgain {
					again = item
				}
				phase = phaseOpen
			case err == nil, clierr.IsNotFound(err):
				phase = phaseGet
			case WaitShouldStop(err):
				return p.OnAbort(err)
			case refusesWatch(err):
				watching = false
				phase = phaseGet
			default:
				if p.RecordErr(ctx, err) {
					lastErr = clierr.From(err)
				}
				endCycle()
			}
			continue
		case phaseGet:
			before := time.Now()
			obj, err := client.Get(ctx, p.GVR, p.Namespace, p.Name)
			sent(before)
			switch {
			case err != nil && clierr.IsNotFound(err):
				return err
			case err != nil && WaitShouldStop(err):
				return p.OnAbort(err)
			case err != nil:
				if p.RecordErr(ctx, err) {
					lastErr = clierr.From(err)
				}
			default:
				last, lastErr = obj, nil
				out, oerr := p.Observe(ctx, obj, SourceGet)
				if oerr != nil {
					return oerr
				}
				if out == WaitDone {
					return nil
				}
			}
			endCycle()
			continue
		case phaseOpen:
			streamCtx, cancelOpen := context.WithCancel(ctx)
			streamCancel = cancelOpen
			before := time.Now()
			ch, err := ow.WatchObject(streamCtx, p.GVR, p.Namespace, p.Name, listRV, streamLength(ctx))
			if !errors.Is(err, ErrWatchUnavailable) {
				sent(before)
			}
			if err != nil {
				streamCancel()
			}
			switch {
			case err == nil:
				events, started, got = ch, time.Now(), false
				phase = phaseStream
				continue
			case WaitShouldStop(err):
				return p.OnAbort(err)
			case refusesWatch(err):
				watching = false
			default:
				failed++
				if failed >= failedWatchLimit {
					watching = false
				}
			}
			endCycle()
			continue
		}

		if phase == phaseSleep && timerC == nil {
			timer = time.NewTimer(sleep)
			timerC = timer.C
		}
		if phase == phaseStream && again != nil && againC == nil {
			againTimer = time.NewTimer(p.Interval)
			againC = againTimer.C
		}
		select {
		case <-ctx.Done():
			return finish()
		case <-timerC:
			timer, timerC = nil, nil
			beginCycle()
		case <-againC:
			againTimer, againC = nil, nil
			out, oerr := p.Observe(ctx, again, SourceCheckAgain)
			if oerr != nil {
				return oerr
			}
			switch out {
			case WaitDone:
				return nil
			case WaitCheckAgain:
			default:
				again = nil
			}
		case ev, ok := <-events:
			switch {
			case !ok:
				closeStream()
				endCycle()
			case ev.Type == ObjectChanged:
				got = true
				last = ev.Object
				out, oerr := p.Observe(ctx, ev.Object, SourceEvent)
				if oerr != nil {
					return oerr
				}
				dropAgain()
				switch out {
				case WaitDone:
					return nil
				case WaitCheckAgain:
					again = ev.Object
				case WaitClose:
					closeStream()
					notNext = time.Now().Add(p.Interval)
					endCycle()
				}
			case ev.Type == ObjectDeleted:
				got = true
				closeStream()
				endCycle()
			case ev.Type == ObjectFailed:
				if WaitShouldStop(ev.Err) {
					return p.OnAbort(ev.Err)
				}
				closeStream()
				endCycle()
			}
		}
	}
}
