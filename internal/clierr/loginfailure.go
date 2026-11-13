package clierr

import (
	"fmt"
	"strings"
)

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
	// Status is the HTTP status the server answered, 0 when no answer
	// came; never serialized. A revoke with the token as its own bearer
	// reads 401, 403 and 404 differently from one with another bearer.
	Status int `json:"-"`
	// LogoutStatus is the HTTP status the logout endpoint answered for a
	// v3 token revoked with its own bearer, 0 when logout was not sent or
	// no answer came; never serialized.
	LogoutStatus int `json:"-"`
	// TLSError is the TLS_TRUST error of a delete that failed on the
	// server's certificate, nil otherwise; never serialized. Error keeps the
	// request's own text.
	TLSError *Error `json:"-"`
}

// Provenance says who created a login credential and therefore who may
// revoke it.
type Provenance string

const (
	ProvenanceDerived       Provenance = "derived"
	ProvenanceLegacySession Provenance = "legacySession"
	ProvenanceUserSupplied  Provenance = "userSupplied"
	// ProvenanceExecLogin marks a login credential the exec plugin created
	// for itself under the token-reuse setting; it is persisted with the
	// entry, and only the setting decides whether it may be used.
	ProvenanceExecLogin Provenance = "execLogin"
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
	// ConfigRollbackFailed means an entry this operation wrote could not be
	// restored to its prior state after the credential commit failed.
	ConfigRollbackFailed ConfigState = "rollbackFailed"
	// ConfigStoreChangeNotApplied means the credential is committed in the
	// store the login changed to, but neither the old copy of the entry's
	// credential could be deleted nor the pin written, so this directory's
	// readers still resolve the previous credential. The change's steps
	// stay recorded, and the entry's exec-credential cache writes stay
	// paused, until a later login finishes them.
	ConfigStoreChangeNotApplied ConfigState = "storeChangeNotApplied"
	// ConfigDeclarationNotWritten means the credential is committed in the
	// store the login opened, but the record of which store this directory
	// keeps credentials in could not be written. Nothing is lost and
	// nothing is wrong with the credential; the directory still names the
	// store it named before, and the next login writes the new one.
	ConfigDeclarationNotWritten ConfigState = "declarationNotWritten"
	// ConfigTransitionNotRecorded means the credential is committed in the
	// store the login opened, but the config write that would have put the
	// directory's credential store change into effect failed, so the
	// change never started: the other entries' credentials are where they
	// were, and readers still resolve to the store they resolved to
	// before. Running the login again is the way out.
	ConfigTransitionNotRecorded ConfigState = "transitionNotRecorded"
	// ConfigTransitionLost means the config write did put the change into
	// effect, and the record of what the change still had to do was not
	// the one it named by the time the login looked: removed, replaced by
	// another process, or unreadable. Nothing can finish work nobody has
	// a readable record of, so the login says which of those it found and
	// names the remedy that fits.
	ConfigTransitionLost ConfigState = "transitionLost"
)

// LoginStage is where a login failed: while acquiring the credential or
// while committing it.
type LoginStage string

const (
	StageAuthentication LoginStage = "authentication"
	StageCommit         LoginStage = "commit"
	// StageClusterToken is the plugin-created login's own step: creating
	// and validating the cluster token that proves the new credential can
	// create, before anything is persisted.
	StageClusterToken LoginStage = "clusterToken"
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
	AuthProvider     string `json:"authProvider"`
	HandoffAt        string `json:"handoffAt"`
	PollEndedAt      string `json:"pollEndedAt"`
	RequestExpiresAt string `json:"requestExpiresAt,omitempty"`
}

// TokenFate is one token's outcome and cleanup.
type TokenFate struct {
	Outcome CreateOutcome `json:"outcome"`
	// Kind is the token's kind when it is known without a name: the derived token create
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
	// ExpiresAt is when the token expires, when the create response said.
	// It feeds the recovery text only and is never rendered.
	ExpiresAt string `json:"-"`
}

