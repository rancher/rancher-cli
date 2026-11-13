package engine

import (
	"fmt"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// MatchText words MatchOne's answers for one kind of object.
type MatchText struct {
	Noun, Plural string             // "project", "projects"
	Flag         clierr.CommandWord // "--project"; "" when the value is a positional argument
	NotFound     string             // the NOT_FOUND message
	NotFoundHint clierr.Command     // the NOT_FOUND hint; the zero Command is no hint
}

// MatchOne picks the object v names among ids, by id or by display name.
// An id match wins: when v is also other objects' display name, warning
// names them and the flag value that selects each. With no id match, one
// display-name match resolves and two or more are AMBIGUOUS_SELECTION with
// the candidates narrowed to the matches, sorted by id. No match is
// NOT_FOUND with t.NotFound, t.NotFoundHint and every candidate.
func MatchOne(v string, ids []string, displayOf func(id string) string,
	cands []clierr.Candidate, t MatchText) (id, warning string, err error) {
	var byDisplay []string
	idFound := false
	for _, x := range ids {
		if x == v {
			idFound = true
			continue
		}
		if displayOf(x) == v {
			byDisplay = append(byDisplay, x)
		}
	}
	slices.Sort(byDisplay)
	switch {
	case idFound:
		return v, idWinsWarning(v, byDisplay, t), nil
	case len(byDisplay) == 1:
		return byDisplay[0], "", nil
	case len(byDisplay) == 0:
		return "", "", clierr.New(clierr.CodeNotFound, t.NotFound).
			WithCandidates(cands).WithHintCommand(t.NotFoundHint)
	default:
		narrowed := make([]clierr.Candidate, 0, len(byDisplay))
		for _, c := range cands {
			if slices.Contains(byDisplay, c.ID) {
				narrowed = append(narrowed, c)
			}
		}
		slices.SortFunc(narrowed, func(a, b clierr.Candidate) int { return strings.Compare(a.ID, b.ID) })
		return "", "", clierr.New(clierr.CodeAmbiguousSelection,
			fmt.Sprintf("%d %s have display name %s; pass the %s id", len(byDisplay), t.Plural, clierr.ShellQuote(v), t.Noun)).
			WithCandidates(narrowed)
	}
}

// idWinsWarning is the warning for an id match that is also the display
// name of others, "" when there are none. A value that cannot be printed in a
// command leaves out the sentence that tells the reader to pass it; the
// subject then names the value without the flag.
func idWinsWarning(v string, others []string, t MatchText) string {
	if len(others) == 0 {
		return ""
	}
	q := clierr.ShellQuote(v)
	leftOut := false
	subject, ok := clierr.FlagFragment(t.Flag, v)
	if !ok {
		leftOut = true
		subject = "the value " + q
	}
	quoted := make([]string, len(others))
	for i, o := range others {
		quoted[i] = clierr.ShellQuote(o)
	}
	var warning string
	if len(others) == 1 {
		warning = fmt.Sprintf("%s is the id of %s %s and the display name of %s %s; the command uses the one with that id.",
			subject, t.Noun, q, t.Noun, quoted[0])
		if pass, ok := clierr.FlagFragment(t.Flag, others[0]); ok {
			warning += " To use the other one, pass " + pass
		} else {
			leftOut = true
			warning = strings.TrimSuffix(warning, ".")
		}
	} else {
		pass := "pass its id"
		if t.Flag != "" {
			pass = "pass " + string(t.Flag) + " with its id"
		}
		warning = fmt.Sprintf("%s is the id of %s %s and the display name of %s %s; the command uses the one with that id. To use one of the others, %s",
			subject, t.Noun, q, t.Plural, strings.Join(quoted, ", "), pass)
	}
	if leftOut {
		return clierr.WithLeftOutNote(warning)
	}
	return warning
}
