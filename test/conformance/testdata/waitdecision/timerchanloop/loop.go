package timerchanloop

import (
	"context"
	"time"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// poll waits on a channel it was given, with no call of time in the loop,
// and never asks engine.WaitShouldStop about an error.
func poll(ctx context.Context, c engine.ClusterClient, gvr schema.GroupVersionResource, tick <-chan time.Time) error {
	for {
		if _, err := c.Get(ctx, gvr, "", "x"); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick:
		}
	}
}
