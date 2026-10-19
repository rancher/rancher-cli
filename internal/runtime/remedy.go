package runtime

import (
	"bytes"
	"io"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
)

// UnprintableConfigDirNote is what a text says in place of its commands
// when the config directory's path holds characters output.Printable
// removes: printed as it is, the path would be another path, so no command
// can name it and none is offered.
const UnprintableConfigDirNote = "the config directory's path holds characters that cannot be printed; no command is offered"

// RemedyPrefix is what follows the word rancher in every remedy command an
// invocation against dir prints: --config and the directory, shell-quoted,
// for any dir, the default included, since the shell that runs the command
// may resolve another directory. printable is false when the path holds a
// character a terminal line cannot show; the invocation then offers no
// command at all (withoutCommands).
func RemedyPrefix(dir string) (prefix string, printable bool) {
	if output.Printable(dir) != dir {
		return "", false
	}
	return " --config " + clierr.ShellQuote(dir), true
}

// remedyPrefix is this invocation's RemedyPrefix: none when its remedies
// name no directory.
func (streams IO) remedyPrefix() (string, bool) {
	if streams.RemedyDir == "" {
		return "", true
	}
	return RemedyPrefix(streams.RemedyDir)
}

// withoutCommands is s with its remedy commands left out, for a directory
// no command can name: the text up to the clause that holds the first one
// (a clause opens after "; " or " ("), then UnprintableConfigDirNote. A
// text whose first clause holds a command becomes the note alone.
func withoutCommands(s string) string {
	kept, _, found := strings.Cut(s, clierr.Rancher)
	if !found {
		return s
	}
	cut := max(strings.LastIndex(kept, "; "), strings.LastIndex(kept, " ("))
	if cut <= 0 {
		return UnprintableConfigDirNote
	}
	return kept[:cut] + "; " + UnprintableConfigDirNote
}

// ExpandText is s with every remedy command it holds written for this
// invocation; when the directory cannot be printed, s without them and
// with the sentence that says why.
func (streams IO) ExpandText(s string) string {
	if !clierr.HasRemedy(s) {
		return s
	}
	prefix, printable := streams.remedyPrefix()
	if !printable {
		return withoutCommands(s)
	}
	return clierr.ExpandRemedies(s, prefix)
}

// ExpandError is ce with every remedy command in its message, hint,
// warnings and details written for this invocation. When the directory
// cannot be printed no command is offered: the hint, which is a command,
// is dropped whenever it holds one; a details payload's command fields are
// emptied; every other text keeps its words up to the clause holding its
// first command and says why; and the message says it when no message or
// warning already does.
func (streams IO) ExpandError(ce *clierr.Error) *clierr.Error {
	prefix, printable := streams.remedyPrefix()
	if printable {
		return ce.MapText(func(s string) string { return clierr.ExpandRemedies(s, prefix) })
	}
	out := ce.MapText(withoutCommands)
	if out == ce {
		return out
	}
	if clierr.HasRemedy(ce.Hint) {
		out.Hint = ""
	}
	if rc, ok := out.Details.(clierr.RemedyCommands); ok {
		out.Details = rc.MapCommands(func(s string) string {
			if s == UnprintableConfigDirNote {
				return ""
			}
			return s
		})
	}
	said := strings.Contains(out.Message, UnprintableConfigDirNote)
	for _, w := range out.Warnings() {
		said = said || strings.Contains(w, UnprintableConfigDirNote)
	}
	if !said {
		out.Message += "; " + UnprintableConfigDirNote
	}
	return out
}

// expandAll maps ExpandText over warnings.
func (streams IO) expandAll(warnings []string) []string {
	if len(warnings) == 0 {
		return warnings
	}
	out := make([]string, len(warnings))
	for i, w := range warnings {
		out[i] = streams.ExpandText(w)
	}
	return out
}

// plainRemedies is s with every remedy command written without a
// directory: the form for text that is not a remedy channel of the
// invocation (result text and data) and for a renderer that has no
// invocation to ask.
func plainRemedies(s string) string { return clierr.ExpandRemedies(s, "") }

// plainRemedyWriter removes the marker from every remedy command written
// through it. Result data is not a remedy channel, so a command that
// reaches it prints as the plain command. Each Write is taken whole: an
// encoder writes one value in one call, so a command is never split
// across two. The one change it makes to structured output: a U+2063
// directly before the word rancher is dropped, wherever the text came
// from; every other byte is written as it is.
type plainRemedyWriter struct{ w io.Writer }

var markedRancher = []byte(clierr.Rancher)

func (p plainRemedyWriter) Write(b []byte) (int, error) {
	if !bytes.Contains(b, markedRancher) {
		return p.w.Write(b)
	}
	if _, err := p.w.Write(bytes.ReplaceAll(b, markedRancher, []byte("rancher"))); err != nil {
		return 0, err
	}
	return len(b), nil
}

// PrintableRemedies is output.Printable for a line that holds remedy
// commands: every character a terminal line cannot show is removed except
// the markers, which the invocation's prompter expands when it prints the
// line.
func PrintableRemedies(s string) string {
	parts := strings.Split(s, clierr.Rancher)
	for i, p := range parts {
		parts[i] = output.Printable(p)
	}
	return strings.Join(parts, clierr.Rancher)
}
