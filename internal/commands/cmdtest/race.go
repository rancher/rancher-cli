package cmdtest

import (
	"sync"
	"testing"
	"time"
)

// raceDeadline bounds every wait of a Race, so a writer that never gets
// where the test expects fails the test instead of hanging it.
const raceDeadline = 30 * time.Second

// Race runs one exec-plugin process in a goroutine of its own and holds
// it at a point the test chooses, so the test can run a command between
// that process's steps. Every wait has a deadline; the goroutine's
// errors come back on a channel, never through the test's own failure
// calls; and the held process is let go when the test ends, however it
// ends.
type Race struct {
	read, release chan struct{}
	done          chan raceResult
	readOnce      sync.Once
	releaseOnce   sync.Once
}

type raceResult struct {
	tok string
	err error
}

// NewRace makes a Race for the test.
func NewRace(t *testing.T) *Race {
	t.Helper()
	r := &Race{read: make(chan struct{}), release: make(chan struct{}), done: make(chan raceResult, 1)}
	t.Cleanup(r.Release)
	return r
}

// Hold is what the held process runs where it waits: it says it got
// there, then waits to be released. It never waits past the deadline.
func (r *Race) Hold() {
	r.readOnce.Do(func() { close(r.read) })
	select {
	case <-r.release:
	case <-time.After(raceDeadline):
	}
}

// Start runs the process for the entry name against url in dir.
func (r *Race) Start(t *testing.T, dir, url, name string) {
	t.Helper()
	ctx := t.Context()
	go func() {
		tok, err := RunKubectl(ctx, dir, url, name)
		r.done <- raceResult{tok, err}
	}()
}

// WaitHeld waits until the process is held, and fails the test if it
// ended first or the deadline passed.
func (r *Race) WaitHeld(t *testing.T) {
	t.Helper()
	select {
	case <-r.read:
	case res := <-r.done:
		r.done <- res
		t.Fatalf("the racing process ended before it was held: %v", res.err)
	case <-time.After(raceDeadline):
		t.Fatal("the racing process was not held within the deadline")
	}
}

// Release lets the held process go on.
func (r *Race) Release() { r.releaseOnce.Do(func() { close(r.release) }) }

// Wait waits for the process to end and returns the credential it was
// handed, failing the test on its error or at the deadline.
func (r *Race) Wait(t *testing.T) string {
	t.Helper()
	select {
	case res := <-r.done:
		if res.err != nil {
			t.Fatalf("the racing process failed: %v", res.err)
		}
		return res.tok
	case <-time.After(raceDeadline):
		t.Fatal("the racing process did not end within the deadline")
	}
	return ""
}
