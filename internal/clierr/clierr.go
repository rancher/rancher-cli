package clierr

const (
	CodeContextNotSet        = "CONTEXT_NOT_SET"
	CodeConfirmationRequired = "CONFIRMATION_REQUIRED"
	CodeDryRunNotApplicable  = "DRY_RUN_NOT_APPLICABLE"
	CodeAmbiguousSelection   = "AMBIGUOUS_SELECTION"
	CodeConflict             = "CONFLICT"
	CodeAuth                 = "AUTH"
	CodeNotFound             = "NOT_FOUND"
	CodeRBAC                 = "RBAC"
	CodeValidation           = "VALIDATION"
	CodeGeneric              = "GENERIC"
)

// exitCodes maps a code to its process exit status.
var exitCodes = map[string]int{
	CodeValidation:           2,
	CodeConfirmationRequired: 2,
	CodeDryRunNotApplicable:  2,
	CodeAmbiguousSelection:   2,
	CodeContextNotSet:        2,
	CodeAuth:                 3,
	CodeNotFound:             4,
	CodeConflict:             5,
	CodeRBAC:                 6,
	CodeGeneric:              1,
}

// Candidate is a selectable option returned with AMBIGUOUS_SELECTION.
type Candidate struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Error is the structured CLI error emitted in the error envelope.
type Error struct {
	Code       string      `json:"code"`
	Message    string      `json:"message"`
	Hint       string      `json:"hint,omitempty"`
	Phase      string      `json:"phase,omitempty"`
	Candidates []Candidate `json:"candidates,omitempty"`
	Retryable  bool        `json:"retryable"`
	Tier       string      `json:"tier,omitempty"`
}

func New(code, message string) *Error { return &Error{Code: code, Message: message} }

func (e *Error) Error() string { return e.Message }

// ExitCode returns the process exit status for this error's code.
func (e *Error) ExitCode() int {
	if c, ok := exitCodes[e.Code]; ok {
		return c
	}
	return 1
}

func (e *Error) WithHint(h string) *Error            { e.Hint = h; return e }
func (e *Error) WithPhase(p string) *Error           { e.Phase = p; return e }
func (e *Error) WithTier(t string) *Error            { e.Tier = t; return e }
func (e *Error) WithRetryable(b bool) *Error         { e.Retryable = b; return e }
func (e *Error) WithCandidates(c []Candidate) *Error { e.Candidates = c; return e }

// AllCodes lists every stable error code (for the schema manifest).
func AllCodes() []string {
	return []string{
		CodeContextNotSet, CodeConfirmationRequired, CodeDryRunNotApplicable,
		CodeAmbiguousSelection, CodeConflict,
		CodeAuth, CodeNotFound, CodeRBAC, CodeValidation, CodeGeneric,
	}
}
