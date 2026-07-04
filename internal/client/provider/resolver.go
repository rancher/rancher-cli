package provider

// Tier is the API backend that serves an operation.
type Tier string

const (
	TierPublic Tier = "public"
	TierCRD    Tier = "crd"
	TierNorman Tier = "norman"
)

// Stability is the support contract of an operation.
type Stability string

const (
	Stable     Stability = "stable"
	Beta       Stability = "beta"
	Deprecated Stability = "deprecated"
)

// Binding is the resolved tier+stability for an operation.
type Binding struct {
	Tier      Tier
	Stability Stability
}

// Resolver maps operation names to their current tier binding.
type Resolver struct {
	m map[string]Binding
}

// NewResolver returns the Phase-0/1 op map. Promotions are edits here only.
func NewResolver() *Resolver {
	return &Resolver{m: map[string]Binding{
		// public tier (RK-API), stable
		"create project": {TierPublic, Stable},
		"get projects":   {TierPublic, Stable},
		"delete project": {TierPublic, Stable},
		// crd tier, beta reads (Phase 1)
		"get clusters":   {TierCRD, Beta},
		"get nodes":      {TierCRD, Beta},
		"get namespaces": {TierCRD, Beta},
	}}
}

// Resolve returns the binding for an op, if known.
func (r *Resolver) Resolve(op string) (Binding, bool) {
	b, ok := r.m[op]
	return b, ok
}

// All returns a copy of the full op map (for the schema manifest).
func (r *Resolver) All() map[string]Binding {
	out := make(map[string]Binding, len(r.m))
	for k, v := range r.m {
		out[k] = v
	}
	return out
}
