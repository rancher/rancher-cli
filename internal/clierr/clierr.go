package clierr

import "errors"

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
	Candidates []Candidate `json:"candidates,omitempty"`
	Retryable  bool        `json:"retryable"`
	Tier       string      `json:"tier,omitempty"`
	// Details carries an optional command-specific structured payload
	// (e.g. revoke's partial-failure split). Shape is owned by the
	// producing command; consumers decode by command context.
	Details any `json:"details,omitempty"`
}

func New(code, message string) *Error { return &Error{Code: code, Message: message} }

func (e *Error) Error() string { return e.Message }

// ExitCode returns the process exit status for this error's code.
func (e *Error) ExitCode() int { return ExitCode(e.Code) }

// ExitCode returns the process exit status for code; unknown codes exit 1.
func ExitCode(code string) int {
	if c, ok := exitCodes[code]; ok {
		return c
	}
	return 1
}

// CodeExits returns a copy of the full code→exit mapping (for the schema
// manifest's exitCodes field).
func CodeExits() map[string]int {
	out := make(map[string]int, len(exitCodes))
	for code, exit := range exitCodes {
		out[code] = exit
	}
	return out
}

func (e *Error) WithHint(h string) *Error            { e.Hint = h; return e }
func (e *Error) WithTier(t string) *Error            { e.Tier = t; return e }
func (e *Error) WithRetryable(b bool) *Error         { e.Retryable = b; return e }
func (e *Error) WithCandidates(c []Candidate) *Error { e.Candidates = c; return e }
func (e *Error) WithDetails(d any) *Error            { e.Details = d; return e }

// AllCodes lists every stable error code (for the schema manifest).
func AllCodes() []string {
	return []string{
		CodeContextNotSet, CodeConfirmationRequired, CodeDryRunNotApplicable,
		CodeAmbiguousSelection, CodeConflict,
		CodeAuth, CodeNotFound, CodeRBAC, CodeValidation, CodeGeneric,
	}
}

// AsNotFound returns err's *Error when err is a structured NOT_FOUND error,
// nil otherwise — for callers that also need the concrete error (message,
// hint) without a second chain walk.
func AsNotFound(err error) *Error {
	var ce *Error
	if errors.As(err, &ce) && ce.Code == CodeNotFound {
		return ce
	}
	return nil
}

// IsNotFound reports whether err is a structured NOT_FOUND error.
func IsNotFound(err error) bool { return AsNotFound(err) != nil }
