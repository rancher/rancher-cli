package rangeloop

import (
	"context"
	"time"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func poll(ctx context.Context, c engine.ClusterClient, gvr schema.GroupVersionResource) error {
	for range 10 {
		if _, err := c.Get(ctx, gvr, "", "x"); err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return ctx.Err()
}
