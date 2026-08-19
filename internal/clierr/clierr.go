package clierr

import (
	"errors"
	"maps"
)

const (
	CodeNotLoggedIn          = "NOT_LOGGED_IN"
	CodeConfirmationRequired = "CONFIRMATION_REQUIRED"
	CodeDryRunNotApplicable  = "DRY_RUN_NOT_APPLICABLE"
	CodeAmbiguousSelection   = "AMBIGUOUS_SELECTION"
	CodeConflict             = "CONFLICT"
	CodeLockTimeout          = "LOCK_TIMEOUT"
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
	CodeNotLoggedIn:          2,
	CodeAuth:                 3,
	CodeNotFound:             4,
	CodeConflict:             5,
	CodeRBAC:                 6,
	// 7 is not an error exit: `skill status` claims it as a success status
	// (drift detected), and SuccessExitCodes must stay disjoint from this map.
	CodeLockTimeout: 8,
	CodeGeneric:     1,
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
	// Details carries an optional command-specific structured payload
	// (e.g. revoke's partial-failure split). Shape is owned by the
	// producing command; consumers decode by command context.
	Details any   `json:"details,omitempty"`
	cause   error // not serialized; enables errors.Is/As chaining
}

func New(code, message string) *Error { return &Error{Code: code, Message: message} }

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.cause }

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
	maps.Copy(out, exitCodes)
	return out
}

func (e *Error) WithHint(h string) *Error { e.Hint = h; return e }

// WithMessage returns a copy of e with msg as its message; every other
// field (code, hint, cause, retryable, candidates, details) carries over,
// so a caller amending the text cannot silently drop the rest.
func (e *Error) WithMessage(msg string) *Error {
	c := *e
	c.Message = msg
	return &c
}

func (e *Error) WithCause(err error) *Error          { e.cause = err; return e }
func (e *Error) WithRetryable(b bool) *Error         { e.Retryable = b; return e }
func (e *Error) WithCandidates(c []Candidate) *Error { e.Candidates = c; return e }
func (e *Error) WithDetails(d any) *Error            { e.Details = d; return e }

// Retryable builds a new Error with the given code and message with Retryable
// set to true. It is shorthand for New(code, message).WithRetryable(true).
func Retryable(code, message string) *Error { return New(code, message).WithRetryable(true) }

// AllCodes lists every stable error code (for the schema manifest).
func AllCodes() []string {
	return []string{
		CodeNotLoggedIn, CodeConfirmationRequired, CodeDryRunNotApplicable,
		CodeAmbiguousSelection, CodeConflict, CodeLockTimeout,
		CodeAuth, CodeNotFound, CodeRBAC, CodeValidation, CodeGeneric,
	}
}

// Wrap re-messages err for a caller adding command context: when err carries
// a structured *Error its classification (code, retryability, hint,
// candidates, details) survives with msg as the new message; otherwise a
// plain GENERIC error with msg is built, keeping err as the cause. Use it
// where a wrapper would otherwise flatten a classified error (e.g. a
// LOCK_TIMEOUT from a store) into the catch-all shape.
func Wrap(err error, msg string) *Error {
	if ce, ok := errors.AsType[*Error](err); ok {
		return ce.WithMessage(msg)
	}
	return New(CodeGeneric, msg).WithCause(err)
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

// Classified reports whether e carries a specific classification beyond the
// mapErr catch-all default: a non-Generic code, a retry signal, or a
// caller-facing hint. The catch-all shape (CodeGeneric, Retryable false,
// Hint "") is indistinguishable from transport noise and must not pass through
// identity-critical guards unchanged.
func Classified(e *Error) bool {
	return e.Code != CodeGeneric || e.Retryable || e.Hint != ""
}
