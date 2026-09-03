package clierr

import "fmt"

// TokenKind names the store a Rancher token lives in and therefore the
// path that deletes it and the CLI kind that addresses it.
type TokenKind string

const (
	TokenKindExt TokenKind = "ext" // ext.cattle.io/v1 Token, the `token` kind
	TokenKindV3  TokenKind = "v3"  // management.cattle.io/v3 Token, the deprecated `management-token` kind
)

// TokenRef identifies one server-side token by resource name.
type TokenRef struct {
	Kind TokenKind `json:"kind"`
	Name string    `json:"name"`
}

// String renders kind/name for messages.
func (r TokenRef) String() string { return string(r.Kind) + "/" + r.Name }

// CreateOutcome classifies what a token create left on the server, as far
// as the client can tell. Only a deterministic 4xx proves absence.
type CreateOutcome string

const (
	CreateNotAttempted CreateOutcome = "notAttempted"
	CreateNotCreated   CreateOutcome = "notCreated"
	CreateKnown        CreateOutcome = "known"
	CreateUnidentified CreateOutcome = "unidentified"
	CreateUnknown      CreateOutcome = "unknown"
)

// RevokeResult is one token's revocation result.
type RevokeResult string

const (
	ResultRevoked       RevokeResult = "revoked"
	ResultAlreadyGone   RevokeResult = "alreadyGone"
	ResultFailed        RevokeResult = "failed"
	ResultNotAttempted  RevokeResult = "notAttempted"
	ResultNotApplicable RevokeResult = "notApplicable"
)

// RevokeOutcome records one revocation attempt.
type RevokeOutcome struct {
	Ref    *TokenRef    `json:"ref,omitempty"`
	Result RevokeResult `json:"result"`
	Error  string       `json:"error,omitempty"`
}

// Provenance says who created a login credential and therefore who may
// revoke it.
type Provenance string

const (
	ProvenanceDerived       Provenance = "derived"
	ProvenanceLegacySession Provenance = "legacySession"
	ProvenanceUserSupplied  Provenance = "userSupplied"
)

// CredentialBackend names the physical credential store a commit reached.
type CredentialBackend string

const (
	BackendFile    CredentialBackend = "file"
	BackendKeyring CredentialBackend = "keyring"
	BackendExec    CredentialBackend = "exec"
)

// PriorSource says where the effective prior token physically lived before
// a write, or that this could not be determined. PriorUnset is the zero
// value and means no backend was reached; every reached backend sets an
// explicit value: file reports none|file|unknown, keyring reports
// none|keyring|file|unknown, exec always reports unknown (its protocol has
// no read or snapshot operation).
type PriorSource string

const (
	PriorUnset   PriorSource = ""
	PriorNone    PriorSource = "none"
	PriorKeyring PriorSource = "keyring"
	PriorFile    PriorSource = "file"
	PriorUnknown PriorSource = "unknown"
)

// CredentialState is what the credential store holds after a login
// attempt, as the backend reported it and as compensation changed it.
type CredentialState string

const (
	CredentialNotWritten               CredentialState = "notWritten"
	CredentialCommitted                CredentialState = "committed"
	CredentialCompensated              CredentialState = "compensated"
	CredentialCompensationFailed       CredentialState = "compensationFailed"
	CredentialCommittedUncompensatable CredentialState = "committedUncompensatable"
	CredentialPossiblyCommitted        CredentialState = "possiblyCommitted"
	CredentialHelperNonAtomic          CredentialState = "helperNonAtomic"
)

// ConfigState is what config.yaml holds after a login attempt.
type ConfigState string

const (
	ConfigNotWritten  ConfigState = "notWritten"
	ConfigWritten     ConfigState = "written"
	ConfigWriteFailed ConfigState = "writeFailed"
)

// LoginStage is where a login failed: while acquiring the credential or
// while committing it.
type LoginStage string

const (
	StageAuthentication LoginStage = "authentication"
	StageCommit         LoginStage = "commit"
)

// PrimaryFailure is the primary error's own projection, kept whole so
// composing cleanup details around it never discards it. Warnings are not
// here: they ride the outer error so they render once.
type PrimaryFailure struct {
	Code       string      `json:"code"`
	Message    string      `json:"message"`
	Hint       string      `json:"hint,omitempty"`
	Retryable  bool        `json:"retryable"`
	Candidates []Candidate `json:"candidates,omitempty"`
	Details    any         `json:"details,omitempty"`
}

// UnmarkedTokenSearch carries what the CLI actually knows about a token it
// cannot name: the provider it was created through and the client-side
// time window of the browser handoff. These narrow a candidate set; they
// cannot identify one token.
type UnmarkedTokenSearch struct {
	AuthProvider         string `json:"authProvider"`
	HandoffAt            string `json:"handoffAt"`
	PollEndedAt          string `json:"pollEndedAt"`
	ServerRelayExpiresAt string `json:"serverRelayExpiresAt,omitempty"`
}

// TokenFate is one token's outcome and cleanup.
type TokenFate struct {
	Outcome CreateOutcome `json:"outcome"`
	// Kind is the token's kind when it is known without a name: the mint
	// endpoint only creates ext tokens, the SAML authtokens flow only v3
	// tokens, and a readable login response declares its shape. Empty when
	// a synchronous login response was lost or established no shape: the
	// token, if it
	// exists, may be of either kind and recovery searches both.
	Kind      TokenKind            `json:"kind,omitempty"`
	Ref       *TokenRef            `json:"ref,omitempty"`
	Marked    bool                 `json:"marked"`
	CreatedAt string               `json:"createdAt,omitempty"`
	Cleanup   *RevokeOutcome       `json:"cleanup,omitempty"`
	Search    *UnmarkedTokenSearch `json:"search,omitempty"`
}

