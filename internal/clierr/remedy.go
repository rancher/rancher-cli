package clierr

import "strings"

// RemedyMarker opens every remedy command a builder writes, just ahead of
// the word rancher, so the invocation that prints the command decides what
// follows that word: the config directory it runs against (kubectl's exec
// plugin and the kubeconfig command name it, because their directory comes
// from the exec entry or a flag the operator's shell does not carry), or
// nothing. U+2063 is a Unicode format character, which output.Printable
// removes, so a marker that reaches a terminal without being expanded
// leaves the plain command, and the command's own text is unchanged.
const RemedyMarker = "\u2063"

// Rancher begins every remedy command: Rancher + " auth login ...". A Go
// constant, so a constant remedy stays a constant.
const Rancher = RemedyMarker + "rancher"

// ExpandRemedies writes prefix after the word rancher in every remedy
// command s holds: " --config '<dir>'" for an invocation whose remedies
// name its config directory, "" for every other. A marker anywhere else is
// left as it is; only a marker ahead of rancher opens a remedy.
func ExpandRemedies(s, prefix string) string {
	if !strings.Contains(s, RemedyMarker) {
		return s
	}
	return strings.ReplaceAll(s, Rancher, "rancher"+prefix)
}

// HasRemedy reports whether s holds a remedy command.
func HasRemedy(s string) bool { return strings.Contains(s, Rancher) }

// MarkRemedy is cmd, a command built as "rancher <rest>" by code whose
// output also reaches result data, written as a remedy command. Anything
// else is returned as it is.
func MarkRemedy(cmd string) string {
	if strings.HasPrefix(cmd, "rancher ") {
		return RemedyMarker + cmd
	}
	return cmd
}

// RemedyText is a details payload whose text can hold remedy commands; the
// renderer maps each of its strings through the invocation's expansion.
type RemedyText interface {
	MapText(f func(string) string) TextDetails
}

// RemedyCommands is a details payload with fields that hold a command and
// nothing else, which a renderer that can offer no command empties.
type RemedyCommands interface {
	MapCommands(f func(string) string) TextDetails
}

// MapText returns e with f applied to its message, hint, warnings and, for
// a details payload that implements RemedyText, its details. e itself is
// returned when none of them holds a marker, so the common error pays one
// scan. The copy keeps e's code, cause, retryability and candidates.
func (e *Error) MapText(f func(string) string) *Error {
	if !e.holdsMarker() {
		return e
	}
	c := *e
	c.Message = f(e.Message)
	c.Hint = f(e.Hint)
	c.warnings = make([]string, len(e.warnings))
	for i, w := range e.warnings {
		c.warnings[i] = f(w)
	}
	if rt, ok := e.Details.(RemedyText); ok {
		c.Details = rt.MapText(f)
	}
	return &c
}

// holdsMarker reports whether any text MapText maps holds a marker.
func (e *Error) holdsMarker() bool {
	if strings.Contains(e.Message, RemedyMarker) || strings.Contains(e.Hint, RemedyMarker) {
		return true
	}
	for _, w := range e.warnings {
		if strings.Contains(w, RemedyMarker) {
			return true
		}
	}
	if rt, ok := e.Details.(RemedyText); ok {
		held := false
		rt.MapText(func(s string) string {
			held = held || strings.Contains(s, RemedyMarker)
			return s
		})
		return held
	}
	return false
}

// MapCommands maps the payload's two command fields, the recovery
// commands.
func (d LoginFailureDetails) MapCommands(f func(string) string) TextDetails {
	if d.CredentialRecovery != nil {
		cr := *d.CredentialRecovery
		cr.Command = f(cr.Command)
		d.CredentialRecovery = &cr
	}
	if d.ConfigRecovery != nil {
		cr := *d.ConfigRecovery
		cr.Command = f(cr.Command)
		d.ConfigRecovery = &cr
	}
	return d
}

// MapText maps every string of the payload that can carry a command: the
// primary failure's message and hint, the two recovery commands and the
// recovery text.
func (d LoginFailureDetails) MapText(f func(string) string) TextDetails {
	d.Primary.Message = f(d.Primary.Message)
	d.Primary.Hint = f(d.Primary.Hint)
	if d.CredentialRecovery != nil {
		cr := *d.CredentialRecovery
		cr.Command = f(cr.Command)
		d.CredentialRecovery = &cr
	}
	if d.ConfigRecovery != nil {
		cr := *d.ConfigRecovery
		cr.Command = f(cr.Command)
		d.ConfigRecovery = &cr
	}
	d.Recovery = f(d.Recovery)
	return d
}
