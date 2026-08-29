package runtime

import (
	"os"
	"strings"

	"github.com/rancher/rancher-cli/internal/output/style"
)

// ActivateColor resolves whether ANSI styling turns on for one output
// stream. --color=never/always short-circuits regardless of environment.
// Otherwise, a non-empty NO_COLOR turns styling off; failing that, a
// CLICOLOR_FORCE or FORCE_COLOR value other than "0" turns styling on
// regardless of isTTY; failing that, styling follows isTTY. Called once per
// stream with that stream's own TTY-ness, so stdout and stderr can land on
// different results from the same flag and environment values.
//
// An empty NO_COLOR counts as unset — a common shell accident, not a
// deliberate opt-out. CLICOLOR_FORCE=0 and FORCE_COLOR=0 count as unset.
// When both force variables are set and disagree, whichever is not "0"
// wins; neither ordering is an error.
func ActivateColor(colorFlag, noColor, cliColorForce, forceColor string, isTTY bool) bool {
	switch colorFlag {
	case "never":
		return false
	case "always":
		return true
	}
	if noColor != "" {
		return false
	}
	if forceSet(cliColorForce) || forceSet(forceColor) {
		return true
	}
	return isTTY
}

func forceSet(v string) bool { return v != "" && v != "0" }

// stylesFor resolves the per-stream Style for one invocation. Both are the
// zero (disabled) Style whenever the invocation's format is not among the
// styleable set (table/default/wide) — json, yaml, name, and template stay
// plain no matter what --color, NO_COLOR, or a force variable says.
func stylesFor(streams IO) (out, err style.Style) {
	if !textRenderFormat(streams.Format) {
		return style.Style{}, style.Style{}
	}
	noColor := os.Getenv("NO_COLOR")
	cliColorForce := os.Getenv("CLICOLOR_FORCE")
	forceColor := os.Getenv("FORCE_COLOR")
	out = style.Style{Enabled: ActivateColor(streams.Color, noColor, cliColorForce, forceColor, streams.OutTTY)}
	err = style.Style{Enabled: ActivateColor(streams.Color, noColor, cliColorForce, forceColor, streams.ErrTTY)}
	return out, err
}

// boldShownOnceLine wraps the line ending in ShownOnceSuffix — the
// once-only credential value's own line — in bold, leaving every other
// line and all whitespace untouched. A text with no matching line, or a
// disabled Style, is returned unchanged.
func boldShownOnceLine(text string, s style.Style) string {
	if !s.Enabled {
		return text
	}
	lines := strings.SplitAfter(text, "\n")
	for i, line := range lines {
		body, hadNewline := strings.CutSuffix(line, "\n")
		if !strings.HasSuffix(body, ShownOnceSuffix) {
			continue
		}
		styled := s.Bold(body)
		if hadNewline {
			styled += "\n"
		}
		lines[i] = styled
	}
	return strings.Join(lines, "")
}
