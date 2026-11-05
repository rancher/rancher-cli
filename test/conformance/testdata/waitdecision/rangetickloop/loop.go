package rangetickloop

import (
	"context"
	"time"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// poll reads on every tick of time.Tick(time.Second).
func poll(ctx context.Context, c engine.ClusterClient, gvr schema.GroupVersionResource) error {
	for range time.Tick(time.Second) {
		_, err := c.Get(ctx, gvr, "", "x")
		if err == nil || ctx.Err() != nil {
			return err
		}
	}
	return nil
}
