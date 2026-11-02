package telemetrysend

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/rancher/rancher-cli/internal/telemetry"
)

// The pacing of the attempts.
const (
	// attemptEvery is the least time between two attempts of one directory.
	attemptEvery = time.Hour
	// maxJitter bounds the random delay added to the next attempt after a
	// request that ended without a keep decision.
	maxJitter = 10 * time.Minute
	// maxWait is the longest wait between two attempts.
	maxWait = telemetry.MaxAttemptWait
	// maxCoveredIDs is the most record ids the state file keeps: the slot pool
	// of one hour and a quarter more.
	maxCoveredIDs = telemetry.SlotsPerHour + telemetry.SlotsPerHour/4
	// maxStoredFailures bounds the stored failure count before it is used.
	maxStoredFailures = 1000
	// noAnswerLock is the reason recorded when the upload lock could not be
	// taken for any reason other than another process holding it.
	noAnswerLock = "lock unavailable"
)

// Seams. Tests replace them; the calling test must not use t.Parallel().
var (
	// jitter is the random delay added to the next attempt.
	jitter = func() time.Duration { return rand.N(maxJitter) }
	// afterLook runs between the first look for events and the lock.
	afterLook func()
)

// Input is what Send needs. Send reads and sends no credential.
type Input struct {
	// ConfigDir is the configuration directory.
	ConfigDir string
	// Version is the build version; only a development build sends.
	Version string
	// Origin is the canonical origin of the endpoint, as Origin returns it.
	Origin string
	// Encoder writes the requests and reads the answers.
	Encoder Encoder
	// Written are the compacted files the hourly pass of this run wrote (the
	// third result of telemetry.Record).
	Written []string
	// Deadline is the time the whole attempt must end by.
	Deadline time.Time
	// Floor is the least time that must be left before the deadline for the
	// first request to start.
	Floor time.Duration
	// Debug receives one line before and one after each request; nil prints
	// nothing.
	Debug io.Writer
	// RecordingOn resolves the recording controls again, right after the upload
	// lock is taken and just before the files are deleted and the state is
	// written. It runs while the upload lock is held, so it must take no lock:
	// a lock taken inside it would be taken under the upload lock. It may wait
	// for a file read, which takes up to 2 seconds on Windows, so the deadline is
	// checked after each call. A nil function sends nothing.
	RecordingOn func() bool
}

// Send makes one upload attempt for a run that recorded an event. It returns
// nothing and prints nothing besides the debug lines: a failure is dropped and
// a panic is recovered. It does nothing for a release build, for an origin that
// Origin would change or refuse, for an attempt that is not due, and when less
// than the floor is left before the deadline.
//
// It looks for ready events with a read only, then takes the upload lock and
// reads the state file again: an attempt that another process ended in the
// meantime is not due, and this one stops. It then resolves the recording
// controls again and stops when recording is off or no time is left. Otherwise
// it lists the events again, sends them in requests inside the receiver's
// limits, deletes the files the answers covered, lists the events once more
// when time is left to drop the record ids of the deleted files from the
// state, writes the state file and releases the lock. The lock is released on
// every return; the state is written only when the state file read after the
// take is due or missing, recording is on and time is left. It creates
// telemetry/upload only to take the lock, which it does only when there are
// events to send or covered files to delete.
func Send(ctx context.Context, in Input) {
	defer func() { _ = recover() }()
	if ctx.Err() != nil || !telemetry.IsDevelopmentVersion(in.Version) {
		return
	}
	if origin, err := Origin(in.Origin); err != nil || origin != in.Origin {
		return
	}
	if in.Encoder == nil || in.RecordingOn == nil || time.Until(in.Deadline) < in.Floor {
		return
	}
	s, err := telemetry.OpenStore(in.ConfigDir)
	if err != nil {
		return
	}
	defer func() { _ = s.Close() }()
	a := &attempt{in: in, store: s, start: time.Now()}
	a.run(ctx)
}

