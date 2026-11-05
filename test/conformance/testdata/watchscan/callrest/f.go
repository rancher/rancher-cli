package callrest

import (
	"context"

	"k8s.io/client-go/rest"
)

func open(ctx context.Context, c rest.Interface) {
	_, _ = c.Get().AbsPath("/x").Watch(ctx)
}
