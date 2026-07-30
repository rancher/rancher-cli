package commands

import "sort"

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
