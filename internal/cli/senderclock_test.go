package cli

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/telemetry"
	"github.com/rancher/rancher-cli/internal/telemetrysend"
)

// senderClock holds the clock of the upload deadline checks still at the
// current time, so a row that needs the time to run out at a point it chooses
// does not race the real time. It mirrors testClock in internal/telemetrysend.
type senderClock struct {
	mu       sync.Mutex
	now      time.Time
	deadline time.Time
	ctx      *passedContext
}

// stopSenderClock holds the clock still until the test ends.
func stopSenderClock(t *testing.T) *senderClock {
	t.Helper()
	c := &senderClock{now: time.Now(), ctx: newPassedContext(t.Context())}
	t.Cleanup(telemetrysend.SetClockForTest(c.read))
	return c
}

func (c *senderClock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// install replaces sendNow with a function that notes the deadline of the
// attempt and runs it under the context of the clock. With late, the encoder
// lets the time run out after each answer is read.
func (c *senderClock) install(t *testing.T, late bool) {
	t.Helper()
	orig := sendNow
	t.Cleanup(func() { sendNow = orig })
	sendNow = func(_ context.Context, in telemetrysend.Input) telemetry.UploadResult {
		c.mu.Lock()
		c.deadline = in.Deadline
		c.mu.Unlock()
		if late {
			in.Encoder = lateEncoder{Encoder: in.Encoder, pass: c.pass}
		}
		return orig(c.ctx, in)
	}
}

// pass moves the clock 20 ms past the deadline the attempt runs under and then
// ends the context of the clock, in the order the real deadline does: the time
// is up before the request fails. It is safe to call from a server handler.
func (c *senderClock) pass() {
	c.mu.Lock()
	c.now = c.deadline.Add(20 * time.Millisecond)
	c.mu.Unlock()
	c.ctx.pass()
}

// lateEncoder reads each answer, then lets the time run out.
type lateEncoder struct {
	telemetrysend.Encoder
	pass func()
}

func (e lateEncoder) RefusedCount(answer []byte) (int, bool) {
	e.pass()
	return e.Encoder.RefusedCount(answer)
}

// passedContext is a context that pass ends with context.DeadlineExceeded, as
// the context of a request ends when its deadline passes. Its done channel is
// its own, so a context derived from it takes its error from Err. It also ends
// when its parent ends.
type passedContext struct {
	context.Context
	done   chan struct{}
	once   sync.Once
	passed atomic.Bool
}

func newPassedContext(parent context.Context) *passedContext {
	p := &passedContext{Context: parent, done: make(chan struct{})}
	go func() {
		select {
		case <-parent.Done():
			p.end()
		case <-p.done:
		}
	}()
	return p
}

func (p *passedContext) Done() <-chan struct{} { return p.done }

func (p *passedContext) Err() error {
	if p.passed.Load() {
		return context.DeadlineExceeded
	}
	return p.Context.Err()
}

func (p *passedContext) pass() {
	p.passed.Store(true)
	p.end()
}

func (p *passedContext) end() { p.once.Do(func() { close(p.done) }) }
