package calloutside

import (
	"context"
	"time"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// poll calls engine.WaitShouldStop after the loop, never on a Get error
// inside it.
func poll(ctx context.Context, c engine.ClusterClient, gvr schema.GroupVersionResource) error {
	var last error
	for range 10 {
		_, err := c.Get(ctx, gvr, "", "x")
		if err == nil {
			return nil
		}
		last = err
		time.Sleep(time.Second)
	}
	if engine.WaitShouldStop(last) {
		return last
	}
	return ctx.Err()
}
