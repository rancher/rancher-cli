package othercalls

import "github.com/rancher/rancher-cli/internal/engine"

type cmd struct{}

// PlanCreate is a local function, not the engine's.
func PlanCreate() {}

func (c cmd) Plan() {
	PlanCreate()
	_ = engine.PlanUpdate
	_ = engine.DiffObjects
}