// attempt is one call of Send.
type attempt struct {
	in    Input
	store *telemetry.Store
	start time.Time
	// prior is the state file as read after the lock was taken, or after taking
	// it failed; the zero value when it did not read.
	prior telemetry.UploadState
}

// left reports how much time remains before the deadline.
func (a *attempt) left() time.Duration { return time.Until(a.in.Deadline) }

// debug writes one debug line; a write error is dropped.
func (a *attempt) debug(line string) {
	if a.in.Debug != nil {
		_, _ = io.WriteString(a.in.Debug, line+"\n")
	}
}

// recordingOn reports whether the controls still allow a write.
func (a *attempt) recordingOn() bool { return a.in.RecordingOn() }

func (a *attempt) run(ctx context.Context) {
	state, ok := a.store.ReadUploadState()
	if ok && !state.AttemptDue(a.start) {
		return
	}
	list, err := a.store.Ready(telemetry.ReadyOptions{Written: a.in.Written})
	if err != nil {
		return
	}
	// Whether telemetry/upload exists before the lock is taken: the take
	// creates it.
	existed, _ := a.store.UploadLockStatus()
	if len(leaveOut(list.Events, state.CoveredIDs)) == 0 && !existed {
		// Nothing to send, and no earlier attempt created telemetry/upload
		// for covered files that wait for a deletion: create nothing.
		return
	}
	if a.left() < a.in.Floor {
		return
	}
	if afterLook != nil {
		afterLook()
	}
	lock, err := a.store.TakeUploadLock()
	switch {
	case err == nil:
	case errors.Is(err, telemetry.ErrUploadLockHeld),
		errors.Is(err, telemetry.ErrNoTelemetryDirectory),
		errors.Is(err, telemetry.ErrUnsafeUploadDir):
		return
	default:
		a.writeBackoff()
		return
	}
	defer func() { _ = lock.Release() }()
	// Another process may have finished an attempt between the first read of
	// the state file and the lock: read it again, and stop when it is not due.
	a.prior, ok = a.store.ReadUploadState()
	if ok && !a.prior.AttemptDue(a.start) {
		return
	}
	if !a.recordingOn() {
		if !ok {
			// No state file: the directory holds only this attempt's lock.
			_ = lock.Release()
			_ = a.store.DeleteUploadState()
		}
		return
	}
	if a.left() <= 0 {
		return
	}
	list, err = a.store.Ready(telemetry.ReadyOptions{Written: a.in.Written})
	if err != nil {
		return
	}
	res := a.sendAll(ctx, leaveOut(list.Events, a.prior.CoveredIDs))
	a.finish(lock, list, res)
}

// leaveOut returns the events whose record id is not in ids.
func leaveOut(events []telemetry.StoredEvent, ids []string) []telemetry.StoredEvent {
	if len(ids) == 0 {
		return events
	}
	skip := idSet(ids)
	var out []telemetry.StoredEvent
	for _, ev := range events {
		if !skip[ev.UID] {
			out = append(out, ev)
		}
	}
	return out
}

