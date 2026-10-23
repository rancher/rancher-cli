package negative

import (
	"context"
)

type clock struct{}

func (clock) Context() context.Context { return context.Background() }

func wait(r clock) {
	<-r.Context().Done()
}

func plain(ctx context.Context) {
	<-ctx.Done()
}
