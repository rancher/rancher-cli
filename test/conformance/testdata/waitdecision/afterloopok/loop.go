package afterloopok

import (
	"context"
	"time"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func poll(ctx context.Context, c engine.ClusterClient, gvr schema.GroupVersionResource) error {
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
		case <-time.After(time.Second):
		}
	}
}
