package twice

import "github.com/rancher/rancher-cli/internal/engine"

type cmd struct{}

func (c cmd) Plan() {
	_ = engine.PlanCreate
	_ = engine.PlanCreate
	_ = engine.CreateDiff
}
