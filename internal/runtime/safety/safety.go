package safety

// DiffEntry is a single field change in an Effect.
type DiffEntry struct {
	Path string `json:"path"`
	From any    `json:"from"`
	To   any    `json:"to"`
}

// Effect is the server-computed preview of a mutation (from Plan).
type Effect struct {
	Action            string      `json:"action"`
	Target            string      `json:"target"`
	Diff              []DiffEntry `json:"diff,omitempty"`
	ResourceVersion   string      `json:"resourceVersion,omitempty"`
	ConflictDetection bool        `json:"conflictDetection"`
	Reversible        bool        `json:"reversible"`
	Destructive       bool        `json:"destructive"`
	Tier              string      `json:"tier,omitempty"`
	Stability         string      `json:"stability,omitempty"`
}
