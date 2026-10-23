package method

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
)

type cmd struct{}

func (c cmd) Plan(ctx context.Context) {
	_ = engine.PlanCreate
}
