package walk

import "github.com/rancher/rancher-cli/internal/engine"

type cmd struct{}

func (c cmd) Plan() {
	_ = engine.CreateDiff
}
