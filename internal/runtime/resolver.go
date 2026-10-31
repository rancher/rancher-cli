package runtime

import (
	"fmt"
	"maps"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// MapResolver is a static op→binding table implementing Resolver. The kind
// table's generated bindings are wrapped in one.
type MapResolver map[string]safety.Binding

func (m MapResolver) Resolve(op string) (safety.Binding, bool) { b, ok := m[op]; return b, ok }

// All returns a fresh copy so no caller can mutate the resolver's bindings
// out from under the other consumers of the same map.
func (m MapResolver) All() map[string]safety.Binding { return maps.Clone(m) }

// ComposeResolvers merges two resolvers, rejecting any op defined by both - a
// single source per op (the kind table OR the bespoke map, never both).
// BuildRoot fails on the returned error.
func ComposeResolvers(a, b Resolver) (Resolver, error) {
	merged := map[string]safety.Binding{}
	maps.Copy(merged, a.All())
	for op, bind := range b.All() {
		if _, dup := merged[op]; dup {
			return nil, fmt.Errorf("command %q has its contract and stability set in two places", op)
		}
		merged[op] = bind
	}
	return MapResolver(merged), nil
}
