package clierr

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

const (
	CodeNotLoggedIn                    = "NOT_LOGGED_IN"
	CodeConfirmationRequired           = "CONFIRMATION_REQUIRED"
	CodeDryRunNotApplicable            = "DRY_RUN_NOT_APPLICABLE"
	CodeAmbiguousSelection             = "AMBIGUOUS_SELECTION"
	CodeConflict                       = "CONFLICT"
	CodeLockTimeout                    = "LOCK_TIMEOUT"
	CodeAuth                           = "AUTH"
	CodeCredentialReplaced             = "CREDENTIAL_REPLACED"
	CodeLoginIncomplete                = "LOGIN_INCOMPLETE"
	CodeCredentialTransitionIncomplete = "CREDENTIAL_TRANSITION_INCOMPLETE"
	CodeNotFound                       = "NOT_FOUND"
	CodeRBAC                           = "RBAC"
	CodeValidation                     = "VALIDATION"
	CodeGeneric                        = "GENERIC"
	CodePrimeRequired                  = "PRIME_REQUIRED"
	CodeTLSTrust                       = "TLS_TRUST"
	CodeCapabilityUnsupported          = "CAPABILITY_UNSUPPORTED"
	CodeInterrupted                    = "INTERRUPTED"
)

// LearnMorePrimeURL is the one stable, product-owned destination a
// PRIME_REQUIRED hint points at. Product owns the redirect target; the CLI
// never embeds a campaign-specific link.
const LearnMorePrimeURL = "https://www.rancher.com/products/rancher-prime"

// TextDetails is the projection a details payload owes the default-format
// error text: the indented lines rendered after the message/hint/candidates
// block. WithDetails accepts only a value implementing it, so an unprojected
// details type fails to compile rather than silently rendering nothing on
// the default format.
type TextDetails interface {
	TextDetails() []string
}

// PrimeRequiredDetails is PRIME_REQUIRED's structured details payload: the
// detector key and the server the check ran against, so an agent gets a
// stable machine shape rather than prose parsing.
type PrimeRequiredDetails struct {
	Feature string `json:"feature"`
	Server  string `json:"server"`
}

// TextDetails projects the checked feature and server onto one indented
// line.
func (d PrimeRequiredDetails) TextDetails() []string {
	return []string{fmt.Sprintf("feature: %s (checked against server %s)", d.Feature, d.Server)}
}

// CapabilityUnsupportedDetails is CAPABILITY_UNSUPPORTED's structured
// details payload - the discovery-based detector key and the server the
// check ran against, mirroring PrimeRequiredDetails' shape for a
// non-Prime capability gate (the ext.cattle.io API-discovery tier).
type CapabilityUnsupportedDetails struct {
	Feature string `json:"feature"`
	Server  string `json:"server"`
}

// TextDetails projects the checked capability and server onto one indented
// line.
func (d CapabilityUnsupportedDetails) TextDetails() []string {
	return []string{fmt.Sprintf("capability: %s (checked against server %s)", d.Feature, d.Server)}
}

// CommittedContextDetails is the details payload for a non-retryable error
// surfaced AFTER a mutation has already committed on the server: a
// post-commit render failure on a Delivery: DeliveryOnceOnly command, a
// missing/malformed delivered value, or a wait timeout. Naming the
// resource and a non-mutating recovery keeps every post-commit failure
// from reading as an invitation to blindly retry - retrying a committed
// create/add/rotate creates a second credential. PartialOutput is set only
// by the render-failure path, when the final stdout write may already have
// exposed part of the payload.
type CommittedContextDetails struct {
	Committed     bool   `json:"committed"`
	ResourceKind  string `json:"resourceKind"`
	ResourceName  string `json:"resourceName"`
	SecretName    string `json:"secretName,omitempty"`
	PartialOutput bool   `json:"partialOutput,omitempty"`
	Recovery      string `json:"recovery"`
}

// TextDetails projects the committed resource, an optional partial-output
// note, and the non-mutating recovery step onto indented lines. Retry
// guidance comes from the error's own Retryable/Recovery fields, never a
// generic line here - a blind "do not retry" would contradict a producer
// whose stated recovery is exactly a re-run.
func (d CommittedContextDetails) TextDetails() []string {
	lines := []string{
		fmt.Sprintf("committed: %s %s - the change may already be on the server", d.ResourceKind, d.ResourceName),
	}
	if d.PartialOutput {
		lines = append(lines, "partial output may already be on stdout")
	}
	lines = append(lines, "recover: "+d.Recovery)
	return lines
}

