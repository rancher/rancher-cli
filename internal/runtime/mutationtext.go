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
