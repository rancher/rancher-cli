package assignaction

import "github.com/rancher/rancher-cli/internal/runtime/safety"

func make(eff *safety.Effect) {
	eff.Action = safety.ActionCreate
}