func idSet(ids []string) map[string]bool {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// result is what the requests of one attempt did.
type result struct {
	requests int
	// sent counts the events of requests answered with a 2xx; dropped counts
	// the events of requests answered with a 4xx that deletes the batch.
	sent, dropped int
	// refused is the sum of the counts of the 2xx answers; refusedKnown is
	// false when no request got a 2xx or one of the counts was unknown.
	refused      int
	accepted     bool
	refusedKnown bool
	// status is the last answer's status, 0 when the last request got none,
	// and noAnswer says why.
	status   int
	noAnswer string
	// keep is true when an answer said keep and wait; retryAfter is the wait
	// that answer asked for when hasRetryAfter is true.
	keep          bool
	retryAfter    time.Duration
	hasRetryAfter bool
	// covered are the record ids of the requests whose answers covered them.
	covered []string
}

// finish decides what to delete and what the state file says, and writes it.
// Nothing is deleted and nothing is written when recording was turned off.
func (a *attempt) finish(lock *telemetry.UploadLock, list *telemetry.ReadyList, res result) {
	if !a.recordingOn() {
		return
	}
	covered := orderedUnion(a.prior.CoveredIDs, res.covered)
	deleted := false
	if len(covered) > 0 && a.left() > 0 {
		a.store.DeleteCovered(list.Cover(idSet(covered)))
		deleted = true
	}
	if deleted && a.left() >= a.in.Floor {
		// Drop the ids of the files just deleted. With less than the floor left
		// the walk is skipped and the next attempt drops them.
		if after, err := a.store.Ready(telemetry.ReadyOptions{Written: a.in.Written}); err == nil {
			list = after
		}
	}
	covered = keepListed(covered, list.Events)

	st := a.prior
	st.LastAttempt = a.start
	st.CoveredIDs = covered
	if res.requests == 0 {
		st.NextAttempt = a.start.Add(attemptEvery + jitter())
		st.Failures = 0
	} else {
		st.HTTPStatus = res.status
		st.NoAnswer = res.noAnswer
		st.Sent = res.sent
		st.Dropped = res.dropped
		st.Refused = nil
		if res.accepted && res.refusedKnown {
			n := res.refused
			st.Refused = &n
		}
		failures := min(max(a.prior.Failures, 0), maxStoredFailures)
		if res.keep {
			st.Failures = failures + 1
			wait := backoffWait(failures)
			if res.hasRetryAfter {
				wait = min(max(attemptEvery, res.retryAfter), maxWait)
			}
			st.NextAttempt = a.start.Add(wait)
		} else {
			st.Failures = 0
			st.NextAttempt = a.start.Add(attemptEvery + jitter())
		}
	}
	_ = lock.WriteState(st)
}

// writeBackoff records an attempt that could not take the upload lock for a
// reason other than another process holding it, so the next recorded run does
// not repeat the walk before the next attempt time. It reads the state file
// again first: a process that holds the lock may have ended an attempt since
// the first read, and its state stays as it wrote it.
func (a *attempt) writeBackoff() {
	var ok bool
	a.prior, ok = a.store.ReadUploadState()
	if ok && !a.prior.AttemptDue(a.start) {
		return
	}
	if !a.recordingOn() {
		return
	}
	failures := min(max(a.prior.Failures, 0), maxStoredFailures)
	st := a.prior
	st.LastAttempt = a.start
	st.HTTPStatus = 0
	st.NoAnswer = noAnswerLock
	st.Failures = failures + 1
	st.NextAttempt = a.start.Add(backoffWait(failures))
	_ = a.store.WriteStateWithoutLock(st)
}

// backoffWait is the wait after a keep decision with no Retry-After: one hour
// doubled for each earlier consecutive failure, at most a day. The doubling
// stops before the shift can overflow.
func backoffWait(earlier int) time.Duration {
	if earlier >= 5 {
		return maxWait
	}
	return min(attemptEvery<<max(earlier, 0), maxWait)
}

// orderedUnion joins the older ids and the newer ones, keeping the last place
// of an id that occurs twice.
func orderedUnion(older, newer []string) []string {
	all := slices.Concat(older, newer)
	seen := make(map[string]bool, len(all))
	var rev []string
	for _, id := range slices.Backward(all) {
		if !seen[id] {
			seen[id] = true
			rev = append(rev, id)
		}
	}
	slices.Reverse(rev)
	return rev
}

// keepListed returns the ids that an event of the list still has, at most
// maxCoveredIDs, the most recent ones when there are more.
func keepListed(ids []string, events []telemetry.StoredEvent) []string {
	if len(ids) == 0 {
		return nil
	}
	listed := make(map[string]bool, len(events))
	for _, ev := range events {
		listed[ev.UID] = true
	}
	var out []string
	for _, id := range ids {
		if listed[id] {
			out = append(out, id)
		}
	}
	if len(out) > maxCoveredIDs {
		out = out[len(out)-maxCoveredIDs:]
	}
	return out
}
