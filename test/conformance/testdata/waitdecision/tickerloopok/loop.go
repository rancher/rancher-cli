package tickerloopok

import (
	"context"
	"time"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func poll(ctx context.Context, c engine.ClusterClient, gvr schema.GroupVersionResource) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
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
		case <-ticker.C:
		}
	}
}
