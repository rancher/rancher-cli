package commands

import (
	"sort"

	"github.com/rancher/rancher-cli/internal/config"
)

// sortedKeys returns m's keys, sorted lexicographically — Go map iteration
// order is not stable, and completion candidate order is pinned (external
// review finding 2).
func sortedKeys[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// contextNames returns the configured context names, sorted
// lexicographically, or nil on any config load error (a missing config is
// not an error — config.Load returns an empty, usable Config).
func contextNames(dir string) []string {
	cfg, err := config.Load(config.FilePath(dir))
	if err != nil {
		return nil
	}
	return sortedKeys(cfg.Contexts)
}

// serverNames returns the configured server names, sorted lexicographically,
// or nil on any config load error. Shared by `config use-server`, `config
// delete-server`, and `config get-servers`.
func serverNames(dir string) []string {
	cfg, err := config.Load(config.FilePath(dir))
	if err != nil {
		return nil
	}
	return sortedKeys(cfg.Servers)
}