// LoginTokenNoun is Rancher's name for the token a login creates and
// deletes: a session token for the password and OAuth flows, a kubeconfig
// token for SAML, which Search identifies. A nil fate is not SAML.
func (f *TokenFate) LoginTokenNoun() string {
	if f != nil && f.Search != nil {
		return "kubeconfig token"
	}
	return "session token"
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

// SupersededToken is a CLI-created token an entry no longer holds the
// credential for: it was replaced by a later login and has not yet been
// retired. Its owner is captured from the create response that made it,
// never inferred later; an empty owner is a fact the retirement rules
// handle, not a gap to fill.
type SupersededToken struct {
	Ref          TokenRef `json:"ref"`
	UserID       string   `json:"userId,omitempty"`
	ExpiresAt    string   `json:"expiresAt,omitempty"`
	SupersededAt string   `json:"supersededAt"`
	// Server is the canonical identity of the server the token lives on.
	// A record without one belongs to its entry's current server; a record
	// whose server is not that is never retired by the entry's logins.
	Server string `json:"server,omitempty"`
	// LastAttempt is the most recent failed retirement, carried without
	// its Ref (the record's own Ref is the token); nil until a delete has
	// failed. A successful retirement removes the record instead.
	LastAttempt *RevokeOutcome `json:"lastAttempt,omitempty"`
}

// PriorEntry is the complete pre-write snapshot of a server entry, carried
// so an operator can rebuild trust and URL by hand when the entry could
// not be restored. CA is the exact stored value, byte for byte; the text
// projection names only its shape.
type PriorEntry struct {
	URL                 string            `json:"url"`
	CA                  string            `json:"ca,omitempty"`
	CAFingerprint       string            `json:"caFingerprint,omitempty"`
	CAFromEnvironment   bool              `json:"caFromEnvironment,omitempty"`
	Provenance          Provenance        `json:"provenance,omitempty"`
	TokenRef            *TokenRef         `json:"tokenRef,omitempty"`
	TokenUserID         string            `json:"tokenUserId,omitempty"`
	CredentialExpiresAt string            `json:"credentialExpiresAt,omitempty"`
	SupersededTokens    []SupersededToken `json:"supersededTokens,omitempty"`
}

// ConfigRecovery is present only when an entry this operation wrote could
// not be rolled back: the entry, its complete prior state (nil when the
// operation created it), and the one command that removes it. The command
// ends in the same config write that just failed and, when the file is
// still unwritable, fails the same way naming the file.
type ConfigRecovery struct {
	Entry   string      `json:"entry"`
	Prior   *PriorEntry `json:"prior,omitempty"`
	Command string      `json:"command"`
}

// caShape names the prior CA's shape for the text projection without
// carrying its bytes.
func (p *PriorEntry) caShape() []DetailPart {
	switch {
	case p.CA == "":
		return []DetailPart{LabelPart("system roots")}
	case strings.Contains(p.CA, "-----BEG"):
		return []DetailPart{LabelPart(fmt.Sprintf("inline PEM (%d bytes)", len(p.CA)))}
	default:
		return []DetailPart{ValuePart("path ", p.CA)}
	}
}

// text is the prior entry on one line: URL, CA shape and fingerprint,
// provenance, the token it named with owner and expiry, and how many
// superseded records it carried.
func (p *PriorEntry) text() []DetailPart {
	parts := []DetailPart{ValuePart("", p.URL), LabelPart(", CA ")}
	parts = append(parts, p.caShape()...)
	if p.CAFingerprint != "" {
		parts = append(parts, ValuePart(" fingerprint ", p.CAFingerprint))
	}
	if p.CAFromEnvironment {
		parts = append(parts, LabelPart(", last login connected with RANCHER_CLI_CACERT"))
	}
	if p.Provenance != "" {
		parts = append(parts, ValuePart(", provenance ", string(p.Provenance)))
	}
	if p.TokenRef != nil {
		parts = append(parts, TokenRefParts(", token ", *p.TokenRef)...)
		switch {
		case p.TokenUserID != "" && p.CredentialExpiresAt != "":
			parts = append(parts, ValuePart(" (user ", p.TokenUserID), ValuePart(", expires ", p.CredentialExpiresAt), LabelPart(")"))
		case p.TokenUserID != "":
			parts = append(parts, ValuePart(" (user ", p.TokenUserID), LabelPart(")"))
		case p.CredentialExpiresAt != "":
			parts = append(parts, ValuePart(" (expires ", p.CredentialExpiresAt), LabelPart(")"))
		}
	}
	switch n := len(p.SupersededTokens); n {
	case 0:
	case 1:
		parts = append(parts, LabelPart(", 1 superseded token"))
	default:
		parts = append(parts, LabelPart(fmt.Sprintf(", %d superseded tokens", n)))
	}
	return parts
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
	ClusterToken       *TokenFate          `json:"clusterToken,omitempty"`
	ConfigRecovery     *ConfigRecovery     `json:"configRecovery,omitempty"`
	Recovery           string              `json:"recovery"`
}

// DataLossRiskText is the one sentence every CredentialRecovery carries:
// auth logout erases every backend for the server, not only the one that
// failed.
func DataLossRiskText(server string) string {
	return "removes the credential for " + server + " from every backend (keychain, file, credential helper), including a credential from an earlier login"
}

// TextDetails projects the payload onto indented lines, primary first.
func (d LoginFailureDetails) TextDetails() []DetailLine {
	lines := []DetailLine{Line(ValuePart("failed: ", d.Primary.Message), ValuePart(" (", d.Primary.Code), LabelPart(")"))}
	// The primary's own details keep their default-format projection: a
	// TLS classification's certificate facts must not vanish because the
	// login schema now wraps them.
	if td, ok := d.Primary.Details.(TextDetails); ok {
		lines = append(lines, td.TextDetails()...)
	}
	lines = append(lines, Line(ValuePart("stage: ", string(d.Stage))))
	if d.ConfigState != "" {
		lines = append(lines, Line(ValuePart("config: ", string(d.ConfigState))))
	}
	if d.CredentialState != "" {
		parts := []DetailPart{LabelPart("credential store")}
		if d.CredentialRecovery != nil {
			parts = append(parts, ValuePart(" (", string(d.CredentialRecovery.Backend)), LabelPart(")"))
		}
		parts = append(parts, ValuePart(": ", string(d.CredentialState)))
		if d.CredentialRecovery != nil && d.CredentialRecovery.CompensationError != "" {
			parts = append(parts, ValuePart("; undo failed: ", d.CredentialRecovery.CompensationError))
		}
		lines = append(lines, Line(parts...))
	}
	if d.CredentialRecovery != nil && d.CredentialRecovery.Command != "" {
		lines = append(lines, Line(
			ValuePart("clear the local credential with: ", d.CredentialRecovery.Command),
			ValuePart("; that command ", d.CredentialRecovery.DataLossRisk)))
	}
	if d.OperationMarker != "" {
		lines = append(lines, Line(ValuePart("operation marker: [op:", d.OperationMarker), LabelPart("]")))
	}
	if d.SessionToken != nil {
		lines = append(lines, fateLine(LabelPart(d.SessionToken.LoginTokenNoun()), d.SessionToken))
	}
	if d.DerivedToken != nil {
		lines = append(lines, fateLine(LabelPart("derived token"), d.DerivedToken))
	}
	if d.ClusterToken != nil {
		lines = append(lines, fateLine(LabelPart("cluster token"), d.ClusterToken))
	}
	if d.ConfigRecovery != nil {
		parts := []DetailPart{ValuePart("config entry ", d.ConfigRecovery.Entry), LabelPart(" could not be restored")}
		if d.ConfigRecovery.Command != "" {
			parts = append(parts, ValuePart("; remove it with: ", d.ConfigRecovery.Command))
		}
		lines = append(lines, Line(parts...))
		if d.ConfigRecovery.Prior != nil {
			lines = append(lines, DetailLine{
				Indent: "  ",
				Parts:  append([]DetailPart{LabelPart("prior entry: ")}, d.ConfigRecovery.Prior.text()...),
			})
		}
	}
	return append(lines, Line(ValuePart("recover: ", d.Recovery)))
}

// fateLine is one token's line: noun, the reference when there is one, the
// outcome and the cleanup result.
func fateLine(noun DetailPart, f *TokenFate) DetailLine {
	parts := []DetailPart{noun}
	if f.Ref != nil {
		parts = append(parts, TokenRefParts(" ", *f.Ref)...)
	}
	parts = append(parts, ValuePart(": ", string(f.Outcome)))
	if f.Ref == nil && f.Kind != "" {
		parts = append(parts, ValuePart(" (", string(f.Kind)), LabelPart(")"))
	}
	if f.Ref == nil && f.Kind == "" && f.Outcome != CreateNotCreated {
		parts = append(parts, LabelPart(" (kind unknown)"))
	}
	if f.Marked {
		parts = append(parts, LabelPart(", marked"))
	}
	// Every cleanup result the payload carries is rendered: a notAttempted
	// or notApplicable cleanup is a fact, not an omission.
	if f.Cleanup != nil && f.Cleanup.Result != "" {
		if f.Cleanup.Result == ResultFailed {
			parts = append(parts, ValuePart("; cleanup failed: ", f.Cleanup.Error))
		} else {
			parts = append(parts, ValuePart("; cleanup: ", string(f.Cleanup.Result)))
		}
	}
	return Line(parts...)
}
