package assigndiff

import "github.com/rancher/rancher-cli/internal/runtime/safety"

func clear(eff safety.Effect, p *safety.Effect) safety.Effect {
	eff.Diff = nil
	p.Diff = []safety.DiffEntry{}
	return eff
}
