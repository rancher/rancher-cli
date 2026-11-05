package watchloopok

import (
	"context"
	"time"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// watch lists and watches with a timer between the cycles and ends the wait
// when engine.WaitShouldStop says so.
func watch(ctx context.Context, w engine.ObjectWatcher, gvr schema.GroupVersionResource) error {
	for {
		_, rv, err := w.ListObject(ctx, gvr, "", "x")
		if err != nil && engine.WaitShouldStop(err) {
			return err
		}
		if err == nil {
			if _, err := w.WatchObject(ctx, gvr, "", "x", rv, time.Minute); err == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
