package cleardiff

import "github.com/rancher/rancher-cli/internal/runtime/safety"

func wipe(eff safety.Effect) {
	clear(eff.Diff)
}
