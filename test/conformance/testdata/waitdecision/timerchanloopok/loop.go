package timerchanloopok

import (
	"context"
	"time"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// poll waits on a channel it was given, with no call of time in the loop,
// and ends the wait when engine.WaitShouldStop says so.
func poll(ctx context.Context, c engine.ClusterClient, gvr schema.GroupVersionResource, tick <-chan time.Time) error {
	for {
		_, err := c.Get(ctx, gvr, "", "x")
		if err == nil {
			return nil
		}
		if engine.WaitShouldStop(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick:
		}
	}
}
