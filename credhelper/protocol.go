// Package credhelper defines the rancher credential helper wire protocol
// (version 1) and a kit for writing helpers. See CREDENTIAL_HELPERS.md at the
// repository root for the protocol contract.
package credhelper

// ProtocolVersion is the wire protocol version this package speaks.
const ProtocolVersion = 1

// Verbs a helper binary is invoked with as its single argument.
const (
	VerbGet   = "get"
	VerbStore = "store"
	VerbErase = "erase"
)

// Request is the JSON object the CLI writes to a helper's stdin. ServerURL is
// always in canonical form (CanonicalServerURL) — the canonical value is the
// wire contract, so helpers may key storage on it verbatim.
type Request struct {
	ProtocolVersion int    `json:"protocolVersion"`
	ServerURL       string `json:"serverURL"`
	Name            string `json:"name"`
	Token           string `json:"token,omitempty"` // store only
}

// Response is the JSON object a helper writes to stdout on success. A get
// response without a token means "no credential" (not an error). ExpiresAt,
// when set, is RFC 3339 and marks the token short-lived. Unsupported is a
// store response declaring the helper read-only.
type Response struct {
	ProtocolVersion int    `json:"protocolVersion"`
	Token           string `json:"token,omitempty"`
	ExpiresAt       string `json:"expiresAt,omitempty"`
	Unsupported     bool   `json:"unsupported,omitempty"`
}
