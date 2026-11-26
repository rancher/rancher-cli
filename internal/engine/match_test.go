package engine

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

type matchFixture struct {
	ids     []string
	display map[string]string
	cands   []clierr.Candidate
}

func newMatchFixture(pairs ...string) matchFixture {
	f := matchFixture{display: map[string]string{}}
	for i := 0; i < len(pairs); i += 2 {
		f.ids = append(f.ids, pairs[i])
		f.display[pairs[i]] = pairs[i+1]
		f.cands = append(f.cands, clierr.Candidate{ID: pairs[i], Name: pairs[i+1]})
	}
	return f
}

func (f matchFixture) match(v string, t MatchText) (string, string, error) {
	return MatchOne(v, f.ids, func(id string) string { return f.display[id] }, f.cands, t)
}

var projectText = MatchText{Noun: "project", Plural: "projects", Flag: "--project",
	NotFound: "project not found", NotFoundHint: clierr.NewCommand("access projects")}

// TestMatchOneIDMatchWinsWithoutAWarningWhenNoDisplayNameMatches asserts an
// id match resolves silently when no other object has v as its display
// name.
func TestMatchOneIDMatchWinsWithoutAWarningWhenNoDisplayNameMatches(t *testing.T) {
	t.Parallel()
	f := newMatchFixture("p-1", "web", "p-2", "db")
	id, warning, err := f.match("p-1", projectText)
	require.NoError(t, err)
	assert.Equal(t, "p-1", id)
	assert.Empty(t, warning)
}

// TestMatchOneIDMatchWinsOverDisplayNameMatches asserts the id wins whether
// one or several other objects have v as their display name, with the
// warning wording for the flag form and the positional form.
func TestMatchOneIDMatchWinsOverDisplayNameMatches(t *testing.T) {
	t.Parallel()
	positional := projectText
	positional.Flag = ""
	one := newMatchFixture("p-1", "first", "p-2", "p-1")
	several := newMatchFixture("p-3", "p-1", "p-1", "first", "p-2", "p-1")
	for name, tc := range map[string]struct {
		f    matchFixture
		t    MatchText
		want string
	}{
		"one other, flag":          {one, projectText, "--project 'p-1' is the id of project 'p-1' and the display name of project 'p-2'; the command uses the one with that id. To use the other one, pass --project 'p-2'"},
		"one other, positional":    {one, positional, "'p-1' is the id of project 'p-1' and the display name of project 'p-2'; the command uses the one with that id. To use the other one, pass 'p-2'"},
		"several others, flag":     {several, projectText, "--project 'p-1' is the id of project 'p-1' and the display name of projects 'p-2', 'p-3'; the command uses the one with that id. To use one of the others, pass --project with its id"},
		"several others, position": {several, positional, "'p-1' is the id of project 'p-1' and the display name of projects 'p-2', 'p-3'; the command uses the one with that id. To use one of the others, pass its id"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			id, warning, err := tc.f.match("p-1", tc.t)
			require.NoError(t, err)
			assert.Equal(t, "p-1", id)
			assert.Equal(t, tc.want, warning)
		})
	}
}

// TestMatchOneIDMatchWarningLeavesOutAFlagValueThatCannotBePrinted asserts a
// value that cannot be one argument of a printed command drops the sentence
// that tells the reader to pass it, or the flag in front of the id that
// matched, and puts the left-out note in the warning once.
func TestMatchOneIDMatchWarningLeavesOutAFlagValueThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	positional := projectText
	positional.Flag = ""
	for name, tc := range map[string]struct {
		id, other string
		t         MatchText
		subject   string
		dropped   bool
	}{
		"other with apostrophe":       {"p-1", "p-'2", projectText, "--project 'p-1' is the id of project 'p-1'", true},
		"other with curly apostrophe": {"p-1", "p-\u20192", projectText, "--project 'p-1' is the id of project 'p-1'", true},
		"other with zero width space": {"p-1", "p-\u200b2", projectText, "--project 'p-1' is the id of project 'p-1'", true},
		"other with double backslash": {"p-1", "p-\\\\2", positional, "'p-1' is the id of project 'p-1'", true},
		"id with apostrophe":          {"p-'1", "p-2", projectText, "the value ", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newMatchFixture(tc.id, "first", tc.other, tc.id)
			_, warning, err := f.match(tc.id, tc.t)
			require.NoError(t, err)
			assert.Equal(t, 1, strings.Count(warning, clierr.LeftOutNote))
			assert.True(t, strings.HasSuffix(warning, "; "+clierr.LeftOutNote), warning)
			assert.True(t, strings.HasPrefix(warning, tc.subject), warning)
			assert.Equal(t, !tc.dropped, strings.Contains(warning, "To use the other one"))
		})
	}
}

