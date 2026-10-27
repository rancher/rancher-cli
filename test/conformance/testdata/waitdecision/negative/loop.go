package negative

import (
	"context"
	"time"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// store has a four-argument Get like ClusterClient's but is not one.
type store struct{}

func (store) Get(context.Context, string, string, string) error { return nil }

// timerWithoutClient waits with a timer and calls a Get on another type.
func timerWithoutClient(ctx context.Context, s store) {
	for {
		if err := s.Get(ctx, "", "", "x"); err == nil {
			return
		}
		time.Sleep(time.Second)
	}
}

// clientWithoutTimer reads in a loop with no wait between the reads.
func clientWithoutTimer(ctx context.Context, c engine.ClusterClient, gvr schema.GroupVersionResource, names []string) {
	for _, name := range names {
		_, _ = c.Get(ctx, gvr, "", name)
	}
}
