package pass

import (
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

type other struct {
	Action string
	Diff   []int
}

func use(eff safety.Effect, o *other) int {
	_ = safety.Effect{Action: safety.ActionUpdate, Target: "t"}
	_ = safety.Effect{Action: safety.ActionDelete}
	_ = safety.Effect{}
	_ = other{Action: "create", Diff: nil}
	o.Action = "create"
	o.Diff = nil
	eff.Action = safety.ActionUpdate
	eff.Warnings = nil
	_, _ = kinds.Lookup("secret")
	o.Diff[0] = 1
	o.Diff = append(o.Diff, 2)
	clear(o.Diff)
	m := map[string]int{}
	clear(m)
	_ = append([]int{}, 1)
	copied := make([]safety.DiffEntry, len(eff.Diff))
	copy(copied, eff.Diff)
	_ = eff.Diff[0].To
	return len(eff.Diff)
}