func TestMatchOneOneDisplayNameMatchResolves(t *testing.T) {
	t.Parallel()
	f := newMatchFixture("p-1", "web", "p-2", "db")
	id, warning, err := f.match("db", projectText)
	require.NoError(t, err)
	assert.Equal(t, "p-2", id)
	assert.Empty(t, warning)
}

// TestMatchOneDuplicateDisplayNamesAreAmbiguousAndSortedByID asserts two or
// more display-name matches with no id match are AMBIGUOUS_SELECTION whose
// candidates are only the matches, sorted by id, whatever order they were
// given in.
func TestMatchOneDuplicateDisplayNamesAreAmbiguousAndSortedByID(t *testing.T) {
	t.Parallel()
	f := newMatchFixture("p-3", "web", "p-9", "db", "p-1", "web", "p-2", "web")
	_, _, err := f.match("web", projectText)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Equal(t, "3 projects have display name 'web'; pass the project id", ce.Message)
	assert.Equal(t, []clierr.Candidate{{ID: "p-1", Name: "web"}, {ID: "p-2", Name: "web"}, {ID: "p-3", Name: "web"}}, ce.Candidates)
}

// TestMatchOneNotFoundCarriesTheTextAndTheCandidatesByID asserts no match is
// NOT_FOUND with the caller's message and hint and the candidates sorted by
// id, whatever order they were given in, with no total for a short list.
func TestMatchOneNotFoundCarriesTheTextAndTheCandidatesByID(t *testing.T) {
	t.Parallel()
	f := newMatchFixture("p-2", "db", "p-1", "api")
	_, _, err := f.match("web", projectText)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "project not found", ce.Message)
	assert.Equal(t, clierr.NewCommand("access projects").Field(), ce.Hint)
	assert.Equal(t, []clierr.Candidate{{ID: "p-1", Name: "api"}, {ID: "p-2", Name: "db"}}, ce.Candidates)
	assert.Zero(t, ce.CandidatesTotal)
}

// TestMatchOneNotFoundCutsALongCandidateList asserts a NOT_FOUND for a list
// of more than clierr.CandidateLimit objects has the first
// clierr.CandidateLimit by id and the size of the full list as candidates and
// total, and a list of exactly clierr.CandidateLimit has all of them and no
// total.
func TestMatchOneNotFoundCutsALongCandidateList(t *testing.T) {
	t.Parallel()
	for _, n := range []int{clierr.CandidateLimit, clierr.CandidateLimit + 1, 300} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			t.Parallel()
			var pairs []string
			for i := n - 1; i >= 0; i-- {
				pairs = append(pairs, fmt.Sprintf("p-%03d", i), fmt.Sprintf("name-%03d", i))
			}
			f := newMatchFixture(pairs...)
			_, _, err := f.match("web", projectText)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeNotFound, ce.Code)
			require.Len(t, ce.Candidates, min(n, clierr.CandidateLimit))
			for i, c := range ce.Candidates {
				assert.Equal(t, fmt.Sprintf("p-%03d", i), c.ID)
			}
			if n > clierr.CandidateLimit {
				assert.Equal(t, n, ce.CandidatesTotal)
			} else {
				assert.Zero(t, ce.CandidatesTotal)
			}
		})
	}
}

// TestMatchOneAmbiguousSelectionListsEveryMatch asserts the candidates of
// AMBIGUOUS_SELECTION are never cut: the caller must choose among all of them.
func TestMatchOneAmbiguousSelectionListsEveryMatch(t *testing.T) {
	t.Parallel()
	var pairs []string
	for i := range clierr.CandidateLimit + 5 {
		pairs = append(pairs, fmt.Sprintf("p-%03d", i), "web")
	}
	_, _, err := newMatchFixture(pairs...).match("web", projectText)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Len(t, ce.Candidates, clierr.CandidateLimit+5)
	assert.Zero(t, ce.CandidatesTotal)
}

// TestMatchOneNotFoundListsTheMatchesFirst asserts a NOT_FOUND lists the
// entries whose id or name contains the typed value, whatever the case, before
// the others, each part by id, and the total counts every entry.
func TestMatchOneNotFoundListsTheMatchesFirst(t *testing.T) {
	t.Parallel()
	var pairs []string
	for i := 299; i >= 0; i-- {
		name := fmt.Sprintf("name-%03d", i)
		switch i {
		case 250:
			name = "my-WEB"
		case 100:
			name = "webapp"
		}
		pairs = append(pairs, fmt.Sprintf("p-%03d", i), name)
	}
	_, _, err := newMatchFixture(pairs...).match("web", projectText)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	var got []string
	for _, c := range ce.Candidates {
		got = append(got, c.ID)
	}
	assert.Equal(t, []string{"p-100", "p-250", "p-000", "p-001", "p-002", "p-003", "p-004", "p-005", "p-006", "p-007"}, got)
	assert.Equal(t, 300, ce.CandidatesTotal)
}
