package appenddiff

import "github.com/rancher/rancher-cli/internal/runtime/safety"

func grow(eff safety.Effect) []safety.DiffEntry {
	return append(eff.Diff, safety.DiffEntry{})
}
