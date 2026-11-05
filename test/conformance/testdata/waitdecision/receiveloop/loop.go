package receiveloop

import (
	"context"
	"time"

	"github.com/rancher/rancher-cli/internal/engine"
)

// receive reads events with a timer and never asks engine.WaitShouldStop
// about an error event.
func receive(ctx context.Context, events <-chan engine.ObjectEvent) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok || ev.Type == engine.ObjectFailed {
				return nil
			}
		case <-time.After(time.Second):
		}
	}
}
