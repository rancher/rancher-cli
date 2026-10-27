package rangeloopok

import (
	"context"
	"time"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func poll(ctx context.Context, c engine.ClusterClient, gvr schema.GroupVersionResource) error {
	for range 10 {
		_, err := c.Get(ctx, gvr, "", "x")
		if err == nil {
			return nil
		}
		if engine.WaitShouldStop(err) {
			return err
		}
		time.Sleep(time.Second)
	}
	return ctx.Err()
}
