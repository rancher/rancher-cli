package assignelement

import "github.com/rancher/rancher-cli/internal/runtime/safety"

func rewrite(eff safety.Effect, p *safety.Effect) {
	eff.Diff[0] = safety.DiffEntry{}
	eff.Diff[1].To = "x"
	p.Diff[2].From, p.Action = nil, safety.ActionUpdate
}