// CredentialRecovery is the local half of recovery: which backend the
// commit reached, what it may hold for this server after the failure, and
// the command that clears it, with its risk stated. Present only when the
// final state can leave a local residue.
type CredentialRecovery struct {
	Backend              CredentialBackend `json:"backend"`
	State                CredentialState   `json:"state"`
	PriorSource          PriorSource       `json:"priorSource,omitempty"`
	HelperRecordRetained bool              `json:"helperRecordRetained,omitempty"`
	CompensationError    string            `json:"compensationError,omitempty"`
	Command              string            `json:"command,omitempty"`
	DataLossRisk         string            `json:"dataLossRisk"`
}

// LoginFailureDetails is the details payload of every auth login failure
// after this operation attempted to create a token or reached its commit:
// the primary failure, the stage, what each persistence system holds, the
// fate of every token the operation created, and the recovery step.
type LoginFailureDetails struct {
	Primary            PrimaryFailure      `json:"primary"`
	Stage              LoginStage          `json:"stage"`
	ConfigState        ConfigState         `json:"configState,omitempty"`
	CredentialState    CredentialState     `json:"credentialState,omitempty"`
	CredentialRecovery *CredentialRecovery `json:"credentialRecovery,omitempty"`
	OperationMarker    string              `json:"operationMarker,omitempty"`
	SessionToken       *TokenFate          `json:"sessionToken,omitempty"`
	DerivedToken       *TokenFate          `json:"derivedToken,omitempty"`
	Recovery           string              `json:"recovery"`
}

// RecoveryCommands renders the kind-correct list and revoke commands for a
// token. Reads carry no gate; the deprecated gate applies to mutations of
// the v3 kind only. This is the only source of that text.
func RecoveryCommands(ref TokenRef) (list, revoke string) {
	switch ref.Kind {
	case TokenKindV3:
		return "rancher get management-tokens",
			"rancher delete management-token " + ref.Name + " --apply --yes --allow-deprecated"
	default:
		return "rancher get tokens", "rancher delete token " + ref.Name + " --apply --yes"
	}
}

// DataLossRiskText is the one sentence every CredentialRecovery carries:
// auth logout erases every backend for the server, not only the one that
// failed.
func DataLossRiskText(server string) string {
	return "removes the credential for " + server + " from every backend (keychain, file, credential helper), including a credential from an earlier login"
}

// TextDetails projects the payload onto indented lines, primary first.
func (d LoginFailureDetails) TextDetails() []string {
	lines := []string{fmt.Sprintf("failed: %s (%s)", d.Primary.Message, d.Primary.Code)}
	// The primary's own details keep their default-format projection: a
	// TLS classification's certificate facts must not vanish because the
	// login schema now wraps them.
	if td, ok := d.Primary.Details.(TextDetails); ok {
		lines = append(lines, td.TextDetails()...)
	}
	lines = append(lines, "stage: "+string(d.Stage))
	if d.ConfigState != "" {
		lines = append(lines, "config: "+string(d.ConfigState))
	}
	if d.CredentialState != "" {
		line := "credential store"
		if d.CredentialRecovery != nil {
			line += " (" + string(d.CredentialRecovery.Backend) + ")"
		}
		line += ": " + string(d.CredentialState)
		if d.CredentialRecovery != nil && d.CredentialRecovery.CompensationError != "" {
			line += "; undo failed: " + d.CredentialRecovery.CompensationError
		}
		lines = append(lines, line)
	}
	if d.CredentialRecovery != nil && d.CredentialRecovery.Command != "" {
		lines = append(lines, "clear the local credential with: "+d.CredentialRecovery.Command+" ("+d.CredentialRecovery.DataLossRisk+")")
	}
	if d.OperationMarker != "" {
		lines = append(lines, "operation marker: [op:"+d.OperationMarker+"]")
	}
	if d.SessionToken != nil {
		lines = append(lines, fateLine("session token", d.SessionToken))
	}
	if d.DerivedToken != nil {
		lines = append(lines, fateLine("derived token", d.DerivedToken))
	}
	return append(lines, "recover: "+d.Recovery)
}

func fateLine(label string, f *TokenFate) string {
	name := label
	if f.Ref != nil {
		name += " " + f.Ref.String()
	}
	line := name + ": " + string(f.Outcome)
	if f.Ref == nil && f.Kind != "" {
		line += " (" + string(f.Kind) + ")"
	}
	if f.Ref == nil && f.Kind == "" && f.Outcome != CreateNotCreated {
		line += " (kind unknown)"
	}
	if f.Marked {
		line += ", marked"
	}
	// Every cleanup result the payload carries is rendered: a notAttempted
	// or notApplicable cleanup is a fact, not an omission.
	if f.Cleanup != nil && f.Cleanup.Result != "" {
		if f.Cleanup.Result == ResultFailed {
			line += "; cleanup failed: " + f.Cleanup.Error
		} else {
			line += "; cleanup: " + string(f.Cleanup.Result)
		}
	}
	return line
}
