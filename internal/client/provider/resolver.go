package provider

import "github.com/rancher/rancher-cli/internal/runtime/safety"

// Resolver maps operation names to their current tier binding.
type Resolver struct {
	m map[string]safety.Binding
}

// NewResolver returns the Phase-0/1 op map. Promotions are edits here only.
// Keys are the canonical-singular Descriptor.Name (grammar rule: nouns are
// canonical singular; plural/short forms are accepted aliases that resolve to the
// singular). Phase-1 read/write commands MUST register with singular names so the
// schema merge finds their tier/stability here.
func NewResolver() *Resolver {
	return &Resolver{m: map[string]safety.Binding{
		// public tier (RK-API), stable
		"create project": {safety.TierPublic, safety.Stable},
		"get project":    {safety.TierPublic, safety.Stable},
		"delete project": {safety.TierPublic, safety.Stable},
		// crd tier, beta reads (Phase 1)
		"get cluster":   {safety.TierCRD, safety.Beta},
		"get node":      {safety.TierCRD, safety.Beta},
		"get namespace": {safety.TierCRD, safety.Beta},
	}}
}

// Resolve returns the binding for an op, if known.
func (r *Resolver) Resolve(op string) (safety.Binding, bool) {
	b, ok := r.m[op]
	return b, ok
}

// All returns a copy of the full op map (for the schema manifest).
func (r *Resolver) All() map[string]safety.Binding {
	out := make(map[string]safety.Binding, len(r.m))
	for k, v := range r.m {
		out[k] = v
	}
	return out
}
