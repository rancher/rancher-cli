package engine

import (
	"fmt"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// MatchText words MatchOne's answers for one kind of object.
type MatchText struct {
	Noun, Plural string // "project", "projects"
	Flag         string // "--project"; "" when the value is a positional argument
	NotFound     string // the NOT_FOUND message
	NotFoundHint string
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
			WithCandidates(cands).WithHint(t.NotFoundHint)
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
// name of others, "" when there are none.
func idWinsWarning(v string, others []string, t MatchText) string {
	if len(others) == 0 {
		return ""
	}
	q := clierr.ShellQuote(v)
	flag := ""
	if t.Flag != "" {
		flag = t.Flag + " "
	}
	quoted := make([]string, len(others))
	for i, o := range others {
		quoted[i] = clierr.ShellQuote(o)
	}
	if len(others) == 1 {
		return fmt.Sprintf("%s%s is the id of %s %s; %s %s also has the display name %s - pass %s%s for that one",
			flag, q, t.Noun, q, t.Noun, quoted[0], q, flag, quoted[0])
	}
	pass := "pass the id of the one you mean"
	if t.Flag != "" {
		pass = "pass " + t.Flag + " with the id of the one you mean"
	}
	return fmt.Sprintf("%s%s is the id of %s %s; %s %s also have the display name %s - %s",
		flag, q, t.Noun, q, t.Plural, strings.Join(quoted, ", "), q, pass)
}
