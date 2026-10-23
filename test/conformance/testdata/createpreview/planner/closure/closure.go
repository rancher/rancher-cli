package closure

import "github.com/rancher/rancher-cli/internal/engine"

type cmd struct{}

func (c cmd) Plan() {
	run := func() {
		_ = engine.PlanCreateObserved
	}
	run()
}