// exitCodes maps a code to its process exit status.
var exitCodes = map[string]int{
	CodeValidation:           2,
	CodeConfirmationRequired: 2,
	CodeDryRunNotApplicable:  2,
	CodeAmbiguousSelection:   2,
	CodeNotLoggedIn:          2,
	CodeAuth:                 3,
	// The stored credential is unusable for the same reason an AUTH failure
	// is: the command cannot authenticate as who it was asked to be.
	CodeCredentialReplaced: 3,
	CodeLoginIncomplete:    3,
	// A store change that did not finish leaves credentials where the
	// directory no longer says they are: the same "cannot authenticate as
	// who it was asked to be" class, and retryable once the backend the
	// remaining work needs is reachable.
	CodeCredentialTransitionIncomplete: 3,
	CodeNotFound:                       4,
	CodeConflict:                       5,
	CodeRBAC:                           6,
	// 7 is not an error exit: `skill status` claims it as a success status
	// (drift detected), and SuccessExitCodes must stay disjoint from this map.
	CodeLockTimeout:           8,
	CodePrimeRequired:         9,
	CodeTLSTrust:              10,
	CodeCapabilityUnsupported: 11,
	// 128 plus SIGINT's number, the shell convention for a process ended
	// by an interrupt. INTERRUPTED exits 130 for either signal the command
	// listens for; only the forced exit on a second signal carries that
	// signal's own number.
	CodeInterrupted: 130,
	CodeGeneric:     1,
}

