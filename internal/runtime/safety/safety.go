package safety

// Action is the closed verb vocabulary for Effects. A strict superset of
// rancher-ai-mcp's PlanResource.Type (create|update|delete) per AL-2.
type Action string

const (
	ActionCreate Action = "create"
	ActionUpdate Action = "update"
	ActionDelete Action = "delete"
	ActionScale  Action = "scale"
)

// Valid reports whether a is one of the closed enum values.
func (a Action) Valid() bool {
	switch a {
	case ActionCreate, ActionUpdate, ActionDelete, ActionScale:
		return true
	}
	return false
}

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

// DiffEntry is a single field change in an Effect.
type DiffEntry struct {
	Path string `json:"path"`
	From any    `json:"from"`
	To   any    `json:"to"`
}

// Effect is the server-computed preview of a mutation (from Plan). Tier and
// Stability are stamped by the pipeline from the resolver binding — values a
// command sets are overwritten, never trusted. Server is stamped by the
// surface adapter (Branch 2); Cluster is set by the command's Plan.
type Effect struct {
	Action            Action      `json:"action"`
	Target            string      `json:"target"`
	Server            string      `json:"server,omitempty"`
	Cluster           string      `json:"cluster,omitempty"`
	Diff              []DiffEntry `json:"diff,omitempty"`
	ResourceVersion   string      `json:"resourceVersion,omitempty"`
	ConflictDetection bool        `json:"conflictDetection"`
	Reversible        bool        `json:"reversible"`
	Destructive       bool        `json:"destructive"`
	Tier              Tier        `json:"tier,omitempty"`
	Stability         Stability   `json:"stability,omitempty"`
	// Warnings are advisory preview notes rendered with the Effect BEFORE
	// confirmation (version-not-in-discovery, etcd-quorum impact, deletion
	// cascade). They never gate — gating is Action/Destructive/Stability's
	// job — they inform the user/agent deciding whether to --apply.
	// json:"-" DELIBERATELY (review R2-6): the Effect marshals inside the
	// envelope's data field, and renderEffect copies these into the
	// envelope's top-level warnings — serializing here too would emit
	// every warning twice (data.warnings + warnings).
	Warnings []string `json:"-"`
}
