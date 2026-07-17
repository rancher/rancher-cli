package provider

import "github.com/rancher/rancher-cli/internal/runtime/safety"

// Resolver maps operation names to their current tier binding.
type Resolver struct {
	m map[string]safety.Binding
}

// NewResolver returns the bespoke op map: bindings for non-generic operations
// only (composites, auth-adjacent). Bindings for generated generic ops
// (get/delete <kind>) are owned by the kind table (kinds.Bindings) and composed
// in at BuildRoot — an op defined by both sources fails composition. Keys are
// the canonical-singular Descriptor.Name; promotions of a bespoke op are edits
// here only.
func NewResolver() *Resolver {
	return &Resolver{m: map[string]safety.Binding{
		"create project":           {Tier: safety.TierPublic, Stability: safety.Stable},
		"create namespace":         {Tier: safety.TierPublic, Stability: safety.Stable},
		"project add-namespace":    {Tier: safety.TierPublic, Stability: safety.Stable},
		"project remove-namespace": {Tier: safety.TierPublic, Stability: safety.Stable},
		"project add-member":       {Tier: safety.TierPublic, Stability: safety.Beta},
		"project remove-member":    {Tier: safety.TierPublic, Stability: safety.Beta},
		"create secret":            {Tier: safety.TierPublic, Stability: safety.Beta},
		"create resource":          {Tier: safety.TierCRD, Stability: safety.Beta},
		"create token":             {Tier: safety.TierPublic, Stability: safety.Beta},
		"create kubeconfig":        {Tier: safety.TierPublic, Stability: safety.Beta},
		"create cluster":           {Tier: safety.TierCRD, Stability: safety.Beta},

		"inspect pod":             {Tier: safety.TierCRD, Stability: safety.Beta},
		"inspect deployment":      {Tier: safety.TierCRD, Stability: safety.Beta},
		"inspect project":         {Tier: safety.TierPublic, Stability: safety.Beta},
		"get cloud-credential":    {Tier: safety.TierCRD, Stability: safety.Beta},
		"get kubernetes-versions": {Tier: safety.TierCRD, Stability: safety.Beta},
		"images":                  {Tier: safety.TierCRD, Stability: safety.Beta},
		"usage":                   {Tier: safety.TierCRD, Stability: safety.Beta},

		"logs":           {Tier: safety.TierCRD, Stability: safety.Beta},
		"support-bundle": {Tier: safety.TierCRD, Stability: safety.Beta},
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