// ShellQuote quotes s for a POSIX sh command line: wrapped in single quotes,
// with each embedded single quote closed, escaped, and reopened. Every value
// interpolated into a machine hint (a name, URL, or path) goes through this
// so the emitted command round-trips as one argument even when the value
// carries a space or shell metacharacter. The remedy marker is removed
// from the value, so no name, URL or path can place the invocation's
// --config inside a command it only appears in.
func ShellQuote(s string) string {
	s = strings.ReplaceAll(s, RemedyMarker, "")
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
	Details  any      `json:"details,omitempty"`
	cause    error    // not serialized; enables errors.Is/As chaining
	warnings []string // not serialized directly; rides the envelope's top-level "warnings", never data.error
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
// field (code, hint, cause, retryable, candidates, details, warnings)
// carries over, so a caller amending the text cannot silently drop the rest.
func (e *Error) WithMessage(msg string) *Error {
	c := *e
	c.Message = msg
	// The struct copy above aliases e's warnings backing array; clone it so
	// a later WithWarnings append on c (or e) can never overwrite the
	// other's slice.
	c.warnings = slices.Clone(e.warnings)
	return &c
}

func (e *Error) WithCause(err error) *Error          { e.cause = err; return e }
func (e *Error) WithRetryable(b bool) *Error         { e.Retryable = b; return e }
func (e *Error) WithCandidates(c []Candidate) *Error { e.Candidates = c; return e }

// WithDetails attaches d as the error's structured details payload: d must
// implement TextDetails, so a details type with no reviewed default-format
// projection fails to compile rather than shipping silent on stderr. The
// full value still marshals into the envelope unchanged for -o json/-o yaml.
func (e *Error) WithDetails(d TextDetails) *Error { e.Details = d; return e }

// WithWarnings appends w to the error's warning ride: advisories that belong
// on the same channel as a success result's Warnings, but attached to a
// command that ultimately failed. The field is never serialized as part of
// the Error value itself (it carries no json tag and the field is
// unexported) - a renderer reads it through Warnings() and places it at the
// envelope's top level or as "# warning:" stderr lines, never under
// data.error, so a warning cannot print twice.
func (e *Error) WithWarnings(w ...string) *Error {
	e.warnings = append(e.warnings, w...)
	return e
}

// Warnings returns a copy of the error's warning ride, in the order they
// were added.
func (e *Error) Warnings() []string {
	return slices.Clone(e.warnings)
}

// Retryable builds a new Error with the given code and message with Retryable
// set to true. It is shorthand for New(code, message).WithRetryable(true).
func Retryable(code, message string) *Error { return New(code, message).WithRetryable(true) }

// AllCodes lists every stable error code (for the schema manifest).
func AllCodes() []string {
	return []string{
		CodeNotLoggedIn, CodeConfirmationRequired, CodeDryRunNotApplicable,
		CodeAmbiguousSelection, CodeConflict, CodeLockTimeout,
		CodeAuth, CodeCredentialReplaced, CodeLoginIncomplete,
		CodeCredentialTransitionIncomplete,
		CodeNotFound, CodeRBAC, CodeValidation, CodePrimeRequired, CodeTLSTrust,
		CodeCapabilityUnsupported, CodeInterrupted, CodeGeneric,
	}
}

// CodeInfo describes one stable error code: the code, the process exit
// status it maps to, and what a caller may read into it. Retryability is a
// property of each error, not of its code, and travels in the envelope.
type CodeInfo struct {
	Code string `json:"code"`
	Exit int    `json:"exit"`
	Desc string `json:"desc"`
}

// codeDescs is the one place a code's meaning is written down. It covers
// exactly the codes AllCodes lists, which a test asserts, so the schema
// manifest and the reference can never carry a code without a meaning.
var codeDescs = map[string]string{
	CodeNotLoggedIn:                    "no server is selected, the selected server entry does not exist, or it has no stored credential and RANCHER_CLI_TOKEN is not set",
	CodeConfirmationRequired:           "the mutation was not applied because a required gate flag (--apply, --yes, --allow-deprecated) was missing; the message names the flags",
	CodeDryRunNotApplicable:            "--dry-run was given to a command that does not mutate",
	CodeAmbiguousSelection:             "more than one value matched and none was selected; the error lists the candidates when it can",
	CodeConflict:                       "a write collided with existing state: a resource changed on the server since it was read, a name already exists, a server entry is registered with a different URL or CA, or the config directory changed while the command ran",
	CodeLockTimeout:                    "a config-directory lock was not released within the wait, usually because another rancher process is writing",
	CodeAuth:                           "authentication failed: the server rejected the credential (HTTP 401), the exchange with the identity provider failed or timed out, or an environment URL was set without RANCHER_CLI_TOKEN",
	CodeCredentialReplaced:             "the stored credential for the server entry was replaced by another config directory that shares this one's identity, or by a login in progress",
	CodeLoginIncomplete:                "a login for the server entry is in progress, or ended before it recorded its credential identity",
	CodeCredentialTransitionIncomplete: "a change of this directory's credential store has work left: the old copy of the credential it changed, or the store pin, is not settled yet, so a second store change is refused until it is; other commands proceed",
	CodeNotFound:                       "no such resource, server entry, or name: the server returned HTTP 404, the config directory has no such entry, or the name matched nothing in already-fetched data",
	CodeRBAC:                           "the server denied the request for lack of permission (HTTP 403), including Rancher's proxy rejecting an invalid or insufficient token",
	CodeValidation:                     "the request was refused before or by the server: a missing or invalid flag or argument, a mutation under read-only mode, a disabled feature, or an object the server refused (HTTP 400 or 422)",
	CodeGeneric:                        "an error with no more specific code, including a server or transport failure",
	CodePrimeRequired:                  "the command requires Rancher Prime and the connected server does not have it",
	CodeTLSTrust:                       "the server's TLS certificate is not trusted and was not accepted",
	CodeCapabilityUnsupported:          "the connected server does not support the workflow the command needs",
	CodeInterrupted:                    "the command was ended by Ctrl-C or a termination signal",
}

// Codes lists every stable error code with its exit status and meaning, in
// AllCodes order; the schema manifest and the reference table both render
// this list.
func Codes() []CodeInfo {
	codes := AllCodes()
	out := make([]CodeInfo, 0, len(codes))
	for _, c := range codes {
		out = append(out, CodeInfo{Code: c, Exit: ExitCode(c), Desc: codeDescs[c]})
	}
	return out
}

// Interrupted is the error for a command the operator ended (Ctrl-C or a
// termination signal): not retryable as such, no hint, exit 130.
func Interrupted() *Error { return New(CodeInterrupted, "interrupted") }

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
// nil otherwise - for callers that also need the concrete error (message,
// hint) without a second chain walk.
func AsNotFound(err error) *Error {
	if ce, ok := errors.AsType[*Error](err); ok && ce.Code == CodeNotFound {
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
