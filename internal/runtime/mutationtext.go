package runtime

import (
	"fmt"
	"strings"
)

// TextLines renders first as the unindented headline, followed by zero or
// more already-formatted detail or safety-context lines, each indented two
// spaces — the shared shape behind every command's default-format text
// body, generic and bespoke alike.
func TextLines(first string, lines ...string) string {
	var b strings.Builder
	b.WriteString(first + "\n")
	for _, line := range lines {
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}

// MutationText renders the shared kubectl-terse success line for a
// mutation — "<kind>/<name> <verb>" — followed by zero or more indented
// detail or safety-context lines (see TextLines). Central to every generic
// create/patch/delete text body and reused verbatim by bespoke commands
// whose success line takes the same "<kind>/<name> <verb>" shape (grant,
// the OIDC client secret lifecycle, revoke).
func MutationText(kind, name, verb string, lines ...string) string {
	return TextLines(fmt.Sprintf("%s/%s %s", kind, name, verb), lines...)
}

// ShownOnceSuffix marks the line within a DeliveryOnceOnly command's Text
// body that carries the one value a caller can never retrieve again (a
// bearer token, a client secret). Every once-only command appends it to
// that line, and only that line, so the render path can find and emphasize
// it without inspecting the value itself.
const ShownOnceSuffix = "   (shown once — store it now)"

// OnceOnlyLine builds the Text line for a DeliveryOnceOnly command's
// unretrievable value ("<label>: <value>" plus ShownOnceSuffix) together
// with the matching Result.OnceOnlyNotice sentence ("this <subject> is
// shown exactly once — store it now"), so a command cannot produce the
// inline emphasis without the paired advisory, or vice versa.
func OnceOnlyLine(label, value, subject string) (line, notice string) {
	return label + ": " + value + ShownOnceSuffix,
		"this " + subject + " is shown exactly once — store it now"
}
