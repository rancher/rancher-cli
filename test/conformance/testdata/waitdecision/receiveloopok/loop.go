package receiveloopok

import (
	"context"
	"time"

	"github.com/rancher/rancher-cli/internal/engine"
)

// receive reads events with a timer and ends the wait when
// engine.WaitShouldStop says so about an error event.
func receive(ctx context.Context, events <-chan engine.ObjectEvent) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if ev.Type == engine.ObjectFailed && engine.WaitShouldStop(ev.Err) {
				return ev.Err
			}
		case <-time.After(time.Second):
		}
	}
}
