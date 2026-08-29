package runtime

import (
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/output/style"
)

// textLinesString renders first as the unindented headline, followed by zero
// or more already-formatted detail or safety-context lines, each indented
// two spaces — the shared shape behind every command's default-format text
// body, generic and bespoke alike.
func textLinesString(first string, lines ...string) string {
	var b strings.Builder
	b.WriteString(first + "\n")
	for _, line := range lines {
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}

// StyleFirstLineBold returns a StyledText builder that bolds text's first
// line only — the same rule TextLines and MutationText apply automatically,
// exposed for a caller that must assemble Text by hand (concatenating
// several MutationText bodies, say) but still wants the shared convention.
func StyleFirstLineBold(text string) func(style.Style) string {
	return func(s style.Style) string { return BoldFirstLine(text, s) }
}

// StyleEveryLineBold returns a StyledText builder that bolds every
// non-empty line — for a Text whose lines are ALL headlines, such as a
// multi-binding revoke concatenating one kubectl-terse confirmation per
// binding.
func StyleEveryLineBold(text string) func(style.Style) string {
	return func(s style.Style) string {
		return StyleLines(text, s, func(_ int, body string) (string, bool) {
			if body == "" {
				return "", false
			}
			return s.Bold(body), true
		})
	}
}

// TextLines renders the shared kubectl-terse text body (see
// textLinesString) into a Result whose StyledText builder bolds the
// headline. Merge the returned Text and StyledText into a fuller Result the
// caller is assembling; the zero-valued remainder (Kind, Data, and so on)
// carries nothing meaningful.
func TextLines(first string, lines ...string) Result {
	text := textLinesString(first, lines...)
	return Result{Text: text, StyledText: StyleFirstLineBold(text)}
}

// MutationText renders the shared kubectl-terse success line for a mutation
// — "<kind>/<name> <verb>" — followed by zero or more indented detail or
// safety-context lines (see TextLines). Central to every generic
// create/patch/delete text body and reused verbatim by bespoke commands
// whose success line takes the same "<kind>/<name> <verb>" shape (grant, the
// OIDC client secret lifecycle, revoke).
func MutationText(kind, name, verb string, lines ...string) Result {
	return TextLines(fmt.Sprintf("%s/%s %s", kind, name, verb), lines...)
}

// AccentLine re-renders res's styled text so the line built from content
// prints in yellow instead of plain or bold — the safety-relevant fact a
// TextLines or MutationText caller opts into after building the Result.
// content is the same unindented string the caller passed to TextLines or
// MutationText, matched against both its headline form (no indent) and its
// detail-line form (the two-space indent TextLines adds), so a caller never
// has to know which shape its own line took. Yellow replaces rather than
// compounds the headline bold when content is the first line, so a line
// never carries two emphases.
func AccentLine(res Result, content string) Result {
	text := res.Text
	detail := "  " + content
	res.StyledText = func(s style.Style) string {
		return StyleLines(text, s, func(i int, body string) (string, bool) {
			if body == content || body == detail {
				return s.Yellow(body), true
			}
			if i == 0 {
				return s.Bold(body), true
			}
			return "", false
		})
	}
	return res
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
