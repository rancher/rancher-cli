package rangetickerloop

import (
	"context"
	"time"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// poll reads on every tick of tick.
func poll(ctx context.Context, c engine.ClusterClient, gvr schema.GroupVersionResource, tick <-chan time.Time) error {
	for range tick {
		_, err := c.Get(ctx, gvr, "", "x")
		if err == nil || ctx.Err() != nil {
			return err
		}
	}
	return nil
}
