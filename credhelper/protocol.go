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
// always in canonical form (CanonicalServerURL) - the canonical value is the
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
// store response declaring the helper read-only. Erased, on an erase
// response, reports whether a stored credential actually existed and was
// removed: true = deleted, false = nothing was stored, absent = the helper
// cannot tell (the CLI then reports the erase as a deletion - the safe,
// over-reporting direction).
type Response struct {
	ProtocolVersion int    `json:"protocolVersion"`
	Token           string `json:"token,omitempty"`
	ExpiresAt       string `json:"expiresAt,omitempty"`
	Unsupported     bool   `json:"unsupported,omitempty"`
	Erased          *bool  `json:"erased,omitempty"`
}

// EraseOutcome is a Handler's answer to Erase: was a credential actually
// deleted? EraseUnknown is the zero value - a helper that cannot tell
// returns it and the response simply omits the erased field.
type EraseOutcome int

const (
	EraseUnknown       EraseOutcome = iota // cannot tell - reported as deleted downstream
	EraseDeleted                           // a stored credential existed and is gone
	EraseNothingStored                     // nothing was stored; the erase was a no-op
)
