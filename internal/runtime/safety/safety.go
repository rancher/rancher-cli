package safety

// Action is the closed verb vocabulary for Effects. A strict superset of
// rancher-ai-mcp's PlanResource.Type (create|update|delete).
type Action string

const (
	ActionCreate Action = "create"
	ActionUpdate Action = "update"
	ActionDelete Action = "delete"
)

// Valid reports whether a is one of the closed enum values.
func (a Action) Valid() bool {
	switch a {
	case ActionCreate, ActionUpdate, ActionDelete:
		return true
	}
	return false
}

// Contract is the Rancher Kubernetes Public API membership designation for an
// operation. Public means the kind is on the semi-official Rancher Kubernetes
// Public API list; Internal means it is not cited there.
type Contract string

const (
	ContractPublic   Contract = "public"
	ContractInternal Contract = "internal"
)

// Stability is the support contract of an operation, independent of
// Contract: today's rows pair {public,stable} and {internal,beta}, and
// Deprecated's designated first row is the legacy v3 Token kind
// (Token.management.cattle.io - still serving UI session tokens and some
// derived tokens in Rancher 2.15), which will land as internal/deprecated
// with its writes behind --allow-deprecated.
type Stability string

const (
	Stable     Stability = "stable"
	Beta       Stability = "beta"
	Deprecated Stability = "deprecated"
)

// Binding is the resolved contract+stability for an operation.
type Binding struct {
	Contract  Contract
	Stability Stability
}

// DiffEntry is a single field change in an Effect.
type DiffEntry struct {
	Path string `json:"path"`
	From any    `json:"from"`
	To   any    `json:"to"`
}

// Object is one object a multi-object mutation acts on or reports: its kind
// and name, what the command did or will do to it (Outcome, a short phrase
// the command defines), and for a failure the error's code and message. A
// refusal reason for an object the command leaves alone rides in Message.
type Object struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// The outcomes of an Object, in the order text output lists them. A command
// uses the ones that apply to what it does.
const (
	OutcomeRancherKeeps    = "rancher keeps"
	OutcomeRancherDeletes  = "rancher deletes"
	OutcomeUnknown         = "unknown"
	OutcomeWillBeDeleted   = "will be deleted"
	OutcomeNotDeleted      = "not deleted"
	OutcomeBeingDeleted    = "being deleted"
	OutcomeDeletionStarted = "deletion started"
	OutcomeAlreadyGone     = "already gone"
	OutcomeFailed          = "failed"
	OutcomeNotAttempted    = "not attempted"
)

// Effect is the preview of a mutation (from Plan): its Diff lists the fields
// the request sets or changes, and the server's dry-run still decides whether
// the preview succeeds. Contract and Stability are NOT serialized - the
// schema manifest is the single published source for those; the internal
// Binding continues to drive the --allow-deprecated gate in the pipeline.
// Server is stamped by the surface adapter (Branch 2); Cluster is set by the
// command's Plan.
type Effect struct {
	Action Action `json:"action"`
	Target string `json:"target"`
	// DisplayKind is the canonical CLI kind from the kind table (e.g.
	// "globalrolebinding", "management-token", "provisioning-cluster") -
	// NEVER an API plural or a Kubernetes kind. Every default-format
	// mutation text renders from DisplayKind, never from Target (an API
	// resource plural unsuited for display). A Plan that omits it is a
	// command bug (checkPlan).
	DisplayKind string `json:"displayKind"`
	// DisplayName is the resource name, or a generateName prefix plus "*"
	// for a preview whose real name is not yet known (dry-run text only -
	// the committed success identity is derived from the returned object
	// and never carries the "*" suffix). A Plan that omits it is a command
	// bug (checkPlan).
	DisplayName       string      `json:"displayName"`
	Server            string      `json:"server,omitempty"`
	Cluster           string      `json:"cluster,omitempty"`
	Diff              []DiffEntry `json:"diff,omitempty"`
	ResourceVersion   string      `json:"resourceVersion,omitempty"`
	ConflictDetection bool        `json:"conflictDetection"`
	Reversible        bool        `json:"reversible"`
	Destructive       bool        `json:"destructive"`
	// Objects lists the objects beyond Target that the command acts on or
	// reports, each with its outcome. Empty for a command that touches one
	// object, which leaves its output unchanged.
	Objects []Object `json:"objects,omitempty"`
	// Warnings are advisory preview notes rendered with the Effect BEFORE
	// confirmation (version-not-in-discovery, etcd-quorum impact, deletion
	// cascade). They never gate - gating is Action/Destructive/Stability's
	// job - they inform the user/agent deciding whether to --apply.
	// json:"-" DELIBERATELY: the Effect marshals inside the
	// envelope's data field, and renderEffect copies these into the
	// envelope's top-level warnings - serializing here too would emit
	// every warning twice (data.warnings + warnings).
	Warnings []string `json:"-"`
}
